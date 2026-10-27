# 流式开场预读设计

## 背景

`internal/proxy/proxy.go` 的 `handleStream` 一收到上游 2xx 响应头，就调用 `markResponseStarted` 并 `WriteHeader(200)`，之后才开始读 SSE。故障转移只能在写客户端响应头之前判定（`ShouldRetry` → `ErrAttemptAbandoned`），所以上游「先回 200、紧接着在流里报错」时没法换候选。

r00059 的上游原始流只有两个事件：

```
event: response.created   ← 上游 200，网关立即写客户端 200
event: error              ← 约 2ms 后：too_many_requests / rate_limit_exceeded
```

结果：客户端收到 200 + SSE 失败事件，`reason=conversion_error`，`breakerOutcomeFor` 把 `ErrConversion` 归为 Ignored。同一 provider 被 Azure 限流时，后续请求也都会撞上同一个错误。

## 方案：预读开场事件，推迟提交

在 `handleStream` 里，客户端响应头写出之前先加一段**预读阶段**：

```
上游 2xx 响应头
   │
   ▼  预读：逐个解析 SSE 事件，暂存原始字节，不写客户端
   ├─ 开场事件 → 继续暂存
   ├─ 失败事件 → 不提交：交给 ShouldRetry（见「失败事件处理」）
   └─ 以下任一 → 提交：写 200 头，原样冲刷暂存字节，转入原有转发逻辑
        · 第一个非开场、非失败的事件（内容 / 推理 / 工具调用 / 完成等）
        · 保持上限到时（3s）
        · 暂存超过 maxSSEEventBytes（8 MiB）
        · 上游 EOF 或读错误（之后交给原有收尾逻辑，语义不变）
```

`markResponseStarted` 和 `x-ai-gateway-*` 头的写出时机都跟着挪到提交点。放弃时不写任何东西，下一个候选可以覆盖这些头，和现有的 header 阶段放弃完全一致。

### 读取模型

预读需要「最多等 3s」，同步 `resp.Body.Read` 做不到。改为单一后台读取 goroutine，通过 channel 把数据块送给主循环。预读阶段和正文阶段共用这一个 goroutine，避免两个 goroutine 同时读 Body。活跃超时 watcher 照旧，`cancel()` 关闭 Body 后，阻塞中的 Read 会返回错误。

预读阶段客户端断开时：`ctx` 被取消，按现有约定 `clientRequestCanceled` 返回 nil，不写任何东西。

### 各格式的开场 / 失败事件

| 上游格式 | 开场事件（继续暂存） | 失败事件 |
|---|---|---|
| openai-responses | `response.created`、`response.in_progress`、SSE 注释行 | `type` 为 `error` 或 `response.failed` |
| anthropic | `message_start`、`ping`、SSE 注释行 | `type: error`（如 `overloaded_error`） |
| openai-chat | 只带 `role` 的 delta（无 content / tool_calls / finish_reason）、空 `choices`、SSE 注释行 | 顶层有 `error` 对象 |

判别只看 `data:` 行的 JSON 载荷，与 `event:` 行无关（openai-chat 没有 `event:` 行）。无法解析成 JSON 的行视为「非开场」并立即提交，保持保守：网关看不懂的东西不扣着。

### 失败事件 → 等效状态码

上游真实状态码仍是 200（`OnUpstreamStatus` 已上报，日志 `upstreamStatus` 不改）。另外按错误 `type` / `code` 映射一个**等效状态码**，交给 failover 分类和熔断判定：

| type / code | 等效状态码 |
|---|---|
| `rate_limit_exceeded`、`rate_limit_error`、`too_many_requests`、`insufficient_quota` | 429 |
| `overloaded_error` | 529 |
| `server_error`、`api_error`、`internal_server_error` | 500 |
| `service_unavailable` | 503 |
| `authentication_error`、`invalid_api_key` | 401 |
| `permission_error` | 403 |
| `invalid_request_error` 等其他 4xx 类 | 400 |
| 无法识别 | 502 |

`retryAfter` 统一传 0：SSE 错误事件里没有标准的 Retry-After 字段，不去猜。

### 失败事件处理

proxy 新增导出类型 `UpstreamStreamError{Status int; Type, Code, Message string; Raw []byte}`。

1. 调 `ShouldRetry(Status, 0, err)`。classifyFailure 走它对状态码已有的分支，所以 `onRateLimit` / `onServerError` / `onAuthError` / 本地重试等开关自然生效，不新增配置。
2. 放弃：`OnErrorBody` 记录原始事件（脱敏、限 8 KiB，复用现有逻辑），返回 `ErrAttemptAbandoned`。
3. 不放弃（最后一个候选，或开关关闭）：此时还没写响应头，所以写**真实 HTTP 错误**，状态码用等效状态码，正文是 JSON 错误对象（带上游原文 message），返回 `UpstreamStreamError`。这样客户端看到的是 429 而不是 200 + SSE 失败事件，和上游直接回 429 的情况一致，客户端自己的退避重试也能生效。

### main.go 对接

- `classifyFailure`：状态码参数已经是等效状态码，原逻辑不用改；reason 照旧是 `429/ratelimit` 之类，与 header 阶段的同类失败聚合在一起，日志筛选不会割裂。
- `breakerOutcomeFor`：最终失败路径里 `attemptStatus` 是 200。用 `errors.As` 取出 `UpstreamStreamError`，按它的等效状态码判定（429 / 4xx 不计，5xx / 502 计入）。
- `attemptFailureReason`：同样按等效状态码给出 reason，不再记 `conversion_error`。
- AttemptDetail 的 `error` 写上游原文 message，`errorBody` 写原始事件。

### 透传路径

同格式透传（含 ChatGPT OAuth、namespace 还原）也走预读。预读阶段只按行解析用来判别，暂存的是**原始字节**；提交时原样写出，再回到原有的 16 KiB 块透传或逐行还原。客户端收到的字节序列和改动前完全一样，只是开场部分晚到最多 3s。

## 默认取舍（可在确认时改）

1. **保持上限固定为 3s，不做配置项。** 实测失败事件出现在开场后 2ms~475ms，3s 留足余量。正常请求里推理 / 内容事件一到就提交，几乎不增加首字延迟。只有「开场后上游长时间不出任何事件」的请求才会多等这 3s。
2. **最后一个候选写真实错误状态码。** 这是对客户端可见的行为变化：从「200 + SSE 失败事件」变成「429 / 5xx + JSON」。
3. **日志 `upstreamStatus` 保持真实的 200。** 失败性质由 reason（如 `429/ratelimit`）和 error 原文表达。按尝试状态码筛 `429` 时，这类记录不会出现，要按 reason 或关键词找。

## 范围外

- **提交后的流内错误**（已经有内容写给客户端，之后才报错）：语义不变，仍是 `conversion_error`，不计熔断。要计入熔断需另行讨论，因为提交后的上游错误和网关转换失败目前共用 `ErrConversion`，需要先拆开。
- **预读期 EOF / 读错误**：先提交，再走原有收尾逻辑。改成可转移会改变 passthrough 下「EOF 即正常结束」的既有语义，这次不动。
