# DESIGN.md — 本地重试（localRetry）

## 1. 需求

某些上游 provider 对特定模型偶发失败（连接失败、5xx、限流等瞬时故障），连续请求几次就能成功。需要能按 **provider / 路由 / 候选(target)** 配置：在把请求转移到下一个候选之前，先对**同一个候选**重试若干次（每次间隔固定时长），仍失败才交给 failover 换下家。

用户已确认：
1. 触发重试的失败类型**复用现有 failover 的同一套码表**（同样的 onTransportError / onServerError / onRateLimit / onQueueTimeout / onStreamHeaderTimeout / onRequestTimeout / onAuthError 判据）。
2. 间隔为**可配置的固定时长**（不做退避）。
3. 与 maxAttempts / 熔断的关系按 Claude 推荐（见 §5）。

## 2. 与现有 failover 的关系

- failover：候选 A 失败 → **换候选 B**（不同 provider/model）。
- localRetry：换候选之前 → 对**候选 A 自身**重试 N 次（带间隔），N 次仍失败才交给 failover 换下家。

两者正交。执行顺序：`熔断准入 → contextWindow 裁决 → [localRetry 内层循环：转发 → 失败则等待重试] → failover 换候选`。

## 3. 关键约束：只能重试「响应尚未开始」的失败

localRetry 复用现有放弃机制（`proxy.ShouldRetry` → `ErrAttemptAbandoned`），该机制只在**尚未向客户端写入任何字节**时触发。因此：

- 可重试：连接失败、响应头超时、非流式整体超时、上游返回非 2xx（普通 4xx 业务错误不在码表内，不重试）、429。
- 不可重试：流式请求上游已回 200、字节已开始下行后中途断开——已写出的内容收不回，与现有 failover 转移边界完全一致。

正好覆盖用户描述的「连着请求几次才成功」（失败发生在响应开始之前）。

## 4. 配置模型（三级覆盖，与 maxTokens/extraBody 一致）

新增结构体 `config.LocalRetry`：

```go
type LocalRetry struct {
    // MaxRetries 同一候选的额外重试次数；nil=继承下层，0=在本层显式关闭。
    MaxRetries *int `yaml:"maxRetries,omitempty" json:"maxRetries,omitempty"`
    // IntervalMs 每次重试前等待毫秒；nil=用默认 defaultLocalRetryIntervalMs。
    IntervalMs *int `yaml:"intervalMs,omitempty" json:"intervalMs,omitempty"`
}
```

在 `Provider` / `Route` / `Target` 三处各加 `LocalRetry *LocalRetry`（omitempty）。

**字段级三级覆盖**（target > route > provider，逐字段取第一个非 nil），与 maxTokens/contextWindow 一致。router 新增 `resolveLocalRetry(target, route, provider)`，合成到 `router.Candidate`：

- 有效 maxRetries = 逐层第一个非 nil 的 `.LocalRetry.MaxRetries`；≤0 或全 nil → 关闭（Candidate 记为 0）。
- 有效 intervalMs = 逐层第一个非 nil 的 `.LocalRetry.IntervalMs`；nil 时用默认 1000ms（仅在 maxRetries>0 时有意义）。

`*int` 语义：nil=继承；显式 0 的 MaxRetries=在本层关闭（覆盖下层的开启）。用指针而非值类型正是为了区分「本层想关」与「没配、继承下层」。

Candidate 新增字段（建议）：`LocalRetryMax int`（0=关闭）、`LocalRetryIntervalMs int`。

## 5. 与 maxAttempts / 熔断 / 队列的关系（Claude 推荐，含对初步倾向的修正）

- **maxAttempts（failover 额度）**：localRetry **不消耗**。maxAttempts 约束「换了几个候选」，同一候选的自我重试不算换候选；一个候选无论本地重试多少次，只在放弃它（或成功）时计一次 failover 尝试。
- **熔断上报（修正）**：**每个候选访问只上报一次** `breaker.Report`，用本地重试**结束后的最终结果**。
  - 修正原因：`breaker.Allow` 在 half_open 会借出一个探针额度（probesInFlight++），必须由**恰好一次** `Report` 归还（见 main.go:782 附近注释）。若每次本地重试都 Report，一次 Allow 对应多次 Report，会打乱半开探针计数、把 provider 永久卡在「探针已满」。故不能按最初说的「每次失败都上报」；改为聚合成一次，粒度与今天「一次请求对一个候选上报一次」一致。
- **队列**：`forwardAttempt` 用 defer 收口队列 slot 的 Acquire/release，每次本地重试是一次独立的 Acquire→forward→release；间隔等待发生在两次 forwardAttempt **之间**（不持有 slot）。directMode 无队列，逻辑同样成立。

## 6. failoverReason 重构：把「码表」与「failover.enabled 开关」解耦

localRetry 需要在 **failover.enabled=false 时也能工作**（常见场景：只有一个 flaky provider、没开 failover，但想让它多试几次）。而现在 `failoverReason` 顶部有 `if !f.Enabled { return {} }`，enabled=false 时一切都判成不可转移。

方案：抽出**不含 enabled 门禁**的分类函数（沿用同一套 `TransferOnXxx` 判据与 freeAttempt 逻辑）：

```
classifyFailure(f, upstreamCode, retryAfter, err) failoverDecision
    // 只判「这类失败按码表该不该重试/转移」，不看 f.Enabled
failoverReason(...) =
    if !f.Enabled { return failoverDecision{} }
    return classifyFailure(...)   // 对外语义不变，现有 failover 测试不受影响
```

- failover 转移仍走 `failoverReason`（enabled 门禁不变）。
- localRetry 走 `classifyFailure`，判据：`decision.transfer && !decision.freeAttempt`。
  - 排除 `freeAttempt`（429 + Retry-After 超过 maxRetryAfterMs）：上游明确自报「这段时间不可用」，本地重试等它没意义，交回 failover 直接换候选（保持现有 free-skip 行为）。

## 7. 主循环改造（cmd/gateway/main.go 候选循环）

候选循环内、`forwardAttempt` 外层加一层本地重试内循环（伪代码，非最终实现）：

```
for pos := ...; attempts < attemptLimit; pos++ {          // 外层：候选
    candidate := ...
    // provider 禁用跳过、breaker.Allow、contextWindow 裁决 —— 每候选一次，不变
    localBudget := candidate.LocalRetryMax
    var outcome forwardAttemptOutcome
    for {                                                 // 内层：本地重试
        hasNext := attempts+1 < attemptLimit && pos+1 < len(order)
        detail := 新建 AttemptDetail（每次转发一条）
        outcome = s.forwardAttempt(..., forwardAttemptInput{
            allowFailoverTransfer: hasNext,               // enabled 由 forwardAttempt 内部叠加
            allowLocalRetry:       localBudget > 0,
            attemptNo:             attempts + 1,          // failover 尝试号，本地重试期间恒定
            httpAttemptNo:         nextHTTPAttemptNo + 1,
            ...
        })
        追加 detail；outcome.requestStarted 时 nextHTTPAttemptNo++
        if outcome.buildErr != "" { break }
        if outcome.abandoned && !outcome.freeAttempt && localBudget > 0 {
            localBudget--
            记一条 trail（如 name:local_retry）
            if !sleepCtx(r.Context(), interval) { break }  // 客户端断开：停止重试
            continue                                       // 重试同一候选
        }
        break
    }
    // —— 每候选访问只跑一次的既有处理 ——
    if outcome.buildErr != "" { breaker.Report(Ignored); ...; continue }
    if s.breaker != nil { s.breaker.Report(name, outcome.breakerOutcome) }  // ← 从 forwardAttempt 后移到此，保证每候选一次
    if outcome.freeAttempt { freeSkips++; ...; continue }
    attempts++
    ... 既有 success / abandoned 处理不变 ...
}
```

forwardAttempt 内部 `ShouldRetry` 改为（替换现有 `if in.allowRetry {...}`，把 `allowRetry` 拆成两个标志）：

```
d := classifyFailure(&cfg.Failover, code, retryAfter, err)
failoverOK := cfg.Failover.Enabled && in.allowFailoverTransfer && d.transfer
localOK    := in.allowLocalRetry && d.transfer && !d.freeAttempt
if !failoverOK && !localOK { return false }
abandonReason, abandonBreaker, abandonFree = d.reason, breakerOutcomeFor(code, err), d.freeAttempt
return true
```

**客户端断开**：间隔等待必须 `select { case <-time.After(interval): case <-r.Context().Done(): }`，断开时立即停止重试，不空睡。

## 8. 观测

- 每次本地重试都产生独立的 `metrics.AttemptDetail`（trail 里能看到该候选被试了几次），用一个可区分标记（如 `Reason:"local_retry"` 或 AttemptDetail 新增 `LocalRetry bool`）。
- `x-ai-gateway-attempts` 头维持 = failover 尝试号语义不变（本地重试是子尝试，不抬该计数）。
- v1 不新增顶层 metrics 计数，AttemptDetails 已足够；如需要再议。

## 9. 校验与默认值

- `applyDefaults`：**不物化** LocalRetry（同 Enabled / OneMContext，避免每次 PUT 落噪音）。intervalMs 的默认 1000 只在 §4 的解析期套用。
- `validate`：新增 `validateLocalRetry(label, *LocalRetry)`，在 validateProvider、route 循环、每个 target 三处调用（对齐 validateExtraBody 的调用点）：
  - MaxRetries 非 nil：`0 ≤ v ≤ maxLocalRetries`（建议 10）。0 合法（=关闭），与 maxAttempts 不同（那里 0 非法），因为「不重试」是本功能的自然取值。
  - IntervalMs 非 nil：`0 ≤ v ≤ maxLocalRetryIntervalMs`（建议 60000）。0 = 立即重试。
- 常量放在 config.go 现有 `default*` 常量块附近：`defaultLocalRetryIntervalMs=1000`、`maxLocalRetries=10`、`maxLocalRetryIntervalMs=60000`。

## 10. 前端（必做，非可选——原结论已纠正）

> 初稿误判为「可选」。实测 `TestFrontendCoversAllProviderFields` / `TestFrontendCoversAllRouteFields` 是仓库护栏：**每个后端 config 字段都必须出现在前端所有重建点**，否则保存时被静默丢弃、测试直接失败。因此 `localRetry` 的前端串接是测试强制项。

需把 `localRetry`（嵌套对象 `{maxRetries, intervalMs}`，两者为数值或 null；两者皆空时整体 omit，语义参照 `contextWindow` 的 null/omitempty 与 `vision` 的嵌套对象）串进 `cmd/gateway/web/src/*.js.part` 的 provider / route / target 全部重建点：`00-state`（空表单）、`02-config-normalize`（normalizeConfig，含 routes 与其 targets）、`07-nav-payload`（configPayload）、`10-preview-yaml`（YAML 预览）、`11-providers`（editProvider / duplicate / saveProvider / 空表单）。并在对应编辑表单加 maxRetries / intervalMs 输入项。改后**重建 `index.html` 产物**并确认 `webbuild -check` exit=0（否则 `TestRepoArtifactMatchesSources` 失败）。

## 11. 不做 / 边界

- 不做退避（用户明确固定间隔即可）。
- 不对流式已开始的响应重试（§3）。
- 不改 failover.enabled 的对外语义（§6 只是抽函数）。
- freeAttempt（429 超长 Retry-After）不本地重试（§6）。
- 未配置 localRetry 时，全链路行为与改动前逐字节一致。
