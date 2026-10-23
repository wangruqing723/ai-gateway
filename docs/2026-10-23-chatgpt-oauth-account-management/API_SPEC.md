# ChatGPT OAuth 账户绑定管理 API

- 日期：2026-10-23
- 状态：已实现（未运行测试）

现有 `GET /api/chatgpt/accounts` 已返回 `expiresAt`、`providers`、`tags` 和 `note`。到期标签与账户搜索均由前端使用这些字段，不新增读取接口。

## 删除账户绑定

### `DELETE /api/chatgpt/accounts/{accountId}/bindings/{providerName}`

解除指定 Provider 对指定账户的 OAuth 绑定。该操作保留 `authType: chatgptOAuth` 和 Provider 其他设置，清除 `accountId` 并将 Provider 设为禁用。已禁用且未绑定账户的 OAuth Provider 仍可重新绑定；重新绑定成功后会自动启用。

请求无需 body。路径中的账户 ID 与 Provider 名称须分别 URL 解码。

成功：

```json
{ "status": "unbound", "provider": "codex-main", "enabled": false }
```

错误：

- `404 account_not_found`：账户不存在。
- `404 provider_not_found`：Provider 不存在。
- `409 binding_not_found`：Provider 当前没有绑定该账户，配置未变更。
- `500 config_update_failed`：生成 revision 或保存配置失败；不返回令牌或 API Key。

操作使用 ChatGPT 管理 API 的同源防护，并串行化配置读取、修改、落盘和热重载。OAuth Provider 在已禁用时允许 `accountId` 为空；成功后 `GET /api/chatgpt/accounts` 中该 Provider 会从账户的 `providers` 列表移除，账户满足现有删除条件。响应附带更新后的 `ETag`，与配置 revision 保持一致。

## 更换绑定

### `PUT /api/chatgpt/accounts/{accountId}/bindings/{providerName}`

将已有的 ChatGPT OAuth Provider 绑定到路径中的账户。目标账户须存在且状态为 `ready`；Provider 须存在且认证方式为 `chatgptOAuth`，可处于未绑定或已绑定其他账户状态。成功后旧账户自动解除关联；其他配置保持不变。禁用 Provider 也可选择，绑定成功后自动启用；原本启用的 Provider 保持启用。

请求无需 body。路径中的账户 ID 与 Provider 名称须分别 URL 解码。

成功：

```json
{ "status": "bound", "provider": "codex-main", "accountId": "account-id" }
```

错误：

- `404 account_not_found`：目标账户不存在。
- `409 account_not_ready`：目标账户需要重新授权。
- `404 provider_not_found`：Provider 不存在。
- `409 provider_not_oauth`：Provider 不是 ChatGPT OAuth Provider。
- `500 config_update_failed`：生成 revision 或保存配置失败，运行配置不变。

该操作复用配置串行锁、revision、配置保存和 `applyRuntimeConfig`。成功后账户列表的 `providers` 映射会反映新的关联关系；响应不含任何令牌或其他凭据。
