# TASKS

日期：2026-08-30
需求：per-provider 代理 + 弹窗检测按钮 + route 字段防线 + race 镜像

格式：`[ ] 任务名 | 优先级 | 估时 | 依赖`

设计见 `DESIGN.md`，接口契约见 `API_SPEC.md`。**两份文档是任务的规范来源，
实现前必须读完对应章节**，尤其 §1.6（脱敏与 sentinel 必须成对）与 §2.3（不得写健康缓存）。

---

## A. 配置层

```
[ ] T1 config.Provider 加 Proxy 字段 + 校验 + 常量 | P0 | 1h | —
```
- 落点：`internal/config/config.go`
- 加 `Proxy string` 字段，yaml/json 双 `omitempty`（理由同 `userAgent`，写一行注释指向它）
- 加常量 `maxProviderProxyRunes = 256`、`ProxyPasswordKeepSentinel`（与 `APIKeyKeepSentinel` 并列）
- `validate` 里镜像 `userAgent` 那段（`config.go:826` 附近）：空值放过；非空时
  `url.Parse` 必须成功、scheme ∈ {`http`,`https`,`socks5`}、host 非空、长度与控制字符检查
- `applyDefaults` **不得**碰该字段
- `config.example.yaml` 加注释示例：带认证 / 不带认证两种，并写明「留空 = 用全局
  `HTTPS_PROXY`，全局也没有则直连」
- 验收：`go test ./internal/config/` 绿；新增 table-driven 用例覆盖合法值、非法 scheme、
  空 host、超长、控制字符

```
[ ] T2 新增 internal/httpclient 包（Pool） | P0 | 2h | —
```
- 落点：新建 `internal/httpclient/httpclient.go` + `_test.go`
- 契约见 `API_SPEC.md` §3、`DESIGN.md` §1.3
- 默认 client 的 Transport 参数必须与现有 `cmd/gateway/main.go:69` **逐项一致**
  （`Proxy: http.ProxyFromEnvironment`、`MaxIdleConns: 200`、
  `MaxIdleConnsPerHost: 100`、`IdleConnTimeout: 90s`，不设 `Client.Timeout`）
- `For("")` 必须返回与 `Default()` **同一个指针**
- 键用 `url.Parse` 后重新拼的规范化串
- 并发安全（`For` 会被多请求 goroutine 同时调）
- `Reconcile(active)` 丢弃未引用的 client 并对其 Transport 调 `CloseIdleConnections()`
- 验收：测试覆盖「同一代理两次取到同一指针」「空串取到 Default 同一指针」
  「非法串返回 error 且不返回 Default」「Reconcile 后被丢弃的键再取会新建」
  「并发 For 无 race」（用 `-race` 跑）

```
[ ] T3 代理密码脱敏 + PUT sentinel 还原（必须同一任务完成） | P0 | 2.5h | T1
```
- **不可拆分**：只做脱敏不做 sentinel 会让任何一次 UI 保存把脱敏值写进磁盘、
  真密码永久丢失（详见 `DESIGN.md` §1.6）
- 落点 1：`internal/config/config.go` 的 `RedactYAML`/`redactAPIKeys` 路径，
  按字段名认出 `proxy`，只把**密码位**换成 `ProxyPasswordKeepSentinel`
- 落点 2：`cmd/gateway/main.go:1813` 附近构造 JSON view 的路径，同样处理
- 落点 3：`cmd/gateway/main.go:1915` 那段 sentinel 还原逻辑旁，加代理密码的还原：
  磁盘上同名 provider 有密码则填回；没有则 **400 拒绝**（凭空 sentinel），
  沿用宽松判据（不要求 baseUrl/format 未变）
- 无密码的代理串（`http://proxy:7890`、只有用户名的）**不脱敏**
- 验收：
  - round-trip 测试：磁盘有带密码代理 → GET 返回 sentinel → 原样 PUT 回去 →
    磁盘密码**未变**（这条是本任务的核心断言）
  - 无密码代理串 GET 后原样返回，不被改写
  - 凭空带 sentinel 被 400 拒绝
  - YAML 路径与 JSON view 路径**各有**一条断言（漏一条等于没做）

## B. 出网路径接入

```
[ ] T4 providerhealth 签名改 resolver + ProbeAdHoc | P0 | 2h | T2
```
- 落点：`internal/providerhealth/health.go`
- `CheckAll` / `CheckProvider` 的 `client *http.Client` 参数改为 `ClientResolver`
  （类型定义见 `API_SPEC.md` §2.2），解析发生在 `checkAll` 的循环体内
- 新增 `ProbeAdHoc`：过 `sem`，**不写 `storeStatus`**、不参与 generation/fingerprint
- 解析失败 → 该 provider 结果为 `error` + 解析错误串，**不退回默认 client**
- 同步改 `cmd/gateway/main.go:168` 的 `providerHealthRuntime` 接口
- 验收：`go test ./internal/providerhealth/` 绿；新增测试断言
  「不同 provider 用不同 proxy 时 resolver 被按各自的值调用」
  「ProbeAdHoc 调用前后 `statuses` 缓存不变」

```
[ ] T5 vision 改 resolver | P1 | 1h | T2
```
- 落点：`internal/vision/vision.go`（`New` 签名 + `t.httpClient` 用法）
- `Translate` 已收 `*config.Provider`，就地解析
- `visionRuntime` 接口签名不变
- 验收：`go test ./internal/vision/` 绿（现有测试注入 client 的方式要跟着改成注入 resolver）

```
[ ] T6 转发与模型查询按 provider 取 client | P0 | 1.5h | T2
```
- 落点：`cmd/gateway/main.go`
- `forwardAttempt` 里 `Options.HTTPClient`（现 `main.go:922`）改为按当前候选 provider 解析
- `handleProviderModels`（现 `main.go:1416`）解析后传给 `fetchUpstreamModels`
  （后者签名不变）
- 解析失败：转发路径按该候选构建失败处理（**跳过该候选、继续下一个**，
  与既有「候选构建失败则跳过」一致）；模型查询路径返回 502 带错误串
- `server` 结构体的 `httpClient` 字段换成 pool（或并存，由实现决定，但
  **不得留下任何仍直接用单一 client 出网的路径**）
- 验收：`go test ./cmd/gateway/` 绿；新增测试断言「provider 配了代理时，
  转发确实经过该代理」（用 httptest 起一个假代理，断言它收到了 CONNECT 或绝对 URL 请求）

```
[ ] T7 Pool 接入 applyRuntimeConfig 的 Reconcile | P0 | 0.5h | T2,T6
```
- 落点：`cmd/gateway/main.go` 的 `applyRuntimeConfig`（现 `main.go:1992`）
- 与 `qm` / `providerHealth` / `breaker` / `selector` 并列，收集新配置里所有非空
  `provider.Proxy` 作为 active 集合调 `pool.Reconcile`
- 验收：测试断言「热重载把某 provider 的代理从 A 改成 B 后，A 的 client 被丢弃」

## C. 探测端点

```
[ ] T8 新增 POST /api/providers/probe | P0 | 2.5h | T1,T3,T4
```
- 落点：`cmd/gateway/main.go`（路由注册紧跟 `/api/providers/models` 之后 + 新 handler）
- 完整契约见 `API_SPEC.md` §1，逐条对照实现，尤其：
  - 边界检查四项（method/Origin/Content-Type 只收 JSON/body 限制）
  - apiKey 与代理密码两个 sentinel 的回查与「凭空 sentinel → 400」
  - 探测失败（DNS/连接/超时）返回 **200 + status=error**，不是 5xx
  - 没拿到 `sem` 槽位返回 **503**，不是 200+error
  - 返回前对 message 做凭据脱敏
- 校验复用 `config` 包已有逻辑与文案，不新写判据
- 验收：`go test ./cmd/gateway/` 绿；测试覆盖上述每条错误终态，
  以及「探测结果未写进健康检测缓存」

```
[ ] T9 实测 Go 是否支持 socks5h scheme | P2 | 0.5h | T1
```
- 结论写进 `KNOWN_ISSUES.md` 或直接反映到 T1 的 scheme 白名单
- 支持则加入白名单并补测试；不支持或无法确认则**保持不加**，
  并在 `config.example.yaml` 注释里说明只支持 `http/https/socks5`
- 不要凭印象下结论——没实测就按「不支持」处理

## D. 前端

```
[ ] T10 provider 代理字段铺到全部重建点 | P0 | 2h | T1
```
- 五处重建点清单见 `DESIGN.md` §1.8
- 外加 `index.template.html` 弹窗输入框；表格加代理列（**只显示 host，不显示 userinfo**）
- 弹窗里加一行说明：密码位显示占位符表示沿用已存密码，要改密码需整串重填
- **改完必须重建产物**：`go run ./cmd/webbuild`，并确认 `-check` exit=0
- 验收：`TestFrontendCoversAllProviderFields` 绿（加字段后它会先失败，全部补齐才转绿）；
  `TestRepoArtifactMatchesSources` 绿

```
[ ] T11 弹窗检测按钮 | P0 | 2h | T8,T10
```
- 落点：`cmd/gateway/web/src/app/11-providers.js.part` + `index.template.html`
- 行为见 `DESIGN.md` §2.6：用**当前表单值**拼请求体；结果只渲染在弹窗内、
  **不合并进 `health.providerHealth`**；`probing` 布尔防重入；名称/URL 为空先本地挡
- 编辑场景且用户未重填密钥（`apiKeyConfigured && !apiKey`）时发 sentinel
- 复用首页那套状态配色与文案渲染
- **改完必须重建产物并确认 `-check` exit=0**
- 验收：手动过三条路径（新增/编辑/克隆）各测一次，贴出实际结果

## E. 防线与工具链

```
[ ] T12 route 字段白名单覆盖度防线 | P1 | 1.5h | —
```
- 落点：`cmd/gateway/main_test.go`，新增 `TestFrontendCoversAllRouteFields`
- **只检查 `02-config-normalize.js.part` 的 routes map 回调区间**，
  不检查 `12-routes.js.part`（`saveRoute` 有意按条件省字段，纳入必然误报）
- 作用域必须精确到 `const routes` 起的那段（理由见 `DESIGN.md` §3.2：
  按整个函数体判断会因 providers 段的同名字段假通过）
- 字段清单用反射从 `config.Route` 的 json tag 取
- 剥注释后判断（复用现有 `stripJSComments`）
- 断言恰好找到 1 处受检区间
- **自检必须做**：手动删掉 `normalizeConfig` 里的 `strategy` 一行，确认测试变红；
  再删掉 `route.provider`，确认也变红（这一条专门验证作用域没过宽）；改回后确认转绿。
  自检过程与结果要贴出来。
- 验收：上述自检的实际输出

```
[ ] T13 race 测试改用 Debian 版 Go 镜像 | P1 | 0.5h | —
```
- 落点：`CLAUDE.md` 的「Build, Test, and Development Commands」一节
- 加一条 race 专用命令，用 `golang:1.23`（预装 gcc，无需联网装包）；
  其余命令仍用 `golang:1.23-alpine`（体积小、启动快）
- 说明为何 race 单独用 Debian 镜像（alpine 缺 gcc，装包依赖 alpine 源，
  实测两次分别卡 18/17 分钟）
- 验收：该命令实跑一次并贴出输出

## F. 文档

```
[ ] T14 README / CLAUDE.md 同步 per-provider 代理 | P1 | 1h | T1,T6,T7
```
- README：新增 per-provider 代理说明，含三级回退表、带认证形式、
  **以及「无法只为某个 provider 关掉全局代理，用 `NO_PROXY` 排除」这条非目标**
- CLAUDE.md「Key Mechanisms」的「出网代理」条目改写：
  从「唯一出口」改为「按 provider 解析、Pool 缓存连接池」，
  并保留 `ProxyFromEnvironment` 零值坑与 `sync.Once` 那两条既有结论
- CLAUDE.md 请求流程一节：`applyRuntimeConfig` 那条补上 pool 也要 Reconcile
- 验收：文档与实现一致，无残留的「唯一出口」表述

---

## 依赖顺序建议

```
T1 ─┬─ T3 ─┐
    ├─ T9  │
    └─ T10 ┤
T2 ─┬─ T4 ─┴─ T8 ── T11
    ├─ T5
    └─ T6 ── T7
T12、T13 独立，可先做（不依赖任何前置）
T14 收尾
```

建议实现顺序：T12 → T13（独立、快、先把防线和工具链就位）→ T1 → T2 → T3 →
T4 → T6 → T7 → T5 → T8 → T10 → T11 → T9 → T14

## 全局验收门槛（每个任务完成都要过，最终一并确认）

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -l ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23 go test -race ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go run ./cmd/webbuild -check
```

`gofmt -l` 必须**无输出**；其余 exit=0。

**改了 `cmd/gateway/web/src/` 就必须重建产物**：漏建会让 `TestRepoArtifactMatchesSources`
失败。本项目已连续两轮出现「改了前端源码、忘了重建 index.html」，
且自述「测试全绿」而实际未跑。**自报全绿不作为验收依据，只认实跑输出。**
