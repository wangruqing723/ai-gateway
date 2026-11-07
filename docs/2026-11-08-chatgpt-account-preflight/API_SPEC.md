# 账户预检接口

用户已确认 TASKS.md 的 D01–D05，接口已实现。路径以现有 `/api/chatgpt/accounts` 命名空间扩展。

## 1. 单账户诊断扩展

`POST /api/chatgpt/accounts/{id}/diagnose`

```json
{
  "checks": ["local", "inference"],
  "mode": "account",
  "model": "<用户指定模型>",
  "prompt": "<选填测试内容>",
  "confirm": "consume_quota"
}
```

- checks 沿用 local、refresh、usage、inference，本地检查始终执行；步骤重复或未知值拒绝。
- refreshMode 沿用 auto / force；只有选择 refresh 时允许携带。
- mode 可为 account / provider。选择 inference 时，新调用方必须显式给出；兼容旧请求：mode 缺省且 provider 非空按 provider 模式，二者都缺省则拒绝，不静默切换。
- provider 模式要求 provider 对应已保存且绑定当前账户的 OAuth Provider；account 模式禁止 provider 字段。
- model 沿用当前 200 UTF-8 字节、禁止空白与控制字符的约束，不把模型目录成员资格作为手填模型的强制白名单。
- prompt 最多 4096 UTF-8 字节，缺省或空白回落默认 `Reply with OK.`，非空原样使用；非字符串、非法 UTF-8、NUL 或超限拒绝。
- 未选择 inference 时禁止携带 mode、provider、model、prompt、confirm，避免静默忽略。前端按所选步骤组装请求。
- 消费确认沿用 confirm=consume_quota。
- 请求体限制为 64 KiB，超过返回 413；所有普通字段校验必须在出网前完成。

响应保留当前 data 内的诊断报告。实际调用步骤新增 mode 表示 account / provider；账户模式省略 provider。报告不返回 prompt 或输出文本。AccountView 最近调用视图需对应区分来源，不能冒充 Provider 调用。

已有状态与分类优先复用；入参非法 400、账户不存在 404、账户已有诊断运行 409。不因上游单项失败把整个已成功执行诊断的 HTTP 响应改为 5xx。

## 2. 账户模型目录

`GET /api/chatgpt/accounts/{id}/models`

读取当前账户目录，允许命中服务端有效缓存。无有效缓存时按用户触发查询远端；前端必须说明目录查询可能刷新过期凭据。

`POST /api/chatgpt/accounts/{id}/models/refresh`

请求体为空 JSON 对象，显式刷新目录；不等于强制刷新 OAuth 凭据，仍遵守有效 Retry-After。前端禁止循环自动刷新。

响应：

```json
{
  "accountId": "<账户 ID>",
  "models": ["<模型名称>"],
  "source": "chatgpt-codex",
  "queriedAt": "<RFC3339 时间>",
  "cached": false,
  "stale": false,
  "message": "目录来自此账户，不代表已完成模型调用验证"
}
```

复用 Provider 目录的 queriedAt、cached、stale 语义。失败时只在同账户、同凭据身份下允许返回旧目录并标 stale。账户消失或凭据变更时禁止返回旧结果。

错误：账户不存在 404，身份变化 409，限流 429 并带 Retry-After，其他目录服务错误 502；凭据错误沿用现有目录安全分类。响应禁止带凭据世代内部对象和上游原文。

暂停账户允许单独查询目录，遵循已有目录语义；批量预检的暂停出网跳过规则独立，不在批量启动时自动查目录。

## 3. 批量账户预检

`POST /api/chatgpt/accounts/diagnose/batch`

```json
{
  "accountIds": ["<账户 A>", "<账户 B>"],
  "checks": ["local", "inference"],
  "mode": "account",
  "model": "<统一测试模型>",
  "prompt": "<本批统一的选填测试内容>",
  "confirm": "consume_quota"
}
```

- 账户上限、并发、单项与整批预算按 D01 确认值实施。
- 不支持 provider 模式或逐账户覆写模型和文本；禁止 provider 字段。
- accountIds 必须非空、无重复且格式合法。未知字段拒绝。无 inference 时省略 mode/model/prompt/confirm。
- 整批结构和消费确认验证通过前，不启动 worker、不查询目录、不轮换令牌。
- 语法合法但账户不存在、忙碌或在执行期间被删除时返回逐项结果，不使其他账户失败。
- 同时仅一个批量预检；已有批次返回 409。单账户忙碌返回逐项 busy，不无限等待。
- 与管理操作预检分开，不使用 previewToken 或应用接口；没有配置写入，不要求配置 revision。

### 流式响应

成功接收后返回 `text/event-stream`，前端使用 fetch 解析，正确处理跨块、多字节字符和多行事件。建立流前的错误仍为普通 JSON 错误。发送心跳以防本地中间层空闲超时，但不制造假进度。

- `start`：total、concurrency、accountBudgetMs、budgetMs；不能回显 prompt。
- `result`：index、accountId、status，以及已有诊断 report 或安全 message。status 为 success / partial / failure / skipped / cancelled / busy / not_found。
- `done`：total、completed、durationMs、cancelled，以及按 status 汇总的计数。每个输入账户正常情况下恰有一个 result。

汇总定义：仅所选步骤参与成功判定；存在失败且存在成功为 partial，仅失败为 failure；没有失败但所选出网步骤被跳过且本地成功为 partial；全部所选步骤跳过为 skipped；用户未选择的步骤不影响结果。取消未完成的账户为 cancelled，可携带已完成的步骤。不能以总 status 代替逐步骤说明。

流建立后遇到系统性错误，输出安全终止事件并取消剩余任务，不向已提交流写第二套 HTTP JSON 错误。未收到 done 的客户端显示“结果未完整收到”，不自动重试消费操作。

### 取消

前端 AbortController 中断请求；服务端从请求 context 停止派发和等待，及时释放 gate、执行槽和账户互斥。已完成的令牌轮换与调用记录不回滚。前端保留已收到结果，未确认的调用显示未知，不承诺取消费用。

## 4. 通用边界与兼容测试

- 目录 GET 与所有 POST 都校验本机管理访问边界；POST 严格校验 Origin、Content-Type 与 JSON 字段。
- 所有响应 no-store；不把模型目录响应、实际模型输出或 prompt 存入浏览器持久化缓存。
- 账户 ID 使用已有验证和路径解码约束；精确批量路由先于单账户动态后缀分派，避免把 diagnose 当成账户 ID。
- 新旧前端混用：旧的 Provider 诊断请求继续生效；新增字段不会要求已有调用方立即升级。
- 覆盖非法参数零出网、缺少确认、暂停、删除重建、凭据替换、超时、取消、流断开、并发 gate 释放与迟到响应。
- 前后端均不得自动将目录查询成功解释为实际调用成功。
