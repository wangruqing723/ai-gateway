# DESIGN — 监控仪表盘错误可见性增强

> 需求目录：`docs/_archive/2026-09-08-dashboard-error-visibility/`
> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 历史共识来自 2026-09-08 grilling 会话，见文末「决策记录」。主实现为 2026-09-09 的 `2b74d3d`，本次核对基线为 `46a72c6`。
> 领域术语：见仓库根 `CONTEXT.md`（软降级、图片识别独立计数、状态变化事件、恢复事件、运行异常区、周期健康检测）。

## 最终状态说明（2026-11-01）

- vision Result / 分类、独立指标与事件环、breaker/health 回调、周期健康调度、日志筛选与运行异常区均已落地，原 TASKS 未勾选属于文档滞后。
- 当前 `WindowImages` 包含缓存命中；识别失败率的分母为 `WindowRecognized + WindowFailed`，排除缓存命中。请求成败仍看响应状态与请求 Error，视觉失败作为独立软降级。
- 运行异常区的现行布局已随后续 UI 需求调整为与延迟排行配对、自然高度加最大高度封顶；最近错误通栏，配置健康移至上方右栏。原全宽 section 只代表早期编排，不能据此判断当前实现有缺陷。
- 本会话主会话 Claude 的容器全量测试、vet、webbuild 与相关包 race 已通过，具体范围及格式既有例外见 TASKS.md 最终验收记录。本次文档子任务未重复执行，也未实跑原真实服务/浏览器故障场景。
- 尚不足的真实视觉失败全链路、周期调度启停/热重载/关闭取消、breaker/health 回调贯通 HTTP 与原烟测专项，已迁移到 `docs/KNOWN_ISSUES.md` 的「历史观测与后缀功能专项回归覆盖」，不再作为本需求归档门槛；不把这些专项写成已通过。

## 0. 背景与目标

需求提出时（2026-09-08），三类异常信号对仪表盘不可见；以下为历史背景，当前已补齐：

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
- **`cmd/gateway`**:接线(回调注入、周期 goroutine、reqLog 填充)、`/api/metrics` 响应扩展、`visionRuntime` 接口扩展。
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

`Total` 是实际处理的图片块数（Cached + Recognized + Failed）；ctx 在处理下个块前中断的未尝试图片不计入，已进入识别后返回取消/超时的图片计 Failed。

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
// 现行字段均为 omitempty，缓存/真实识别计数为后续补充
VisionImages       int    `json:"visionImages,omitempty"`       // 已处理图片块总数（含缓存）
VisionCached       int    `json:"visionCached,omitempty"`       // 缓存命中
VisionRecognized   int    `json:"visionRecognized,omitempty"`   // 非缓存识别成功
VisionFailed       int    `json:"visionFailed,omitempty"`       // 识别失败
VisionFailCategory string `json:"visionFailCategory,omitempty"` // 首个失败大类
VisionFailMessage  string `json:"visionFailMessage,omitempty"`  // 首个失败原文（已截断）
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
- 事件去重规则（原 Q7）：EventLog 不去重，breaker/providerhealth 负责判断转变。`Report` / 单项 `Reset` 只在状态实际改变时回调。**现行 `ResetAll` 有一条例外**：closed 状态但失败计数非零也进入复位回调列表（from/to 都是 closed），server 因此记录手动恢复事件；不再笼统描述成所有来源都严格只在状态转换时发一次。该边界未有专项回调断言，已移交统一回归项，本文只记录源码事实。
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
    WindowImages     int     `json:"windowImages"`     // 窗口已处理图片块总数（含缓存）
    WindowCached     int     `json:"windowCached"`     // 窗口缓存命中
    WindowRecognized int     `json:"windowRecognized"` // 窗口非缓存识别成功
    WindowFailed     int     `json:"windowFailed"`     // 窗口识别失败
    FailureRate      float64 `json:"failureRate"`      // failed/(recognized+failed)，无尝试为 0
    RecentFailed []VisionFailureEntry `json:"recentFailed"` // 日志环最近失败明细（≤8 条）
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

- vision 汇总复用 collector 当前可配置窗口的秒桶，累计 `VisionImages/VisionCached/VisionRecognized/VisionFailed`；无桶数据时按现行 fallback 从窗口内日志补算。失败率只取非缓存识别结果的 `failed/(recognized+failed)`，不按图片总数反推。
- `RecentFailed` 从日志环倒序取 `VisionFailed>0` 的记录，最多 8 条；与指标窗口独立，窗口计数归零时仍可能显示较早的最近失败。
- `Events` 来自 EventLog，倒序且最多 100 条；后续多密钥功能复用同一事件环添加 key 事件，不改变本需求的内存边界。

### 2.6 日志页筛选(LogFilter 扩展)

`LogFilter` 新增 `VisionFailed string`(值 `yes`):match 时 `VisionFailed>0` 即命中。HTTP query param `visionFailed=yes`。前端下拉新增选项「图片识别失败」。

## 3. 配置设计

### 3.1 新配置项(周期健康检测,Q11 决策 B)

```yaml
providerHealth:
  checkIntervalSeconds: 300   # 默认 0 = 关闭；>0 每 N 秒探测启用的普通 API Provider
```

- 结构:`config.Config` 新增 `ProviderHealth ProviderHealthConfig`,`ProviderHealthConfig{ CheckIntervalSeconds int }`。
- 默认 0 = 不启用(零值=未配置惯例)。validate:负数报错,0 放行,上限 86400(一天)。
- 热重载传播：`applyRuntimeConfig` 在间隔变化时调用 `restartProviderHealthLoop`，先取消旧 context，再按新值创建 ticker；0 关闭。每 tick 读取当前配置，`stopProviderHealthLoop` 在服务退出时取消请求与循环。
- 周期/手动检测共用 `CheckAll`，状态落盘到内存快照时由 Checker 统一发正常↔异常边界回调；首检异常发一次、首检正常不发，warn↔error 不发，重复状态不重复。调度层不直接生成事件。
- 后续 Provider 启用/禁用需求使整表/周期探测跳过禁用 Provider；单项显式检测保留。ChatGPT OAuth 是后续独立能力，不走普通 `/v1/models` 可用性探测。
- 手动检测保留不变。

### 3.2 不新增的配置

- vision 失败不新增任何配置(软降级是固定行为,不可配)。
- EventLog 容量、明细条数、截断长度全部硬编码,不进配置(与现有 recentErrors=8 同惯例)。

## 4. 前端设计

### 4.1 监控页「运行异常」区(index.template.html + 04/09)

当前在监控观测区首行右侧，与延迟排行形成 1/3 + 2/3；自然高度并以 `xl:max-h-[26rem]` 封顶。最近错误在次行通栏，配置健康位于上方右栏。历史初稿的「配置健康上方整行」在后续 UI 需求中调整，保留为历史方案，不作为当前缺陷。

三段内容,从上到下:

1. **图片识别摘要行**：当前拆分为「实际识别 · 缓存命中 · 失败（识别失败率 X%）」，明确缓存不进入失败率分母。无窗口图片计数时灰字「暂无图片请求」，有失败时警告色。
2. **图片识别最近失败**(≤8 条):每行 时间 | 请求 ID | n/m 失败 | 大类徽章 | 截断原文。大类中文标签映射:timeout_or_cancel→超时/取消、network→网络/代理、upstream_http→上游 HTTP、response_parse→响应解析、image_format→图片格式、queue→队列、other→其他。
3. **状态事件列表**：后端环最多 100 条，`runEventRows()` 只展示最新 20 条，不设展开全量交互；标题显示已展示/总量。时间、事件徽章、Provider 与 Detail 并列，恢复事件低饱和；当前同一环还承载后续 key 事件。

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

half_open 是 `Allow` 内的 lazy 过渡，不在此发回调。`Report` / 单项 `Reset` 在解锁后按实际转变发事件；`ResetAll` 的 closed 非零失败计数复位例外见 §2.4。恢复事件只有真实探针成功或人工复位时生成，低流量下不会轮询制造恢复事件。

### 5.2 周期健康检测不阻塞优雅关闭

周期 goroutine 的 context 由 `restartProviderHealthLoop` 从保存的 parent 派生，旧循环在间隔变更或关闭时取消；`main` 退出信号及服务异常路径调用 `stopProviderHealthLoop`。不再使用未落地的 shutdownHooks/chan 候选方案。调度取消与关闭的专项验证未找到，已迁移统一回归项。

### 5.3 vision 秒桶计数的窗口归零问题

`Metrics()` 按配置窗口过滤秒桶，窗口内无图片计数时汇总为零，前端摘要显示「暂无图片请求」。`RecentFailed` 从日志环独立取最新 8 条，故摘要无窗口样本并不保证最近失败列表为空；`TestCollectorRecentVisionFailuresUsesLogRingBeyondMetricWindow` 已锁定此行为。

### 5.4 metrics 包对外依赖方向

EventLog、VisionSummary 定义在 `internal/metrics`；breaker 的 `func(provider, fromState, toState string)` 与 providerhealth 的 `func(provider, fromStatus, toStatus string, status Status)` 回调由 server 注入并映射事件，不由两包 import metrics。`handleMetrics` 获取 Collector 的 Vision 汇总后补 EventLog 的 events。依赖方向保持 cmd/gateway → 各内部包，metrics 不 import breaker/providerhealth。

### 5.5 Translate 签名变更的波及面

`visionRuntime.Translate`、`server.handle` 与 `runtimeVisionSpy` 均已同步为 `([]any, vision.Result)`。普通 spy 可返回全零；专项接线测试使用非零 Result 锁定计数/首失败，真实失败上游贯通专项另已迁移。旧 `docs/plans` 不在本次维护范围。

### 5. 现有测试基线

- `internal/metrics/metrics_test.go`、`internal/vision/vision_test.go`、`cmd/gateway/main_test.go`、`internal/breaker/breaker_test.go`、`internal/providerhealth/*_test.go`、`internal/webbuild/webbuild_test.go`(产物一致性)。
- 新增测试随各任务落对应包。

## 6. 验收标准与最终结论

以下为原验收目标；截至 2026-11-01 的真实覆盖、未确认烟测与已迁移专项以 TASKS.md 文首为准。功能已落地、通用自动验证已通过，缺口移交统一跟踪后获准归档，不把下列真实故障场景宣称为本次全部实跑。

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
