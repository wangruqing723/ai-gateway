# ChatGPT OAuth 专用代理设计

状态：已集成；2026-11-01 已按当前代码及 README、`.env.example` 补正代理适用范围。

## 目标

允许 ChatGPT OAuth 管理请求单独走代理：授权码交换、账户用量查询及用量查询触发的令牌刷新使用管理器的 OAuth 客户端。真实 Provider 转发及其触发的访问令牌刷新则使用当前 Provider 的客户端，按提供商代理配置或全局代理配置路由。这样只想代理授权和用量管理时，不必逐个维护 `NO_PROXY` 提供商列表；并不保证所有令牌刷新都走 OAuth 专用代理。

## 配置与路由

- 新增单 URL 环境变量 `AI_GATEWAY_OAUTH_PROXY`，由 Compose 传入网关容器。
- ChatGPT OAuth 初始化 HTTP 客户端时，非空值通过现有 `httpclient.Pool.For` 创建专用代理客户端。
- 配置热重载回收不再使用的 Provider 代理连接池时，将 OAuth 专用代理也作为活跃代理保留。
- `AI_GATEWAY_OAUTH_PROXY` 为空时继续使用默认客户端，保持现有 `HTTP_PROXY` / `HTTPS_PROXY` 行为，避免影响已有部署。
- 提供商请求解析逻辑保持不变：提供商 `proxy` 非空时使用其专用代理；为空时继承全局 `HTTP_PROXY` / `HTTPS_PROXY`，未设置全局代理时直连。
- 用户只想代理授权和用量管理时，设置 `AI_GATEWAY_OAUTH_PROXY` 并清空 `AI_GATEWAY_HTTP_PROXY` 与 `AI_GATEWAY_HTTPS_PROXY`，无需为每个提供商添加 `NO_PROXY`。此时 Provider 请求及其触发的刷新按各自代理配置运行，没有 Provider 代理时直连。

## 数据流

```text
授权码交换 ── 管理器 OAuth 客户端 ── OAuth 专用代理 / 全局代理 / 直连 ──> OAuth 服务
用量查询及其触发的刷新 ── 管理器 OAuth 客户端 ── 同上 ──> usage / OAuth 服务
Provider 转发及其触发的刷新 ── 当前 Provider 客户端 ── provider.proxy / 全局代理 / 直连 ──> Codex / OAuth 服务
```

## 兼容性与边界

- 不修改配置文件格式、管理 API 或提供商路由行为。
- OAuth 专用代理设置优先于全局代理，只作用于管理器客户端发起的授权码交换、用量查询及用量查询触发的刷新。`forwardAttempt` 在真实转发前调用 `AccessToken(..., 当前 Provider client)`，该刷新不走专用 OAuth 客户端。
- 未配置专用代理时，管理器客户端沿用全局代理；没有单独 `provider.proxy` 的 Provider 客户端也沿用全局代理，因此两类请求可能共享全局路径，但不是强制共享专用 OAuth 代理。
- `NO_PROXY` 只作用于全局环境代理；显式 `AI_GATEWAY_OAUTH_PROXY` 和 `provider.proxy` 不读取它。
- 原“所有 OAuth 令牌刷新均走专用代理”的概括已被以上按调用来源区分的口径替代；这与当前 README、`.env.example` 一致。
