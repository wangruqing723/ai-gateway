# TASKS — 监控仪表盘错误可见性增强

> 设计依据:`DESIGN.md`(本目录)。术语:根 `CONTEXT.md`。
> 全部任务完成后统一验证,验证命令见文末。

## 任务清单

- [ ] T1 vision 分类与 Result | P0 | 1h | 无
- [ ] T2 RequestLog/秒桶 vision 字段 | P0 | 1h | T1
- [ ] T3 Translate 接线到 reqLog | P0 | 0.5h | T1, T2
- [ ] T4 EventLog 与熔断回调 | P0 | 1.5h | 无
- [ ] T5 健康回调与周期检测 | P0 | 1.5h | 无
- [ ] T6 /api/metrics 扩展 | P0 | 1h | T2, T4
- [ ] T7 日志筛选 visionFailed | P1 | 0.5h | T2
- [ ] T8 前端运行异常区 | P0 | 2h | T6
- [ ] T9 前端日志页联动 | P1 | 0.5h | T7
- [ ] T10 config.example + 文档 | P1 | 0.5h | T5
- [ ] T11 容器内全量验证 | P0 | 1h | T1-T10

## T1 vision 分类与 Result

**目标**:`internal/vision` 能把失败情况结构化带出来,并给错误定大类。

**改动**:
1. 新增 `internal/vision/result.go`(或并入 vision.go):`Result`、`Failure` 结构体与 `ClassifyFailure(err) string`,大类 slug 与判据严格按 DESIGN 2.2 的表;错误原文截断 200 字符。
2. `Translate` 签名改 `([]any, Result)`:统计 `Total/Cached/Recognized/Failed`,`Failed>0` 时填 `FirstFailure`(分类 + 截断原文)。现有 `stats` 结构体保留,Result 由 stats + 首失败拼出。
3. `processBlocks` 的失败分支(vision.go:186-190)不变——软降级占位文字照旧,只是同时把 err 传给 Result 收集(通过给 processBlocks/callVision 传 `*Result` 或在 stats 里加 firstFailure 字段,任选,保持内部简洁)。
4. ctx 中断(processBlocks 顶部 `ctx.Err() break`)不算失败不计总数——那些图片根本没尝试。现有 st.skipped 字段实际无递增点,可顺手删除或保留,不做强求,但 Result 不引入 skipped。

**验收**:`vision_test.go` 新增用例——错误 endpoint / 非 2xx / 坏 source 各断言 Result 字段与分类 slug;既有测试改签名后全绿。

## T2 RequestLog/秒桶 vision 字段

**目标**:metrics 包能存、能窗口聚合 vision 统计。

**改动**(`internal/metrics/metrics.go`):
1. `RequestLog` 追加 `VisionImages/VisionFailed/VisionFailCategory/VisionFailMessage`(DESIGN 2.3 的 JSON tag,omitempty,加在既有字段后)。
2. `metricBucket` 增加 `visionImages/visionFailed int`;`addMetricLocked` 累加;窗口聚合在 `Metrics()` 里加 VisionSummary(含 fallback:providerTotals 为空时扫日志环,同现有 fallback 模式,DESIGN 2.5)。
3. `VisionSummary`、`VisionFailureEntry` 结构体(DESIGN 2.5);`RecentFailed` 倒序取 `VisionFailed>0` 记录 ≤8 条。
4. `isSuccess` 不动——vision 字段不影响成败判定。

**验收**:`metrics_test.go` 新增:构造带 vision 字段的 Add 后 Metrics() 的窗口聚合/fallback/RecentFailed 截断正确;`VisionFailed>0 && Status=200` 仍是 success。

## T3 Translate 接线到 reqLog

**目标**:main.handle 把 Result 落进 reqLog。

**改动**(`cmd/gateway/main.go`):
1. `visionRuntime` 接口(main.go:155-158)的 `Translate` 返回值改双值。
2. main.go:511-516 调用点:`internal.Messages, vres := s.translator.Translate(...)`,把 Total/Failed/首失败写进 reqLog 四字段。
3. `main_test.go` 的 `runtimeVisionSpy`(2446 行附近)同步改签名。

**验收**:容器内 `go build ./...` 过;main_test 全绿。

## T4 EventLog 与熔断回调

**目标**:熔断状态转变产生事件。

**改动**:
1. `internal/metrics` 新增 `Event`、`EventLog`(容量 100 环形,DESIGN 2.4),`Add(kind, provider, detail)`;`Events()` 倒序返回。
2. `internal/breaker`:`Settings` 或 `New` 增加 `OnStateChange func(provider, fromState, toState string)`(nil 安全);在 `Report` 的 closed→open、half_open→open、探针成功回 closed 处,以及 `Reset`/`ResetAll` 复位处回调。**Allow 里的 open→half_open lazy 转移不回调**(DESIGN 5.1)。回调在锁外调用(先在锁内记录转变,解锁后发),避免回调阻塞状态机。
3. main.go 接线:构造 `metrics.NewEventLog()`;回调里 `breaker_open` / `breaker_recovered`(from=half_open 探测成功或手动 Reset,detail 注明「连续失败 N 次后打开」/「探针成功」/「手动复位」)。熔断禁用时(Enabled=false)不注册回调事件——`SetSettings` 禁用时清空全部状态(既有行为),此时状态已清,不产生恢复事件,可接受。

**验收**:`breaker_test.go` 新增回调触发/nil 安全/锁外调用用例;metrics EventLog 环形覆盖测试。

## T5 健康回调与周期检测

**目标**:健康状态转变产生事件;支持配置驱动的周期自动检测。

**改动**:
1. `internal/providerhealth`:`Checker` 增加 `OnStateChange func(provider, fromStatus, toStatus string, s Status)`(nil 安全);在 `CheckAll`/`CheckProvider` 落 statuses 的地方,对 `ok→error/warn`、`error/warn→ok` 的转变触发回调。`unchecked` 视为非事件源(首检从 unchecked→ok 不发事件,unchecked→error/warn 发)。warn↔error 之间互转不发(同向异常内部波动,DESIGN 决策:只看正常↔异常边界)。回调在锁外调用。
2. `internal/config`:`Config.ProviderHealth ProviderHealthConfig{CheckIntervalSeconds int}`;`applyDefaults` 不填默认(0=关);`validate`:负数或 >86400 报错。`config.example.yaml` 加注释条目。
3. `cmd/gateway` main.go:启动时若 interval>0,起 goroutine `time.Ticker` 循环:每 tick 调 `s.providerHealth.CheckAll(ctx, cfg, s.resolveClient)`(cfg 取当前 `s.cfg`,周期内热重载生效);ctx 由 main 的 cancel 派生,优雅关闭时停止。`applyRuntimeConfig` 里 interval 变化时通知 goroutine(实现取一:重启 goroutine 或 chan 下发新值;倾向 stop chan + 重建,逻辑最直白)。
4. main.go 接线 OnStateChange:`health_down`(进入 error 或 warn,detail 含 HTTPCode/消息)与 `health_recovered`。

**验收**:`providerhealth` 测试新增状态转变回调/nil 安全;config 测试新增 validate 边界;手动 CheckAll 两次同状态不重复发事件。

## T6 /api/metrics 扩展

**目标**:响应带 vision 与 events 段。

**改动**:
1. `metrics.Response` 加 `Vision VisionSummary json:"vision,omitempty"`、`Events []Event json:"events,omitempty"`。
2. `Collector.Metrics()` 的调用点在 cmd/gateway:`handleMetrics` 改为组装——`resp := s.metrics.Metrics(now); resp.Vision = ...; resp.Events = s.eventLog.Events()`。Vision 的 RecentFailed 部分在 Collector.Metrics 里已算(挂 Response.Vision),Events 由 server 层拼(DESIGN 5.4 依赖方向:EventLog 在 metrics 包,但实例由 server 持有)。
3. server struct 新增 `eventLog *metrics.EventLog` 字段。

**验收**:handler 测试断言新 JSON 段;空数据时 vision 段为空对象或省略(omitempty 语义一致即可)。

## T7 日志筛选 visionFailed

**改动**(`internal/metrics/metrics.go` + main.go handleLogs):
1. `LogFilter.VisionFailed string`;`match()`:`f.VisionFailed=="yes" && r.VisionFailed>0`。
2. `handleLogs` 解析 query param `visionFailed`。

**验收**:metrics match 单测;既有筛选不受影响。

## T8 前端运行异常区

**改动**(`cmd/gateway/web/src/`):
1. `index.template.html`:监控页「配置健康」上方新增整行 section「运行异常」,结构按 DESIGN 4.1(摘要行 / 最近失败 ≤8 / 事件列表最新 20 条可滚动)。
2. `04-monitor.js.part`:`refreshMonitor` 里 `loadMetrics()` 解析 `resp.vision`/`resp.events` 存 state(`visionSummary`、`runEvents`)。
3. `09-format-metrics.js.part`:新增 `visionCategoryLabel(slug)` 中文映射、事件徽章样式函数(恢复事件低饱和)。
4. 空态与骨架屏对齐现有区块风格(参考最近错误的 skeleton 写法)。
5. **产物重建**:`make web`(web-css 因新增 class 需重跑 tailwind;web-html 重拼 index.html)。

**验收**:容器内 `go run ./cmd/webbuild -check` exit=0;手动浏览验证布局。

## T9 前端日志页联动

**改动**:
1. `index.template.html` 日志页状态筛选下拉加 `<option value="visionFailed">图片识别失败</option>`。
2. `05-logs.js.part`:筛选值映射到 query param `visionFailed=yes`;其余值走原 `status` 参数。
3. 日志表格行:`VisionFailed>0` 时显示红色徽章「视觉 n/m 失败」,否则保留原「视觉」徽章逻辑。
4. `make web` 重建产物。

**验收**:同 T8,`webbuild -check` exit=0。

## T10 config.example + 文档

1. `config.example.yaml` 补 `providerHealth.checkIntervalSeconds` 注释条目(默认 0=关闭)。
2. 若 CLAUDE.md 的「Key Mechanisms / Request Flow」需要提及周期健康检测与运行异常区,补一句(保守,不展开)。

## T11 容器内全量验证

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.23-alpine sh -c 'gofmt -l ./cmd ./internal && go vet ./... && go test ./...'
```

(容器内也要跑 `go run ./cmd/webbuild -check`。gofmt -l 应无输出。)

附加验收(DESIGN 第 6 节的 3-5 条,需真实跑服务):
- 伪造 vision provider 验证运行异常区显示;
- 触发熔断阈值验证事件;
- `checkIntervalSeconds: 5` 验证周期健康事件。
(如本地环境不便,可降级为 HTTP 边界测试覆盖同等断言,验收时说明。)

## 验证命令(GOPROXY 说明)

容器内验证必须带 `-e GOPROXY=https://goproxy.cn,direct` 与 module cache 卷 `ai-gateway-gomod`,否则 modernc.org/sqlite 拉取超时(2026-09-04 实测)。
