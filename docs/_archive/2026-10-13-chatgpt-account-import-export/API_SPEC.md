# ChatGPT OAuth 账户导入与导出 API

状态：实现阶段已完成并集成；2026-11-01 按当前代码补正。四种结构的支持是现行代码能力，不等于已经完成四种工具的专项互操作验收；验证边界见 `TASKS.md`。

所有路径仍由 `handleChatGPTAuthAPI` 处理，并沿用同源 Origin 检查。新接口只接受 POST；导出接口把账户 ID 明确列在请求中，导入接口接受原始 JSON 或 ZIP 文件字节。

## 导入

`POST /api/chatgpt/accounts/import`

Content-Type: `application/json` 或 `application/zip`。

- `application/json`：请求体为待导入格式原文；可为单个账户对象、对象数组、sub2api envelope 或 `tokens` nested account。
- `application/zip`：JSON 文件可包含受支持的单账户、数组或批次封装，只在内存中读取，不落盘；允许安全目录条目，拒绝非普通 JSON 文件及不安全路径。
- 限制：JSON/ZIP 原始请求体最大 16 MiB；ZIP 最多 200 个总条目、100 个 JSON 文件，每个 JSON 文件最多 2 MiB，声明的解压总大小最多 16 MiB；解析后整批最多 100 个账户。实际读取单文件也受 2 MiB 上限保护。
- 格式自动识别：sub2api envelope、官方 `auth.json`/nested `tokens`、CLIProxyAPI 与 Cockpit Tools flat token JSON。相同 flat schema 无法区分来源时报告为 `cockpit_tools_or_cliproxyapi`。

成功响应（HTTP 200）：

```json
{
  "data": {
    "detectedFormat": "sub2api",
    "imported": 2,
    "skippedDuplicates": 1,
    "accounts": [
      { "id": "acc_example", "email": "user@example.com", "status": "ready" }
    ],
    "warnings": ["第 3 条账户已存在，未覆盖现有凭据"]
  }
}
```

`accounts` 只返回本次新增账户的现有 `AccountView`，不含令牌。重复项按 ChatGPT account ID 跳过，包括本批次内重复与已存在账户，不更新现有数据。整个批次先验证再保存；任一无效条目使请求失败且不导入其他条目。缺少 `refresh_token` 的账户必须包含未来的到期时间；响应 `warnings` 会标明该账户到期后需要重新授权。新导入账户的 `authorizedAt` 为本次导入时间，不是外部工具的原始授权时间；用量、标签和备注初始为空。

错误响应：

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid_import` | JSON 无效、格式不支持、条目缺少必需 OAuth 字段或 account ID 无法解析 |
| 403 | `forbidden_origin` | 跨站 Origin |
| 413 | `request_too_large` | 请求体、条目数或 ZIP 解压大小超限 |
| 415 | `unsupported_media_type` | Content-Type 不是 JSON/ZIP |
| 503 | `oauth_unavailable` | 账户管理器不可用 |
| 500 | `account_import_failed` | 校验通过后持久化失败，批次已回滚 |

错误文案不得拼接令牌或原始文件片段；可以包含条目序号和字段名。

## 导出

`POST /api/chatgpt/accounts/export`

请求：

```json
{
  "format": "auth_json",
  "accountIds": ["acc_example"]
}
```

`format` 可选值：`cockpit_tools`、`cliproxyapi`、`sub2api`、`auth_json`。`accountIds` 必须为 1–100 个现有账户 ID；重复 ID、未知 ID 或空数组返回 400。导出格式不受影响的只读元信息不进入文件；凭据本身只允许在本接口中返回。

| 格式 | 单账户响应 | 多账户响应 |
| --- | --- | --- |
| `cockpit_tools` | JSON 数组中的 portable token 记录 | 包含全部记录的 JSON 数组 |
| `cliproxyapi` | CLIProxyAPI Codex auth JSON 对象 | ZIP，每个 `.json` 文件对应一个 Codex auth 文件 |
| `sub2api` | `sub2api-data` envelope | 同一 envelope 中含多条 `accounts` |
| `auth_json` | 官方 Codex OAuth `auth.json` 对象 | ZIP，每个账户目录内一份 `auth.json` |

成功响应使用匹配格式的 Content-Type，设置 `Content-Disposition: attachment`、`Cache-Control: no-store`、`Pragma: no-cache` 和 `X-Content-Type-Options: nosniff`。导出请求只接受 `application/json`，请求体上限 16 KiB。多账户 ZIP 中只有 JSON 文件，不包含 config、Provider 绑定、使用量缓存或本地标签/备注。

错误响应：

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid_export` | 请求格式错误、format 不支持、账户 ID 无效或所选账户无法生成目标格式 |
| 403 | `forbidden_origin` | 跨站 Origin |
| 413 | `request_too_large` | 导出请求体超过 16 KiB |
| 415 | `unsupported_media_type` | Content-Type 不是 JSON |
| 503 | `oauth_unavailable` | 账户管理器不可用 |
| 500 | `account_export_failed` | 序列化或 ZIP 生成失败 |

如果所选账户缺少官方 `auth.json` 要求的 `id_token`，`auth_json` 导出返回 400，指出账户条目和缺少的字段；不生成部分 ZIP。
