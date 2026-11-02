# ChatGPT OAuth 账户接入设计

日期：2026-10-12
状态：已集成；保留本机 OAuth 授权与请求调用验证的历史记录；2026-11-01 已按当前代码补正
分支：dev
参考：[CLIProxyAPI 管理 API 文档](https://github.com/router-for-me/CLIProxyAPIDocs/blob/main/docs/en/management/api.md)、[OpenAI Codex 本地 OAuth 回调实现](https://github.com/openai/codex/blob/main/codex-rs/login/src/server.rs)

## 1. 需求目标

在 Provider 管理页增加 ChatGPT OAuth 账户的授权、列表和删除能力；授权后可把账户绑定到一个 Provider，并通过现有路由转发 OpenAI Responses 请求。旧的 API Key Provider 配置、路由和凭据脱敏行为保持可用。

本设计将“ChatGPT auth”按 Codex OAuth 登录理解。OAuth token 不进入 `config.yaml` 或 `/api/config`；Provider 只保存认证类型和账户 ID。账户数据单独持久化，供重启后继续使用和刷新 token。

## 2. 当前结构与集成点

- `config.Provider` 现有 `authType`、`accountId` 与 API Key 配置分流；OAuth Provider 使用固定的 Codex Responses 上游路径，普通 `openai-responses` Provider 仍走通用 Responses 路径。
- `/api/config` 保存整个 Provider 配置，并以 sentinel 保护 API Key；普通 API Key Provider 的 `/api/providers/models` 和健康检测查询 `/v1/models`，OAuth Provider 按账户状态处理，不使用该探测。
- Provider 编辑页位于 `cmd/gateway/web/src/app/11-providers.js.part` 与 `src/index.template.html`；生成的 `web/index.html` 必须同步重建。
- 生产 Compose 把 `./data` 挂到 `/app/data` 保存账户文件；开发 Compose 继承同一账户数据目录，但不发布 OAuth 回调端口。共享文件只由单个进程协调写入，不应从两套实例同时更新账户或刷新用量。

## 3. 方案

### 3.1 账户凭据与 Provider 分离

- 新增 `internal/chatgptauth`，负责 OAuth session、PKCE、state 校验、token 交换/刷新、账户元数据和持久化。
- OAuth 凭据保存到 `data/chatgpt-accounts.json`，文件权限设为 `0600`，通过临时文件加原子替换写入。普通账号列表 API 返回不含 access token、refresh token 或 id token 的 `AccountView`；当前安全视图还包含授权时间、用量、标签和备注。后续新增的显式导出 API 是凭据返回的专用例外，不把 token 放入普通读取接口。
- `config.Provider` 已增加 `authType` 与 `accountId`。`authType` 缺省等价于现有 API Key 认证；`chatgptOAuth` 只支持 `openai-responses` 和固定 Codex 上游地址，不可同时配置 `apiKey` 或 `apiKeys`。启用时必须有账户引用；禁用状态允许空 `accountId`，用于后续解绑流程。同一 OAuth 账户可供多个 Provider 引用，每个 Provider 仍独立保留并发、代理、重试等设置。
- Provider 删除不自动删除账户。删除仍被 Provider 引用的账户返回 409，禁用但仍有账户引用的 Provider 也会计入；用户须先解绑或改/删相关 Provider。现行通用 `PUT /api/config` 仍无条件检查所有 OAuth 账户引用，因此解绑成空引用后普通配置保存会失败；该未修复缺陷转由 `docs/KNOWN_ISSUES.md` 的“OAuth Provider 解绑后普通配置保存失败”跟踪。

### 3.2 OAuth 登录与回调

管理页先显示授权选项弹窗，使用者明确选择打开授权页面或复制授权链接后才创建 session，并轮询该 session 状态。session 使用随机 state、PKCE verifier、短有效期和一次性兑换；自动 listener 与后续增加的手动回调 API 共用校验及兑换函数。回调 URI 当前固定为 `http://localhost:{实际端口}/auth/callback`，主机、端口和路径须与 session 一致。回调成功后 token 原子保存，前端随后刷新账户列表。

生产 `docker-compose.yml` 将回调端口只发布到宿主机 `127.0.0.1`，使用 `1455`，容器内该端口不可用时回退到 `1457`。开发 `docker-compose.dev.yml` 覆盖端口列表，只发布管理端口 `7790`，没有发布 OAuth 回调端口；可使用手动回调提交能力，但本次未验证开发容器实际授权，不能把生产自动回调验收外推过去。OAuth callback 不依赖浏览器 Origin，但必须通过 state 与会话 URI 校验；发起、状态查询、账户删除等管理操作沿用跨站 Origin 防护。

### 3.3 上游请求

- OAuth Provider 固定使用 OpenAI Responses 格式，走 Codex/ChatGPT OAuth 对应的上游 endpoint 和必要请求头；不能把通用 API Key Provider 的 `/v1/responses` 路径直接套用。
- 转发前从账户管理器取得有效 access token；临近到期时串行刷新并原子落盘。只有刷新明确返回 `invalid_grant` 时才持久化“需重新授权”，临时网络或其他上游刷新失败返回错误而不一律作废账户。转发触发的刷新使用当前 Provider 客户端，不使用管理器专用 OAuth 代理客户端。
- `/v1/models` 不作为 OAuth 账户的前提。当前 OAuth Provider 的模型查询返回空列表和手动填写提示，不承诺已接入 Codex 模型目录；健康状态取账户授权状态，不能用 API Key Provider 的模型探测语义误判。
- 请求正文仍沿用本项目 Responses converter。Anthropic / Chat Completions 输入仅在现有转换器可转换为 Responses 请求时支持。

### 3.4 管理页

Provider 页提供独立的 ChatGPT 账户卡片：授权账户、展示脱敏元信息和授权状态、删除未被引用账户。Provider 表单提供认证方式选择；选 OAuth 时可选已有账户，隐藏 API Key 输入，并锁定为 Responses 格式及 Codex 上游地址。配置预览不显示 token。

改动 `.js.part` 或模板后，按项目约定重建 `cmd/gateway/web/index.html`；同步 Provider 配置归一化、保存 payload、YAML 预览与字段覆盖检查。

## 4. API 草案

端点请求/响应详见同目录 `API_SPEC.md`。所有管理端点只在本机管理页面使用；OAuth callback 为无 Origin 的浏览器顶层跳转入口，但由一次性 state 保护。

## 5. 不在范围内

- 本基础阶段不接入 ChatGPT 网页 cookie、浏览器会话或手工粘贴 refresh token；后续独立需求已实现令牌文件导入/导出，不应把此历史范围当作当前全局禁令。
- 本基础阶段不做配额/用量读取、账号轮询策略、WebSocket Responses 或实时语音；后续账号 insights 需求已增加用量读取，本节只记录原阶段边界。
- 不把 CLIProxyAPI 作为运行时依赖，也不复制其完整管理服务；只参考其 OAuth 发起、回调状态和凭据管理模式。

## 6. 主要验收条件

1. 可以从管理页授权一个或多个 ChatGPT 账户；session 过期、state 不匹配、端口占用和 token 交换失败均返回可理解的错误。
2. 账户信息在服务重启后保留，列表和配置 API 中均看不到任何 OAuth token。
3. OAuth Provider 可通过现有路由承接 Responses 请求；API Key Provider 的配置与请求行为不变。
4. Provider 引用的账户不能被删除；移除全部引用后可删除并清除本地 token。
5. 生产 Docker Compose 下回调仅通过宿主机 loopback 暴露；保留既有本机 OAuth 授权与调用验证记录，不将其外推为开发 Compose 自动回调或所有失败边界均已验证。

## 7. 2026-11-01 文档维护说明

本文以当前代码补正后续已集成能力，保留基础阶段需求与既有人工验收历史；本次未运行真实 OAuth 或专项测试。归档与现有通用回归记录见 `TASKS.md`，专项覆盖继续由 `docs/KNOWN_ISSUES.md` 的“ChatGPT 账户功能专项回归覆盖”跟踪。
