# 性能与 UI 布局优化设计

日期：2026-08-28
状态：规划完成，待委托实现
来源：对当前 `dev` 分支（HEAD `24bf842`）全量通读后提出的 17 条优化建议

> **本文档的证据边界**：全部结论来自静态阅读源码，**没有实机运行服务、没有开浏览器验证、没有做性能测量**。
> 因此不写任何「优化后提升 X%」的量化承诺。凡涉及运行时表现的改法，验收标准都落在「行为可观察的变化」上，
> 而不是数字指标。第 17 条（响应式断点）明确标注为**待实机验证**，实现前必须先在浏览器里量一眼。

---

## 1. 背景与范围

网关的核心链路（converter / router / queue / proxy / balancer / breaker）已经过 2026/07 的
hardening 轮次，`TODO-review.md` 记录的 16 项历史问题全部关闭。本轮不碰核心转发语义，
只针对两个此前没有系统审查过的面：

- **观测面**：`/health`、`/api/metrics`、`/api/logs` 三个端点的开销，以及前端 5 秒轮询对它们的放大
- **前端面**：`cmd/gateway/web/index.html`（2938 行，单文件 202KB）的布局约束与样式一致性

两个面有一处交汇：前端 `setInterval(5000)` 无条件轮询，把观测端点的单次开销乘上了一个常数。
所以「减少端点单次开销」和「降低调用频次」是同一个问题的两端，都在本轮范围内。

### 1.1 明确不在本轮范围

- 核心转发链路的语义改动（converter / proxy / balancer / breaker 的判据与边界）
- `internal/queue` 的准入与限速算法
- 新增功能（本轮只做优化与加固，不加能力）
- 把前端换成构建型框架（Vue / React / Svelte）——见 5.2 的取舍说明

---

## 2. 现状分析

### 2.1 观测端点：六处 `MarshalIndent`

`cmd/gateway/main.go` 里所有面向前端的 JSON 响应都用 `json.MarshalIndent(x, "", "  ")`：

| 端点 | 位置 | 调用方 |
|---|---|---|
| `/api/metrics` | `main.go:1270` | 前端每 5s |
| `/api/logs` | `main.go:1299` | 前端每 5s |
| `/health` | `main.go:1071` | 前端每 5s |
| `/v1/models` | `main.go:1359` | 客户端按需 |
| `/api/providers/health` | `main.go:1141`、`1152` | 手动触发 |
| `/api/providers/breaker/reset` | `main.go:1118` | 手动触发 |

这些响应的唯一消费者是 `fetch().json()`，缩进对它没有意义。
`/api/logs?limit=100` 每次返回 100 条、每条 20+ 字段的 `RequestLog`，缩进带来的体积增量最大。

### 2.2 `/health` 的两个重开销动作

`handleHealth`（`main.go:1027`）每次调用都做：

1. **`runtime.ReadMemStats(&m)`**（`main.go:1044-1045`）——会 stop-the-world。
2. **`s.cache.GetStats()`**（`main.go:1049`）——`cache.go:147`，在持有 `Cache.mu` 的情况下跑
   `COUNT(*)` + `SUM(LENGTH(description))` + `os.Stat`。

第 2 点的代价不只是三次查询：`Cache.mu` 是 cache 包的**单一全局互斥**（`cache.go:23`，注释说明
是为对齐 Node 版 `withDB` 队列语义而串行化所有 DB 操作）。`vision` 翻译走 `Get`/`Set`
（`cache.go:93`、`cache.go:107`）抢的是同一把锁。也就是说健康检查的全表扫会和图片识别的缓存读写互相阻塞。

前端轮询频率 5 秒（`index.html:1413`），多开一个浏览器标签页就翻倍。

### 2.3 `Collector` 的全量拷贝

- **`Logs`**（`metrics.go:318`）先 `c.snapshot()` 拷完整 1000 条环形缓冲，再倒序过滤、取前 100。
- **`Metrics`**（`metrics.go:342`）也在 RLock 内 `snapshotLocked()` 拷一份，但这份 `records`
  只在两处用得上：`len(resp.Providers) == 0` 的兜底分支（`metrics.go:401-413`）和
  `recentErrors(records, 8)`（`metrics.go:418`）。常规路径（桶里有数据）整份拷贝白费。

`snapshotLocked`（`metrics.go` 内）每次 `make([]RequestLog, 0, len(c.records))` 加两次 append，
`RequestLog` 是含 20+ 字段、多个 string 的结构体，1000 条的拷贝不是可忽略量。

### 2.4 前端轮询不看可见性也不看 tab

`index.html:1410-1413`：

```js
async init() {
    await this.loadConfig();
    await this.refreshMonitor();
    window.setInterval(() => this.refreshMonitor(false), 5000);
```

`refreshMonitor`（`index.html:1616`）在非 logs tab 时并发拉 `/health` + `/api/metrics` + `/api/logs`
三个端点。问题有三层：

1. **不看 `document.visibilityState`**：后台标签页照样轮询。
2. **不看 `activeTab`**：在 providers / routes / settings 三个配置页也照样拉监控数据，而这些页面
   完全不渲染 `health` / `metrics`。
3. **`setInterval` 不因页面卸载或错误停止**，也没有句柄可停。

### 2.5 只读观测端点缺跨站门

`handle()` 里对写操作和敏感操作都过了 `isCrossSiteRequest`：

| 端点 | 跨站检查 | 位置 |
|---|---|---|
| `POST /api/providers/health` | ✅ | `main.go:266` |
| `POST /api/providers/breaker/reset` | ✅ | `main.go:281` |
| `GET /api/providers/models` | ✅ | `main.go:296` |
| `PUT /api/config` | ✅ | `main.go:1488` |
| `POST /api/config/reload` | ✅ | `main.go:1511` |
| 推理端点 | ✅ | `main.go:338` |
| **`GET /api/metrics`** | ❌ | `main.go:243` |
| **`GET /api/logs`** | ❌ | `main.go:252` |
| **`GET /health`** | ❌ | `main.go:305` |
| **`GET /v1/models`** | ❌ | `main.go:315` |

普通跨站 `fetch` 读不到响应体（没有 CORS 响应头，浏览器拦截），所以这不是直接的信息泄露。
真实风险面是 **DNS rebinding**：攻击者控制的域名先解析到自己的 IP、页面加载后重绑到 `127.0.0.1`，
此时浏览器认为同源，`Origin` 头也是攻击者域名但 `isCrossSiteRequest` 对 `GET /api/logs` 根本没被调用。
`/api/logs` 的响应含 `clientIp`、`keyFingerprint`、`keySource`、provider 名、模型名、
错误全文（`metrics.go:110-138`）。

**注意**：这里不泄露 API Key 明文（`keyFingerprint` 是 SHA-1 前 8 位，`helpers.go:289`），
所以定性为「该补的加固」，不是高危漏洞。

### 2.6 P95 是直方图桶上界，UI 未提示

`latencyBounds`（`metrics.go:29`）是固定 19 个边界：
```
0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000, 600000
```

`percentile`（`metrics.go:58`）返回**命中桶的上界**，不是插值估计。UI 显示 "2.0s" 时真实 P95
可能在 1001–2000ms 的任意位置。

叠加第二个因素：统计窗口默认 15 分钟（`metrics.go:17`，注释已说明取 15 而非 1 是为了避免
低流量下的噪声），但低流量本地网关在 15 分钟内可能仍只有个位数样本。
`percentile` 的 rank 算法 `(total*p + 99) / 100`：total=3、p=95 时 rank=3，
P95 直接等于最慢的那一条。UI 上（`index.html:725-732`）没有任何关于样本量或精度的提示。

### 2.7 前端：右侧配置预览栏吃掉 25% 宽度

`index.html:339` 的配置区栅格是 `xl:grid-cols-12`，主内容 `xl:col-span-9`（`:340`），
右侧 `<aside>` `xl:col-span-3`（`:673`）常驻「配置预览」——一个只读的 YAML 片段
（`configPreview()`，`index.html:2433`）。

代价直接写在代码注释里。`index.html:601-604`：

> 列宽预算很紧：这块在 `xl:col-span-8` 里，右侧还有配置预览栏。
> 并发/限速/队列等待三个数值合并成一列「限流」，否则每列都要留出表头「队列等待」的宽度，
> 表格会宽到必须横向滚动、操作列被裁掉。

以及 `index.html:619-625`：

> 名称列若按内容定宽（`w-px`），一个超长 provider 名就能把列撑到近千像素，后面几列全被顶到容器外、
> 操作列直接被裁掉……写死各 50% 才能两边都留出可读长度。

同类注释还有 `:378-382`（候选列 `w-full max-w-0` + 内层 `min-w-0` 的两层 hack）、
`:404-405`（操作列 `w-px`）、`:636`（去掉 `https://` 前缀省宽度）、`:784`（min-w 保持 660 的精确计算）。

这批 hack 都是**为了在剩下的 75% 里塞进宽表格**。注释里还留了一处线索表明 span 值改过：
`:337-338` 写「9/3 而不是 8/4」，而 `:601` 的注释仍说「这块在 `xl:col-span-8` 里」——
说明为了给表格挤宽度，已经从 8/4 调到 9/3 一次了。

预览栏本身的必要性也可疑：「全局设置」页已有完整的 YAML 编辑器 tab（`index.html:690-704`），
能看到并直接编辑原文。

### 2.8 前端：配置页四张统计卡片的垂直占用

`index.html:283-316` 在 providers / routes / settings 三个页面常驻四张卡片：
Providers 数、Routes 数、Mode、Timeout。`p-4` + `text-3xl` 数字 + 副标题，约 130px 垂直空间。

信息价值低：
- Providers / Routes 数就是下方表格的行数，扫一眼就知道
- Mode / Timeout 在「全局设置」表单里本来就能直接改（`index.html:442`、`:458`）

而垂直空间在这几页很紧：`main` 在 `lg:` 断点锁死 `lg:h-[calc(100vh-4rem)] lg:overflow-hidden`
（`index.html:281`），表格拿到的是**减去 header 64px、卡片 130px、模式切换条、表头之后的剩余高度**。

header 右侧已经有现成的 chip 模式可以复用（`index.html:332-333` 的 `lan` / `schedule` chip）。

### 2.9 前端：statusMessage 顶部横幅遮挡内容

`index.html:268-271`：`fixed` + `style="top: 5rem"` + `left-4 right-4`（`lg:left-80 lg:right-8`），
z-index 60。它会盖住主内容区顶部。

**这一点代码注释里已经自认了**。`index.html:218-223` 解释未保存浮层为什么做成右下角可拖拽时写：

> 也不放顶部，顶部浮层会盖住统计卡片（statusMessage 就是这个毛病）

即未保存提示已经因为这个问题改成右下角（`index.html:224-239`，还支持拖拽 + localStorage 记忆位置），
但 statusMessage 自己还留在顶部。同一个页面两种浮层策略，其中一种是已知有问题的那种。

叠加因素：错误提示停留 20 秒（`index.html:1859`，注释说明后端校验原文较长、5 秒读不完），
遮挡时间不短。

### 2.10 前端：日志表八个后端字段没有落点

`metrics.RequestLog`（`metrics.go:110-138`）有 20+ 字段，前端日志表只用了 8 列。**未使用的字段**：

| 字段 | 排查价值 |
|---|---|
| `QueueWaitMs` | **判断慢在队列还是慢在上游**——本轮最有价值的一个 |
| `UpstreamStatus` | 上游真实状态码 vs 网关返回码的差异 |
| `Route` | 命中了哪条路由规则（`match` 模式） |
| `TargetModel` | 实际发给上游的模型名（与客户端请求的 `model` 可能不同） |
| `ResponseBytes` | 响应体量级 |
| `Vision` | 是否触发了视觉翻译 |
| `Method` / `Path` | 请求端点 |

同时日志表 8 列全部 `nowrap` + `truncate`，**错误列截得最狠**：
`index.html:1059` 的 `w-full max-w-0 truncate`，完整错误只在 `title` 里。
注释（`index.html:1009-1012`）说明这是从 12 列压到 8 列的结果——为了把行高从 127px 降到一行。

行展开能同时解决两件事：截断的错误文本有地方完整显示，八个未用字段有落点。

### 2.11 前端：导航项写了两遍

- 桌面侧栏：`index.html:142-171`，5 个 `<button>`，`border-l-4` 高亮 + `icon-fill` 填充图标
- 移动抽屉：`index.html:250-264`，同 5 个 tab，`btn btn-secondary w-full justify-start`

两处样式不同（侧栏是左边框高亮，抽屉是通用次要按钮，且抽屉**没有当前 tab 的高亮态**），
加 tab 要改两处。

### 2.12 前端：tooltip 两套行为

`showTip`（`index.html:1998`）实现了即时 tooltip，注释（`:1996-1997`）明确：

> 原生 `title` 有 1-2 秒延迟且不可配置，这里 `mouseenter` 即显示。

`:1256-1257` 还说明了为什么用 `fixed` 而非 CSS `::after`：徽章外层是 `overflow-x-auto`，
绝对定位气泡会被滚动容器裁掉。

但 `showTip` 只用在监控表的健康列（`:815-820`）和熔断列（`:829-834`）。仍在用原生 `title` 的：

| 位置 | 内容 |
|---|---|
| `index.html:627` | provider 全名 |
| `index.html:637` | baseUrl 全文（列表里被截断） |
| `index.html:1033` | 完整时间戳 + 请求 ID |
| `index.html:1041` | IP · Key 来源 · Key 指纹（三行 `\n` 拼接） |
| `index.html:1058` | 转移轨迹全文 |
| `index.html:1060` | 错误全文 |
| `index.html:811` | 监控表 provider 全名 |
| `index.html:396-397` | 视觉伴随配置 |

后六项恰恰是排查时最常悬停的——同一张表里，最需要即时反馈的反而延迟最长。

### 2.13 前端：琥珀色在同一行有两种含义

同一行的两个 chip 都可能是琥珀底色，语义完全不同：

- `providerHealthClass('warn')`（`index.html:2288`）：上游可达但返回非 200
- `breakerClass` 的「闭合但有失败计数」（`index.html:2340`）：请求在正常发，只是攒了几次连续失败

`breakerClass` 的注释（`:2338-2339`）解释了为什么不给纯绿：

> 请求确实还在正常发（所以不是 danger），但「离打开熔断还差几次」和干净的闭合不是一回事

理由成立，但**徽章文本已经承载了这个信息**：`breakerLabel` 在这一态输出 `正常 3/3`
（`index.html:2323`）。文本已经说清了，底色再抢一次注意力，代价是与健康列的 `warn` 撞色。
琥珀应该留给真正需要注意的 `half_open` / `open`。

### 2.14 前端：1024–1280px 的双滚动区（待实机验证）

三处断点不一致：

| 元素 | 断点与行为 | 位置 |
|---|---|---|
| `main` | `lg:h-[calc(100vh-4rem)] lg:overflow-hidden` | `index.html:281` |
| 配置区主栏 | `lg:min-h-0 lg:overflow-y-auto` | `index.html:340` |
| 配置区侧栏 | `lg:min-h-0 lg:overflow-y-auto` | `index.html:673` |
| **分栏栅格** | `xl:grid-cols-12` | `index.html:339` |

即：**限高和内滚在 `lg`（1024px）生效，而分栏要到 `xl`（1280px）**。
1024–1279px 之间栅格仍是单列，`aside`（配置预览）作为第二行元素，
外层 `overflow-hidden` + 内层各自 `overflow-y-auto` 的组合下，可能整块落在视口外且没有滚动入口。

**这条是从 class 组合推出来的，我没有在浏览器里实测。** 实现前必须先在 1100px 左右量一眼。
如果 2.7 的折叠方案落地后 `aside` 不再常驻，这个问题可能自动消失——所以本条排在 2.7 之后做。

### 2.15 前端：无障碍缺口

| 缺口 | 位置 |
|---|---|
| 侧栏 tab 按钮无 `role="tab"` / `aria-selected` | `index.html:142-171` |
| 图标按钮只有 `title`，无 `aria-label` | `:408-422`、`:651-659`、`:754-774`、`:877-884` |
| 表头无 `scope="col"` | `:356-360`、`:608-613`、`:797-805`、`:1017-1024` |
| `tabindex="0"` 的 chip 无 `aria-describedby` 关联 tooltip | `:820`、`:834` |

`aria-live` 已经做对了（statusMessage `:268`、未保存浮层 `:226`），确认弹窗也有
`role="alertdialog" aria-modal="true"`（`:1270`）——所以这不是从零开始，是补齐剩下的。

**边界**：完整 WCAG 合规需要辅助技术实测和专家人工评审，本轮只补这几处明确可改的，不声称达标。

### 2.16 前端：Tailwind Play CDN 与外部字体

**Play CDN（407KB）**：`vendor/tailwindcss.js` 是浏览器内运行时 JIT 编译版。
代码里已有三处注释在绕它的坑（`index.html:114-121`）：

> 自己定义而不用 Tailwind 的 `animate-spin`：vendor 里是 Play CDN 的 JIT 编译器，
> 只出现在 Alpine `:class` 表达式里的类名不保证被扫描到，赌它等于赌图标不转。

`gw-spin`、`gw-grab`、`gw-grabbing` 三个类因此手写 CSS。这个坑对未来每个只在 `:class` 里
出现的类名都成立，是持续的隐患而不是一次性问题。

**外部字体**：`index.html:11-12` 从 `fonts.googleapis.com` / `fonts.gstatic.com` 加载
Inter、JetBrains Mono、Material Symbols Outlined。CSP 为此专门开了口子（`helpers.go:124`）：

```
style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:
```

Material Symbols 是 **ligature 字体**——图标靠把 `<span>monitoring</span>` 的文本内容
连字成图形来渲染。字体拉不到时，界面上所有图标会退化成英文单词：`monitoring`、`dns`、
`alt_route`、`receipt_long`、`tune`、`hub`、`save`、`delete`……全站图标按钮变成一片英文文本。

对一个定位「轻量本地 API 网关」、默认只发布到 `127.0.0.1`（`CLAUDE.md` 安全小节）的服务，
UI 完整性依赖公网 CDN 是设计矛盾。离线环境、内网隔离、Google 域名不可达时界面直接可用性受损。

### 2.17 前端：单文件 2938 行

`index.html` 2938 行 / 202KB，其中 `gatewayApp()`（`:1288-2935`）是单个约 1650 行、
60+ 方法的返回对象。`go:embed`（`main.go:44`）要求单文件部署，这是约束不是缺陷，
但可以在构建期拼装、开发期分文件。

---

## 3. 优化项总表（17 条 ↔ 任务 ID 对照）

**编号约定**：本文档正文用「第 N 条」指代 2.1–2.17 的分析条目；`TASKS.md` 用 `S<阶段>-<序号>`
指代可委托的任务。两者是同一批工作的两种视角，下表是唯一的映射来源——正文里出现的
「第 N 条」都可以在这里查到对应任务 ID。

分阶段依据是**风险与依赖**（详见第 4 节），不另设 P0/P1/P2 维度：早期版本曾并行维护一套
优先级分层，与阶段划分产生了矛盾（把前端布局项标成 P0，却又归入「不动布局」的阶段一），
已统一为下表。`TASKS.md` 里的 P0/P1/P2 只是阶段的同义标签（阶段一=P0，阶段二=P1，阶段三=P2）。

| 第 N 条 | 项 | 面 | 任务 ID | 依据位置 |
|---|---|---|---|---|
| 1 | 六处 `MarshalIndent` → `Marshal` | 后端 | `S1-1` | `main.go:1070/1118/1141/1152/1270/1299/1359` |
| 2 | `/health` 的 `GetStats` + `ReadMemStats` 加 TTL 缓存 | 后端 | `S1-2` | `main.go:1044-1050`、`cache.go:147` |
| 3 | 前端轮询加可见性 + tab 门控 | 前端 | `S1-4` | `index.html:1413`、`1616` |
| 4 | 折叠右侧配置预览栏，表格拿回全宽 | 前端 | `S2-1` | `index.html:339-340`、`673-687` |
| 5 | 配置页四张统计卡片 → header chip | 前端 | `S2-2` | `index.html:283-316`、`332-333` |
| 6 | statusMessage 横幅 → 右下角 toast 栈 | 前端 | `S2-3` | `index.html:268-279`、`218-223` |
| 7 | 熔断列「闭合但有失败计数」底色改中性 | 前端 | `S2-7` | `index.html:2340` |
| 8 | `Collector.Logs` / `Metrics` 去掉全量拷贝 | 后端 | `S1-3` | `metrics.go:318-339`、`342-419` |
| 9 | 日志表行展开，接入 8 个未用字段 | 前端 | `S2-4` | `metrics.go:110-138`、`index.html:1059` |
| 10 | 只读观测端点补 Host 白名单校验 | 后端 | `S1-5` | `main.go:243/252/305/315` |
| 11 | 导航项数据驱动，侧栏与抽屉共用 | 前端 | `S3-4` | `index.html:142-171`、`250-264` |
| 12 | 原生 `title` 统一走 `showTip` | 前端 | `S2-6` | `index.html:627/637/811/1033/1041/1058/1060` |
| 13 | P95 低样本量时不给数值 + 标注桶近似 | 前后端 | `S1-6` | `metrics.go:29/58`、`index.html:725-732` |
| 14 | 1024–1280px 双滚动区（**先实测**） | 前端 | `S2-8` | `index.html:281/339/340/673` |
| 15 | 无障碍补齐（tab 语义、aria-label、scope） | 前端 | `S3-5` | 见 2.15 表 |
| 16 | Tailwind 构建期编译 | 前端 | `S3-2` | `index.html:11-12/114-121` |
| 16' | 字体与图标本地化 | 前端 | `S3-3` | `index.html:11-12`、`helpers.go:124` |
| 17 | `index.html` 拆分 + 构建期拼装 | 前端 | `S3-1` | `index.html:1288-2935` |

**一处拆分**：第 16 条在分析阶段是「Tailwind 构建期编译 + 字体本地化」一条，落到任务时拆成
`S3-2`（Tailwind）与 `S3-3`（字体图标）两项——两者无依赖关系、可分别验收，合成一个任务会让
「视觉零差异」和「断网可用」两个不同的验收标准挤在一起。表中以 16 / 16' 区分。

---

## 4. 分阶段实施

按**风险与依赖**分三阶段。每阶段结束都是一个可独立验收、可独立提交的完整状态。

### 阶段一（`S1-1` ~ `S1-6`）：后端减负 + 轮询门控 + 只读端点加固

对应分析条目第 1、2、3、8、10、13 条，任务 ID 依次为 `S1-1` `S1-2` `S1-4` `S1-3` `S1-5` `S1-6`
（映射见第 3 节；`TASKS.md` 的阶段一清单与此处一一对应，共 6 项）。

**为什么先做**：纯后端 + 两处前端逻辑，不动任何布局，回归面最小。
`S1-1` / `S1-2` / `S1-3` 是「同样的输出、更少的开销」，`S1-4` 是「更少的调用」，
`S1-5` 是纯准入校验、不改任何响应体，`S1-6` 是一处卡片文案加一个已有字段的复用。

**改动范围**：`cmd/gateway/main.go`、`internal/cache/cache.go`（或 `main.go` 侧加缓存层）、
`internal/metrics/metrics.go`、`cmd/gateway/helpers.go`、
`index.html` 的 `init()` / `refreshMonitor()` / P95 卡片。

**关键约束**：

- `S1-1`（第 1 条）：`Marshal` 替换后 `content-length` 计算不变（都是先 marshal 到 `[]byte` 再写头），
  六处的写法一致，不要遗漏 `handleProviderHealthCheck` 的**两个**分支（`main.go:1141` 单 provider、
  `main.go:1152` 全量）。
- `S1-2`（第 2 条）：TTL 缓存**不能引入新的锁竞争**。建议在 `server` 上加字段而不是改 `cache` 包——
  `cache.GetStats()` 自身的锁语义（串行化 DB 操作）是刻意设计，不要动。
  TTL 取 10 秒（大于前端 5 秒轮询周期，保证连续两次轮询至多一次真实查询）。
  `ReadMemStats` 同样缓存，可以共用同一个 TTL 结构。
  **注意**：`/health` 是给人看的运行状态，10 秒陈旧完全可接受；但缓存必须在配置热重载后失效吗？
  不必——`GetStats` 反映的是缓存 DB 内容，与配置无关。
- `S1-3`（第 8 条）：改成在 RLock 内从新到旧遍历、够数即停。**`Logs` 的 offset 语义要保住**
  （`metrics.go:326-331`：offset 大于结果数时返回空切片）。`Metrics` 的兜底分支
  （`len(resp.Providers) == 0`）和 `recentErrors` 仍需要记录，但可以只取需要的量而不是全份。
- `S1-4`（第 3 条）：`setInterval` 要留句柄以便停止。门控逻辑：
  `document.visibilityState === 'visible' && (activeTab === 'monitor' || activeTab === 'logs')`。
  从隐藏切回可见时应立即拉一次（不等下个周期），否则用户切回来看到的是陈旧数据。
  切 tab 到 monitor / logs 时同理。
- `S1-5`（第 10 条）：**做 Host 白名单而不是逐个加 `isCrossSiteRequest`**。
  理由：Host 校验能一次覆盖所有端点（包括未来新增的），而 `isCrossSiteRequest` 靠 `Origin`，
  DNS rebinding 场景下 `Origin` 本身就是攻击者域名、但请求确实到达了本机——
  真正的防线是「`Host` 头必须是 loopback 或配置的 `host`」。
  实现要点：`r.Host` 可能带端口，要 `net.SplitHostPort` 后判断；
  已有 `isLoopbackHostname`（`helpers.go:112`）可复用。判据四条与错误体见 `API_SPEC.md` 第 4 节。
  **兼容性风险**：无 `Origin` 的本机 CLI 请求（Claude Code、Codex CLI）必须继续可用——
  它们的 `Host` 是 `127.0.0.1:7789`，符合白名单。但如果用户通过反向代理或自定义 hostname 访问，
  Host 会是别的值。所以白名单要包含配置里的 `cfg.Host`，并且**这条改动必须有测试覆盖
  「Host 为配置值时放行」**。若判断有兼容风险，退回到「只给三个观测端点加 `isCrossSiteRequest`」
  的保守方案，并在 KNOWN_ISSUES.md 记录取舍。
- `S1-6`（第 13 条）：先看 `Summary` 已有的 `WindowRequests`（窗口内请求数）够不够用——
  **优先复用，不新增字段**；只有在它与 P95 的样本口径不一致时才加 `LatencySamples`
  （字段形状见 `API_SPEC.md` 第 3.1 节）。
  前端在样本量 `< 20` 时 P95 卡片显示样本量而非数值，卡片副标题加「按桶上界近似」。
  阈值 20 是判断、不是实测得出的——写进注释说明理由（rank 算法在小样本下让 P95 退化为最大值）。

**验收**：`go test ./...` 与 `go test -race ./...` 全绿；六个端点响应体 JSON **语义**不变
（字段与值一致，仅缩进消失）；`Host` 为 loopback 与配置值时放行、为外部域名时 403
（`forbidden_host`）且有测试覆盖；前端在后台标签页与配置页不再发出这三个请求
（浏览器 Network 面板可验）；低样本量下 P95 卡片不出数值。

### 阶段二（`S2-1` ~ `S2-8`）：前端布局重构

对应分析条目第 4、5、6、9、12、7、14 条，任务 ID 依次为 `S2-1` `S2-2` `S2-3` `S2-4` `S2-6` `S2-7` `S2-8`，
外加 `S2-5`（列宽 hack 退役——从第 4 条里拆出来独立验收，理由见下）。共 8 项，
与 `TASKS.md` 阶段二清单一一对应。第 11 条（导航数据驱动）虽然也是前端，但不动布局、
不依赖 `S2-1`，归入阶段三 `S3-4`。

**为什么排第二**：都动布局与样式，回归面集中在视觉层；且 `S2-1`（折叠预览栏）是其余几条的地基——
表格拿回全宽后，`S2-4` 的行展开才有横向空间，`S2-8` 的断点问题也可能自动消失。

**改动范围**：只有 `cmd/gateway/web/index.html`。

**关键约束**：

- `S2-1`（第 4 条）是本阶段核心。折叠状态记 localStorage（与 `dirtyToastPos`、`gw.models`
  同一套做法）。折叠后随之失效的列宽 hack 交给 `S2-5` 单独清理，本条不顺手改——
  「腾出空间」和「回收补丁」的验收标准不同（一个看交互，一个看回归），合成一项会互相掩盖。
- `S2-2`（第 5 条）：卡片信息并入 header chip 时，Mode / Timeout 两项在「全局设置」页与表单字段重复，
  可以只在 providers / routes 页显示 chip。
- `S2-3`（第 6 条）：statusMessage 与未保存浮层同时出现时不能互相遮挡。未保存浮层可拖拽且位置持久化
  （`:2048-2093`），toast 栈的定位要考虑这一点——建议 toast 固定在右下、未保存浮层默认位置上移，
  或 toast 出现时未保存浮层临时让位。**这是本阶段唯一需要设计判断的交互细节**，
  实现时若发现两者难以共存，记入 KNOWN_ISSUES.md 由 Claude 决策，不要自行改变未保存浮层的既有行为。
- `S2-4`（第 9 条）：行展开用 Alpine 的局部状态（每行一个 `expanded` 标志或一个 `expandedLogId`）。
  展开面板里字段为空的不显示（多数字段有 `omitempty`，前端拿到的是 `undefined`）。
  `queueWaitMs` 与 `durationMs` 建议并排展示，这是「慢在哪」的直接对比。
- `S2-5`（第 4 条拆出）：清理那批为「挤进 75% 宽度」写的补丁。需要复查的位置：
  `:601-604`（限流列合并的理由）、`:619-625`（名称/URL 各 50%）、`:378-382`（候选列两层 min-w-0）、
  `:404-405`（操作列 w-px）、`:636`（去 https 前缀）、`:784`（min-w 660 的计算）。
  **注意**：这些 hack 未必全部要撤——`truncate` + tooltip 对超长 provider 名仍是对的。
  撤掉哪条就把对应注释一并改掉，留下与现实不符的注释比不改更糟。
- `S2-6`（第 12 条）：`showTip` 目前签名是 `showTip(text, event)`，多行文本靠 `\n`（如 `:1041` 的三行拼接）。
  气泡是单个 `<span x-text>`（`:1261`），`\n` 不会渲染成换行——需要给气泡加
  `whitespace-pre-line`，否则多行 tooltip 会挤成一行。这是迁移时必须一起改的。
- `S2-7`（第 7 条）：只改 `breakerClass` 里 `consecutiveFailures > 0 && state === 'closed'` 这一态的底色，
  改成中性（如 `bg-white/[.05] text-muted` 或保留边框但去掉琥珀填充）。
  **不要动** `breakerLabel` 的文本（`正常 3/3` 要留着）、不要动 `half_open` / `open` 的琥珀与红。
- `S2-8`（第 14 条）：**先在浏览器里量 1100px 宽度的实际表现**，确认问题存在再改。
  改法是把内滚门控与分栏门控统一到 `xl:`（或把分栏降到 `lg:`，取决于 1024px 下双栏是否还可读）。
  若 `S2-1` 落地后问题已自然消失，本条转为「验证并记录无需改动」，结论写进 KNOWN_ISSUES.md。

**验收**：真实浏览器验收（桌面 1280px / 1440px / 1920px + 移动 390x844）；
表格在 1440px 下无横向滚动、操作列完整可见；预览栏折叠状态刷新后保持；
行展开内容完整、空字段不显示；两种浮层不互相遮挡；tooltip 多行正常换行；
1024–1280px 区间所有内容可达。

### 阶段三（`S3-1` ~ `S3-5`）：工程化与可维护性

对应分析条目第 17、16、16'、11、15 条，任务 ID 依次为 `S3-1` `S3-2` `S3-3` `S3-4` `S3-5`。
共 5 项，与 `TASKS.md` 阶段三清单一一对应。第 10、13 条（Host 白名单、P95 样本量）
已提前到阶段一（`S1-5` / `S1-6`），第 14 条已归入阶段二 `S2-8`——它们不在本阶段。

**为什么排最后**：`S3-1` / `S3-2` 是构建流程改动，需要单独验证 `go:embed` 与 Docker 构建；
`S3-4` / `S3-5` 独立但优先级低。本阶段不阻塞前两阶段，可单独排期。

**改动范围**：`index.html`、`cmd/gateway/helpers.go`（CSP）、
可能新增构建脚本与 `Makefile` 目标、`Dockerfile`、`web/vendor/fonts/`。

**关键约束**：

- `S3-1`（第 17 条）：拆分后必须保证 `go:embed web/index.html`（`main.go:44`）仍拿到完整单文件，
  且开发模式 `AI_GATEWAY_WEB_DIR` 的热加载（`main.go:1375-1380`）仍工作。
  依赖 `S3-2` 的构建流程——若 `S3-2` 走方案 B（不引入构建），本条要重新考虑做法。
  **建议在阶段二全部验收后再动**，否则会与布局改动大面积冲突。
- `S3-2`（第 16 条）：Tailwind 改构建期编译需要 Node 工具链，而当前仓库无 `package.json`
  （Node 版已固化为 tag `v1.0.0-node`）。**这是本条最大的成本**：为前端 CSS 引入 Node 构建依赖，
  与「Go 单二进制」的定位有张力。两个方案，实现前需要 Claude 决策：
  - **方案 A**：引入最小 Node 构建步骤（`package.json` + `tailwindcss` CLI），产物 CSS 提交进仓库，
    `go:embed` 照旧。开发者改 class 后需跑一次构建。
  - **方案 B**：不引入构建，手写一份精简 CSS 替代 Tailwind（当前用到的 utility 类数量有限）。
    彻底去掉 407KB，但改样式的心智成本上升。
  完成后 CSP 的 `script-src` 去掉 `'unsafe-eval'`（细节见 `API_SPEC.md` 第 5.1 节）。
- `S3-3`（第 16' 条）：woff2 子集放 `web/vendor/fonts/`、CSS 改 `@font-face` 指本地、
  CSP 去掉 `fonts.googleapis.com` / `fonts.gstatic.com`。
  **`go:embed` 的 `web/vendor/*` 是单层通配、不递归子目录**，新增 `vendor/fonts/` 必须同步改 embed 指令，
  否则字体不进二进制（详见 `API_SPEC.md` 第 5.2 节）。
  Material Symbols 若换内联 SVG 则改动面大（全站几十处图标），本条只做字体本地化，
  SVG 化另开需求（两种落地方式与推荐见 `API_SPEC.md` 第 5.3 节）。
- `S3-4`（第 11 条）：抽成 `navItems` 数组（`{ id, label, icon }`），侧栏与抽屉各自 `x-for` 渲染但
  **保留各自的样式**（侧栏 `border-l-4` 高亮、抽屉按钮式）——统一的是数据源不是外观。
  顺带给抽屉补上当前 tab 高亮（现在没有）。注意侧栏点击时 providers / routes 会附带
  `editMode = 'visual'`（`:148`、`:154`），抽屉也有（`:253`、`:256`），这个副作用要保留。
- `S3-5`（第 15 条）：只补 2.15 表里那四类（tab 语义、aria-label、`scope`、焦点可见）。
  不声称 WCAG 达标。依赖 `S2-6`——tooltip 统一后才好一起补 `aria-describedby`。

**验收**：`go test ./...` / `-race` 全绿；离线环境（断网或 hosts 屏蔽 Google 域名）下界面图标正常；
构建产物里能取到字体（`S3-3`）；Docker 镜像构建通过；开发模式热加载仍工作；
视觉与改动前零差异（`S3-1` / `S3-2` 的核心验收标准）。

---

## 5. 取舍说明

### 5.1 为什么不做后台定时指标采集

考虑过让后端自己定时算好 metrics 快照、前端只读缓存。否决理由：
本地网关 QPS 低，`Metrics()` 的开销主要来自全量拷贝（第 8 条已解决），
桶遍历本身是 901 个固定桶的线性扫，不值得引入后台 goroutine 与生命周期管理。
`CLAUDE.md` 也记录了当前进程里唯一的 `time.NewTicker` 是前端热加载 SSE ——
保持这个简洁性有价值。

### 5.2 为什么不换前端框架

`index.html` 单文件 + `go:embed` + Alpine 的组合让部署是真正的单二进制，
没有 npm install、没有构建产物同步问题。第 17 条的拆分是在保住这个性质的前提下改善可维护性，
而不是推翻它。换 Vue / React 会引入完整前端工程链，与「轻量本地网关」的定位不符。

### 5.3 为什么第 10 条推荐 Host 白名单而非逐个加 Origin 检查

`isCrossSiteRequest` 检查的是 `Origin`/`Sec-Fetch-Site`，防的是普通跨站请求。
但普通跨站请求本来就读不到响应（无 CORS 头），所以对**只读**端点，加 Origin 检查的边际收益很小。
真正有威胁的 DNS rebinding 场景下，浏览器认为同源、`Origin` 是攻击者域名，
而攻击者页面能正常读到响应——此时唯一可靠的判据是 `Host` 头。
Host 白名单同时覆盖已有和未来新增的所有端点，是更彻底的一层。

### 5.4 关于「优化」的克制

17 条里没有一条改变功能行为。这是刻意的：核心链路刚过 hardening 轮次、测试覆盖 13 个包，
本轮的价值在于减负与体验，不在于加能力。任何实现中发现的「顺手改进」若涉及行为语义，
记入 KNOWN_ISSUES.md 而不要自行落地。

---

## 6. 风险与回归面

| 风险 | 涉及项 | 缓解 |
|---|---|---|
| `Marshal` 替换漏改某处 → 前端解析不受影响但一致性差 | 1 | 六处逐一核对，含 `handleProviderHealthCheck` 两个分支 |
| TTL 缓存让 `/health` 数据陈旧被误判为卡死 | 2 | TTL 10 秒，远小于人的观察周期；`/health` 语义本就是概览 |
| 轮询门控后用户切回页面看到陈旧数据 | 3 | 可见性恢复与 tab 切换时立即拉一次 |
| `Collector` 遍历改写破坏 offset / limit 语义 | 8 | 现有 `metrics_test.go` 覆盖，改后必须全绿 |
| 折叠预览栏后清理列宽 hack 过头 → 窄屏溢出回归 | 4 | 保留 `truncate` + tooltip；1440 / 1280 / 390 三档实测 |
| toast 栈与可拖拽未保存浮层争位 | 6 | 见阶段二约束；难共存则记 KNOWN_ISSUES.md |
| Host 白名单误拒本机 CLI 或自定义 hostname | 10 | 白名单含 `cfg.Host`；三种情况都要测试覆盖 |
| Tailwind 构建引入 Node 依赖，与单二进制定位冲突 | 16 | 两方案待 Claude 决策，不由实现方自行选定 |
| 拆分 `index.html` 破坏 `go:embed` 或开发热加载 | 17 | 两条路径都要验证；依赖第 16 条方案确定 |

---

## 7. 验证命令

按 `CLAUDE.md` 约定，Go 验证在容器内跑，验证容器名固定 `ai-gateway-dev-verify`：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -l ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
```

`-race` 需要完整镜像（race detector 依赖 cgo，alpine 变体不含 C 工具链，
这一点 `docs/plans/2026-08-23-failover-breaker-lb.md:278` 已记录）：

```bash
docker run --rm -v "$PWD":/work -w /work -v ai-gateway-go-mod-cache:/go/pkg/mod golang:1.23 go test -race ./...
```

前端验收用开发模式热加载：

```bash
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d
```

---

## 8. 明确不做

- 改变任何转发语义、判据或错误终态
- 后台定时全量指标采集（见 5.1）
- 更换前端框架（见 5.2）
- 给网关加鉴权（`CLAUDE.md` 已明确：远程访问由前置代理负责，本轮不改变这个边界）
- 声称 WCAG 合规（只补明确缺口）
- 给出任何「优化后提升 X%」的量化承诺——本轮无性能实测数据支撑
