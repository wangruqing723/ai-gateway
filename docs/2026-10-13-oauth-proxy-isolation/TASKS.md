# OAuth 代理隔离任务

- [x] 增加 `AI_GATEWAY_OAUTH_PROXY` Compose 环境变量透传，并更新 `.env.example` | P1 | 20 分钟 | 无
- [x] 让 ChatGPT OAuth 客户端优先使用专用代理，未设置时保持当前全局代理回退 | P1 | 30 分钟 | Compose 环境变量
- [x] 更新 README，说明 OAuth 专用代理、全局提供商代理和直连行为 | P1 | 20 分钟 | 客户端路由
- [x] 检查配置示例和代理路由实现的一致性 | P1 | 15 分钟 | 前三项
