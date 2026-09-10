# TASKS · [1M] 后缀透传与 provider extraHeaders

> 需求根因：第三方中转站靠 **model 名的 `[1m]` 后缀** 识别 1M 上下文；网关 `StripOneMSuffix` 剥掉后缀后才转发，上游收不到信号，报「1m 上下文已经全量可用，请启用 1m 上下文后重试」。官方 Anthropic API 不认字面后缀（会 404），故不能无脑全保留——必须按 provider 可控。同时顺带加通用 `extraHeaders`，覆盖个别中转还要 beta 头等自定义头的场景。

## 决策摘要（已与用户确认）

- `oneMContext`: 枚举 `strip`（默认，现行行为）/ `preserve`（拼回后缀）。仅 `preserve` 时拼。
- 后缀拼接规则：**一律拼到上游 model 名尾部**。target.model 空 → 透传剥后名字 + 原始后缀形态；target.model 配了 → 配置名 + 原始后缀形态。
- `contextWindow` 覆盖：`[1m]` 标记存在时仍覆盖为 1_000_000，与 preserve 无关（本地预算与上游识别是两件事）。
- `extraHeaders`：map[string]string；黑名单禁覆盖 `x-api-key`/`authorization`/`content-type`/`user-agent`；允许覆盖 `anthropic-version`/`accept`。三条出网路径都带：转发、`fetchUpstreamModels`、`providerhealth.checkOne`。
- 后缀原始形态保留：客户端写 `[1m]` 就转发 `[1m]`，写 `[1M]` 就转发 `[1M]`（不归一化大小写）。

## 任务清单

### T1 · config 新增字段与校验（P0）
文件：`internal/config/config.go`、`internal/config/config_test.go`
- [ ] 在 `Provider` 结构体加 `OneMContext string`（`yaml:"oneMContext,omitempty" json:"oneMContext,omitempty"`）与 `ExtraHeaders map[string]string`（`yaml:"extraHeaders,omitempty" json:"extraHeaders,omitempty"`）。
- [ ] `validate`：`oneMContext` 仅允许 `""` / `strip` / `preserve`，否则报错含字段路径；`extraHeaders` key 非空、不命中黑名单（不区分大小写：`x-api-key`/`authorization`/`content-type`/`user-agent`），value 长度 ≤ 1024 rune，条目数 ≤ 20，否则报错。
- [ ] `applyDefaults`：`oneMContext` 为空时不物化（保持 nil 语义，omitempty 不写出）；`extraHeaders` 为空保持 nil。
- [ ] 常量：`extraHeadersMaxEntries=20`、`extraHeadersMaxValueRunes=1024`、`extraHeaderBlocklist`。
- [ ] 测试：合法/非法 oneMContext 取值；黑名单 key 被拒；空 map 不写进 YAML（`TestContextWindowOmitEmptyYAML` 扩展）。

### T2 · router 后缀拆分与 preserve 拼回（P0）
文件：`internal/router/router.go`、`internal/router/router_test.go`
- [ ] 新增 `splitOneMSuffix(model string) (stripped, originalMarker string, has bool)`：大小写不敏感识别尾部 `[1m]`，返回剥后名字 + **客户端原始书写的标记文本**（含方括号，如 `[1M]`）+ 命中标志。`StripOneMSuffix` 改为基于它实现，行为不变（旧调用方无感知）。
- [ ] `MatchRoute`：用 `splitOneMSuffix` 取 `originalMarker`；当 `hasOneM && provider.OneMContext=="preserve"` 时，`TargetModel = TargetModel + originalMarker`（target.model 空时是剥后客户端名 + marker，配了时是配置名 + marker）。非 preserve 保持现行剥后无后缀。
- [ ] `contextWindow` 覆盖为 `OneMContextWindow` 的逻辑不变（hasOneM 时覆盖，与 preserve 无关）。
- [ ] 测试：preserve 下 target.model 空/配了两种情况都拼回原始形态；strip/默认不拼；大小写形态保留；`StripOneMSuffix` 旧契约不破。

### T3 · proxy 应用 extraHeaders（P0）
文件：`internal/proxy/upstream.go`、`internal/proxy/upstream_test.go`
- [ ] `setUpstreamHeaders` 末尾（所有 `Set` 之后）遍历 `p.ExtraHeaders` 应用：跳过黑名单 key（与 config.validate 同名单，双保险），其余 `req.Header.Set(k, v)`。
- [ ] 测试：自定义头被写入；黑名单头被跳过；`ExtraHeaders` 为 nil 时行为不变（逐字节兼容）。

### T4 · 模型查询与健康检测应用 extraHeaders（P1）
文件：`cmd/gateway/main.go`（`fetchUpstreamModels`）、`internal/providerhealth/health.go`（`checkOne`）
- [ ] 两处在各自现有 `Set` 之后，复用 T3 同一黑名单逻辑应用 `p.ExtraHeaders`。抽公共 helper（如 `proxy.ApplyExtraHeaders(h http.Header, extra map[string]string)`）避免三处重复。
- [ ] 测试：`fetchUpstreamModels`/`checkOne` 带上自定义头（可用 httptest server 断言）。

### T5 · /api/config 脱敏与热重载（P1）
文件：`cmd/gateway/main.go`（`configViewSnapshot`、`applyRuntimeConfig`）
- [ ] **决策：extraHeaders 不脱敏**（非密钥字段，用户应把密钥放 apiKey；前端 placeholder 明确提示）。DESIGN.md 记录此已知风险。
- [ ] 热重载：`oneMContext`/`extraHeaders` 随正常 YAML 解码 + `applyRuntimeConfig` 生效，无需新 Reconcile（每次请求读 Provider 字段，不缓存）。仅补测试验证改配置后新值生效。
- [ ] `restartRequiredFields` 不涉及（非 host/port）。

### T6 · 前端编辑器（P0）
文件：`cmd/gateway/web/src/app/11-providers.js.part`、`cmd/gateway/web/src/app/02-config-normalize.js.part`、`cmd/gateway/web/src/index.template.html`
- [ ] provider 表单加 `oneMContext` 下拉（留空/strip/preserve，默认留空=现行行为）。
- [ ] provider 表单加 `extraHeaders` 键值对编辑器（与现有字段风格一致；placeholder 提示「不要放密钥，密钥用 apiKey；黑名单头会被忽略」）。
- [ ] `02-config-normalize.js.part`：加载/保存时规范化 `oneMContext` 与 `extraHeaders`（空 map → 不写出）。
- [ ] 配置预览 YAML 正确渲染两项。

### T7 · webbuild 重建产物（P0）
- [ ] 改完前端 `.part` 后运行 `webbuild -check`，确认 `exit=0` 且 `cmd/gateway/web/index.html` 已重建。**未重建 index.html 会被 `TestRepoArtifactMatchesSources` 捕获，但自述不会暴露，必须实跑。**

### T8 · config.example.yaml 更新（P1）
- [ ] 在 provider 示例段加注释示例：`oneMContext: preserve`（说明：仅第三方中转用，官方 API 不要开）与 `extraHeaders` 示例（`anthropic-beta: context-1m-2025-08-07` 等）。

### T9 · 集成测试与格式化（P0）
- [ ] `go test ./...` 全绿（容器内 `golang:1.23-alpine`）。
- [ ] `go vet ./...` 干净；`gofmt -w ./cmd ./internal`。
- [ ] race：`go test -race ./internal/router/ ./internal/proxy/ ./internal/config/`（涉及并发读 ExtraHeaders）。
- [ ] `webbuild -check` exit=0。

## 验收标准（实跑，不信自述）

1. 单测全绿、vet 干净、gofmt 无 diff、webbuild -check exit=0。
2. 关键行为：preserve + 带后缀请求 → `forwardAttempt` 构建的上游 body `model` 字段含原始 `[1m]`/`[1M]` 后缀；strip/默认 → 不含。
3. extraHeaders 黑名单头绝不写进上游请求；自定义头出现在转发、模型查询、健康检测三条路径。
4. 无 `[1m]` 请求时逐字节兼容（无后缀、无 extraHeaders 时行为与改动前一致）。

## 不做

- 不透传客户端 `anthropic-beta` 头（核验结论：官方 1M 已 GA 不需要 beta 头；中转要的是 model 后缀。透传 beta 头不对症，省略）。
- 不改 `contextWindow` 预算裁决逻辑（仅后缀拼接是新增行为）。
- 不为 `extraHeaders` 做密钥脱敏（见 T5 决策）。
