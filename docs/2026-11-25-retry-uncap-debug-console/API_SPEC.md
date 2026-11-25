# API_SPEC：调试控制台与相关接口变更

创建日期：2026-11-25

## 1. `POST /api/debug/run`

对已保存的配置发起一次调试请求，以 SSE 事件流返回执行过程与结果。

### 1.1 边界

- 方法：只接受 `POST`，否则 405。
- 跨站 Origin：403（与其他管理接口一致）。
- `Content-Type`：必须是 `application/json`，否则 415。
- 请求体上限：与推理请求相同，超限 413 `request_too_large`。
- 请求体严格解码：未知字段、多个 JSON 值都返回 400。

### 1.2 请求体

```json
{
  "mode": "provider",
  "clientFormat": "anthropic",
  "provider": "my-provider",
  "routeIndex": 0,
  "routeMatch": "claude-*",
  "targetIndex": 0,
  "targetProvider": "my-provider",
  "body": { "model": "claude-x", "messages": [] }
}
```

| 字段 | 类型 | 适用模式 | 说明 |
|---|---|---|---|
| `mode` | string | 全部 | `provider` / `route` / `target` |
| `clientFormat` | string | 全部 | `anthropic` / `openai-chat` / `openai-responses` |
| `body` | object | 全部 | 该客户端格式下的完整请求体，必须含非空字符串 `model` |
| `provider` | string | `provider` | 已保存配置中的 Provider 名称 |
| `routeIndex` | int | `route`、`target` | 路由在已保存配置 `routes` 中的下标 |
| `routeMatch` | string | `route`、`target` | 该路由的 `match`，用于确认配置未变 |
| `targetIndex` | int | `target` | 候选在该路由 `targets` 中的下标 |
| `targetProvider` | string | `target` | 该候选的 Provider 名称，用于确认配置未变 |

`body.model` 的含义：

- `provider` 模式：直接发往上游的目标模型。
- `route` / `target` 模式：客户端请求的模型名，用于路由匹配。

与模式无关的字段出现时返回 400。

### 1.3 进入事件流之前的错误

以下错误以普通 JSON 错误响应返回（与现有 `writeJSONError` 同形），不进入 SSE：

| 状态 | 错误类型 | 触发条件 |
|---|---|---|
| 400 | `debug_invalid_request` | 请求体不合法、字段缺失或取值非法、`body` 不是对象或缺少 `model` |
| 404 | `debug_target_not_found` | Provider 不存在，或路由 / 候选下标越界 |
| 409 | `debug_config_changed` | `routeMatch` 或 `targetProvider` 与已保存配置对应位置不一致 |

### 1.4 事件流

校验通过后响应 `200`，`Content-Type: text/event-stream`，`Cache-Control: no-store`。每条事件格式为 `event: <名称>` + `data: <单行 JSON>` + 空行。事件按下列顺序出现：

| 事件 | 次数 | 数据 |
|---|---|---|
| `meta` | 1 | 本次调试的解析结果 |
| `upstream_request` | 0..N | 每一次真实发出的上游请求（含本地重试、换密钥） |
| `response_start` | 0..1 | 链路写给客户端的状态码与响应头 |
| `response_chunk` | 0..N | 链路写给客户端的响应正文分片 |
| `done` | 1 | 结束，附本次请求日志 |

`upstream_request` 可以出现在 `response_start` 之前或之间（多次尝试时）。`done` 一定是最后一条。

**`meta`**

```json
{
  "requestId": "…",
  "mode": "route",
  "clientFormat": "anthropic",
  "model": "claude-x",
  "route": "claude-*",
  "strategy": "failover",
  "candidates": [
    { "provider": "a", "targetModel": "m1", "providerFormat": "anthropic", "enabled": true }
  ]
}
```

- `provider` 模式下没有 `route` / `strategy`，`candidates` 恰好一项。
- 路由解析失败（未命中或命中其他路由）时仍发出 `meta`，`candidates` 为空，随后的 `response_start` 给出对应错误状态。

**`upstream_request`**

```json
{
  "sequence": 1,
  "provider": "a",
  "targetModel": "m1",
  "providerFormat": "anthropic",
  "method": "POST",
  "url": "https://example.com/v1/messages",
  "headers": { "Authorization": "<已脱敏>", "User-Agent": "…" },
  "body": "{…}",
  "bodyTruncated": false
}
```

- `sequence` 与同一次尝试的 `AttemptDetail.Sequence` 一致。
- 脱敏规则见 `DESIGN.md` §B6。`body` 是字符串，上限 256 KiB。

**`response_start`**

```json
{ "status": 200, "headers": { "Content-Type": "text/event-stream", "X-Ai-Gateway-Provider": "a" } }
```

内容与真实客户端在同一请求下会收到的状态码和响应头一致。

**`response_chunk`**

```json
{ "data": "event: message_start\ndata: {…}\n\n" }
```

`data` 是 UTF-8 文本，分片边界不保证与上游事件对齐，但保证不切断多字节字符。把全部 `data` 按序拼接即为真实客户端会收到的完整响应正文。

**`done`**

```json
{ "log": { "…": "与 /api/logs 单条记录同形" }, "aborted": false }
```

- `log` 包含状态码、耗时、首字耗时、实际 Provider 与目标模型、尝试次数、轨迹、尝试明细、`debug: true`、`debugMode`。
- 客户端中途断开时服务端无法送达 `done`，前端按「已中止」处理。

### 1.5 事件流内的调试专属错误

以下情况通过 `response_start` + `response_chunk` 表达，正文与现有网关错误 JSON 同形：

| 状态 | 错误类型 | 触发条件 |
|---|---|---|
| 422 | `debug_route_mismatch` | `route` / `target` 模式下，模型名实际命中的路由不是所选路由。消息写明实际命中的路由 |

其余错误（`route_not_found`、`conversion_error`、`context_window_exceeded`、`no_selectable_provider_key`、`breaker_open`、上游 4xx/5xx 等）与真实链路的状态码和正文一致。

### 1.6 各模式对运行时状态的影响

| 项 | `provider` | `route` | `target` |
|---|---|---|---|
| 路由匹配、策略、粘性 | 不参与 | 照常 | 匹配照常；不轮转、不写粘性 |
| 故障转移 | 无 | 照常 | 无（只有一个候选） |
| 本地重试 | 无 | 照常 | 照常（三层覆盖值生效） |
| Provider 停用 | 忽略，照样发 | 照常跳过 | 照常跳过 |
| 熔断准入与上报 | 不准入、不上报 | 照常 | 照常 |
| 密钥选择 | 可换密钥，遵守已有冷却 | 照常 | 照常 |
| 密钥冷却更新 | 不更新 | 照常 | 照常 |
| 队列限速 | 照常 | 照常 | 照常 |
| 图片识别 | 不做 | 照常 | 照常 |
| 请求日志 | 写入，带调试标记 | 写入，带调试标记 | 写入，带调试标记 |
| `/api/metrics` 聚合 | 不计入 | 不计入 | 不计入 |

## 2. `GET /api/logs` 变更

- 新增查询参数 `debug`：`only` 只返回调试记录，`exclude` 排除调试记录，缺省或空返回全部。其他取值返回 400。
- 单条记录新增字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `debug` | bool | 调试请求为 `true`，否则缺席 |
| `debugMode` | string | `provider` / `route` / `target`，非调试请求缺席 |
| `attemptDetailsOmitted` | int | 被折叠掉的尝试明细条数，为 0 时缺席 |

- `attemptDetails` 最多 40 条（头 20 + 尾 20）。`attemptTrail` 最多 40 段，中间以 `…省略 N 段…` 连接。

## 3. `GET /api/metrics` 变更

调试记录不计入任何聚合：总请求数、窗口请求数、成功率、延迟分位、状态码分布、Provider 统计、图片识别统计、最近错误、最近图片识别失败。响应结构不变。

## 4. 配置变更

`localRetry.maxRetries`（Provider / 路由 / 候选三层）：取值范围由 `0–10` 改为任意非负整数。负数报错信息为 `<label>.maxRetries 不能为负数`。配置结构与字段名不变，旧配置无需迁移。
