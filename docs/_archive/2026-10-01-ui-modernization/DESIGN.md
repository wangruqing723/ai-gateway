# 前端 UI 现代化重构设计

日期：2026-10-01（需求创建日期保留）
状态：已完成；2026-11-01 文档按代码同步并获准归档。
分支：dev（原实施分支）
参考：用户提供的目标视觉稿（深蓝毛玻璃、大圆角卡片、细线图标、通栏侧边栏、无全局顶栏）

## 最终完成与验收记录（2026-11-01）

- 主实现为 2026-10-02 的 `09eb4b4`，本次以 `46a72c6` 对照源码。批次 1 的历史验收保留；**2026-11-01 使用者确认此前验收完成：批次 2–5 浏览器视觉验收已完成**。本次没有重新执行浏览器视觉验收。
- 色板、阴影与细线图标轴均已落地；圆角当前为 `lg/xl/2xl = 12/16/20px`。下文 8px 仅代表重构前历史基线，不是当前限制。
- 当前监控页主区 Provider 3/4，右栏 1/4 放运行时状态、状态码分布与配置健康，三块自然高度。观测区首行延迟排行 1/3 + 运行异常 2/3（`xl:max-h-[26rem]`），次行最近错误通栏（`xl:max-h-[22rem]`），Grid 默认 stretch 对齐且按内容收缩；不再固定 `xl:h-[26rem]`。
- 后续 `a1ea575`（2026-10-03）增加流式首字耗时，`35fe068`（2026-10-05）调整「首字/耗时」表头，`f3561ab`（2026-10-17）精修布局与列表/抽屉交互。这些是需求后演进，不能按本轮「不动逻辑」红线倒推为违规或漏做。
- 本会话主会话 Claude 在 `golang:1.27-alpine` 容器确认 `webbuild -check`、`go vet ./...` 退出 0，`go test -count=1 ./...` 全部通过；CSS 重建逐字节比对沿用 2026-10-02 历史记录，本次未重跑 Tailwind CLI。格式唯一既有例外 `internal/breaker/breaker.go` 与此需求无关。
- 1024–1280px 已不属于支持目标，原 S2-8 于 2026-10-01 关闭。本目录无剩余视觉验收门槛，已获准归档。

> **本文定位**：这是一次**只动 UI、不动功能逻辑**的重构（见决策 Q6=A）。
> 参考稿提供的是「材质与观感语言」——毛玻璃、渐变、大圆角、徽章样式、细线图标、
> 侧边栏风格——套用到现有布局上，而非逐页照搬参考稿的结构。
> 结构性改动以去顶栏、弹窗改右侧抽屉、监控页重排为主；日志主筛选也在落地时由 6 格收为 5 列。其余页保留字段和交互，材质更新；后续需求演进与本轮纯 UI 红线分开记录。
> 代码是事实来源；本文已按现行代码与历史实现记录同步，不以旧方案覆盖后续已验收调整。

---

## 1. 需求目标

本轮将需求提出时「高饱和青 + 近黑毛玻璃」的管理界面改为
「偏蓝深色 + 柔和青 + 大圆角浮层」。技术栈保持 Alpine + Tailwind CLI，原五个页签不变，
本轮不新增业务交互；之后接入账户/多密钥等需求不受本轮历史范围限制。

## 2. 不在本轮范围

- **不换框架**（Q2=A）：不引入 Vue/React/Svelte 与打包器，`cmd/webbuild` 产物管线、SSE 热加载、
  CI 三道关卡、钉住 HTML 字符串的 Go 测试全部保留。
- **不动功能逻辑**（Q6=A）：不新增、不删除、不改任何交互行为。像素可以全变，行为必须逐条可比对。
  - 唯一的「交互删除」是导航壳层面的死代码：移动端抽屉与汉堡按钮（Q5 确认只做桌面端后它们无服务对象）。
- **不照搬参考稿里本项目没有数据源的东西**（Q6 的直接后果）：
  - 顶部「在途请求」实时面板——指标系统只有已完成请求的环形缓冲，无在途追踪，不新增。
  - token 计数（输入/输出/缓存读取/推理）——`RequestLog` 无这些字段，只有启用上下文窗口时的
    估算输入 token，不补埋点。
  - 首字延迟——本轮不新增；后续 `a1ea575` 已实现流式首字耗时，`35fe068` 调整「首字/耗时」表头与列宽，不再将「未记录」当作当前事实。
  - 版本号——仓库无版本变量（构建仅 `-ldflags="-s -w"`），不显示假版本。
- **不改信息架构**（Q3）：五个页签与「哪些内容归哪一页」不变，只重排页内布局。
- **不做亮色主题**（Q4=B 方向，暂不做亮色）：`darkMode:'class'` 配置保留，仍只有深色一套。
- **不改后端**：本轮不碰任何 `.go` 文件的运行逻辑；只改前端源文件与随仓库提交的两个产物。

## 3. 决策记录（Q1–Q18 最终答复）

| 编号 | 决策 | 落地口径 |
|---|---|---|
| Q1 | 协作模式仅落地文档，Claude 自己实现 | 出 DESIGN+TASKS，不委托 Codex |
| Q2 | A 留在 Alpine + Tailwind CLI | 工具链一行不动 |
| Q3 | 保持五页签，只重排页内布局 | 信息架构不变 |
| Q4 | 参考稿材质语言 | 偏蓝深色、柔和青、大圆角浮层 |
| Q5 | 只桌面端 | 移动端/平板不作为目标，抽屉导航删除 |
| Q6 | A 只动 UI 不动功能 | 行为逐条不变 |
| Q7 | 分批交付，每批浏览器验收 | 5 批，顺序见 §9 |
| Q8 | A 纯 CSS 渐变 | 不引壁纸图片资源，渐变调偏蓝 |
| Q9 | A 只改令牌值不改名 | 模板类名不动，圆角放开 |
| Q10 | 日志页只换材质 | 9 列表格结构、展开详情全保留 |
| Q11 | A 保留 Material Symbols | 调字重轴变细，不动字体文件 |
| Q12 | B 弹窗改右侧抽屉 | Provider/路由弹窗从居中模态改右缘抽屉 |
| Q13 | A 去顶栏 | 删标题副标题、健康移侧栏底、操作移操作条、删移动端抽屉 |
| Q14 | 按建议批次顺序 | 见 §9 |
| Q15 | B 监控页完整重排 | 面板重新编排，替换绝对定位等高 hack |
| Q16 | A 按色板方向调 | 第一批可视产物上微调具体数值 |
| Q17 | A 配置三页顶部轻量操作条 | 保存/重载/预览的统一落点；监控/日志按钮进卡片标题行 |
| Q18 | A 侧边栏底部状态卡 | 在线/监听/模式 + 保留配置文件路径 + 页脚「本地安全模式」无版本 |

## 4. 硬约束（适用于本轮 2026-10-01 纯 UI 实施）

下述行为/字段不改约束保留原交付边界，不将后续独立需求的功能与布局演进倒推为本轮违规。当前产物与字段护栏继续有效。

### 4.1 两个随仓库提交的产物必须重建
- `cmd/gateway/web/index.html` 由 `src/index.template.html` + `src/app/*.js.part` 经 `cmd/webbuild` 拼成。
  改模板或片段后必须 `go run ./cmd/webbuild`（或 `make web-html`）重建；CI `webbuild -check` 比对失败即红。
- `cmd/gateway/web/vendor/tailwind.css` 由 `make web-css`（`npx tailwindcss@3.4.17`）从 `src/tailwind.css`
  + `tailwind.config.js` 生成。**改任何 class 文本都要单独重建**——`webbuild -check` 不兜底 CSS，
  CI「Verify built stylesheet is up to date」用固定版本重建并 `diff`，忘重建即红（见 [[frontend-css-rebuild-separate-from-webbuild]]）。

### 4.2 Go 测试钉死的产物字符串（改 HTML 时必须保留）
- `TestEmbeddedAdminPageUsesLocalAssetsAndSafeConfigState`：产物必须含 `href="/vendor/tailwind.css"`、
  `src="/vendor/alpine.min.js"`、图标 SVG data URI、`x-show="isConfigTab()"`、`!canSave()`、`isDirty()`、
  `apiKeyConfigured`、`If-Match`、`application/yaml`、`configPayload()`；且不得含任何 CDN/Google Fonts/Play CDN 残留。
- `TestEmbeddedAdminPageUsesActualListenerAndKeepsRestartWarnings`：必须含 `restartRequired: []`、
  `this.restartRequired = data.restartRequired || []`、`health?.listenAddress`、`restartSuffix(result)`（≥2 次）。
  → **含义**：去顶栏后，`health?.listenAddress` 必须在侧边栏状态卡里仍出现；保存/重载按钮的
  `!canSave()` / `isDirty()` / `restartSuffix` 逻辑必须随按钮一起搬到操作条，不能丢。
- `TestBuiltStylesheetKeepsLocalFontsAndComponentLayers`：产物 CSS 必须含五个字体 woff2 url、
  `font-family:Material Symbols Outlined`、`font-feature-settings:"liga"`、`font-display:block`，
  以及组件类 `.btn{`/`.btn-primary{`/`.btn-secondary{`/`.btn-danger{`/`.field{`/`.chip{`/`.glass{`/
  `.material-symbols-outlined{`/`.icon-fill{`、层外类 `.gw-spin{`/`.gw-grab{`/`.gw-grabbing{`、`@keyframes gw-spin`；
  且 components 必须排在 utilities 之前（按字节偏移校验若干对）。
  → **含义**：组件类只能改取值，不能改名、不能删；`@layer components` 结构保留以保住层叠顺序。
- `TestStaticAssetsAndSecurityHeaders`：`/`、`/vendor/*` 的 Content-Type 与安全头；CSP 不得回退放行 Google 域名。

### 4.3 前端字段覆盖护栏（改抽屉表单时必须保留）
- `TestFrontendCoversAllProviderFields`：每个 `config.Provider` 字段必须出现在 `.js.part` 的**每个** provider
  重建点（按 `maxQueueWait` 定位对象字面量）。
- `TestFrontendCoversAllRouteFields`：每个 `config.Route` 字段必须在 `02-config-normalize.js.part` 的 routes
  回调里完整回写。
- `TestFrontendSentinelLiteralsMatchBackend`：`APIKeyKeepSentinel` / `ProxyPasswordKeepSentinel` 字面量必须保留。
  → **含义**：这三条防的是 `.js.part` 里的**数据重建逻辑**。本轮弹窗改抽屉只动模板 HTML 结构与样式，
    不动这些 JS 重建逻辑，护栏自然保持；但凡在抽屉里挪动字段输入框，务必确认对应的 `x-model` 绑定与
    save/normalize 逻辑一字不改。

### 4.4 其他边界
- **CSP**（`cmd/gateway/helpers.go:161`）：`default-src 'self'`、`font-src 'self' data:`、`img-src 'self' data:`、
  `connect-src 'self'`。任何外部域名资源都会被拒。壁纸因此只能走纯渐变（Q8=A）或内联 data（本轮不用）。
- **图标字体子集**：Material Symbols 按 37 个图标名子集化。本轮保留图标集（Q11=A）、只调字重轴，
  **不新增任何图标名**；若某批确需新图标，必须重新子集化字体（需外网），需单独提出。
- **圆角令牌已放开**：重构前 `lg/xl/2xl` 曾统一为 8px；本轮落地并保留至今的是 12/16/20px（与 §5.2 一致）。令牌名称保留，8px 只是历史基线，不再写成当前全局钉死值。

## 5. 设计令牌方案（第一批）

只改值不改名（Q9=A）。下表保留重构前基线，原拟值已经落地为现行值，已与 `tailwind.config.js` 对照，不再是待校准方案。

### 5.1 色板（`tailwind.config.js` theme.extend.colors）
| 令牌 | 重构前 | 现行值（已落地） | 说明 |
|---|---|---|---|
| ink | `#090a0f` | `#0b0e14` | 底色转偏蓝黑 |
| panel | `#12131c` | `#141824` | 面板一档，偏蓝提亮 |
| panel2 | `#191b25` | `#1b2130` | 面板二档 |
| panel3 | `#22242f` | `#252c3d` | 面板三档（下拉/tooltip 背景） |
| line | `rgba(255,255,255,.09)` | `rgba(148,163,184,.14)` | 边框转冷调、略加强 |
| muted | `#9da6b5` | `#94a3b8` | 次要文字转 slate |
| text | `#e7eaf0` | `#e8edf5` | 主文字 |
| cyan | `#47d6ff` | `#38bdf8` | 主色降饱和（天蓝） |
| cyan2 | `#00d2ff` | `#22d3ee` | 强调青降饱和 |
| violet | `#8f92ff` | `#a5a8ff` | 略提亮 |
| good | `#34d399` | 保留 | 成功绿 |
| warn | `#fbbf24` | 保留 | 警告琥珀 |
| danger | `#fb7185` | 保留 | 危险玫红 |

### 5.2 圆角（borderRadius）
现行 `lg: 12px`、`xl: 16px`、`2xl: 20px`；重构前均为 8px。卡片通过既有 rounded 类使用更大圆角，名称没有改变。

### 5.3 阴影（boxShadow.glow）
重构前为 `0 0 0 1px rgba(71,214,255,.12), 0 16px 48px rgba(0,0,0,.28)`；
现行 `0 0 0 1px rgba(148,163,184,.08), 0 18px 50px rgba(0,0,0,.35)`（弱化青色描边环，加深浮层投影）。

### 5.4 字重轴（`src/tailwind.css` 组件层）
`.material-symbols-outlined` 现行 `wght 300`（重构前 500），`.icon-fill` 现行 `wght 400`（重构前 600）。字号轴、FILL 与既有本地图标字体保留。

### 5.5 背景渐变（`src/tailwind.css` @layer base body）
重构前为右上角青色径向光 + 深色线性；现行在 `(78%,-12%)` 放柔青径向光、`(8%,108%)` 放极淡 indigo，线性底色为 `#0d1118 → #0b0e14`。
纯 CSS，不引图片（Q8=A）。

## 6. 导航壳改造（第一批，Q13=A + Q18=A）

- **删除 64px 全局顶栏** `<header>`：页面标题/副标题与侧边栏导航项语义重复，直接删（`pageTitle`/`pageSubtitle`
  成员保留在 JS 不碍事，仅不再渲染）。
- **删除移动端抽屉与汉堡按钮**（`mobileNavOpen` 相关 DOM）：只做桌面端，无服务对象。
- **侧边栏通栏到底**，不再 `hidden lg:flex`——桌面端恒显；主区从第一张卡片直接开始。
- **侧边栏底部状态卡**（替代原顶栏健康胶囊）：
  - 一张状态卡：在线圆点 + 监听地址（`health?.listenAddress`，护栏要求保留此表达式）+ 队列/直通模式。
  - 保留「配置文件」路径卡（Q18=A，运维高频信息）。
  - 页脚一行：「本地安全模式」（对应仅环回发布），**不显示版本**。
- **配置三页顶部轻量操作条**（Q17=A）：一行右对齐按钮簇，承载 配置预览 / 重载 / 保存 + 「需重启」提示。
  不是 64px 的壳，只贴着内容区顶部。`x-show="isConfigTab()"`、`!canSave()`、`isDirty()`、`restartSuffix` 全部随按钮搬来。
  - 监控页、日志页的操作按钮（检测/重置熔断/刷新；自动滚动/重载/清空/日志目录）进各自卡片标题行。
- **未保存浮层**（可拖拽 dirty toast）：功能不变（Q6），仅随令牌更新材质。

## 7. 各页改造方案

### 7.1 监控仪表盘（第二批，现行布局与历史调整）

- 4 张 KPI 在桌面 `xl:grid-cols-4` 横排，Provider 运行状态为主视觉。
- 主区 `xl:grid-cols-4`，Provider 占 3 列，右侧 aside 占 1 列。右栏的运行时状态、状态码分布、配置健康按内容自然高度；状态码卡不再通过 `flex-1` 强行吸收余高。
- 观测区 `xl:grid-cols-3`：首行延迟排行 1 列 + 运行异常 2 列，各以 `xl:max-h-[26rem]` 封顶；次行最近错误 3 列通栏，以 `xl:max-h-[22rem]` 封顶。
- 默认 Grid stretch 保持同行对齐，行高取内容较高的一块，数据少时整行收缩。各列表 `min-h-0 flex-1 overflow-y-auto` 滚动，配置健康不设滚动容器；不使用固定 `xl:h-[26rem]` 或旧的 `xl:absolute xl:inset-0` 脱流等高。

**历史方案保留**：重构前绝对定位撑高；2026-10-02 初次落地经历按内容体量配对、运行异常与最近错误换位、四面板固定 26rem 的两轮返工。2026-10-17 `f3561ab` 改为上述自然高度布局，把配置健康移入右栏、最近错误通栏。旧方案不再是当前验收规范，详见 TASKS.md 历史记录。

### 7.2 提供商（第四批，只换材质）
- 10 列表格结构不变（启用/名称/格式/Base URL/Key/代理/并发/速率/队列等待/操作）。
- 行内交互不变（启用开关、编辑/复制/删除、tooltip、禁用行压暗）。
- 只重做：卡片材质、表头样式、行分隔、徽章、按钮、hover 态。
- 「添加 Provider」按钮进卡片标题行。

### 7.3 路由（第四批，只换材质）
- 5 列表格（#/匹配模式/候选/视觉/操作）结构不变。
- 拖拽排序、就地改候选模型名、行内模型下拉全部保留行为，只换材质。

### 7.4 请求日志（第三批，Q10 只换材质）
- 本轮保留当时 9 列定宽表格、colgroup 与展开详情结构，仅换材质；当时 24 对字段/6 列尝试明细属于历史基线，不是当前最大字段数。
- 主筛选落地时由原 6 格收为桌面 5 列，高级诊断字段与筛选绑定保留。
- 日志卡标题行放高级诊断 / 刷新 / 清空等操作，横向滚动 `min-w-[930px]` 保留。
- 当前依然 9 列，但后续首字耗时需求已经调整列序/列宽与详情字段，表头为「首字/耗时」；功能与配色后续演进不算本轮只换材质承诺未完成。

### 7.5 全局设置（第五批，只换材质）
- 两张卡（全局设置 / 故障转移与熔断）+ YAML 编辑器，结构不变。
- 字段栅格、分区、徽章、checkbox 组只换材质。
- 1024–1280px 已不属于支持目标，原 S2-8 于 2026-10-01 关闭；此处只保留历史说明，统一未完成清单不再保留已关闭事项。

### 7.6 弹窗改右侧抽屉（第五批，Q12=B）
- Provider / 路由由居中模态改成右缘抽屉。实际外层为 `fixed inset-0 flex justify-end`，内层 `glass flex h-full w-full max-w-[40rem] flex-col border-l`，与配置预览统一交互语言。
- 头尾固定，表单区 `min-h-0 flex-1 overflow-y-auto`。本轮保留当时字段/分区/动态行与绑定，只换外壳和材质。
- 后续 `f3561ab` 精修高级设置、焦点、ESC、遮罩关闭和未保存保护；账户/多密钥等还会新增字段，不把本轮「绑定一字不动」写成当前代码永久不变。
- 确认对话框、tooltip、toast、下拉仍按各自居中/锚定方式呈现。

## 8. 实施原则

- **每批先改源文件再重建两个产物**，缺一不可：`make web-css`（CSS）+ `go run ./cmd/webbuild`（HTML）。
- **令牌先行**：第一批把色板/圆角/阴影/字重落到 `tailwind.config.js` 与 `src/tailwind.css`，
  后续各批只在模板里用这些令牌，不再散落硬编码颜色。
- **行为零回归**：不改任何 `.js.part` 的逻辑成员；只改 `index.template.html` 的结构与 class，
  和 `src/tailwind.css`/`tailwind.config.js` 的令牌与组件取值。

## 9. 批次划分与验收

| 批次 | 内容 | 对应 § |
|---|---|---|
| 1 | 设计令牌 + 导航壳（去顶栏、侧栏底部状态卡、配置操作条） | §5 §6 |
| 2 | 监控仪表盘完整重排（替换绝对定位等高） | §7.1 |
| 3 | 请求日志（只换材质） | §7.4 |
| 4 | 提供商 + 路由两个列表页（只换材质） | §7.2 §7.3 |
| 5 | 两个弹窗改抽屉 + 全局设置页 | §7.5 §7.6 |

原流程为每批自动检查后使用者浏览器验收；批次 1 历史记录已通过，批次 2–5 于 2026-11-01 由使用者确认此前已完成视觉验收，当前没有「通过才进下一批」的未完成门槛。本次未启动 compose 或重新执行浏览器验收。

## 10. 现行重跑命令（不改写历史测试记录）

从仓库根目录执行，串行复用固定验证容器名。以下为复现方式，不表示本次文档子任务执行过。

```bash
# 全量 Go 测试、vet 与 HTML 产物检查（国内 GOPROXY + module cache）
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work -v ai-gateway-gomod:/go/pkg/mod \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27-alpine sh -c 'go run ./cmd/webbuild -check && go vet ./... && go test -count=1 ./...'

# CSS 产物单独重建；版本与既有构建管线固定一致，webbuild 不兜底 CSS。
make web-css

# 若改过前端源码，重建 HTML 后再检查（使用容器工具链）。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work -v ai-gateway-gomod:/go/pkg/mod \
  -e GOPROXY=https://goproxy.cn,direct \
  golang:1.27-alpine sh -c 'go run ./cmd/webbuild && go run ./cmd/webbuild -check'

# gofmt -l 有输出也可能退出 0，必须检查输出内容；既有 breaker.go 例外见文首。
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work golang:1.27-alpine gofmt -l ./cmd ./internal
```

race 使用 Debian 版 `golang:1.27`（含 gcc），不要在 Alpine 临时装编译器。本轮纯 UI 本身不要求新增 race；本会话已核实的相关包 race 记录见 TASKS.md。
