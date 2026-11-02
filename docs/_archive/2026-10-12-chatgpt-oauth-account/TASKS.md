# TASKS.md — ChatGPT OAuth 账户接入

日期：2026-10-12
状态：已集成；保留本机 OAuth 授权与请求调用验证的历史记录；2026-11-01 归档记录已补全

| 状态 | 任务 | 优先级 | 估时 | 依赖 |
|---|---|---:|---:|---|
| [x] | 新增 ChatGPT 账户、OAuth session 与 token 存储数据结构 | P0 | 2h | — |
| [x] | 实现 OAuth PKCE/state、localhost 回调、轮询状态、token 交换与刷新 | P0 | 4h | 数据结构 |
| [x] | 扩展 Provider 认证类型、校验、配置脱敏与热重载 | P0 | 2h | 数据结构 |
| [x] | 接入 Codex Responses endpoint、OAuth 鉴权头、手动模型名与账户状态探测 | P0 | 3h | OAuth、Provider |
| [x] | 新增账户管理 API，沿用跨站请求边界并保护 token 不出现在响应中 | P0 | 2h | OAuth、Provider |
| [x] | 增加账户列表/授权/删除 UI 和 OAuth Provider 表单绑定 | P0 | 3h | 管理 API、Provider |
| [x] | 补齐生产 Compose 的 localhost 回调端口映射、配置示例和使用说明；开发 Compose 不发布回调端口 | P1 | 1h | OAuth、UI |
| [x] | 重建嵌入式前端产物并检查文档、字段序列化与产物同步 | P0 | 1h | 全部 |

## 完成标准

- OAuth 账户可添加、持久化、用于 Provider 路由、查看状态和安全删除。
- Token 保存在 `data/` 的账户存储文件中；普通管理 API、配置读取和日志不返回 token，后续新增的显式导出接口为专用例外。
- 生产 Compose 的 Docker 回调只通过 loopback 端口可达；开发 Compose 不发布回调端口，不能据生产验收认定开发自动回调也通过。API Key Provider 既有配置仍可加载和保存。
- OAuth Provider 的配置序列化、UI 表单与 `web/index.html` 产物一致。

## 2026-11-01 文档维护与归档记录

- [x] 对照当前代码基准 `46a72c6` 确认基础接入已集成（原实现提交 `d99c7e3`），保留“本机已完成 OAuth 授权与请求调用验证”的既有历史记录，不改写为本次维护重新授权。
- [x] 同步后续授权交互、账户安全视图及解绑后的配置限制；把回调发布范围明确为生产 Compose，开发 Compose 只发布管理端口 `7790`，不追加开发容器自动回调已验收的结论。
- [x] 将 OAuth 会话/state/PKCE、过期与端口失败、令牌刷新/轮换及持久化失败边界的专项回归缺口移交 `docs/KNOWN_ISSUES.md` 的“ChatGPT 账户功能专项回归覆盖”统一跟踪；这些检查项不是已新增或已通过的专项测试。

主会话于 2026-11-01 已在 Docker `golang:1.27-alpine` 实跑前端 `webbuild -check`、`go vet ./...`、`go test -count=1 ./...`，均 exit=0，相关包 race 检测通过。本次文档编辑没有新增业务测试；通用验证由主会话实际运行，结果如上。`internal/chatgptauth` 当前没有 `*_test.go`；通用 HTTP 与假账户转发测试不证明真实 OAuth 及其全部错误/回滚边界。

归档口径：基础实现阶段完成，遗留专项覆盖转为根级已知问题继续跟踪；文档已同步，目录已于 2026-11-02 移入归档，归档变更尚未提交。

## 最终归档状态（2026-11-02）

- 已按使用者授权移入 `docs/_archive/2026-10-12-chatgpt-oauth-account/`，需求创建日期保留。
- 文档按当前代码和使用者已确认的验收同步，历史失败或阶段记录不再表示当前归档阻断；仍未修复的缺陷和专项测试欠账保留在 [统一已知问题](../../KNOWN_ISSUES.md)。
- 本次仅维护和移动文档，没有修改业务代码、真实配置或账户，没有提交、合并或推送。
