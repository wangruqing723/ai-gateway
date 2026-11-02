# ChatGPT OAuth 账户绑定管理 API

- 日期：2026-10-23
- 状态：已集成；现有通用回归通过；2026-11-01 使用者确认既有验收完成

原“未运行测试”是实现当时的阶段记录，已由 `TASKS.md` 中的现有回归记录替代；本轮不新增专项测试，也未修复下述普通配置保存限制。

现有 `GET /api/chatgpt/accounts` 已返回 `expiresAt`、`providers`、`tags` 和 `note`。到期标签与账户搜索均由前端使用这些字段，不新增读取接口。

## 删除账户绑定

### `DELETE /api/chatgpt/accounts/{accountId}/bindings/{providerName}`

解除指定 Provider 对指定账户的 OAuth 绑定。该操作保留 `authType: chatgptOAuth` 和 Provider 其他设置，清除 `accountId` 并将 Provider 设为禁用。已禁用且未绑定账户的 OAuth Provider 仍可重新绑定；新绑定的变更分支成功后会自动启用。此处不表示后续普通 `PUT /api/config` 已支持空账户引用，详见本节现行限制。

请求无需 body。路径中的账户 ID 与 Provider 名称须分别 URL 解码。

成功：

```json
{ "status": "unbound", "provider": "codex-main", "enabled": false }
```

错误：

- `400 invalid_account_id` / `invalid_provider_name`：路径值为空、无法解码或包含斜杠。
- `404 account_not_found`：账户不存在。
- `404 provider_not_found`：Provider 不存在。
- `409 binding_not_found`：Provider 当前没有绑定该账户，配置未变更。
- `500 config_update_failed`：生成 revision 或保存配置失败；不返回令牌或 API Key。

操作使用 ChatGPT 管理 API 的同源防护，并串行化配置读取、修改、落盘和热重载。配置层允许已禁用 OAuth Provider 的 `accountId` 为空，专用解绑 API 可成功保存；成功后 `GET /api/chatgpt/accounts` 中该 Provider 会从账户的 `providers` 列表移除，账户满足现有删除条件。响应附带更新后的 `ETag`，与配置 revision 保持一致。

### 现行限制：解绑后普通配置保存失败

通用 `PUT /api/config` 对每个 `authType: chatgptOAuth` Provider 无条件执行 `Account(p.AccountID)`，没有跳过已禁用且空 `accountId` 的记录。故解绑成功后再执行普通配置保存，即使只改其他设置，也会返回 `400 config_validation_error`，提示该 Provider 引用了不存在的 ChatGPT OAuth 账户。

2026-11-01 主会话已在 `--network none` 容器中使用临时假账户复现：绑定成功与解绑前普通保存均为 200，解绑为 200（`enabled=false`、`accountId` 省略），解绑后普通保存为 400，重新绑定后普通保存恢复 200。此记录不是本次文档维护重跑，也不涉及真实授权或上游。问题已移交 `docs/KNOWN_ISSUES.md` 的“OAuth Provider 解绑后普通配置保存失败”统一跟踪，**未修复**；使用者确认的常规验收不包含此缺陷已修的结论。

## 更换绑定

### `PUT /api/chatgpt/accounts/{accountId}/bindings/{providerName}`

将已有的 ChatGPT OAuth Provider 绑定到路径中的账户。目标账户须存在且状态为 `ready`；Provider 须存在且认证方式为 `chatgptOAuth`，可处于未绑定或已绑定其他账户状态。新绑/改绑成功后旧账户解除该 Provider 的关联，其他配置保持不变；禁用 Provider 在这个变更分支保存成功后会自动启用，原本启用的保持启用。

同账户重复 PUT 的例外：若 Provider 的 `accountId` 已等于路径账户 ID，接口直接返回 HTTP 200 和 `status: bound`，不重新保存配置、不发放新 ETag，也不改变 `enabled`。因此原本禁用的同账户 Provider 仍保持禁用，不能把这一幂等成功当成“自动启用”。

请求无需 body。路径中的账户 ID 与 Provider 名称须分别 URL 解码。

成功：

```json
{ "status": "bound", "provider": "codex-main", "accountId": "account-id" }
```

错误：

- `400 invalid_account_id` / `invalid_provider_name`：路径值为空、无法解码或包含斜杠。
- `404 account_not_found`：目标账户不存在。
- `409 account_not_ready`：目标账户需要重新授权。
- `404 provider_not_found`：Provider 不存在。
- `409 provider_not_oauth`：Provider 不是 ChatGPT OAuth Provider。
- `500 config_update_failed`：生成 revision 或保存配置失败，运行配置不变。

发生绑定变更时复用配置串行锁、revision、配置保存和 `applyRuntimeConfig`，响应附带新的 ETag；同账户提前返回的例外见上文。成功后账户列表的 `providers` 映射反映新的关联关系，响应不含任何令牌或其他凭据。
