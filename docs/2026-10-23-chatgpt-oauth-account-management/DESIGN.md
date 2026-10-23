# ChatGPT OAuth 账户管理体验设计

## 范围

本次只调整 ChatGPT OAuth 账户管理页和 Provider 编辑流程。账户 API 已返回安全视图中的 `expiresAt`、`providers`、`tags`、`note`；不改变 OAuth 刷新策略和账户存储格式。

## 到期状态

- 复用前端 `chatGPTExpiryInfo(account)` 的 72 小时、24 小时和已过期判断，避免列表标签与账户详情出现不同口径。
- `status !== "ready"` 时沿用现有“需重新授权”状态。`ready` 时显示访问令牌到期倒计时；超过 72 小时为正常色，72 小时内和 24 小时内沿用现有警告/危险色。缺少有效 `expiresAt` 时显示“访问令牌到期时间未知”。
- 用 Alpine 响应式时间值每分钟更新一次判断，让固定的 `expiresAt` 也能在页面停留期间产生状态变化。提示只描述访问令牌时间，不代表订阅或额度状态；详情中自动续期说明继续保留。

## 可搜索账户选择器

- Provider 抽屉收起时显示当前账户；展开 ChatGPT 账户下拉框后，在面板内显示搜索输入并由前端过滤已连接账户。
- 搜索不区分大小写，匹配邮箱、账户 ID、套餐、标签和备注；下拉选项仍标明需要重新授权的账户并禁止选择。
- 新建和编辑共用同一搜索状态与过滤逻辑。编辑 Provider 时当前账户仍保持选中；搜索输入不会改动绑定值。键盘可用上下键、回车和 Esc 操作。

## 快捷换绑与解绑

- 任意 `ready` 账户行都提供绑定/换绑已有 Provider 的入口，不要求该账户当前已绑定 Provider。用户选择一个已绑定其他账户的 OAuth Provider 后，将 Provider 改绑给当前账户。
- 改绑目标 Provider 时仅替换其 `accountId`：目标 Provider 之前关联的账户自动解除该关联；目标账户原有的其他 Provider 绑定保留。Provider 的路由、启用状态及其他设置不变。
- 账户行对每个关联 Provider 提供“解绑并停用”操作，并在执行前确认 Provider 名称及停用影响。
- 新增专用解绑 API。服务端在配置操作锁内确认 Provider 当前确实引用该账户，再清除 Provider 的 OAuth `authType` 与 `accountId`，保留其余配置并设为禁用；通过现有配置保存与运行时应用流程提交。清除凭据关联后 Provider 不会在路由请求中被使用，也不会因为缺少 API Key 发出请求。
- 新增账户到已有 Provider 的改绑 API。服务端在配置操作锁内校验目标账户存在且可用、目标 Provider 为 ChatGPT OAuth Provider，再原子替换其账户 ID并保存、热重载。
- 解绑完成后刷新账户列表和 Provider 配置，使“删除账户”立即可用。Provider 记录和路由配置仍保留，用户之后可编辑并重新绑定或配置其他认证方式。

## 安全与失败处理

- 新 API 沿用 ChatGPT 管理接口的同源校验、路径解码和统一 JSON 错误结构。
- 账户或 Provider 不存在、目标账户需要重新授权、Provider 认证类型不匹配、配置 revision 或落盘失败时，不报告绑定变更成功；不在响应中返回令牌或其他凭据。
- 配置更新需遵循现有串行保存、校验、落盘和 `applyRuntimeConfig` 事务顺序；保存失败时保留当前运行配置。

## 主要涉及位置

- `cmd/gateway/web/src/index.template.html`：账户状态、Provider 关联操作、集成搜索的账户下拉框。
- `cmd/gateway/web/src/app/11-providers.js.part`：到期时间刷新、搜索过滤、账户行绑定/换绑与解绑调用。
- `cmd/gateway/main.go`：绑定/解绑路由与配置事务。
- `cmd/gateway/web/index.html`：通过 `make web-html` 重建。
