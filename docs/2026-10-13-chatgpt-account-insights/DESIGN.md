# 设计：ChatGPT 账号授权与用量信息

## 背景

当前账号管理 API 已提供邮箱、套餐、授权状态、访问令牌到期时间、最近刷新时间和绑定提供商。页面当前主要显示邮箱、账号 ID、套餐、状态及绑定关系。授权时长和用量窗口尚未进入安全视图。

`ExpiresAt` 表示访问令牌的到期时间。它通常会通过 refresh token 自动续期，因此不能把它当作账号已授权时长或 ChatGPT 订阅截止日期。

## 数据流

1. OAuth 首次授权成功时记录 `AuthorizedAt`。已有关联账号重新授权时保留原值。
2. 解析 ID token / access token 中已有的套餐类型和可选订阅日期声明；缺少声明时不生成猜测值。
3. 管理 API 使用服务端凭据请求 ChatGPT usage 接口，解析实际返回的各限额窗口和可选 credits 信息。
4. 账号列表提供最近一次的安全用量快照。快照短暂缓存在管理器内存中；未缓存或超过 5 分钟时，页面触发批量刷新。
5. UI 显示每个窗口的剩余百分比和重置时间。手动刷新可立即重新查询；单个账号失败只更新该账号的查询状态，不影响其他账号。

## 字段与展示语义

- **首次授权时间 / 已授权时长**：来自持久化的 `AuthorizedAt`，仅在首次完成 OAuth 时写入，重新授权不变。
- **套餐/账号类型**：优先使用 token claim 中已有的 `chatgpt_plan_type` / `plan_type`；若 usage 响应提供类型则更新已知值。未知时显示“未知”。
- **订阅周期**：可选的 `chatgpt_subscription_active_start`、`chatgpt_subscription_active_until` 声明；它们与用量窗口重置时间分别展示。此类 claim 是可选上游元数据，不能作为每个账号都有的保证。
- **剩余用量**：每个上游 rate limit window 的 `used_percent` 转为 `100 - used_percent`。以窗口实际时长和重置时间标注；不硬编码某个配额窗口。
- **Credits**：若上游返回余额及相关标记则显示；否则不显示该项。
- **查询状态**：记录最近查询时间及安全的错误摘要；不包含上游原始响应、token 或敏感请求头。
- **历史账号**：旧账号若缺少 `AuthorizedAt`，页面显示“未知”。不从文件修改时间、token 刷新时间或本次重新授权时间推算。

## 后端设计

- 扩展已持久化的 account 元数据及 `AccountView` 安全结构；凭据字段继续仅留在服务端。
- 增加 usage client，复用 OAuth 管理器持有的账号凭据，并在请求前确认 access token 有效。
- 对 usage 请求设置独立超时、并发上限和短期缓存；不要让列表读取等待所有上游请求。
- 以每个账号为单位记录错误，成功响应的账号仍返回新快照。
- 使用 ChatGPT Web backend 的 usage 接口作为兼容实现。该接口不是本项目可依赖的稳定公开 API，响应字段和可用性可能变化；解析逻辑应允许字段缺失，并使失败只影响用量卡片。

## 前端设计

- 在现有账号卡片中保留状态和绑定提供商，并增加“已授权时长”“账号/套餐类型”“用量”信息区。
- 按窗口逐项显示剩余百分比和重置时间；没有数据时显示获取中、尚无数据或查询失败状态。
- 增加页面级“刷新用量”按钮。账号列表初次加载后，只刷新无缓存或超过缓存期限的数据；刷新过程不阻塞账号操作。
- 保持窄屏布局可读；不在 UI 中暴露令牌或原始 claims。

## 参考与兼容性

OpenAI Codex 当前实现解析的 rate-limit 信息包含 `used_percent`、窗口时长/重置时间及可选 credits；这可以作为响应字段的参考，但不代表 ChatGPT Web backend 是稳定公开 API：[Codex backend client](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client.rs)。社区 CLIProxyAPI 使用该 usage 接口的经验也显示，上游错误和响应变化需要由 UI 容错：[CLIProxyAPI issue #2503](https://github.com/router-for-me/CLIProxyAPI/issues/2503)。

## 尚待用户确认

本方案把“授权时长”定义为首次绑定至今的时长，并将订阅有效期单独展示；旧账号缺少首次授权时间时显示未知。若用户希望“授权时长”表示其他语义，应在实现前调整字段定义。
