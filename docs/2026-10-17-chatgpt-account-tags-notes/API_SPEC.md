# ChatGPT OAuth 账户标签和备注 API

所有路径由 `handleChatGPTAuthAPI` 分派并沿用同源 Origin 检查。更新接口只操作本地账户元数据，不接收或返回 OAuth 令牌。

## 更新账户元数据

`PATCH /api/chatgpt/accounts/{id}`

请求 `Content-Type: application/json`：

```json
{
  "tags": ["工作", "备用"],
  "note": "用于晚间批处理"
}
```

- `tags`：可选字符串数组；最多 20 项，每项最多 32 个 Unicode 字符。服务端修剪首尾空白、忽略空项，并按大小写不敏感方式去重，保留首个标签的原始大小写。传 `[]` 清空。
- `note`：可选字符串，最多 2000 个 Unicode 字符。传空字符串清空；其他内容原样保存。
- 至少传一个字段；未传字段不变。未知字段、错误类型或超限值返回 400。
- 请求体上限 16 KiB；超限返回 413。

成功响应（HTTP 200）：

```json
{
  "data": {
    "id": "acc_example",
    "email": "user@example.com",
    "status": "ready",
    "tags": ["工作", "备用"],
    "note": "用于晚间批处理",
    "providers": ["chatgpt-main"]
  }
}
```

`data` 是现有 `AccountView`，不包含 `accessToken`、`refreshToken` 或 `idToken`。

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid_account_metadata` | JSON 无效、字段类型错误或标签/备注超限 |
| 403 | `forbidden_origin` | 跨站 Origin |
| 404 | `account_not_found` | 账户 ID 不存在 |
| 413 | `request_too_large` | 请求体超过 16 KiB |
| 503 | `oauth_unavailable` | 账户管理器不可用 |
| 500 | `account_metadata_update_failed` | 原子持久化失败，内存变更已回滚 |

标签和备注是网关本地元数据，不会进入现有第三方兼容导出格式。
