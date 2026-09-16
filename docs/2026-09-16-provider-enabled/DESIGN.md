# Provider 启用/禁用功能设计

> 本文记录的是最终落地的实现。早期规划稿曾设想「router 层过滤 + 启动期对单
> target 路由的唯一禁用目标报错 + vision 禁用报启动错误」，落地时推翻了这三条
> （理由见下文各节）。代码是唯一事实来源，本文与之对齐。

## 需求目标

在 Provider 配置项上增加可选 `enabled` 字段（`*bool`，`nil` 等同 `true`），用户
可通过前端界面或直接编辑配置文件软禁用某个 Provider；被禁用的 Provider 在候选
循环里被跳过、不消耗故障转移额度，但仍保留在配置中。

## 核心语义

### 禁用 = 软停用，运行时剔除候选

- **多 target 路由**：禁用的候选在 `cmd/gateway/main.go` 的候选循环里跳过，
  不消耗 `maxAttempts`，剩余候选接管。
- **单 target 路由且唯一目标被禁用**：启动**不报错**（禁用不该挡住网关起来），
  运行时返回 `503 all_candidates_disabled`。启动横幅会给该路由打一行
  `⚠️ 全部候选已禁用` 的零成本预警。
- **配置合法性**：禁用 Provider 仍保留在配置中，不影响网关启动或配置保存。
  `validate` 同时服务 `Load` 与 `PUT /api/config`，对禁用报错会让前端那个开关
  点不下去（保存即 400），落盘后网关反而起不来。
- **独立于熔断/健康检测**：禁用是用户显式声明，与熔断（运行时自动触发）、
  健康检测（手动/周期探测）无蕴含关系。一个 Provider 可以同时处于
  「已禁用」和「熔断打开」状态。

## 架构分层

```
配置层 (internal/config)
  ↓ Enabled *bool 加载，IsEnabled() 统一读取；applyDefaults 不物化
路由层 (internal/router)
  ↓ MatchRoute 刻意不过滤禁用候选（过滤留在候选循环，留诊断）
候选循环 (cmd/gateway/main.go server.handle)
  ↓ provider_disabled 跳过 → 503 all_candidates_disabled 终态
前端界面 (cmd/gateway/web)
  ↓ Provider 表格就地开关、监控页已禁用徽章、路由编辑器候选后缀
```

## 实现关键点

### 1. 配置字段

`internal/config/config.go` 的 `Provider` 结构体增加：

```go
Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
```

读取统一走 accessor，默认值只留一处来源：

```go
func (p *Provider) IsEnabled() bool {
    if p == nil {
        return false
    }
    return BoolOr(p.Enabled, true)
}
```

`applyDefaults` **有意不物化** `Enabled`（理由同 `OneMContext`）：物化后每次
`PUT /api/config` 保存都会给每个 provider 落一行 `enabled: true`，纯噪音。
`nil` 与 `false` 必须走不同分支，故用 `*bool`——不是为了让 `validate` 报错，
而是为了让 `nil` 承载「默认启用」。

`validate` 对 `Enabled` 不做任何校验：三种取值都合法，无可报错的边界。

### 2. 路由层不过滤

`internal/router/router.go` 的 `MatchRoute` **刻意保留**禁用候选，原样复制
Provider 进候选列表。过滤落在 `cmd/gateway/main.go` 的候选循环：

- 在 router 剔除会让候选列表变空、静默 `continue` 到下一条路由，请求最终落到
  catch-all 上一个完全不相关的模型，且诊断信息全无。
- 候选循环里跳过能记 `AttemptDetail{Kind:"provider_disabled"}` 并给出
  `503 all_candidates_disabled` 终态，日志和指标都看得到发生过什么。

`Match` 结构体新增 `VisionDisabled bool`：vision provider 被禁用时
`VisionProvider` 置 nil、`VisionDisabled=true`、`VisionModel` 仍带出（软降级
日志要说清本来该用哪个视觉模型）。

### 3. 候选循环过滤（无启动校验）

`cmd/gateway/main.go` 的 `server.handle` 候选循环里，禁用检查**优先于**熔断
判断（禁用是稳定的人工状态，比瞬时熔断更该顶在前面）：

```go
if !candidate.Provider.IsEnabled() {
    disabledSkips++
    trail = append(trail, name+":provider_disabled")
    reqLog.AttemptDetails = append(reqLog.AttemptDetails, metrics.AttemptDetail{
        Kind: "provider_disabled", Outcome: "skipped", Reason: "provider_disabled",
        Provider: name, TargetModel: candidate.TargetModel, ...
    })
    continue
}
```

跳过不消耗 `maxAttempts`，与熔断跳过、context_skip 同等待遇。终态分支
按优先级排序，禁用优先于熔断：

```go
switch {
case attempts == 0 && disabledSkips > 0:
    writeJSONError(w, http.StatusServiceUnavailable, "all_candidates_disabled",
        "全部候选上游均已禁用")
case attempts == 0 && breakerSkips > 0:
    ...  // breaker_open，带 retry-after
...
}
```

`all_candidates_disabled` **不带 `retry-after`**：人工禁用没有恢复时间，
给假数字会误导客户端按节奏空转重试。

启动横幅 `printBanner` 给「全部候选已禁用」的路由打 `⚠️ 全部候选已禁用` 警告。

### 4. 前端界面

- **Provider 表格就地开关**：`cmd/gateway/web/src/index.template.html` 的
  配置页 Provider 表头加「启用」列（10 列），行首 checkbox `@change="toggleProvider(name)"`
  就地落盘（不走编辑弹窗，禁用多发生在上游抖动、要立刻摘流量的时候）。
  禁用行整体 `opacity-50` 压暗，但开关格 `opacity-100` 抵消。
- **监控页**：`providerStateCounts()` 增加 `disabled` 计数，标题旁有
  `已禁用 N` 徽章（只在存在时出现）；`providerStatusRows()` 的状态列对禁用行
  显示「已禁用」chip 压过直通/排队/忙碌，行整体压暗但单行检测按钮保持可见可点。
- **路由编辑器候选下拉**：禁用 provider 仍可选（路由可预先配好等待恢复的
  目标），option 文本加后缀 `（已禁用）`。
- **Provider 编辑弹窗**：基本信息分区标题右侧加 enabled checkbox，
  跟随「保存 Provider」一起提交。
- 空态行 `colspan` 从 9 改成 10 与新表头对齐。

### 5. 热重载传播

`applyRuntimeConfig` 已有 `queue.Reconcile`、`breaker.Reconcile`、
`selector.Reconcile`、`httpclient.Pool` 的 `Reconcile` 逻辑。禁用 provider
在各处都被排除：队列不再为其分配执行槽、熔断器不纳入活跃集、代理池释放旧
Transport 的空闲连接。重新启用后立即可用（队列/熔断状态保留不清零）。

### 6. 健康检测与 `/v1/models` 查询

与早期规划稿「不受 `enabled` 影响」不同，落地实现按探测入口分两档：

- **周期探测 / 整表快照**（`Snapshot`、`checkAll`）：**跳过**已禁用 provider。
  禁用的常见起因恰恰是它挂了或 key 过期，继续探只会稳定失败、刷日志、
  刷异常计数。快照里也不给它留 unchecked——前端靠
  `config.providers[name].enabled` 渲染「已禁用」标记，再放一条 unchecked
  会让同一个 provider 在界面上同时是「已禁用」和「未检测」。
- **单 provider 显式检测**（`CheckProvider`）：**不跳过**。那是禁用期间
  确认上游是否恢复的唯一入口——用户点某一行的检测按钮，正是想知道
  「现在能不能把它开回来」。
- **`/v1/models` 模型列表查询**（`fetchUpstreamModels`）：不受 `enabled`
  影响。模型列表查询用于配置路由时选模型，不该被禁用拦住。

`Snapshot` 跳过禁用 provider 时先排除 nil：`IsEnabled()` 对 nil receiver
返回 false，但 nil 是配置写坏了（`checkAll` 会落一条 error 状态），跟着
跳过会把那条结论从快照里抹掉。

### 7. vision 禁用 = 运行时软降级，不报启动错误

早期规划稿曾设想「vision provider 被禁用时启动报错」，落地推翻了这条：

- vision 是可选能力，禁用 vision 不该影响纯文本请求，更不该挡住网关启动。
- `MatchRoute` 把禁用的 vision provider 标记为 `VisionDisabled`、`VisionProvider`
  置 nil，请求照常转发（带原始图片块打到目标模型）。
- `server.handle` 据此把每张图片记一次识别失败，复用现有软降级通道
  （`VisionFailCategory="other"`、`VisionFailMessage="vision provider 已禁用，跳过图片翻译"`），
  让这个降级在 `/api/metrics` 与运行异常区可见。
- `vision.CountImages` 为此新增，与 `HasImages` 同一遍历深度（顶层 content +
  一层 tool_result）。

### 8. 配置示例

`config.example.yaml` 的 Provider 注释：

```yaml
providers:
  mimo:
    baseUrl: "..."
    apiKey: "..."
    # enabled: true   # 可选：false 时该 provider 不参与转发（默认 true）。
    #                  # 候选循环里跳过，不消耗 maxAttempts；全部候选都被禁用
    #                  # 的路由运行时返回 503 all_candidates_disabled，启动不报错。
    #                  # 周期健康探测与整表快照跳过已禁用；单行手动检测仍可单独探测。
```

## 向后兼容

- 旧配置未显式写 `enabled` 时 `nil` 等同 `true`，行为不变。
- `*bool` 语义：`nil` = 未配置（默认启用）、`true`/`false` = 显式配置。
- 物化会污染存量配置，故 `applyDefaults` 不填、`omitempty` 落盘，未配置的
  provider 在 YAML 里不出现该字段。

## 风险与边界

- **单 target 路由唯一目标被禁用**：启动不报错，运行时 `503 all_candidates_disabled`，
  启动横幅打 `⚠️ 全部候选已禁用` 警告。修正路径：启用该 provider、删掉该路由、
  或加候选。
- **全部候选被禁用**：与「全部候选都熔断」同等对待，运行时返回 503，
  但终态码不同（`all_candidates_disabled` vs `breaker_open`）且不带 retry-after。
- **vision provider 被禁用**：运行时软降级，图片不翻译、请求带原图转发，
  每张图片记一次识别失败，在 `/api/metrics` 与运行异常区可见。
- **禁用与熔断并存**：禁用检查在熔断之前，禁用候选根本不会调用
  `breaker.Allow`，两者不互相消耗额度。
