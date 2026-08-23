# 故障转移、熔断、负载均衡实施计划

日期：2026-08-23
状态：阶段一进行中（配置层部分落盘，详见文末「当前代码状态」）

---

## 1. 现状：三项能力都不存在

### 1.1 没有负载均衡，是静态单播路由

`config.Route` 只有一个 `provider` 字段，`router.MatchRoute` 按 `routes` 顺序做 glob 匹配、首条命中即返回，没有权重、轮询、最少连接。同一个模型名永远打到同一个上游。

多条 route 指向不同 provider 只是**按模型名静态分流**，与「同一模型在多个上游间分配流量」不是一回事。

容易被误认为负载均衡的是 `internal/queue`：per-provider 的 `maxConcurrent` + `maxPerSecond` 滑动窗口 + FIFO 排队 + `maxQueueWait`。那是单上游的流量整形与背压，达到上限时请求在本地排队，等不到返回 503 `queue_timeout`，不会转投其他 provider。

### 1.2 没有熔断

全仓搜索 `breaker|circuit|failover|fallback|retry` 只命中 `config.go` 的 `canDirectFallbackAfterCreate/Rename`，那是配置保存时临时文件创建失败后直写的回退，与上游故障无关。

`internal/providerhealth` 容易被当成熔断，但它**不参与请求决策**，全部使用点是：

- `Snapshot()` → 塞进 `GET /health` 响应给人看（`cmd/gateway/main.go:554`）
- `CheckAll()` → 只由 `POST /api/providers/health` 手动触发（`main.go:582`）
- `InvalidateChanged()` → 配置热重载时按 provider 指纹清理过期状态（`main.go:1042`）

探测方式是 GET 上游 `/v1/models`（`modelsEndpoint`），5 秒超时，最多 4 并发，结果带 1 秒冷却缓存。没有后台定时探活 —— 进程里唯一的 `time.NewTicker` 是前端热加载 SSE 轮询 `index.html` 的 mtime。

`handle()` 从 `MatchRoute`（`main.go:370`）到 `proxy.Forward`（`main.go:487`）完全不读健康状态。provider 被标成 `error`，请求照样往它发。

### 1.3 没有故障转移，失败即终态

`proxy.Forward`（`internal/proxy/proxy.go:57`）流式和非流式各只有一次 `opts.HTTPClient.Do(req)`，没有重试循环。错误直接映射成客户端响应：

| 情况 | 响应 |
|---|---|
| 连接失败 | 502 `proxy_error` |
| 超时（含流式 header 超时、活跃超时） | 504 `timeout_error` |
| 上游 4xx/5xx，含 429 | 原样透传状态码与响应体 |
| 客户端已断开 | 静默返回 |

`config.example.yaml:11` 把这个设计写明了：限流由上游用 429 处理，网关「原样透传，不重试、不排队」。流式中途失败只补发合规 SSE 收尾事件（`finishStream`，`proxy.go:403`），不切换上游 —— 部分响应已发出，重放不安全。

---

## 2. 调研：LiteLLM / Portkey / Cloudflare 怎么做

> 说明：本节来自搜索结果摘要，WebFetch 在调研时不可用，未读到完整页面。官方文档的默认值与 GitHub issue 描述可信度较高，但具体参数当前值建议实现时再核原文。

**LiteLLM** 把可靠性拆成三层且彼此独立：`num_retries`（同一 deployment 内重试）→ `fallbacks`（跨 model group 转移）→ `cooldown`（per-deployment 摘除）。cooldown 用 `allowed_fails` 计数配 `cooldown_time`，可用 `allowed_fails_policy` 按错误类型分别设阈值。路由策略默认 `simple-shuffle`（带权重），另有 `least-busy` / `usage-based-routing` / `latency-based-routing`。

**Portkey** 是 JSON config 编排，`retry` 与 `fallback` 是可组合的块。默认重试状态码 `[429, 500, 502, 503, 504]`，fallback 默认在任何非 2xx 触发。有个硬上限：最大重试等待 60 秒，上游要求更长延迟就跳过重试直接返回错误。

**Cloudflare** Universal Endpoint 已废弃，改成 Dynamic Routing —— 可视化/JSON 编排流程，per-model 的 timeout 与 retry，用 `cf-aig-step` 响应头告知客户端实际哪一步成功。

### 2.1 印证的部分

- **429 触发转移是三家共识**，Portkey 直接把它放进默认重试码。
- **熔断需要最小样本数**：LiteLLM issue #17418 就是「错误率熔断在第一次失败就触发，因为缺少最小请求数阈值」。
- **流式保守边界站得住**：Portkey 文档自身有冲突 —— streaming 页称「stream 中途失败可自动 retry 或 fallback」，但 Anthropic overloaded 那页明确写「流式请求上的 output guardrail 只能是信息性的，不触发 fallback 或 retry，要自动 fallback 请用非流式」。对 Claude Code 这类解析 SSE 的客户端，已发过 `message_start` 再来一遍会破坏解析。

### 2.2 LiteLLM 的一串同类 issue 给出的教训

#24366（429 被错包成 `APIConnectionError` 导致 cooldown 不触发）、#27362（`APIConnectionError` 硬编码导致 failover 根本不发生）、#32425 与 #29283（配置项被静默忽略）。

共同点是**错误分类与配置生效与否不可观测** —— 用户配了以为生效，实际没有。两个应对写进本方案：错误分类结果必须进 `AttemptTrail`；新增配置字段继续走 `KnownFields(true)` 严格解码，并且默认值处理不得静默覆盖用户显式值（见 4.1 的 `*bool` 缺陷）。

---

## 3. 已定决策

### 3.1 用户拍板（候选按「不同厂商/账号」配置，即独立额度池）

| # | 决策 | 值 | 依据 |
|---|---|---|---|
| 1 | failover 默认 | 关（`enabled: false`） | 重试有上游双花风险（首次可能已被上游实际处理只是响应丢了），需用户主动接受 |
| 2 | 429 转移 | **开** | 独立额度池，A 超额换 B 真实有效；与三家默认一致 |
| 3 | 超时转移 | 整体/活跃**关**，流式 header 超时**开** | header 阶段客户端零字节，边界干净；整体超时会让耗时翻倍（120s → 240s），客户端很可能先自行断开 |
| 4 | 队列等待预算 | **各候选各自给满** | 独立池，A 排满就该立刻去 B；共用预算会让第二候选只剩几秒、转移形同虚设 |

第 4 条不引入总预算字段，用 `maxAttempts` 兜住最坏情况。最坏耗时 = Σ(各候选 `maxQueueWait` + 上游超时)，默认 2 次尝试即 30s+30s 队列 + 上游超时。这一点写进 `config.example.yaml` 注释，并建议开 failover 时调低 `maxQueueWait`。

### 3.2 调研后修订

| # | 决策 | 理由 |
|---|---|---|
| 5 | 熔断判据用**连续失败计数**，不用错误率 | LiteLLM 主流做法是 `allowed_fails`，错误率是后加的且有 #17418。本地个人网关 QPS 可能长期 < 1，错误率窗口内常只有一两个样本，噪声大到无统计意义。原方案的 `windowMs`/`minSamples`/`errorRatePercent` 收敛成 `consecutiveFailures`（默认 3） |
| 6 | 必须处理 `Retry-After` | 原方案漏掉。LiteLLM #20722（上游发递增 Retry-After，router 每次只 sleep 最初的短值）、#34399（只有单 deployment 才尊重 Retry-After，多 deployment 无脑立即重试）都栽在这里 |
| 7 | 加 `x-ai-gateway-provider` / `x-ai-gateway-attempts` 响应头 | 学 `cf-aig-step`。原方案只记进 `RequestLog`，客户端看不到。重试全部发生在 `WriteHeader` 之前，流式也能带 |
| 8 | 全部候选熔断时返回 503 + `Retry-After`，不是全部放行 | 全部放行等于熔断不存在。LiteLLM 抛 `RouterRateLimitError` 附 "Try again in X seconds"，且 #27823 在要求规范成标准 `Retry-After` 头。取最早恢复候选的剩余冷却时间 |

`Retry-After` 两处用途：

- **转移决策**：429 带 `Retry-After` 且超过 `maxRetryAfterMs`（借 Portkey 的 60 秒）说明该 provider 短期没戏，直接跳过转下一个
- **熔断冷却**：用 `Retry-After` 的值覆盖固定冷却时长（LiteLLM 的 dynamic cooldown time 就是这么做的）

---

## 4. 阶段一：候选列表 + 故障转移

一次把「候选列表」地基铺好，三阶段都长在它上面：阶段一按顺序试，阶段二给候选加准入过滤，阶段三换选择策略。默认全关，不写新字段时行为与现在逐字节一致。

### 4.1 配置结构（`internal/config/config.go`）

候选是 (provider, model) 对而非单纯 provider 名 —— `mimo-v2.5-pro` 与 `deepseek-chat` 不通用，所以 `Route.Model` 必须跟着走：

```go
type Target struct {
    Provider string `yaml:"provider" json:"provider"`
    Model    string `yaml:"model" json:"model"`
}

type Route struct {
    Match    string   `yaml:"match" json:"match"`
    Provider string   `yaml:"provider" json:"provider,omitempty"` // 单目标写法（存量兼容）
    Model    string   `yaml:"model" json:"model,omitempty"`
    Targets  []Target `yaml:"targets" json:"targets,omitempty"`   // 多目标写法
    Vision   *Vision  `yaml:"vision" json:"vision,omitempty"`
}

func (r *Route) TargetList() []Target // 写了 targets 用 targets，否则退化成单目标
```

不在 `applyDefaults` 里归一化，靠 `TargetList()` 现算：`/api/config` 的 PUT 会把结构写回磁盘，归一化会把用户的单目标写法擅自重写成 `targets` 形态。

**已发现的设计缺陷：`Failover.OnXxx` 必须用 `*bool`。**

当前落盘的实现用的是 `bool`（`config.go:90-102`），无法区分「用户没写」与「用户显式写 false」。默认值要求 `onTransportError` / `onServerError` / `onRateLimit` / `onQueueTimeout` / `onStreamHeaderTimeout` 为 true，但 Go 的 bool 零值是 false；若在 `applyDefaults` 里写 `if !c.Failover.OnRateLimit { = true }`，用户显式关闭会被静默改回 true —— 正是 LiteLLM #32425 那类「配置项被静默忽略」的 bug。

**修法**：这些字段改成 `*bool`，nil 代表未设置，`applyDefaults` 只在 nil 时填默认；`validate` 不做额外校验。取值处统一走一个 `func boolOr(p *bool, def bool) bool` 辅助函数。

`validate` 新增：

- 两种写法互斥（都写或都不写都报错）
- `targets` 长度 1–5
- 每个 target 的 provider 存在、model 非空
- 拒绝完全重复的 (provider, model) 对；同 provider 不同 model 合法（降级到便宜模型）
- `failover.maxAttempts` 1–5，`failover.maxRetryAfterMs` 0–60000

### 4.2 路由（`internal/router/router.go`）

```go
type Candidate struct {
    Provider    *config.Provider // 值拷贝，隔离 APIKey 写入
    TargetModel string
}

type Match struct {
    RouteMatch     string
    Candidates     []Candidate
    VisionProvider *config.Provider
    VisionModel    string
}
```

`Match.Provider` / `Match.TargetModel` 删除，`main.go` 的 `reqLog.Provider` / `reqLog.TargetModel` 赋值跟着改。每个 Candidate 的 Provider 各做一次值拷贝，沿用现有避免共享指针的做法。

`Provider.Name`（`config.go:37`，`applyDefaults` 在 `:203` 赋值）保留不动 —— 队列、健康检测、日志都依赖它。

### 4.3 proxy 的可重试信号（`internal/proxy/proxy.go`）

不改 `Forward` 返回签名，加回调钩子。现有 4 条写响应路径原样保留，`ShouldRetry` 为 nil 时行为完全不变，现有 `proxy_test.go`（722 行）不用改：

```go
// Options 新增
// ShouldRetry 只在「尚未向客户端写入任何字节」时调用。
// 返回 true：Forward 立即放弃本次尝试，返回 ErrAttemptAbandoned，不写客户端响应。
// 返回 false 或为 nil：按现有逻辑把错误 / 上游响应写回客户端。
ShouldRetry func(upstreamCode int, retryAfter time.Duration, err error) bool

var ErrAttemptAbandoned = errors.New("本次尝试已放弃，交由调用方重试")
```

4 个决策点，全部在任何写入之前：

| 位置 | 现有行为 | 钩子输入 |
|---|---|---|
| 流式 `Do()` 失败 | 502 / 504 | err（含 `headerTimedOut` 区分） |
| 流式 `resp >= 400` | `handleStreamError`（`proxy.go:507`） | status + `Retry-After` |
| 非流式 `Do()` 失败 | 502 / 504 | err |
| 非流式 `resp >= 400` | `handleError`（`proxy.go:549`） | status + `Retry-After` |

`handleStream`（`proxy.go:198`）一旦开写不再有钩子 —— 已发出的 SSE 字节不可回收，中途失败仍走 `finishStream` 补合规收尾事件。**这是硬边界。**

放弃时把 `resp.Body` 限量 drain 再 Close，保住连接复用。

### 4.4 handle 主链（`cmd/gateway/main.go`）

视觉翻译留在循环外 —— 产出的是文字描述，与目标无关，重复调用纯浪费。循环内：解析 key → 构建上游 body → 取队列槽 → Forward。

槽位不能再用 `defer release()`（当前在 `main.go:444` 的 Acquire 之后），否则循环里累积不释放。抽成单次尝试函数，`defer` 收在里面：

```go
func (s *server) forwardAttempt(...) (abandoned bool, buildErr error) {
    // defer release() 在此函数内，panic 也不漏槽
}
```

两个必须处理的坑：

**passthrough 会污染共享 body。** 现在 passthrough 分支直接 `upstreamMap = body` 再改 `upstreamMap["model"]`，改的是解析出来的那个 map。第一次尝试改了 `model` 和 `messages`，第二次就读到脏数据。每次尝试要浅拷贝一层。

**候选间 format 可能不同。** `isPassthrough` 依赖目标的 format，必须进循环。现在 `internal.Err != nil && !isPassthrough` 直接返回 400 —— 循环里改成「该候选构建失败则跳过」，全部候选都构建不出来才返回错误。

### 4.5 观测（`internal/metrics/metrics.go`）

保持「一个 HTTP 请求一条日志」，加两个字段：

```go
Attempts     int    `json:"attempts,omitempty"`
AttemptTrail string `json:"attemptTrail,omitempty"` // "mimo:429/ratelimit → deepseek:200"
```

trail 里带错误分类而不只是状态码 —— 让人一眼看出网关是怎么归类的（对应 2.2 的教训）。

`Provider` / `TargetModel` 记最终成功服务的那个。拆成 N 条会让 per-provider 成功率失真（一个后来成功的请求会同时计一次失败）。代价是失败候选不进 provider 维度统计，阶段二的熔断自己维护计数器，不依赖这份数据。

响应头 `x-ai-gateway-provider` / `x-ai-gateway-attempts` 在写响应前设置。

### 4.6 前端（`cmd/gateway/web/index.html`，1661 行）

- 路由表单的单选 provider（`:883` 附近的 `routeForm.provider` select）→ 可增删的目标行列表
- 路由表格的单个 provider chip（`:339` 附近）→ chip 序列
- 请求日志表加 trail 列

Alpine 结构改动约 150 行，是本阶段最容易低估的部分。

---

## 5. 阶段二：熔断

新包 `internal/breaker`，自己维护计数器，不复用 `internal/metrics`（那套有界秒桶 + 固定延迟直方图是仪表盘语义，混进来会让它背两个职责）。

```go
func (b *Breaker) Allow(provider string) (ok bool, retryAfter time.Duration)
func (b *Breaker) Report(provider string, outcome Outcome)
func (b *Breaker) Snapshot() map[string]State
```

closed →（连续失败达 `consecutiveFailures`）→ open →（冷却后）→ half-open →（探测结果）→ closed / open。

**计入失败的**：传输错误、5xx、超时。
**不计入的**：429（上游正常限流被判故障比不熔断更糟，但它会走 `Retry-After` 的冷却路径）、401/403（配置问题，熔断修不了且会掩盖密钥过期）。

半开探测用下一个真实入站请求当探针，不用 `providerhealth` 的 `modelsEndpoint` —— 那探的是 `/v1/models`，通了不代表聊天端点可用。并发探针数用 atomic 限 1。

配置全局一份，不做 per-provider 覆盖：`enabled` / `consecutiveFailures`（默认 3）/ `openMs` / `halfOpenProbes`。

接入点只有两处：候选循环里过滤 `!Allow(name)` 的，以及每次尝试结束后 `Report`。不动 proxy 的写入边界，风险最低。

全部候选熔断 → 503 + `Retry-After`（取最早恢复者的剩余冷却）。

顺带把状态挂进 `/health` 与前端 provider 状态表，加手动重置按钮。**不让 `POST /api/providers/health` 自动重置熔断** —— 两个机制判据不同，耦合起来行为难解释。

---

## 6. 阶段三：负载均衡

路由级 `strategy: failover | round-robin | least-queue`，默认 `failover`（即阶段一的顺序行为）。选中目标之外，失败仍按剩余候选转移。

`least-queue` 几乎白送：`queue.Manager.StatusOf`（`internal/queue/queue.go:328`）已返回 running / queued，取最小和即可，比轮询更准。`round-robin` 需要 per-route 计数器，`router` 目前是无状态纯函数，得引入 Selector 由 server 持有并传入。

**prompt cache 粘性是必需项不是优化项。** Anthropic 与 OpenAI 都按上游侧前缀缓存计费，同一会话在多个上游间跳会让缓存全部失效，可能又慢又贵。协议里没有会话 ID，方案是对可缓存前缀（system + 首条 user 消息）做哈希，维护带 TTL 的 LRU 映射到目标，只在非 failover 策略下生效。这块最琐碎，也是它排最后的原因。

directMode 下没有队列，`least-queue` 退化成轮询，需在文档写明。

---

## 7. 验证

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -l ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test -race ./internal/queue/ ./internal/proxy/
```

新增测试：

- **config**：targets 解析、两种写法互斥、重复候选、`*bool` 显式 false 不被覆盖（table-driven）
- **router**：多候选顺序、Provider 值拷贝隔离
- **proxy**：4 个钩子触发点、`ShouldRetry` 为 nil 时行为不变、放弃时零写入、`Retry-After` 解析
- **cmd/gateway**：两个 fake 上游（首个 502、次个 200）断言客户端拿到 200 且 trail 正确；流式已开写后不重试；passthrough 下第二次尝试不读到脏 body

---

## 8. 明确不做

- **LiteLLM 的两层重试**（同目标重试 + 跨目标转移）。同目标重试只对瞬时 5xx 有意义，对 429 是纯浪费还加重上游压力，带来的配置与状态复杂度不小。保持单层候选列表按顺序试。以后需要再加 `retryOnSameTarget`，不影响现在的结构
- 流式中途切换上游（字节已发出，重放不安全）
- 跨 provider 响应缓存
- 后台定时全量探活（只在熔断半开时按需探）
- 自动重写用户配置格式

---

## 9. 当前代码状态

`internal/config/config.go` 已落盘（53 行新增，`git status` 中唯一被修改的文件）：

- `Target` 结构体
- `Route.Targets` 字段 + `Provider`/`Model` 加 `omitempty`
- `TargetList()` 方法
- `Failover` 结构体（**`OnXxx` 是 `bool`，需按 4.1 改成 `*bool`**）
- `Config.Failover` 字段

**未落盘**：

- `applyDefaults` 里 Failover 的默认值
- `validate` 里 Failover 与 route 的新校验
- `maxRetryAfterCapMs` 常量
- 4.2 之后的全部内容

代码当前可编译（`Provider.Name` 未改动）。
