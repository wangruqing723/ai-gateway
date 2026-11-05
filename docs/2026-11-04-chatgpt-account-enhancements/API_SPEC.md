# ChatGPT OAuth 账户增强接口草案

创建日期：2026-11-04
核对基线：`9e2c92a`。
状态：第一、二阶段接口已实现（见 2.1、3.3、4.1），其余仍为规划提案。第二阶段参数已获用户确认，见 TASKS.md「第二阶段已确认参数」。

任务：[TASKS.md](TASKS.md)。设计：[DESIGN.md](DESIGN.md)。未定的产品参数见 TASKS.md 的 D01–D09。

## 1. 兼容性与共同约束

### 1.1 保留现有接口

| 已有接口 | 当前能力 | 本需求约束 |
|---|---|---|
| `GET /api/chatgpt/accounts` | 返回安全账户列表及绑定名称 | 保留 `data` 和旧字段；新增安全视图及账户 ETag |
| `POST /api/chatgpt/accounts/import` | 原始 JSON/ZIP 导入，重复跳过 | 保留默认行为；不隐式增加覆盖参数 |
| `POST /api/chatgpt/accounts/export` | 明确账户集合和第三方格式导出 | 继续兼容，不加入本地标签备注 |
| `PATCH /api/chatgpt/accounts/{id}` | 更新标签备注 | 保留既有请求形状；新批量和暂停采用新入口 |
| `DELETE /api/chatgpt/accounts/{id}` | 未被引用时删除 | 不自动解除绑定 |
| `PUT`、`DELETE /api/chatgpt/accounts/{id}/bindings/{provider}` | 绑定、换绑、解绑并停用提供商 | 保留配置 ETag 与现有幂等差别 |
| `POST /api/chatgpt/accounts/usage/refresh` | 全账户刷新，支持 force 查询参数 | 保留既有刷新与缓存语义 |
| `POST /api/chatgpt/accounts/{id}/usage/refresh` | 单账户用量刷新 | 保留现有入口 |
| `POST /api/chatgpt/oauth/start`、`POST /api/chatgpt/oauth/callback`、`GET /api/chatgpt/oauth/status` | 授权会话与手动回调 | 不重定义授权协议 |

本需求不默认把旧接口改成必须携带新参数。旧接口产生的账户变更也必须推进新增控制版本，防止绕过并发检查。

### 1.2 新接口共同规则

- 沿用网关 Host 防护和本机管理边界，不新增 CORS 放行。
- 新增敏感迁移、显式凭据更新、诊断与管理变更入口要求可信本机浏览器同源 Origin。新增只读账户视图保持现有同源防护，不影响模型客户端的无 Origin 请求兼容。
- 不把上述检查称为身份认证，不能直接向公网暴露管理面。
- JSON 请求只接受一个严格对象，拒绝未知字段、重复键、错误类型和尾随值。
- 固定方法，错误方法返回 405 并附 Allow；媒体类型不匹配返回 415。
- 请求体、解码大小、账户数、响应大小和并发均有上限。现有原始导入上限保持不变；新封装额外大小限制在第二阶段冻结。
- 所有新增账户响应禁止缓存，使用 `Cache-Control: no-store`、`Pragma: no-cache` 和 `X-Content-Type-Options: nosniff`。
- 普通响应、预检和错误只返回安全元信息，不返回令牌、密码、凭据哈希或原始上游正文。
- 示例使用假数据。新增枚举和值只是拟议的网关内部契约，不代表上游字段。

### 1.3 版本与预检凭证

账户列表拟附带账户控制 ETag。新管理写入、导入和恢复应用要求精确 `If-Match`；不接受弱 ETag 或通配符。缺少返回 428，冲突返回 409。

涉及绑定影响的预检同时绑定当前配置版本。用量快照刷新不改变账户控制版本，但目标凭据轮换仍由内部版本检查防止陈旧覆盖。

预检凭证使用有期限、不可伪造且不含源内容的签名令牌，绑定源数据、规范化选择、基础版本、相关凭据版本和最终计划。摘要使用带服务端密钥的方式，不返回公开秘密哈希。

应用时重新解析和重建计划，不相信前端提供的差异，不用预检凭证跳过校验。成功后的版本变化使旧计划不可重放。有效期和输入封装参数在第二阶段确认，不能被实现方自行补成未知默认。

## 2. 安全账户视图

保留现有 `id`、`email`、`plan`、`status`、时间、`tags`、`note`、`usage` 和 `providers`。新增字段按阶段提供：

| 拟新增字段 | 语义 | 阶段 |
|---|---|---|
| `enabled` | 用户管理状态，旧数据缺失时按启用兼容 | 第三阶段 |
| `authorization` | 令牌有效、可刷新、需重新授权及观测时间 | 第一阶段 |
| `usageState` | 查询状态、最后成功时间、缓存新鲜度 | 第一阶段 |
| `invocation` | 最近真实调用状态、来源、模型和时间 | 第一阶段基础；第四阶段完善 |
| `admission` | 账户暂停、授权阻断、冷却和资源状态 | 第五阶段 |
| `background` | 后台任务是否启用、最近结果、下次计划 | 第六阶段 |

拟议最小示例：

```json
{
  "data": [
    {
      "id": "acc_example",
      "email": "account@example.invalid",
      "status": "ready",
      "authorization": {
        "state": "refreshable",
        "message": "访问令牌已到期，可尝试自动刷新"
      },
      "usageState": {
        "state": "error",
        "freshness": "stale",
        "message": "查询失败，保留上次成功数据"
      },
      "invocation": {
        "state": "unknown",
        "message": "尚无真实调用记录"
      },
      "providers": ["example-provider"]
    }
  ]
}
```

`ready` 不因为 `invocation` 未知而变成不可用，也不因为存在 refresh token 就假装已验证刷新成功。状态时间和额度数据必须有来源。

### 2.1 第一阶段已实现的字段

以下字段已随第一阶段实现，出现在 `GET /api/chatgpt/accounts`、单账户用量刷新、标签备注更新、导入结果和授权完成视图中。旧字段语义不变。

| 字段 | 取值与含义 |
|---|---|
| `authorization.state` | `valid`：访问令牌未过期。`refreshable`：访问令牌已过期或缺失，但有刷新凭据。`reauthorization_required`：刷新凭据被拒绝，或没有刷新凭据且访问令牌已过期。 |
| `authorization.canRefresh` | 是否持有可用的刷新凭据；需要重新授权时为 `false`。 |
| `authorization.lastRefreshFailedAt`、`lastRefreshError` | 最近一次自动刷新失败的时间和固定文案；刷新或重新授权成功后清空；调用方取消不记录。 |
| `authorization.credentialsUnsaved` | 刷新得到的新凭据写盘失败，只在内存中；下一次成功写盘后清除。 |
| `usageState.state` | `unknown`、`ok`、`error`。 |
| `usageState.freshness` | `none`：没有成功数据。`fresh`：最近成功查询在 30 分钟缓存周期内。`stale`：已超过缓存周期。 |
| `usageState.lastAttemptAt`、`lastSuccessAt` | 旧快照缺少 `lastSuccessAt` 且没有错误时，以最近一次尝试时间兼容。 |
| `usageState.resetPendingWindows` | 重置时间已到、但还没有更新数据的窗口名称；不代表额度已恢复。 |
| `invocation.state` | `unknown`、`success`、`failure`；附 `provider`、`model`、`observedAt`、`httpStatus`、`reason`。只记录真实发出的上游尝试，客户端取消不记录。 |

`authorization`、`invocation` 中的刷新失败和调用结果只属于当前进程，不写入账户文件，重启后回到未知。重新授权会清空旧凭据的调用结果，并丢弃重新授权前开始、之后才返回的尝试结果。

OAuth Provider 的健康状态新增 `unverified` 档位，出现在 `/health` 的 `providerHealth` 与 `POST /api/providers/health` 响应中。它表示凭据条件满足、但该 Provider 还没有经网关的真实调用结果。前端按未检测同档计数，不显示为健康。只有经该 Provider 的真实调用才能得到 `ok`、`warn` 或 `error`：

- 限流、其他 4xx 和响应转换失败为 `warn`。
- 鉴权拒绝、5xx、无响应和成功响应头之后的流中断为 `error`。
- 自动刷新失败比最近一次调用更新时显示 `warn`。

## 3. 导入预检与显式更新

### 3.1 `POST /api/chatgpt/accounts/import/preview`

拟采用严格 JSON 外层封装源数据，统一覆盖文本、JSON 文件和 ZIP 文件。封装设计不改变旧原始导入接口。

```json
{
  "source": {
    "mediaType": "application/json",
    "encoding": "utf8",
    "content": "<本次待导入 JSON 原文>"
  },
  "actions": []
}
```

JSON 源使用 UTF-8 字符串，ZIP 源使用匹配媒体类型及 base64 字节封装。实际解码后的限制复用旧导入限制；外层编码开销单独限制。禁止 URL 引用或任意服务器文件路径。

未指定动作时，新账户建议新增，重复账户建议跳过。逐项动作拟为：

```json
{
  "index": 1,
  "action": "update",
  "targetAccountId": "acc_example"
}
```

- `index` 指解析后稳定条目序号，不是页面排序下标。
- `action` 可选 `add`、`skip`、`update`，并按目标存在性校验。
- 更新必须显式指定本地目标且上游身份一致，不凭邮箱匹配。
- 无效条目完整报告，但默认阻止整批应用，不自动采用部分导入。
- 重复目标或冲突声明阻止应用；预检不调用上游，不落盘。

安全响应拟包括 `canApply`、统计、逐条元信息、建议动作、错误、受影响提供商、基础版本及不含原文的 `previewToken`。不能返回已解析的凭据结构或完整源 JSON。

### 3.2 `POST /api/chatgpt/accounts/import/apply`

请求携带与预检相同的 `source`、`actions` 和 `previewToken`，并使用账户 `If-Match`。输入或动作改变必须重新预检。

后端检查令牌和所有相关版本，协调目标刷新与更新，一次保存全部账户。没有实际改变时返回无需应用，不推进版本。

成功统计拟增加 `updated`，同时保留 `imported`、`skippedDuplicates`、`warnings` 和安全账户视图。旧原始导入接口返回形状不受影响。

更新保证保留本地 ID、首次授权时间、标签备注、暂停状态和提供商绑定。不能静默保留缺失 refresh token 再拼接为另一组凭据；字段保留细则必须在阶段确认中写清。

拟议典型错误（已实现的错误码见 3.3）：

| HTTP | 拟议 error.type | 含义 |
|---|---|---|
| 400 | `invalid_import` | 格式、字段或身份关系无效 |
| 409 | `account_conflict` | 账户控制版本或目标凭据版本已变化 |
| 409 | `config_conflict` | 预检涉及的绑定配置已变化 |
| 409 | `preview_mismatch` | 输入或选择与有效计划不一致 |
| 410 | `preview_expired` | 计划已过期，需要重新预检 |
| 413 | `request_too_large` | 编码、解码或账户数量超限 |
| 428 | `precondition_required` | 缺少精确账户版本 |
| 500 | `account_import_failed` | 本地批次保存失败，按事务结果明确报告 |

### 3.3 第二阶段已实现的导入契约

两个入口共用同一请求形状，严格 JSON 解码，拒绝未知字段、重复键、`null` 和尾随值。只接受可信本机同源 Origin，响应带 `Cache-Control: no-store`。

```json
{
  "source": { "mediaType": "application/json", "encoding": "utf8", "content": "<假数据>" },
  "actions": [{ "index": 1, "action": "update", "targetAccountId": "acc_example" }],
  "previewToken": "<仅 apply 携带>"
}
```

- `source` 只支持 JSON 原文加 `utf8`，或 ZIP 字节加 `base64`。
- 外层请求上限 24 MiB，解码后的源上限 16 MiB，与旧导入一致。
- `actions` 省略的条目按建议动作处理：新账户新增，已有账户跳过。无效条目必须显式选择 `skip` 才能应用。
- 预检返回计划、`baseVersion`、ETag 和 `previewToken`。没有可应用变更时 `previewToken` 与 `expiresAt` 为 `null`。
- 预检凭证 10 分钟有效，用进程签名密钥派生的导入专用 HMAC 绑定账户版本、配置版本、输入摘要和计划摘要，不含源内容。
- 应用必须携带精确强 `If-Match`，并在账户写锁内重建计划，与凭证中的摘要比对后一次提交。成功返回新的账户 ETag 和 `updated` 计数。

| HTTP | error.type | 含义 |
|---|---|---|
| 400 | `invalid_import` | JSON、源编码、动作或身份关系无效 |
| 400 | `invalid_preview_token` | 凭证格式或签名无效 |
| 409 | `account_conflict` | `If-Match` 或账户、目标凭据世代在预检后变化 |
| 409 | `config_conflict` | 绑定配置版本在预检后变化 |
| 409 | `preview_mismatch` | 源内容或逐项动作与凭证不一致 |
| 410 | `preview_expired` | 凭证已过期 |
| 413 | `request_too_large` | 外层请求、解码后源或账户数量超限 |
| 415 | `unsupported_media_type` | 请求不是 `application/json` |
| 428 | `precondition_required` | `apply` 缺少精确强 `If-Match` |
| 500 | `account_import_failed` | 写盘失败，整批回滚 |

## 4. 诊断

### `POST /api/chatgpt/accounts/{id}/diagnose`

请求拟区分 `local`、`refresh`、`usage`、`inference` 步骤。默认仅本地检查；会出网或轮换令牌的步骤由调用方明确选择。

```json
{
  "checks": ["local", "usage"],
  "forceRefresh": false
}
```

实际模型调用另需明确 `provider`、`model` 和消费确认字段。提供商须为该账户绑定的已保存 OAuth 提供商，不能接受任意 baseUrl、代理 URL 或客户端传入的令牌。

响应拟包含每步状态、时间、脱敏原因、代理来源类别、是否触发令牌交换和是否发生实际调用。步骤状态采用成功、失败、跳过或取消，不用一个总“健康”掩盖部分失败。

诊断请求受短总超时和响应大小限制。确切预算在第二阶段细化；不自动遍历所有账户/模型，不把管理诊断混入用户调用统计。

### 4.1 第二阶段已实现的诊断契约

上文草案中的布尔 `forceRefresh` 未采用：网关严格 JSON 解码器不支持布尔字段，且枚举字符串能拒绝含义不清的组合。实际请求：

```json
{
  "checks": ["local", "refresh", "usage", "inference"],
  "refreshMode": "auto",
  "provider": "example-provider",
  "model": "example-model",
  "confirm": "consume_quota"
}
```

- `checks` 可省略，取值 `local`、`refresh`、`usage`、`inference`，不能重复。`local` 总会执行。
- `refreshMode` 只在选择 `refresh` 时允许，取值 `auto` 或 `force`，默认 `auto`。
- `provider`、`model`、`confirm` 只在选择 `inference` 时允许，且必须全部提供。`confirm` 固定为 `consume_quota`。
- 模型名去掉首尾空白后不超过 200 字节，不能含空白或控制字符。
- Provider 必须是已保存配置中绑定此账户的 `chatgptOAuth` Provider。请求不能携带 baseUrl、代理或令牌。
- 请求体上限 4 KiB。同一账户同时只允许一个诊断，重复请求返回 409 `diagnose_in_progress`。

步骤按 `local → refresh → usage → inference` 顺序执行，每步返回 `success`、`failure`、`skipped` 或 `cancelled`：

- 本地检查显示需要重新授权时，出网步骤全部跳过。刷新链路失败时，后续步骤跳过。
- 用量查询失败不阻断实际调用。用量服务返回 401/403 记为 `query_failed`，不据此判定刷新凭据失效。
- 实际调用经该 Provider 的代理、队列和请求期刷新路径发出，最大输出 256 token。Provider 已停用时跳过，不绕过启停设置。
- 实际调用会写入账户的最近调用记录，但不写请求日志与指标，不上报熔断器，不打印上游正文，也不参与上游抓包。

失败类别：`authorization`、`quota`、`model`、`upstream`、`network`、`timeout`、`gateway`、`query_failed`、`no_refresh_token`、`conflict`、`storage`、`invalid_response`、`not_found`、`unknown`。`proxySource` 取 `oauth_proxy`、`provider_proxy`、`environment` 或 `direct`，不返回代理地址。

总预算 45 秒。预算耗尽或客户端断开按取消处理：已开始的步骤标记 `cancelled`，未开始的步骤不执行，不把取消记成用量查询失败、刷新失败或调用失败。客户端断开时不写响应。已经完成的令牌交换和已发出的模型调用不会回滚。

响应：

```json
{
  "data": {
    "accountId": "acc_example",
    "durationMs": 1200,
    "budgetMs": 45000,
    "tokenExchanged": false,
    "actualCall": true,
    "steps": [{ "name": "local", "status": "success", "message": "访问令牌有效，到期前会在使用时自动刷新", "durationMs": 0 }],
    "account": { "id": "acc_example", "status": "ready" }
  }
}
```

`account` 是诊断结束后的安全账户视图，供前端更新该行。错误码：400 `invalid_account_id` / `invalid_diagnose_request` / `invalid_diagnose_target`，403 跨站 Origin，404 `account_not_found`，405，409 `diagnose_in_progress`，413 `request_too_large`，415。

## 5. 暂停与批量操作

第三阶段冻结契约（2026-11-04 用户确认 D01 与 T15、T17 推荐值）。以下接口均只接受可信本机同源 Origin，严格 JSON 解码，响应带 `Cache-Control: no-store`、`Pragma: no-cache` 和 `X-Content-Type-Options: nosniff`。

### 5.1 `POST /api/chatgpt/accounts/{id}/state`

```json
{ "state": "paused" }
```

- `state` 取 `active` 或 `paused`。严格解码器不支持布尔，故不用 `enabled: false`。
- 必须携带账户列表返回的精确强 `If-Match`；缺少返回 428 `precondition_required`，不一致返回 409 `account_conflict`。
- 目标状态与当前相同时返回 `changed: false`，不推进版本，不写盘。
- 成功响应：`data.account` 为安全账户视图，`data.changed`，`data.providers` 为引用该账户的 Provider 列表，每项含 `name`、`configEnabled`、`effective`（`active`、`account_paused`、`provider_disabled`）。响应头 `ETag` 为新的账户版本。
- 不修改 Provider `enabled` 或绑定；恢复不会启用原本停用的 Provider。
- 错误：400 `invalid_account_id` / `invalid_account_state`，403，404 `account_not_found`，405，409 `account_conflict`，413，415，428，500 `account_state_update_failed`（写盘失败，内存回滚）。

账户视图新增 `enabled`（总是输出；旧数据缺失时为 `true`）和 `pausedAt`（暂停时间，恢复后清空）。账户文件新增可省略的 `paused` 与 `pausedAt`；旧文件缺失时按启用兼容。第三方导出不包含这两个字段，导入新增账户默认启用，显式凭据更新和重新授权保留暂停状态。

### 5.2 请求准入（D01）

- 候选循环在 Provider 停用检查之后检查账户暂停；暂停的候选记 `AttemptDetail{kind:"account_paused", outcome:"skipped", reason:"account_paused"}`，不消耗 `maxAttempts`，不调用 `breaker.Allow` 或 `breaker.Report`。
- 已通过前置检查的尝试在取得令牌后、获得队列槽后各复查一次。排队中被暂停的等待者立即被唤醒并按同一跳过处理，不占队列槽，不计真实尝试。
- 已开始发送的上游请求继续完成，暂停不截断响应。
- 一个真实请求都未发出时，全部候选因账户暂停跳过返回 503 `all_candidates_paused`；同时存在 Provider 停用与账户暂停跳过时返回 503 `all_candidates_disabled`，消息说明两类原因。两者都不带 `retry-after`，优先级与原有停用档相同，高于无密钥、熔断、限流、上下文和转换错误。
- 诊断的实际调用遵守暂停，跳过并说明；本地检查、刷新链路和用量查询属于显式管理操作，暂停时仍可执行。
- OAuth Provider 健康在账户暂停时显示 `unchecked`，消息说明账户已暂停。

### 5.3 `POST /api/chatgpt/accounts/batch/preview` 与 `/batch/apply`

```json
{
  "operation": "tags",
  "accountIds": ["acc_example"],
  "addTags": ["工作"],
  "removeTags": ["旧标签"],
  "previewToken": "<仅 apply 携带>"
}
```

- `operation` 取 `tags`、`pause`、`resume`、`delete`。`accountIds` 1–100 个，必须唯一且全部存在；不存在的 ID 返回 404 `account_not_found` 并列出 ID。
- `addTags`、`removeTags` 只在 `tags` 中允许，至少一项非空；沿用单账户标签的 Unicode、长度和数量规则，大小写不敏感去重；同一标签不能同时添加和移除。添加后超过 20 个的账户标记阻断。
- `delete` 阻断被任何 Provider 引用的账户（包括停用的 Provider），不提供自动解绑。存在阻断项时整批不可应用。
- 预检返回 `operation`、`baseVersion`、`canApply`、`noChanges`、`summary{total,change,unchanged,blocked}`、逐项 `entries[{accountId,email,action,before,after,providers,errors}]`，以及 `previewToken`、`expiresAt`（不可应用或无变更时为 `null`）。
- 预检凭证 10 分钟有效，以批量专用 HMAC 密钥绑定账户版本、配置版本、规范化输入摘要和计划摘要。
- 应用必须携带精确强 `If-Match` 与凭证，与预检相同的输入；在配置事务锁和账户写锁内重建计划并比对后一次写盘。写盘失败整批回滚，返回 500 `account_batch_failed`。成功返回新 ETag、逐项结果和最新账户视图。
- 错误：400 `invalid_batch` / `invalid_preview_token`，404 `account_not_found`，409 `account_conflict` / `config_conflict` / `preview_mismatch`，410 `preview_expired`，413，415，428，500。

### 5.4 `POST /api/chatgpt/accounts/usage/batch-refresh`

```json
{ "accountIds": ["acc_example"], "refreshMode": "auto" }
```

- `accountIds` 1–50 个，唯一；`refreshMode` 取 `auto`（30 分钟缓存内复用）或 `force`，默认 `auto`。
- 并发 3，总等待 60 秒，不自动重试。总预算耗尽后未完成项标记 `cancelled`，不写入失败快照。
- 同一时间只允许一个批量刷新，重复请求返回 409 `batch_refresh_in_progress`。
- 不推进账户控制版本，暂停账户仍可手动刷新。
- 响应 `data.results[{accountId,status,category,message,durationMs}]`，`status` 取 `success`、`cached`、`failure`、`cancelled`、`not_found`；`data.accounts` 为所请求且仍存在账户的最新视图；`data.durationMs`、`data.budgetMs`。

## 6. 调用统计、日志与事件

### 6.1 `GET /api/chatgpt/accounts/{id}/metrics`

拟返回当前进程累计和配置窗口统计，明确时间、样本量及来源：

- 用户真实调用次数与成功/失败次数。
- 真实上游尝试次数，含本地重试和转移。
- 可解释的尝试延迟分位数、限流次数、最后成功时间。
- 诊断真实调用单独列出，不混入用户调用成功率。
- 授权刷新和用量查询只在管理状态中展示，不冒充模型调用。

没有样本时相应比率和延迟为未知或 null，不以 0 表示瞬时成功。统计不承诺进程重启之前的历史，不由日志条数推算订阅余额。

### 6.2 现有日志与指标的增量

真实尝试拟增加可选 `accountId` 和来源维度，取尝试发出时的账户快照。仅添加安全身份，不能把 access token 指纹当成稳定账户 ID。

若增加账户筛选，按真实尝试身份匹配，而非当前提供商绑定。现有顶层 Provider、状态、次数和请求日志语义保持不变。

### 6.3 `GET /api/chatgpt/accounts/{id}/events`

拟从现有有界运行事件流筛选账户相关事件。事件增加可选账户身份，保留提供商维度；不新增无限磁盘历史。

状态事件含种类、时间、安全说明和是否有证据恢复。重复轮询不重复生成同类异常；旧请求迟到成功不触发新凭据的恢复。

### 6.4 通知配置

页面内提醒先以已有安全事件实现。浏览器权限、外部渠道及配置落点由 D02 决定，当前不承诺具体 webhook、邮件或第三方服务接口。

外部通知仅在明确配置并确认启用后发送；默认不发送。通知不得携带凭据、提示词或上游正文，失败不阻断模型请求。

### 6.5 第四阶段冻结契约

`GET /api/chatgpt/accounts/{id}/metrics`：账户不存在返回 404 `account_not_found`。

```json
{
  "data": {
    "accountId": "acc_x",
    "windowMinutes": 15,
    "since": "2026-11-04T08:00:00Z",
    "window": { "requests": 12, "successes": 10, "failures": 2, "localRetries": 1, "rateLimited": 1,
                "successRate": 0.833, "latencySamples": 11, "p50LatencyMs": 500, "p95LatencyMs": 2000,
                "skips": { "accountPaused": 0, "authorization": 0, "queue": 0 },
                "diagnostics": { "successes": 1, "failures": 0 },
                "refresh": { "successes": 1, "failures": 0 }, "usage": { "successes": 2, "failures": 0 } },
    "total": { "...": "与 window 相同字段，覆盖当前进程" },
    "lastSuccessAt": "2026-11-04T08:10:00Z",
    "lastFailureAt": "2026-11-04T08:09:00Z",
    "events": []
  }
}
```

- `since` 是统计起点：进程内首次使用账户管理器的时间。
- `successRate` 在 `requests` 为 0 时为 null；`p50LatencyMs`、`p95LatencyMs` 在 `latencySamples` 为 0 时为 null。延迟值是固定直方图桶上界。
- `lastSuccessAt`、`lastFailureAt` 只取真实调用，不含诊断调用，缺失时省略。
- `events` 是该账户最近的运行事件，按时间倒序，最多 10 条。

`GET /api/chatgpt/accounts/events`：返回含账户维度的运行事件，按时间倒序，结构同 `/api/metrics` 的 `events`。

运行事件新增可选字段 `accountIds`、`account`；`recovered` 对 `*_recovered` 事件为 true。请求日志的尝试明细新增可选 `accountId`，只在 OAuth 候选上出现。

账户列表与单账户视图的 `invocation` 新增可选 `lastSuccessAt`：来自账户统计的最后一次真实调用成功时间，不含诊断调用；最近一次失败或重新授权都不清空，进程重启后回到缺失。Provider 维度的调用记录不带此字段。页面「最近调用成功时间」排序以它为准，缺失视为未知并置后。

## 7. 账户准入、策略与后台配置

以下是所需能力，不是已冻结的 YAML 字段。具体命名、落点、默认和边界由对应阶段确认项决定。

| 配置能力 | 必须定义的契约 | 确认项 |
|---|---|---|
| 账户并发、速率和等待 | 同账户共享、未配置与零值、与提供商约束的关系 | D03 |
| 直通模式账户限制 | 是否跳过本地限制以及冷却独立性 | D04 |
| 额度排序 | 显式开关或新策略、适用窗口、数据过期及粘性优先级 | D05 |
| 存储写入所有权 | 跨进程、单写者或隔离存储，以及非写者行为 | D06 |
| 后台令牌和用量 | 默认开关、活跃范围、周期、并发、退避和取消 | D07 |

账户列表安全视图拟显示准入原因、冷却截止和下次后台计划。新终态和 skip reason 在第五阶段统一对照既有优先级表冻结，不单独拍定与现行冲突的响应码。

不新增隐式“切换到任何账户”的 API。候选仍来自明确路由，排序不发网络请求、不扩展尝试额度、提交响应后不切换。

### 7.1 第五阶段冻结契约

配置（语义见 DESIGN 6.4）：

```yaml
chatgptAccountLimits:
  maxConcurrent: 2      # 可省略：不限制；显式 1–100
  maxPerSecond: 0       # 0–100，0 表示不限制
  sharedCooldown: false # 显式开启账户级额度冷却
routes:
  - match: "gpt-*"
    strategy: round-robin
    quotaAware: true    # 只对多候选路由有效
    targets: [...]
```

`DELETE /api/chatgpt/accounts/{id}/cooldown`：手动解除账户冷却，不需要 If-Match。

- 200：`{ "data": { "account": AccountView, "changed": bool } }`；没有冷却时 `changed` 为 false。
- 404 `account_not_found`；400 `invalid_account_id`；跨站请求 403；其他方法 405。

账户视图新增可选 `cooldownUntil`（RFC3339），只在冷却未到期时出现。

`GET /api/chatgpt/accounts/{id}/metrics` 新增：

```json
"admission": {
  "limited": true,
  "running": 1, "queued": 2,
  "maxConcurrent": 2, "maxPerSecond": 0,
  "sharedCooldown": true,
  "cooldownUntil": "2026-11-04T09:00:00Z"
}
```

`limited` 为 false 时 `running`、`queued` 为 0，上限字段省略。`window.skips` 与 `total.skips` 新增 `cooldown`。

候选跳过与终态：尝试明细 `kind`、`reason` 为 `account_cooldown`，`retryAfterMs` 为剩余冷却。全部候选未发出请求且存在冷却跳过时返回 429 `account_usage_limited`，带 `retry-after`。运行事件新增 `account_cooldown`、`account_cooldown_cleared`。

### 7.2 第六阶段冻结契约

配置（语义见 DESIGN 7.1）：

```yaml
chatgptBackground:
  tokenRefresh: false        # 活跃账户到期前 10 分钟预刷新
  usageRefresh: false        # 活跃账户按周期刷新用量
  usageIntervalMinutes: 30   # 30–1440
  activeWindowMinutes: 60    # 5–1440
```

账户视图新增可选 `background`，只在任一后台功能开启时出现：

```json
"background": {
  "active": true,
  "lastUsedAt": "2026-11-05T08:00:00Z",
  "skipped": "",
  "token": { "lastRunAt": "...", "lastResult": "ok", "lastMessage": "...", "nextAt": "...", "failures": 0 },
  "usage": { "lastRunAt": "...", "lastResult": "error", "lastMessage": "...", "nextAt": "...", "failures": 2 }
}
```

`skipped` 取值：`inactive`、`paused`、`reauthorization_required`、`no_refresh_token`，为空表示参与调度。对应功能关闭时省略 `token` 或 `usage`。

`GET /api/chatgpt/background`：200 `{ "data": { "tokenRefresh": bool, "usageRefresh": bool, "running": bool, "crossProcessLocking": bool, "reason": "", "lastTickAt": "..." } }`。`reason` 为 `unsupported_platform` 表示平台不支持跨进程锁而未启动。其他方法 405。

写入入口等不到跨进程存储锁时返回各自原有的保存失败错误，不写入任何内容。

## 8. 原生加密备份与恢复

### 8.1 文件容器

拟采用版本化加密容器，不沿用第三方导出名冒充兼容格式。内容大类：

- 可识别的类型和版本。
- 经批准的认证加密及密码派生算法标识。
- 有资源上下界的派生参数、随机盐和 nonce。
- 已认证密文。

具体格式、扩展名、算法及参数由 D08 冻结。未知版本、算法或过大派生参数必须在进行昂贵运算前拒绝。

解密后的载荷包含所选账户凭据、本地元数据和真实已有时间字段。不包含用量缓存、运行健康、冷却、统计或 OAuth session。是否带安全绑定描述及恢复范围由 D09 决定。

### 8.2 `POST /api/chatgpt/accounts/backup/export`

拟请求包含明确账户 ID 集合和仅本次使用的密码；要求可信本机同源、账户版本及显式操作。

响应是无缓存加密附件，不返回可读账户 JSON，不自动刷新或重授权。不保存密码，不把密码写 URL 或日志。

### 8.3 `POST /api/chatgpt/accounts/backup/preview`

拟请求包含加密源、密码和恢复选择。只接受内存上传，不支持任意路径或远程 URL。

返回安全身份、拟新增/更新项、元数据覆盖范围、绑定冲突及是否可应用，不返回解密原文。密码错误或认证失败使用统一安全错误。

### 8.4 `POST /api/chatgpt/accounts/backup/apply`

携带同一加密源、密码、选择和有效预检凭证，重新解密校验并提交。禁止仅凭前端解密的明文结构绕过认证容器。

推荐先原子恢复账户，再返回安全源 ID 至目标 ID 映射，供现有配置迁移使用。若用户选择连带修改配置，需先补充跨文件事务契约；当前草案不承诺原子一键恢复。

原生备份不会改变现有第三方导出接口。敏感边界参考 [已接受的本机敏感传输决策](../adr/0001-local-sensitive-transfer-boundary.md)。

### 8.5 第七阶段冻结契约

语义见 DESIGN 8.4。三个端点都只接受 POST，媒体类型为 `application/json`，并且必须带本机同源 Origin（复用 `configTransferOriginAllowed`），否则返回 403。响应都带 `Cache-Control: no-store`、`Pragma: no-cache` 和 `X-Content-Type-Options: nosniff`。请求 JSON 拒绝未知字段、重复键和尾随内容。

**容器文件。** 扩展名为 `.aigw-backup`，内容是单个 JSON 对象：

```json
{
  "format": "ai-gateway-chatgpt-backup",
  "version": 1,
  "kdf": { "name": "pbkdf2-hmac-sha256", "iterations": 600000, "salt": "<base64 16B>" },
  "cipher": { "name": "aes-256-gcm", "nonce": "<base64 12B>" },
  "ciphertext": "<base64>"
}
```

附加认证数据是以下字段用 `\x00` 拼接的字节串：`ai-gateway-chatgpt-backup`、`v1`、kdf 名称、迭代次数的十进制、盐的 base64、cipher 名称、nonce 的 base64。

解密后的明文：

```json
{
  "version": 1,
  "createdAt": "RFC3339",
  "accounts": [{
    "id": "acc_…", "chatgptAccountId": "…", "email": "", "plan": "",
    "idToken": "", "accessToken": "", "refreshToken": "",
    "expiresAt": "", "lastRefresh": "", "authorizedAt": "",
    "subscriptionActiveStart": "", "subscriptionActiveUntil": "",
    "tags": [], "note": "", "needsReauth": false, "paused": false, "pausedAt": ""
  }]
}
```

明文最多 100 个账户，并拒绝未知字段。

**`POST /api/chatgpt/accounts/backup/export`**

- 请求：`{ "accountIds": ["acc_…"], "password": "…" }`。必须带 `If-Match: "<账户版本>"`，缺失返回 428，不一致返回 409 `account_conflict`。
- 请求体最大 64 KiB。`accountIds` 为 1–100 个、不重复，且都必须存在。
- 200：响应体是容器文件，`Content-Type: application/octet-stream`，`Content-Disposition: attachment; filename="chatgpt-accounts-<UTC yyyymmdd-hhmmss>.aigw-backup"`。

**`POST /api/chatgpt/accounts/backup/preview`**

- 请求：`{ "backup": "<容器 JSON 文本>", "password": "…", "metadata": "keep"|"replace", "restorePaused": false, "actions": [{ "index": 1, "action": "add"|"update"|"skip", "targetAccountId": "" }] }`。
- `metadata` 默认为 `keep`。`actions` 可选，省略的条目采用建议动作。请求体最大 24 MiB，`backup` 最大 16 MiB。
- 200：`{ "data": { …RestorePlan, "previewToken": "…"|null, "expiresAt": "…"|null } }`，并带 `ETag: "<账户版本>"`。只有 `canApply` 为 true 时才签发 `previewToken`，有效期 10 分钟。

RestorePlan 结构：

```json
{
  "baseVersion": "…", "createdAt": "…", "metadata": "keep", "restorePaused": false,
  "canApply": true, "noChanges": false,
  "summary": { "total": 0, "add": 0, "update": 0, "skip": 0, "invalid": 0, "blocked": 0 },
  "entries": [{
    "index": 1, "sourceAccountId": "acc_…", "email": "", "plan": "",
    "expiresAt": "", "lastRefresh": "", "hasRefreshToken": true,
    "tags": [], "hasNote": false, "paused": false,
    "status": "new|existing|invalid", "duplicateOf": 0,
    "matchedAccountId": "", "matchedEmail": "", "affectedProviders": [],
    "suggestedAction": "", "action": "", "allowedActions": [],
    "targetAccountId": "", "remapped": false,
    "metadataChanges": false, "pausedChange": false,
    "invalidReason": "", "errors": [], "warnings": []
  }],
  "blockers": []
}
```

新增条目的 `targetAccountId` 只在保留源 ID 时有值。需要重映射时 `remapped` 为 true，`targetAccountId` 为空，新 ID 在应用时生成。

**`POST /api/chatgpt/accounts/backup/apply`**

- 请求体与预检相同，另加 `previewToken`。必须带 `If-Match`。
- 应用时重新解密并在写锁内重建计划，摘要与预检一致才提交。
- 200：`{ "data": { "imported": 0, "updated": 0, "skipped": 0, "accounts": [AccountView], "mappings": [{ "sourceAccountId": "", "targetAccountId": "", "action": "add|update|skip", "remapped": false }] } }`，并带新的 `ETag`。

**预检凭证。** 签名密钥由 `configTransferSigningKey` 派生，域分隔为 `chatgpt-account-backup\x00v1`。凭证绑定以下内容，不绑定密码，因为应用时会重新解密：

- 账户版本与配置 revision。
- 输入摘要，即容器原字节、`metadata`、`restorePaused` 和排序后的 `actions` 的 HMAC。
- 计划指纹的 HMAC。

**错误类型。**

| HTTP | 类型 | 含义 |
|---|---|---|
| 400 | `invalid_backup` | 容器结构或明文无效 |
| 400 | `unsupported_backup` | 格式、版本、算法或参数不受支持 |
| 400 | `backup_decrypt_failed` | 固定文案「密码错误或备份已损坏」 |
| 400 | `invalid_backup_request` | 请求字段无效，或密码不符合要求 |
| 400 | `invalid_preview_token` | 预检凭证无效 |
| 409 | `preview_mismatch` | 内容或选择与预检不一致 |
| 409 | `config_conflict` | 配置 revision 已变化 |
| 409 | `account_conflict` | 账户版本或计划已变化 |
| 410 | `preview_expired` | 预检已过期 |
| 413 | `request_too_large` | 请求或容器超过大小上限 |
| 428 | `precondition_required` | 缺少 If-Match |
| 500 | `account_restore_failed` | 落盘失败，整批未保留 |

## 9. 接口冻结和验收

- 每阶段开始前把相关提案及仍未决定参数更新为已确认契约；只实施已冻结范围。
- 每个端点覆盖 Host、Origin、方法、媒体类型、大小、未知字段、重复键、取消、版本冲突和脱敏。
- 所有示例和自动测试使用假账户与假上游，不把计划中的接口当成可以直接调用的现行 API。
- 新增凭据更新、后台刷新、通知和恢复不得通过隐式默认产生真实外发或破坏现有存储。
- 真实上游能力或新增字段需在相关阶段可靠核验后再依赖，本轮没有进行这类确认。
