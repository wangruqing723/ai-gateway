# Provider 多密钥：接口契约

行为与取舍见 [DESIGN.md](DESIGN.md)。所有管理接口沿用现有同源校验（`isCrossSiteRequest`）、统一 JSON 错误结构 `{"error":{"type","message"}}`，响应不含密钥明文。

## 1. 配置文件（YAML / `PUT /api/config` 请求体）

`providers.<name>` 新增字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `apiKeys` | 数组，≤10 | 与 `apiKey`、`authType: chatgptOAuth` 互斥；空数组等同不写 |
| `apiKeys[].id` | string | 必填，`^[A-Za-z0-9_-]{1,32}$`，Provider 内唯一 |
| `apiKeys[].name` | string | 选填，≤64 字符，不含控制字符，可重名 |
| `apiKeys[].value` | string | 必填，非空、不含控制字符；同 Provider 内不重复；可为 `__AI_GATEWAY_KEEP_API_KEY__` 表示保留已存值 |
| `apiKeys[].enabled` | bool | 选填，缺省启用 |
| `keyCooldown.authMs` / `quotaMs` / `rateLimitMs` | int | 选填，0-86400000；仅列表模式可写 |

校验错误示例（400 `config_validation_error`）：

```
providers.tabi 不能同时配置 apiKey 与 apiKeys
providers.tabi.apiKeys 最多 10 个（当前 11）
providers.tabi.apiKeys[1].id 重复: k7f3a9c2
providers.tabi.apiKeys[2].value 与 apiKeys[0] 重复
providers.tabi.keyCooldown 仅可与非空 apiKeys 一起使用
providers.tabi 使用 ChatGPT OAuth 时不能同时配置 apiKeys
providers.tabi.apiKeys[k1b8e4d0] 无法保留已有密钥：磁盘上没有该身份的已存密钥
```

## 2. `GET /api/config`

每个列表模式 Provider 的密钥视图：

```json
{
  "apiKeys": [
    {"id": "k7f3a9c2", "name": "主账号", "value": "__AI_GATEWAY_KEEP_API_KEY__",
     "valueConfigured": true, "fingerprint": "3F2A9C1B"},
    {"id": "k1b8e4d0", "value": "__AI_GATEWAY_KEEP_API_KEY__", "enabled": false,
     "valueConfigured": true, "fingerprint": "A0C4D2E9"}
  ],
  "keyCooldown": {"authMs": 300000}
}
```

`valueConfigured`、`fingerprint` 只读，提交 `PUT` 前前端必须剔除。单值写法保持现有 `apiKey` + `apiKeyConfigured`。`GET /api/config/raw` 中 `apiKeys[*].value` 替换为 sentinel，其他同名键不动。

## 3. `GET /health` 新增字段

```json
{
  "providerKeys": {
    "tabi": [
      {"id": "k7f3a9c2", "name": "主账号", "fingerprint": "3F2A9C1B",
       "enabled": true, "state": "cooling", "reason": "auth", "status": 401,
       "remainingMs": 241000, "until": "2026-10-31T12:04:00+08:00"},
      {"id": "k1b8e4d0", "fingerprint": "A0C4D2E9", "enabled": false, "state": "disabled"}
    ]
  }
}
```

- `state`：`disabled` | `selectable` | `cooling`。停用优先于冷却展示（两者可同时成立时显示 `disabled`，同时返回 `remainingMs` 供界面附注）。
- `reason`：`auth` | `quota` | `rateLimit`，仅冷却时出现。
- 只包含列表模式 Provider；单值写法不出现。

## 4. `POST /api/providers/keys/cooldown/reset`

查询参数：`provider`（必填）、`key`（必填，密钥身份）。

- 200：`{"provider":"tabi","key":"k7f3a9c2","reset":true,"keys":[...同 providerKeys 条目]}`；未在冷却时 `reset:false`。
- 400：缺参数。404：Provider 不存在、非列表模式或密钥身份不存在。
- 只清冷却，不改启用状态、不发探测；不影响熔断。

## 5. `POST /api/providers/probe` 请求体扩展

```json
{
  "name": "tabi", "baseUrl": "https://tabitoken.com", "format": "anthropic",
  "apiKeys": [
    {"id": "k7f3a9c2", "value": "__AI_GATEWAY_KEEP_API_KEY__"},
    {"id": "k1b8e4d0", "value": "sk-new", "enabled": false}
  ],
  "testKeyId": "k1b8e4d0",
  "userAgent": "", "proxy": ""
}
```

- `apiKey` 与 `apiKeys` 互斥；`testKeyId` 只能与 `apiKeys` 一起出现，且必须命中列表中的身份。
- 有 `testKeyId`：只测该密钥（停用的也测）。无 `testKeyId`：按顺序测已启用密钥，遇密钥相关失败换下一个，首次成功结束。
- 不读也不写密钥冷却与健康缓存。响应结构不变（`{"status": providerhealth.Status}`），`message` 中出现的所有密钥值都替换为 sentinel。

## 6. `GET /api/providers/models`

请求不变。列表模式按顺序用可选择密钥查询，遇密钥相关失败换下一个；全部停用或冷却时返回 503：

```json
{"error":{"type":"no_selectable_provider_key","message":"provider tabi 没有可选择的 Provider 密钥"}}
```

## 7. 推理请求终态

一个真实请求都没发出、且原因是全部候选无可选择密钥：503 `no_selectable_provider_key`；存在冷却中密钥时带 `retry-after`（秒，最小 1）。优先级见 DESIGN.md §5.6。

## 8. 请求日志与事件

`GET /api/logs` 中 `attemptDetails[]` 新增：

| 字段 | 说明 |
|---|---|
| `keyId` | 密钥身份，仅列表模式 |
| `keyName` | 密钥名称，仅列表模式且有名称时 |
| `keyFingerprint` | 指纹，列表模式与单值写法都填 |
| `kind: "key_skip"` | 候选因无可选择密钥被跳过 |
| `outcome: "key_switched"` | 本次请求失败后换了下一个密钥 |
| `reason` | 新增 `401/key_auth`、`403/key_auth`、`402/key_quota`、`429/key_rate_limit`、`no_selectable_key` |

`GET /api/metrics` 的 `events[]` 新增 `kind`：`key_cooldown`、`key_recovered`（后者 `recovered: true`）。
