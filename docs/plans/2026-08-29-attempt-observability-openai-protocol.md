# 失败尝试可观测性与 OpenAI 双协议上游支持方案

日期：2026-08-29

状态：已完成（全量 Go 验证仍受当前环境无法下载既有 YAML / SQLite 依赖所限；已完成不依赖网络的定向测试、离线编译校验与静态检查）

## 1. 结论摘要

本需求可以支持，建议拆成两个可以独立交付的能力：

1. 上游尝试可观测性：一条客户端请求继续只生成一条顶层请求日志，但增加结构化的 attemptDetails，记录每个候选上游的请求、失败、跳过、转移和最终结果。
2. Provider 级 OpenAI 协议选择：保留现有 format: openai 的 Chat Completions 语义，新增 format: openai-responses。路由实际选中的 Provider 决定发往上游的端点和请求格式；客户端格式与上游格式不同的时候，经由内部统一格式双向转换。

复杂度判断：

- 失败尝试明细与日志筛选：中等。现有故障转移循环、RequestLog、/api/logs 和前端日志表已经存在，主要工作是把字符串轨迹升级为结构化数据，并在 proxy 的错误边界采集正文。
- Responses 上游支持：中高。请求体、非流式响应和 SSE 流式响应都要补齐，尤其是 function calling、工具结果、拒答、截断和 Responses 的语义事件。
- 合计：中高，但不需要重写路由架构。当前已经有 Anthropic、OpenAI Chat、OpenAI Responses 三种客户端格式和统一中间格式，新增工作集中在 Provider 侧 Responses 分支及可观测性契约。

## 2. 当前问题与代码事实

### 2.1 截图中的 500 代表什么

截图中的请求标识为 r02040。按当前故障转移日志语义，可以看到候选 muyuan 的一次上游尝试返回 500，随后候选 100x-xn 成功返回 200。也就是说：

- 500 是中间候选上游的结果，不一定是网关最终返回给客户端的状态；
- 最终请求如果由 100x-xn 成功完成，顶层客户端响应可以是 200；
- 当前 attemptTrail 只能告诉我们类似 muyuan:500 → 100x-xn:200 的结果，不能告诉我们 muyuan 返回的错误 JSON、上游 request id、请求耗时和网关的错误分类；
- 仅凭截图无法确认 500 的根因是模型不存在、鉴权失败、上游内部错误，还是该服务不支持网关发出的端点。要确认根因，必须看到该次上游的错误正文或上游侧日志。

当前最需要避免的误判是：把 format: openai 理解成“任意 OpenAI 风格接口”。在本项目中它目前明确表示 OpenAI Chat Completions，并不表示 Responses。

### 2.2 当前客户端格式识别

converter.DetectClientFormat 按入口路径识别客户端格式：

| 客户端入口 | 内部名称 | 当前行为 |
|---|---|---|
| /v1/messages | anthropic | 解析 Anthropic 请求 |
| /v1/chat/completions | openai-chat | 解析 Chat Completions 请求 |
| /v1/responses | openai-responses | 解析 Responses 请求 |

三种客户端请求都会先规范化为 converter.Internal。因此客户端使用哪种 API，和上游使用哪种 API，是两个独立维度。

### 2.3 当前 Provider 格式识别

config.Provider.Format 当前只接受：

| 配置值 | 实际含义 | 上游路径 | 鉴权 |
|---|---|---|---|
| anthropic | Anthropic Messages | /v1/messages | x-api-key + anthropic-version |
| openai | OpenAI Chat Completions | /v1/chat/completions | Authorization: Bearer ... |

internal/proxy/upstream.go 的 buildUpstreamURL 对除了 anthropic 之外的值都回退到 /v1/chat/completions，而 forwardAttempt 对除了 anthropic 之外的请求也调用 ToOpenAIChatBodyChecked。因此当前不存在“配置为 OpenAI Responses 上游”的能力。

### 2.4 Claude Code 和 Codex 当前分别会发生什么

#### Claude Code

Claude Code 通常请求网关的 /v1/messages：

1. 网关识别客户端格式为 anthropic；
2. 请求被转换为内部格式；
3. Provider 配置为 openai 时，网关调用 ToOpenAIChatBodyChecked；
4. 实际请求上游 /v1/chat/completions；
5. 上游 Chat 响应再转换回 Anthropic 响应或 Anthropic SSE。

因此，当前是网关把 Claude Code 的 Anthropic 请求转换成 OpenAI Chat Completions 请求，不是 Claude Code 自己完成这次转换。

#### Codex

Codex 通常请求网关的 /v1/responses：

1. 网关识别客户端格式为 openai-responses；
2. FromOpenAIResponses 将 input、instructions、Responses 工具项规范化为内部格式；
3. Provider 配置为 openai 时，网关仍调用 ToOpenAIChatBodyChecked；
4. 实际请求上游 /v1/chat/completions；
5. Chat 响应或 Chat SSE 再转换回 Responses 响应或 Responses SSE。

所以当前答案是：如果 Codex 请求经过本项目，而目标 Provider 的 format 是 openai，网关会把 Responses 请求转换成上游需要的 Chat Completions 格式。

但这不是根据上游自动探测的结果，而是 format: openai 的固定语义。如果上游只实现 /v1/responses，当前配置没有正确的表达方式，最终可能因为实际请求打到了 /v1/chat/completions 而失败。

## 3. 目标配置和兼容原则

### 3.1 新增 Provider 格式

保留旧配置的含义：

~~~yaml
providers:
  chat-upstream:
    baseUrl: https://example.invalid
    format: openai
    apiKey: <CHAT_API_KEY>

  responses-upstream:
    baseUrl: https://example.invalid
    format: openai-responses
    apiKey: <RESPONSES_API_KEY>
~~~

规则如下：

- openai 永远表示 Chat Completions，不改变现有用户配置的行为；
- openai-responses 表示 Responses API；
- Provider 格式是按 Provider 配置的，不是按某一次请求临时猜测；
- 同一条路由的候选可以使用不同格式，故障转移时允许从 Chat 上游切到 Responses 上游，或反过来；
- 如果同一个厂商同时需要两个格式，初期可以配置成两个 Provider 名称，共用同一个 baseUrl 但使用不同 format；
- 不根据 /v1/models、错误码或响应体自动猜测格式。自动猜测会让失败请求多发一次、加重请求成本，也会造成重试行为不可解释。

### 3.2 请求的统一数据流

~~~text
客户端入口
  ├─ /v1/messages              -> anthropic
  ├─ /v1/chat/completions      -> openai-chat
  └─ /v1/responses             -> openai-responses
                 |
                 v
          converter.Internal
                 |
                 v
      按候选 Provider.Format 构建上游请求
  ├─ anthropic                  -> POST /v1/messages
  ├─ openai                     -> POST /v1/chat/completions
  └─ openai-responses           -> POST /v1/responses
                 |
                 v
      上游响应转换回客户端入口格式
~~~

转换选择由两部分共同决定：

- 客户端格式：由入口路径确定；
- 上游格式：由候选 Provider 的 format 确定。

不应使用“客户端是 OpenAI，所以直接把请求原样发给 OpenAI 上游”的判断。只有客户端格式和上游格式兼容时才可以透传；否则必须经过内部格式。

## 4. 三种客户端 × 三种上游协议矩阵

目标支持矩阵如下。表中的“转换”表示经过 converter.Internal，不是简单修改 URL。

| 客户端请求 | Anthropic 上游 | OpenAI Chat 上游 | OpenAI Responses 上游 |
|---|---|---|---|
| Claude Code：/v1/messages | 透传 | Anthropic → Chat 转换 | Anthropic → Responses 转换 |
| OpenAI Chat：/v1/chat/completions | Chat → Anthropic 转换 | 透传 | Chat → Responses 转换 |
| Codex：/v1/responses | Responses → Anthropic 转换 | Responses → Chat 转换 | 透传 |

“透传”仍有两个运行时动作：

- 必须替换路由后的目标模型；
- 若启用视觉翻译，必须只替换已经翻译过的消息内容，同时保留上游原生扩展字段。

因此代码层的 IsPassthrough 应明确支持：

- anthropic → anthropic；
- openai → openai-chat；
- openai-responses → openai-responses。

不能把 Provider openai-responses 当作普通 openai 走默认分支，否则请求体会错误地使用 messages，URL 也会错误地使用 /v1/chat/completions。

## 5. 失败尝试可观测性设计

### 5.1 保留一条顶层请求日志

继续维持“一次客户端 HTTP 请求对应一条 RequestLog”：

- Attempts 仍表示真实发起的上游 HTTP 尝试次数；
- Provider / TargetModel 仍表示最终实际服务客户端的候选；
- Status 仍表示网关最终返回给客户端的状态；
- AttemptTrail 继续保留，作为表格中的紧凑摘要和旧客户端兼容字段；
- 新增 AttemptDetails，记录完整的每次候选决策。

不把一次包含 muyuan:500 和 100x-xn:200 的客户端请求拆成两条顶层日志。拆分会让请求总数、成功率、Provider 统计和 P95 延迟失真：一个最终成功的客户端请求会被错误算成一条失败和一条成功。

### 5.2 AttemptDetail 建议字段

建议在 internal/metrics 中增加结构化类型：

~~~go
type AttemptDetail struct {
    Sequence           int
    AttemptNumber      int
    Kind               string
    Provider           string
    TargetModel        string
    ProviderFormat     string
    StartedAt          string
    DurationMs         int64
    UpstreamStatus     int
    Outcome            string
    Reason             string
    Error              string
    ErrorBody          string
    ErrorBodyTruncated bool
    UpstreamRequestID  string
    RetryAfterMs       int64
    FreeAttempt        bool
    ResponseStarted    bool
}
~~~

RequestLog 增加 AttemptDetails []AttemptDetail 字段，并为其定义稳定的 JSON 字段名。

| 字段 | 语义 |
|---|---|
| Sequence | 所有候选事件的顺序，包含跳过事件 |
| AttemptNumber | 真实发起 HTTP 请求时从 1 开始；熔断跳过、构建失败没有此编号 |
| Kind | request、breaker_skip、build_skip、queue_skip |
| ProviderFormat | 本次候选实际选择的 anthropic、openai 或 openai-responses |
| UpstreamStatus | 真实收到的上游响应状态；没有响应头时为 0 |
| Outcome | success、transferred、final_error、skipped、build_error 等稳定枚举 |
| Reason | 429/ratelimit、500/server、transport_error、timeout、header_timeout、conversion_error 等分类 |
| Error | 网关或传输层错误文本，不能替代 Reason |
| ErrorBody | 限量后的上游错误正文，便于定位真实 500 原因 |
| FreeAttempt | 429 且 Retry-After 超限时为 true，表示没有消耗 maxAttempts |
| ResponseStarted | 是否已经向客户端写出响应头或 SSE 字节；为 true 后不得再切换上游 |

AttemptDetails 中每一条实际 request 都是用户所说的“一个失败请求”或“一个成功请求”。其中失败的上游请求不再只能藏在字符串 AttemptTrail 中。

### 5.3 记录时机

单次候选尝试应有一个独立的 detail 生命周期：

1. 进入候选循环时创建 detail，记录 provider、目标模型、Provider 格式和开始时间；
2. 熔断拒绝时记录 breaker_skip，不增加 Attempts；
3. 请求体构建失败时记录 build_skip，不增加 Attempts；
4. 队列等待超时时记录 queue_skip，根据现有 freeAttempt 语义决定是否占用额度；
5. 真正调用 proxy 前分配 AttemptNumber；
6. proxy 收到响应头时记录上游状态码、上游 request id 和 Retry-After；
7. failover 决定放弃时将 Outcome 记为 transferred，然后进入下一个候选；
8. 最终成功时记为 success；
9. 最终没有转移的失败记为 final_error；
10. 用单次尝试的开始时间计算 DurationMs，不能复用顶层请求开始时间。

AttemptTrail 继续生成，例如：

~~~text
muyuan:500/server → 100x-xn:200
~~~

但它只做摘要。完整诊断必须从 attemptDetails 读取。

### 5.4 上游错误正文采集边界

错误正文是定位截图中 500 的关键，但不能无限制保留上游返回内容。建议：

- 非流式上游状态码大于等于 400：在向客户端转发或丢弃前，读取最多 8 KiB，写入 ErrorBody；
- 流式响应在尚未写入客户端前就收到 4xx/5xx：同样最多读取 8 KiB；
- 超过上限时保留前 8 KiB，并设置 ErrorBodyTruncated: true；
- 上游没有响应头的连接失败、DNS/TLS 失败和超时没有 ErrorBody，只记录 Error 和 Reason；
- 流式响应已经写出 message_start、response.created 或其他客户端字节后发生的读取错误，不触发故障转移，只记录该尝试的终止信息；
- 读取错误正文时要继续 drain 并关闭 response body，不能因为观测功能破坏连接复用；
- 不记录请求体、API Key、Authorization、Cookie 和完整请求头；
- 对错误正文中明显的 api_key、token、authorization 等敏感字段做脱敏，或者只保留结构化的 type / code / message；
- q 搜索默认可以命中 provider、状态、reason、error 和 request id，但是否搜索完整错误正文需要显式决定，避免把大段或敏感内容带入前端搜索。

错误正文应在 proxy 读取一次后同时服务于两个目的：最终失败时按原有语义写回客户端，发生 failover 时保存到 detail 后丢弃。不能先把 body 消费掉，再让旧的错误处理逻辑读取空 body。

### 5.5 /api/logs 查询扩展

现有 provider、status、model、stream、q 过滤器保留原义，其中 provider 和 status 仍筛选顶层客户端请求结果。新增：

| 查询参数 | 匹配对象 | 例子 |
|---|---|---|
| attemptProvider | 任意 AttemptDetails.Provider | ?attemptProvider=muyuan |
| attemptStatus | 任意 AttemptDetails.UpstreamStatus，支持状态码、4xx、5xx、error | ?attemptStatus=500 |
| attemptOutcome | 任意 detail 的稳定结果枚举 | ?attemptOutcome=transferred |

建议行为：

- 只要任意一条 AttemptDetail 匹配，顶层请求就进入结果；
- 返回的顶层记录保留完整 AttemptDetails，方便看到失败后实际切换到了谁；
- q 扩展搜索 AttemptDetails 中的 provider、model、format、reason、error 和 upstream request id；
- 顶层 status=200 与 attemptStatus=500 可以同时成立，分别表示最终客户端结果和中间上游结果；
- 第一阶段不改变 /api/logs 的分页与环形缓存容量，不为每次尝试额外增加顶层记录。

### 5.6 前端展示

请求日志表保留主行信息，并增加可展开的“上游尝试明细”：

- 主行：客户端时间、模型、最终 Provider、最终状态、总耗时、Attempts、AttemptTrail；
- 子行：序号、Provider、目标模型、Provider 格式、上游状态、Outcome、Reason、耗时；
- 失败 detail 可展开查看 Error、限量后的 ErrorBody、上游 request id；
- ResponseStarted=true 的尝试显示“已开始响应，不可转移”；
- attemptProvider、attemptStatus、attemptOutcome 在筛选栏提供输入；
- 现有没有 AttemptDetails 的旧日志仍正常显示；
- 错误正文必须转义后使用文本展示，不能当 HTML 插入页面。

## 6. OpenAI Responses 上游支持范围

### 6.1 请求转换

新增内部格式到 Responses 的 checked converter，例如 ToOpenAIResponsesBodyChecked。初始支持范围建议为：

| 内部语义 | Responses 请求 |
|---|---|
| 目标模型 | model |
| system 指令 | instructions |
| 普通消息 | input 中的 message item |
| 工具定义 | Responses function tool |
| tool use | function_call item |
| tool result | function_call_output item，并保持 call_id |
| 最大输出 token | max_output_tokens |
| 流式标志 | stream |
| 已有可安全保留的原生字段 | 按白名单从 Internal.Extra 复制 |

不能把 Chat 的工具定义直接塞到 Responses：

- Chat Completions 的 function 定义是外层 type=function 加内层 function；
- Responses 的 function 定义是同一对象上的 type、name、description、parameters；
- Chat 的 tool_calls 和 Responses 的 function_call 不是同一响应项；
- tool result 必须通过 call_id 重新关联。

对于 store、previous_response_id、reasoning、text.format、内置工具、音频和其他 Responses 专属字段，应先制定字段策略：

1. 目标也是 Responses 且走透传时原样保留；
2. 目标是 Chat 时，仅转换已经明确定义的兼容子集；
3. 无法表达且会改变语义的字段应返回明确的 conversion_error，不能静默丢弃；
4. 第一阶段不宣称支持所有 Responses 专属能力，避免把“能生成文本”误报为“完整兼容 Responses”。

特别是 Responses 的 stateful 能力和 reasoning item 不能简单映射为 Chat 的一条 assistant message。若 Codex 请求携带这类字段而目标是 Chat，验收用例必须明确是拒绝、降级还是有损转换。

### 6.2 非流式响应转换

新增 ConvertOpenAIResponsesResponseChecked，至少覆盖：

- Responses 顶层 response 对象；
- output 中的 message / output_text；
- function_call 转为 Chat 的 tool_calls 或 Anthropic 的 tool_use；
- Chat / Anthropic 的 finish reason 与 Responses 的 status / incomplete_details；
- input/output token usage；
- refusal 与 error 的明确映射；
- openai-responses 客户端到 openai-responses Provider 时原样返回。

目标格式为 Chat 时，不能只取一个名为 output_text 的便利字段后丢失 tool call；目标格式为 Anthropic 时，也不能把 function call 当普通文本。

### 6.3 SSE 流式响应转换

Responses 流是带事件类型的语义事件流，不等同于 Chat 的 data: {...} chunk 流。至少需要处理：

- response.created / response.in_progress；
- response.output_text.delta / response.output_text.done；
- output item added / done；
- function call arguments delta / done；
- response.completed；
- response.incomplete；
- response.failed 和 error。

转换方向：

| 上游 | 客户端 | 处理 |
|---|---|---|
| Responses | Responses | 透传 SSE |
| Responses | Chat | 聚合 item 状态并生成 chat.completion.chunk 与 [DONE] |
| Responses | Anthropic | 生成对应的 message start、content block、tool use、message delta 和 stop 事件 |
| Chat / Anthropic | Responses | 沿用现有 transformer，将上游 delta 转换为 Responses 语义事件 |

Responses 的 reasoning、audio、file search、code interpreter、computer use 等事件不应在第一阶段伪装成普通文本。应按能力逐项支持，未支持事件要么安全忽略并记录转换告警，要么在响应尚未开始时返回明确转换错误；不能生成结构上合法但语义错误的 SSE。

流式故障边界保持现有决策：

- 上游响应头还没导致客户端写出字节时，可以 failover；
- 一旦网关向客户端写出状态头或 SSE 字节，不切换上游；
- 中途失败只发送目标客户端格式的失败终态；
- AttemptDetail.ResponseStarted 记录这个边界。

### 6.4 URL、请求头和 proxy 分支

Provider 为 openai-responses 时：

- URL 为去掉 BaseURL 末尾 /v1 后的 /v1/responses；
- 鉴权仍为 Authorization: Bearer ...；
- Content-Type / Accept 仍允许 JSON 和 event-stream；
- handleResponse 按 Provider 格式选择 Chat 或 Responses 响应转换器；
- handleStream 创建 NewStreamTransformer("openai-responses", clientFormat)；
- 未知 Provider 格式不能静默落入 Chat 分支，配置校验和运行时分支都要显式报错。

## 7. Vision 约束

当前 internal/vision 的 doRecognize 固定以 OpenAI Chat Completions 形式调用视觉模型：

- 请求体使用 messages；
- 请求地址使用 /v1/chat/completions；
- 鉴权使用 Bearer；
- 识别结果被替换为内部消息中的文字描述。

新增 Responses 上游时，不要隐式改变 Vision 模块的协议。建议第一阶段：

- Vision provider 继续按当前 Chat 语义运行；
- 视觉翻译仍在候选上游循环外执行一次；
- 主请求目标为 openai-responses 时，使用已经生成的文字描述构建 Responses input；
- 若需要让视觉识别本身调用 Responses，应作为单独需求，另行增加 Vision provider 的协议配置、响应解析和缓存验收；
- 没有视觉翻译时，Responses 请求中的原生图片输入需要在 ToOpenAIResponsesBodyChecked 中明确实现或明确拒绝，不能因为有一个 Chat 图片转换器就认为两种 API 完全兼容。

## 8. 实施阶段与任务清单

### 阶段 A：失败尝试明细

涉及文件：

- internal/metrics/metrics.go：增加 AttemptDetail、日志序列化和筛选；
- internal/proxy/proxy.go：在尚未写客户端的错误路径采集状态、Retry-After、错误正文和 request id；
- cmd/gateway/main.go：候选循环创建并收口 detail，保持 Attempts / AttemptTrail 兼容；
- cmd/gateway/web/index.html 与 cmd/gateway/web/src/index.template.html：展开明细和筛选；
- 必要时更新 API 相关测试。

验收：

- 首个假上游返回 500，第二个假上游返回 200；
- 顶层只有一条请求日志，最终状态为 200；
- attemptDetails 中有首个 500 的 provider、目标模型、format、耗时、reason 和限量后的错误正文；
- /api/logs?attemptProvider=...&attemptStatus=500 可以筛出该顶层请求；
- Attempts 不把 breaker skip、build skip 和 free attempt 错算为真实请求；
- 流式已经开写后不产生第二次上游尝试。

### 阶段 B：Provider Responses 非流式

涉及文件：

- internal/config/config.go、config.example.yaml：接受 openai-responses；
- internal/converter/converter.go：Responses 请求构建；
- internal/converter/response.go：Responses 非流式响应解析和反向转换；
- internal/proxy/upstream.go、internal/proxy/proxy.go：URL、请求头和格式分支；
- cmd/gateway/main.go：按候选 Provider 格式选择构建器；
- converter / proxy / gateway 测试。

验收：

- format: openai 的 fake provider 收到 /v1/chat/completions 和 messages；
- format: openai-responses 的 fake provider 收到 /v1/responses 和 input；
- Claude Code → Responses 上游能够返回 Anthropic 格式；
- Codex → Chat 上游能够返回 Responses 格式；
- Chat / Responses 工具调用和工具结果至少完成一轮往返；
- 原有只有 anthropic / openai 的配置和行为不变。

### 阶段 C：Provider Responses 流式

涉及文件：

- internal/converter/stream.go：Responses 事件解析、状态聚合和目标 SSE 生成；
- internal/proxy/proxy.go：选择新 transformer、采集流式 detail；
- cmd/gateway/main.go：混合格式候选的状态和失败轨迹；
- stream / proxy / gateway 测试。

验收：

- Responses 上游到 Responses 客户端可透传；
- Responses 上游到 Chat 客户端可生成有效 Chat chunk；
- Responses 上游到 Anthropic 客户端可生成有效 Anthropic SSE；
- 文本、tool call、完成、incomplete、failed 至少各有测试；
- 转换器遇到未支持事件时不会产生错误的成功终态；
- 流式响应已经开始后，失败只生成当前客户端协议的失败终态，不 failover。

### 阶段 D：配置和运维收口

- 更新配置示例、管理页面 Provider 格式选项和说明；
- 在 Provider 状态、请求明细和错误提示中显示实际上游格式；
- 文档说明 openai 与 openai-responses 的差异；
- 增加混合协议故障转移的 smoke test；
- 在变更说明中记录 Responses 专属字段的支持 / 拒绝列表。

## 9. 测试矩阵

| 组件 | 测试重点 |
|---|---|
| config | 接受 openai-responses；拒绝未知 format；旧配置默认不变 |
| converter request | 三种客户端到三种上游的 body、模型、tools、tool result |
| converter response | 三种上游响应到三种客户端的文本、tool call、usage、错误状态 |
| converter stream | Responses 关键事件到 Chat / Anthropic；Chat / Anthropic 到 Responses |
| proxy | Responses URL、Bearer header、非流式错误正文限量、SSE 错误正文 |
| gateway | 首个 500 后切第二 Provider；混合 Chat / Responses 候选；body 不污染 |
| metrics | detail 序列化、环形缓存、attemptProvider / attemptStatus / attemptOutcome |
| web | 展开 detail、错误正文转义、旧日志兼容 |
| vision | 主请求 Provider 改为 Responses 后，Vision 仍按既有 Chat 端点工作 |

## 10. 风险与待确认事项

### 10.1 Responses 专属字段的兼容策略

Responses 不只是把 messages 改名为 input。官方文档明确区分了 Messages 与 Items、choices 与 output、Chat function tool 与 Responses function tool，并提供了 stateful conversation、reasoning 和内置工具等额外能力。

实现前需要确认：

- Codex 当前实际会发送哪些 Responses 字段；
- store / previous_response_id / reasoning item 是否必须保留；
- text.format 是否需要与 Chat 的 response_format 做双向转换；
- 不可转换字段是拒绝请求，还是允许显式有损降级。

### 10.2 Provider 格式不是能力探测

某些第三方服务只实现 OpenAI API 的一部分，声明“OpenAI 兼容”不代表同时支持 Chat 和 Responses。配置中的 format 必须表达实际端点能力；新增格式不能保证上游完整兼容 OpenAI 的全部字段。

### 10.3 错误正文的隐私和容量

错误正文对排查 500 很有价值，也可能包含内部路径、提示词片段、账号标识或敏感字段。8 KiB 上限、脱敏和仅保存在内存环形日志中应作为默认安全边界；是否允许在 UI 中复制完整正文需要单独确认。

### 10.4 热重载和旧客户端兼容

新增 Provider format 后，配置严格校验、/api/config 脱敏回显、前端选择项、热重载和运行时 switch 必须同时更新。旧配置不写新字段时，不能因为默认分支变化而从 Chat 变成 Responses。

### 10.5 仍需从上游确认截图中的具体 500

本地代码分析可以确认请求路径和转换方向，但无法凭截图确定 muyuan 的 500 业务原因。落地阶段应使用 attemptDetails.errorBody 和 upstreamRequestId 对照 muyuan 的服务日志，优先确认：

1. 网关实际请求的是 /v1/chat/completions 还是上游期望的 /v1/responses；
2. 请求体是否包含上游可接受的 messages / input 字段；
3. 目标模型是否存在于该 Provider；
4. API Key、模型权限和上游限流情况；
5. 上游返回的 JSON error type、code、message。

## 11. 官方协议参考

- OpenAI Responses 迁移说明：[Migrate to the Responses API](https://developers.openai.com/api/docs/guides/migrate-to-responses)
- OpenAI 流式说明：[Streaming API responses](https://developers.openai.com/api/docs/guides/streaming-responses)
- Responses API 参考：[Responses API](https://developers.openai.com/api/reference/resources/responses)
- Responses 流式事件参考：[Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events/)

官方文档的核心差异与本方案直接相关：Chat Completions 使用 messages 和 choices；Responses 使用 input / Items 和 output；Responses 流式接口使用带类型的语义事件。因此不能只替换 URL，必须同时实现请求、响应和流式事件的协议转换。
