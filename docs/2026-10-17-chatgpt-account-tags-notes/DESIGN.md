# ChatGPT OAuth 账户标签和备注设计

## 当前结构

- `internal/chatgptauth.Manager` 在 `data/chatgpt-accounts.json` 保存 OAuth 账户和令牌；`AccountView` 是不含令牌的管理 API 视图。
- `/api/chatgpt/accounts` 及账户用量、导入/导出、删除路由由 `cmd/gateway/main.go` 的 `handleChatGPTAuthAPI` 分派。
- 管理页账户表格位于 `cmd/gateway/web/src/index.template.html`，账户 API 行为位于 `cmd/gateway/web/src/app/11-providers.js.part`；嵌入式产物由 `make web-html` 更新。

## 数据模型

- 在内部 `account` 和安全 `AccountView` 中增加 `Tags []string` 与 `Note string`。
- JSON 字段使用 `tags` 和 `note`，空值省略；旧账户记录反序列化后自然为空，不更改存储 envelope 版本。
- `viewOf` 返回 tags 的副本，避免 API 视图与账户管理器共享可变 slice。
- 元数据与 OAuth 令牌在同一账户记录中持久化，但继续沿用现有凭据文件权限和原子保存流程。

## 更新接口

- 增加 `PATCH /api/chatgpt/accounts/{id}`，只更新标签和备注；字段可分别省略，至少要提供一个字段。
- 新增 `Manager.UpdateMetadata`，在 Manager 写锁下读取当前值、规范化输入、暂存更新并持久化；落盘失败时恢复旧值。
- Handler 沿用 ChatGPT API 的同源校验，对请求体做大小限制和严格 JSON 解码，只返回更新后的 `AccountView` 与当前 Provider 引用。
- `tags` 显式传空数组可清空标签，`note` 显式传空字符串可清空备注。未传的字段保留原值。
- 约束：最多 20 个标签，每个最多 32 个 Unicode 字符，备注最多 2000 个 Unicode 字符；标签修剪首尾空白、忽略空项并按不区分大小写去重。

## 管理页

- 在紧凑账户行中显示标签 chip，方便账户较多时快速区分。
- 在当前可展开的详情行中增加标签输入、备注多行输入和保存按钮；不修改 OAuth 授权、令牌刷新或 Provider 绑定流程。
- 前端使用独立编辑草稿，保存成功后以服务器返回的 `AccountView` 替换该账户；失败时保留草稿并显示错误。
- 所有账户元数据都作为文本渲染，不将标签或备注插入 HTML。

## 导入、导出与兼容性

- 现有账户导出格式服务于 Cockpit Tools、CLIProxyAPI、sub2api 和官方 `auth.json`，继续只导出各自格式定义的 OAuth 字段，不携带本地标签/备注。
- 外部导入来源没有受本项目控制的通用标签/备注语义，导入的新账户字段为空。
- 本地 JSON 使用可选字段，不需要迁移脚本；升级后读取已有账户，之后的账户保存会写入新字段。
