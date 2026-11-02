# API 规格：ChatGPT 账号用量刷新

状态：已集成；2026-11-01 使用者确认既有验收完成，文档已按当前代码补正；专项回归边界见 `TASKS.md`。

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
    "lastAttemptAt": "2026-10-13T09:00:00Z",
    "lastSuccessAt": "2026-10-13T09:00:00Z",
    "lastCheckedAt": "2026-10-13T09:00:00Z"
  }
}
```

上述数值仅为结构示例。实际响应只包含实现支持且上游提供有效比例的窗口，以及可得的可选字段和 credits 余额；不填造默认配额。窗口缺少或无法提供有效 `used_percent` 时，不返回整个窗口，重置时间/长度也不单独展示。`error` 仅是可展示的安全摘要，无错误时该字段省略。

既有 `expiresAt` 继续表示 access token 到期时间，不改变其语义。账号的 `accessToken`、`refreshToken` 及上游原始响应不得加入此 API。

## 刷新用量

### `POST /api/chatgpt/accounts/usage/refresh`

可选查询参数 `force=true` 跳过 30 分钟缓存并强制查询；省略或为 `false` 时复用仍有效的结果，包括最近一次失败状态。有效期以最近尝试时间 `lastAttemptAt` 为准，旧快照缺少该字段时回退到兼容字段 `lastCheckedAt`。

刷新所有已连接 ChatGPT 账号的 usage 快照，返回与账号列表相同的安全视图：

```json
{
  "data": [
    {
      "id": "account-id",
      "status": "ready",
      "usage": {
        "lastAttemptAt": "2026-10-13T09:00:00Z",
        "lastSuccessAt": "2026-10-13T09:00:00Z",
        "lastCheckedAt": "2026-10-13T09:00:00Z"
      }
    }
  ]
}
```

逐账号执行并发上限为 3 的上游查询，每账号查询预算为 15 秒。单个账号失败时保留旧快照，将安全错误摘要写入该账号的 `usage.error` 并继续处理其他账号；批量接口仍返回 HTTP 200 和完整账号列表。当前处理器不把单账号上游失败或请求取消统一升级为批量非 2xx；账户管理器不可用、方法或同源检查失败等入口错误才使用既有 API 错误格式。

列表 API 读取内存快照，不同步等待上游。快照和最近查询状态会随账户记录原子保存到 `data/chatgpt-accounts.json`，启动时恢复；缓存落盘失败只记录安全日志，仍保留刚查询到的内存快照，重启后可能回到较旧的磁盘快照。缓存期限为 30 分钟；页面按兼容的 `lastCheckedAt` 判断是否缺失或过期，手动刷新使用 `?force=true` 立即绕过期限。

`lastAttemptAt` 是最近查询尝试时间，`lastSuccessAt` 是最近成功时间，`lastCheckedAt` 保留为兼容字段，含义与最近尝试时间一致。失败时保留旧窗口、credits 和最近成功时间，仅更新尝试时间及安全 `error`；空 `error` 和空窗口列表按 `omitempty` 省略。

## Usage 窗口字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | 上游窗口标识，如 `primary` / `secondary` |
| `limitWindowSeconds` | number | 上游提供的窗口长度；缺失或为 0 时省略；整个窗口仍要求有效使用比例 |
| `usedPercent` | number | 可解析的上游使用比例；无有效比例不返回整个窗口，重置时间/长度也不单独展示 |
| `remainingPercent` | number | 与有效 `usedPercent` 一起返回，计算为 `100 - usedPercent`，限制在 0–100 |
| `resetAt` | string | 解析后的 UTC 时间；若无法解析则省略 |

当前解析 `rate_limit`、`code_review_rate_limit` 和 `additional_rate_limits` 内的 primary/secondary 窗口；窗口按该解析顺序返回，名称可带 code review 或额外限额名称。窗口数量和可选字段来自受支持的上游结构，客户端不应依赖固定配额；未知结构不会自动变成窗口。比例字段缺失或为 null 时丢弃该窗口；字段类型错误使整个 usage 响应无法解码时，按单账号查询失败处理并保留旧快照。

## 单账号刷新

### `POST /api/chatgpt/accounts/{accountId}/usage/refresh`

刷新指定账户并返回 `{ "data": AccountView }`，包含当前 Provider 引用，不含令牌。此接口默认强制查询，只有显式 `force=false` 才复用 30 分钟缓存；这与批量接口默认不强制的口径不同。账户 ID 无效返回 `400 invalid_account_id`，账户不存在返回 `404 account_not_found`，入口遵循既有方法和同源检查。单账号上游查询失败同样保留旧快照及安全错误，不把用量查询失败等同于账户已失效。

## 文档维护说明

2026-11-01 按当前代码将原 5 分钟/仅内存/比例缺失仅省略字段的描述替换为上述实现；专项测试未在本次维护新增，缺口移交 `docs/KNOWN_ISSUES.md` 的“ChatGPT 账户功能专项回归覆盖”，归档记录见 `TASKS.md`。
