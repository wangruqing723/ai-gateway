# ChatGPT OAuth 授权交互与 Provider 密钥列优化

日期：2026-10-12
状态：实现完成；待本机验证实际 OAuth 流程

## 需求

1. 点击“连接账户”只打开授权选项，不立即创建授权会话或打开浏览器页面。
2. 用户可在弹窗中选择“打开授权页面”或“复制授权链接”。
3. 授权链接生成后，用户可粘贴 ChatGPT 授权完成后返回的完整回调 URL 并提交。
4. Provider 列表中 ChatGPT OAuth Provider 的密钥列应清楚标识 OAuth，不能显示原始 Material Symbols 名称。

## 方案

### 授权弹窗

- “连接账户”只切换前端弹窗状态，不调用 OAuth start API。
- “打开授权页面”按钮在用户手势中先通过 `_blank` 创建新标签页，再请求后端创建 session，成功后导航到授权 URL；打开失败时显示可复制链接。
- “复制授权链接”按钮请求后端创建 session，将完整授权 URL 复制到剪贴板，并在弹窗中显示可选择的 URL 作为复制失败时的兜底。
- 两种操作均使用相同的 session 状态轮询流程；自动或手动回调完成后刷新账户列表，失败时保留可读错误信息。

### 授权回调

- OAuth start API 不接收自定义 `redirectUri`，始终使用 callback listener 当前实际监听地址，并把它绑定到 session。
- 授权链接生成后才显示“ChatGPT 授权后的回调链接”输入框。用户可将浏览器地址栏中的完整 URL 提交给 callback API。
- 后端验证 URL 的 scheme、host、port、path 和 `state`，并要求它与当前待完成 session 匹配；code exchange 使用 session 保存的 PKCE verifier 和 redirect URI。
- 自动 listener 和手动提交共用同一处理函数，通过 session 状态防止并发或重复兑换。
- 当前 callback server 只监听本机 `1455` 或回退端口 `1457` 的 `/auth/callback`。外部域名/反向代理需要额外回调路由和 OAuth 客户端注册配置，不在本需求范围内。

### Provider 密钥列

- 将 `smart_toy` 图标字形替换为显式的 `OAuth`/`ChatGPT OAuth` 小标签。
- 普通 API Key Provider 的已有状态图标保持原样。

## 数据流

1. 用户点击“连接账户”后，前端只显示弹窗，不创建 OAuth session，也不打开授权页面。
2. 用户明确选择打开或复制后，前端调用 start API；打开方式先在当前浏览器窗口创建 `_blank` 标签页。
3. 后端创建 PKCE/state session，并保存固定 callback URI 用于授权请求和 token 交换。
4. ChatGPT 完成授权后，listener 自动接收回调；若自动回调没有完成，用户把完整回调 URL 粘贴到生成链接后出现的输入框提交。
5. 前后端按 state 找到 session，兑换并保存 token；前端轮询后刷新账户列表。

回调地址限于当前 listener 实际端口上的 `localhost` 或 `127.0.0.1`，路径固定 `/auth/callback`。外部域名/反向代理需要额外的回调路由和 OAuth 客户端注册配置，不在本需求范围内。
