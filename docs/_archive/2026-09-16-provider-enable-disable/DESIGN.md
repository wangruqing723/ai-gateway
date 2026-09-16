# Provider 启用/禁用 — 设计文档

> 状态：已与用户对齐（Q1–Q14 全部拍板）。本文件是委托 Codex 实现的依据。
> 决策记录见文末「决策摘要」。
>
> **归档说明（2026-09-16）**：本文件是第一版设计稿，保留 Q1–Q14 的决策记录。
> 落地实现以 `docs/2026-09-16-provider-enabled/` 为准（该目录与代码对齐）。

## 1. 需求

给每个 Provider 加一个启用/禁用开关。禁用后该 Provider 不再参与请求转发（包括 vision 翻译），但保留在配置中、UI 仍可见，随时可重新启用。核心场景：临时下线维护、上游挂了、key 过期——不想删配置、改天再开。

## 2. 语义（Q1 = A2，软停用）

- 禁用 = **人工声明该 Provider 不参与转发**。它不是探测结果、不是失败计数，不随时间自动恢复。
- 禁用**不影响启动**：`config.validate` 不因「Provider 被禁用」「全部 Provider 被禁用」「路由唯一目标被禁用」而报错（Q14）。
- 禁用**不影响保存**：`PUT /api/config` 不会因上述情况拒绝。
- 被禁用的 Provider 在候选循环里被**静默剔除候选**（非报错），与熔断跳过同族但在熔断判断之前。

与既有概念的边界（见 `CONTEXT.md`）：
- **已禁用**：人工声明，不自动恢复。
- **熔断打开**：自动触发、冷却后自恢复。
- **健康异常**：探测观察到的状态。
三者无蕴含关系，可叠加。

## 3. 字段定义（Q3 = B1）

`internal/config/config.go` 的 `Provider` 结构体新增字段：

```go
// Enabled 为 nil 表示启用（默认），false 表示已禁用，true 显式启用。
// 用 *bool：Go 的 bool 零值是 false，直接用值类型会让所有存量配置里没写该字段
// 的 Provider 加载后全部变成"已禁用"，是破坏性变更。nil 承载"默认启用"，
// applyDefaults 不物化它（对齐 OneMContext 的处理），避免 PUT 保存给每个
// Provider 都写进 enabled: true。
Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
```

新增 accessor（字段名 `Enabled` 与方法名冲突，Go 不允许，故用 `IsEnabled`）：

```go
// IsEnabled 返回该 Provider 是否启用；nil 视为启用（默认）。
func (p *Provider) IsEnabled() bool { return BoolOr(p.Enabled, true) }
```

`BoolOr` 已存在于 `config.go:253`。`applyDefaults` **不**触碰该字段（对齐 `OneMContext` 注释 `config.go:71-73` 的"避免写回配置文件"）。

`validate` / `validateProvider` 对 `enabled` 不做任何校验——`true`/`false`/nil 都是合法终态。

## 4. 后端改动点

### 4.1 候选剔除（Q13 = H2，落在 `main.go` 候选循环）

**不**在 `router.MatchRoute` 剔除候选（H1 会让候选空时静默 `continue` 到下一条路由、可能落到 catch-all，无诊断）。落在 `cmd/gateway/main.go` 的候选循环，与熔断跳过同族。

位置：`main.go:681` 熔断判断 `if s.breaker != nil && !s.breaker.Allow(name)` **之前**插入禁用判断（禁用是人工声明，优先级高于自动熔断）：

```go
// 伪代码，不是实现代码
if !candidate.Provider.IsEnabled() {
    disabledSkips++
    trail = append(trail, name+":provider_disabled")
    reqLog.AttemptTrail = append(reqLog.AttemptTrail, metrics.AttemptStep{
        Provider:   name,
        Model:      candidate.TargetModel,
        Outcome:    "skipped",
        ErrorType:  "provider_disabled",
        Error:      "provider 已禁用，跳过该候选",
    })
    continue
}
```

`disabledSkips` 是新增计数器，与现有 `breakerSkips`/`contextSkips`/`freeSkips` 同族，在 `main.go:664` 附近的 `var (...)` 块里声明。

终态 switch（`main.go:825` 起）在最前加一个分支，优先级 **disabled > breaker > rateLimited > context > buildErr > 兜底**：

```go
// 伪代码
case attempts == 0 && disabledSkips > 0:
    reqLog.AttemptTrail = strings.Join(trail, " → ")
    reqLog.Error = "全部候选上游均已禁用"
    writeJSONError(w, http.StatusServiceUnavailable, "all_candidates_disabled", reqLog.Error)
```

503（非 400/500）：与熔断一致，这是"服务端暂时不可用"，成因在网关配置侧，不是客户端请求问题。**不带 `retry-after`**：人工禁用没有恢复时间，给假数字会误导客户端重试。

### 4.2 Vision 剔除（Q10 = F1，落在 `MatchRoute`）

Vision 不走候选循环，唯一落点是 `internal/router/router.go:84`：

```go
// 伪代码：现有 vSrc != nil 判断旁边加 enabled 检查
if vSrc := cfg.Providers[route.Vision.Provider]; vSrc != nil && vSrc.IsEnabled() {
    ...
}
```

VisionProvider 为 nil → `handle()` 的 `needVision` 判断（`main.go:585`）整段跳过图片翻译 → 请求带原始图片块打到目标模型。

**计入图片识别失败（Q10 b）**：在 `handle()` 里 vision 结果产出后，若 `matched.VisionProvider != nil` 但因禁用未翻译（需要区分"路由没配 vision"和"配了但被禁用"）。具体：`MatchRoute` 设 `m.VisionProvider` 时若 vision 配置存在但 provider 被禁用，在 `Match` 里加一个标记字段 `VisionDisabled bool`，`handle()` 据此在图片请求路径上记一次图片识别失败（`visionResult.FirstFailure` 机制或直接写 `reqLog.VisionFailed`，按 `internal/vision` 现有失败分类填 `category`，建议归类"其他"并带"vision provider 已禁用"消息）。Codex 实现时核对 `internal/vision` 的失败结构体字段，保持与既有软降级记账一致。

> 注：`Match` 结构体（`router.go:28`）新增 `VisionDisabled bool` 字段。这是 router 层唯一新增字段，不改 `Candidates` 契约。

### 4.3 三处 Reconcile 排除（Q7）

`cmd/gateway/main.go` `applyRuntimeConfig`（`main.go:2547` 起）构造三个集合时，**跳过** `!provider.IsEnabled()` 的条目：

- **queue limits map**（`main.go:2547-2553`）：不加入禁用 provider → 走 `queue.Reconcile` 的 retired/drain（新 `Acquire` 返 `ErrProviderRemoved`、在途排空后回收）。对禁用 provider 的 `Acquire` 会被拒，是候选过滤之外的第二道闸。
- **breaker active 集合**（`main.go:2580-2583`）：不加入 → `breaker.Reconcile` 删除其状态 → 重新启用拿到干净状态（连续失败清零、冷却取消）。
- **httpclient active proxy 集合**（`main.go:2548-2553`，`provider.Proxy != ""` 才加入）：禁用 provider 的代理不加入 → Pool 关闭其空闲连接。注意 Pool 按规范化代理 URL 聚合，只有没有任何启用 provider 引用该代理时才清。

### 4.4 健康检测（Q8 = 第一档）

`internal/providerhealth/health.go`：

- `CheckAll` / `checkAll`（`health.go:168, 171-198`）遍历 `cfg.Providers` 时**跳过禁用 provider**——周期检测和批量手动检测都不探。
- `CheckProvider`（`health.go:242`，`?provider=name` 单个）**照常探测**，不因禁用拒绝。这是"禁用期间确认上游是否恢复"的唯一入口。
- `Snapshot`（`health.go:77`）遍历 `cfg.Providers` 时**跳过禁用 provider**，不返回 unchecked 条目——前端靠 `enabled` 字段判禁用态，不让"未检测"和"已禁用"混淆。
- `InvalidateChanged`（`health.go:342`）的 fingerprint 现含 baseURL/format/apiKey/userAgent/proxy。**新增 `enabled`**：禁用↔启用切换视为 fingerprint 变化，触发缓存失效 + `generation` bump（丢弃在途 `CheckAll` 结果）。`configFingerprint`（`health.go:389`）相应加入 enabled。

### 4.5 健康端点与配置端点

- `GET /health`（`handleHealth`，`main.go:1653`）：`queues` 构造循环（`main.go:1666-1669`）**跳过禁用 provider**；`breaker.Snapshot()` 已因 Reconcile 不含禁用 provider；`providerHealth.Snapshot` 已跳过。结果：禁用 provider 在 `health.queues`/`health.breakers`/`health.providerHealth` 三处均缺席，前端靠 `enabled` 字段判。
- `GET /api/providers/models?provider=`（`handleProviderModels`，`main.go:1811`）：**不拒绝禁用 provider**（Q9），照常查询上游模型列表。
- `handleProviderProbe`（`main.go:1893`）：**不拒绝**（Q9），照常探测。
- `GET /api/config`（`handleGetConfig`，`main.go:2311`）+ `configViewSnapshot`（`main.go:2356`）：`enabled` 字段随 Provider JSON 透传；`apiKeyConfigured` 注入循环（`main.go:2327`）不涉及 enabled，无需改。
- `PUT /api/config`（`handlePutConfig`，`main.go:2462`）：`apiKey`/proxy 密码 keep-sentinel 回填循环不涉及 enabled，`enabled` 是普通字段随结构体走，无需特殊处理。

### 4.6 启动 banner（Q11 = 第一个）

`cmd/gateway/helpers.go:264-266` `printBanner` 遍历 provider 时：

- 每个禁用的 provider 行尾标 `[已禁用]`。
- 额外：遍历 `cfg.Routes`，对"全部候选都被禁用"（单目标：provider 禁用；多目标：所有 target 的 provider 都禁用）的路由，输出一行警告：`路由 %q 的全部候选均已禁用，请求将不可用`。

不报错、不阻止启动。

### 4.7 validate 不拦（Q14）

`internal/config/config.go` 的 `validate` / `validateProvider` / `validateRouteTargets` **不**为禁用加任何拦截。`len(c.Providers) == 0` 检查（`config.go:899`）保持原样——全部被禁用时 `len(Providers)` 仍 > 0，不触发该检查。`validateRouteTargets` 对禁用 provider 的引用**不**报"引用未定义"（禁用 ≠ 未定义）。

## 5. 前端改动点

框架 Alpine.js，单全局组件，`src/app/*.js.part` 按文件名序拼接。`index.html` 是提交产物，改完必须跑 `make web-html`，CI 与 `TestRepoArtifactMatchesSources` 用 `go run ./cmd/webbuild -check` 字节比对。

**关键陷阱**：`configPayload()`（`07-nav-payload.js.part:29-80`）是 `PUT /api/config` 的全量替换载荷，注释明确写了"漏一个字段 = 每个 provider 该字段被静默抹掉、无报错"。`enabled` 必须贯穿以下所有触点，漏一处就会让"编辑某 provider 弹窗"把它的禁用状态悄悄改回启用。

### 5.1 字段贯穿（必做，T8）

| 触点 | 文件:行 | 改动 |
|------|---------|------|
| `providerForm` 初始形状 | `00-state.js.part:143-161` | 加 `enabled: true` |
| `editProvider` | `11-providers.js.part:26-49` | 回填 `enabled: existing?.IsEnabled() ?? true`（JS 端读 `existing.enabled`，nil/undefined → true） |
| `duplicateProvider` | `11-providers.js.part:57-85` | 复制源设 `enabled: true`（副本默认启用） |
| `saveProvider` 写入对象 | `11-providers.js.part:125-145` | 写入 `enabled: this.providerForm.enabled` |
| `configPayload` | `07-nav-payload.js.part:31-44` | provider 对象加 `enabled` 字段（注意 nil 语义：前端用 `?? true` 归一，载荷里显式写 bool） |
| `normalizeConfig` | `02-config-normalize.js.part` | 读回 `enabled`，nil → true |
| `providersYaml` | `10-preview-yaml.js.part:87-115` | 渲染 `enabled`（仅 false 时输出，保持 omitempty 语义） |

前端 nil 归一约定：**所有读 `provider.enabled` 的地方一律 `provider.enabled ?? true`**，与后端 `IsEnabled()` 的 nil→true 对齐。

### 5.2 列表行 toggle + 置灰（Q6 = E3，列表行不立即保存）

`index.template.html:617` provider 列表行 `<template x-for="(provider, name) in config.providers">`：

- 行操作列（现 `:677-685` edit/duplicate/delete 三个按钮）加第四个 toggle 按钮，或行首加开关。点击改 `config.providers[name].enabled = !current`，**不调** `persistConfig()`（Q6：批量禁用，走统一保存）。标记 dirty 让右上角/悬浮保存按钮亮起。
- 禁用行整行置灰：`:class` 加 `provider.enabled === false ? 'opacity-50 ...'`（具体 class Codex 对齐既有置灰风格）。
- 名称列旁加角标 `[已禁用]` 或 icon。

### 5.3 编辑弹窗 checkbox（Q6 = E3）

`index.template.html:1669-1705` "基本信息"区加 checkbox，沿用 `failover.enabled` 的 pill 视觉（`index.template.html:493-501`）：

```html
<!-- 伪结构 -->
<label class="...pill...">
  <input type="checkbox" x-model="providerForm.enabled" ...>
  <span>启用</span>
</label>
```

`providerForm.enabled` 初始 true（新增 provider 默认启用）；`editProvider` 回填。保存走 `saveProvider` → `persistConfig`。

### 5.4 配置期 provider 下拉（Q9 = 保留 option + disabled + 后缀）

**只改配置期两处**，历史日志筛选的两处下拉（`index.template.html:1284`、`:1321`）**不改**——历史日志里就是有被禁用 provider 的记录，不能过滤。

候选行（`:1882`）和 vision（`:2021`）的 `<template x-for="(provider, name) in config.providers">` 生成的 `<option>`：

```html
<!-- 伪结构：保留 option，加 disabled + 后缀，保住 :selected 回显 -->
<option :value="name"
        :selected="name === target.provider"
        :disabled="provider.enabled === false"
        x-text="name + (provider.enabled === false ? '（已禁用）' : '')">
</option>
```

`:selected` 回显模式（注释 `:1878-1881` 解释了为什么不能省）在 option 仍存在时正常工作——**绝不**把禁用 provider 的 option 过滤掉，否则 `x-model` 值找不到对应 option、浏览器回落到第一项、一保存就改错目标。

### 5.5 监控页禁用态（Q12 = G1 + Q8 置灰）

`09-format-metrics.js.part`：

- `providerStatusRows()`（`:287`）：返回对象加 `disabled: provider.enabled === false`，禁用行健康/熔断列走灰态占位（`—` 或"已禁用"），不读 `health.queues[name]`（反正缺席）。`status` 字段禁用时设"已禁用"。
- `providerStateCounts()`（`:344`）：新增 `disabled` 计数；禁用 provider 不计入 `ok`/`error`/`unchecked`/`tripped` 任一档（从分母扣出）。顶部 chip（`index.template.html:827-865`）加第五个"已禁用"。

### 5.6 config.example.yaml（T12）

`config.example.yaml` providers 段加注释行：

```yaml
    # enabled: true        # 可选：留空/true = 启用（默认），false = 禁用。
    #                     # 禁用后该 Provider 不参与转发与 vision 翻译，但保留配置，
    #                     # 随时可重新启用。不影响启动，也不影响保存。
```

## 6. 测试要求

| 测试 | 覆盖 |
|------|------|
| `internal/config/config_test.go` | 存量配置无 `enabled` 字段 → `IsEnabled()` 返回 true；显式 false → false；PUT 往返不丢字段；`validate` 不拦全部禁用 |
| `internal/router/router_test.go` | vision provider 被禁用 → `Match.VisionProvider == nil` 且 `VisionDisabled == true`；转发候选仍包含禁用 provider 的 Candidate（剔除在 main.go，不在 router） |
| `cmd/gateway/main_test.go` | 候选循环：禁用 provider 跳过、`AttemptTrail` 有 `provider_disabled`；全禁用 → 503 `all_candidates_disabled`；disabled + breaker 混合 → 报 disabled 终态；vision 禁用 → 图片识别失败计数 +1 |
| `cmd/gateway/main_test.go` | `applyRuntimeConfig`：禁用 provider 不在 queue/breaker/pool 三个 active 集合 |
| `internal/providerhealth` 测试 | `CheckAll` 跳过禁用；`CheckProvider` 不跳过；`Snapshot` 不返回禁用 provider 条目；`InvalidateChanged` 检测 enabled 变化 |
| `internal/webbuild` | `go run ./cmd/webbuild -check` exit=0（前端产物一致） |

所有 Go 测试在容器内跑（`golang:1.27-alpine`，容器名 `ai-gateway-dev-verify`），race 检测用 `golang:1.27` Debian 版。前端改完必须 `make web-html` 重建 `index.html`，否则 `TestRepoArtifactMatchesSources` 失败。

## 7. 验收标准

1. 存量 `config.yaml`（无 `enabled` 字段）加载后所有 provider `IsEnabled()` == true，行为零变化。
2. 禁用一个被单目标路由引用的 provider：网关正常启动、`PUT /api/config` 正常保存；该路由请求返回 503 `all_candidates_disabled`，`AttemptTrail` 含 `xxx:provider_disabled`。
3. 多目标路由禁用其中一个候选：请求正常走剩余候选，被禁用候选不出现在尝试链。
4. 禁用 vision provider 的路由：图片请求不调 vision、带原始图片块转发、图片识别失败计数 +1。
5. 禁用后 `GET /health`：该 provider 不在 queues/breakers/providerHealth 任一 map。
6. `?provider=<禁用>` 的健康检测与模型列表查询照常工作。
7. 前端：列表行 toggle 改状态但不立即保存；编辑弹窗 checkbox；监控页禁用行置灰 + 第五个 chip；路由配置下拉禁用 option 带后缀且可回显。
8. `go test ./...` 全绿；`go run ./cmd/webbuild -check` exit=0。

## 8. 决策摘要

| Q | 决策 |
|---|------|
| Q1 | A2 软停用，静默剔除候选，不影响启动/保存 |
| Q2 | 统一走 `PUT /api/config`，列表行可 toggle，走悬浮窗/右上角统一保存 |
| Q3 | B1 `Enabled *bool` + omitempty，applyDefaults 不物化，`IsEnabled()` = BoolOr(p.Enabled, true) |
| Q4 | C1 只做 provider 级 |
| Q5 | D1 词汇表用"已禁用"（disabled） |
| Q6 | E3 列表行 + 编辑弹窗都加；列表行不立即保存（批量） |
| Q7 | queue/breaker/httpclient 三处 Reconcile 都排除禁用 provider |
| Q8 | 周期 + 批量手动检测跳过禁用；`?provider=` 单个照常；监控/列表禁用行置灰 |
| Q9 | 保留 option + `disabled` 属性 + "（已禁用）"后缀（仅配置期两处下拉） |
| Q10 | F1 vision 剔除 + 计入图片识别失败 |
| Q11 | banner 标 `[已禁用]` + 全禁用路由额外警告 |
| Q12 | G1 监控行保留 + 显式禁用标记 + 第五个统计 chip |
| Q13 | H2 候选剔除落在 main.go 循环，终态 503 `all_candidates_disabled` |
| Q14 | validate 不拦，只 banner 警告 |
