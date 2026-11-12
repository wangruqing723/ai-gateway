# 错误来源与原始错误证据设计

状态：用户已确认推荐方案，实施中；实现方 Claude。

## 现状与问题

- internal/metrics/metrics.go 的 RequestLog 只有 Error 摘要；AttemptDetail 已有 Error、Reason、ErrorBody、ErrorBodyTruncated、UpstreamRequestID、ResponseStarted。
- internal/proxy/proxy.go 已有错误体脱敏与 8 KiB 观测上限；现有回调主要处理 HTTP 错误和预读错误，流中途显式错误没有完整接入这条观测链。
- internal/converter/stream.go 的 Responses 错误提取主要保留消息，容易与真正的转换失败共用错误路径。
- cmd/gateway/web/src/index.template.html 已在尝试详情使用 x-text 展示错误正文，可复用，不另建独立日志页。
- 上游原始流落盘依赖调试开关，包含正常内容，不能默认作为前端错误正文来源。

## 提议范围

覆盖现有推理请求日志、所有候选尝试、终态摘要、容器错误日志和前端日志列表/详情。管理 API、余额查询等独立接口不在本期范围。对原本未进入请求日志的 HTTP 边界拒绝不自动扩展采集范围。

保留「错误返回」而不是整条生成流：HTTP 非成功响应保留受限脱敏正文；流式只保留错误事件或终态快照中的错误对象及必要事件元数据，不保存成功文本、工具参数或全量 response.output。

## 分类原则

来源与错误类别分开。来源描述观察到错误的位置，不声称找到了最终责任人。

| 来源 | 前端标签 | 判定依据 |
|---|---|---|
| upstream | 上游返回错误 | 上游非成功 HTTP，或明确的 error/response.failed 等失败事件 |
| gateway | 网关处理错误 | 本地配置/路由/排队/预算裁决，或已证实的本地转换能力限制 |
| transport | 连接异常 | DNS/TLS/连接失败、超时、读取中断；不能据此判定是网关还是远端责任 |
| client | 客户端中断 | 已确认请求上下文因客户端离开而取消 |
| unknown | 来源未确定 | 历史无字段记录、歧义错误或无法可靠归因 |

上游无效 JSON 记录为 upstream_invalid_response，不保留可能包含成功内容的整段正文。请求构建阶段的转换不支持记录为 gateway_conversion_unsupported；其他缺少可靠类型证据的转换错误保持 unknown/response_conversion_error，不以文案推断责任。

上游返回的 HTTP 400 仍标 upstream，意思是错误由上游返回，不代表用户请求一定正确。

## 数据与传播

- 优先扩展现有结构，不用正则从自由文本反推来源。
- 转换器为上游显式失败提供类型化错误，携带事件类型、错误码、原消息及最小错误载荷；不引入对 proxy 或 metrics 的依赖。
- proxy 把 typed error 转为统一诊断信息，并在同格式透传路径观察失败事件但不改写原始字节。解析必须按完整 SSE 事件处理，覆盖分块、CRLF、多 data 行与大小上限；只观察受支持协议。
- main 将诊断填入当前 AttemptDetail；本地失败由产生错误的边界明确填入来源。
- 每次尝试独立保存诊断，不能让失败候选的信息覆盖后来成功候选。请求最终失败时汇总终态原因；最终成功时顶层不显示失败标签，但展开可看到历史失败。
- 200 后失败保留 status/upstreamStatus=200，以 outcome 和 Error 表示请求失败，不能伪造已发送的 HTTP 状态。
- 观测分类不直接改变 classifyFailure、breakerOutcomeFor、ShouldRetry 和密钥冷却。新 typed error 必须兼容现有 errors.Is/As 判定；变更前后行为由测试锁定。

## 原文保留与安全

推荐沿用现有 8 KiB 每尝试观测上限；超过上限明确标记截断。保留的是「脱敏后的上游原始错误」，不承诺与原始字节完全一致。超出观测预算且不是完整 JSON 的自由文本不保存片段，只标截断，避免凭据边界泄露和大段正则扫描占用转发超时。单个 SSE 观察事件沿用 8 MiB 上限；超限丢弃至事件边界，不保证可提取未知事件内的错误，不能将超长正常事件误标为失败。

先结构化脱敏再限量；超大或非 JSON 错误需要有界处理，避免为了脱敏无界读取。保留 UTF-8 有效性；不将截断 JSON 误标成完整 JSON。脱敏覆盖嵌套敏感字段、消息里的当前请求凭据和可识别 Bearer/API key 内容；错误码与消息也走统一脱敏。自由文本不可能保证识别全部业务隐私，界面应提示原文可能含业务信息。

默认不扩大调试原始流落盘，不将调试文件内容直接暴露给浏览器。不保存完整请求头或鉴权头。文本展示只用 x-text，不用 x-html；复制内容与显示内容同为脱敏版本。

原文有独立状态：available、truncated、not_provided、not_captured；空值不能被误解为已经保存了空响应。

## 前端展示

- 日志列表：错误摘要前展示来源文字标签，不能只靠颜色区分；200 后失败显示「失败 · HTTP 200」。
- 尝试详情：来源、错误类别、上游错误码、原消息、Provider、请求标识、响应是否已开始、原始错误展开区。
- 对 r00035 显示「上游返回错误 / upstream_http2_stream_error」，原消息保留 Upstream HTTP/2 stream failed，另给网关说明「响应已开始，未执行故障转移」。
- 无旧字段的记录显示来源未确定，不根据 conversion_error 猜测来源。

## 建议 API 增量

RequestLog 与 AttemptDetail 新增可选 errorSource、errorCategory；AttemptDetail 新增 upstreamErrorType、upstreamErrorCode、upstreamErrorMessage、errorBodyState。复用 errorBody、errorBodyTruncated、upstreamRequestId；不在顶层重复大段正文。

旧 error/reason 等字段保留兼容；真正上游显式错误的展示文案改正，但不能让文本变化改变重试控制。成功日志可省略 errorSource；失败且无法判断时显式 unknown。

## 验证矩阵

- Anthropic、Chat、Responses：HTTP 4xx/5xx、开场失败、中途失败、同格式与跨格式。
- Responses：扁平 error、嵌套 error、response.failed、未知错误码、失败终态携带巨大 output。
- 本地：请求构建失败、转换不支持、窗口不足、无可用密钥、排队超时。
- 传输：连接失败、响应头超时、读流中断、客户端取消；不能错误标记为上游显式错误。
- 多候选：首个失败后成功、全部失败、本地重试、密钥切换、响应开始后禁止切换。
- 安全：密钥在嵌套字段与自由消息中、超大正文、HTML、截断 UTF-8、未知字段。
- r00035 使用只含必要事件的合成夹具，不提交真实会话抓包。

## 已确认范围

1. 只保存脱敏限量的错误正文/失败事件，不新增完整响应流录制。
2. 沿用内存日志保留期限，不新增持久化存储。
3. 标识用于管理页面与容器日志，不改变发给 Claude Code/Codex 的错误响应契约。
