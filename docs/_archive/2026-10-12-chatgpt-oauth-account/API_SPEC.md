# ChatGPT OAuth 账户管理 API

日期：2026-10-12
状态：已实现并集成；2026-11-01 按当前代码补正

本文件保留基础接入接口；后续账户安全视图已增加授权时间、用量、标签和备注，授权交互也增加了手动回调提交接口。示例是最小结构，不代表完整字段集合。

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

由 OAuth 浏览器重定向调用，接收 `code`、`state` 或 OAuth 错误。state 必须匹配未过期 session 且只能消费一次；回调的 scheme、主机、端口和路径还必须匹配 session 保存的 URI，当前主机固定为 `localhost`，不能以 `127.0.0.1` 替代。成功后保存凭据并返回不含 token 的完成页面。生产 `docker-compose.yml` 将两个端口仅映射到宿主机 loopback；优先使用 1455，容器内端口占用时回退 1457。`docker-compose.dev.yml` 仅发布 `127.0.0.1:7790:7789`，不发布这两个回调端口，生产自动回调的验收不能外推为开发容器也已通过。开发容器可通过同一会话的 `POST /api/chatgpt/oauth/callback` 手动提交完整回调 URL，详见后续授权交互 API；本次维护未实跑开发容器授权。

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

旧配置缺少 `authType` 时继续使用 API Key 行为。`chatgptOAuth` 配置不得同时携带 `apiKey` 或 `apiKeys`，格式必须为 `openai-responses`，上游地址固定为 `https://chatgpt.com/backend-api/codex`。启用的 OAuth Provider 必须有账户引用；配置层允许禁用 Provider 的 `accountId` 为空，以支持后续解绑 API。

现行限制：通用 `PUT /api/config` 仍对所有 OAuth Provider 无条件执行账户存在性校验，即使 Provider 已禁用、`accountId` 为空也会返回 `400 config_validation_error`。解绑 API 成功不等于后续普通配置保存可用；该缺陷未修复，转由 `docs/KNOWN_ISSUES.md` 的“OAuth Provider 解绑后普通配置保存失败”跟踪。
