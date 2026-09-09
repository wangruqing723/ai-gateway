# API_SPEC：新增接口契约

## 1. `internal/tokenest`（新包）

```go
package tokenest

// Estimate 估算内部请求的输入 token 数。
//
// 纯字符启发式，零依赖：ASCII ÷ 4、非 ASCII ÷ 1.5，每条消息 +4 固定开销。
// 图片块计 0（见 DESIGN.md 4.2 的风险说明）。
//
// 返回值是估算值，误差约 ±20-30%，调用方必须自备安全余量。
// in 为 nil 时返回 0。
func Estimate(in *converter.Internal) int
```

**注意导入方向**：`tokenest` 依赖 `converter`。`converter` 不得反向依赖
`tokenest`，否则成环。若嫌这个依赖太重，可改成只接受
`Estimate(system any, messages []any, tools any) int` 的纯数据签名——
**由 Codex 选择，但必须避免环**。倾向后者（纯数据签名，零包依赖）。

### 内部常量（不导出）

```go
asciiCharsPerToken    = 4.0
nonASCIICharsPerToken = 1.5
perMessageOverhead    = 4
perImageTokens        = 1500   // 图片块固定计值，见 DESIGN.md 4.2
```

## 1.1 `[1M]` 后缀处理

放在 `internal/router`（路由匹配和转发都要用，且 `converter` 不该依赖路由概念）：

```go
// OneMContextMarker 是 Claude Code 声明 100 万上下文的本地标记。
const OneMContextMarker = "[1m]"

// OneMContextWindow 是带该标记时视为的窗口值。
const OneMContextWindow = 1_000_000

// StripOneMSuffix 剥离模型名尾部的 [1M] 标记（大小写不敏感，容忍标记前的空格）。
// 返回 (剥离后的模型名, 是否带有该标记)。无标记时原样返回。
func StripOneMSuffix(model string) (string, bool)
```

调用点两处，都在 `MatchRoute` 内：

1. `globMatch` 用剥离后的名字匹配 —— 否则写精确模型名的路由永远不中（实测见
   DESIGN.md 4.4）。
2. `target.Model == ""` 时 `targetModel` 取**剥离后**的名字 —— 否则本地标记原样发给
   上游，上游不认。

窗口解析随之变成：带标记时 `OneMContextWindow` 覆盖三层配置值。

## 2. `internal/config`

```go
// Provider 新增
ContextWindow *int `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`

// Route 新增
ContextWindow *int `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`

// Target 新增
ContextWindow *int `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`

// Config 顶层新增
ContextSafetyMargin *int `yaml:"contextSafetyMargin,omitempty" json:"contextSafetyMargin,omitempty"`
```

```go
// 新增常量
const ContextWindowCeiling = 10_000_000
const DefaultContextSafetyMargin = 4096

// 新增 accessor（默认值只留一处来源，与 AttemptLimit() 等同风格）
func (c *Config) SafetyMargin() int   // nil → DefaultContextSafetyMargin
```

`yaml` tag **必须带 `omitempty`**：落盘走 `yaml.Marshal`，缺了它每次保存都会给未配置
的路由塞一行 `contextWindow: null`。

## 3. `internal/router`

```go
// Candidate 新增
// ContextWindow 该候选的上下文窗口，nil 表示未配置（不启用预算裁决）。
// 优先级：target > route > provider，由 MatchRoute 合成。
ContextWindow *int

// 新增，与 resolveMaxTokens 同构
func resolveContextWindow(target config.Target, route config.Route, provider *config.Provider) *int
```

`Candidate.ContextWindow` 的最终值：带 `[1M]` 标记时恒为 `OneMContextWindow`，
覆盖 `resolveContextWindow` 的三层结果；无标记时用三层结果。

**注意**：覆盖只作用于 `ContextWindow`，**不动 `MaxTokens`**。客户端声明大上下文
不等于允许更大的输出，输出上限仍归三层 `maxTokens` 管。

## 4. `cmd/gateway`

```go
// 新增常量：输出预算下限。低于此值视为该候选装不下。
const MinOutputBudget = 1024

// 新增：计算该候选的输出预算。
//
// 返回 (budget, ok)：
//   - candidate.ContextWindow == nil → (0, true)，表示未配置、不限制
//   - budget >= MinOutputBudget      → (budget, true)
//   - budget <  MinOutputBudget      → (budget, false)，调用方应跳过该候选
//
// budget 可能为负（输入已超窗口），返回负值供错误信息展示。
func resolveContextBudget(window *int, estimatedInput, safetyMargin int) (budget int, ok bool)
```

`forwardAttemptInput` 新增字段：

```go
// contextBudget 本次允许的输出上限硬顶，0 表示不限制。
contextBudget int
```

## 5. `internal/metrics`

```go
// RequestLog 新增
// EstimatedInputTokens 本次请求的估算输入 token 数，0 表示未估算。
EstimatedInputTokens int `json:"estimatedInputTokens,omitempty"`
```

`AttemptDetail.Kind` 新增取值 `"context_skip"`（不是新字段，是既有字段的新取值，
与 `"breaker_skip"` / `"build_skip"` 并列）。

## 6. HTTP 终态

| 场景 | 状态码 | code | 说明 |
|------|--------|------|------|
| 全部候选装不下 | `413` | `context_window_exceeded` | 用 413 让客户端可能触发自身压缩；400 会被当协议错误重投 |

响应体沿用 `writeJSONError` 现有形状，message 里带估算值与各候选窗口。

## 7. 优先级链（完整，供实现对照）

输出上限的最终值：

```
1. 三层配置 maxTokens（target > route > provider）  ← 命中则用它
2. 客户端传入值                                      ← 三层全 nil 且客户端传了
3. converter.DefaultMaxTokens (32768)               ← 都没有
                    ↓
4. 若 contextBudget > 0，取 min(上面的结果, contextBudget)   ← 本次新增
```

第 4 步是**独立的一层压制**，作用在 1-3 的结果之上，不参与 1-3 的选择。
