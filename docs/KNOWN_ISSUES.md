# KNOWN_ISSUES：跨需求统一跟踪

本文件只收未完成的代码问题、专项回归覆盖和未补全的验证记录。已完成需求可以归档，但这些遗留事项不随归档关闭。

2026-11-01 按当前代码重新核对并迁入本次归档的遗留项。使用者确认 Provider 启停、UI 现代化、OAuth 授权交互、账户用量和账户管理的既有验收已完成，原人工验收待办不再作为阻断。使用者验收不等于下列代码缺陷已修复，也不等于已经新增专项自动化测试。

## [2026-11-01] OAuth Provider 解绑后普通配置保存失败

- 来源：[账户管理体验需求](./_archive/2026-10-23-chatgpt-oauth-account-management/TASKS.md)。
- 状态：已用断网临时实例和假账户复现，尚未修复。
- 现象：点击“解绑并停用”返回成功，Provider 保留 `authType: chatgptOAuth`、清空 `accountId` 并设为停用。之后修改超时、路由或其他 Provider 等普通配置并保存，会返回 HTTP 400 `config_validation_error`，提示该 OAuth Provider 的账户不存在。
- 原因：`internal/config/config.go` 的 `validateProvider` 允许停用的 OAuth Provider 不绑定账户；但 `cmd/gateway/main.go` 的 `handlePutConfig` 仍对所有 OAuth Provider 无条件执行 `Account(p.AccountID)`。空账户 ID 不存在，所以整份配置保存被拒绝。
- 影响：不是解绑失败，也不会删除原账户。当前运行配置不被失败保存覆盖，但其他设置也无法写入。对其他 Provider 的直接请求不因此自动中断；是否仍能继续请求取决于已有路由是否有可用候选。
- 临时规避：使用账户页的绑定入口给这个 Provider 重新绑定已有账户；或在一次普通保存的请求里删除这条未绑定 Provider，并同步处理其路由引用。只保持停用、刷新页面或重启不会消除空账户 ID 的保存校验冲突。
- 修复方向：普通配置保存与 `validateProvider` 使用相同语义，允许“停用且未绑定”的合法配置；启用的 OAuth Provider 仍必须绑定有效账户。补“绑定成功、解绑成功、解绑后仍可保存无关设置、重新绑定”的 HTTP 回归测试。
- 本次复现：容器 `--network none`，仓库只读挂载，账户和配置在独立临时目录。只使用未来到期的假 token，没有请求真实上游，也没有读写正式配置和账户。

```text
绑定假账户：HTTP 200
绑定时修改普通设置并保存：HTTP 200
解绑并停用 Provider：HTTP 200
解绑后 Provider：enabled=false，accountId 为空，authType=chatgptOAuth
解绑后修改普通设置并保存：HTTP 400
{"error":{"message":"providers.oauth-demo.accountId 引用了不存在的 ChatGPT OAuth 账户","type":"config_validation_error"}}
重新绑定原账户：HTTP 200
重新绑定后普通保存恢复：HTTP 200
```

## [2026-11-01] ChatGPT 账户功能专项回归覆盖

- 来源：[基础 OAuth 接入](./_archive/2026-10-12-chatgpt-oauth-account/TASKS.md)、[授权交互](./_archive/2026-10-12-oauth-login-controls/TASKS.md)、[账户导入导出](./_archive/2026-10-13-chatgpt-account-import-export/TASKS.md)、[账户用量](./_archive/2026-10-13-chatgpt-account-insights/TASKS.md)、[OAuth 代理隔离](./_archive/2026-10-13-oauth-proxy-isolation/TASKS.md)、[账户标签备注](./_archive/2026-10-17-chatgpt-account-tags-notes/TASKS.md)、[账户管理](./_archive/2026-10-23-chatgpt-oauth-account-management/TASKS.md)。
- 状态：实现和既有验收已归档，专项自动化测试不足继续跟踪；本次没有补写业务测试。
- 当前证据：`internal/chatgptauth` 没有独立的 `*_test.go`。网关现有测试使用假账户覆盖了部分转发行为，但不能等同于 PKCE/state、令牌轮换、用量解析、多格式互操作和所有账户管理事务均有专门测试。
- 待补：OAuth state/PKCE、过期或错误回调、并发回调、刷新轮换及落盘失败；用量窗口缺失字段、失败缓存、强制刷新、重启恢复和授权时间保留；四种格式 JSON/ZIP 往返、重复账户跳过、坏批次及持久化失败回滚、ZIP 路径/大小边界；管理请求与 Provider 请求触发刷新时的代理选择；标签备注 Unicode 校验、数量边界、故障回滚、重启持久化与导出排除；账户绑定/解绑、同账户重复绑定保留禁用状态、停用未绑定 Provider 的配置保存与重绑保持其他设置。
- 验证边界：使用者已确认现有授权交互、用量展示和账户管理验收。这里仅跟踪可自动重跑的回归覆盖；不要求为了归档重新登录或调用付费上游。外部工具版本间的全部导入导出互操作记录尚未补全，不以通用测试通过替代。

## [2026-11-01] 历史观测与后缀功能专项回归覆盖

- 来源：[监控错误可见性](./_archive/2026-09-08-dashboard-error-visibility/TASKS.md)、[后缀透传与自定义头](./_archive/2026-09-10-onem-suffix-passthrough/TASKS.md)。
- 状态：功能已实现，现有测试和静态检查通过；早期任务清单里指定的部分端到端回归没有对应的完整自动化覆盖。
- 已有覆盖：视觉失败分类与汇总、事件回调、后缀拆分与路由目标、配置校验及自定义头 helper 的单元测试。
- 待补：通过实际 HTTP 出口断言 `preserve` 后上游 `model` 保留原始 `[1m]` / `[1M]`，覆盖目标模型名为空或显式配置、同格式或跨格式；断言健康检测和模型查询分别把自定义头送到假上游，并验证配置 API 保存/重载后的新值；通过假视觉上游验证错误经 Translate、请求日志和指标 API 贯通；周期健康检测的启动、配置变更、停止和取消；熔断或健康回调到事件流及 HTTP 展示的联动，包含 ResetAll 对 closed 状态但存在失败计数时的复位事件。现有 spy 或 helper 单测不能替代这些专项集成覆盖。
- 要求：优先用本地假上游和临时配置，不读写真实密钥、不触发正式 Provider 请求。不能把 helper 单测代替所有出口路径的集成测试。

## [2026-08-29] Responses 真实上游回归记录待补全（原 KI-1）

- 来源：[尝试链观测与协议兼容](./_archive/2026-08-29-attempt-observability-review-fixes/TASKS.md)。
- 历史记录：当时四组协议组合使用本地假上游跑通，真实 Responses 上游返回鉴权错误，没有完成该轮全部真实上游回归。
- 当前边界：基础 OAuth 接入已有历史授权与调用验收；不能据此自动认定早期要求的四组跨格式流式和非流式场景全部完成。本次不查询真实上游或账户，不再保留“线上无 Responses 流量、风险为零”这一已经过期的判断。
- 待补：在授权的测试环境记录所需协议组合的流式和非流式调用结果，或者明确哪些组合不再属于支持范围。现有的本地自动化测试继续保留。

## [2026-08-29] 跨协议流式转换的真实网络边界记录待补全（原 KI-2）

- 来源：[尝试链观测与协议兼容](./_archive/2026-08-29-attempt-observability-review-fixes/TASKS.md)。
- 历史记录：Responses SSE 到 Anthropic SSE 已用真实 HTTP 长连接跑通，但当时假上游一次写完整载荷，真实上游的分块、半行、keep-alive 和事件顺序未形成完整验证记录。
- 当前边界：代理和转换器已有回归测试，本次相关测试及竞态检查通过。这里保留的是特定真实网络条件的记录缺口，不能概括为当前整个流式功能未经测试。
- 待补：用受控假上游明确复现分块和半行等边界，必要时记录真实上游异常案例，并给出稳定的测试名称和运行结果。

## [2026-08-29] “已开始响应，未转移”标记的专项展示记录待补全（原 KI-3）

- 来源：[尝试链观测与协议兼容](./_archive/2026-08-29-attempt-observability-review-fixes/TASKS.md)。
- 历史记录：前端 `responseStarted && outcome !== 'success'` 标记存在，但缺少针对该标记的真实失败流量展示记录。
- 当前边界：多密钥需求已补充“内容提交后不切换”的回归，当前显示逻辑仍存在；不能把它等同于所有流中断、活跃超时场景的专项视觉记录都已补齐。
- 待补：用假上游先发送内容再中断，确认终态、尝试明细与该标记一致，同时断言提交后不转移和预期的熔断上报。

## [2026-08-29] `isMeaningfulExtra` 与真实客户端字段集的专项比对记录待补全（原 KI-4）

- 来源：[尝试链观测与协议兼容](./_archive/2026-08-29-attempt-observability-review-fixes/TASKS.md)。
- 历史记录：显式默认值的放行规则按当时协议语义制定，缺少一份针对真实客户端完整字段集合的系统性比对记录。
- 当前边界：函数仍参与跨格式校验，已有默认值与负向能力约束回归。不能把早期“尚未发过真实 Codex 请求”的说法作为今天的整体状态；本次只保留专项比对记录尚未补全的欠账。
- 待补：在明确授权并保护提示词、密钥的前提下，对采集的客户端字段形状逐项对照；支持规则以当前代码和明确的协议能力为准，不因个别请求成功就放开所有未知字段。
- 保护：保留 `previous_response_id`、`conversation`、`background` 等不兼容能力的负向断言；不要删除检查来获得表面通过。
