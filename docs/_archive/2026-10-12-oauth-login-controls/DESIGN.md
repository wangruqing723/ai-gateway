# ChatGPT OAuth 授权交互与 Provider 密钥列优化

日期：2026-10-12
状态：已集成；2026-11-01 使用者确认既有验收完成；文档已按当前代码补正

## 需求

1. 点击“连接账户”只打开授权选项，不立即创建授权会话或打开浏览器页面。
2. 用户可在弹窗中选择“打开授权页面”或“复制授权链接”。
3. 授权链接生成后，用户可粘贴 ChatGPT 授权完成后返回的完整回调 URL 并提交。
4. Provider 列表中 ChatGPT OAuth Provider 的密钥列应清楚标识 OAuth，不能显示原始 Material Symbols 名称。

## 方案

### 授权弹窗

- “连接账户”只切换前端弹窗状态，不调用 OAuth start API。
- “打开授权页面”按钮在用户手势中先通过 `_blank` 创建空白标签页，再请求后端创建 session，成功后导航到授权 URL；若新标签页被拦截，则不创建 session，提示改用复制链接。已创建 session 后导航失败时保留可复制链接。
- “复制授权链接”按钮请求后端创建 session，将完整授权 URL 复制到剪贴板，并在弹窗中显示可选择的 URL 作为复制失败时的兜底；已有进行中的会话时复用同一授权链接。
- 两种操作均使用相同的 session 状态轮询流程；自动或手动回调完成后刷新账户列表，并在完成提示中展示可得的邮箱，失败时保留可读错误信息。邮箱与账户状态展示已落地，不再是待实现项。

### 授权回调

- OAuth start API 不接收自定义 `redirectUri`，始终使用固定主机 `localhost` 与 callback listener 当前实际端口组成 URI，并把它绑定到 session；listener 的绑定 IP 不替换 URI 主机。
- 授权链接生成后才显示“ChatGPT 授权后的回调链接”输入框。用户可将浏览器地址栏中的完整 URL 提交给 callback API。
- 后端验证 URL 的 scheme、host、port、path 和 `state`，并要求它与当前待完成 session 匹配；code exchange 使用 session 保存的 PKCE verifier 和 redirect URI。
- 自动 listener 和手动提交共用同一处理函数，通过 session 状态防止并发或重复兑换。
- 当前 callback server 使用端口 `1455`，占用时回退 `1457`，路径固定 `/auth/callback`；默认监听 loopback，生产 Compose 在容器内监听 `0.0.0.0` 后仅发布到宿主机 loopback。外部域名/反向代理需要额外回调路由和 OAuth 客户端注册配置，不在本需求范围内。

### Provider 密钥列

- 将 `smart_toy` 图标字形替换为显式的 `OAuth`/`ChatGPT OAuth` 小标签。
- 普通 API Key Provider 的已有状态图标保持原样。

## 数据流

1. 用户点击“连接账户”后，前端只显示弹窗，不创建 OAuth session，也不打开授权页面。
2. 用户明确选择打开或复制后，前端调用 start API；打开方式先在当前浏览器窗口创建 `_blank` 标签页。
3. 后端创建 PKCE/state session，并保存固定 callback URI 用于授权请求和 token 交换。
4. ChatGPT 完成授权后，listener 自动接收回调；若自动回调没有完成，用户把完整回调 URL 粘贴到生成链接后出现的输入框提交。
5. 前后端按 state 找到 session，兑换并保存 token；前端轮询后刷新账户列表。

回调地址必须匹配 session 保存的 scheme、主机、端口和路径；当前 URI 的主机固定为 `localhost`，路径固定 `/auth/callback`，`127.0.0.1` 虽为 loopback 也不能替代 session 的主机。原“localhost 或 127.0.0.1”描述与现行 `sameCallbackURI` 不符，以此口径替代。外部域名/反向代理需要额外的回调路由和 OAuth 客户端注册配置，不在本需求范围内。

生产 Compose 发布回调端口到宿主机 loopback；开发 Compose 仅发布管理端口 `7790`，不发布回调端口。本次使用者确认的是既有真实授权流程验收，不新增开发容器自动回调已通过的结论。
