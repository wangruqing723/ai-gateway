# ChatGPT OAuth 授权交互 API 变更

日期：2026-10-12
状态：已实现

## `POST /api/chatgpt/oauth/start`

无需请求体。创建包含一次性 `state` 和 PKCE 的授权会话，回调 URI 固定为当前 OAuth listener 的地址。

响应包含 `sessionId`、`authUrl` 和 `expiresAt`。前端在用户明确选择打开授权页面或复制链接后才调用该 API。

## `POST /api/chatgpt/oauth/callback`

用于手动提交 ChatGPT 授权后的完整回调 URL；自动回调 listener 与该 API 共用同一套校验和兑换流程。

请求：

```json
{
  "callbackUrl": "http://localhost:1455/auth/callback?code=...&state=..."
}
```

后端校验 URL 的 HTTP scheme、`/auth/callback` 路径、`state`、回调主机和端口，并要求它匹配当前待完成的授权会话。有效 URL 会用该会话保存的 PKCE verifier 和 redirect URI 兑换令牌。重复提交或与自动回调并发时不会重复兑换。

成功响应返回 `pending`、`processing`、`completed` 或 `error` 状态。格式无效、会话不存在/过期或 URL 与当前会话不匹配时，返回 `400 invalid_oauth_callback`。

## `GET /api/chatgpt/oauth/status?sessionId=...`

查询授权会话状态；会话不存在或过期时返回 `404 oauth_session_not_found`。
