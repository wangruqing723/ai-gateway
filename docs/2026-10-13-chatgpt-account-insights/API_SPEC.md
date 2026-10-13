# API 规格：ChatGPT 账号用量刷新

## 现有账号列表

### `GET /api/chatgpt/accounts`

保持现有 `{ "data": [...] }` 响应结构。账号对象增加以下安全字段；字段可选或为空：

```json
{
  "authorizedAt": "2026-10-13T08:00:00Z",
  "subscriptionActiveStart": "2026-10-01T00:00:00Z",
  "subscriptionActiveUntil": "2026-11-01T00:00:00Z",
  "usage": {
    "planType": "plus",
    "windows": [
      {
        "name": "primary",
        "limitWindowSeconds": 18000,
        "usedPercent": 32,
        "remainingPercent": 68,
        "resetAt": "2026-10-13T12:00:00Z"
      }
    ],
    "credits": {
      "balance": "12.50"
    },
    "lastCheckedAt": "2026-10-13T09:00:00Z",
    "error": ""
  }
}
```

上述数值仅为结构示例。实际响应只包含上游提供的窗口、字段和 credits；缺少信息时省略，不填造默认配额。`error` 仅是可展示的安全摘要。

既有 `expiresAt` 继续表示 access token 到期时间，不改变其语义。账号的 `accessToken`、`refreshToken` 及上游原始响应不得加入此 API。

## 刷新用量

### `POST /api/chatgpt/accounts/usage/refresh`

可选查询参数 `force=true` 跳过 5 分钟缓存并强制查询；省略或为 `false` 时复用仍有效的缓存。

刷新所有已连接 ChatGPT 账号的 usage 快照，返回与账号列表相同的安全视图：

```json
{
  "data": [
    {
      "id": "account-id",
      "status": "active",
      "usage": {
        "windows": [],
        "lastCheckedAt": "2026-10-13T09:00:00Z",
        "error": ""
      }
    }
  ]
}
```

逐账号执行并发上限为 3 的上游查询。单个账号失败时，将安全错误摘要写入该账号的 `usage.error` 并继续处理其他账号；批量接口仍返回 HTTP 200 和完整账号列表。若刷新请求本身无法执行（例如服务正在关闭），使用既有 API 错误格式返回非 2xx。

列表 API 读取缓存，不同步等待上游。缓存期限为 5 分钟；页面在缺少快照或快照过期时调用此刷新接口，手动刷新使用 `?force=true` 立即绕过缓存期限。

## Usage 窗口字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | 上游窗口标识，如 `primary` / `secondary` |
| `limitWindowSeconds` | number | 上游提供的窗口长度；缺失时省略 |
| `usedPercent` | number | 上游使用比例，若无法解析则省略 |
| `remainingPercent` | number | 有效 `usedPercent` 存在时计算为 `100 - usedPercent`，限制在 0–100 |
| `resetAt` | string | 解析后的 UTC 时间；若无法解析则省略 |

窗口按上游数据顺序返回，允许窗口数量、名称和字段随上游变化。客户端不应依赖固定窗口集合。
