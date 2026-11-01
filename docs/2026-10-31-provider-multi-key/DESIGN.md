# Provider 多密钥与 Key 切换：设计

术语以仓库根 `CONTEXT.md`「上游与凭据」一节为准：Provider、Provider 密钥、密钥身份、路由候选、Key 切换、故障转移、密钥相关失败、密钥停用、密钥冷却、可选择密钥。本文所有结论均已与使用者逐条确认（grilling 共 44 项），实现时不得重新取舍；确有冲突记 `KNOWN_ISSUES.md`。

## 1. 目标与范围

- 一个 Provider 可配置多份可互换的 API Key；请求在候选内部按配置顺序切换密钥，不占故障转移与本地重试额度。
- 仅 API Key。`authType: chatgptOAuth` 仍为一个 Provider 绑一个账户，不与密钥列表共存。
- 只做有序切换，不做轮转分流、不做密钥级粘性；会话粘性仍只记 `provider/model`。
- 旧的单值 `apiKey` 写法行为逐字节不变：不冷却、不切换、仍可在为空时回退客户端请求头。
- 不提供自动合并重复 Provider 的工具，不改使用者本地 `config.yaml`。

## 2. 配置模型（`internal/config`）

```yaml
providers:
  tabi:
    baseUrl: https://tabitoken.com
    format: anthropic
    apiKeys:                 # 与 apiKey 互斥；非空即「列表模式」
      - id: k7f3a9c2         # 必填，Provider 内唯一，1-32 位 [A-Za-z0-9_-]
        name: 主账号          # 选填，≤64 字符，可重名，仅展示
        value: sk-xxx        # 必填，非空、不含控制字符，同 Provider 内不可重复
      - id: k1b8e4d0
        value: sk-yyy
        enabled: false       # 选填，nil = 启用；applyDefaults 不物化
    keyCooldown:             # 选填，仅列表模式可写；各项 0-86400000
      authMs: 300000         # 401 / 403，默认 300000
      quotaMs: 60000         # 402，默认 60000
      rateLimitMs: 30000     # 429，默认 30000
```

- 新增类型 `ProviderKey{ID, Name, Value string; Enabled *bool}`、`KeyCooldown{AuthMs, QuotaMs, RateLimitMs *int}`；`Provider` 增 `APIKeys []ProviderKey`、`KeyCooldown *KeyCooldown`，yaml/json 一律 `omitempty`。
- accessor（默认值只此一处）：`(*Provider).UsesKeyList()`（`len(APIKeys) > 0`）、`(ProviderKey).IsEnabled()`、`(*Provider).KeyCooldownFor(class)`。
- `apiKeys: []` 与不写等价。`apiKey` 非空且 `apiKeys` 非空 → 报错；`chatgptOAuth` 配 `apiKeys` → 报错（同现有 apiKey 冲突措辞）；`keyCooldown` 非 nil 而非列表模式 → 报错。
- 数量上限 10（含停用）。启用的 Provider 允许全部密钥停用，保存不报错，运行时按 §5 跳过。
- 值重复校验在 sentinel 回填之前会误判：`validate` 中跳过值为 `APIKeyKeepSentinel` 的条目，`handlePutConfig` 回填后再调用导出的 `config.ValidateProviderKeyValues` 复查。
- `ValidateProbeProvider` 同样校验密钥列表字段（不校验并发限额，同现状）。

## 3. 机密处理与管理 API 回填

- `RedactYAML` 只替换 `providers.*.apiKeys[*].value`，不得误伤 `extraBody` 等处同名 `value` 键；`apiKey`、代理密码规则不变。
- `configViewSnapshot`：每个密钥的 `value` 改为 sentinel；`handleGetConfig` 在每个密钥视图上附 `valueConfigured: true` 与 `fingerprint`（与请求日志同算法，见 §7）。前端提交时这两个字段必须剔除（严格解码会拒绝未知字段）。
- `handlePutConfig` 回填规则（按 Provider 名 + 密钥身份）：
  - 旧配置是列表模式：sentinel 只能回填旧列表中同 `id` 的值，否则 400「无法保留已有密钥：磁盘上没有该身份的已存密钥」。
  - 旧配置是单值 `apiKey`（网页上把旧写法转换为列表）：新列表中**至多一条** sentinel 可继承旧 `apiKey`，多条则 400。
  - 新配置回到单值 `apiKey` sentinel：沿用现有规则。
  - 回填后复查值重复。
- `/api/providers/probe` 的 sentinel 回填与上面同规则（按请求体 `name` + 密钥身份），**不**因表单地址改变而拒绝（沿用现状，已确认不收紧）。
- 启动横幅：列表模式只打印「密钥 启用 m/总 n」与各密钥指纹；单值写法仍用 `mask`。

## 4. 密钥运行时状态（新包 `internal/keypool`）

只依赖 `internal/config`，供 `cmd/gateway`、`internal/vision`、`internal/providerhealth` 使用，避免成环。

- **状态键**：`(provider name, key id)`。每条状态带 `credHash`（密钥值的 SHA-256）与所属 Provider 的 `connFP`。
- **connFP** 覆盖：`baseUrl`、`format`、`authType`、`proxy`、`userAgent`、`extraHeaders`（键排序）。不含并发、限速、排队、输出上限、上下文窗口、`extraBody`、本地重试、冷却时长、启用状态。
- **Reconcile(cfg)**（`applyRuntimeConfig` 内调用）：删掉已删除的 Provider / 密钥；`connFP` 变 → 清空该 Provider 全部密钥状态；`credHash` 变 → 清空该密钥状态；改名（`name`）、排序、启用切换、`keyCooldown` 变化都**保留**状态。Provider 改名等同删旧建新。进程重启清空全部。
- **Ticket**：选择密钥时发放，含单调递增序号、`credHash`、`connFP`。`Report` 时 ticket 与当前状态的 `credHash/connFP` 不一致即丢弃（旧版本请求不得更新新版本状态）。
- **可选择**：已启用、未冷却（`now < until` 视为冷却）、且不在调用方传入的本候选排除集中。`Select` 按配置顺序返回首个可选择密钥；无可选择时返回最早冷却结束时间（仅当存在冷却中的密钥）。
- **冷却时长**：失败类别只按状态码（不解析正文；OpenAI `insufficient_quota` 作为 429 也按限流处理）：

| 状态码 | 类别 | 默认冷却 |
|---|---|---:|
| 401、403 | auth | 300000ms |
| 402 | quota | 60000ms |
| 429 | rateLimit | 30000ms |

  - quota / rateLimit 带有效 `Retry-After` 时，冷却 = `Retry-After`，不截短、不受 `maxRetryAfterMs` 影响；auth 不看 `Retry-After`。
  - 配置值为 0：关闭该类默认冷却；若有 `Retry-After` 仍遵守。
  - 冷却期间再失败：`until = max(旧 until, 新 until)`，不累加。时长配置变化不回改已有 `until`。
- **冷却只按以下方式结束**：到期、手动解除（`Release`，只清冷却，不改启用状态、不发探测）、凭据替换、connFP 变化、删除密钥、进程重启。**任何成功都不清除冷却**。
- **事件**（回调由 server 接到 `eventLog`）：
  - auth / quota 失败使密钥进入冷却 → `key_cooldown`。同一密钥、同一凭据、同一类别在恢复前只记一次；类别变化可再记一次。rateLimit 冷却不记事件。
  - 每次 auth / quota 失败把 `abnormalSeq` 更新为该 ticket 序号；之后 **ticket 序号大于 `abnormalSeq`** 的成功才记 `key_recovered` 并结束异常（冷却开始前已发出、后来才成功的请求不算恢复）。
  - 冷却到期本身不记恢复。
- **Snapshot(cfg)**：列表模式 Provider 的每个密钥：`id`、`name`、`fingerprint`、`enabled`、`state`（`disabled` / `selectable` / `cooling`）、冷却时 `reason`（类别 + 状态码）、`remainingMs`、`until`。

## 5. 主转发（`cmd/gateway/main.go`）

### 5.1 候选检查顺序

每个候选：① Provider 停用 → ② 协议不兼容 → ③ **无可选择密钥**（列表模式，候选开始时判定）→ ④ 熔断 `Allow` → ⑤ 上下文窗口。③ 在熔断前：不发请求的候选不占半开探针。③ 跳过不消耗额度、不排队、不调 `breaker.Report`，记 `AttemptDetail{Kind: "key_skip", Outcome: "skipped", Reason: "no_selectable_key"}`，trail `name:no_selectable_key`，并汇总最早恢复时间。

### 5.2 候选内部循环（伪代码，示意意图）

```
excluded := {}                       // 本候选因密钥相关失败放弃的密钥，本次请求不再回头
chainStart, chainUsed := 新链, 0     // Key 切换链共用超时；本地重试开启新链
for {                                // 本地重试循环（现有）
    for {                            // Key 切换循环（新增，仅列表模式）
        ticket := keypool.Select(provider, excluded)   // 每次重选：并发请求可能刚冷却了某密钥
        if ticket == nil { break }                      // 竞态下无可选择：按「最后一次实际失败」收尾
        hasNextKey := 除 ticket 外仍有可选择密钥
        outcome := forwardAttempt(..., key=ticket, keySwitchAllowed=hasNextKey,
                                  timeout=剩余链预算)    // 每次都重新 Acquire 队列
        上报 keypool（见 5.4）
        if outcome.keySwitch { excluded += ticket; continue }   // 立即换，不等待、不扣本地重试
        break
    }
    现有本地重试判定；本地重试只处理非密钥类失败与 5.3 的例外
}
breaker.Report(本候选最终结果)        // 仍每候选一次
```

- **触发 Key 切换**：401、402、403、429（含流式预读阶段的等效状态码）。独立于 `failover.enabled` 与 `onXxx` 开关，也不受 `maxAttempts`、本地重试次数约束。其他 4xx、5xx、网络错误、超时、队列超时、转换失败一律不切换，走现有本地重试 / 故障转移。响应已提交给客户端后不再切换。
- **ShouldRetry 判定次序**：列表模式下若状态码属密钥相关、存在下一个可选择密钥、链预算剩余 ≥ 1s → 放弃并标 `keySwitch`；否则落回现有 `classifyFailure` 判定（failover / 本地重试）。
- **本地重试**：列表模式下 401 / 402 / 403 **不做**本地重试（重试同一坏密钥无意义）；仍可按现有规则故障转移。

### 5.3 只剩一个密钥时短暂 429 的例外

同时满足：本候选已无其他可选择密钥、这次 429 未超过现有 `maxRetryAfterMs` 阈值（即不是 freeAttempt）、本地重试仍有剩余次数 → 在同一密钥上按原间隔本地重试，**此时不冷却**；重试结束仍是密钥相关失败才冷却。鉴权、权限、付款不适用。

### 5.4 冷却上报时机

- 放弃并切换的密钥：立即按真实状态码与 `Retry-After` 冷却，加入 `excluded`。
- 无法切换、交给故障转移或原样返回的密钥相关失败：同样冷却（5.3 例外推迟到重试耗尽）。
- 成功：`Report(success)`，仅用于恢复事件判定。
- 非密钥类失败：不更新密钥状态。

### 5.5 超时与排队

- Key 切换链共用一次转发的超时：非流式共用 `timeout`（或直通 `directTimeoutNoStream`）；流式共用响应提交前预算（响应头超时，预读时间计入已用），提交后沿用现有活跃超时。
- 已用时间只计上游等待（从拿到队列槽之后到 `proxy.Forward` 返回），**排队等待不扣**。每次重新排队仍受 `maxQueueWait` 约束；排队超时按现有队列超时处理，不算密钥相关失败。直通模式不排队。
- 本地重试保持现有独立预算，开启新链。

### 5.6 额度与终态

- `freeAttempt`（长 `Retry-After` 不占额度）按候选汇总：本候选所有实际请求都是符合现有规则的长限流才免额；停用、冷却跳过始终不计额度。
- 无法转移时保留最后一次实际失败的现有响应方式；**绝不**回退客户端请求头里的密钥（列表模式下即使全部停用 / 冷却 / 失败）。
- 一个真实请求都没发出时，终态优先级（`switch` 次序）：

| 优先级 | 原因 | HTTP | error type |
|---|---|---:|---|
| 1 | Provider 已停用 | 503 | `all_candidates_disabled` |
| 2 | 无可选择密钥 | 503 | `no_selectable_provider_key`；存在冷却中密钥时带 `retry-after`（最早恢复，≥1 秒） |
| 3 | 熔断 | 503 | `breaker_open` |
| 4 | 长时间限流 | 429 | `all_candidates_rate_limited` |
| 5 | 上下文窗口不够 | 413 | `context_window_exceeded` |
| 6 | 协议转换失败 | 400 | `conversion_error` |

- 熔断：仍每候选按最终结果上报一次；密钥相关失败均为 4xx，本就 `OutcomeIgnored`。
- `x-ai-gateway-attempts` 与 `RequestLog.Attempts` 保持现有口径（Key 切换不计）。

### 5.7 请求日志

- `AttemptDetail` 增 `keyId`、`keyName`、`keyFingerprint`（无明文）；每次真实请求一条明细。被切换的明细 `Outcome: "key_switched"`、`Reason` 形如 `401/key_auth`、`402/key_quota`、`429/key_rate_limit`。单值写法只填 `keyFingerprint`。
- trail 中带密钥身份：`name#keyId:401/key_auth`；单值写法保持现状。
- 顶层 `KeySource` / `KeyFingerprint` 继续表示最后一次实际使用的密钥；列表模式 `KeySource` 为 `provider`。

## 6. 其他出网路径

| 路径 | Key 切换 | 遵守已有冷却 | 更新冷却 | 时间预算 |
|---|---|---|---|---|
| 图片识别 | 是 | 是 | 是 | 现有共享识别预算（120s），换 Key 不重置 |
| 健康检测 | 是 | 是 | 否 | 现有单检 5s，换 Key 不重置 |
| 已保存配置的模型查询 | 是 | 是 | 否 | 现有 10s |
| 未保存表单的连接测试 | 是 | 否 | 否 | 现有 5s |
| 单个密钥测试 | 否（只测一个） | 否 | 否 | 现有 5s；停用的密钥也可测 |

- 共同规则：只在密钥相关失败（401/402/403/429）时换下一个；其他失败立即结束。首次成功即结束，不合并多个密钥的结果。
- **图片识别**：列表模式不调 `ResolveAPIKey`。每次换 Key 重新排队（非直通）。无可选择密钥时识别失败，类别归 `other`，消息写明「没有可选择的 Provider 密钥」。
- **健康检测**：每 Provider 仍一条状态。

| 情形 | 健康状态 |
|---|---|
| 前面密钥鉴权失败，后面密钥成功 | `ok`，消息注明哪些密钥失败 |
| 第一个密钥返回 5xx 等非密钥失败 | 按现有规则归类，不换 Key |
| 所有尝试都是密钥相关失败 | 按最后一次结果的现有规则归类 |
| 全部停用或冷却 | `warn`，不发请求，消息「没有可选择的 Provider 密钥」 |

  `providerFingerprint` 需纳入密钥列表（身份、值、启用状态），否则改密钥后健康缓存不失效。
- **模型查询**：无可选择密钥时返回 503 `no_selectable_provider_key`。
- **连接测试**：请求体新增 `apiKeys`（可含 sentinel，按 §3 回填）与 `testKeyId`；有 `testKeyId` 时只测该密钥，否则按顺序测已启用密钥。

## 7. 观测

- 指纹统一为 `SHA-1` 大写十六进制前 8 位（现 `keyFingerprint` 算法）；在 `keypool` 导出唯一实现，`cmd/gateway` 复用，避免两份。
- 事件：`key_cooldown`（低频、非限流）、`key_recovered`（`Recovered: true`）；`Event.Provider` 填 Provider 名，`Detail` 写密钥名 / 身份、状态码、冷却时长。`metrics.EventLog.Add` 的恢复判定需识别 `key_recovered`。
- `/health` 增 `providerKeys`：`{providerName: [keypool.Snapshot 条目]}`，仅列表模式 Provider 出现。
- 手动解除：`POST /api/providers/keys/cooldown/reset?provider=&key=`；现有熔断重置**不**连带清除密钥冷却。
- 不新增密钥排行榜、不新增按密钥聚合的指标。

## 8. 网页

- Provider 抽屉里的密钥列表：新增、替换值、删除、停用、拖拽排序、单行「测试此密钥」、冷却中显示原因与剩余时间并可「解除冷却」。身份由前端随机生成（如 `k` + 7 位 hex），不可编辑；名称可改。只显示指纹，不显示值的头尾片段。
- 冷却时长三项输入框仅在列表模式显示；全部停用时给出提示（不阻止保存）。
- 编辑旧单值写法：显示为列表第一行；列表未改动就保存仍写回单值 `apiKey`；一旦增、删、改名、停用、排序、替换值，保存为 `apiKeys` 并提示「将启用密钥冷却」；之后不自动改回单值。
- 复制 Provider 不复制密钥（同现状）；切换到 ChatGPT OAuth 清空密钥列表。
- Provider 表格的「已配置 Key」徽标在列表模式显示启用数 / 总数与冷却数。
- 请求日志详情显示每次尝试的密钥名称 / 身份 / 指纹；运行异常区渲染两种新事件。
- 改了 `cmd/gateway/web/src/` 必须重建 `index.html`（`go run ./cmd/webbuild`），`go run ./cmd/webbuild -check` 必须 exit 0。优先复用已有 class，避免重建 Tailwind（需联网 npx）；确需新 class 时记入 `KNOWN_ISSUES.md`。

## 9. 推导出的默认做法（未单独提问）

- 请求开始时取配置快照；热重载不改变在途请求使用的密钥列表，旧 ticket 的上报被版本校验丢弃。
- 会话粘性只记 Provider 与模型；熔断按候选最终结果上报一次。
- 所有密钥停用或冷却时，健康检测记 `warn`，运行异常区可能同时出现健康事件与密钥冷却事件。

## 10. 范围外

- 轮转分流、密钥级配额 / 并发、密钥级指标排行。
- ChatGPT OAuth 多账户。
- 自动合并重复 Provider。
- 按错误正文细分冷却类别。
- 表单地址变化时拒绝使用已保存密钥（可另提需求）。
