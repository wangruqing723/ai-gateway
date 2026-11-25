# DESIGN：去掉本地重试次数上限 + 调试控制台

创建日期：2026-11-25

本需求包含两部分，互相独立，可分别实现与验收：

- **A. 去掉本地重试次数上限**（优化）
- **B. 调试控制台**（新增功能）

## 0. 已确认的决策（使用者 2026-11-24 逐条确认，全部按 Claude 倾向）

| # | 决策 |
|---|---|
| 1 | Provider / 路由 / 候选三层的 `localRetry.maxRetries` 一起去掉上限。`intervalMs`（0–60000）与 `failover.maxAttempts`（1–6）不动。 |
| 2 | 「无上限」只表示放开为任意非负整数，不新增「无限重试」专用写法。 |
| 3 | 每个请求的尝试明细只保留头 20 条 + 尾 20 条，中间折叠并记录省略数量；轨迹文本同样折叠。 |
| 4 | 调试控制台里的「格式」指客户端协议格式：`anthropic` / `openai-chat` / `openai-responses`。上游格式仍由 Provider 配置决定。 |
| 5 | Provider 测试是「隔离单发」：不转移、不本地重试、不上报熔断、不更新密钥冷却；停用或熔断中的 Provider 也能测；仍走队列限速；多密钥时可换密钥并遵守已有冷却。 |
| 6 | 路由测试走完整真实链路，效果等同一次真实请求。另提供「指定候选」模式：套用该候选的三层覆盖值，但不转移。 |
| 7 | 调试请求写入请求日志并打「调试」标记（可筛选），不计入监控指标。 |
| 8 | 请求编辑：简易表单 + 可切换的原始 JSON。工具调用、图片、多轮对话只走原始 JSON。 |
| 9 | 响应展示状态码、耗时、首字耗时、命中的 Provider 与模型、尝试轨迹、用量；正文分「渲染文本」与「原始响应」；可中止；展示脱敏后的实际上游请求体与请求头。 |
| 10 | 左侧导航新增「调试」页签；提供商卡片与路由行各加「调试」按钮，跳转并预填。 |
| 11 | 只针对已保存的配置测试。 |

---

## A. 去掉本地重试次数上限

### A1. 上限的来历（调查结论）

- 位置：`internal/config/config.go` 的 `maxLocalRetries = 10`，由 `validateLocalRetry` 统一校验三层。
- 出处：提交 `50aedd3`（2026-09-25）。原设计 `docs/_archive/2026-09-24-local-retry/DESIGN.md` §9 只写「建议 10」，没有论证。
- 运行时没有任何逻辑依赖该值不超过 10。它只是顺带挡住了两件事：单请求最坏耗时、单条请求日志的体积。

### A2. 配置校验

- 删除 `maxLocalRetries` 常量与上界判断，只保留「不能为负数」。
- 错误信息改为 `<label>.maxRetries 不能为负数`。
- `intervalMs` 的 0–60000 校验保持不变。
- `applyDefaults` 仍不物化 `LocalRetry`。

### A3. 尝试明细与轨迹折叠

目标：重试次数很大时，**请求进行期间**的内存和最终日志体积都有界。不能等请求结束再裁剪。

- 常量：头部保留 20 条，尾部保留 20 条。
- `metrics.RequestLog` 新增 `AttemptDetailsOmitted int`（JSON `attemptDetailsOmitted,omitempty`），表示被折叠掉的明细条数。
- 所有往 `reqLog.AttemptDetails` 追加的位置改为走同一个追加入口。条数超过 40 时丢弃第 21 条（头部之后最旧的一条），并把省略计数加一。保留下来的明细 `Sequence` 不改写，前端凭序号断档定位折叠处。
- 轨迹（`AttemptTrail`）同样在追加阶段折叠：保留头 20 段 + 尾 20 段，中间插入一段 `…省略 N 段…`。
- 明细不超过 40 条的请求，日志 JSON 必须与改动前逐字节一致。
- 已知限制：`/api/logs` 的 `attemptProvider` / `attemptStatus` / `attemptOutcome` 筛选只能匹配保留下来的明细。

### A4. 前端

- 三处输入框（Provider 表单、路由表单、候选行）去掉 `max="10"`。
- 提示文案改为：次数不设上限；次数很大时每次失败都会完整重试一轮，只有成功、次数用尽或客户端断开才会停，建议配合间隔使用。
- 请求日志详情：`attemptDetailsOmitted > 0` 时，在第 20 条之后插入一行「已省略 N 条尝试明细」。

### A5. 风险（已告知使用者，不加额外保护）

- `maxRetries` 很大且 `intervalMs: 0`，遇到快速失败的上游会形成紧循环，直到客户端断开。这是放开上限的直接后果，按使用者决策不设保护，只在界面文案里提示。
- 每次重试仍会向标准输出打一行日志，容器日志量随重试次数增长。

---

## B. 调试控制台

### B1. 三种模式

| 模式 | 值 | 语义 |
|---|---|---|
| Provider 测试 | `provider` | 绕过路由，对指定 Provider + 目标模型隔离单发 |
| 路由测试 | `route` | 走完整真实链路，等同一次真实请求 |
| 指定候选 | `target` | 走真实链路，但候选列表收窄为所选的那一个 |

### B2. 后端总体结构

- 新入口 `POST /api/debug/run`，契约见 `API_SPEC.md`。
- 新文件 `cmd/gateway/debug_console.go`：请求解析与校验、信封写入器、Provider 模式的执行逻辑。
- 把 `server.handle()` 中「推理请求」那一段（从生成 `reqID` 到函数结束）抽成可复用的函数，真实流量与调试入口共用。真实流量路径的行为必须逐字节不变，现有测试不改断言即全部通过。

抽出的函数需要三个可选注入点，全部为空时即真实流量：

1. **路由解析**：默认 `router.MatchRoute`。调试入口注入自己的解析函数，用于路由不符校验与候选收窄。
2. **调试标记**：非空时给 `reqLog` 打调试标记与模式。
3. **上游请求采集回调**：非空时透传给每一次 `forwardAttempt`。

### B3. 信封写入器

调试入口把链路写给客户端的内容包成自己的 SSE 事件流（事件定义见 `API_SPEC.md`）。

- 实现 `http.ResponseWriter`，并满足流式转发路径实际用到的全部可选接口（至少 `http.Flusher`；以 `internal/proxy` 的真实用法为准）。
- 首次 `WriteHeader` 或 `Write` 时发出 `response_start`，之后每次 `Write` 发出一条 `response_chunk`。
- `response_chunk` 的数据是 JSON 字符串。一次 `Write` 可能切在多字节字符中间，写入器要把不完整的 UTF-8 尾字节留到下一次再发，结束时冲刷剩余字节。
- 每条事件写完立即 `Flush`。
- 可参考 `chatgpt_diagnose.go` 的 `chatGPTDiagnoseWriter` 与 `statusRecorder`。

### B4. 路由模式与指定候选模式

注入的路由解析函数行为：

1. 用与真实链路相同的规范化模型名调用 `router.MatchRoute`。
2. 没有命中任何路由：沿用真实链路的 `route_not_found`。
3. 命中的路由不是使用者所选的那条（模型名不匹配所选路由，或被更靠前的路由先命中）：返回 422 `debug_route_mismatch`，消息里写明实际命中的路由。不强行改用所选路由，因为真实流量永远不会那样走。
4. `target` 模式：把 `Candidates` 收窄为所选下标的那一个，`Strategy` 置为 failover 等价值，避免推进轮转计数器或写入会话粘性。`RouteMatch`、图片识别相关字段保持不变。

`target` 模式收窄后就是一条单候选路由上的真实请求，因此：

- 该候选的 `maxTokens`、`contextWindow`、`extraBody`、`localRetry` 三层覆盖值照常生效。
- 熔断准入与上报、密钥冷却、图片识别、ChatGPT 账户统计都照常发生。
- 只有一个候选，自然没有转移。

`route` 模式不做任何收窄，策略、粘性、故障转移全部照常。

### B5. Provider 模式

独立执行路径，不经过候选循环，直接调用 `forwardAttempt`（先例：`diagnoseChatGPTInferencePlan`）。步骤：

1. 按客户端格式把请求体规范化为内部格式；转换失败或目标格式不兼容时返回 400 `conversion_error`。
2. 构造候选：Provider 取已保存配置的值拷贝，目标模型取请求体里的 `model`。Provider 级的 `maxTokens`、`contextWindow`、`extraBody` 以及 `[1M]` 标记的处理要与 `router.MatchRoute` 一致，建议在 `internal/router` 导出一个「按 Provider 构造单候选」的辅助函数复用同一套解析，不要在 `cmd/gateway` 里另写一份。本地重试次数固定为 0。
3. 若候选配置了 `contextWindow`，按真实链路同样的公式算预算；低于 1024 返回 413 `context_window_exceeded`。
4. 密钥：
   - 列表模式用 `keypool.Select` 按顺序选择，遵守已有冷却；没有可选密钥返回 503 `no_selectable_provider_key`（有冷却项时带 `retry-after`）。
   - 遇到密钥相关失败且还有可选密钥、切换链预算充足时换下一把，预算口径与真实链路相同。
   - **任何情况下都不调用 `keyPool.Report`**。
   - 单值模式沿用 `forwardAttempt` 内的既有解析。
5. 调用 `forwardAttempt`：`allowFailoverTransfer=false`、`allowLocalRetry=false`、`diagnostic=true`。
6. **不调用** `breaker.Allow` / `breaker.Report`，不检查 Provider 的启停状态，不做图片识别，不写会话粘性。
7. 队列：不做任何绕过，`forwardAttempt` 内的 `Acquire` 照常生效（直通模式下本来就没有队列）。
8. 结果处理：
   - 正常情况下上游响应（含 4xx/5xx）由 `forwardAttempt` 直接写给信封写入器。
   - `buildErr` → 400 `conversion_error`。
   - 账户暂停或共享冷却 → 503，消息说明原因。
   - 其他未写出响应的放弃 → 502，消息带轨迹。
9. 请求日志：与真实请求同形，`Path` 记 `/api/debug/run`，带调试标记与模式。

与既有诊断入口的差异（有意为之）：ChatGPT 账户诊断「不绕过 Provider 启停」，而调试控制台的 Provider 模式允许测试已停用的 Provider，这是使用者 2026-11-24 的明确决策。绑定账户被暂停时仍不发请求，因为该检查在 `forwardAttempt` 内部，本需求不改动。

`diagnostic=true` 的既有效果包括：不打印原文日志、不参与落盘抓包、ChatGPT 账户侧按诊断调用处理。实现时须确认这些效果都符合「隔离单发」；若发现某项会改变运行时状态或与本设计冲突，记入 `KNOWN_ISSUES.md`，不擅自改架构。

### B6. 上游请求采集

- `proxy.Options` 新增可选回调，在每一次真实发出上游 HTTP 请求之前调用，参数为最终的方法、URL、请求头、请求体。回调为空时不做任何额外工作。
- `forwardAttemptInput` 新增对应的可选字段，由调试入口注入。
- 采集到的内容作为 `upstream_request` 事件发给前端，**发出前必须脱敏**：
  - 请求头：承载凭据的头（`Authorization`、`x-api-key`、`api-key`、`Cookie`、`Proxy-Authorization`、ChatGPT 账户相关头，以及名称含 `key` / `token` / `secret` / `auth` 的头）值替换为固定占位文本。优先复用仓库已有的脱敏规则，不要另立一套判据。
  - URL：去掉 userinfo；查询参数名含 `key` / `token` / `secret` / `sig` / `password` 的值替换为占位文本。
  - 请求体：原样返回，超过 256 KiB 时截断并标记。
- 采集内容只进入本次响应的事件流，不写入请求日志，不落盘，不打印到标准输出。

### B7. 日志与指标

- `metrics.RequestLog` 新增 `Debug bool`（`debug,omitempty`）与 `DebugMode string`（`debugMode,omitempty`）。
- `Collector.Add`：调试记录只写入环形日志，不累加 `totalRequests`，不进入秒桶与 Provider 统计。
- 监控页的「最近错误」「最近图片识别失败」跳过调试记录。
- `LogFilter` 新增 `Debug`，`/api/logs` 支持 `debug=only` / `debug=exclude`，缺省返回全部。
- 说明：`route` / `target` 模式是真实链路，熔断器、密钥冷却、ChatGPT 账户统计仍会被这些请求影响，只有 `/api/metrics` 的聚合不计入。

### B8. 前端

**结构**

- 新增片段 `cmd/gateway/web/src/app/18-debug-console.js.part` 与模板中的「调试」页签区块。
- `navItems` 新增 `{ id: 'debug', label: '调试', icon: 'bolt' }`。图标只用字体子集里已有的名字（`bolt`、`close`、`content_copy`、`progress_activity` 等）。新增图标名需要重新子集化字体，本需求不做。
- 同步补齐导航标题、副标题、滚动容器映射等按页签分支的位置。
- 新增 Tailwind class 后必须 `make web-css` 重建 `vendor/tailwind.css`；优先复用现有组件类。

**表单**

- 模式切换：Provider / 路由 / 指定候选。
- Provider 模式：选择 Provider（含已停用，标注状态）+ 目标模型。目标模型复用路由表单里现有的模型选择组件（数据来自 `/api/providers/models`），同时允许手填。
- 路由模式：选择路由 + 模型名。路由匹配模式不含通配符时自动填入模型名，否则留空并把匹配模式作为占位提示。
- 指定候选模式：在路由模式基础上再选一个候选。
- 客户端格式：三选一。
- 下拉选项一律来自已保存的配置；存在未保存修改时显示提示「调试使用已保存的配置」。
- 请求编辑两种视图：
  - 简易表单：system、用户消息、流式开关（默认开）、max tokens（默认留空，留空即不传，由网关的 maxTokens 优先级链决定）。
  - 原始 JSON：从简易表单生成，可自由编辑；编辑后以 JSON 为准。JSON 不合法时禁用发送并提示。
- 简易表单按客户端格式生成请求体：
  - `anthropic`：`system` + `messages`，上限字段 `max_tokens`。
  - `openai-chat`：`messages`（含 system 消息），上限字段 `max_tokens`；流式时附带 `stream_options.include_usage: true` 以便拿到用量。
  - `openai-responses`：`instructions` + `input`，上限字段 `max_output_tokens`。
- 表单内容只存在内存里，不写入 localStorage。

**响应区**

- 概要：HTTP 状态码、总耗时、首字耗时、实际命中的 Provider 与目标模型、尝试次数、轨迹、用量。用量由前端按客户端格式从响应正文解析，解析不到就不显示。
- 视图切换：渲染文本、原始响应、上游请求、尝试明细、响应头。
  - 渲染文本：只提取文本增量；思考内容与工具调用不渲染，看原始响应。
  - 上游请求：每次真实发出的请求一条，可展开看 URL、请求头、请求体。
  - 尝试明细：复用请求日志详情的现有渲染。
- 运行中显示进度并提供「中止」，中止即关闭连接；同一时间只允许一个调试请求。
- 所有上游返回内容一律按纯文本渲染（`x-text`），不得作为 HTML 插入。

**入口**

- 提供商卡片与路由行各加一个「调试」按钮，点击后切到调试页签并预填对应的模式与对象。
- 请求日志列表：调试记录显示「调试」徽标；筛选区增加调试筛选（全部 / 仅调试 / 排除调试）。

### B9. HTTP 边界

与其他管理接口一致：只接受 `POST`；跨站 Origin 拒绝；`Content-Type` 必须是 `application/json`；请求体大小上限与推理请求相同。

### B10. 不做

- 不支持测试未保存的表单配置（已有「连接测试」）。
- 不保存调试历史，不提供请求模板库。
- 简易表单不支持工具调用、图片、多轮对话。
- 不提供指定某一把密钥的调试（已有「单密钥测试」）。
- 不新增图标字体子集。
