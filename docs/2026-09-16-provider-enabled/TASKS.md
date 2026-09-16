# Provider 启用/禁用功能任务清单

> 本文是最终落地清单，与 `DESIGN.md` 对齐。早期规划稿（router 层过滤、
> 启动校验报错、vision 禁用报启动错误）已被推翻，见 DESIGN.md 各节注释。

## 后端任务

### 配置层
- [x] 配置字段定义 | 无
  - `internal/config/config.go` 的 `Provider` 结构体增加 `Enabled *bool`（`omitempty`）
  - 新增 `IsEnabled()` accessor：`nil` 默认 `true`，nil receiver 返回 `false`
  - `applyDefaults` **不物化** `Enabled`（同 `OneMContext`），`validate` 不校验该字段
  - `config.example.yaml` 注释说明 `enabled` 语义（已更正早期错误描述）

### 路由层
- [x] 路由匹配保留禁用候选 | 配置字段定义
  - `internal/router/router.go` 的 `MatchRoute` 刻意**不**过滤禁用候选
    （过滤留在候选循环，保留诊断与 503 终态）
  - `Match` 结构体新增 `VisionDisabled bool`
  - vision provider 被禁用时置 `VisionDisabled=true`、`VisionProvider=nil`、仍带 `VisionModel`

### 候选循环（替代启动校验）
- [x] 候选循环过滤禁用候选 | 路由层
  - `cmd/gateway/main.go` 的 `server.handle`：禁用检查优先于熔断，跳过不消耗 `maxAttempts`
  - 记 `AttemptDetail{Kind:"provider_disabled", Outcome:"skipped"}`
  - 终态 `503 all_candidates_disabled`（优先于 `breaker_open`），不带 retry-after
  - `printBanner` 给全禁用路由打 `⚠️ 全部候选已禁用` 警告
  - `main()` / `applyRuntimeConfig` 在队列、代理池、熔断活跃集里都排除禁用 provider
  - `handleHealth` 的 queues 排除禁用 provider
  - **未做启动校验**（故意的：禁用不该挡住网关启动，详见 DESIGN.md §3）

### vision 软降级
- [x] vision provider 禁用走运行时软降级 | 候选循环
  - `internal/vision/vision.go` 新增 `CountImages`（与 `HasImages` 同遍历深度）
  - `server.handle`：`VisionDisabled && HasImages` 时每张图片记一次识别失败
    （`VisionFailCategory="other"`）
  - **未做启动报错**（故意的：vision 是可选能力，详见 DESIGN.md §7）

### 健康检测分档
- [x] 健康检测按入口分档 | 候选循环
  - `internal/providerhealth/health.go`：`Snapshot` 与 `checkAll` 跳过禁用 provider
    （先排除 nil，避免把 nil provider 的 error 状态从快照抹掉）
  - `CheckProvider`（单行显式检测）不跳过——禁用期间确认恢复的唯一入口
  - `/v1/models` 模型列表查询不受 `enabled` 影响

## 前端任务

### Provider 表格与监控页
- [x] 配置页就地开关 | 无
  - `cmd/gateway/web/src/index.template.html`：表头加「启用」列（9→10 列）
  - 行首 checkbox `@change="toggleProvider(name)"` 就地落盘，禁用行 `opacity-50`
  - 空态 `colspan` 9→10
  - `cmd/gateway/web/src/app/11-providers.js.part`：`toggleProvider` / `addProvider` /
    `duplicateProvider` / `editProvider` / `saveProvider` 处理 enabled
- [x] 监控页禁用标记 | 配置页就地开关
  - `09-format-metrics.js.part`：`providerStateCounts().disabled`、`providerStatusRows().enabled`
    与「已禁用」状态 chip
  - `index.template.html`：`已禁用 N` 徽章、禁用行压暗、单行检测按钮保持可见
- [x] Provider 编辑弹窗开关 | 配置页就地开关
  - `index.template.html`：基本信息分区标题右侧 enabled checkbox
- [x] 路由编辑器候选后缀 | 配置页就地开关
  - `index.template.html`：候选 provider 下拉 option 文本加 `（已禁用）` 后缀
- [x] 配置归一化与预览 | 配置页就地开关
  - `02-config-normalize.js.part`、`07-nav-payload.js.part`、`10-preview-yaml.js.part`：
    enabled 的归一化、PUT 透传（false 写入、true 走 omitempty）、YAML 预览

## 测试

- [x] 后端单元测试 | 候选循环
  - `internal/config/config_test.go`：`TestProviderIsEnabled`（三态 + nil receiver）、
    `TestEnabledValidationAcceptsAllThreeStates`、
    `TestDisabledProviderStillPassesRouteAndVisionValidation`、`TestEnabledOmitEmptyYAML`
  - `internal/router/router_test.go`：`TestMatchRouteKeepsDisabledCandidates`、
    `TestMatchRouteAllCandidatesDisabledStillMatches`、
    `TestMatchRouteDisabledVisionProviderSetsFlag`、`TestMatchRouteEnabledVisionProviderNotFlagged`
  - `internal/providerhealth/health_test.go`：`TestSnapshotSkipsDisabledProviders`、
    `TestCheckAllSkipsDisabledProviders`、`TestCheckProviderStillChecksDisabledProvider`
  - `cmd/gateway/main_test.go`：`TestHandleSkipsDisabledCandidateWithoutConsumingAttempts`、
    `TestHandleAllCandidatesDisabledReturns503`、`TestHandleDisabledVisionProviderDegradesSoftly`

## 验收

- [x] webbuild 产物重建并 `-check` 通过（`index.html` 与源码一致）
- [x] `gofmt -l ./cmd ./internal` 无输出
- [x] `go vet ./...` 通过
- [x] `go test ./...` 全量通过（含上述新增用例）
- [ ] 端到端验证（本地启动、前端 toggle、响应头确认）— 留给用户在本地环境跑

## 文档

- [x] DESIGN.md 与代码对齐（推翻了 router 过滤 / 启动报错 / vision 报错三处）
- [x] TASKS.md 与代码对齐（清理虚假 [x] 与损坏尾部）
- [x] config.example.yaml 注释更正（早期「启动会报错」描述已改为运行时 503）
- [x] CONTEXT.md 已在规划阶段加「已禁用 Provider」术语定义
