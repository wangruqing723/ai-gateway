# TASKS — 监控仪表盘错误可见性增强

> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 设计依据：`DESIGN.md`（本目录）。术语：根 `CONTEXT.md`。
> 主实现已于 2026-09-09 提交：`2b74d3d`；本次以 `46a72c6` 的代码与现有测试为准补正陈旧清单，不重新实现。

## 最终完成与验收记录（2026-11-01）

- T1–T10 的功能与配置链路均已落地；此前 T1–T11 全部未勾选是文档状态滞后，不代表功能未实现。
- 本会话主会话 Claude 已在 `golang:1.27-alpine` 容器确认 `webbuild -check`、`go vet ./...` 退出 0，`go test -count=1 ./...` 全部通过；`golang:1.27` 下 converter/gateway/metrics/providerhealth/router/proxy/keypool/vision 的 race 全部通过。本次文档子任务引用这些真实验证记录，未重复运行。
- 格式检查唯一既有例外是 `internal/breaker/breaker.go`，与本需求及此次 Markdown 维护无关，未顺带修改；不再写成全仓库 `gofmt -l` 无输出。
- 当前图片识别失败率为 `WindowFailed / (WindowRecognized + WindowFailed)`，分母排除缓存命中；`WindowImages` 仍包含缓存命中。请求成功率继续独立判定。
- 运行异常区已随后续 UI 改造调整位置与高度：当前与延迟排行配对，使用自然高度和最大高度封顶，最近错误通栏，配置健康在上方右栏。早稿的全宽 section 不是当前验收约束。

### 已核对的专项覆盖

| 范围 | 当前真实测试覆盖 |
|---|---|
| vision 分类与结果 | `TestClassifyFailureUsesStableCategories`、`TestTranslateReturnsStructuredFailureResult`：稳定分类，401 / 坏 JSON / 坏 source 的 Result 与首失败摘要 |
| vision 聚合与筛选 | `TestCollectorMetricsAggregatesVisionAndKeepsRequestSuccessIndependent`、`TestCollectorVisionFailureRateExcludesCacheHitsFromDenominator`、`TestCollectorMetricsVisionFallbackScansLogRing`、`TestCollectorRecentVisionFailuresUsesLogRingBeyondMetricWindow`、`TestCollectorLogsFiltersByVisionFailure` |
| 事件与回调 | `TestEventLogKeepsNewestHundredEvents`、`TestStateChangeCallbackIsOutsideBreakerLockAndSkipsLazyHalfOpen`、`TestResetCallbackOnlyReportsExplicitStateTransition`、`TestStateChangeCallbackOnlyReportsNormalAbnormalBoundaries`、`TestCheckAllReportsUncheckedAbnormalOnce` |
| 配置与 HTTP 边界 | `TestDecodeAndValidateProviderHealthInterval`、`TestMetricsAndLogsExposeVisionFailuresAndEvents`、`TestHandleStoresVisionResultWithoutChangingRequestSuccess`（最后一个使用 vision spy，并非真实失败上游的全链路测试） |

### 已迁移的专项回归遗留

专项回归覆盖不足迁移到 `docs/KNOWN_ISSUES.md` 的「历史观测与后缀功能专项回归覆盖」，不再留作本需求归档门槛。

- [x] 已迁移：错误 endpoint 经真实 `Translate` 返回结构化 Result，以及真实视觉失败贯通请求日志与 `/api/metrics` 的专项回归；现有分类单测和 vision spy 接线测试不等同于该覆盖。
- [x] 已迁移：周期健康调度启停、间隔热重载、关闭取消，以及真实 breaker/health 回调贯通 EventLog 与管理 HTTP API 的专项回归；现有包内状态机/回调测试与手工向 EventLog 加事件的 HTTP 测试不等同于该覆盖。`ResetAll` 对 closed 状态非零失败计数的复位回调也需专项锁定现行行为。
- [x] 已迁移：原附加验收的伪造 vision 上游、熔断打开/恢复/手动复位、5 秒周期健康异常/恢复的服务与浏览器烟测记录。本次未实跑或确认这些场景，不虚构通过。

以上 `[x]` 只表示遗留项已移交统一跟踪，不表示专项测试已经补齐或烟测已经通过。

## 任务清单

- [x] T1 vision 分类与 Result | P0 | 1h | 无
- [x] T2 RequestLog/秒桶 vision 字段 | P0 | 1h | T1
- [x] T3 Translate 接线到 reqLog | P0 | 0.5h | T1, T2
- [x] T4 EventLog 与熔断回调 | P0 | 1.5h | 无
- [x] T5 健康回调与周期检测 | P0 | 1.5h | 无
- [x] T6 /api/metrics 扩展 | P0 | 1h | T2, T4
- [x] T7 日志筛选 visionFailed | P1 | 0.5h | T2
- [x] T8 前端运行异常区 | P0 | 2h | T6
- [x] T9 前端日志页联动 | P1 | 0.5h | T7
- [x] T10 config.example + 文档 | P1 | 0.5h | T5
- [x] T11 容器内全量验证 | P0 | 1h | T1-T10

## 历史实施分解（现行符号与验收口径已补正）

以下保留原 T1–T11 的目标与实施范围；完成状态、测试实际覆盖与已迁移项以文首最终记录为准，不构成新的未完成清单。

## T1 vision 分类与 Result

**目标**:`internal/vision` 能把失败情况结构化带出来,并给错误定大类。

**改动**:
1. `internal/vision/result.go` 已定义 `Result` / `Failure` 与 `ClassifyFailure`，按稳定大类返回 slug，摘要按 200 rune 截断。
2. `Translate` 已返回 `([]any, Result)`，从 stats 汇总 Total/Cached/Recognized/Failed；有失败时复制 FirstFailure。
3. `processBlocks` 在失败时保留软降级占位文本，并在 `stats.firstFailure` 只收集首条分类/摘要；不再保留传 `*Result` / 合并文件等未采用的备选。
4. ctx 在 `processBlocks` 开始处理下个块前中断时，未尝试的图片不计 Total/Failed；已进入 `callVision` 后因取消/超时返回错误的图片计失败。`stats.skipped` 仍保留，Result 不引入 skipped。

**验收记录**：`vision_test.go` 已有稳定分类及 401 / 坏 JSON / 坏 source 的 Result 用例，随本会话全量测试通过；错误 endpoint 的真实 Result 专项原承诺未找到，已迁移，见文首。

## T2 RequestLog/秒桶 vision 字段

**目标**:metrics 包能存、能窗口聚合 vision 统计。

**改动**(`internal/metrics/metrics.go`):
1. `RequestLog` 已有 `VisionImages/VisionCached/VisionRecognized/VisionFailed/VisionFailCategory/VisionFailMessage`（omitempty），后续补充缓存命中与真实识别数。
2. `metricBucket` 同步累计 images/cached/recognized/failed；窗口汇总与无桶数据时的日志环 fallback 都带上四项。失败率为 `failed/(recognized+failed)`，缓存命中不参与分母。
3. `VisionSummary`、`VisionFailureEntry` 已落地；`RecentFailed` 从日志环倒序取 `VisionFailed>0` 的记录，最多 8 条，明细不受指标窗口限制。
4. `isSuccess` 不变：Error 为空且 200≤Status<400 才算成功，vision 字段不改变请求成败。

**验收记录**：文首列出的聚合/分母/窗口/fallback/截断/筛选用例均存在并随全量测试通过。

## T3 Translate 接线到 reqLog

**目标**:main.handle 把 Result 落进 reqLog。

**改动**(`cmd/gateway/main.go`):
1. `visionRuntime.Translate` 已改为 `([]any, vision.Result)`。
2. `server.handle` 接收 Result，写入 images/cached/recognized/failed 与首失败分类/摘要。
3. `runtimeVisionSpy` 已同步签名。

**验收记录**：`TestHandleStoresVisionResultWithoutChangingRequestSuccess` 用 spy 验证接线与请求成功口径，全量测试通过；真实失败 vision 上游贯通请求/API 的专项已迁移。

## T4 EventLog 与熔断回调

**目标**:熔断状态转变产生事件。

**改动**:
1. `internal/metrics` 新增 `Event`、`EventLog`(容量 100 环形,DESIGN 2.4),`Add(kind, provider, detail)`;`Events()` 倒序返回。
2. `internal/breaker.Settings.OnStateChange` 已注入锁外回调；`Report` 的 closed→open、half_open→open、half_open→closed 与单项 `Reset` 的实际状态改变回调，`Allow` 的 lazy open→half_open 不回调。`ResetAll` 还会对 closed 但失败计数非零的项发复位回调，现行边界见 DESIGN §2.4。
3. server 持有 `metrics.NewEventLog()`，`onBreakerStateChange` 映射打开/恢复事件，区分探针成功与手动复位。`breakerSettingsWithCallback` 始终带回调；Enabled=false 时 `Report` 不处理结果，`SetSettings` 禁用会清空状态且不发恢复回调，不是「禁用时未注册回调」。

**验收记录**：锁外回调与 lazy half_open 不发事件、单项 Reset 实际转变、EventLog 环形覆盖测试均已存在；真实状态机贯通 EventLog/HTTP 与 `ResetAll` 特殊复位回调专项已迁移，见文首。

## T5 健康回调与周期检测

**目标**:健康状态转变产生事件;支持配置驱动的周期自动检测。

**改动**:
1. `internal/providerhealth`:`Checker` 增加 `OnStateChange func(provider, fromStatus, toStatus string, s Status)`(nil 安全);在 `CheckAll`/`CheckProvider` 落 statuses 的地方,对 `ok→error/warn`、`error/warn→ok` 的转变触发回调。`unchecked` 视为非事件源(首检从 unchecked→ok 不发事件,unchecked→error/warn 发)。warn↔error 之间互转不发(同向异常内部波动,DESIGN 决策:只看正常↔异常边界)。回调在锁外调用。
2. `internal/config`:`Config.ProviderHealth ProviderHealthConfig{CheckIntervalSeconds int}`;`applyDefaults` 不填默认(0=关);`validate`:负数或 >86400 报错。`config.example.yaml` 加注释条目。
3. `cmd/gateway` 已通过 `startProviderHealthLoop` / `restartProviderHealthLoop` / `stopProviderHealthLoop` 管理 ticker goroutine。每 tick 读取当前配置调用 `CheckAll`；间隔变更先 cancel 旧循环再启动，0 关闭，服务退出取消当前循环；不再保留「实现取一」的未决方案。
4. main.go 接线 OnStateChange:`health_down`(进入 error 或 warn,detail 含 HTTPCode/消息)与 `health_recovered`。

**验收记录**：包内状态边界/锁外回调、首检异常只发一次和 config 间隔边界测试存在并通过；周期调度启停、间隔热重载、退出取消及真实回调贯通 HTTP 的专项已迁移，不能用通用热重载测试代替。

## T6 /api/metrics 扩展

**目标**:响应带 vision 与 events 段。

**改动**:
1. `metrics.Response` 加 `Vision VisionSummary json:"vision,omitempty"`、`Events []Event json:"events,omitempty"`。
2. `Collector.Metrics()` 直接算出 `Response.Vision`；`handleMetrics` 只在取指标后补上 `s.eventLog.Events()`，不需要 server 层再计算 Vision。
3. server struct 新增 `eventLog *metrics.EventLog` 字段。

**验收记录**：`TestMetricsAndLogsExposeVisionFailuresAndEvents` 断言新增 vision/events 与日志筛选；此用例直接填指标和 EventLog，不是实际状态机事件贯通测试。

## T7 日志筛选 visionFailed

**改动**(`internal/metrics/metrics.go` + main.go handleLogs):
1. `LogFilter.VisionFailed string`;`match()`:`f.VisionFailed=="yes" && r.VisionFailed>0`。
2. `handleLogs` 解析 query param `visionFailed`。

**验收**:metrics match 单测;既有筛选不受影响。

## T8 前端运行异常区

**改动**(`cmd/gateway/web/src/`):
1. `index.template.html` 已有「运行异常」区，含图片识别摘要、最近失败 ≤8 与状态事件最近 20 条。原方案位于配置健康上方整行；当前位置与自然高度按后续 UI 演进，见文首，不再按旧全宽编排验收。
2. `04-monitor.js.part`:`refreshMonitor` 里 `loadMetrics()` 解析 `resp.vision`/`resp.events` 存 state(`visionSummary`、`runEvents`)。
3. `09-format-metrics.js.part`:新增 `visionCategoryLabel(slug)` 中文映射、事件徽章样式函数(恢复事件低饱和)。
4. 空态与骨架屏对齐现有区块风格(参考最近错误的 skeleton 写法)。
5. **产物重建**:`make web`(web-css 因新增 class 需重跑 tailwind;web-html 重拼 index.html)。

**验收记录**：本会话容器 `webbuild -check` 退出 0；原伪造故障服务与浏览器展示专项记录已迁移，不声称本次手动浏览验证通过。

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

## T11 容器内全量验证（现行重跑方式）

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27-alpine sh -c 'go run ./cmd/webbuild -check && go vet ./... && go test -count=1 ./...'

# -l 有输出仍可退出 0，必须单独看输出；本次既有 breaker.go 例外见文首。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work golang:1.27-alpine gofmt -l ./cmd ./internal
```

原附加验收包括伪造 vision provider 的运行异常显示、熔断打开/恢复/手动复位事件、5 秒周期健康异常/恢复。未确认这些服务/浏览器烟测已实跑；现有 HTTP 测试只覆盖部分等价断言，未覆盖部分已统一迁移，见文首，不再作为当前归档门槛。

## 验证命令（GOPROXY 说明）

现行 Go 验证使用 `golang:1.27-alpine`；race 使用含 gcc 的 `golang:1.27` Debian 镜像，不在 Alpine 临时装编译器。容器内验证带 `-e GOPROXY=https://goproxy.cn,direct` 与 module cache 卷 `ai-gateway-gomod`，避免直连依赖源超时。原 2026-09-04 的 modernc.org/sqlite 下载超时记录保留；更新镜像与重跑命令不改写历史测试日期。

## 最终归档状态（2026-11-02）

- 已按使用者授权移入 `docs/_archive/2026-09-08-dashboard-error-visibility/`，需求创建日期保留。
- 文档按当前代码和使用者已确认的验收同步，历史失败或阶段记录不再表示当前归档阻断；仍未修复的缺陷和专项测试欠账保留在 [统一已知问题](../../KNOWN_ISSUES.md)。
- 本次仅维护和移动文档，没有修改业务代码、真实配置或账户，没有提交、合并或推送。
