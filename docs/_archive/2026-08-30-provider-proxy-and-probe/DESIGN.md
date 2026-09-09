# DESIGN：per-provider 代理 + 弹窗检测按钮

日期：2026-08-30
需求来源：用户四条

1. route 字段加白名单覆盖度防线
2. race 测试改用 Debian 版 Go 镜像（不再依赖 alpine 源装 gcc）
3. 每个 provider 可单独设置代理，三级回退
4. 添加/编辑/克隆 provider 的弹窗里加检测按钮，语义同首页检测

本文档只定义「做什么/为什么」与接口契约，不含实现代码。实现见 `TASKS.md`。

---

## 一、需求 3：per-provider 代理

### 1.1 配置字段与语义

新增 `Provider.Proxy`：

```go
Proxy string `yaml:"proxy,omitempty" json:"proxy,omitempty"`
```

两处 `omitempty` 的理由与既有 `userAgent` 完全相同（yaml 少了会给每个 provider 落一行
`proxy: ""` 纯噪音；json 少了前端拿到一堆空字段）。空值是合法终态，`applyDefaults` 不碰。

三级回退（用户明确口径）：

| provider.proxy | 环境变量 `HTTPS_PROXY` 等 | 实际行为 |
|---|---|---|
| 非空 | 任意 | 用 provider 配的代理 |
| 空 | 有 | 用环境变量的全局代理 |
| 空 | 无 | 直连 |

**后两级不需要写任何判断逻辑**：`http.ProxyFromEnvironment` 在环境变量为空时返回
`(nil, nil)`，语义就是直连。所以「空 → 全局 → 直连」天然由现有共享 client 覆盖，
只有「非空」这一档需要新建 client。这是本设计能保持小的关键。

### 1.2 为什么必须做 client 池，不能按请求建 Transport

现状 `cmd/gateway/main.go:69` 的 `httpClient` 是**所有出网路径的唯一出口**，
带 `MaxIdleConns: 200 / MaxIdleConnsPerHost: 100 / IdleConnTimeout: 90s`。

每个请求新建 `http.Transport` 会让连接池彻底失效：每次请求重新 TCP + TLS 握手，
且旧 Transport 的空闲连接直到 GC 才释放，高并发下会堆出大量 CLOSE_WAIT。
必须按「代理配置」为键缓存 client——同一个代理的所有 provider 共用一个连接池。

### 1.3 新增包 `internal/httpclient`

放 `internal/` 而不是 `cmd/gateway`：`providerhealth` 与 `vision` 都要按 provider 取
client，放 `cmd` 会让这两个包反向依赖 `main`。

契约（伪代码，仅表意）：

```
package httpclient

// Pool 按代理配置缓存 *http.Client，同一代理共用连接池。
type Pool struct { ... }

// NewPool 建池。默认 client 的 Transport 用 http.ProxyFromEnvironment，
// 参数与现有 main.go 的 httpClient 逐项一致（MaxIdleConns 200 /
// MaxIdleConnsPerHost 100 / IdleConnTimeout 90s，不设 Client.Timeout）。
func NewPool() *Pool

// Default 返回走全局代理（或直连）的 client。
func (p *Pool) Default() *http.Client

// For 返回该代理对应的 client；proxyURL 为空时等价于 Default()。
// 代理串非法时返回 error——不静默退回 Default()：那会让「配了代理但没生效」
// 表现为直连成功或直连失败，两种都看不出是代理配错了，与既有
// ProxyFromEnvironment 漏设那个坑同一性质。
func (p *Pool) For(proxyURL string) (*http.Client, error)

// Reconcile 丢弃不再被任何 provider 引用的 client，
// 并对被丢弃的 Transport 调 CloseIdleConnections()。
func (p *Pool) Reconcile(active map[string]struct{})
```

`Reconcile` 是必需的，不是锦上添花：热重载把某 provider 的代理从 A 改成 B 后，
A 的 client 会连着空闲 TCP 连接留在 map 里，既不再被使用也不释放，
直到 `IdleConnTimeout` 才断。反复改配置会持续堆积。调用点与既有
`queue` / `breaker` / `selector` 三者并列放在 `applyRuntimeConfig` 里，遵循同一套惯例。

**`For` 的 error 只是防御**：合法性在 `config.validate` 就该拦住（见 1.5），
运行时还能拿到非法值意味着有 bug，此时报错比猜测更可取。

### 1.4 四条出网路径都要改（这是本需求跨模块的根因）

现状四条路径共用一个 `s.httpClient`，改成按 provider 解析后，
其中两条的**函数签名必须变**——它们现在收的是「一个 client」，而需要的是
「一个能按 provider 解析 client 的东西」。

| 路径 | 位置 | 现状 | 改法 |
|---|---|---|---|
| ① 转发 | `main.go:922` `Options.HTTPClient` | `s.httpClient` | 在 `forwardAttempt` 里按当前候选的 provider 解析 |
| ② 模型列表 | `main.go:1416` `fetchUpstreamModels` | 传 `s.httpClient` | 在 `handleProviderModels` 里解析后传入，签名不变 |
| ③ 健康检测 | `providerhealth.CheckAll/CheckProvider` | 参数 `client *http.Client` | **签名改为 resolver**，见下 |
| ④ vision 翻译 | `vision.Translator.httpClient` | 构造时存一个 | **改为 resolver**，见下 |

③ 的关键：`checkAll` 内部是**遍历所有 provider 并发探测**，每个 provider 的代理可能不同，
所以解析必须发生在循环体内，无法在调用方一次性解析完再传进来。

④ 的关键：`Translate` 已经收 `*config.Provider` 参数，具备就地解析的条件，
但它现在用的是构造时存下的 `t.httpClient`。

统一引入一个函数类型，两个包各自声明同形状的参数，**不**让它们 import
`internal/httpclient`（保持零耦合、便于测试注入）：

```
// 语义：入参是 provider.Proxy 的值，出参是该走的 client。
type ClientResolver func(proxyURL string) (*http.Client, error)
```

`main` 侧用 `pool.For` 直接满足该签名。测试里注入一个返回 `httptest` client 的闭包即可，
现有测试改动量最小。

### 1.5 校验（`config.validate`）

镜像既有 `userAgent` 那段的写法：

- 空值放过（合法终态 = 不配置）。
- 非空时 `url.Parse` 必须成功。
- scheme 白名单：`http`、`https`、`socks5`。
  - `socks5h` **暂不纳入**：Go 的 `http.Transport` 是否原生识别该 scheme 我没有实测证据，
    没核实就写进白名单等于给用户一个可能静默失效的选项。
    列为独立任务实测后再决定（见 TASKS T9）。
- host 必须非空（`http://` 这种只有 scheme 的串要拒）。
- 长度上限与控制字符：与 `userAgent` 同一套判据，常量单列
  `maxProviderProxyRunes = 256`。

### 1.6 安全：代理串里的密码必须脱敏 + 配套 sentinel（P0，成对出现）

代理 URL 的合法形式包含 userinfo：`http://user:pass@proxy.example.com:7890`。
企业代理确实常带认证，不能只支持无认证形式。

现有脱敏只认 `apiKey` 字段名（`config.RedactYAML` → `redactAPIKeys`
按 key 匹配，`config.go:667`）。不处理的话代理密码会出现在
`GET /api/config` 的返回、前端配置页输入框、前端 YAML 预览里。

**关键：脱敏和 sentinel 必须成对做，只做一半会造成数据损坏。**
前端 `normalizeConfig` 读全部字段、`configPayload` 写回全部字段，
所以只要 GET 返回的是脱敏值，任何一次 UI 保存都会把脱敏后的假值写进磁盘、
真密码永久丢失——这正是 apiKey 当年引入 `APIKeyKeepSentinel` 的原因，
不能在新字段上把同一个坑再踩一遍。

口径：

1. **无密码就不脱敏。** `proxy: "http://proxy:7890"` 里没有秘密，原样返回，
   用户能在界面上正常看到和编辑。userinfo 只有用户名没有密码时同样不脱敏
   （用户名不是凭据）。这覆盖绝大多数情况，代价为零。
2. **有密码时只把密码位换成 sentinel**，保留 scheme/user/host/port：

   ```
   磁盘：  http://alice:s3cret@proxy.corp.com:7890
   返回：  http://alice:__AI_GATEWAY_KEEP_PROXY_PASSWORD__@proxy.corp.com:7890
   ```

   新增常量 `config.ProxyPasswordKeepSentinel`，与 `APIKeyKeepSentinel` 并列。
   保留 host 是有意的：用户需要看出自己配的是哪个代理，只有密码该藏。
3. **`PUT /api/config` 侧解析还原**：密码位等于 sentinel 时，取磁盘上同名 provider
   的原密码填回。沿用 apiKey 那条已定的宽松判据（`main.go:1919-1923`）——
   不要求 host 未变，改了代理地址仍可保留密码；磁盘上没有已存密码却带 sentinel 则
   **拒绝并报错**，那是「凭空带 sentinel」，不能静默吞成空密码。
4. **两条输出路径都要覆盖**：`RedactYAML` 的 YAML 路径，以及 `GET /api/config`
   构造 JSON view 的路径（`main.go:1813` 附近）。漏一条就等于没做。

UX 取舍：输入框里会直接显示 `__AI_GATEWAY_KEEP_PROXY_PASSWORD__` 这串字面量。
不美观但零额外状态——不引入第二个 `proxyConfigured` 字段、不做「显示态/模型态」双表示。
弹窗里给一行说明：密码位是占位符表示沿用已存密码，要改密码就整串重填。

### 1.7 明确的非目标

**无法表达「这个 provider 强制不走代理」。** 按用户定的口径，空值 = 继承全局，
所以配了全局代理时，某个走内网的 provider 没有办法只为自己关掉代理。
现成逃生口是 `NO_PROXY`（按 host 排除，语义与 curl/docker 一致，README 已有说明），
足够覆盖该场景，因此**不**新增 `proxy: "direct"` 之类的特殊值——
那会让空值的语义从「一档」变成「两档」，且与环境变量的既有约定不一致。
README 里写清这条即可。

### 1.8 前端改动点

`Provider` 结构体加字段后，`TestFrontendCoversAllProviderFields`（`main_test.go:938`）
会**自动开始失败**，直到 5 个重建点全部补上 `proxy`。这条防线正是为此建的，
本需求把它当验收机制用，不需要额外人工清单：

| 文件 | 位置 |
|---|---|
| `00-state.js.part` | 初始 state 里的 providerForm 默认值 |
| `02-config-normalize.js.part` | `normalizeConfig` 的 providers 循环 |
| `07-nav-payload.js.part` | `configPayload` |
| `10-preview-yaml.js.part` | `providersYaml`（按 `userAgent` 的写法条件 push） |
| `11-providers.js.part` | `addProvider` / `editProvider` / `duplicateProvider` / `saveProvider` 四处 |

外加 `index.template.html` 的弹窗输入框，以及 provider 表格里是否展示代理列
（**建议展示**：代理配错是「明明配了却没生效」类问题，表格里能一眼看到有没有配、配的是哪个，
比进弹窗逐个点开强；只展示 host，不展示 userinfo）。

---

## 二、需求 4：弹窗检测按钮

### 2.1 为什么不能复用 `POST /api/providers/health?provider=`

现有单点检测端点从**已落盘配置**取 provider（`main.go:1409` 读 `cfg.Providers[name]`）。
弹窗里三种场景全都不成立：

| 场景 | 复用现有端点的结果 |
|---|---|
| 新增 | 磁盘上没这个名字 → 404 |
| 克隆 | 同上 → 404 |
| 编辑（改了 url/key/UA/代理还没保存） | 探测的是**改前的旧值**，绿了红了都与用户眼前填的无关 |

第三种最危险：它会给出一个看起来可信、实际无关的结论。所以必须新增一个
**接受请求体里的临时 provider 配置**的端点。

### 2.2 语义与首页检测完全一致（用户口径「和首页检测功能一样」）

复用 `providerhealth.checkOne` 的判据，不另立一套：
2xx → `ok`「可用」；401/403 → `error`「鉴权失败」；404/405 → `warn`「检测端点不可用，但上游可达」；
5xx → `error`「上游服务异常」；其他 → `warn`。返回 `httpCode` / `latencyMs` / `endpoint` / `message`。

`checkOne` 目前是包私有的纯函数，正好可以直接复用，需要导出一个不写缓存的入口。

### 2.3 关键约束：绝不能写健康检测缓存

`Checker.statuses` 是给首页表格用的、按 provider 名索引的**已落盘配置**的状态。
把临时配置的探测结果写进去，会让首页显示一个磁盘上并不存在的配置的状态——
用户保存前关掉弹窗，那个绿灯还留在表格里，属于纯误导。

所以新入口要：**过 `sem` 并发节流**（它是真出网请求，绕开全局节流等于给了一个刷上游的口子）、
**不调 `storeStatus`**、**不参与 generation/fingerprint 逻辑**。

### 2.4 apiKey 的 sentinel 处理

编辑场景下用户没重填密钥时，前端表单里 `apiKey` 为空、`apiKeyConfigured` 为 true。
此时探测必须能用上磁盘里的真密钥，否则检测结果必然是 401，毫无意义。

做法与 `PUT /api/config` 一致：前端在这种情况下发 `apiKey: APIKeyKeepSentinel`，
后端按请求体里的 `name` 去磁盘配置找原密钥；找不到则拒绝（凭空 sentinel）。
新增与克隆场景前端已把 `apiKeyConfigured` 置 false、要求重填，天然不会发 sentinel，
两边规则统一。代理密码的 sentinel 同理。

### 2.5 安全：SSRF 面

该端点让调用方指定任意 `baseUrl` 并使网关去请求它。评估结论是**不新增实质权限**：

- 管理面只允许 loopback Origin，且同一管理面上的 `PUT /api/config`
  本来就能写任意 baseUrl 再触发探测，权限严格更强。
- `checkOne` 只回状态码/耗时/固定文案，**不回上游响应体**，因此不构成读取原语。

要落实的是：新端点必须注册进 `handle()` 的特殊路由分支，从而继承既有的
method / Origin / media-type / body-limit 检查，不能绕开那层边界。

### 2.6 前端行为

弹窗底部加「检测」按钮，与「保存」并列。

- 点击时用**当前表单值**（不是 `this.config`）拼请求体，这是整个功能的要点。
- 结果**只渲染在弹窗内**，不合并进 `health.providerHealth`——那个 map 属于已落盘 provider，
  混入未保存配置的结果会污染首页表格（同 2.3 的理由，前后端都要守住）。
- 独立的 `probing` 布尔防重入，沿用 `checkOneProvider` 的写法。
- 名称/URL 为空时前端先挡，给出与 `saveProvider` 一致的提示，不发无意义请求。
- 检测不改变任何持久状态，因此**不需要**用户确认，也不该顺带保存。

---

## 三、需求 1：route 字段防线

### 3.1 现状：route 和 provider 的风险不对称

- `configPayload` 里 routes 走 `JSON.parse(JSON.stringify(...))` 深拷贝
  （`07-nav-payload.js.part`），自动保全全部字段，**这条路径不会丢字段**。
- 真正逐字段重建的只有 `normalizeConfig`（`02-config-normalize.js.part:23-50`）。
  历史上 `route.strategy` 就是在这里丢的，第 32 行注释是那次的伤痕记录。
- `12-routes.js.part` 的 `addRoute` / `editRoute` / `duplicateRoute` / `saveRoute`
  是**表单态**，与落盘结构不是一对一：`saveRoute` 有意按条件省字段
  （单候选不写 `strategy`、`failover` 是默认值不落盘、`vision` 仅启用时写）。

**结论：防线只应覆盖 `normalizeConfig`。** 照 provider 那条的判据要求
「每个重建点都提到每个字段」，会让 `saveRoute` 必然误报——它省字段是设计而非缺陷。

### 3.2 作用域必须精确到 routes 的 map 回调，不能是整个函数

`normalizeConfig` 同一个函数体里，providers 循环也出现 `provider` / `model` 之外的字段名，
且 `provider`、`model` 这两个词在 providers 段与 routes 段都会出现。
若按整个函数体判断，即使 `route.provider` 被删掉，测试也会因为 providers 段里的同名
字面量而假通过——和第一版 provider 防线栽在注释上是同一类错误。

所以定位方式：从 `const routes` 起，按括号/花括号配对取到该语句结束，
在这段区间内剥注释后逐字段检查。字段清单用反射从 `config.Route` 的 json tag 取，
新增字段自动纳入。

`strategy` / `targets` / `vision` 在 `normalizeConfig` 里是**在对象字面量之外**条件赋值的，
所以不能沿用 provider 那套「取对象字面量」的 span 逻辑，必须取整个 map 回调区间。

### 3.3 断言命中数

routes 只有 1 个受检点，断言「恰好找到 1 处」。provider 那条用的是
`sites < 5` 下界，理由相同：定位逻辑失效时循环会全部跳过、测试假绿。

---

## 四、需求 2：race 测试改用 Debian 版 Go 镜像

### 4.1 起因

`-race` 需要 cgo/gcc，`golang:1.23-alpine` 不带，装 `gcc musl-dev` 时连续两次卡在
alpine 镜像源（18 分钟、17 分钟无进展），导致 race 验证长期缺失。

`golang:1.23`（Debian 基础）**预装 gcc**，无需联网装包即可跑 `-race`。
代价是镜像体积（约 800 MB），但拉取源是 Docker Hub 而不是 alpine 源。

### 4.2 关于「用 ai-gateway-gomod 卷缓存 apk」的评估

用户问过是否可行。技术上无冲突（gomod 挂 `/go/pkg/mod`，apk 缓存在 `/var/cache/apk`，
路径不重叠），但**解决不了实际问题**：卡住的是首次下载，而缓存要等一次成功下载之后才有内容，
首次下不来则缓存恒为空。附带缺点是两类不相干缓存混在一个卷里、以后无法分别清理。
因此不采用该方案，改用预装 gcc 的镜像（这也是用户选定的方向）。

### 4.3 落点

CLAUDE.md 的「Build, Test, and Development Commands」一节补一条 race 专用命令，
说明为何 race 用 Debian 镜像、其余仍用 alpine（体积小、启动快）。

---

## 五、风险与验收

| 风险 | 说明 | 对策 |
|---|---|---|
| 代理密码脱敏做了、sentinel 漏了 | 任何一次 UI 保存都会把脱敏值写进磁盘，真密码永久丢失 | §1.6 两件事必须同一个任务内完成；补 round-trip 测试 |
| 脱敏漏了 JSON view 路径 | YAML 预览安全了、配置页仍明文 | 两条路径各一条测试 |
| 按请求新建 Transport | 连接池失效、CLOSE_WAIT 堆积 | client 必须走 Pool 缓存；补「同一代理两次取到同一 client」测试 |
| Pool 不 Reconcile | 改代理后旧 client 连着空闲连接不释放 | 与 queue/breaker/selector 并列接入 `applyRuntimeConfig` |
| 弹窗检测写了健康缓存 | 首页显示未落盘配置的状态 | 新入口不碰 `storeStatus`；补测试断言缓存未变 |
| route 防线作用域过宽 | 因 providers 段同名字段假通过 | 必须精确到 routes map 回调区间；补「删掉字段应失败」的自检 |
| `ProxyFromEnvironment` 的 `sync.Once` | 进程内改 env 不生效，无法用单测稳定覆盖全局代理档 | 全局代理档做端到端验证，不写脆弱单测（既有 CLAUDE.md 已记录该结论） |

验收门槛（四道，缺一不可）：
`gofmt -l` 无输出、`go vet ./...`、`go test ./...`、`webbuild -check` exit=0。
另加 `go test -race ./...`（本需求同时把它变得可执行）。

