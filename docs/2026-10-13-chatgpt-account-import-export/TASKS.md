# ChatGPT OAuth 账户导入与导出

## 目标

在管理页导入、导出 ChatGPT OAuth 账户，并兼容 Cockpit Tools、CLIProxyAPI、sub2api 与官方 Codex `auth.json` 的令牌结构。导入后账户仍由网关账户管理器续期、持久化和供 Provider 绑定。

## 范围与约定

- 在 ChatGPT OAuth 账户区域提供导入、导出入口，并支持从单个账户卡片直接导出。
- 可选择单个或多个账户导出。Cockpit Tools 与 sub2api 使用各自可识别的 JSON 结构；多账户 CLIProxyAPI 与官方 `auth.json` 导出为 ZIP，内部每个账户一份独立 JSON 文件。
- 导入接受 JSON 或本功能导出的 ZIP，自动识别格式；单文件、数组和批量封装均可导入。
- 只处理人类 ChatGPT OAuth 令牌；API Key、Agent Identity、Bedrock 等 `auth.json` 模式不属于本需求。没有 refresh token 的条目可在 access token 有效期内使用，到期后须重新授权。
- 导入要求 `access_token`，并要求能从 `account_id` 或令牌声明中得到 ChatGPT account ID。缺少 `refresh_token` 的令牌仅在可解析到未来的 access token 到期时间时接受；到期后账户须重新授权。
- 导入账户不自动创建或修改 Provider 绑定。已存在的 ChatGPT account ID 视为重复项并跳过，保留现有令牌、账户 ID 和绑定关系；单次导入整体原子化，任何无效条目都不落盘。
- 继续使用 `data/chatgpt-accounts.json` 的原子写入和 `0600` 权限。令牌只出现在明确触发的导出响应中，不进入普通账户列表 API、日志或错误消息。
- 导入支持 `id_token`；如果外部令牌缺少该字段，网关仍可在令牌本身包含足够身份信息时导入，但生成官方 `auth.json` 时需给出清楚的格式错误。

## 任务

1. [x] 在 `internal/chatgptauth` 增加共享格式解析、规范化、格式导出与批量导入/导出能力；持久化 `id_token`，并在 OAuth 登录与刷新时同步更新。
2. [x] 加入重复账户识别、全量校验后原子保存、ZIP 安全边界和格式错误信息；确保导入不改动 Provider 配置。
3. [x] 在 `cmd/gateway` 增加同源保护的导入/导出 POST API，设置请求体上限、下载文件名和 `Cache-Control: no-store`，成功响应只返回不含令牌的账户视图与汇总。
4. [x] 在账户管理区增加导入对话框、格式选择/说明、多账户选择导出，以及卡片级快捷导出；支持选择 JSON/ZIP 文件并显示导入结果。
5. [x] 同步嵌入式前端产物，确保页面模板与开发/生产实际使用的资源一致。

## 验收标准

- 四种格式的账户可导入；四种格式均可导出；导出的 CLIProxyAPI/官方批量 ZIP 可再次导入。
- 一份文件含多账户时能一次处理；任一条目格式错误时没有账户被写入。
- 重复导入不会覆盖现有令牌或破坏 Provider 绑定，并会在汇总中显示跳过数量。
- 页面普通账户列表与状态接口不包含 access/refresh/id token。
- 导出文件正确设置附件下载和禁止缓存头；令牌文件保持 `0600`。

## 格式参考

- [Cockpit Tools Codex 格式转换](https://github.com/jlcodes99/cockpit-tools/blob/main/src/utils/codexExportFormats.ts)：`cockpit_tools`、`auth_json`、`sub2api`、`cpa` 的导出字段与多文件规则。
- [CLIProxyAPI Codex token storage](https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/auth/codex/token.go)：扁平 `id_token`、`access_token`、`refresh_token`、`account_id`、`last_refresh`、`email`、`type`、`expired` 文件结构；兼容可选 `plan_type`。
- [sub2api Codex 导入](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/handler/admin/account_codex_import.go)：从 `tokens`、扁平字段及 `credentials` 读取 Codex OAuth 凭据。
- [官方 Codex auth storage](https://github.com/openai/codex/blob/main/codex-rs/login/src/auth/storage.rs) 与 [TokenData](https://github.com/openai/codex/blob/main/codex-rs/login/src/token_data.rs)：`auth_mode: "chatgpt"`、`tokens`、`last_refresh` 与官方令牌字段。
