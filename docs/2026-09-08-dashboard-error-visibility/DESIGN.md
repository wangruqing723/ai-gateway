# DESIGN — 监控仪表盘错误可见性增强

> 需求目录:`docs/2026-09-08-dashboard-error-visibility/`
> 状态:已与用户对齐(2026-09-08 grilling 会话,共识见本文末尾「决策记录」)
> 领域术语:见仓库根 `CONTEXT.md`(软降级、图片识别独立计数、状态变化事件、恢复事件、运行异常区、周期健康检测)

## 0. 背景与目标

三类异常信号目前对仪表盘不可见:

1. **图片识别失败**:vision 翻译失败被吞成占位文字(`vision.go:186-190`),metrics 只有 `reqLog.Vision=true` 布尔,连错误串都没留;图片全挂但下游 200 的请求被记为成功。
2. **熔断打开**:状态转变瞬间无任何记录(`breaker.go:177-193`),只能事后从 `/health` 快照的 `totalOpens` 推断。
3. **Provider 健康异常**:探测结果只存在 `/health` 快照,不进任何事件流;且健康检测无周期触发,只有手动点按钮。

目标:把三类信号补进监控仪表盘,**全部被动展示**(不推送、无告警状态机、不需确认消除)。

## 1. 架构总览

```
vision.Translate ──(新增 Result)──→ main.handle ──(reqLog 新字段)──→ metrics.Collector
breaker.Report ──(新增 OnStateChange 回调)──→ metrics.EventLog ──→ /api/metrics
providerhealth.Checker ──(新增 OnStateChange 回调)──→ metrics.EventLog
                                          ↑
main 周期健康检测 goroutine(新配置驱动) ──┘
/api/metrics 响应新增 vision、events 段 → 前端「运行异常」区 + 日志页标记/筛选
```

模块边界:

- **`internal/vision`**:仅新增「把失败情况带出来」的能力(返回结构化 Result),不依赖 metrics。
- **`internal/metrics`**:新增 EventLog(熔断/健康状态变化事件环形缓冲)与 vision 汇总聚合;RequestLog 新增 vision 统计字段。
- **`internal/breaker`**:新增 OnStateChange 回调(与 `now` 注入同理,依赖注入,不依赖 metrics)。
- **`internal/providerhealth`**:新增 OnStateChange 回调;周期检测的调度逻辑放 `cmd/gateway`(health.go 保持无 goroutine,与现有 CheckAll 并发模型一致)。
- **`cmd/gateway`**:接线(回调注入、周期 goroutine、reqLog 填充)、`/api/metrics` 响应扩展、`translateRuntime` 接口扩展。
- **前端**:`04-monitor.js.part` / `09-format-metrics.js.part` / `05-logs.js.part` / `index.template.html`。

## 2. 数据结构设计

### 2.1 vision.Result(内部/vision 新增)

`Translate` 签名变更(接口 `visionRuntime` 同步改):

```go
type Result struct {
    Total      int  // 图片块总数(含缓存命中)
    Cached     int  // 缓存命中(算成功)
    Recognized int  // 实际调 API 成功
    Failed     int  // 识别失败(含 ctx 取消中断的已处理图片)
    FirstFailure *Failure // 首个失败(无失败为 nil)
}

type Failure struct {
    Category string // 错误大类,见 2.2
    Message  string // 截断后的原始错误
}
```

- 失败明细按请求聚合,`Failure` 只存首条(展示用);完整逐图错误继续走 stderr logf,不进 metrics。
- 单张图片错误原文截断 200 字符(与现有 AttemptDetail.ErrorBody 截断口径一致)。
- `Translate` 返回值改 `([]any, Result)`;所有调用方(main.go、测试 spy)同步改。

### 2.2 图片识别错误大类(internal/vision 分类函数)

`ClassifyFailure(err) string`,返回稳定 slug:

| 大类 | 判据 |
|------|------|
| `timeout_or_cancel` | `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded`,或 visionRequestTimeout 触发 |
| `network` | `client.Do` 返回 err(连接失败/DNS/TLS),代理解析失败 |
| `upstream_http` | HTTP 状态非 2xx(`视觉 API 响应异常 (HTTP %d)`) |
| `response_parse` | JSON 解析失败、空 text、响应超限、读取失败 |
| `image_format` | `toOpenAIImageBlock` 的 source 缺失/类型不支持 |
| `queue` | `queue.ErrQueueTimeout` / `ErrProviderRemoved` |
| `other` | 兜底 |

- 分类函数内部用 `errors.Is` / `strings.Contains` 匹配现有错误文案,不改现有错误产生逻辑。
- 首个失败的大类与原文同时展示(Q6 决策:大类 + 原文并列)。

### 2.3 RequestLog 新增字段(internal/metrics)

```go
// 在既有字段后追加,不动现有字段顺序;omitempty 保持旧行为
VisionImages     int    `json:"visionImages,omitempty"`     // 图片块总数
VisionFailed     int    `json:"visionFailed,omitempty"`     // 识别失败数
VisionFailCategory string `json:"visionFailCategory,omitempty"` // 首个失败大类
VisionFailMessage   string `json:"visionFailMessage,omitempty"`   // 首个失败原文(已截断)
```

- 软降级语义(Q2):这些字段**不参与 isSuccess 判定**,只做独立展示与筛选。
- `Vision bool` 字段保留(表示「本请求启用了 vision」),与计数字段正交。

### 2.4 EventLog(internal/metrics 新增)

```go
type Event struct {
    ID        string `json:"id"`        // 自增序号,前端 key 用
    Time      string `json:"time"`      // 2006-01-02 15:04:05
    Kind      string `json:"kind"`      // "breaker_open" | "breaker_recovered" | "health_down" | "health_recovered"
    Provider  string `json:"provider"`  // provider 名
    Detail    string `json:"detail"`    // 一句话说明(如失败次数/HTTPCode)
    Recovered bool   `json:"recovered"` // 恢复事件标记,前端低优先级样式
}

type EventLog struct { /* mu + 环形缓冲,容量 100,与 Collector 同款式 */ }
func (e *EventLog) Add(kind, provider, detail string)  // 内部封 Recovered
```

- 容量 100 条(量级沿用现有环形缓冲惯例)。
- 事件去重规则(Q7):EventLog 不做去重——**去重由事件源(breaker/providerhealth)保证**:状态机只在「转变」时回调一次;恢复事件是独立的 kind,由同一状态机在恢复转变时触发。熔断器 `Reset`/`ResetAll`(手动复位)也走 `breaker_recovered` 事件,Detail 标注「手动」。
- 熔断语义:closed→open、half_open→open(探测失败重开)产生 `breaker_open`;open→half_open→closed(探针成功)、手动 Reset 产生 `breaker_recovered`。half_open 本身不产生事件(它是过渡态,用户无需感知;且 Allow 内的 lazy 转移在锁内,回调会阻塞请求路径——见 5.1)。

### 2.5 /api/metrics 响应扩展

```go
type Response struct {
    Summary      Summary
    Providers    []ProviderStats
    StatusCodes  map[string]int
    RecentErrors []RequestLog
    Vision       VisionSummary    `json:"vision,omitempty"`   // 新增
    Events       []Event          `json:"events,omitempty"`   // 新增
}

type VisionSummary struct {
    WindowImages    int     `json:"windowImages"`    // 窗口内图片块总数
    WindowFailed    int     `json:"windowFailed"`    // 窗口内失败数
    FailureRate     float64 `json:"failureRate"`     // 失败率(无样本时 0)
    RecentFailed []VisionFailureEntry `json:"recentFailed"` // 最近失败明细(≤8 条)
}

type VisionFailureEntry struct {
    Time     string `json:"time"`
    ID       string `json:"id"`       // 请求 ID,跳日志页用
    Failed   int    `json:"failed"`   // 失败数
    Total    int    `json:"total"`    // 图片块总数
    Category string `json:"category"` // 大类 slug
    Message  string `json:"message"`  // 截断原文
}
```

- vision 汇总窗口:复用 collector 的秒桶(窗口内)对 `VisionImages/VisionFailed` 累加;秒桶暂无这些计数的,需要在 `metricBucket` 增加 per-bucket vision 计数字段。窗口内无样本时 fallback 扫日志环(与 provider fallback 同模式)。
- `RecentFailed` 从日志环倒序取 `VisionFailed>0` 的记录,最多 8 条(量级沿用 recentErrorsLocked(8))。
- `Events` 来自 EventLog,倒序(最新在前),容量截断 100。

### 2.6 日志页筛选(LogFilter 扩展)

`LogFilter` 新增 `VisionFailed string`(值 `yes`):match 时 `VisionFailed>0` 即命中。HTTP query param `visionFailed=yes`。前端下拉新增选项「图片识别失败」。

## 3. 配置设计

### 3.1 新配置项(周期健康检测,Q11 决策 B)

```yaml
providerHealth:
  checkIntervalSeconds: 300   # 默认 0 = 关闭;> 0 时每 N 秒自动探测全部 provider
```

- 结构:`config.Config` 新增 `ProviderHealth ProviderHealthConfig`,`ProviderHealthConfig{ CheckIntervalSeconds int }`。
- 默认 0 = 不启用(零值=未配置惯例)。validate:负数报错,0 放行,上限 86400(一天)。
- 热重载传播:`applyRuntimeConfig` 中若 interval 变化,通知周期 goroutine 重读(用 `chan int` 或 atomic,取一;倾向 chan+单个 goroutine 监听,重启间隔)。
- **探测请求本身不发事件**:周期检测与手动检测共用 `CheckAll`,CheckAll 内已有结果缓存/冷却;OnStateChange 回调在结果落 status 时统一触发,所以周期/手动/单测路径都自然产生事件,无需在调度层特判。
- 手动检测保留不变。

### 3.2 不新增的配置

- vision 失败不新增任何配置(软降级是固定行为,不可配)。
- EventLog 容量、明细条数、截断长度全部硬编码,不进配置(与现有 recentErrors=8 同惯例)。

## 4. 前端设计

### 4.1 监控页「运行异常」区(index.template.html + 04/09)

位置:监控页「配置健康」区块上方(同宽度整行),标题「运行异常」。

三段内容,从上到下:

1. **图片识别摘要行**:「图片识别:N 失败 / M 张(失败率 X%)」,失败率>0 时琥珀色;无数据时灰字「暂无图片请求」。
2. **图片识别最近失败**(≤8 条):每行 时间 | 请求 ID | n/m 失败 | 大类徽章 | 截断原文。大类中文标签映射:timeout_or_cancel→超时/取消、network→网络/代理、upstream_http→上游 HTTP、response_parse→响应解析、image_format→图片格式、queue→队列、other→其他。
3. **状态事件列表**(≤100,实际展示最新 20 条,超出折叠):时间 | 事件徽章(熔断打开/已恢复/健康异常/已恢复) | Provider | Detail。恢复事件(Recovered=true)用低饱和样式。

- 空态:「暂无运行异常」绿色提示。
- 数据源:`refreshMonitor()` 已拉 `/api/metrics`,新增解析 `resp.vision` / `resp.events` 存 state(`visionSummary`、`runEvents`)。
- 徽章配色:异常类用现有 danger/warn 色系,恢复类用 good 色系但低饱和(border-good/20 bg-good/5)。

### 4.2 日志页联动(05-logs + template)

- 表格行:既有 `Vision` 徽章处,`VisionFailed>0` 时改为红色徽章「视觉 n/m 失败」(替代原来的「视觉翻译: 已启用」徽章;两者互斥,失败>0 优先)。
- 筛选下拉「状态」新增选项「图片识别失败」(value=`visionFailed`),映射到 query param `visionFailed=yes`。

### 4.3 产物重建

改 `src/` 后必须跑 `make web`(web-css + web-html),否则 `TestRepoArtifactMatchesSources` 失败;验收时单独确认 `go run ./cmd/webbuild -check` exit=0。

## 5. 关键设计决策与风险

### 5.1 熔断回调不挂在 Allow 的 lazy 转移上

half_open 转移发生在 `Allow()` 锁内(`breaker.go:136`),若回调在锁内发事件,每次请求路径都持锁做 map append。决策:只在 `Report`/`Reset`/`ResetAll` 的显式状态转变处回调(open→closed 探针成功、half_open→open、closed→open、手动复位)。open→half_open 的 lazy 转移不回调,用户从快照看当前状态即可。**代价:恢复事件只在探针成功的那次 Report 时产生,若探针从不被发(低流量),恢复事件延迟到下一次真实请求。可接受,不做轮询补偿。**

### 5.2 周期健康检测不阻塞优雅关闭

周期 goroutine 用 `context.WithCancel(baseCtx)`;main 的信号处理里 cancel 它,避免 shutdown 挂 30s。实现时给 server 增加一个 `shutdownHooks []func()` 或直接在 main 里持有 cancel(倾向后者,少一个概念)。

### 5.3 vision 秒桶计数的窗口归零问题

`Metrics()` 聚合按窗口过滤秒桶;vision 计数挂在 bucket 上天然窗口化。窗口内无任何 vision 请求时 `VisionSummary` 全零+空明细,前端显示「暂无图片请求」——这是正确语义,不做 fallback 之外的补偿。

### 5.4 metrics 包对外依赖方向

EventLog、VisionSummary 都定义在 `internal/metrics`,breaker/providerhealth 只通过注入的回调(`func(kind, provider, detail string)`)报告,不 import metrics。依赖方向:cmd/gateway → metrics,cmd/gateway → breaker/providerhealth,回调在 cmd/gateway 接线。**metrics 不 import breaker/providerhealth。**

### 5.5 Translate 签名变更的波及面

`visionRuntime.Translate` 接口、main.go 调用点(1 处)、`main_test.go` 的 spy(2446 行附近)。测试 spy 的 Result 可全零——接口方法签名一致即可。`docs/plans` 下旧文档不改。

### 5. 现有测试基线

- `internal/metrics/metrics_test.go`、`internal/vision/vision_test.go`、`cmd/gateway/main_test.go`、`internal/breaker/breaker_test.go`、`internal/providerhealth/*_test.go`、`internal/webbuild/webbuild_test.go`(产物一致性)。
- 新增测试随各任务落对应包。

## 6. 验收标准

1. 容器内 `go vet ./... && go test ./...` 全绿(验证命令带 GOPROXY 与 module cache 卷,见 TASKS.md 验证节)。
2. `go run ./cmd/webbuild -check` exit=0(容器外宿主机?—— 宿主机无 Go,容器内跑)。
3. 伪造 vision provider(错误 endpoint/401 key)发起带图请求,监控页「运行异常」区出现失败汇总+明细,请求成功率不受影响;日志页能按「图片识别失败」筛出该请求。
4. 连续失败触发熔断阈值,「运行异常」区出现 `熔断打开` 事件;探针成功后出现恢复事件;手动 Reset 也产生恢复事件。
5. 配置 `providerHealth.checkIntervalSeconds: 5` + 一个坏 provider,几秒内出现 `健康异常` 事件;恢复 provider 后(改回好上游)下个周期出现恢复事件。
6. 新配置项在 `config.example.yaml` 有注释条目;validate 负数报错。
7. 请求成功率口径不变:全图片失败+下游 200 的请求仍是「成功」,只计 vision 独立统计。

## 决策记录(与用户对齐,2026-09-08)

| # | 决策 | 选择 |
|---|------|------|
| Q1 | 提醒形态 | A 被动展示 |
| Q2 | vision 失败 vs 请求成败 | A 软降级+独立计数 |
| Q3 | 补哪些盲区 | 熔断打开 + Provider 健康异常 |
| Q4 | vision 计数单位 | A 按图片块 |
| Q5 | 展示详细程度 | A 聚合+明细 |
| Q6 | 失败分类方式 | A 固定大类+原文 |
| Q8 | 恢复是否显示 | A 显示恢复事件(低优先级) |
| Q9 | 持久化 | A 仅内存 |
| Q10 | 面板组织 | A 统一运行异常区 |
| Q11 | 周期检测 | B 可配置默认关 |
| Q12 | 明细行粒度 | A 按请求聚合 |
| Q13 | 日志页联动 | A 标记+筛选 |
| Q14 | 数据出口 | A 扩展 /api/metrics |
