# OAuth 模型与提供商余额接口草案

创建日期：2026-11-07。
状态：已实现并通过本地假数据验收；验收记录见 TASKS.md，尚未提交发布。
设计与公开证据：[DESIGN.md](DESIGN.md)。任务：[TASKS.md](TASKS.md)。

## 1. 配置草案

所有字段使用 lower camelCase。未配置块必须整体省略，不在普通保存时扩散默认值。

### 1.1 Provider.balance

| 字段 | 类型 | 语义与校验 |
|---|---|---|
| type | string | 必填，`newapi` / `sub2api`；块缺失表示不查询 |
| source | string | `auto` / `account` / `apiKey`，省略为 auto |
| baseUrl | string | 可选管理站点基址；默认 Provider URL 去掉末尾 `/v1`；只允许 http/https，无 userinfo/query/fragment |
| accessToken | string | 独立账户查询令牌，仅 account 或 auto 使用；不接受控制字符，最多 16 KiB |
| userId | string | 可选 New API 旧认证用户 ID，正整数十进制字符串；上限为有符号 64 位整数 |
| apiKeyId | string | 列表模式使用 API Key 查询时必填，必须命中 Provider.apiKeys 的 ID；单值模式不填 |
| quotaPerUnit | string | 仅 New API；可选正十进制数，最多 64 字节、最多 18 位小数、数值不大于 10^18；表示每 1 USD 的原始配额，未配由 `/api/status` 发现 |
| refreshIntervalMinutes | *int | 省略/0 表示手动；开启时 5–1440 分钟 |

规则：

- `account` 必须配置独立 accessToken；失败不回落到模型密钥。account 或 auto+token 模式拒绝未使用的 apiKeyId。
- quotaPerUnit 使用纯十进制字符串：整数部分必填，小数点后须有数字，允许前导零，不接受正号、指数、`.5` 或 `1.`。
- `auto` 有独立 accessToken 时只走账户接口，否则使用已保存的模型密钥查询适配器额度接口。
- API Key 路径只在响应中确实有账户额度字段时展示账户项，否则明确展示密钥/订阅口径。
- 显式 `apiKey` 与独立 accessToken 互斥，避免隐藏未使用的秘密。列表模式不自动切换查询密钥。
- 不从客户端头回退查询密钥。缺密钥、停用或冷却时返回安全状态，不改变 Provider 运行状态。
- API Key 查询的管理 baseUrl 必须与 Provider 同 origin；独立令牌模式可显式配置不同站点，不复用调用密钥。
- 直接 ChatGPT OAuth Provider 不使用这个余额块，已有 OAuth 用量功能保持独立。

示例只使用假值，不能直接拿来测试真实账户：

```yaml
providers:
  newapi-demo:
    baseUrl: https://newapi.example.invalid/v1
    format: openai
    apiKey: fake-model-key
    balance:
      type: newapi
      source: account
      accessToken: fake-query-token
      userId: "123"
      refreshIntervalMinutes: 0
  sub2api-demo:
    baseUrl: https://sub2api.example.invalid/v1
    format: anthropic
    apiKey: fake-model-key
    balance:
      type: sub2api
      source: auto
      refreshIntervalMinutes: 0
```

### 1.2 Provider.modelCatalog

只允许 ChatGPT OAuth Provider 配置：

| 字段 | 类型 | 语义 |
|---|---|---|
| clientVersion | string | 目录兼容参数；默认 `0.167.0`，格式为非负数字的 major.minor.patch；每段最多 6 位，最多 32 字节 |

不允许自定义 OAuth 目录 URL，不放宽现有官方 BaseURL 限制，不把查询版本当作转发 User-Agent。

### 1.3 秘密与普通配置

- 普通 JSON/YAML 输出把非空 accessToken 替换为 `__AI_GATEWAY_KEEP_BALANCE_TOKEN__`。
- JSON 读视图可增 `balance.accessTokenConfigured`，保存载荷不回传该只读字段。
- 普通保存留空保留需前端明确发占位符；清除则发空字符串；余额功能整体关闭删除 balance 块及其秘密。
- 占位符不能用于新 Provider 或已改变查询目标的配置。错误只指出字段，不输出令牌内容。
- 已有敏感提供商/路由导出包含实际查询令牌，需扩展导出风险说明和预览的秘密存在标识。普通预览仍不返回原值。

## 2. 模型接口

### 2.1 GET /api/providers/models?provider=<name>

保留已有 method、Provider 参数和 `models: string[]`，新增 OAuth 处理与可选查询元信息：

```json
{
  "provider": "codex-demo",
  "models": ["fake-model-a", "fake-model-b"],
  "source": "chatgpt-codex",
  "queriedAt": "2030-01-01T00:00:00Z",
  "cached": false,
  "stale": false,
  "message": "目录来自绑定账户，不代表已完成模型调用验证"
}
```

- OAuth 从远端 `models[].slug` 解析，默认仅 `visibility: list`，不按 supported_in_api 排除。
- 账户 ID、令牌和原始目录均不返回。
- 首次焦点触发查询视为明确配置期交互；不在页面载入时遍历全部 OAuth Provider。
- 有效缓存直接返回。刷新失败且仍有同身份旧目录时可返回旧值并注明 stale 与固定错误；无旧值返回错误，手填仍可用。失败至少等待 30 秒，429 遵守更长 Retry-After；失败状态保留，不因再次 GET 被重标成新鲜成功。
- 保持现有普通 Provider 的 API Key 查询和多密钥切换语义；新增余额密钥规则不作用于模型查询。

### 2.2 POST /api/providers/models/refresh

由模型组合框的明确刷新按钮调用，丢弃浏览器结果并触发服务端受控刷新。复用当前管理面同源边界。

```json
{"provider":"codex-demo","revision":"当前已保存配置的版本"}
```

- 严格 application/json，请求最多 64 KiB，未知字段、重复键、null、尾随值均拒绝。
- 必须为本机同源 Origin。只查询已保存配置；revision 不匹配返回 409，不查询表单草稿。
- 响应与 GET 兼容，正常刷新为 cached=false；去重或限频复用时说明实际行为。
- OAuth 可取消阶段预算 30 秒含必要取令牌/交换，目录请求最多 10 秒，响应上限 8 MiB；普通模型查询保持 10 秒共享预算。
- 已发生轮换后的安全保存沿用账户管理器独立的存储等待，可能额外等待最多 30 秒，不承诺整个 HTTP 操作严格在可取消预算内返回。取消或超时后不继续查目录，也不丢掉已换得的新凭据。
- OAuth 401/403 不直接标记刷新凭据失效，也不循环强制刷新；沿用正常取令牌规则并如实报告目录拒绝。

模型失败类别包括：不存在的 Provider、账户未绑定/不存在、OAuth 管理器不可用、需重新授权、目录不支持、鉴权被拒、超时、无效响应、响应超限、配置/账户变化和限频。普通 Provider 既有错误类型保持兼容，OAuth 新错误均使用固定文案。

## 3. 余额接口

### 3.1 GET /api/providers/balances

只读当前进程的缓存，不出网、不刷新令牌。可用 `?provider=<name>` 查询单项，不带参数返回当前已保存 Provider 的安全状态。

```json
{
  "revision": "当前配置版本",
  "providers": {
    "newapi-demo": {
      "type": "newapi",
      "source": "account",
      "state": "ok",
      "freshness": "fresh",
      "refreshing": false,
      "lastAttemptAt": "2030-01-01T00:00:00Z",
      "lastSuccessAt": "2030-01-01T00:00:00Z",
      "nextRefreshAt": null,
      "retryAfterSeconds": null,
      "error": null,
      "snapshot": {
        "queriedAt": "2030-01-01T00:00:00Z",
        "items": [
          {
            "kind": "account_balance",
            "source": "newapi.user_self",
            "remaining": "12.500000",
            "used": "2.000000",
            "limit": null,
            "unit": "USD",
            "unlimited": false,
            "keyId": null,
            "windows": []
          }
        ],
        "warnings": []
      }
    }
  }
}
```

安全状态：

| 字段 | 语义 |
|---|---|
| state | `not_configured` / `not_queried` / `ok` / `failed` / `unsupported` / `credentials_required` |
| freshness | `none` / `fresh` / `stale`；与最近一次查询结果分开 |
| snapshot | 最近同身份成功快照；没有成功数据为 null，不能构造零余额 |
| error | null 或固定安全 `{code,message,httpStatus}`，不保留上游原文 |
| nextRefreshAt | 自动刷新/退避的下次计划，不承诺该时刻余额必然变化 |
| sharedProviders | 同查询来源的其他 Provider 名称数组，排序且不含自身；这些行不可作为独立余额相加 |

`failed` 也可带旧 snapshot，lastSuccessAt 保持旧时间。refreshing 是独立状态，不将旧值改成 0。

额度项 kind：

- `account_balance`：上游钱包或明确授权的账户余额。
- `key_quota`：选定 API Key 的独立限额；无限额标记只作用于该密钥。
- `subscription_quota`：订阅额度及周期窗口，不是钱包余额。
- `usage_only`：只有安全用量，没有可确认剩余；remaining=null。

金额和配额输出十进制字符串或 null。真实零为字符串 `"0"`，不得因空值回退而丢失。New API 原始单位为 `quota`，换算确认后为 USD；详情保留 rawRemaining、rawUsed、rawUnit、quotaPerUnit 与 conversionSource，后者为 configuration 或 upstream_status。不混用单位，不按展示符号自行换算其他币种。

窗口项含 `name`、`limit`、`used`、`remaining`、`unit`、`resetAt` 和 `expiresAt`。缺失时间为 null，不从其他字段推算未知周期。

### 3.2 POST /api/providers/balances/refresh

同时支持逐行与明确的可见列表批量刷新：

```json
{
  "revision": "当前已保存配置的版本",
  "providers": ["newapi-demo", "sub2api-demo"]
}
```

- 仅 POST、严格 application/json、本机同源 Origin，请求最多 64 KiB。
- providers 必须为 1–50 个非空、不重复的已保存名称；结构/版本校验失败时整批不出网。
- 请求通过后各项独立执行，最多并发 2，每查询来源 15 秒总预算，整批 60 秒总预算。取消或批次预算耗尽后保留已完成结果，停止未开始的项；共享查询仍有其他等待者时仅移除本批订阅，最后一个等待者离开才取消出网。不谎称撤回已经发生的上游读取。
- 同来源去重；每来源 30 秒最短刷新间隔，有效 Retry-After 更长时以它为准。间隔内返回 cached/skipped 原因，不发第二次请求。
- 各项返回 `status: success|cached|failed|skipped|cancelled`，附安全余额视图；单项失败不把其他成功改成失败。
- HTTP 200 表示批次受理并返回结果，不代表全部上游成功。
- 批次处理中配置变化时，原来源结果不能写回新配置，相关项返回 configuration_changed。

```json
{
  "revision": "当前配置版本",
  "results": [
    {"provider":"newapi-demo","status":"success","balance":{}},
    {"provider":"sub2api-demo","status":"failed","balance":{}}
  ]
}
```

上例 balance 空对象仅表示结构位置，实际必须返回完整安全视图。

### 3.3 HTTP 边界与安全错误

全部新增查询响应带 `Cache-Control: no-store`、`Pragma: no-cache`、`X-Content-Type-Options: nosniff`。浏览器不直接请求上游，令牌仅在服务端 HTTP 头中使用。

| HTTP 状态 | 适用情形 |
|---|---|
| 400 | 请求形状、名称或查询配置不合法 |
| 403 | 本机同源边界拒绝 |
| 404 | 指定 Provider 不存在 |
| 405 | method 不符 |
| 409 | revision 已变化 |
| 413 | 管理请求超过上限 |
| 415 | 非 application/json |
| 503 | 查询服务不可用或停止中 |

余额项内部错误使用固定 code：`credentials_required`、`credential_rejected`、`permission_denied`、`unsupported_endpoint`、`upstream_timeout`、`upstream_unreachable`、`invalid_response`、`response_too_large`、`rate_limited`、`configuration_changed`、`query_key_unavailable`、`query_cancelled`。

New API HTTP 200 + success:false、Sub2API 非零 code、上游 401/403 等均识别失败。查询失败不修改密钥冷却或 Provider 健康，不透出令牌、URL userinfo、邮箱、完整个人资料或 HTML 登录页正文。

## 4. 上游协议映射

| 类型与来源 | 固定查询路径 | 认证 | 主要映射 |
|---|---|---|---|
| newapi / account | `/api/user/self` | 独立 Bearer，userId 配置了才附 New-Api-User | quota / used_quota → 账户配额 |
| newapi / apiKey | `/api/usage/token` | 已选 API Key Bearer | 密钥限额；有效 user_quota 另记账户项 |
| newapi / 单位发现 | `/api/status` | 不携带账户或模型凭据 | quota_per_unit；失败保留原始单位 |
| sub2api / account | `/api/v1/auth/me` | 独立 JWT Bearer | code=0，data.balance → 钱包余额 |
| sub2api / apiKey | `/v1/usage` | 已选 API Key Bearer | 钱包 / 密钥 / 订阅 / 仅用量的明确结构 |
| chatgptOAuth / models | `/backend-api/codex/models?client_version=...` | 绑定账户 Bearer + ChatGPT-Account-Id | models[].slug 与 visibility |

基址拼接仅剥除明确的末尾 `/v1`，保留反代子路径。跨域 redirect 不跟随。上游差异与证据见 DESIGN 2，尚未针对用户实际运营站点作真实兼容验证。
