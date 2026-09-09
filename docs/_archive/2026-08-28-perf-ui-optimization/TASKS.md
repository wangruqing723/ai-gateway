# 性能与 UI 优化任务清单

日期：2026-08-28
需求 slug：`perf-ui-optimization`
设计文档：[DESIGN.md](./DESIGN.md) · 接口约定：[API_SPEC.md](./API_SPEC.md)

---

## 格式说明

```
[ ] 任务名 | 优先级 | 估时 | 依赖
```

优先级口径：

- **P0**：低风险、见效直接，或属安全加固，本轮必做。
- **P1**：需要重排布局或改数据流，收益大但要肉眼验收。
- **P2**：结构性重构与打磨，可单独排期，不阻塞 P0/P1。

估时是单人实现 + 自测的净工时，不含 code review 往返。

---

## 阶段一（P0）：低风险减负与加固

这一阶段全部是「不动布局、不改数据契约」的改动，可以独立提交、独立回滚。

```
[ ] S1-1 观测端点去掉 MarshalIndent          | P0 | 0.5h | 无
[ ] S1-2 /health 的 GetStats 与 ReadMemStats 加 TTL 缓存 | P0 | 2h   | 无
[ ] S1-3 Collector.Logs / Metrics 去掉全量拷贝 | P0 | 2.5h | 无
[ ] S1-4 前端轮询加可见性与 tab 门控          | P0 | 1.5h | 无
[ ] S1-5 只读观测端点补 Host 白名单校验        | P0 | 2h   | 无
[ ] S1-6 P95 低样本量不出数，改显示样本数      | P0 | 2h   | S1-1
```

### S1-1 观测端点去掉 `MarshalIndent`

- **目标**：六个观测端点的 JSON 序列化从 `json.MarshalIndent(x, "", "  ")` 改为 `json.Marshal(x)`。
- **文件**：`cmd/gateway/main.go`
- **改法要点**：涉及 `handleMetrics`（`main.go:1270`）、`handleLogs`（`:1299`）、`handleHealth`（`:1071`）、`handleModels`（`:1359`）、`handleProviderHealthCheck`（`:1141` 与 `:1152` 两处）、`handleBreakerReset`（`:1118`）。这些响应的唯一消费者是前端 JS，缩进是纯开销；`/api/logs?limit=100` 每 5 秒返回一次 100 条 20+ 字段记录，缩进能让体积多出三四成。
- **接口约束**：JSON 字段名、结构、HTTP 状态码一律不变；`content-length` 仍按实际字节数写。
- **验收标准**：`go test ./...` 全绿；对同一份数据比较改动前后的 `content-length`，确认明显下降且 `json.Unmarshal` 得到的对象结构一致。

### S1-2 `/health` 的 `GetStats` 与 `ReadMemStats` 加 TTL 缓存

- **目标**：消除 `/health` 每次调用触发的 stop-the-world 与 SQLite 全表扫。
- **文件**：`cmd/gateway/main.go`（`handleHealth`，`:1044-1050`）、`internal/cache/cache.go`（`GetStats`，`:147`）
- **改法要点**：`runtime.ReadMemStats(&m)` 会 STW；`cache.GetStats()` 在持有 cache 全局 `mu` 的前提下跑 `COUNT(*)` + `SUM(LENGTH(description))` + `os.Stat`，而那把锁正是 vision 翻译读写缓存要抢的同一把。前端轮询间隔 5 秒，多开一个标签页就翻倍。给两者各加一层 TTL 缓存（建议 10 秒），TTL 内直接返回上次结果。缓存放在 `server` 上还是 `cache.Cache` 内部由实现者定，但**不得在持有 cache `mu` 的情况下等待 TTL 判断**，否则等于没优化。
- **接口约束**：`/health` 响应的 `cache.total`、`cache.contentSize`、`memory.heapAllocMB`、`memory.sysMB` 字段名与类型不变；允许数值最多滞后一个 TTL 周期。
- **验收标准**：并发打 `/health` 时 `GetStats` 的实际 SQL 执行次数受 TTL 约束（用计数器或测试替身断言）；`go test -race ./...` 全绿。

### S1-3 `Collector.Logs` / `Metrics` 去掉全量拷贝

- **目标**：两个方法不再无条件拷贝整个 1000 条环形缓冲。
- **文件**：`internal/metrics/metrics.go`（`Logs`，`:318`；`Metrics`，`:342`；`snapshot`/`snapshotLocked`）
- **改法要点**：`Logs` 先 `c.snapshot()` 拷完整缓冲再倒序过滤取前 100，改成在 `RLock` 下从最新往旧遍历、凑够 `Limit`（含 `Offset`）即停。`Metrics` 也拷一份，但那份 `records` 只在「桶里没有 provider 数据」的兜底分支（`:401-413`）和 `recentErrors(records, 8)`（`:418`）用得上，常规路径整份白拷——改成按需取，或只取最近若干条供 `recentErrors` 用。
- **接口约束**：`Logs` 的返回顺序（时间倒序）、`Offset`/`Limit` 语义、`Limit <= 0 || > 200` 时回落 100 的行为保持不变；`Metrics` 返回的 `Response` 各字段口径不变。
- **验收标准**：`internal/metrics` 现有测试全绿并补充：`Offset` 跨越环形缓冲回绕点时结果与旧实现一致；缓冲未满、刚满、已回绕三种状态各覆盖一次。

### S1-4 前端轮询加可见性与 tab 门控

- **目标**：标签页不可见或当前不在监控/日志页时，停止 5 秒轮询。
- **文件**：`cmd/gateway/web/index.html`（`init()` 里的 `setInterval`，`:1413`）
- **改法要点**：当前是无条件 `window.setInterval(() => this.refreshMonitor(false), 5000)`，在 providers/routes/settings 页也照样拉 `/health` + `/api/metrics` + `/api/logs`，后台标签页放一天就是一整天的三连请求。加 `document.visibilitychange` 门控，并让非监控/日志页跳过轮询；页面重新可见或切回监控页时立即补拉一次，避免显示陈旧数据。注意 `refreshMonitor(false)` 的 `false` 是「不显示错误、不转刷新图标」的既有语义（见 `:1614` 注释），不要改。
- **接口约束**：手动点刷新按钮的行为不变，仍走 `refreshMonitor(true)`。
- **验收标准**：浏览器 Network 面板确认——切到后台标签页后请求停止，切回后立即恢复并补拉一次；停在「全局设置」页时无这三个请求。

### S1-5 只读观测端点补 Host 白名单校验

- **目标**：堵住 DNS rebinding 下 `/health`、`/api/metrics`、`/api/logs` 可被跨站页面读取的口子。
- **文件**：`cmd/gateway/main.go`（路由分发，`:243`/`:252`/`:305`）、`cmd/gateway/helpers.go`（新增校验函数，与 `isCrossSiteRequest`、`isLoopbackHostname` 同处，`:90-118`）
- **改法要点**：`/api/config` 的 PUT 与 `/api/providers/*` 都过了 `isCrossSiteRequest`（`:266`/`:281`/`:296`/`:1488`/`:1511`），但这三个只读端点没有。普通跨站 fetch 读不到（无 CORS 头），但 DNS rebinding 场景下攻击者页面与网关同源，`/api/logs` 会吐出 `clientIp`、`keyFingerprint`、provider 名、模型名、错误全文。**加一层 Host 校验比逐个补 Origin 检查更彻底**：要求 `r.Host` 的 hostname 是 loopback 或等于配置的 `host`，不满足直接拒。校验放在 `handle()` 靠前处，让它覆盖所有端点（含推理端点）。
- **接口约束**：本机 CLI 客户端（无 Origin、Host 为 `127.0.0.1:7789` 或 `localhost:7789`）必须继续可用——这是现有兼容性底线，不能回归。拒绝时复用 `writeForbiddenOrigin` 或新增同风格的 JSON 错误。
- **验收标准**：`cmd/gateway` 新增测试——Host 为 `evil.example.com` 时三个端点返回 403；Host 为 `127.0.0.1:7789`、`localhost:7789`、`[::1]:7789` 时正常返回；配置了非 loopback `host` 时该 host 也放行。
- **注意**：这条改的是安全边界，实现前先确认不影响 Docker 场景（容器内 `host: "0.0.0.0"`，见 CLAUDE.md 的 Security 小节），必要时把 `0.0.0.0` 配置下的放行口径写进 KNOWN_ISSUES.md 待决策。

### S1-6 P95 低样本量不出数，改显示样本数

- **目标**：避免把「一两个样本算出的 P95」当成可信数字展示。
- **文件**：`internal/metrics/metrics.go`（`latencyBounds` `:29`、`percentile` `:58`）、`cmd/gateway/web/index.html`（P95 卡片 `:725-732`、`metricDuration` `:2219`）
- **改法要点**：`percentile` 返回的是命中桶的上界，UI 显示「2.0s」时真实值可能落在 1001–2000ms 任意处。叠上 15 分钟窗口（`DefaultWindowMinutes = 15`，`:17`）在低流量下常常只有个位数样本，那时 P95 基本等于最慢的那一条。两处一起改：后端在 `Summary` 里带上窗口内的延迟样本数，前端样本数低于阈值（建议 20）时不显示 P95、改显示样本量，并在卡片副标题标注「按桶上界近似」。
- **接口约束**：新增字段用 `omitempty` 之外的稳定写法（前端要靠它判断），已有 `p50/p95/p99LatencyMs` 字段保留不删。
- **验收标准**：`internal/metrics` 测试断言新字段等于窗口内实际样本数；前端在样本数 0 / 5 / 50 三种情况下分别显示占位、样本量、P95。

---

## 阶段二（P1）：布局重排与信息密度

这一阶段依赖 S2-1 先做——它解锁的宽度是后续几条的前提。**每条都需要肉眼验收**，建议在 1280px / 1440px / 1920px 三档宽度各看一遍。

```
[ ] S2-1 配置预览栏改为可折叠抽屉            | P1 | 4h   | 无
[ ] S2-2 配置页统计卡片压成 header chip       | P1 | 2h   | S2-1
[ ] S2-3 statusMessage 移到右下角 toast 栈    | P1 | 3h   | 无
[ ] S2-4 日志表加行展开详情面板              | P1 | 4h   | 无
[ ] S2-5 表格列宽 hack 退役与回归            | P1 | 3h   | S2-1, S2-2
[ ] S2-6 tooltip 统一走 showTip              | P1 | 2.5h | 无
[ ] S2-7 熔断列琥珀色语义去重                | P1 | 1h   | 无
[ ] S2-8 修 1024–1280px 区间的双滚动区        | P1 | 2h   | S2-1
```

### S2-1 配置预览栏改为可折叠抽屉

- **目标**：把常驻的右侧「配置预览」栏改成默认折叠，表格拿回全宽。
- **文件**：`cmd/gateway/web/index.html`（`aside`，`:673-687`）
- **改法要点**：这是本轮**杠杆最大的一条**。那个 `xl:col-span-3` 常驻栏拿走 25% 宽度，而它只是只读 YAML 片段——「全局设置」页本来就有完整的 YAML 编辑器 tab（`:690-704`）。代价全写在代码注释里了：`w-1/2 max-w-0`、`min-w-[600px]`、「操作列被裁掉」、「去掉 https:// 前缀省宽度」、「三个数值合并成一列限流」（见 `:601-604`、`:619-625`、`:632-634`）。改成默认折叠的抽屉或浮层，保留 `copyPreview()` 入口（`:2552`）。左侧内容区从 `xl:col-span-9` 改为全宽。
- **接口约束**：`configPreview()` / `providersPreview()` / `routesPreview()` / `fullConfigPreview()` 的输出内容不变，只改容器。
- **验收标准**：抽屉展开/折叠状态在切 tab 时的行为明确且一致；折叠时 providers 与 routes 表格在 1280px 下无横向滚动条、操作列完整可见。

### S2-2 配置页统计卡片压成 header chip

- **目标**：回收配置页顶部约 130px 垂直空间。
- **文件**：`cmd/gateway/web/index.html`（统计卡片 `:283-316`、header chip 区 `:331-334`）
- **改法要点**：Providers / Routes / Mode / Timeout 四张卡片在 providers、routes、settings 三页常驻。但前两个数字就是下方表格的行数（`providerCount()` / `routeCount()`），后两个在「全局设置」表单里本来就能直接改。1080p 笔记本上 header 64px + 卡片 130px + 表头，留给表格的高度已经很紧。header 右侧已有 `lan` / `schedule` chip 的现成模式，并进去即可。
- **接口约束**：四个数值仍要可见，不能直接删掉。
- **验收标准**：1080p 高度下 providers 表可见行数明显增加；四个数值在 header 区可读，窄屏（sm 以下）有合理的降级显示。

### S2-3 statusMessage 移到右下角 toast 栈

- **目标**：消除顶部横幅遮挡内容的问题，与「未保存」浮层统一为右下角 toast 栈。
- **文件**：`cmd/gateway/web/index.html`（statusMessage `:268-279`、dirtyToast `:224-239`）
- **改法要点**：`statusMessage` 是 `fixed` + `top: 5rem` 的顶部横幅，会盖住内容顶部——这一点代码注释里自己已经承认了（`:220`：「顶部浮层会盖住统计卡片（statusMessage 就是这个毛病）」）。未保存提示已因此改成右下角可拖浮层，statusMessage 却还留在原地。两者统一到右下角上下堆叠。**必须保留的既有行为**：错误停留 20 秒、成功 5 秒（`showStatus`，`:1853-1861`）；手动关闭按钮；`role="status"` + `aria-live="polite"`；dirtyToast 的拖拽与 localStorage 位置记忆（`:2048-2121`），尤其 `dirtyToastStyle()` 必须继续返回对象而不是字符串（`:2039-2047` 注释说明了原因：字符串写法会覆写 `x-show` 的 `display:none`）。
- **接口约束**：`showStatus(message, type)` 的调用签名不变——它有 20+ 处调用点。
- **验收标准**：连续触发多条提示时堆叠不重叠、不互相顶掉；与 dirtyToast 同时出现时两者都可读；拖过 dirtyToast 后刷新页面位置仍记住，且 `canSave()` 转 false 时浮层确实消失（这是 `:2043` 记录过的回归点）。

### S2-4 日志表加行展开详情面板

- **目标**：启用后端已采集但前端完全没展示的 8 个字段，同时解决错误文本被截断的问题。
- **文件**：`cmd/gateway/web/index.html`（日志表 `:1013-1068`）、参考 `internal/metrics/metrics.go`（`RequestLog`，`:110-138`）
- **改法要点**：`RequestLog` 里 `Route`、`TargetModel`、`QueueWaitMs`、`ResponseBytes`、`UpstreamStatus`、`Vision`、`Method`、`Path` 八个字段前端一个都没显示。同时日志表 8 列全 `nowrap` + `truncate`，排查时最需要的错误文本恰恰截得最狠（`w-full max-w-0 truncate`，`:1059`）。点击行展开成详情面板一举两得。**`queueWaitMs` 和 `upstreamStatus` 是重点**——它们决定「慢在队列还是慢在上游」这个判断。
- **接口约束**：`/api/logs` 已经返回这些字段，无需改后端。表格现有列与筛选行为不变。
- **验收标准**：展开行显示全部字段且错误全文不截断；展开态在轮询刷新后不丢（或明确定义为「刷新后收起」并保持一致）；键盘可展开/收起。

### S2-5 表格列宽 hack 退役与回归

- **目标**：S2-1、S2-2 腾出空间后，清理那批为窄宽度写的补丁，并确认没有回归。
- **文件**：`cmd/gateway/web/index.html`（providers 表 `:597-668`、routes 表 `:337-433`、监控表 `:777-893`）
- **改法要点**：注释里逐条记录了各个 hack 的成因与实测数字（`:601-604` 的「并发/限速/队列等待合并成一列」、`:619-625` 的 `w-1/2 max-w-0`、`:632-638` 的去掉 `https://` 前缀、`:404-405` 的 `w-px`、`:784` 的 `min-w-[660px]` 与「放到 720 会在 1440 宽度下多出 9px 横向滚动」）。**逐条判断哪些可以退役、哪些仍需保留**，退役时同步更新或删除对应注释——注释里的实测数字是在旧布局下量的，留着会误导后人。不要一次性全删，按表格逐个来。
- **接口约束**：无。
- **验收标准**：1280px / 1440px / 1920px 三档下三张表都无意外横向滚动、操作列完整；退役的 hack 对应注释已更新。

### S2-6 tooltip 统一走 `showTip`

- **目标**：消除同一张表里两种 tooltip 行为并存。
- **文件**：`cmd/gateway/web/index.html`（`showTip` `:1998`、`hideTip` `:2010`、tooltip 容器 `:1258-1262`）
- **改法要点**：`showTip` 已实现即时 tooltip，注释里明确说原生 `title` 有 1–2 秒延迟且不可调（`:1996`）——但它只用在监控表的健康/熔断两列。provider 全名（`:627`）、baseUrl 全文（`:637`）、日志完整时间戳与请求 ID（`:1033`）、客户端 IP·Key 来源·指纹（`:1041`）、错误全文（`:1060`）、转移轨迹（`:1058`）全都还挂在 `title` 上，而这些恰恰是排查时最常悬停的。改成统一走 `showTip`。注意 tooltip 容器用 `fixed` 定位是有原因的（`:1256` 注释：表格外层是 `overflow-x-auto`，绝对定位气泡会被裁掉）。
- **接口约束**：多行 tooltip（如 `:1041` 用 `\n` 分隔的三行）在 `showTip` 下要能正确换行显示。
- **验收标准**：所有列悬停即显示、无延迟；表格横向滚动时气泡不被裁切；键盘 focus 也能触发（现有 chip 已有 `tabindex="0"` + `@focus`，新增处保持一致）。

### S2-7 熔断列琥珀色语义去重

- **目标**：让同一行的两个 chip 颜色不再撞语义。
- **文件**：`cmd/gateway/web/index.html`（`providerHealthClass` `:2286`、`breakerClass` `:2334-2341`）
- **改法要点**：健康列的 `warn`（上游可达但非 200）和熔断列「闭合但有失败计数」（`:2340`）都是琥珀底色，同一行两个琥珀 chip 含义完全不同。熔断徽章文本已经写了分数（`正常 3/3`，`:2323`），信息量够了——这一态底色回到中性，把琥珀留给真正的 half_open / open。
- **接口约束**：`breakerLabel` 的文本与 `breakerResettable` 的判据（`:2272`）不变——后者与后端 `ResetAll` 的计数条件一致，改了会让重置按钮的出现时机对不上。
- **验收标准**：闭合有失败计数、half_open、open 三态颜色互不相同；与健康列 `warn` 不再同色。

### S2-8 修 1024–1280px 区间的双滚动区

- **目标**：修掉这个区间内「配置预览」可能被裁在视口外且无滚动入口的问题。
- **文件**：`cmd/gateway/web/index.html`（`main` `:281`、内滚容器 `:340` 与 `:673`、分栏 `:339`）
- **改法要点**：`main` 在 `lg:` 就锁死高度不滚（`lg:h-[calc(100vh-4rem)] lg:overflow-hidden`），配置区的内滚容器也是 `lg:overflow-y-auto`，但分栏是 `xl:grid-cols-12` 才生效。也就是说 1024–1279px 之间 grid 还是单列、外层 `overflow-hidden` + `flex-1` 已经在限高，第二行的「配置预览」可能整块被裁在视口外。把内滚门控和分栏门控统一到 `xl:`。
- **接口约束**：无。
- **验收标准**：**先在 1100px 左右实机确认问题存在**（这条是从 class 组合推出来的，未经浏览器验证），再改；改后 1024–1280px 区间所有内容可达。若 S2-1 落地后此问题自然消失，把结论记入 KNOWN_ISSUES.md 并关掉本条。

---

## 阶段三（P2）：结构性重构

这一阶段不阻塞前两阶段，可单独排期。**S3-1 建议在 S2 全部验收后再动**，否则会与布局改动大面积冲突。

```
[ ] S3-1 index.html 拆分与构建流程            | P2 | 8h   | S2 全部
[ ] S3-2 Tailwind Play CDN 换构建期 CSS       | P2 | 5h   | 无
[ ] S3-3 字体与图标本地化                     | P2 | 3h   | 无
[ ] S3-4 导航项抽成数据驱动                   | P2 | 1.5h | 无
[ ] S3-5 无障碍补齐                           | P2 | 3h   | S2-6
```

### S3-1 `index.html` 拆分与构建流程

- **目标**：把 2938 行单文件拆开，同时保住 `go:embed` 单文件部署。
- **文件**：`cmd/gateway/web/index.html`、`cmd/gateway/main.go`（`//go:embed web/index.html web/vendor/*`，`:44`）
- **改法要点**：`gatewayApp()` 单个对象约 1650 行、60+ 方法。拆成几个 Alpine 组件或多个 `<script>` 片段、构建期拼回单文件。**`go:embed` 的 pattern 必须与产物路径匹配**（CLAUDE.md 明确要求），拼装步骤要能在 CI 与本地 `docker build` 中稳定跑。同时保住开发热加载：`AI_GATEWAY_WEB_DIR` 存在时从磁盘读 `index.html` 并经 SSE 刷新（`handleIndex` `:1372`、`injectDevReload` `:1417`），拆分后这条路径不能断。
- **接口约束**：产物仍是单个 `web/index.html`；`/vendor/*` 路径不变。
- **验收标准**：`docker build` 产出的镜像页面功能与拆分前一致；开发模式热加载仍可用；`go test ./...` 全绿。

### S3-2 Tailwind Play CDN 换构建期 CSS

- **目标**：去掉 407KB 运行时编译的 JS 及其连带的类名扫描隐患。
- **文件**：`cmd/gateway/web/vendor/tailwindcss.js`、`cmd/gateway/web/index.html`（`tailwind.config` `:13-47`、自定义 CSS `:114-125`）
- **改法要点**：`vendor/tailwindcss.js` 是 Play CDN 版，在浏览器里运行时编译 CSS。代码里已有三处注释在绕它的坑——`gw-spin`、`gw-grab`、`gw-grabbing` 只能自己写 CSS，因为 JIT 扫不到只存在于 Alpine `:class` 表达式里的类名（`:114-121`）。改成构建期生成 CSS，能同时去掉 407KB JS、运行时编译开销和这一类隐患。注意 `tailwind.config` 里的自定义色板与 `borderRadius` 覆盖要完整迁移。
- **接口约束**：视觉呈现不得变化——这是纯替换，任何色差或间距变化都算回归。
- **验收标准**：构建产物 CSS 覆盖所有实际用到的类（含 Alpine `:class` 表达式里的动态类名，需显式 safelist 或改写成静态类）；页面在三档宽度下与改动前逐屏比对无差异。

### S3-3 字体与图标本地化

- **目标**：去掉对 `fonts.googleapis.com` / `fonts.gstatic.com` 的外部依赖。
- **文件**：`cmd/gateway/web/index.html`（`:10-12`）、`cmd/gateway/helpers.go`（CSP，`:124`）
- **改法要点**：页面从 Google Fonts 加载 Inter / JetBrains Mono 和 Material Symbols 图标字体，CSP 也专门为它开了口子。**Material Symbols 是 ligature 字体，离线或内网环境下字体拉不到，界面上所有图标会退化成 `monitoring`、`dns`、`alt_route` 这样的英文单词**——对一个定位为「本地网关」的服务，这个依赖该去掉。方案二选一：字体 woff2 子集本地化放进 `web/vendor/`，或图标改内联 SVG。字体本地化后同步收紧 CSP 的 `style-src` / `font-src`，去掉 Google 域名。
- **接口约束**：`go:embed web/vendor/*` 已覆盖新增字体文件，无需改 pattern；图标视觉不变。
- **验收标准**：断网启动容器，页面图标与字体正常显示；CSP 头里不再含 `fonts.googleapis.com` / `fonts.gstatic.com`；README / CLAUDE.md 中 vendor 说明同步更新（`web/vendor/README.md` 记录了固定版本与 SHA）。

### S3-4 导航项抽成数据驱动

- **目标**：消除桌面侧栏与移动抽屉的重复定义。
- **文件**：`cmd/gateway/web/index.html`（侧栏 `:142-171`、移动抽屉 `:250-264`）
- **改法要点**：同一组 5 个 tab 写了两遍，样式还不同（侧栏 `border-l` 高亮，抽屉 `btn-secondary`）。加 tab 要改两处、容易漏，视觉上也不一致。抽成一个 `navItems` 数组（`{ id, label, icon, fillIcon }`）+ `x-for` 渲染。注意 providers / routes 两项的点击还会顺带重置 `editMode = 'visual'`，这个副作用要保留。
- **接口约束**：`activeTab` 取值集合不变（`monitor` / `providers` / `routes` / `logs` / `settings`）。
- **验收标准**：桌面与移动两处导航行为一致；移动端点击后抽屉自动关闭的既有行为保留。

### S3-5 无障碍补齐

- **目标**：补上明确可补的几处，不做超出范围的承诺。
- **文件**：`cmd/gateway/web/index.html`（侧栏 `:142-171`、图标按钮多处、表头多处、chip `:815-820` 与 `:829-834`）
- **改法要点**：侧栏 tab 按钮缺 `role="tab"` / `aria-selected`；图标按钮只有 `title` 没有 `aria-label`（如 `:408-422` 的上移/下移/编辑/复制/删除、`:754-774` 的三个监控操作）；表头缺 `scope="col"`；`tabindex="0"` 的 chip 没有 `aria-describedby` 关联到 tooltip 容器。都是小改动。
- **接口约束**：不改变任何交互行为。
- **验收标准**：以上四类问题在对应元素上均已补齐。**完整的 WCAG 结论需要辅助技术实测与人工评审，本任务不声称达成合规**，只交付这几处明确修正。

---

## 委托给 Codex 时的统一约束

以下几条对**所有**任务生效，实现前请通读：

1. **注释与文档用中文**，与仓库现有风格一致（CLAUDE.md 明确要求）。
2. **Go 代码走 `gofmt`**，配置字段用 lower camelCase；若新增配置项，同步更新 `config.example.yaml`、`internal/config.applyDefaults`、`validate` 三处。
3. **本轮任务不新增配置字段**。S1-6 若确实需要新字段，只加在 `metrics.Summary` 响应结构里，不进 `config.yaml`。
4. **不要清理顺手看到的无关代码**。`index.html` 里那些看似冗长的注释记录了具体 bug 的成因（表单恢复污染、`:style` 字符串覆写 `x-show`、字面 NUL 字节让 grep 失效等），**不得删除**；只有当对应 hack 被本轮改动真正退役时才更新它。
5. **验证在 Docker 容器内跑**，容器名固定 `ai-gateway-dev-verify`：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -l ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
```

`-race` 必须用完整 `golang:1.23` 镜像（race detector 依赖 cgo，alpine 变体不含 C 工具链）：

```bash
docker run --rm -v "$PWD":/work -w /work -v ai-gateway-go-mod-cache:/go/pkg/mod golang:1.23 go test -race ./...
```

6. **前端改动需实机验收**，不只看代码：桌面三档宽度（1280 / 1440 / 1920）+ 移动 390x844，参照上一轮 hardening 的验收口径。
7. **默认不自动提交**。改动完成后向用户汇报，由用户拍板 `git commit`；按仓库分支策略提交到 `dev` 而非 `master`。
8. **发现设计问题不要擅自改架构**，记入 `docs/2026-08-28-perf-ui-optimization/KNOWN_ISSUES.md` 等 Claude 决策。

---

## 来源与验证状态

17 条建议来自通读源码：后端 `cmd/gateway/`（`main.go` 1826 行、`helpers.go` 299 行）与 `internal/*`（metrics、cache、queue、breaker、balancer、config、proxy、converter、vision、providerhealth、router），前端 `cmd/gateway/web/index.html` 全 2938 行。

**均为静态阅读结论，未实机运行服务或开浏览器验证。** 其中 S2-8（1024–1280px 双滚动区）是从 Tailwind class 组合推导出的，置信度低于其余各条，实施前需先在浏览器中确认问题存在。文中所有行号是 2026-08-28 的快照，实施时以实际代码为准。
