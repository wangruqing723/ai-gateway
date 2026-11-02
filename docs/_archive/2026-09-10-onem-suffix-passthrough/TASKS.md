# TASKS · [1M] 后缀透传与 provider extraHeaders

> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 主实现已于 2026-09-10 提交：`feed01b`；当前核对基线为 `46a72c6`。此前全部未勾选是文档状态滞后，不代表代码尚未实现。
> 历史根因：需求提出时，部分第三方中转站靠 model 名尾部的 `[1m]` 标记识别 1M 上下文；统一剥后缀导致上游收不到信号。外部 API 依据保留为 2026-09-10 的历史记录，本次不把它当作 2026-11-01 的重新核验结论。

## 决策摘要（已与用户确认，现行代码口径）

- `oneMContext`：`strip`（默认；空串等价）/ `preserve`。仅 `preserve` 拼回客户端原始后缀。
- 后缀一律拼到上游 model 名尾部：target.model 空时为剥后客户端名 + marker；显式配置时为配置名 + marker。`SplitOneMSuffix` 是实际导出名称，`[1m]` / `[1M]` 原样保留，标记前的空白不保留。
- `[1m]` 存在时本地 `contextWindow` 仍覆盖为 1_000_000，与 preserve 无关；不改变 maxTokens 的既有优先级。
- `extraHeaders`：`map[string]string`；配置校验与 `proxy.ApplyExtraHeaders` 双重阻止覆盖 `x-api-key` / `authorization` / `content-type` / `user-agent`，允许覆盖 `anthropic-version` / `accept`。
- 三条出网路径共用 `proxy.ApplyExtraHeaders`：转发、模型查询、健康检测。后续多密钥重构中实际构建请求的函数为 `fetchUpstreamModelsOnce` 与 `providerhealth.checkOneRequest`，上层入口仍是 `fetchUpstreamModels` / `checkOne`。

## 任务清单

### T1 · config 新增字段与校验（P0）
文件：`internal/config/config.go`、`internal/config/config_test.go`
- [x] `Provider` 已有 `OneMContext string` 与 `ExtraHeaders map[string]string`，YAML / JSON 均使用 lower camelCase 和 `omitempty`。
- [x] `validateOneMContext` 仅接受 `""` / `strip` / `preserve`，错误包含 `providers.<name>.oneMContext`。
- [x] `validateExtraHeaders`：头名去首尾空白后不能为空、长度 ≤128 rune、符合 HTTP token；不区分大小写拦截黑名单；值 ≤1024 rune，拒绝除 HTAB 外的 ASCII 控制字符；条目 ≤20。
- [x] `applyDefaults` 不物化 `oneMContext` 的空串；未配置 `extraHeaders` 保持 nil，空 map 由 `omitempty` 省略。这里是 string 零值语义，不是 `*string` 的 nil 语义。
- [x] 常量实际名称为 `maxExtraHeaderEntries=20`、`maxExtraHeaderValueRunes=1024`、`maxExtraHeaderNameRunes=128`；公共黑名单入口为 `config.ExtraHeaderBlocked`。
- [x] 已有测试：`TestOneMContextValidation`、`TestOneMContextOmitEmptyYAML`、`TestExtraHeadersValidation`、`TestExtraHeadersLimits`、`TestExtraHeaderBlocked`。YAML 测试锁定未配置字段不输出，不再误写为扩展 `TestContextWindowOmitEmptyYAML`。

### T2 · router 后缀拆分与 preserve 拼回（P0）
文件：`internal/router/router.go`、`internal/router/router_test.go`
- [x] 已导出 `SplitOneMSuffix(model string) (stripped, marker string, has bool)`；`StripOneMSuffix` 基于它实现，旧返回契约不变。
- [x] `MatchRoute` 用剥后名字匹配路由；有标记且 provider 为 preserve 时，将 marker 拼到最终 `TargetModel`。
- [x] 标记存在时 `ContextWindow` 覆盖为 `OneMContextWindow`，不依赖 preserve。
- [x] 已有 `TestSplitOneMSuffixReturnsOriginalMarker`、`TestMatchRouteOneMContextPreserve`、`TestOneMContextPreserveStillOverridesWindow`、`TestStripOneMSuffix`，覆盖目标名为空/显式配置、默认/strip、原始大小写形态与窗口覆盖。

### T3 · proxy 应用 extraHeaders（P0）
文件：`internal/proxy/upstream.go`、`internal/proxy/upstream_test.go`
- [x] `setUpstreamHeaders` 在网关自设头与鉴权头之后调用 `ApplyExtraHeaders`；helper 去头名首尾空白，跳过空名称与 `config.ExtraHeaderBlocked` 命中的名称。
- [x] 已有 `TestSetUpstreamHeadersAppliesExtraHeaders`、`TestSetUpstreamHeadersExtraHeadersCannotBreakAuth`、`TestApplyExtraHeadersNilAndEmpty`，验证自定义头、协议头覆盖、黑名单保护及 nil/空 map 不改原 Header。

### T4 · 模型查询与健康检测应用 extraHeaders（P1）
文件：`cmd/gateway/main.go`、`internal/providerhealth/health.go`
- [x] 模型查询和健康检测均在设置自身协议/鉴权/UA 头后复用 `proxy.ApplyExtraHeaders(h http.Header, extra map[string]string)`，没有复制黑名单。
- [x] （已迁移）原承诺的模型查询/健康检测真实收到自定义头的 httptest 专项用例未找到，已移交 `docs/KNOWN_ISSUES.md` 的「历史观测与后缀功能专项回归覆盖」；共享 helper 单测不等于这两条边界已经测过。

### T5 · /api/config 脱敏与热重载（P1）
文件：`cmd/gateway/main.go`、前端配置保存链路
- [x] `extraHeaders` 不脱敏的原决策已落地；表单使用独立警告文案说明密钥应放 API Key 字段，`/api/config` 会明文返回该字段。风险见 DESIGN.md。
- [x] 两字段随正常解码和 `applyRuntimeConfig` 替换配置后供新请求读取，无需针对字段新增 Reconcile；模型查询/健康检测构建请求时也读取 Provider 值。
- [x] （已迁移）原承诺的这两个字段经配置 API 保存/重载后新值生效的专项断言未找到，已移交同一统一专项回归项；通用配置热重载测试不作为字段级通过记录。
- [x] 两字段不进入 `restartRequiredFields`，只有监听 host/port 需要重启。

### T6 · 前端编辑器（P0）
文件：`cmd/gateway/web/src/app/11-providers.js.part`、`02-config-normalize.js.part`、`07-nav-payload.js.part`、`10-preview-yaml.js.part`、`index.template.html`
- [x] provider 表单已有 `oneMContext` 下拉（留空/strip/preserve；留空保持默认行为）。
- [x] `extraHeaders` 已有键值行编辑器，独立警告说明不要放密钥及黑名单头限制，不再要求提示必须放在 placeholder 内。
- [x] normalize / provider 重建 / `configPayload()` / YAML 预览链路均携带两字段；表单空行转换为 undefined，不写出空配置。

### T7 · webbuild 重建产物（P0）
- [x] HTML 产物已落地；2026-11-01 主会话 Claude 在 `golang:1.27-alpine` 容器单独确认 `go run ./cmd/webbuild -check` 退出 0。`TestRepoArtifactMatchesSources` 随全量测试通过。本次文档子任务未重建前端。

### T8 · config.example.yaml 更新（P1）
- [x] 示例已有 `oneMContext: preserve` 与 `extraHeaders` 注释，包括默认 strip、适用第三方中转、黑名单、数量/长度限制及明文返回风险。示例 beta 值是历史配置例子，不是当前官方 API 必填声明。

### T9 · 集成测试与格式化（P0）
- [x] 2026-11-01 主会话 Claude 在 `golang:1.27-alpine` 容器执行 `go test -count=1 ./...` 全部通过，`go vet ./...` 退出 0。
- [x] 格式检查记录已补正：唯一既有例外 `internal/breaker/breaker.go` 与本需求无关，未顺带格式化；不声称全仓库 gofmt 无 diff。
- [x] `golang:1.27` 下 router / proxy 的 race 在本会话已通过；同时 converter / gateway / metrics / providerhealth / keypool / vision 的 race 通过。
- [x] 2026-11-02 主会话在 Debian `golang:1.27` 容器执行全量 `go test -race -count=1 ./...`，包含 `internal/config`，全部通过；配置包竞态补验已完成，不再迁移为待办。
- [x] `webbuild -check` 退出 0（见 T7）。

## 最终验收记录（2026-11-01）

- 主实现 `feed01b` 的提交说明曾记录「未经本地验证」；保留这个 2026-09-10 历史事实，不回写为当时已测试。2026-11-01 的容器测试/vet/webbuild 检查补足通用自动验证。
- 后缀拆分、候选 model 与 helper 行为已有上述单测；`cmd/gateway/context_window_test.go` 的 `TestOneMSuffixMatchesRouteStripsUpstreamModelAndUsesMillionWindow` 还覆盖默认 strip 路径真实出网 body 与 1M 本地预算，不能据此推称 preserve 真出网也已覆盖。
- [x] （已迁移）`forwardAttempt` 在 preserve 档真正发出的 body 含客户端原始 `[1m]` / `[1M]` 的专项回归未找到；目标名为空/显式配置、同格式/跨格式路径的真实上游断言移交「历史观测与后缀功能专项回归覆盖」。当前结论来自 router 与请求构建源码，不是该专项测试通过。
- 两条 models/health 自定义头边界及配置 API 字段级保存/重载回归已移交同一项。配置包竞态补验已于 2026-11-02 完成，不属于剩余测试欠账。所有 `[x]（已迁移）` 只表示移交完成，不表示测试已补或已跑。
- 本目录无剩余实现或人工验收门槛，欠账由统一已知问题追踪；本次只改文档，不补代码测试。

## 验证命令（现行工具链与 GOPROXY）

从仓库根目录执行；下面是重跑方式，不是本次文档子任务的命令输出。

```bash
# 全量测试、vet、HTML 产物检查
# 容器串行复用固定名称，module cache 与国内 GOPROXY 避免依赖下载超时。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27-alpine sh -c 'go run ./cmd/webbuild -check && go vet ./... && go test -count=1 ./...'

# 格式只检查；-l 有输出仍可能退出 0，必须检查输出内容。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work golang:1.27-alpine gofmt -l ./cmd ./internal

# 补充 race 的重跑命令；此范围已于 2026-11-02 随全量竞态检测通过。
# 使用 Debian 镜像（含 gcc），不要在 Alpine 临时装编译器。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod-deb:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27 go test -race ./internal/router/ ./internal/proxy/ ./internal/config/
```

## 不做（保留原范围决策）

- 不新增客户端 `anthropic-beta` 头透传。2026-09-10 的设计判断是本问题应由 provider 后缀策略解决；外部 API 的现行要求本次未重新核验，若调整接入须查目标上游文档并做实际请求验证。
- 不改 `contextWindow` 预算裁决逻辑。
- 不为 `extraHeaders` 做密钥脱敏；Origin/Host 防护不等于鉴权，不能因此把真实密钥放在该字段。

## 最终归档状态（2026-11-02）

- 已按使用者授权移入 `docs/_archive/2026-09-10-onem-suffix-passthrough/`，需求创建日期保留。
- 文档按当前代码和使用者已确认的验收同步，历史失败或阶段记录不再表示当前归档阻断；仍未修复的缺陷和专项测试欠账保留在 [统一已知问题](../../KNOWN_ISSUES.md)。
- 本次仅维护和移动文档，没有修改业务代码、真实配置或账户，没有提交、合并或推送。
