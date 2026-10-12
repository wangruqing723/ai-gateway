# ChatGPT OAuth 账户管理 API

日期：2026-10-12
状态：已实现

管理 API 响应为 JSON。跨站 Origin 会被拒绝；错误沿用网关 `{ "error": { "type": "...", "message": "..." } }` 结构。OAuth callback 是 localhost 浏览器跳转，由一次性 state 校验。

## `GET /api/chatgpt/accounts`

列出已保存账户及被 Provider 引用情况，不返回凭据。

```json
{
  "data": [{
    "id": "opaque-account-id",
    "email": "user@example.com",
    "plan": "plus",
    "status": "ready",
    "expiresAt": "2026-10-12T12:00:00Z",
    "lastRefresh": "2026-10-12T10:00:00Z",
    "providers": ["codex-main"]
  }]
}
```

## `POST /api/chatgpt/oauth/start`

创建有时限的授权 session，返回前端打开授权页所需 URL。

```json
{ "sessionId": "opaque-session-id", "authUrl": "https://...", "expiresAt": "2026-10-12T12:10:00Z" }
```

## OAuth 回调 `GET http://localhost:{1455|1457}/auth/callback`

由 OAuth 浏览器重定向调用，接收 `code`、`state` 或 OAuth 错误。state 必须匹配未过期 session 且只能消费一次。成功后保存凭据并返回不含 token 的完成页面。Docker Compose 将两个端口仅映射到宿主机 loopback；优先使用 1455，容器内端口占用时回退 1457。

## `GET /api/chatgpt/oauth/status?sessionId=...`

前端轮询 session：

```json
{ "status": "pending" }
{ "status": "completed", "account": { "id": "opaque-account-id", "email": "user@example.com", "plan": "plus" } }
{ "status": "error", "message": "授权已过期，请重新开始" }
```

完成或失败 session 保留 10 分钟供轮询，之后删除。

## `DELETE /api/chatgpt/accounts/{id}`

删除账户凭据。账户仍被 Provider 引用时返回 `409 account_in_use`，错误对象携带引用 Provider 名称；否则删除凭据并返回 `{ "status": "deleted" }`。

## Provider 配置增量

```yaml
providers:
  codex-main:
    authType: chatgptOAuth
    accountId: opaque-account-id
    format: openai-responses
    baseUrl: https://chatgpt.com/backend-api/codex
    # accountId 由管理页连接账户后选择；不会写入 OAuth token。
```

旧配置缺少 `authType` 时继续使用 `apiKey` 行为。`chatgptOAuth` 配置不得同时携带 `apiKey`，且 `accountId` 必须指向已保存账户。
