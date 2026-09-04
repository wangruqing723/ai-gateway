# TASKS：contextWindow 三层配置与动态输出预算

设计见 `DESIGN.md`，接口契约见 `API_SPEC.md`。**先读完这两份再动手。**

## 任务清单

```
[ ] T1  新增 internal/tokenest 包与单测           | P0 | 1.5h | 无
[ ] T2  config 三层字段 + 顶层 margin + 校验      | P0 | 1.5h | 无
[ ] T3  router.Candidate 合成 ContextWindow      | P0 | 0.5h | T2
[ ] T3b [1M] 后缀剥离 + 路由匹配 + 窗口覆盖      | P0 | 2h   | T3
[ ] T4  预算裁决 + 候选跳过 + 413 终态           | P0 | 2.5h | T1,T2,T3b
[ ] T5  输出上限压制落地（Override/Ensure 调整）  | P0 | 1.5h | T4
[ ] T6  metrics 观测字段与 context_skip          | P1 | 1h   | T4
[ ] T7  前端三层输入框 + 顶层 margin             | P1 | 2.5h | T2
[ ] T8  config.example.yaml 与 CLAUDE.md 同步    | P1 | 1h   | T2,T4
[ ] T9  端到端测试                               | P0 | 2h   | T5,T6
```

合计约 16h。

---

## T1 · 新增 `internal/tokenest` 包与单测 | P0

**目标**：零依赖的输入 token 估算。

**接口**：见 `API_SPEC.md` 第 1 节。**优先选纯数据签名**
`Estimate(system any, messages []any, tools any) int`，避免依赖 `converter` 成环。

**规则**（`DESIGN.md` 4.1）：
- 按 **rune** 分类：ASCII ÷ 4，非 ASCII ÷ 1.5。**不要按 byte**，UTF-8 下中文 3 byte，
  按 byte 估会高估一倍。
- 每条消息 +4 固定开销。
- 图片块（`type` 为 `image` / `image_url` / `input_image`）计**固定 1500／张**，
  不看 base64 长度。理由与三种口径的权衡见 `DESIGN.md` 4.2，注释里要写明
  「计 0 是朝不安全方向低估」这条。
- tools 定义整体按字符估（`json.Marshal` 后走同一套规则），**不进去找图片块**。
- 不碰 `Extra`（与 System/Messages 重复，会双算）。

⚠️ **块类型判定只在消息内容块数组这一层做，不要无限递归下钻**（`DESIGN.md` 4.3）。
用户的 tool schema 里可以有一个名叫 `type` 的属性、值恰好是 `"image"`；无脑递归会把它
误判成图片块，既多算 1500 又漏掉本该计入的文本。这个坑来自 CC-Switch `body_filter.rs`
必须豁免 `properties` / `$defs` 的同类问题。

**验收**：
- 纯 ASCII、纯中文、混合三种输入的估算值符合 ÷4 / ÷1.5 预期。
- 含 N 张图片的消息，图片部分恰好贡献 N×1500，且与 base64 长度无关
  （同一张图换成 10 倍长的 base64，估算值不变）。
- tool schema 里有 `{"properties": {"type": {"const": "image"}}}` 这种形状时，
  **不**被算成图片块。
- nil / 空输入返回 0，不 panic。
- 块数组里 content 为 string 和为块数组两种形状都能处理。

---

## T3b · `[1M]` 后缀剥离 + 路由匹配 + 窗口覆盖 | P0

**文件**：`internal/router/router.go`

接口见 `API_SPEC.md` 1.1。这是**改动前就存在的缺口**，本次一并修。

**改动**：
1. 新增 `StripOneMSuffix`、常量 `OneMContextMarker` / `OneMContextWindow`。
   大小写不敏感（`[1M]` / `[1m]`），容忍标记前的空格（`deepseek-v4-pro [1M]`）。
2. `MatchRoute` 里 `globMatch` 用**剥离后**的名字匹配。
3. `target.Model == ""` 时 `targetModel` 取**剥离后**的名字，不把本地标记发给上游。
4. 带标记时 `Candidate.ContextWindow` 恒为 `OneMContextWindow`，覆盖三层配置值。
   **不动 `MaxTokens`**——声明大上下文不等于允许更大输出。

**验收**（前两条是实测过的现状，改完必须反转）：
- `match: "claude-fable-5"` + 客户端发 `claude-fable-5[1m]` → 改动前 `false`，改动后命中。
- `match: "claude-sonnet-4-5"` + `claude-sonnet-4-5[1M]` → 同上。
- `match: "*"` / `"claude-*"` 这类通配路由行为**不变**（改动前就是 true）。
- `target.Model` 为空时，转发给上游的模型名不含 `[1m]`。
- `target.Model` 非空时，用配置值，与后缀无关。
- 带后缀 → `Candidate.ContextWindow` 为 1000000，即使三层配了 200000。
- 带后缀 → `Candidate.MaxTokens` 仍等于三层解析结果，未被改。
- 不带后缀 → 全部行为与改动前逐字节一致。
- `deepseek-v4-pro [1M]`（标记前有空格）也能剥离干净。

---

## T2 · config 三层字段 + 顶层 margin + 校验 | P0

**文件**：`internal/config/config.go`

**改动**：
1. `Provider` / `Route` / `Target` 各加 `ContextWindow *int`，yaml tag
   **必须带 `omitempty`**（否则每次保存都落 `contextWindow: null`）。
2. `Config` 顶层加 `ContextSafetyMargin *int`。
3. 新增常量 `ContextWindowCeiling = 10_000_000`、`DefaultContextSafetyMargin = 4096`。
4. `applyDefaults`：只在 `ContextSafetyMargin == nil` 时填默认值。
   **`ContextWindow` 三层一律不填默认值**——nil 就是「不启用」。
5. `validate`：三层 `contextWindow` 的 `< 1` 或 `> ContextWindowCeiling` 报错；
   `contextSafetyMargin` 的 `< 0` 报错（**允许 0**）。
6. 新增 accessor `(c *Config) SafetyMargin() int`。
7. **注意 `Target` 已经不能直接做 map 键**（`88434a5` 因为加 `MaxTokens *int` 改过
   一次，见 `config.go:999` 注释）。加 `ContextWindow *int` 后重复检查的键构造
   逻辑要再确认一遍仍只取 provider+model。

**验收**：
- `contextWindow: 0` 和负数在 `Load` 阶段报错，错误信息带字段路径。
- `contextSafetyMargin: 0` 合法且 `SafetyMargin()` 返回 0。
- 未配置 `contextSafetyMargin` 时 `SafetyMargin()` 返回 4096。
- 三层都不配时 `yaml.Marshal` 的输出里**没有** `contextWindow` 行。
- 同 provider+model 但 contextWindow 不同的两个候选仍被重复检查拦下。

---

## T3 · router.Candidate 合成 ContextWindow | P0

**文件**：`internal/router/router.go`

照 `resolveMaxTokens`（`router.go:77`）写一个 `resolveContextWindow`，优先级
target > route > provider，在 `MatchRoute`（`router.go:56` 附近）填进 `Candidate`。

**验收**：三层优先级的 table-driven 测试，含「只配 provider」「target 覆盖 route」
「三层全 nil → Candidate.ContextWindow 为 nil」。

---

## T4 · 预算裁决 + 候选跳过 + 413 终态 | P0

**文件**：`cmd/gateway/main.go`

**改动**：
1. `handle()` 里 vision 翻译**之后**、候选循环**之前**估算一次
   （`DESIGN.md` 5.1：所有候选共用同一估算值）。
2. 新增 `resolveContextBudget`（签名见 `API_SPEC.md` 第 4 节）与常量
   `MinOutputBudget = 1024`。
3. 候选循环内，**紧跟 `breaker.Allow` 之后**（`main.go:569` 之后、`hasNext` 计算
   之前）插入裁决：装不下就 `contextSkips++`、记 `AttemptDetail`、追加 trail、
   `continue`。**不消耗 `attempts` 额度**，与熔断跳过同构。
4. 跳过时**不需要** `breaker.Report`——压根没进 `forwardAttempt`，没有借出探针额度。
   （对比 `buildErr` 分支要 `Report(OutcomeIgnored)`，因为它已经进过 `forwardAttempt`。
   这个差别要在注释里写明，否则后人会照抄错。）
5. 收尾 switch（`main.go:648`）加 `case attempts == 0 && contextSkips > 0`，
   **插在 `freeSkips` 分支之后、`buildErr` 之前**。返回 413 +
   `context_window_exceeded`，message 带估算值与各候选窗口。
6. `breakerSkips` 分支保持在最前（`DESIGN.md` 6.1：故障优先报）。

**验收**：
- 单候选窗口装不下 → 413 + `context_window_exceeded`，`attempts == 0`。
- 双候选、首个装不下次个装得下 → 跳过首个、成功走次个，`attempts == 1`。
- 装不下的跳过**不消耗** `maxAttempts`：`maxAttempts: 1` + 三候选（前两个装不下、
  第三个装得下）→ 仍能成功。
- 熔断 + 装不下同时存在 → 报 503 `breaker_open`（不是 413）。
- `contextWindow` 未配 → 行为与改动前完全一致（无估算、无跳过）。

---

## T5 · 输出上限压制落地 | P0

**文件**：`cmd/gateway/main.go`（`main.go:823-834` 那段）+ 可能动
`internal/converter/converter.go`

**这是本次最容易写错的一处。** 现有代码是 `Override`/`Ensure` 二分：

```go
if in.candidate.MaxTokens != nil {
    converter.OverrideMaxTokens(upstreamMap, p.Format, *in.candidate.MaxTokens)
} else {
    converter.EnsureMaxTokens(upstreamMap, p.Format)
}
```

budget 必须对**两个分支都生效**（`DESIGN.md` 5.3）：三层没配、客户端传了
`max_tokens: 64000`、budget 只有 8k 时，也得压到 8k。

**要求**：
- 保住原语义：三层没配且客户端传了值、且**未超 budget** 时，客户端值不能被改掉。
- 保住 `openai-chat` 的 `max_tokens` / `max_completion_tokens` 互斥处理
  （只改客户端已带的那个键，不能两个都塞）。
- 保住透传路径（同格式）也受约束。
- `contextBudget == 0`（未配置）时行为与改动前**逐字节一致**。

具体怎么重构由你定（可以在 converter 加一个 `ResolveMaxTokens(body, format, ...)`
统一收口，也可以在 main.go 里先算出 effective 值再统一调 `Override`）。
**但不要为了省事把 `EnsureMaxTokens` 删掉**——它的注释解释了透传路径为什么必须有
它，那个约束仍然成立。

**验收**：
- 三层配 32768 + budget 8000 → 下发 8000。
- 三层配 32768 + budget 65536 → 下发 32768（不被抬高）。
- 三层全 nil + 客户端传 64000 + budget 8000 → 下发 8000。
- 三层全 nil + 客户端传 4000 + budget 8000 → 下发 4000（不动）。
- 三层全 nil + 客户端没传 + budget 8000 → 下发 8000（默认值 32768 被压）。
- 三层全 nil + 客户端没传 + 无 budget → 下发 32768（现状不变）。
- 三种 provider format 的字段名分派全部正确；openai-chat 两个键不同时出现。
- 两条透传路径（anthropic→anthropic、openai→openai）同样受压制。

---

## T6 · metrics 观测字段与 context_skip | P1

**文件**：`internal/metrics/*.go`、`cmd/gateway/main.go`

- `RequestLog` 加 `EstimatedInputTokens int`（`json` tag 带 `omitempty`）。
- `AttemptDetail` 用 `Kind: "context_skip"`、`Outcome: "skipped"`、
  `Reason: "context_window_exceeded"`，字段本身不新增。
- trail 追加 `<provider>:context_exceeded`。

**验收**：`/api/logs` 返回体里能看到估算值与 `context_skip` 明细；未启用时
`estimatedInputTokens` 字段不出现（omitempty 生效）。

---

## T7 · 前端三层输入框 + 顶层 margin | P1

**文件**：`cmd/gateway/web/src/` 下 7 个文件，清单见 `DESIGN.md` 第 9 节。

照 `88434a5` 里 `maxTokens` 的改法**逐处对应**加 `contextWindow`：
provider 表单、route 表单、target 行内各一个 number 输入框，外加顶层设置区一个
`contextSafetyMargin`。placeholder 写清「留空 = 不启用窗口预算裁决」。

**改完必须跑 `make web`（`web-css` + `web-html` 两个都要）**。

⚠️ 本仓库已连续三次栽在「改了源码忘了重建产物」上（两次 `index.html`、
一次 `tailwind.css`，最近这次就是加了个 `w-48` 没重建 CSS 导致 CI 红、镜像没打出来）。
若新增了现有 CSS 里没有的工具类，`make web-css` 是必须的，不是可选的。

**验收**：
- `go run ./cmd/webbuild -check` exit=0
- `npx --yes tailwindcss@3.4.17 -c tailwind.config.js -i src/tailwind.css -o /tmp/x.css -m`
  后 `diff /tmp/x.css vendor/tailwind.css` 无差异
- 界面上三层都能填、能存、YAML 预览正确、留空时不落盘该字段

---

## T8 · 配置模板与文档同步 | P1

- `config.example.yaml`：加 `contextWindow` 三处示例（注释说明它与 `maxTokens`
  的关系：只向下压、不向上抬）+ 顶层 `contextSafetyMargin`。
- `CLAUDE.md`：在「Key Mechanisms」加一条 **contextWindow 与输出预算**，写清
  三层优先级、图片固定计值的口径与理由、装不下跳过不消耗额度、413 终态。
  在「Request Flow」的第 7-8 步之间补上估算与裁决的位置。
  另在「Request Flow」第 5 步（`router.MatchRoute`）补一句 `[1M]` 后缀的剥离时机，
  说明路由匹配与转发都用剥离后的名字。

**验收**：`config.example.yaml` 能被 `config.Load` 成功解析（加一个测试覆盖，
或至少手动验证）。

---

## T9 · 端到端测试 | P0

在 `cmd/gateway` 的测试里补：

1. 装不下 → 413，响应体 code 为 `context_window_exceeded`。
2. 首候选装不下、次候选装得下 → 走次候选，`x-ai-gateway-provider` 是次候选、
   `x-ai-gateway-attempts` 为 1。
3. `maxAttempts: 1` + 前两候选装不下 → 第三候选仍被尝试（证明跳过不消耗额度）。
4. budget 压制真的到达上游：假上游断言收到的 `max_tokens` / `max_output_tokens`
   等于 budget 值。三种 format 各一条。
5. `contextWindow` 未配 → 请求体与改动前一致（回归保护）。
6. 客户端发 `claude-sonnet-4-5[1M]`、路由 `match` 写精确模型名 → 命中路由，
   转发给上游的模型名不含后缀，且预算按 1000000 算。

**验收**：全套命令在容器内实跑通过（见下）。

---

## 全套验证命令（必跑，缺一不可）

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work \
  -v ai-gateway-gomod:/go/pkg/mod -w /work -e GOPROXY=https://goproxy.cn,direct \
  golang:1.23-alpine sh -c 'gofmt -l ./cmd ./internal && go vet ./... && go test ./...'

docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work \
  -v ai-gateway-gomod:/go/pkg/mod -w /work -e GOPROXY=https://goproxy.cn,direct \
  golang:1.23 go test -race ./...

docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work \
  -v ai-gateway-gomod:/go/pkg/mod -w /work -e GOPROXY=https://goproxy.cn,direct \
  golang:1.23-alpine go run ./cmd/webbuild -check
```

**注意 `GOPROXY=https://goproxy.cn,direct` 是必须的**——容器直连
`proxy.golang.org` 会 i/o timeout，不加会误判成代码问题。`-race` 必须用 Debian 版
镜像（alpine 没 gcc，临时装曾卡住 17-18 分钟）。

## 交付要求

- **不自动提交**，改完汇报，由用户拍板 `git commit`。
- 发现设计问题记入本目录的 `KNOWN_ISSUES.md`，**不要擅自改架构**。
- 自述「测试全绿」不作为验收依据，Claude 会自己在容器内重跑一遍。
