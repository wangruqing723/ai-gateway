# ChatGPT OAuth 账户接入设计

日期：2026-10-12
状态：实现完成；本机已完成 OAuth 授权与请求调用验证
分支：dev
参考：[CLIProxyAPI 管理 API 文档](https://github.com/router-for-me/CLIProxyAPIDocs/blob/main/docs/en/management/api.md)、[OpenAI Codex 本地 OAuth 回调实现](https://github.com/openai/codex/blob/main/codex-rs/login/src/server.rs)

## 1. 需求目标

在 Provider 管理页增加 ChatGPT OAuth 账户的授权、列表和删除能力；授权后可把账户绑定到一个 Provider，并通过现有路由转发 OpenAI Responses 请求。旧的 API Key Provider 配置、路由和凭据脱敏行为保持可用。

本设计将“ChatGPT auth”按 Codex OAuth 登录理解。OAuth token 不进入 `config.yaml` 或 `/api/config`；Provider 只保存认证类型和账户 ID。账户数据单独持久化，供重启后继续使用和刷新 token。

## 2. 当前结构与集成点

- `config.Provider` 以 `apiKey`、`baseUrl`、`format` 描述单个上游；`openai-responses` 请求目前走通用 `/v1/responses` 路径。
- `/api/config` 保存整个 Provider 配置，并以 sentinel 保护 API Key；`/api/providers/models` 和健康检测默认查询 `/v1/models`。
- Provider 编辑页位于 `cmd/gateway/web/src/app/11-providers.js.part` 与 `src/index.template.html`；生成的 `web/index.html` 必须同步重建。
- Compose 已把 `./data` 挂到 `/app/data`，可用于保存 OAuth 账户文件。

## 3. 方案

### 3.1 账户凭据与 Provider 分离

- 新增 `internal/chatgptauth`，负责 OAuth session、PKCE、state 校验、token 交换/刷新、账户元数据和持久化。
- OAuth 凭据保存到 `data/chatgpt-accounts.json`，文件权限设为 `0600`，通过临时文件加原子替换写入。账号列表 API 仅返回 ID、邮箱、套餐、过期时间、状态和被哪些 Provider 引用；绝不返回 access token、refresh token 或 id token。
- `config.Provider` 增加 `authType` 与 `accountId`。`authType` 缺省等价于现有 API Key 认证；`chatgptOAuth` 必须绑定一个存在的账户，且只支持 `openai-responses`。同一 OAuth 账户可供多个 Provider 引用，每个 Provider 仍独立保留并发、代理、重试等设置。
- Provider 删除不自动删除账户。删除仍被 Provider 引用的账户返回 409；用户先改/删相关 Provider，再删除账户，避免留下不可用路由。

### 3.2 OAuth 登录与回调

管理页发起 OAuth 后打开授权 URL，并轮询该 session 状态。session 使用随机 state、PKCE verifier、短有效期和一次性消费；回调只接受匹配且仍待处理的 state。回调成功后 token 原子保存，前端随后刷新账户列表。

按 Codex OAuth 的 localhost 回调方式实现，兼容 Docker：回调服务只通过 Compose 的 `127.0.0.1` 端口发布，使用 `1455`，容器内该端口不可用时回退到 `1457`。OAuth callback 不依赖浏览器 Origin，但必须通过 state 校验；发起、状态查询、账户删除等管理操作沿用本项目的跨站 Origin 防护。

### 3.3 上游请求

- OAuth Provider 固定使用 OpenAI Responses 格式，走 Codex/ChatGPT OAuth 对应的上游 endpoint 和必要请求头；不能把通用 API Key Provider 的 `/v1/responses` 路径直接套用。
- 转发前从账户管理器取得有效 access token；临近到期时串行刷新并原子落盘，刷新失败将账户状态标为需要重新授权。转发请求只在运行时设置鉴权头。
- `/v1/models` 不作为 OAuth 账户的前提。模型选择需支持 Codex OAuth 模型目录或用户手动填写模型名；健康状态显示账户授权状态，不能用 API Key Provider 的模型探测语义误判。
- 请求正文仍沿用本项目 Responses converter。Anthropic / Chat Completions 输入仅在现有转换器可转换为 Responses 请求时支持。

### 3.4 管理页

Provider 页提供独立的 ChatGPT 账户卡片：授权账户、展示脱敏元信息和授权状态、删除未被引用账户。Provider 表单提供认证方式选择；选 OAuth 时可选已有账户，隐藏 API Key 输入，并锁定为 Responses 格式及 Codex 上游地址。配置预览不显示 token。

改动 `.js.part` 或模板后，按项目约定重建 `cmd/gateway/web/index.html`；同步 Provider 配置归一化、保存 payload、YAML 预览与字段覆盖检查。

## 4. API 草案

端点请求/响应详见同目录 `API_SPEC.md`。所有管理端点只在本机管理页面使用；OAuth callback 为无 Origin 的浏览器顶层跳转入口，但由一次性 state 保护。

## 5. 不在范围内

- 不接入 ChatGPT 网页 cookie、浏览器会话或手工粘贴 refresh token。
- 不做配额/用量读取、账号轮询策略、WebSocket Responses 或实时语音。
- 不把 CLIProxyAPI 作为运行时依赖，也不复制其完整管理服务；只参考其 OAuth 发起、回调状态和凭据管理模式。

## 6. 主要验收条件

1. 可以从管理页授权一个或多个 ChatGPT 账户；session 过期、state 不匹配、端口占用和 token 交换失败均返回可理解的错误。
2. 账户信息在服务重启后保留，列表和配置 API 中均看不到任何 OAuth token。
3. OAuth Provider 可通过现有路由承接 Responses 请求；API Key Provider 的配置与请求行为不变。
4. Provider 引用的账户不能被删除；移除全部引用后可删除并清除本地 token。
5. Docker Compose 下回调仅通过宿主机 loopback 暴露，且容器回调可以完成。
