# ChatGPT OAuth 账户导入与导出设计

## 当前结构

- `internal/chatgptauth/manager.go` 维护账户凭据和 OAuth 流程；磁盘文件使用带 `version`、`accounts` 的内部 envelope，并通过临时文件原子替换，权限为 `0600`。
- `AccountView` 是账户列表和 API 状态视图，刻意不返回令牌。当前持久化账户保存 access/refresh token，但没有保存 OAuth 返回的 `id_token`。
- `/api/chatgpt/accounts` 及用量、删除、OAuth 路由由 `cmd/gateway/main.go` 的 `handleChatGPTAuthAPI` 分派；管理页账户区域在 `web/src/index.template.html`，行为在 `web/src/app/11-providers.js.part`。
- Provider 只引用网关生成的账户 ID。导入外部账户时不应自动改写配置或建立绑定。

## 规范账户与转换边界

在 `internal/chatgptauth` 内把四种文件先转换为单一内部导入记录，再交给 Manager 验证和保存。规范字段为：

```text
id_token, access_token, refresh_token, chatgpt_account_id,
email, plan_type, expires_at, last_refresh
```

字段映射：

| 来源格式 | 入口结构 | 字段映射 |
| --- | --- | --- |
| Cockpit Tools | 单对象、数组；支持 portable 扁平令牌字段及其 Codex 账户 `tokens` 结构 | `account_id` / `chatgpt_account_id`；支持 snake_case 与 camelCase；可忽略标签、分组等网关不管理的元数据 |
| CLIProxyAPI | 单个 auth-dir JSON，或本功能生成的 ZIP | `type: "codex"` 的扁平令牌对象；`expired` 与 `expires_at` 可作为到期时间来源 |
| sub2api | `type: "sub2api-data"`、`version`、`accounts[]` 批次封装，亦接受单个 OAuth account 项 | `accounts[].credentials` 的 OAuth 字段；account `name` 可作为 email 缺失时的展示元数据，不参与 Provider 绑定 |
| 官方 Codex | `auth_mode: "chatgpt"` 的 `auth.json` 对象，亦接受对象数组 | `tokens.{id_token,access_token,refresh_token,account_id}` 与 `last_refresh`；不处理 API Key、PAT 或 Agent Identity 模式 |

检测优先级为 sub2api envelope、官方/嵌套 `tokens` 对象、flat Codex token 文件。Cockpit Tools 与 CLIProxyAPI 的 portable flat token schema 有意兼容；文件本身字段相同、无法可靠判断来源时，在导入结果中标为“Cockpit Tools / CLIProxyAPI 兼容格式”。

账户 ID 按 `account_id`、`chatgpt_account_id`、`chatgptAccountId` 等别名读取；缺失时尝试从 `id_token` 或 `access_token` 的 JWT 声明取得。到期时间读取显式 `expired` / `expires_at`（RFC3339 或 Unix 秒），其次读取 access token 的 `exp`。JWT 仅用于提取元数据，不作为签名验证边界；实际凭据仍由上游验证。

导入必须有 access token 与 ChatGPT account ID。`refresh_token` 可缺省，但须能解析到未来的 access token 到期时间；这类账户在过期前可转发，到期后标记为需重新授权。`id_token` 尽量保留；无 id token 的账户可用于网关转发，但官方格式导出需要有效 `id_token`，此时返回可理解的格式错误。email 和 plan 优先从令牌声明补齐；到期时间不可得时，只有带 refresh token 的账户才可导入。用量不跨格式导入，导入后由网关重新查询。

## 导入事务与身份

1. 解析 JSON/ZIP 时先检查压缩体积、ZIP 条目数、单条目和解压总大小；只读取 `.json`，不解压写入文件系统，拒绝路径异常、非 JSON 和超限 archive。
2. 解析所有账户并完成字段校验后，才进入 Manager 写锁。任何无效条目都会使整个请求失败，不产生部分导入。
3. 以 ChatGPT account ID 做重复检查；已有相同 ID 时保留旧记录并报告跳过，不覆盖可能已轮换的 refresh token。新账户生成网关自己的 `acc_...` ID，首次导入时间作为 `authorizedAt`，usage 从空状态开始。
4. 在内存中暂存新记录，沿用已有临时文件、`fsync`、原子 rename、`0600` 持久化路径；失败则回滚整个导入批次。
5. 新增可选 `IDToken` 持久化字段；旧版本文件缺省时自然为空。OAuth 首次登录与 refresh token rotation 都写入最新 `id_token`；refresh 响应未给该字段时保留原值。

## 导出策略

- 账户选择由导出请求显式传入，避免空参数意外导出全部密钥；管理页的“导出全部”会提交当前账户 ID 列表。
- `cockpit_tools`：portable token 对象数组，可附带 `account_name`；不带 usage、Provider 绑定、网关内部 ID。
- `cliproxyapi`：CLIProxyAPI `type: "codex"` 扁平 auth 文件，字段包含 id/access/refresh token、account_id、email、last_refresh 与 `expired`。单账户下载 JSON，多账户 ZIP 中每个账户一份 JSON。
- `sub2api`：`type: "sub2api-data"`、`version: 1`、`exported_at`、`proxies: []` 与 `accounts[]` envelope；OAuth 令牌放在每条记录的 `credentials` 内。
- `auth_json`：官方 Codex OAuth JSON，使用 `auth_mode: "chatgpt"`、`OPENAI_API_KEY: null`、`tokens` 和 `last_refresh`。单账户文件名为 `auth.json`；多账户 ZIP 每个账户放在独立目录的 `auth.json`。
- ZIP 条目名按 email/account ID 安全清理，避免绝对路径、`..` 和文件名冲突。所有 JSON 使用 UTF-8 与缩进。
- 导出只通过用户明确发起的 POST 返回；设置 `Content-Disposition: attachment`、`Cache-Control: no-store` 和正确 MIME 类型。API 响应、错误、访问日志不写入令牌值。

## 管理页

- 账户列表标题区新增“导入账户”“导出账户”；卡片增加单账户导出快捷入口。
- 导出弹窗可选格式与账户集合，默认全选；所选格式的说明标明多账户会下载 ZIP 的行为。
- 导入弹窗接受 JSON/ZIP 文件，显示自动识别的格式、成功数、重复跳过数和不会泄漏令牌的条目级警告。
- 选择或下载文件前说明内容包含 bearer/refresh 凭据。文件内容只在浏览器内存与本机网关请求中流转，不写入 localStorage。
- 管理页的下载对象 URL 在下载触发后立即回收；请求结束后清空 File 对象、JSON 字符串和临时选择状态。

## 安全约束

- 新路由沿用 `isCrossSiteRequest` 检查和严格 method 检查；请求体与 ZIP 解压内容有单独上限。
- 账户列表、状态轮询、用量和删除响应继续只使用 `AccountView`，不得暴露内部凭据结构。
- ZIP 读取禁止落盘，阻止路径穿越；限制条目数量、单 JSON 大小与总解压大小，避免 ZIP bomb。
- 导出端仅接受现有账户 ID，不能用用户提交的令牌字段直接构造无校验的账户或覆盖既有记录。
