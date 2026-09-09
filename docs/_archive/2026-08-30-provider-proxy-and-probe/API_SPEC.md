# API_SPEC

日期：2026-08-30

本文件定义本需求涉及的接口契约。只描述输入/输出与错误终态，不含实现。

---

## 一、新增：`POST /api/providers/probe`

用请求体里给出的**临时** provider 配置探测上游，不读磁盘配置、不写健康检测缓存。
供 provider 弹窗（新增/编辑/克隆）的检测按钮使用。

### 1.1 为什么不是现有端点

`POST /api/providers/health?provider=` 读已落盘配置，弹窗三种场景均不适用
（新增/克隆 404、编辑探到旧值）。详见 `DESIGN.md` §2.1。

### 1.2 边界检查（必须与既有管理端点一致）

注册进 `main.go` `handle()` 的特殊路由分支，位置紧跟 `/api/providers/models` 之后，
依次检查：

| 检查 | 要求 | 不满足时 |
|---|---|---|
| method | 只允许 `POST` | `writeMethodNotAllowed(w, http.MethodPost)` |
| Origin | `isCrossSiteRequest(r)` 为真则拒 | `writeForbiddenOrigin(w)` |
| Content-Type | `requestMediaType(r)` 必须是 JSON | 415 `unsupported_media_type` |
| body 大小 | `readBodyLimited(r, maxConfigBodyBytes)` | 413（沿用既有错误映射） |

Content-Type 只收 JSON（不复用 `isAllowedConfigMediaType` 的 JSON+YAML）：
这是个结构化小对象，没有 YAML 的需求，收窄输入面。

### 1.3 请求体

```jsonc
{
  "name":      "ar-gh",        // 必填。用于 sentinel 回查磁盘已存密钥；也用于回显
  "baseUrl":   "https://...",  // 必填
  "format":    "anthropic",    // 必填，枚举同 config：anthropic | openai | openai-responses
  "apiKey":    "sk-...",       // 可选。空 = 不带鉴权头；等于 sentinel 时回查磁盘
  "userAgent": "claude-cli/…", // 可选。空 = 不设该头（与转发路径第三档一致）
  "proxy":     "http://…"      // 可选。空 = 走全局代理/直连
}
```

字段语义与 `config.Provider` 逐一对应。**不接受** `maxConcurrent` / `maxPerSecond` /
`maxQueueWait`：探测不过队列，收了也没用，收窄输入面。

### 1.4 sentinel 语义（两个字段各一套，规则相同）

| 字段 | sentinel 常量 | 处理 |
|---|---|---|
| `apiKey` | `config.APIKeyKeepSentinel` | 取磁盘上 `name` 对应 provider 的 `apiKey` |
| `proxy` 的密码位 | `config.ProxyPasswordKeepSentinel` | 取磁盘上同名 provider 代理串里的密码 |

规则与 `PUT /api/config`（`main.go:1915-1930`）保持一致：

- 磁盘上该名字**有**已存值 → 用它。
- 磁盘上该名字**没有**已存值 → **400 拒绝**，message 说明「无法保留已有 apiKey：
  磁盘上没有已存密钥」。不能静默吞成空值，那会让用户看到一个基于空密钥的 401，
  以为是上游拒绝，实际是网关吞了 sentinel。
- 不要求 baseUrl / format 未变（沿用 apiKey 那条已定的宽松判据）。

### 1.5 校验

`baseUrl` / `format` / `userAgent` / `proxy` 复用 `config` 包已有的校验逻辑与文案，
**不新写一套判据**。校验失败返回 400 `config_validation_error`，message 用 config 包的原文。
`name` 只做非空与长度检查（`maxProviderNameRunes`）；它不落盘，无需查重。

### 1.6 成功响应 `200`

结构与 `providerhealth.Status` 一致，字段名沿用现有 JSON tag，
便于前端复用首页那套渲染与配色：

```jsonc
{
  "status": {
    "name":      "ar-gh",
    "status":    "ok",              // unchecked | ok | warn | error
    "endpoint":  "https://…/v1/models",
    "httpCode":  200,
    "latencyMs": 431,
    "message":   "可用",
    "checkedAt": "2026-08-30T22:00:00+08:00"
  }
}
```

外层包一层 `status` 而不是直接返回 Status：留出后续加字段的余地，
且与既有端点返回 `{"providers": {...}}` 的包裹风格一致。

判据完全复用 `providerhealth` 现有那套（2xx→ok、401/403→鉴权失败、
404/405→warn 端点不可用但上游可达、5xx→error、其他→warn）。

**探测本身失败（DNS、连接被拒、超时、代理不可达）不是 HTTP 错误**：
仍返回 200，`status.status = "error"`、`message` 为具体错误串。
与首页检测一致——那是「检测到上游不可用」这一有效结论，不是网关处理失败。

### 1.7 错误响应

| 状态 | code | 场景 |
|---|---|---|
| 400 | `config_validation_error` | 必填缺失、字段非法、凭空带 sentinel |
| 405 | — | 非 POST |
| 403 | — | 跨站 Origin |
| 413 | — | body 超限 |
| 415 | `unsupported_media_type` | Content-Type 非 JSON |
| 503 | `gateway_error` | 未拿到并发槽位（`sem` 满且 ctx 结束） |

503 这条对应 `DESIGN.md` §2.3 的节流：探测要过 `Checker.sem`。
不写成 200+error，因为那是网关侧排队没排上，不是上游的结论——
记成上游异常会让用户以为上游挂了（与 `CheckProvider` 里那段注释同一理由）。

### 1.8 响应必须不泄露凭据

`message` 里可能出现 `http.Transport` 的 dial 错误，其中含代理地址。
返回前对 message 做一次凭据脱敏（代理串的密码位换掉），
避免代理密码经错误信息回到前端或进请求日志。

---

## 二、`providerhealth` 包的接口变更

### 2.1 新增导出：不写缓存的临时探测

```
// ProbeAdHoc 用给定的临时 provider 配置探测上游，返回检测结果。
//
// 与 CheckProvider 的差别：不读配置、不写 statuses 缓存、不参与
// generation/fingerprint 判定。仅共用 sem 并发节流与 checkOne 判据。
// ok=false 表示没拿到并发槽位（调用方应回 503），不表示上游不可用。
func (c *Checker) ProbeAdHoc(ctx context.Context, p *config.Provider, resolve ClientResolver) (Status, bool)
```

### 2.2 签名变更：client → resolver

`CheckAll` / `CheckProvider` 现在收 `client *http.Client`，改为收解析器。
理由见 `DESIGN.md` §1.4：`checkAll` 内部遍历所有 provider 并发探测，
每个 provider 的代理可能不同，解析必须在循环体内发生。

```
// ClientResolver 按 provider 的代理配置返回该用的 HTTP client。
// 入参是 provider.Proxy 的原始值，空串表示走全局代理/直连。
type ClientResolver func(proxyURL string) (*http.Client, error)

func (c *Checker) CheckAll(ctx context.Context, cfg *config.Config, resolve ClientResolver) map[string]Status
func (c *Checker) CheckProvider(ctx context.Context, cfg *config.Config, resolve ClientResolver, name string) (Status, bool)
```

`providerHealthRuntime` 接口（`main.go:168`）同步改签名。

解析失败时（防御路径，配置校验本应拦住）该 provider 的结果是
`status = "error"`、message 为解析错误串——不能退回默认 client，
那会让「代理配错」表现成「直连成功」，比报错更难查。

### 2.3 `checkOne` 内部变化

多带一个代理相关的行为：无。`checkOne` 仍只负责发请求与判定状态，
代理已经体现在传入的 `*http.Client` 上，函数体除签名外不变。

---

## 三、`internal/httpclient` 包（新增）

见 `DESIGN.md` §1.3 的契约。补充约定：

- `NewPool()` 的默认 client 参数必须与现有 `main.go:69` 逐项一致，
  这是行为等价的前提；不要顺手调参。
- `For("")` 必须返回与 `Default()` **同一个** `*http.Client` 指针，
  不是等价的新实例——否则空配置的 provider 各自建池，等于没有连接复用。
- 键用**规范化后**的代理串（`url.Parse` 后重新拼），
  避免 `http://p:7890` 与 `http://p:7890/` 建出两个池。
- 并发安全：`For` 会被多个请求 goroutine 同时调用，走双检锁或 `sync.Map`。

---

## 四、`vision` 包的接口变更

`Translator` 现在构造时存 `httpClient`（`vision.go:38/59`），改为存 resolver。
`Translate` 已收 `*config.Provider`，具备就地解析条件。

```
func New(c *cache.Cache, qm *queue.Manager, resolve ClientResolver, directMode bool) *Translator
```

`visionRuntime` 接口（`main.go:163`）的 `Translate` 签名不变（它本来就收 provider）。

解析失败的处理与 `providerhealth` 一致：不退回默认 client，按翻译失败走既有失败路径。

---

## 五、配置字段（`config.Provider`）

```
Proxy string `yaml:"proxy,omitempty" json:"proxy,omitempty"`
```

- 两处 `omitempty` 的理由同 `userAgent`（见 `config.go:820` 附近注释）。
- 空值是合法终态，`applyDefaults` 不得填默认值。
- 校验见 `DESIGN.md` §1.5；新增常量 `maxProviderProxyRunes = 256`。
- 新增常量 `ProxyPasswordKeepSentinel`，与 `APIKeyKeepSentinel` 并列声明。

`config.example.yaml` 同步加注释示例（含带认证与不带认证两种形式，
以及「留空 = 用全局 HTTPS_PROXY，全局也没有则直连」的说明）。
