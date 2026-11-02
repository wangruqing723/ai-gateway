# Provider 启用/禁用功能任务清单

> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 本文是最终落地清单，与 `DESIGN.md` 对齐。主实现提交为 2026-09-16 的 `72ed47c`，本次核对基线为 `46a72c6`。
> 早期规划稿（router 层过滤、启动校验报错、vision 禁用报启动错误）已被推翻；历史稿位于 `docs/_archive/2026-09-16-provider-enable-disable/`，仅作历史记录，不是当前验收规范；早稿只同步最终稿的归档路径，不改动其历史设计。

## 最终完成与验收记录（2026-11-01）

- 2026-11-01 使用者确认此前验收完成：前端启用/禁用开关、保存生效与实际响应头的端到端验证。此项为使用者确认，不是本次文档维护重新启动服务或实跑浏览器。
- 当前列表 checkbox 通过 `x-model="provider.enabled"` 改内存，之后由顶部保存或未保存浮层统一写盘并热重载；没有 `toggleProvider(name)` 立即写盘路径。
- 当前禁用热重载会从 breaker 活跃集中移除 Provider 并删除其状态；重新启用拿到干净的熔断计数与冷却，不承诺保留禁用前状态。
- 本会话主会话 Claude 在 `golang:1.27-alpine` 容器确认 `webbuild -check`、`go vet ./...` 退出 0，`go test -count=1 ./...` 全部通过；`golang:1.27` 下 converter/gateway/metrics/providerhealth/router/proxy/keypool/vision 的 race 全部通过。
- 格式检查唯一既有例外是 `internal/breaker/breaker.go`，与本需求及本次 Markdown 修改无关，未顺带修整；不再记成全仓库 gofmt 无输出。
- 原唯一未勾的端到端门槛已由使用者确认关闭，本目录没有剩余人工验收门槛。

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
  - `main()` 初始化队列时排除禁用 provider；`applyRuntimeConfig` 的 queue / HTTP 代理池 / breaker 活跃集统一排除禁用 provider，breaker 状态删除后再启用为干净状态。
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
- [x] 配置页列表开关 | 无
  - `cmd/gateway/web/src/index.template.html`：启用列为 10 列表格的第一列，空态 colspan 同为 10。
  - checkbox 实际为 `x-model="provider.enabled"`，只修改内存；顶部保存或未保存浮层调用 `saveConfig()` → `persistConfig()` → `configPayload()` → `PUT /api/config`，写盘热重载后才生效。
  - 旧稿的 `@change="toggleProvider(name)"` 立即落盘没有在当前源码存在，不以它验收。禁用行 `opacity-50`，标题明确「保存后生效」。
  - `11-providers.js.part` 的 `addProvider` / `duplicateProvider` / `editProvider` / `saveProvider` 保留 enabled；新增/复制默认启用，编辑保留原状态，false 显式保存。
- [x] 监控页禁用标记 | 配置页列表开关
  - `09-format-metrics.js.part`：`providerStateCounts().disabled`、`providerStatusRows().enabled`
    与「已禁用」状态 chip
  - `index.template.html`：`已禁用 N` 徽章、禁用行压暗、单行检测按钮保持可见
- [x] Provider 编辑抽屉开关 | 配置页列表开关
  - `index.template.html`：基本信息分区标题右侧 enabled checkbox
- [x] 路由编辑器候选后缀 | 配置页列表开关
  - `index.template.html`：候选 provider 下拉 option 文本加 `（已禁用）` 后缀
- [x] 配置归一化与预览 | 配置页列表开关
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
- [x] 当前格式检查已记录：唯一既有例外为 `internal/breaker/breaker.go`，与此需求无关，未顺带修改
- [x] `go vet ./...` 通过
- [x] `go test ./...` 全量通过（含上述新增用例）
- [x] 端到端验证（本地启动、前端启用/禁用开关与保存生效、响应头确认）—— 2026-11-01 使用者确认此前验收完成；不是本次文档维护实跑

## 文档

- [x] DESIGN.md 与代码对齐（推翻了 router 过滤 / 启动报错 / vision 报错三处）
- [x] TASKS.md 与代码对齐（清理虚假 [x] 与损坏尾部）
- [x] config.example.yaml 注释更正（早期「启动会报错」描述已改为运行时 503）
- [x] CONTEXT.md 已在规划阶段加「已禁用 Provider」术语定义

## 最终归档状态（2026-11-02）

- 已按使用者授权移入 `docs/_archive/2026-09-16-provider-enabled/`，需求创建日期保留。
- 文档按当前代码和使用者已确认的验收同步，历史失败或阶段记录不再表示当前归档阻断；仍未修复的缺陷和专项测试欠账保留在 [统一已知问题](../../KNOWN_ISSUES.md)。
- 本次仅维护和移动文档，没有修改业务代码、真实配置或账户，没有提交、合并或推送。
