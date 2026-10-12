# TASKS.md — ChatGPT OAuth 账户接入

日期：2026-10-12
状态：实现完成；本机已完成 OAuth 授权与请求调用验证

| 状态 | 任务 | 优先级 | 估时 | 依赖 |
|---|---|---:|---:|---|
| [x] | 新增 ChatGPT 账户、OAuth session 与 token 存储数据结构 | P0 | 2h | — |
| [x] | 实现 OAuth PKCE/state、localhost 回调、轮询状态、token 交换与刷新 | P0 | 4h | 数据结构 |
| [x] | 扩展 Provider 认证类型、校验、配置脱敏与热重载 | P0 | 2h | 数据结构 |
| [x] | 接入 Codex Responses endpoint、OAuth 鉴权头、手动模型名与账户状态探测 | P0 | 3h | OAuth、Provider |
| [x] | 新增账户管理 API，沿用跨站请求边界并保护 token 不出现在响应中 | P0 | 2h | OAuth、Provider |
| [x] | 增加账户列表/授权/删除 UI 和 OAuth Provider 表单绑定 | P0 | 3h | 管理 API、Provider |
| [x] | 补齐 Docker localhost 回调端口映射、配置示例和使用说明 | P1 | 1h | OAuth、UI |
| [x] | 重建嵌入式前端产物并检查文档、字段序列化与产物同步 | P0 | 1h | 全部 |

## 完成标准

- OAuth 账户可添加、持久化、用于 Provider 路由、查看状态和安全删除。
- Token 仅保存在 `data/` 的账户存储文件中，管理 API、配置读取和日志不返回 token。
- Docker 回调只通过 loopback 端口可达；API Key Provider 既有配置仍可加载和保存。
- OAuth Provider 的配置序列化、UI 表单与 `web/index.html` 产物一致。
