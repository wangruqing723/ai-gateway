# ChatGPT OAuth 专用代理设计

## 目标

允许 ChatGPT OAuth 令牌交换和刷新请求单独走代理，而提供商上游请求继续按提供商代理配置或全局代理配置路由。这样用户只想让 OAuth 走代理时，不必逐个维护 `NO_PROXY` 提供商列表。

## 配置与路由

- 新增单 URL 环境变量 `AI_GATEWAY_OAUTH_PROXY`，由 Compose 传入网关容器。
- ChatGPT OAuth 初始化 HTTP 客户端时，非空值通过现有 `httpclient.Pool.For` 创建专用代理客户端。
- 配置热重载回收不再使用的 Provider 代理连接池时，将 OAuth 专用代理也作为活跃代理保留。
- `AI_GATEWAY_OAUTH_PROXY` 为空时继续使用默认客户端，保持现有 `HTTP_PROXY` / `HTTPS_PROXY` 行为，避免影响已有部署。
- 提供商请求解析逻辑保持不变：提供商 `proxy` 非空时使用其专用代理；为空时继承全局 `HTTP_PROXY` / `HTTPS_PROXY`，未设置全局代理时直连。
- 用户只想代理 OAuth 时，设置 `AI_GATEWAY_OAUTH_PROXY` 并清空 `AI_GATEWAY_HTTP_PROXY` 与 `AI_GATEWAY_HTTPS_PROXY`。无需为每个提供商添加 `NO_PROXY`。

## 数据流

```text
ChatGPT OAuth 请求 ── AI_GATEWAY_OAUTH_PROXY ──> OAuth 服务
提供商请求 ── provider.proxy（若有）/ 全局代理 / 直连 ──> 提供商
```

## 兼容性与边界

- 不修改配置文件格式、管理 API 或提供商路由行为。
- OAuth 专用代理设置优先于全局代理，仅作用于 OAuth 管理器的令牌交换和刷新客户端。
- 未配置专用代理时保留当前行为；因此已有全局代理配置仍会同时影响 OAuth 和没有单独 `provider.proxy` 的提供商。
- `NO_PROXY` 的现有作用不变，仍可用于全局代理场景下按目标主机绕过代理。
