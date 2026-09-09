# DESIGN：contextWindow 三层配置与动态输出预算

## 1. 问题

上一个提交（`88434a5`）把输出上限做成了三层配置，默认 `32768`。但输出上限和上下文
窗口是**两个不对称的东西**：

- `max_tokens` / `max_completion_tokens` / `max_output_tokens` 是**请求参数**，
  三种协议都有，网关往 body 里覆写一个键就生效。
- 上下文窗口是**模型的物理属性**，没有任何协议提供「本次只准用 100k 窗口」这种
  请求字段。**没有字段可覆写。**

于是 `maxTokens` 这条链缺了另一半：输入一旦逼近窗口，硬塞 `32768` 会让上游返回
`400 max_tokens exceeds context window`，或者静默截断。窗口越小的上游越容易踩。

## 2. 方案

配置里声明每个上游的 `contextWindow`，网关估算输入 token 后，把实际下发的输出
上限压到窗口装得下的范围：

```
budget = contextWindow - estimatedInput - safetyMargin
effectiveMaxTokens = min(三层配置的 maxTokens（或客户端值/默认值）, budget)
```

`budget` 低于下限（装不下）时**跳过该候选**，failover 到下一个——窗口更大的上游
可能装得下。全部候选都装不下才返回错误。

### 2.1 与现有机制的关系

| 机制 | 关系 |
|------|------|
| `maxTokens` 三层 | contextWindow 只**向下压**，不向上抬。三层配的 32768 遇到窗口只剩 8k 就下发 8k；窗口宽裕时不动。 |
| `failover` | 装不下 = 跳过该候选，语义与熔断跳过一致，**不消耗 `maxAttempts` 额度**。理由同 `freeAttempt`：一个装不下的上游不该挤掉本来还能试的候选。 |
| `balancer` 策略 | 不参与。裁决在候选循环内按 `order` 逐个做，不改变尝试顺序。 |
| `vision` 翻译 | 翻译在候选循环外，产出文字描述。估算必须在**翻译之后**做，否则按 base64 字符数估会离谱高估。 |
| `breaker` | 跳过不发网络请求，须 `Report(OutcomeIgnored)` 归还探针额度，与 `buildErr` 分支一致。 |

## 3. 配置

三层，与 `maxTokens` 完全同构（优先级 target > route > provider）：

```yaml
providers:
  glm:
    contextWindow: 200000      # 该上游默认
    maxTokens: 32768
routes:
  - match: "*"
    contextWindow: 128000      # 路由级覆盖
    targets:
      - provider: glm
        model: glm-4.6
        contextWindow: 200000  # 候选级覆盖，优先级最高
```

全局一项，放 config 顶层：

```yaml
contextSafetyMargin: 4096      # 估算误差补偿，默认 4096
```

### 3.1 零值语义

`ContextWindow *int`，与 `MaxTokens` 同理：`nil` = 未配置（**完全不启用本机制**，
保持现状），`0` 由 `validate` 报错。不能用值类型——`0` 会被 `applyDefaults` 静默
改成默认值，让 `validate` 的下界检查变成死代码。

`ContextSafetyMargin *int` 同理，`applyDefaults` 只在 `nil` 时填 `4096`。
显式写 `0` 合法（表示不留余量），但要在 `validate` 里允许 0、拒绝负数。

### 3.2 校验边界

- `contextWindow`：`< 1` 或 `> ContextWindowCeiling`（新增常量，取 `10_000_000`）报错。
- `contextSafetyMargin`：`< 0` 报错；不设上限（大于窗口时所有候选都装不下，是用户
  自己配的，由运行时的「全部装不下」终态兜住，不在启动期拦）。
- **不校验 `maxTokens` 与 `contextWindow` 的相对大小**：`maxTokens > contextWindow`
  是合法配置，运行时会被压到 budget，不是错误。

## 4. token 估算

新增包 `internal/tokenest`，零依赖，纯字符启发式。

```go
// Estimate 估算内部请求的输入 token 数。返回值是估算值，不保证精确。
func Estimate(in *converter.Internal) int
```

### 4.1 估算规则

| 内容 | 规则 |
|------|------|
| ASCII 字符 | ÷ 4 |
| 非 ASCII 字符（中日韩等） | ÷ 1.5 |
| 图片块 | **固定 `PerImageTokens = 1500`／张**，不看 base64 长度 |
| tools 定义 | 按字符估，与文本同规则 |
| 每条消息 | +4（role/分隔符的固定开销） |

按 rune 分类累加，不是按 byte——UTF-8 下一个中文占 3 byte，按 byte 估会高估一倍。

### 4.2 图片为什么给固定值，而不是 0 或按字符

三种口径都试过一轮推演，固定值是唯一不朝不安全方向错的：

| 口径 | 问题 |
|------|------|
| 按 base64 字符估 | 严重高估。几百 KB 的 base64 实际只值 1-2k token，会把明明装得下的请求判成超限 |
| 计 0 | **朝不安全方向低估**。预算算大 → 下发的 `max_tokens` 偏大 → 照样撞上游 400，而预判式的全部意义就是别撞。四张图就是约 8k 缺口，`contextSafetyMargin: 4096` 顶不住 |
| 固定 1500／张 | 单张误差有限（真实值随尺寸在 1-2k 波动），方向可控，且与 base64 体积解耦 |

`PerImageTokens` 定为不导出常量，先不做成配置项——没有证据表明用户能比默认值猜得更准，
多一个旋钞只会多一处误配。真实占用按尺寸浮动，需要更准时应该改成按图片尺寸估，
而不是让用户填一个数。

配了 `vision` 的路由，翻译在候选循环外先跑完，此时图片块已被替换成文字描述，
走正常的字符估算，不会命中这条规则（见 5.1）。

### 4.3 遍历范围

`Internal.System`（string 或 nil）、`Internal.Messages`（`[]any`，元素是
`map[string]any{role, content}`，content 可能是 string 或块数组）、`Internal.Tools`
（数组或 nil）。块数组里 `type == "image"` / `"image_url"` / `"input_image"` 的块
整块跳过，其余按其 `text` 字段估。

不碰 `Internal.Extra`：那是原始请求体，与已规范化的 System/Messages 重复，会双算。

**块类型判定只看已知的三个值**（`image` / `image_url` / `input_image`，与
`internal/vision/vision.go:103` 和 `converter.go:726,856` 现有判定一致），
且只在**消息内容块数组这一层**判定，不做无限递归下钻。

理由来自 CC-Switch 的 `body_filter.rs`：它递归过滤 `_` 前缀字段时，必须显式豁免
`properties` / `patternProperties` / `definitions` / `$defs` —— 那几层下面的键是
**用户定义的字段名**，不是协议字段。同一个坑对我们成立：用户的 tool schema 里完全
可以有一个名叫 `type` 的属性，值恰好是 `"image"`。无脑递归会把它当图片块，
既算错 1500，又漏掉本该计入的文本。

tools 定义整体按字符估（`json.Marshal` 后走同一套规则），**不进去找图片块**——
schema 里不会有真图片，进去只会踩上面那个坑。

## 4.4 `[1M]` 后缀

Claude Code 会发 `claude-fable-5[1m]` 这种带后缀的模型名，用来声明它要 100 万上下文
（CC-Switch 的 `model_mapper.rs` 有 `ONE_M_CONTEXT_MARKER` 和
`strip_one_m_suffix_for_upstream` 专门处理）。这对本次改动有两处影响，实测确认：

**一、路由精确匹配会漏。** `globMatch` 实跑结果：

```
match="claude-fable-5"     model="claude-fable-5[1m]"    => false   ← 漏
match="claude-*"           model="claude-fable-5[1m]"    => true
match="claude-sonnet-4-5"  model="claude-sonnet-4-5[1M]" => false   ← 漏
match="*sonnet*"           model="claude-sonnet-4-5[1M]" => true
```

写通配的路由能中，写精确模型名的路由直接不匹配。而且 `target.Model` 为空时
`targetModel = model`（`router.go:48`），后缀会**原样转发给上游**——上游不认这个
本地标记。这是本次改动之前就存在的缺口，不是新引入的。

**二、后缀声明的窗口与配置值冲突。** 客户端说要 1M，配置里 `contextWindow: 200000`。
按配置算预算会把本可用的输出上限压掉。带后缀时 `contextWindow` 视为 `1000000`，
覆盖三层配置值。

处理：转发前剥离后缀（含 `[1M]` / `[1m]` / 带空格变体），**路由匹配也用剥离后的
名字**，同时把「是否带后缀」作为窗口覆盖信号传下去。

## 5. 裁决点与数据流

```
handle()
  ├─ vision 翻译（循环外，已有）
  ├─ estimated := tokenest.Estimate(internal)     ← 新增：估算一次，所有候选共用
  └─ for pos := range order:
       ├─ breaker.Allow()                          ← 已有：跳过不消耗额度
       ├─ resolveContextBudget(candidate, estimated, margin)   ← 新增
       │    ├─ candidate.ContextWindow == nil → 不启用，budget 无限制
       │    ├─ budget < MinOutputBudget(1024) → 跳过该候选（contextSkips++）
       │    └─ 否则得到 budget
       ├─ forwardAttempt(..., contextBudget: budget)
       │    └─ 落地点 main.go:830 附近：
       │         effective := candidate.MaxTokens 或客户端值/默认值
       │         if budget > 0 && effective > budget { effective = budget }
       │         OverrideMaxTokens(upstreamMap, format, effective)
       └─ ...
```

### 5.1 为什么估算在循环外

输入对所有候选完全相同（vision 翻译已完成，`Internal` 不再变化）。循环内重复估算
是纯浪费；更重要的是**估算值必须一致**，否则同一请求对不同候选算出不同输入量，
排查时对不上。

### 5.2 为什么裁决在 forwardAttempt 之前

跳过要不消耗 `maxAttempts` 额度，必须在调 `forwardAttempt` 之前决定——进了
`forwardAttempt` 就已经算一次尝试了。这与 `breaker.Allow` 的位置同理。

### 5.3 EnsureMaxTokens 路径的处理

三层都没配 `maxTokens` 时走 `EnsureMaxTokens`（只在字段缺失时补默认值）。此时
budget 也要生效：客户端传了 `max_tokens: 64000` 而 budget 只有 8k，必须压到 8k。

所以 budget 生效点不能只挂在 `OverrideMaxTokens` 分支上。改成：

```
// 伪代码，实际实现由 Codex 决定
effective, hasValue := 解析出本次要下发的值（三层配置 > 客户端已有值 > 默认值）
if budget > 0 && effective > budget { effective = budget }
OverrideMaxTokens(...)  // 有了明确值就统一走 Override，不再需要 Ensure 分支
```

**注意**：这是对现有 `Override`/`Ensure` 二分的调整，要保住原有语义——
「三层都没配且客户端传了值」时不能把客户端值改掉（除非超 budget）。

## 6. 终态

候选循环收尾的 switch（`main.go:648`）新增一个分支，插在 `freeSkips` 之后：

```
case attempts == 0 && contextSkips > 0:
    reqLog.Error = "全部候选上游的上下文窗口均装不下本次请求"
    writeJSONError(w, http.StatusRequestEntityTooLarge, "context_window_exceeded", ...)
```

用 `413` 而不是 `400`：语义就是「请求太大」，客户端（Claude Code / Codex）看到 413
才可能触发自己的上下文压缩，收到 400 只会当协议错误重投。

错误信息里带上估算值与各候选窗口，便于排查：
`估算输入 ~198000 token，候选窗口 glm=200000/margin=4096，可用输出预算 -96`。

### 6.1 混合跳过的优先级

熔断跳过 + 装不下跳过同时存在时，现有 switch 是按顺序匹配的。`breakerSkips > 0`
在前，会先命中熔断分支。这个顺序**保持不变**：熔断是上游故障（503 更准），
装不下是请求问题（413），故障优先报。

## 7. 观测

- `metrics.AttemptDetail` 新增 `Kind: "context_skip"`，`Reason: "context_window_exceeded"`，
  与现有 `breaker_skip` 同构。
- trail 里追加 `<provider>:context_exceeded`。
- 请求日志新增字段记估算输入量（`EstimatedInputTokens int`），前端日志详情展示。

## 8. 热重载

`contextWindow` 三层字段随 `/api/config` 落盘并热重载。它只参与**每请求的计算**，
不持有任何状态，因此 `applyRuntimeConfig` **不需要**新增 `Reconcile`——与
`maxTokens` 一样，下一个请求自然读到新值。

`contextSafetyMargin` 同理。

## 9. 前端

三处输入框 + 6 个文件，与 `maxTokens` 完全同构（参照 `88434a5` 的改法）：

| 文件 | 改动 |
|------|------|
| `src/index.template.html` | provider 表单（~1418 行附近）、route 表单（~1562）、target 行内（~1531）各加一个 number 输入框；顶层设置加 `contextSafetyMargin` |
| `src/app/00-state.js.part` | 表单初值 `contextWindow: null` |
| `src/app/02-config-normalize.js.part` | 三层 `pick(x, 'contextWindow', 'ContextWindow') ?? null` |
| `src/app/11-providers.js.part` | provider 表单读写 |
| `src/app/12-routes.js.part` | route / target 读写，`newRouteTarget` 加参数 |
| `src/app/10-preview-yaml.js.part` | YAML 预览三处输出 |
| `src/app/07-nav-payload.js.part` | 导航 payload |

**改完必须跑 `make web`（两个产物都要）**，否则 CI 的 `Verify built stylesheet` 或
`Verify built index.html` 会失败——本仓库已连续三次踩这个坑（两次 index.html、
一次 tailwind.css）。新加的 class 若不在现有 CSS 里（如新的宽度类），
`make web-css` 是必须的。

## 10. 不做的事

- **不做上下文裁剪**：不丢弃、不摘要任何消息。会破坏 prompt cache 前缀（会话粘性
  就白做了）、可能拆散 `tool_use`/`tool_result` 配对，且 Claude Code / Codex 自己
  已在管上下文，两边同时裁会打架。
- **不引入 tokenizer 依赖**：`tiktoken-go` 只对 OpenAI 系准，对 GLM / DeepSeek /
  Claude 都是错的，还给纯 Go 静态构建增体积。
- **不调上游 count_tokens 接口**：每请求 +1 RTT，且不是所有 provider 都有。
- **不改 `truncation` 透传行为**：`converter.go:1193` 那处保持原样。
- **不做反应式矫正**（评估后放弃，证据见下）。

### 10.1 为什么不做反应式矫正

CC-Switch 对同一个问题给的是另一种答案：不预判，先原样转发，等上游返回 400 后按
错误信息改写请求体再重试（`thinking_budget_rectifier.rs`）。评估后不采纳，两条理由：

**一、性能上不占优。** 实测本方案的估算器开销（容器内实跑）：

| 输入 | 估算结果 | 单次耗时 |
|------|----------|----------|
| 英文 800KB | ~200k token | 443µs |
| 中文 800KB | ~182k token | 834µs |
| 英文 4MB | ~1M token | 2.2ms |

预判式每请求恒定约 0.5ms；反应式命中时多一整个 RTT（LLM 请求几百 ms 到几秒）。
分水岭在「会撞上限的请求占比」约 **0.05%**。而 `88434a5` 那次事故里 469 条响应有
71 条齐聚硬上限指纹（15%），差三个数量级。

**二、反应式拿不到正确的上限值。** 它依赖解析上游错误文本，而各家措辞不统一。
CC-Switch 为了认**一家厂商的一个错误**就匹配了三种变体：

```rust
lower.contains("greater than or equal to 1024")
    || lower.contains(">= 1024")
    || (lower.contains("1024") && lower.contains("input should be"))
```

GLM 返回的是 `{"code":"1210","message":"max_tokens 字段值无效"}`——形状完全不同，
**且不含上限值**。所以反应式照样得猜（CC-Switch 硬编 32000 / 64000），只是在烧掉
一个 RTT 之后猜。而本方案的窗口是用户在配置里明确写的，属于已知量。

**三、反应式做不到换候选。** 400 回来时 RTT 已经花在错的上游上了，要转移得重进候选
循环。预判式跳过是免费的（见 5.2），而「跳过装不下的、试窗口更大的」正是本次要的行为。

估算误差这唯一的短板，用图片固定计值（4.2）解决，比整条重试路径便宜得多。

### 10.2 外部印证

CC-Switch issue [#2937](https://github.com/farion1231/cc-switch/issues/2937)（OPEN，
label `proxy`）报的正是本次要解决的问题：Claude Desktop 硬编 `max_tokens: 64000`，
GLM 的 glm-4.6 上限 32000，代理层原样透传，上游 400 报 `1210`。报告者的诉求是
「代理层应该知道每个模型的输出上限，转发前把超限的值钳到模型上限」，绕过办法是
**再自建一个本地代理**做钳制。

我核对了 CC-Switch 的 `model_mapper.rs`：它只做模型名替换，不带任何模型能力数据
（没有窗口、没有输出上限），所以确实没法按模型钳制——issue 至今 OPEN 与此一致。
本次的三层 `contextWindow` 配置就是在补这块能力，方向上得到外部印证。

> 说明：我曾用 `gh search code` 反查该仓库是否有窗口相关实现，但那批搜索连已知存在的
> 符号（`MAX_THINKING_BUDGET`）都返回空，工具本身不可靠，因此上述结论只基于实际读过
> 的文件与 issue 状态，不基于搜索的阴性结果。
