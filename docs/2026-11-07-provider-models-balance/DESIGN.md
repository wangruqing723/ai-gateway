# OAuth 模型目录与提供商余额查询设计

创建日期：2026-11-07。
代码基线：`7ceddd7`。
状态：已实现并通过本地假数据验收，尚未提交发布；真实站点未实测。
任务：[TASKS.md](TASKS.md)。接口：[API_SPEC.md](API_SPEC.md)。

## 1. 目标与不变量

配置路由时查询绑定 OAuth 账户的远端模型，在提供商列表查看上游实际返回的账户余额或有明确口径的额度。

- 模型目录、账户余额、密钥限额、订阅用量和网关请求统计是不同领域事实。
- 查询属于管理面观测，不修改转发候选、熔断、密钥冷却、共享额度冷却或 Provider 启停。
- 普通查询不返回凭据、上游原文或用户完整资料，不使用管理员凭据查询其他账户。
- 首批明确适配 New API 和 Sub2API。未知中转站显示“未配置查询”或“不支持”，不盲试凭据。
- 保持已有深色、中文、桌面优先的 Operate 管理界面，不新增顶层页签或重做设计系统。
- 本机管理边界沿用 [敏感配置传输决策](../adr/0001-local-sensitive-transfer-boundary.md)，同源限制不等价于身份鉴权。

## 2. 公开协议核验

核验日期为 2026-11-07。使用 Context7 的仓库资料、GitHub 固定源码、发布标签源码及独立集成报告交叉核对。没有请求用户真实上游或访问真实账户。

### 2.1 ChatGPT OAuth 模型目录

官方 Codex 客户端使用 `GET <backend base>/models?client_version=<version>`，标准后端基址为 `https://chatgpt.com/backend-api/codex`。响应是 `models` 数组，模型身份字段为 `slug`，不是普通 OpenAI 列表的 `data[].id`。

- 请求按绑定账户使用 Bearer 与 `ChatGPT-Account-Id`，不得借用客户端传入 API Key。
- `visibility` 有 `list`、`hide`、`none`。默认下拉列出 `list`；未列出的模型仍可手填，不把手填模型强制纳入目录白名单。
- `supported_in_api` 是普通 API 认证下的过滤属性。官方源码在 ChatGPT 模式不据此删除模型，所以本功能不能用它过滤 OAuth 目录。
- 模型清单来自远端，不内置模型名称、不按本地套餐声明猜测清单。
- 目录存在不证明单次生成调用必定成功。公开问题中也有目录 404，因此失败回退是必需项。
- 初始兼容参数提议使用已核验发布版 `0.167.0`，可由 Provider 的 `modelCatalog.clientVersion` 覆盖。该值只表示目录协议版本，不是网关冒充 Codex 运行版本。

证据：

- [ModelsClient 固定源码](https://github.com/openai/codex/blob/f5ac7a757e8f6f4fe922f3daab0fcaba02f1fa72/codex-rs/codex-api/src/endpoint/models.rs)。
- [模型字段与认证过滤](https://github.com/openai/codex/blob/f5ac7a757e8f6f4fe922f3daab0fcaba02f1fa72/codex-rs/protocol/src/openai_models.rs)。
- [账户身份头](https://github.com/openai/codex/blob/f5ac7a757e8f6f4fe922f3daab0fcaba02f1fa72/codex-rs/cloud-client/src/types.rs)。
- [发布标签 rust-v0.167.0](https://github.com/openai/codex/releases/tag/rust-v0.167.0)，发布于 2026-11-06；标签中的模型请求实现与核验主分支文件一致。
- [目录 404 的公开报告](https://github.com/openai/codex/issues/55566)，仅作为失败场景证据，不作为用户账户不可查询的结论。

### 2.2 New API

- `GET /api/user/self` 返回 `data.quota` 和 `data.used_quota`，属于面板令牌所代表的用户账户。
- `GET /api/usage/token` 使用模型 API Key，返回密钥的 `total_available`、`total_used`、`unlimited_quota`；这是密钥限额，不当然等于账户余额。
- 某些版本在密钥获准查询账户额度时额外返回 `data.user_quota`。只有字段确实存在且有效，才将其作为账户额度独立展示。
- `GET /api/status` 公开 `quota_per_unit`。USD 换算是原始配额除以该值，不能把默认常量 500000 当作所有站点的永恒配置。
- 站点的 `quota_display_type` 可能选择 CNY、TOKENS 或自定义符号。本功能优先展示核验过的规范 USD 与原始配额，不凭符号猜汇率；单位发现失败则只显示原始配额。

认证存在明确版本差异，不给一个对所有版本都成立的结论：

| 核验版本 | 公开日期 | 本人账户查询要求 |
|---|---|---|
| v1.0.0-rc.21 | 2026-07-11 | 系统访问令牌；必须有匹配的 `New-Api-User`；无效令牌可能是 HTTP 200 + `success:false` |
| v1.0.0-rc.48 | 2026-11-04 | 个人访问令牌；本人查询要求 `profile:read`；`New-Api-User` 不再参与鉴权 |

方案：统一发送 Bearer；用户 ID 配置了才发送旧版兼容头。响应返回 ID 时核对显式配置的用户 ID，不在失败后改变账户身份。新旧认证错误信封都识别，不把 HTTP 200 自动视为成功。

证据：

- [本人余额字段](https://github.com/QuantumNous/new-api/blob/8c8940c999c6c38031dacdd0139cf7b0e1c38308/controller/user.go)。
- [密钥额度与可选 user_quota](https://github.com/QuantumNous/new-api/blob/8c8940c999c6c38031dacdd0139cf7b0e1c38308/controller/token.go)。
- [旧版鉴权](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.21/middleware/auth.go) 与 [旧版 Bearer 支持](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.21/model/user.go)。
- [新版认证说明](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/docs/authentication.md) 与 [路由权限声明](https://github.com/QuantumNous/new-api/blob/8c8940c999c6c38031dacdd0139cf7b0e1c38308/middleware/access_token_routes.go)。
- [公开单位](https://github.com/QuantumNous/new-api/blob/8c8940c999c6c38031dacdd0139cf7b0e1c38308/controller/misc.go) 与 [金额换算](https://github.com/QuantumNous/new-api/blob/8c8940c999c6c38031dacdd0139cf7b0e1c38308/controller/billing.go)。

### 2.3 Sub2API

已确认用户所指项目是 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)。

- `GET /v1/usage` 使用 API Key，返回原生用量对象，不是 `/api/usage/token` 或 `/dashboard/billing`。
- 钱包分支带 `balance`、`remaining`、`unit: USD`，可以展示账户钱包余额。
- `mode: quota_limited` 返回该密钥的总限额和周期限额。只有速率限额时可能没有总剩余，不能补成钱包余额 0。
- 订阅分支带 `subscription` 和周期 limit/usage。`remaining` 是适用周期中最紧的剩余，不是可提现余额；核验实现以 `-1` 表示未设置周期上限。
- `GET /api/v1/auth/me` 使用独立面板 JWT，成功信封是 `code: 0`，用户 DTO 带 `balance`。可用于用户明确要求账户钱包余额而 API Key 路径只暴露密钥额度的情况。
- 面板 JWT 有到期与撤销控制，并可能开启 IP/UA 会话绑定。不能承诺它与可长期使用的 New API 个人令牌等价；本需求不自动登录或持有 refresh token。
- `/v1/usage` 跳过模型计费检查，但仍执行鉴权和用户状态检查。API Key 被禁用或 IP 受限时查询仍可失败。
- 接口返回的 `balance` 原样作为钱包余额。即使另有冻结余额字段也不自行加减，不猜运营站点的“可用现金”口径。

证据：

- [网关路由](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/server/routes/gateway.go)。
- [Usage 钱包/密钥/订阅实现](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/handler/gateway_handler.go)。
- [API Key 鉴权与计费跳过](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/server/middleware/api_key_auth.go)。
- [本人接口](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/handler/auth_handler.go)、[DTO](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/handler/dto/types.go) 与 [JWT 校验](https://github.com/Wei-Shaw/sub2api/blob/6b4b37db43443b5e88e424fe3c69602cc3810dfb/backend/internal/server/middleware/jwt_auth.go)。
- [发布标签 v0.2.20](https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.20)，发布于 2026-11-07；标签的余额分支已交叉核对。
- [独立集成 PR](https://github.com/ding113/claude-code-hub/pull/1496)，2026-09-26 合并；印证 Sub2API 钱包/密钥/订阅差别与 New API 旧认证兼容。其测试自述不是本网关的验收输出。

## 3. 模块与数据流

建议新增 `internal/providermodels` 与 `internal/providerbalance`，分别负责协议解析、身份隔离和查询缓存。配置及 HTTP/UI 留在已有边界，不把新增逻辑继续堆入 `main.go`。

### 3.1 模型目录

1. 从已保存配置捕获 Provider 快照，OAuth 必须绑定有效本地账户。
2. 沿用 `httpclient.Pool` 解析 Provider 代理；OAuth 令牌获取复用现有按账户和跨进程协调链路。
3. 可取消阶段预算内按需刷新，取得令牌、远端账户 ID 和凭据世代的同一身份快照；网络期间不持有配置或账户全局锁。已轮换凭据的安全写回不能因为目录查询取消而丢失。
4. 向官方模型目录发送请求。保留 Provider 的 UA 和允许的额外头；账户身份头由网关最终维护，额外头不能覆盖。
5. 有界解码 `models[].slug`，默认取 `visibility: list`。去空、去重，不按 `supported_in_api` 排除 OAuth 模型。
6. 缓存前核对配置身份与凭据世代；变更则丢弃旧结果，不重新解释为新账户清单。
7. 前端支持下拉选择、手动刷新、时间提示；失败保留原模型文字，目录缺失不能阻断保存手填值。

默认可取消阶段预算 30 秒，目录请求最多 10 秒，目录响应最多 8 MiB，模型最多 2000 项，单个模型名最多 256 字节，拒绝控制字符。目录原文可能包含长说明，不能沿用余额响应的较小上限。兼容字段未知可忽略，错误类型、核心字段缺失和不合法 JSON 不能静默变成空目录。

必要令牌交换完成后，既有账户管理器用独立的存储锁等待安全保存新凭据，等待上限为 30 秒且不受请求 context 取消。此阶段不计入上述可取消预算，因此不承诺 HTTP 全过程严格在 30 秒内结束。预算耗尽后不继续请求目录；已经发生的令牌轮换仍完成安全写回，写盘失败保留内存新凭据并沿用未保存状态。界面取消不能宣称轮换已回滚，不为缩短查询而修改共享刷新安全语义。

OAuth 默认只缓存服务端内存，TTL 5 分钟。目录失败后保留失败状态和旧成功时间，至少等待 30 秒再重试，遵守更长 Retry-After；关闭再打开下拉不会把失败后的旧数据标回新鲜成功。缓存按 Provider 连接设置、认证方式、本地账户对象/ID、凭据世代及目录版本绑定。不缓存秘密原文，不把摘要暴露到浏览器。换绑、重新授权、删除或外部凭据替换均失效。

暂停是转发准入状态，不阻止用户主动的只读管理查询；不自动恢复账户或解除冷却。目录查询成功不写实际调用健康，不计为活跃模型调用。必要的 OAuth 刷新仍沿用已有管理刷新统计。

### 3.2 余额查询

1. Provider 显式选择适配器和查询来源。
2. `auto` 配置独立令牌时等同账户模式；否则使用已保存单值密钥或用户明确选择的列表密钥。
3. 账户令牌失败不切回 API Key。列表密钥不自动切换，不取多把结果求和，不能取客户端传入密钥。
4. 请求使用 Provider 的 HTTP client/代理与配置 UA。固定适配器负责路径、鉴权、错误信封和金额解析。
5. 返回最小安全快照；一个快照可以分别含账户余额与密钥额度项，但各项来源、单位独立，不能相加。
6. 新结果提交前核对配置身份，避免更换令牌、用户 ID 或密钥后旧响应回填。

余额链总预算 15 秒，包括 New API 单位发现；每个响应最多 1 MiB。解析数字区分缺失/null、0、负数及非有限值。输出金额用十进制字符串，避免 JavaScript 整数精度和二进制浮点误差影响展示。

New API 换算优先显式 `quotaPerUnit`，其次上游 `/api/status` 的有效值。均不可用时保留原始 `quota` 单位；单位发现失败不丢掉已取得的有效原始额度。规范 USD 与原始配额可同时展示，不自动计算 CNY 或自定义币种。

Sub2API 以可信结构判断钱包、密钥、订阅和仅用量，不仅根据 `remaining` 一个字段。未知 mode 或缺关键字段时返回“无法识别”，不用默认零填充。

## 4. 配置与凭据保护

配置草案见 API_SPEC。余额适配和上游协议格式独立，不能从 `format: openai` 推断 New API。

- Provider 新增可选 `balance` 和仅 OAuth 可用的 `modelCatalog`。
- 多密钥 API Key 查询要求 `apiKeyId` 明确绑定。不自动改变选定身份；遵守已有启停与冷却，查询自身不改变这些状态。维护账户余额优先使用独立查询令牌。
- 查询站点默认由 Provider URL 仅去掉末尾 `/v1` 后得出。其他非标准子路径不做猜测，可显式填管理站点基址。
- 同域子路径必须保留。禁止凭据出现在 URL userinfo、查询参数或片段里，不接受任意完整查询接口/自定义脚本。
- API Key 查询不得把现有模型密钥发到不同 origin。需要不同管理域名时必须使用明确配置的独立账户令牌，并在表单显示目标站点。
- 独立余额令牌只由余额适配器发送，不进入转发、模型查询、健康检测或普通额外请求头。
- 新占位符 `__AI_GATEWAY_KEEP_BALANCE_TOKEN__` 与已有 API Key 占位符分域。留空保留与明确清除分开表达。
- 普通配置 JSON、原始 YAML、失败解析、日志与缓存均脱敏。存储到本地配置仍受已有磁盘权限约束，不宣称配置文件加密。
- 修改查询目标站点或类型后不能无提示地沿用旧查询令牌。占位符回填只允许同名且同查询目标的已有配置；变化时用户重新明确提供令牌。
- 提供商/路由敏感导出继续携带配置真实凭据，但必须把查询令牌计入导出警告、秘密计数和预览“已配置”标识。普通查询不因此返回明文。
- 配置迁移输入拒绝查询令牌占位符，不从当前配置猜测导入来源的秘密。
- 表单新输入令牌只留弹窗内存，保存、取消和关闭后清空，不写浏览器存储。

## 5. 缓存、刷新与生命周期

- 成功快照新鲜度为 5 分钟。失败保留上次成功快照及时间，最近失败和最近成功分开显示；重启恢复为未查询，不持久化余额。
- 内存状态只保留当前配置中的来源，Provider 删除、认证或连接指纹变化立即清理。新结果不覆盖新身份。
- 单来源并发请求去重。来源身份包含适配类型、规范化查询目标、认证方式、查询凭据的进程内摘要、选定密钥 ID、用户 ID、单位覆盖、代理和 UA，不只用 Provider 名称。列表密钥额外纳入 Provider 名称以隔离已有 per-provider 冷却状态，不能借另一提供商绕过冷却。共享一个查询来源的多行显示同一余额并注明共享，不重复求和。并发查询的取消按订阅者计数：取消一个页面请求不取消其他请求或后台仍在等待的共享查询，最后一个等待者退出才取消该次出网。
- 用户刷新最短间隔 30 秒，间隔内返回已有状态；上游 429 的 Retry-After 更长时遵守它，时间结束不代表额度恢复。
- 批量最多 50 个 Provider、并发 2、整批预算 60 秒，逐项失败不遮蔽其余结果。整批预算耗尽后保留已完成结果，未开始及本批独占的在途项标记取消。浏览器“刷新当前列表”只作用当前可见已配置项，不悄悄包含被筛选隐藏的 Provider。
- 自动刷新按每 Provider 的周期显式开启，默认 0。统一 worker，随机错开最多十分之一周期；普通失败退避不短于配置周期，最长 24 小时；有效 Retry-After 更长时以它为准，不用退避上限截短。独立令牌明确被拒或权限不足时暂停该来源的自动重试，用户更换凭据或明确手动刷新后重新裁决，避免持续发送已失效的面板 JWT。
- 自动刷新跳过停用 Provider，手动查询仍可维护停用项。没有查询设置的 Provider 不发请求。页面打开只读缓存，不启动自动刷新。
- 热重载更新设置，旧任务取消并按身份丢弃；关闭自动刷新或服务退出时取消等待与出网，不启动多个调度循环。
- 所有 HTTP redirect 默认拒绝，避免自定义账户身份头和令牌流向新 origin；不为此修改全局转发客户端策略。

## 6. 页面交互与验证

### 路由模型组合框

沿用抽屉和行内编辑，允许搜索、方向键与回车选模型。显示查询来源和时间，提供“刷新远端模型”；空目录、错误与过时缓存分别表达，不覆盖手填值。OAuth 账户更换后不读取上一账户的浏览器缓存。

### 提供商编辑与列表

编辑抽屉增加可折叠的“余额查询”区：适配类型、来源、管理基址、查询密钥选择或独立令牌、New API 兼容用户 ID、可选单位覆盖和自动刷新周期。保存配置与查询是不同动作，未保存设置提示先保存，不能查询草稿中的未知目标。

列表增加“余额 / 配额”列：大字为值与单位，小字为“账户余额”“密钥额度”或“订阅额度”。未配置、未查询、查询中、过时、错误、明确无限额和真实 0 均有文字。详情展示实际来源、查询时间和窗口，不以 hover 作为查看必要信息的唯一方式。

沿用现有字体、颜色和图标，余额页不新增统计图。窄屏以换行和展开详情承载次要字段，不缩到 11px。加载时保留旧数值而不是闪成零。错误不会弹出包含上游正文的提示。

验收覆盖容器 Go 全量检查、原始配置脱敏、配置迁移、多密钥绑定、缓存身份竞争、默认无出网、自动刷新生命周期和两种模型编辑入口。浏览器测试只读取实际构建产物并拦截全部网络，不能访问真实账户或余额站点。
