# TASKS：provider 级可配置 User-Agent

日期：2026-08-30
设计依据：同目录 `DESIGN.md`（取值优先级见 §3，校验规则见 §5，改动面见 §6）

格式：`[ ] 任务名 | 优先级 | 估时 | 依赖`

---

## T1

`[ ] 配置层加 userAgent 字段与校验 | P0 | 20min | 无`

**文件**：`internal/config/config.go`

**做什么**：
1. `Provider` 结构体加字段，紧跟 `Format` 之后（同属「上游身份」语义）：
   ```go
   UserAgent string `yaml:"userAgent" json:"userAgent,omitempty"`
   ```
   `omitempty` 是必需的：未配置的 provider 不应在 `/api/config` 回显里冒出空字段。
2. 新增常量，注释说明按 rune 计的理由（与 `maxProviderNameRunes` 风格一致）：
   ```go
   maxProviderUserAgentRunes = 256
   ```
3. `validate()` 的 provider 循环内（`maxQueueWait` 校验之后）加校验：
   - 空值直接放行（语义是「不配置」）
   - 长度按 `utf8.RuneCountInString` 判，超限报中文错误，带上当前长度
   - 拒绝 ASCII 控制字符（`< 0x20` 或 `== 0x7F`），**但放行 `\t`**
   - 错误信息格式对齐既有风格：`providers.%s.userAgent ...`

**不要做**：不加默认值（`applyDefaults` 不碰这个字段——空值是合法终态，
不是「待填充」）。不校验内容格式。

**验收**：
- `go test ./internal/config/` 通过
- 新增表驱动测试覆盖：合法值、空值、超 256、含 `\r`、含 `\n`、含 `\x00`、含 `\t`（须放行）
- 旧配置不写该字段时校验通过、行为不变

---

## T2

`[ ] 转发层按优先级设置 User-Agent | P0 | 25min | T1`

**文件**：`internal/proxy/upstream.go`、`internal/proxy/proxy.go`

**做什么**：
1. `setUpstreamHeaders` 增一个参数接收客户端 UA：
   ```go
   func setUpstreamHeaders(req *http.Request, p *config.Provider, clientUserAgent string)
   ```
2. 按 `DESIGN.md` §3 的三档优先级实现。**务必照抄该优先级**，尤其第三档
   必须是「完全不设该头」，不能 `Set("")`——后者会让 UA 头彻底消失，
   而不是退回 Go 默认值（§3 表格有实测数据）。
3. `proxy.go` 两处调用点（约 335、408 行）传入 `opts.ClientReq.Header.Get("User-Agent")`。
   注意 `opts.ClientReq` 可能为 nil（测试构造的 Options 不一定带），取值前判空。

**接口约束**：
- 入参 `clientUserAgent` 为已取好的字符串，不在 `upstream.go` 里碰 `Options`
  （保持该文件不依赖 `Options`，它现在只依赖 `config.Provider`）
- 函数无返回值，行为全部体现在 `req.Header` 上

**验收**：
- `go test ./internal/proxy/` 通过
- `upstream_test.go` 现有 `setUpstreamHeaders` 调用（第 31 行）已适配新签名
- 新增测试覆盖四种组合，断言要区分「头存在但值为默认」与「头不存在」：
  1. 配了 `userAgent` + 客户端也带 UA → 用配置值
  2. 未配 + 客户端带 UA → 用客户端值
  3. 未配 + 客户端无 UA → `req.Header` 里**不存在** `User-Agent` 键
     （用 `_, ok := req.Header["User-Agent"]` 断言 `ok == false`）
  4. 配了 `userAgent` + 客户端无 UA → 用配置值
- 建议补一个端到端测试：起 `httptest` 假上游，断言它收到的 UA 与预期一致

---

## T3

`[ ] 前端 Provider 弹窗支持填写 User-Agent | P0 | 30min | T1`

**文件**：`cmd/gateway/web/src/index.template.html`、
`cmd/gateway/web/src/app/00-state.js.part`、
`cmd/gateway/web/src/app/11-providers.js.part`

**做什么**：
1. 弹窗表单加输入框。位置放在「队列最大等待 ms」之后、表单末尾，
   用 `md:col-span-2` 占满整行（UA 字符串通常较长）。
   - 标签文案：`User-Agent`
   - `placeholder`：`留空则转发客户端真实 UA`
   - 下方加一行 `text-[11px] text-muted` 小字说明用途，参考 name 字段的写法
   - **必须有 `autocomplete="off"`**，与该表单其余输入框一致
2. `00-state.js.part:102` 的 `providerForm` 初始状态加 `userAgent: ''`
3. `11-providers.js.part` **四处**都要改（漏一处就是缺陷）：
   - `addProvider()`：`userAgent: ''`
   - `editProvider()`：`userAgent: p.userAgent || ''`（后端 `omitempty`，字段可能不存在）
   - `duplicateProvider()`：`userAgent: p.userAgent || ''`（**要继承**，它不是密文）
   - `saveProvider()`：白名单对象里加 `userAgent: this.providerForm.userAgent || undefined`
     —— 用 `undefined` 让空值不进 JSON，与后端 `omitempty` 对齐，避免每个 provider
     都存一个空字符串字段
4. 跑 `webbuild` 重新生成 `cmd/gateway/web/index.html`。**不要手改产物**。

**验收**：
- `go test ./internal/webbuild/` 通过（`TestRepoArtifactMatchesSources` 会比对产物与源码）
- `webbuild -check` exit=0
- 前端无障碍：输入框有关联 `<label>`（照抄同表单其它字段的 `<label class="block">` 包裹写法）

**注意**：`saveProvider()` 是白名单重建对象，这是本任务最容易漏的一处。
漏了的症状：YAML 里配好的 `userAgent`，用户在管理页面保存过该 provider 一次后被静默丢弃，
无任何报错。

---

## T4

`[ ] 文档与配置模板同步 | P1 | 15min | T1,T2,T3`

**文件**：`config.example.yaml`、`README.md`、`CLAUDE.md`

**做什么**：
1. `config.example.yaml`：在某个 provider 示例里加注释掉的 `userAgent` 示例，
   注明「留空则转发客户端真实 UA」。
2. `README.md`：provider 配置说明处加该字段。**必须写明这是行为变更**——
   未配置时从原先的 `Go-http-client/1.1` 改为转发客户端真实 UA，
   并说明代价（向上游泄露客户端身份）与覆盖方式（显式配置）。
   可举 ar-gh 的实例：某些上游按客户端类型准入。
3. `CLAUDE.md` 的 Key Mechanisms 加一条，写清三档优先级和
   「第三档必须不设头而非设空字符串」这个坑。

**验收**：三份文档表述一致，取值优先级的描述与 `DESIGN.md` §3 完全对齐，无矛盾表述。

---

## T5

`[ ] 全量验证 | P0 | 10min | T1,T2,T3,T4`

**做什么**：在 `golang:1.23-alpine` 容器内跑五道门（项目约定容器名
`ai-gateway-dev-verify`，宿主无 Go 工具链）：

```
gofmt -l ./cmd ./internal     # 须无输出
go vet ./...                  # 须无输出
go test ./...                 # 全绿
go test -race ./...           # 全绿（需先 apk add gcc musl-dev）
webbuild -check               # exit 0
```

**验收**：五道门全绿。任一不过则回到对应任务修，不得带着失败项交付。

---

## 范围外（不要做）

- **不改** `internal/providerhealth` 与 `fetchUpstreamModels` 的 UA。
  它们走另一条请求构建路径，与转发不共用 `setUpstreamHeaders`。
  这是有意的范围限制，见 `DESIGN.md` §7。若认为该跟着改，
  记入 `KNOWN_ISSUES.md` 待决策，**不要自行扩大改动面**。
- 不改任何 provider 的现有配置值（`config.yaml` 是使用者的本地文件，不在版本控制内）。
- 不加「UA 预设模板下拉框」之类的便利功能——本次只做能配。
