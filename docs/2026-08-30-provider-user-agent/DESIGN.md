# DESIGN：provider 级可配置 User-Agent

日期：2026-08-30
状态：待实现（Codex）

## 1. 背景与问题

网关向上游发请求时**从不设置 `User-Agent`**。`internal/proxy/upstream.go` 的 `setUpstreamHeaders`
只设 `content-type`、`accept` 和鉴权头，Go 的 `http.Client` 因此自动填入默认值
`Go-http-client/1.1`（已由 echo 假上游抓包实证）。

部分上游按客户端类型做准入限制。实测 `ar-gh`（`https://agentrouter.org`）在默认 UA 下
返回 `HTTP 401` + `unauthorized_client_error`（`unauthorized client detected`）；使用者实测
把 UA 设为 `claude-cli/2.1.161 (external, cli)` 后可正常使用。

补充事实：该 401 与凭据有效性无关——用伪造 key 能复现字节级相同的错误正文，
说明上游在校验凭据之前就按客户端特征拒绝准入。

## 2. 目标

1. provider 可显式配置 `userAgent`，YAML 与管理页面两条路径都能配。
2. 未显式配置时，**转发客户端真实 UA**（使用者已拍板的默认行为）。
3. 配置期拦截非法值，避免运行期每次请求都失败在 `invalid header field value`。

## 3. 取值优先级（唯一权威定义）

```
provider.userAgent 非空  → 用它
否则客户端请求带 User-Agent → 原样转发
否则                      → 不设该头，交给 Go 填默认值 Go-http-client/1.1
```

第三档**必须是「不设」而不是「设空字符串」**。实测三种写法行为不同：

| 写法 | 上游实际收到 |
|---|---|
| 完全不设 | `Go-http-client/1.1`（Go 填默认值） |
| `Set("User-Agent", "")` | **头完全消失**（`r.Header["User-Agent"]` present=false） |
| `Del("User-Agent")` | `Go-http-client/1.1` |

即「设空字符串」是唯一能彻底删掉该头的写法。若客户端没带 UA 而代码无脑
`Set(clientUA)`，会发出一个连 UA 头都没有的请求，与「退回默认值」的意图相反。

## 4. 行为变更影响（重要）

这是一次**有意的行为变更**，不是纯增量特性。

现网 16 个 provider 目前一律发 `Go-http-client/1.1`；改动后未配 `userAgent` 的 provider
将改发**客户端真实 UA**（Claude Code 为 `claude-cli/...`，Codex 为其自身标识）。

- 收益：Claude Code 的请求天然带 `claude-cli/...`，ar-gh 这类按客户端限制的上游自动绕过，
  无需逐个 provider 配置。
- 代价：向所有上游泄露客户端身份与版本。使用者已知悉并选择该默认行为。
- 想固定成某个值（不随客户端变化）的 provider，显式配 `userAgent` 即可覆盖。

## 5. 校验规则与理由

Go 在传输层就挡住了头注入：含 `\r` 或 `\n` 的值会让 `client.Do` 直接返回
`net/http: invalid header field value for "User-Agent"`，**不会**被拼进请求。
故这里**不存在头注入漏洞**，配置期校验的目的是「早报错、报得清楚」。

不校验的后果：配错的 provider 每次请求都在传输层失败，而网关会把它当传输错误，
`failover.onTransportError` 默认为 true，于是把所有候选依次试一遍才返回——
一个配置笔误会放大成整条候选链的浪费。

规则：
- 长度 ≤ 256 字符（按 rune 计，与 `maxProviderNameRunes` 的既有风格一致）。
- 拒绝 ASCII 控制字符（`< 0x20` 及 `0x7F`），但**允许水平制表符 `\t`**——
  Go 自身接受它（实测 `"tab\there"` 正常发出），校验不应比传输层更严而造出
  「配置校验拒绝、但协议本身合法」的假失败。
- 空值合法，语义是「不配置」，走第 3 节的第二/三档。
- 不校验内容格式（不强求 `product/version` 形状）：上游要什么值只有使用者知道，
  网关不该替他们判断哪个 UA「像真的」。

## 6. 改动面

| 层 | 文件 | 改动 |
|---|---|---|
| 配置 | `internal/config/config.go` | `Provider` 加 `UserAgent` 字段；`validate` 加校验；新增长度上限常量 |
| 转发 | `internal/proxy/upstream.go` | `setUpstreamHeaders` 增参数，按第 3 节优先级设头 |
| 转发 | `internal/proxy/proxy.go` | 两处调用点（约 335、408 行）传入客户端 UA |
| 前端 | `cmd/gateway/web/src/index.template.html` | Provider 弹窗加输入框 |
| 前端 | `cmd/gateway/web/src/app/00-state.js.part` | `providerForm` 初始状态加字段 |
| 前端 | `cmd/gateway/web/src/app/11-providers.js.part` | **四处**：`addProvider` / `editProvider` / `duplicateProvider` / `saveProvider` |
| 前端产物 | `cmd/gateway/web/index.html` | 跑 `webbuild` 重新生成，不手改 |
| 文档 | `config.example.yaml`、`README.md`、`CLAUDE.md` | 字段说明与默认行为 |

### 前端第 4 处是必须的

`11-providers.js.part` 的 `saveProvider()` 用**白名单重建** provider 对象
（`{baseUrl, apiKey, apiKeyConfigured, format, maxConcurrent, maxPerSecond, maxQueueWait}`）。
漏掉 `userAgent` 会导致：YAML 里配好的值，只要用户在管理页面保存过一次该 provider，
就被静默丢弃。这类「改了别处、丢了这处」的缺陷不会有任何报错，必须在实现时一并覆盖。

`duplicateProvider` 同理——复制 provider 时应继承 `userAgent`（它不是密文，
与 `apiKey` 的「不继承」处理不同）。

## 7. 无需改动的部分

- `/api/config` 的脱敏与回显：配置视图由结构体 JSON 序列化自动产出
  （`main.go:handleGetConfig`），加字段即自动回显。UA 不是密文，不进脱敏名单。
- 热重载：`Provider` 是值拷贝逐请求取用，新字段随配置热重载自然生效，
  无需动 `applyRuntimeConfig` 的 `Reconcile` 三件套。
- `internal/providerhealth`、`fetchUpstreamModels`：本次不改。它们探测
  `/v1/models` 用的是另一条构建路径，与转发不共用 `setUpstreamHeaders`。
  **这是有意的范围限制**，记入 KNOWN_ISSUES 而非顺手扩大改动面——
  健康检测若也要带 UA，属独立需求。
