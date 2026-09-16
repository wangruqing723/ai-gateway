# Provider 启用/禁用 — 任务清单

> 委托给 Codex 实现。每条任务包含文件路径、接口约束与验收标准。
>
> **归档说明（2026-09-16）**：第一版任务清单。落地实现以
> `docs/2026-09-16-provider-enabled/TASKS.md` 为准（该目录与代码对齐）。

## 后端任务（Go）

### T1 - config 字段与 accessor | P0 | 15min | 无依赖

**文件**: `internal/config/config.go`

**改动**:
1. `Provider` 结构体（L120-152）加字段：
   ```go
   Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
   ```
2. 在 `IntOr` 下方（L270 附近）加 accessor：
   ```go
   // IsEnabled 返回该 Provider 是否启用；nil 视为启用（默认）。
   func (p *Provider) IsEnabled() bool { return BoolOr(p.Enabled, true) }
   ```
3. `applyDefaults`（L415-466）**不**触碰 `Enabled` 字段（对齐 `OneMContext` 的"不物化"）。
4. `validate`（L898-983）**不**为 `enabled` 加任何校验——nil/true/false 都是合法终态。

**验收**:
- 存量配置无 `enabled` 字段加载后 `IsEnabled()` == true
- 显式 `enabled: false` 的 provider `IsEnabled()` == false
- `go test ./internal/config` 通过

---

### T2 - router vision 剔除 | P0 | 10min | 依赖 T1

**文件**: `internal/router/router.go`

**改动**:
1. `Match` 结构体（L28-37）加字段：
   ```go
   VisionDisabled bool  // vision 配置存在但 provider 被禁用
   ```
2. `MatchRoute` 函数（L84）vision provider 解析改为：
   ```go
   if route.Vision != nil {
       vSrc := cfg.Providers[route.Vision.Provider]
       if vSrc != nil {
           if vSrc.IsEnabled() {
               vpCopy := *vSrc
               m.VisionProvider = &vpCopy
               m.VisionModel = route.Vision.Model
           } else {
               m.VisionDisabled = true
           }
       }
   }
   ```

**验收**:
- vision provider 被禁用 → `Match.VisionProvider == nil` 且 `VisionDisabled == true`
- `go test ./internal/router` 通过

---

### T3 - 候选循环禁用剔除 | P0 | 30min | 依赖 T1, T2

**文件**: `cmd/gateway/main.go`

**改动**:
1. 候选循环变量块（L664-680）加 `disabledSkips int`
2. 在熔断判断（L683 `if s.breaker != nil && !s.breaker.Allow(name)`）**之前**插入：
   ```go
   if !candidate.Provider.IsEnabled() {
       disabledSkips++
       trail = append(trail, name+":provider_disabled")
       reqLog.AttemptTrail = append(reqLog.AttemptTrail, metrics.AttemptStep{
           Provider:   name,
           Model:      candidate.TargetModel,
           Outcome:    "skipped",
           ErrorType:  "provider_disabled",
           Error:      "Provider 已禁用，跳过该候选",
       })
       continue
   }
   ```
3. 终态 switch（L825 起）**第一个** case（优先级最高）加：
   ```go
   case attempts == 0 && disabledSkips > 0:
       reqLog.AttemptTrail = strings.Join(trail, " → ")
       reqLog.Error = "全部候选上游均已禁用"
       writeJSONError(w, http.StatusServiceUnavailable, "all_candidates_disabled", reqLog.Error)
   ```
4. vision 禁用记账：在 `needVision` 判断（L585）之后、vision 翻译调用（L596-618）区块加逻辑：若 `matched.VisionDisabled && vision.HasImages(internal.Messages)`，记一次图片识别失败（设 `reqLog.VisionFailed++`，若 `FirstFailure` 机制存在则填 category="其他" + message="vision provider 已禁用"；实现时核对 `internal/vision` 的失败结构体，保持既有软降级记账一致）。

**验收**:
- 单目标路由 provider 禁用 → 503 `all_candidates_disabled`，`AttemptTrail` 含 `xxx:provider_disabled`
- 多目标路由禁用一个候选 → 请求正常走剩余候选
- vision provider 禁用 → 不调 vision、`VisionFailed` 计数 +1
- `go test ./cmd/gateway -run TestXxx` 涉及候选循环的测试通过（需补或改测试用例）

---

### T4 - Reconcile 三处排除 | P0 | 20min | 依赖 T1

**文件**: `cmd/gateway/main.go`

**改动**: `applyRuntimeConfig`（L2547 起）构造三个集合时跳过 `!provider.IsEnabled()` 的 provider：

1. **queue limits**（L2547-2553）:
   ```go
   for name, provider := range newCfg.Providers {
       if !provider.IsEnabled() { continue }
       limits[name] = queue.Limits{...}
       ...
   }
   ```
2. **httpclient active proxies**（同一循环，L2551）:
   ```go
   if provider.Proxy != "" {
       activeProxies[provider.Proxy] = struct{}{}
   }
   ```
   已在 `if !provider.IsEnabled() { continue }` 之后，自然排除。
3. **breaker active**（L2580-2583）:
   ```go
   for name, provider := range newCfg.Providers {
       if !provider.IsEnabled() { continue }
       active[name] = struct{}{}
   }
   ```

**验收**:
- 禁用 provider 后热重载 → `queue.StatusOf` 返回空/无该条目、`breaker.Snapshot()` 不含该 name、Pool 关闭其代理连接
- `go test ./cmd/gateway -run TestApplyRuntimeConfig` 类测试通过（需补或改）

---

### T5 - providerhealth 跳过禁用 | P0 | 25min | 依赖 T1

**文件**: `internal/providerhealth/health.go`

**改动**:
1. `checkAll`（L171-198）遍历 `cfg.Providers` 时跳过禁用：
   ```go
   for name, provider := range cfg.Providers {
       if !provider.IsEnabled() { continue }
       ...
   }
   ```
2. `CheckProvider`（L242）**不**跳过禁用，照常探测（单个显式检测不拒绝）。
3. `Snapshot`（L77-89）遍历时跳过禁用：
   ```go
   for name, provider := range cfg.Providers {
       if !provider.IsEnabled() { continue }
       ...
   }
   ```
4. `configFingerprint`（L389-396）计算 fingerprint 时**加入** `enabled` 字段：
   ```go
   for name, p := range cfg.Providers {
       fmt.Fprintf(h, "%s:%s:%s:%s:%s:%s:%t\n",
           name, p.BaseURL, p.Format, p.APIKey, p.UserAgent, p.Proxy, p.IsEnabled())
   }
   ```
5. `InvalidateChanged`（L342-372）缓存失效时检查 enabled：若 `stored.IsEnabled() != current.IsEnabled()` 则视为 fingerprint 变化。

**验收**:
- `CheckAll` 不探禁用 provider
- `CheckProvider(禁用name)` 返回探测结果，不返 404
- `Snapshot` 不返禁用 provider 条目
- enabled 切换触发 fingerprint 变化、`generation` bump
- `go test ./internal/providerhealth` 通过（需补或改）

---

### T6 - handleHealth 跳过禁用 | P0 | 10min | 依赖 T1

**文件**: `cmd/gateway/main.go`

**改动**: `handleHealth`（L1653-1702）的 `queues` 构造循环（L1666-1669）跳过禁用：
```go
for name, p := range cfg.Providers {
    if !p.IsEnabled() { continue }
    queues[name] = s.qm.StatusOf(name, p.MaxConcurrent, p.MaxPerSecond)
}
```

`breaker.Snapshot()` 和 `providerHealth.Snapshot(cfg)` 已在 T4/T5 自然排除，此处只改 queues。

**验收**:
- `GET /health` 响应的 `queues`/`breakers`/`providerHealth` 均不含禁用 provider
- `curl` 测试或 `go test` 验证

---

### T7 - 启动 banner 标注与警告 | P0 | 20min | 依赖 T1

**文件**: `cmd/gateway/helpers.go`

**改动**: `printBanner`（L264-266）:
1. provider 循环中，禁用的行尾加 `[已禁用]`：
   ```go
   for name, p := range cfg.Providers {
       suffix := ""
       if !p.IsEnabled() { suffix = " [已禁用]" }
       logSystem("    %s: %s [%s] key=%s 并发=%d%s", name, p.BaseURL, p.Format, mask(p.APIKey), p.MaxConcurrent, suffix)
   }
   ```
2. 在 provider 循环之后，遍历 `cfg.Routes`，对"全部候选都被禁用"的路由输出警告：
   ```go
   for _, route := range cfg.Routes {
       targets := route.TargetList()
       allDisabled := true
       for _, t := range targets {
           if p := cfg.Providers[t.Provider]; p != nil && p.IsEnabled() {
               allDisabled = false
               break
           }
       }
       if allDisabled && len(targets) > 0 {
           logSystem("    ⚠️  路由 %q 的全部候选均已禁用，请求将不可用", route.Match)
       }
   }
   ```

**验收**:
- 禁用 provider 的启动日志行尾有 `[已禁用]`
- 全部候选禁用的路由有警告行
- 网关正常启动（不因禁用报错）

---

### T8 - config.example.yaml 注释 | P2 | 5min | 无依赖

**文件**: `config.example.yaml`

**改动**: providers 段示例加注释（在 `maxQueueWait` 下方）：
```yaml
    # enabled: true        # 可选：留空/true = 启用（默认），false = 禁用。
    #                     # 禁用后该 Provider 不参与转发与 vision 翻译，但保留配置，
    #                     # 随时可重新启用。不影响启动，也不影响保存。
```

**验收**: 人工确认注释清晰

---

## 前端任务（Alpine.js / HTML）

### T9 - 字段贯穿七触点 | P0 | 30min | 无依赖

**文件**: `cmd/gateway/web/src/app/*.js.part`

**改动**（每处用 `provider.enabled ?? true` 归一 nil）:
1. `00-state.js.part:143-161` `providerForm` 加 `enabled: true`
2. `11-providers.js.part:26-49` `editProvider` 回填 `this.providerForm.enabled = p?.enabled ?? true`
3. `11-providers.js.part:57-85` `duplicateProvider` 设 `enabled: true`
4. `11-providers.js.part:125-145` `saveProvider` 写入对象加 `enabled: this.providerForm.enabled`
5. `07-nav-payload.js.part:31-44` `configPayload` 的 provider 对象加 `enabled: provider.enabled ?? true`（显式写 bool，不传 undefined）
6. `02-config-normalize.js.part` `normalizeConfig` 读回时归一：`provider.enabled = provider.enabled ?? true`（或不归一、让模板处理，但前五处必须归一避免 undefined 进 payload）
7. `10-preview-yaml.js.part:87-115` `providersYaml` 渲染时只在 `enabled === false` 时输出该行（保持 omitempty 语义）

**验收**:
- 编辑任意 provider 保存后 `enabled` 字段不丢
- 复制 provider 得到启用状态
- 前端 console 无 undefined 错误

---

### T10 - 列表行 toggle + 置灰 | P0 | 30min | 依赖 T9

**文件**: `cmd/gateway/web/src/index.template.html`

**改动**:
1. provider 列表行（L617 `<template x-for>`）整行 `:class` 加禁用置灰：
   ```html
   :class="(provider.enabled ?? true) ? '' : 'opacity-50'"
   ```
2. 操作列（L677-685）加 toggle 按钮（在 edit 前或 delete 后）：
   ```html
   <button @click="config.providers[name].enabled = !(config.providers[name].enabled ?? true)"
           :title="(provider.enabled ?? true) ? '禁用' : '启用'"
           :aria-label="(provider.enabled ?? true) ? '禁用' : '启用'"
           class="...icon-button...">
       <span class="material-symbols-outlined text-[18px]"
             x-text="(provider.enabled ?? true) ? 'toggle_on' : 'toggle_off'"></span>
   </button>
   ```
   不调 `persistConfig()`，标记 dirty 让保存按钮亮起（dirty 检测已有，改 `config.providers` 自动触发）。
3. 名称列旁加 `<span x-show="provider.enabled === false" x-cloak>（已禁用）</span>`。

**验收**:
- 点 toggle 改状态但不立即保存
- 禁用行置灰
- 保存按钮亮起

---

### T11 - 编辑弹窗 checkbox | P0 | 15min | 依赖 T9

**文件**: `cmd/gateway/web/src/index.template.html`

**改动**: provider 弹窗（L1642-1808）"基本信息"区（L1669-1705）顶部加 checkbox，沿用 failover pill 视觉（L493-501）：
```html
<label class="flex cursor-pointer items-center gap-3 rounded-lg border border-line bg-ink px-3 py-2">
    <input autocomplete="off" type="checkbox" x-model="providerForm.enabled"
           class="h-4 w-4 rounded border-line bg-panel text-cyan2 focus:ring-cyan2">
    <span class="text-sm font-bold">启用该 Provider</span>
</label>
```

**验收**:
- 编辑禁用 provider，checkbox 回显 unchecked
- 勾/取消勾 → 保存后生效

---

### T12 - 配置期下拉 disabled option | P0 | 20min | 依赖 T9

**文件**: `cmd/gateway/web/src/index.template.html`

**改动**（只改配置期两处，**不改**历史日志下拉 L1284/L1321）:
1. 候选行 provider select（L1882 `<template x-for="(provider, name) in config.providers">`）的 `<option>`：
   ```html
   <option :value="name"
           :selected="name === target.provider"
           :disabled="provider.enabled === false"
           x-text="name + (provider.enabled === false ? '（已禁用）' : '')">
   </option>
   ```
2. vision provider select（L2021）同样改法。

**验收**:
- 禁用 provider 在下拉中显示"xxx（已禁用）"且 disabled
- 已配置该禁用 provider 的路由打开弹窗，回显正确、不可选但也不会回落到第一项

---

### T13 - 监控页禁用态 | P0 | 35min | 依赖 T9

**文件**: `cmd/gateway/web/src/app/09-format-metrics.js.part`

**改动**:
1. `providerStatusRows()`（L287）返回对象加字段：
   ```js
   disabled: provider.enabled === false,
   ```
   禁用行 `status` 改为"已禁用"，`statusClass` 用灰色调。
2. `providerStateCounts()`（L344）加 `disabled` 计数：
   ```js
   let disabled = 0;
   for (const name of names) {
       const provider = this.config.providers[name];
       if (provider.enabled === false) {
           disabled++;
           continue;  // 不计入 ok/error/unchecked/tripped
       }
       // 现有健康/熔断统计...
   }
   return { ..., disabled };
   ```
3. `index.template.html:827-865` 顶部 chip 加第五个"已禁用"（在"未检测"后）：
   ```html
   <div x-show="providerStateCounts().disabled > 0" x-cloak ...灰色调...>
       <span ...material-symbols...>block</span>
       <span>已禁用</span>
       <span ...badge...x-text="providerStateCounts().disabled"></span>
   </div>
   ```
4. 监控表格行（L934 `<template x-for="row in providerStatusRows()">`）加禁用置灰：
   ```html
   :class="row.disabled ? 'opacity-50' : ''"
   ```
   健康/熔断列（L942-995）在 `row.disabled` 时显示"—"或"已禁用"。

**验收**:
- 禁用 provider 在监控页置灰
- 顶部出现"已禁用 N"chip
- 禁用行健康/熔断列显示占位而非 unchecked

---

### T14 - 重建 index.html | P0 | 2min | 依赖 T9-T13

**文件**: `cmd/gateway/web/index.html`

**改动**: 运行 `make web-html`（或 `go run ./cmd/webbuild`）重建产物。

**验收**: `go run ./cmd/webbuild -check` exit=0

---

## 文档任务

### T15 - CONTEXT.md 词条 | P1 | 10min | 无依赖

**文件**: `CONTEXT.md`

**改动**:
1. 在"熔断打开事件"之前插入新词条：
   ```markdown
   **已禁用 Provider**：由使用者显式声明不参与转发的 Provider 状态；它不由探测或失败计数产生，也不随时间自动恢复。已禁用的 Provider 在候选循环中被剔除，不参与请求转发和 vision 翻译。
   ```
2. 修改"熔断打开事件"首句：`某个 Provider 从允许请求的状态转为网关暂时不再向其转发的熔断状态这一事实`（去掉"暂时禁止请求"，避免与"已禁用"撞词）。

**验收**: 词汇表三个概念边界清晰

---

## 测试任务

### T16 - 补充单元测试 | P0 | 60min | 依赖 T1-T7

**文件**: `internal/config/config_test.go`, `internal/router/router_test.go`, `cmd/gateway/main_test.go`, `internal/providerhealth/health_test.go`

**改动**: 每个受影响模块补或改测试用例，覆盖：
- config: `IsEnabled()` 三种状态、PUT 往返、validate 不拦全禁用
- router: vision 禁用逻辑、Match.VisionDisabled
- main: 候选禁用跳过、503 终态、AttemptTrail、Reconcile 排除、vision 禁用记账
- providerhealth: CheckAll 跳过、CheckProvider 不跳过、Snapshot 不含禁用、fingerprint 含 enabled

**验收**: `go test ./...` 全绿（在容器内 `golang:1.27-alpine` 跑）

---

## 依赖关系

```
T1 (config 字段) → T2 (router vision) → T3 (main 候选循环)
                 → T4 (Reconcile)
                 → T5 (providerhealth)
                 → T6 (handleHealth)
                 → T7 (banner)
                 → T16 (tests)

T9 (前端字段) → T10 (列表 toggle)
              → T11 (弹窗 checkbox)
              → T12 (下拉 disabled)
              → T13 (监控页)
              → T14 (rebuild index.html)

T8 (example.yaml) 无依赖
T15 (CONTEXT.md) 无依赖
```

## 实施顺序建议

1. 后端 T1-T7 顺序实施（P0）
2. 后端 T16 补测试（P0）
3. 前端 T9-T14 顺序实施（P0）
4. 文档 T8, T15（P1-P2）
5. 容器内验证：`go test ./...` + `go run ./cmd/webbuild -check`
