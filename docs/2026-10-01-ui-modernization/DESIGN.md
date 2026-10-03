# 前端 UI 现代化重构设计

日期：2026-10-01
状态：规划完成，待分批实现
分支：dev
参考：用户提供的目标视觉稿（深蓝毛玻璃、大圆角卡片、细线图标、通栏侧边栏、无全局顶栏）

> **本文定位**：这是一次**只动 UI、不动功能逻辑**的重构（见决策 Q6=A）。
> 参考稿提供的是「材质与观感语言」——毛玻璃、渐变、大圆角、徽章样式、细线图标、
> 侧边栏风格——套用到现有布局上，而非逐页照搬参考稿的结构。
> 结构性改动只有三处：去顶栏、弹窗改右侧抽屉、监控页重排。其余各页一律保持结构不变、只换材质。
> 代码是唯一事实来源；本文与实现对齐，实现偏离本文时以实跑结果为准并回头更新本文。

---

## 1. 需求目标

把 `cmd/gateway/web` 的管理界面从当前「高饱和青 + 近黑毛玻璃」观感，重构为参考稿那种
「偏蓝深色 + 柔和青 + 大圆角浮层」的现代观感。技术栈不变（Alpine 3.14.9 + Tailwind 3.4.17 CLI 构建），
五个页签划分不变（监控仪表盘 / 提供商 / 路由 / 请求日志 / 全局设置），所有交互路径一个不增不减。

## 2. 不在本轮范围

- **不换框架**（Q2=A）：不引入 Vue/React/Svelte 与打包器，`cmd/webbuild` 产物管线、SSE 热加载、
  CI 三道关卡、钉住 HTML 字符串的 Go 测试全部保留。
- **不动功能逻辑**（Q6=A）：不新增、不删除、不改任何交互行为。像素可以全变，行为必须逐条可比对。
  - 唯一的「交互删除」是导航壳层面的死代码：移动端抽屉与汉堡按钮（Q5 确认只做桌面端后它们无服务对象）。
- **不照搬参考稿里本项目没有数据源的东西**（Q6 的直接后果）：
  - 顶部「在途请求」实时面板——指标系统只有已完成请求的环形缓冲，无在途追踪，不新增。
  - token 计数（输入/输出/缓存读取/推理）——`RequestLog` 无这些字段，只有启用上下文窗口时的
    估算输入 token，不补埋点。
  - 首字延迟——未记录，不补。
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

## 4. 硬约束（任何一批都不得触碰的红线）

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
- **圆角全局钉死**：`tailwind.config.js` 把 `lg/xl/2xl` 三档统一改成 8px。放开为更大圆角会一次性影响
  页面每一个圆角类——这正是本轮想要的效果，但要在第一批整体校验，确认没有不该变圆的地方意外变圆。

## 5. 设计令牌方案（第一批）

只改值不改名（Q9=A）。下表是从参考稿反推的方向，第一批出可视产物时按肉眼校准（Q16=A）。

### 5.1 色板（`tailwind.config.js` theme.extend.colors）
| 令牌 | 现值 | 拟值 | 说明 |
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
`lg: 8px → 12px`，`xl: 8px → 16px`，`2xl: 8px → 20px`。卡片用 `rounded-xl/2xl` 拿到参考稿的大圆角。

### 5.3 阴影（boxShadow.glow）
现 `0 0 0 1px rgba(71,214,255,.12), 0 16px 48px rgba(0,0,0,.28)`
拟 `0 0 0 1px rgba(148,163,184,.08), 0 18px 50px rgba(0,0,0,.35)`（弱化青色描边环，加深浮层投影）。

### 5.4 字重轴（`src/tailwind.css` 组件层）
`.material-symbols-outlined` 的 `font-variation-settings` 把 `wght 500 → 300`（细线条）；
`.icon-fill` 的 `wght 600 → 400`。字号轴与 FILL 轴不动（FILL 1 仍供选中态）。

### 5.5 背景渐变（`src/tailwind.css` @layer base body）
现：右上角青色径向光 + 自上而下深色线性。
拟：主径向光转柔青并下移、左下角补一团极淡的 indigo、线性底色换成偏蓝的 `#0d1118 → #0b0e14`。
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

### 7.1 监控仪表盘（第二批，Q15=B 完整重排）
- 现状：4 张 KPI 卡 + Row A（Provider 运行状态 9 栏 / 右侧 aside 3 栏含运行时状态 + 状态码分布）
  + Row B（延迟排行 5 / 运行异常 7 / 最近错误 7 / 配置健康 5，合计 24 栏＝两子行）。
- 等高靠 `xl:absolute xl:inset-0` 脱流撑起，低于 xl 退回普通流——**本轮替换掉这个脆弱做法**，
  改用 CSS Grid 行高自然对齐（`items-stretch` + 面板内部 `flex flex-col` + 可滚动区 `flex-1 min-h-0`），
  不再依赖绝对定位。
- 面板重排目标：KPI 卡顶部横排；主区左大右小两栏，Provider 运行状态为主视觉；观测类面板
  （延迟排行/运行异常/最近错误/配置健康）编排进规整网格，等高由 grid 保证。
- 具体栅格在第二批给可视方案，验收后定稿。

### 7.2 提供商（第四批，只换材质）
- 10 列表格结构不变（启用/名称/格式/Base URL/Key/代理/并发/速率/队列等待/操作）。
- 行内交互不变（启用开关、编辑/复制/删除、tooltip、禁用行压暗）。
- 只重做：卡片材质、表头样式、行分隔、徽章、按钮、hover 态。
- 「添加 Provider」按钮进卡片标题行。

### 7.3 路由（第四批，只换材质）
- 5 列表格（#/匹配模式/候选/视觉/操作）结构不变。
- 拖拽排序、就地改候选模型名、行内模型下拉全部保留行为，只换材质。

### 7.4 请求日志（第三批，Q10 只换材质）
- 9 列定宽表格 + colgroup 宽度 + 点行展开最多 24 对字段 + 6 列尝试明细子表，**结构全保留**。
- 筛选区（主筛选 6 格 + 高级诊断面板）结构保留。
- 只重做材质：卡片、筛选控件、表头、行 hover、展开区、等级/状态徽章、尝试明细配色。
- 日志卡标题行放操作按钮（自动滚动/重载/清空筛选/高级诊断入口）。
- 横向滚动（`min-w-[930px]`）保留——桌面端宽度足够，不改列结构。

### 7.5 全局设置（第五批，只换材质）
- 两张卡（全局设置 / 故障转移与熔断）+ YAML 编辑器，结构不变。
- 字段栅格、分区、徽章、checkbox 组只换材质。
- 顺带关闭 KNOWN_ISSUES 里「配置页 1024–1280px 窄窗口未肉眼验收」一条——本轮只做桌面端，
  在 1280px+ 验收通过即可标记该历史项不再适用。

### 7.6 弹窗改右侧抽屉（第五批，Q12=B）
- Provider 弹窗、路由弹窗从居中模态（`fixed inset-0 grid place-items-center`，`max-w-3xl` 内滚）
  改为右缘抽屉（`fixed right-0 top-0 bottom-0`，占满高度，`w-[36rem]` 一类），与既有「配置预览」抽屉
  形成一致交互语言。
- **表单字段、分区、动态行（自定义请求头/请求体、候选行、限额面板）与所有 `x-model` 绑定一字不动**
  （§4.3 护栏）——只换外壳容器与排版。
- 确认对话框、tooltip、toast、各下拉保持居中/锚定定位，只换材质。

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

每批交付时 Claude 跑完全部自动检查并贴输出；视觉好坏由用户在浏览器验收（开发模式 SSE 热加载：
`docker compose -f docker-compose.yml -f docker-compose.dev.yml up`）。验收通过才进下一批。

## 10. 验证命令（本机实测形态，见 [[go-verify-container-workarounds]]）

```bash
# 全部 Go 测试（含产物断言、字段护栏）
docker run --pull never --name "ai-gateway-dev-verify-test-$(date +%s)" \
  -v "$PWD":/work -w /work -v ai-gateway-gomod:/go/pkg/mod \
  -e GOPROXY=https://goproxy.cn,direct golang:1.27-alpine go test ./...

# 重建 CSS 产物（本机 node 24 可直接跑；与 CI 同版本 3.4.17）
cd cmd/gateway/web && npx --yes tailwindcss@3.4.17 -c tailwind.config.js -i src/tailwind.css -o vendor/tailwind.css -m

# 重建 HTML 产物并校验
go run ./cmd/webbuild && go run ./cmd/webbuild -check

# gofmt（-l 有输出退出码仍 0，别用 && echo 判断）、vet
docker run --pull never --name "ai-gateway-dev-verify-fmt-$(date +%s)" \
  -v "$PWD":/work -w /work golang:1.27-alpine gofmt -l ./cmd ./internal
```
> race 检测需 Debian 版镜像、本机可能缺；本轮纯前端改动不涉并发，不强制 race。
