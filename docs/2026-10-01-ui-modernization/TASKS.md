# TASKS.md — 前端 UI 现代化重构

格式：`[ ] 任务名 | 批次 | 依赖`
对齐 `DESIGN.md`；每批先改源文件，再重建 `vendor/tailwind.css` 与 `index.html` 两个产物，缺一即 CI 红。

> 贯穿全程的红线（DESIGN §4）：
> - 不改任何 `.js.part` 的逻辑成员（Q6 行为零回归）；只改 `index.template.html` 结构/class 与
>   `src/tailwind.css` / `tailwind.config.js` 的令牌与组件取值。
> - 组件类（`.btn*` `.field` `.chip` `.glass` `.material-symbols-outlined` `.icon-fill` `.gw-*`）
>   只改值、不改名、不删、不挪层。
> - 产物字符串护栏（`health?.listenAddress`、`!canSave()`、`isDirty()`、`restartSuffix` ×2、
>   两条 vendor `<link>/<script>`、图标 data URI）必须在产物里保留。
> - 不新增 Material Symbols 图标名（否则要重新子集化字体，需外网）。

## 进度（2026-10-01）

- **批次 1**:已完成并通过验证(CSS/HTML 产物重建 + `cmd/gateway`、`webbuild` 测试全绿),用户浏览器验收通过。
- **批次 2–5**:源码改动已全部落地,产物重建与自动检查已通过(见下),待用户一次性浏览器验收。
  - 自检结果(2026-10-02):`vendor/tailwind.css` 重建后与提交版逐字节一致(CI 同款 diff);
    `index.html` 重建为 412215 字节且 `webbuild -check` 判定与源码一致;
    `go test ./cmd/gateway/ ./internal/webbuild/` 全绿(覆盖产物字符串断言、样式表字体与组件层层叠顺序、
    Provider/Route 字段覆盖护栏、sentinel 字面量护栏)。模板标签配平 div 308/308、section 15/15、aside 3/3。
  - 批 2 监控重排:绝对定位等高 hack 全部移除;KPI 桌面端 4 列(`xl:grid-cols-4`);主区 3/1(Provider 占 3,
    aside 改 `flex flex-col`、状态码分布 `flex-1` 吸收余高)。
  - **批 2 布局返工(第一轮验收反馈)**:首版观测区用 `xl:items-start`,同行两块高矮悬殊时短的一侧塌出大片空洞
    (延迟排行很长、运行异常几乎空着)。改为**按内容体量配对** + 同行等高(去掉 items-start,用默认 stretch):
    第一行两块长列表(延迟排行 1/3 | 最近错误 2/3),第二行两块短面板(配置健康 1/3 | 运行异常 2/3);
    视觉顺序用 `xl:order-1..4` 调整、DOM 顺序不动;两块长列表统一 `max-h-[22rem]` + `min-h-0 flex-1` 内部滚动封顶,
    使两行高度可控且同行对齐。
  - **批 2 布局返工(第二轮验收反馈)**:「运行异常」是高频显示,与「最近错误」位置对调——运行异常移到第一行右侧
    (延迟排行 1/3 | 运行异常 2/3),最近错误移到第二行右侧(配置健康 1/3 | 最近错误 2/3),仍用 `xl:order` 调序、DOM 不动。
    四块观测面板改为固定等高 `xl:h-[26rem]` + `flex flex-col` + 表体 `min-h-0 flex-1 overflow-y-auto` 卡片内滚动;
    运行异常去掉状态事件内层 `max-h-80` 的二级滚动,整卡单一滚动条。
  - 批 3 日志:三个操作按钮(高级诊断/刷新/清空)迁入卡片标题行,主筛选网格收为 5 列;表格与展开详情结构未动(Q10)。
  - 批 4 提供商/路由:材质由批 1 令牌覆盖,结构与行内交互未动;「添加」按钮本就在卡片标题行。
  - 批 5 弹窗改抽屉:Provider / 路由两个弹窗由居中模态改右缘抽屉(`fixed inset-0 flex justify-end` + `h-full max-w-[40rem] border-l`),表单字段与绑定一字未动;设置页材质由令牌覆盖;KNOWN_ISSUES 窄窗口条目已关闭。

## 批次 1：设计令牌 + 导航壳

- [ ] T1-1 色板/圆角/阴影令牌 | 批1 | —
  - `tailwind.config.js`：按 DESIGN §5.1 改 13 个颜色值（只改值不改名）；
    §5.2 圆角 `lg/xl/2xl` 改 12/16/20px；§5.3 `boxShadow.glow` 改柔和投影。
- [ ] T1-2 字重轴 + 背景渐变 | 批1 | —
  - `src/tailwind.css`：`.material-symbols-outlined` 的 `wght 500→300`、`.icon-fill` 的 `wght 600→400`；
    `@layer base` 的 `body` 背景渐变按 §5.5 调偏蓝。
- [ ] T1-3 删全局顶栏与移动端抽屉 | 批1 | —
  - `index.template.html`：删 `<header>`（64px 顶栏）、`mobileNavOpen` 遮罩与抽屉、汉堡按钮。
  - 顶栏内的健康胶囊、配置预览/重载/保存按钮不是删除而是搬迁（见 T1-4、T1-5）。
- [ ] T1-4 侧边栏通栏 + 底部状态卡 | 批1 | T1-3
  - 侧边栏去掉 `hidden lg:flex`，桌面端恒显通栏到底。
  - 底部新增状态卡：在线圆点 + `health?.listenAddress` + 队列/直通模式；保留「配置文件」路径卡；
    页脚一行「本地安全模式」（不显示版本）。
  - **必须保留** `health?.listenAddress` 表达式（护栏）。
- [ ] T1-5 配置三页顶部操作条 | 批1 | T1-3
  - 配置区（providers/routes/settings）顶部放一行右对齐按钮簇：配置预览、重载、保存 + 「需重启」提示。
  - 搬迁时**原样保留** `x-show="isConfigTab()"`、`:disabled="loading || !canSave()"`、`isDirty()`、
    `restartSuffix(result)` 相关 DOM 与绑定（护栏，`restartSuffix` 在产物里需出现 ≥2 次）。
  - 监控/日志页的操作按钮（检测/重置熔断/刷新；自动滚动/重载/清空/高级诊断）移入各自卡片标题行。
- [ ] T1-6 未保存浮层材质 | 批1 | T1-1
  - 可拖拽 dirty toast 行为不变，仅随新令牌更新材质（圆角/描边/背景）。
- [ ] T1-7 重建产物 + 自检 | 批1 | T1-1..T1-6
  - `make web-css` 重建 CSS；`go run ./cmd/webbuild` 重建 HTML；`go run ./cmd/webbuild -check` exit=0。
  - 容器内 `go test ./...` 全绿（重点 `TestEmbeddedAdminPage*`、`TestBuiltStylesheet*`、
    `TestStaticAssetsAndSecurityHeaders`）。
  - **用户浏览器验收**：整体色调、导航观感、操作条落点、侧栏状态卡；通过才进批 2。

## 批次 2：监控仪表盘完整重排（Q15=B）

- [ ] T2-1 替换绝对定位等高 | 批2 | 批1
  - 去掉 Provider 运行状态、延迟排行等面板的 `xl:absolute xl:inset-0` 脱流 hack，
    改 CSS Grid `items-stretch` + 面板内 `flex flex-col` + 滚动区 `flex-1 min-h-0` 自然等高。
- [ ] T2-2 KPI 卡与主区重排 | 批2 | T2-1
  - 4 张 KPI 卡顶部横排；主区左大右小两栏，Provider 运行状态为主视觉。
  - 保留每张卡的 window chip、`gw-skeleton` 骨架、`_firstLoadDone` 空/载分离逻辑（不动 JS）。
- [ ] T2-3 观测面板编排 | 批2 | T2-1
  - 延迟排行 / 运行异常 / 最近错误 / 配置健康 编排进规整网格，等高由 grid 保证。
  - 表格 min-w、sticky thead、tooltip 行为保留，只换材质。
- [ ] T2-4 重建产物 + 自检 | 批2 | T2-1..T2-3
  - `make web-css` + `webbuild` + `-check` exit=0；`go test ./...` 全绿。
  - **用户浏览器验收**：面板对齐、等高是否稳、信息密度；通过才进批 3。

## 批次 3：请求日志（Q10 只换材质）

- [ ] T3-1 日志卡与筛选区材质 | 批3 | 批1
  - 卡片、主筛选 6 格、高级诊断面板只换材质；筛选结构与 `x-model` 不动。
  - 操作按钮（自动滚动/重载/清空筛选/高级诊断入口）进卡片标题行。
- [ ] T3-2 日志表格材质 | 批3 | T3-1
  - 9 列定宽表格、colgroup 宽度、sticky thead **结构不变**；只换表头样式、行 hover、分隔、
    等级/状态徽章配色。`min-w-[930px]` 横向滚动保留。
- [ ] T3-3 展开详情材质 | 批3 | T3-2
  - 24 对字段 dl 栅格、6 列尝试明细子表、错误全文框、转移轨迹框只换材质；展开/收起逻辑不动。
- [ ] T3-4 重建产物 + 自检 | 批3 | T3-1..T3-3
  - `make web-css` + `webbuild` + `-check`；`go test ./...` 全绿。
  - **用户浏览器验收**：日志页观感贴参考稿材质，列结构未变；通过才进批 4。

## 批次 4：提供商 + 路由列表页（只换材质）

- [ ] T4-1 Provider 列表材质 | 批4 | 批1
  - 10 列表格结构不变；重做卡片/表头/行分隔/徽章/按钮/hover；禁用行 `opacity-50` 保留。
  - 「添加 Provider」按钮进卡片标题行。启用开关、编辑/复制/删除、tooltip 行为不动。
- [ ] T4-2 路由列表材质 | 批4 | 批1
  - 5 列表格结构不变；拖拽排序（`gw-drag-*`）、就地改候选模型名、行内模型下拉行为全保留，只换材质。
  - 候选 chip（provider → model）、视觉 chip 配色随新令牌。
- [ ] T4-3 重建产物 + 自检 | 批4 | T4-1,T4-2
  - `make web-css` + `webbuild` + `-check`；`go test ./...` 全绿（含 `TestFrontendCoversAll*` 护栏）。
  - **用户浏览器验收**：两个列表页材质与行内交互正常；通过才进批 5。

## 批次 5：弹窗改抽屉（Q12=B）+ 全局设置

- [ ] T5-1 Provider 弹窗改右侧抽屉 | 批5 | 批1
  - 外壳从居中模态改 `fixed right-0 top-0 bottom-0` 右缘抽屉，占满高度，`w-[36rem]` 一类。
  - **表单字段、4 个分区、动态行（自定义请求头/请求体）与所有 `x-model`、`:selected`、probe 逻辑一字不动**。
  - ESC 关闭、无点击外部关闭的既有行为保留。
- [ ] T5-2 路由弹窗改右侧抽屉 | 批5 | T5-1
  - 同上外壳改抽屉；候选行、限额展开面板、高级设置、视觉伴随区的字段与绑定全保留。
  - 弹窗内模型下拉（`absolute` 锚定）定位逻辑不动，只换材质。
- [ ] T5-3 全局设置页材质 | 批5 | 批1
  - 两张卡（全局设置 / 故障转移与熔断）+ YAML 编辑器只换材质；字段栅格/分区/徽章/checkbox 组不动。
- [ ] T5-4 确认框/tooltip/toast/下拉材质 | 批5 | 批1
  - 这些悬浮层保持居中/锚定定位，只换材质；拖拽行内编辑的 inline 下拉定位逻辑不动。
- [ ] T5-5 重建产物 + 自检 + 收尾 | 批5 | T5-1..T5-4
  - `make web-css` + `webbuild` + `-check`；`go test ./...` 全绿（含 `TestFrontendSentinelLiteralsMatchBackend`
    与字段护栏）；容器内 `gofmt -l` 无 `.go` 改动、`go vet` 干净。
  - 关闭 `docs/KNOWN_ISSUES.md` 的「配置页 1024–1280px 窄窗口未肉眼验收」一条（本轮只做桌面端）。
  - **用户浏览器验收**：抽屉表单可读性、密集字段排版；通过即整轮完成。

## 验收标准（每批与整轮通用）

1. `go run ./cmd/webbuild -check` exit=0（`TestRepoArtifactMatchesSources` 不失败）。
2. `vendor/tailwind.css` 用 `npx tailwindcss@3.4.17` 重建后与提交版逐字节一致（CI「Verify built stylesheet」同款比对）。
3. 容器内 `go test ./...` 全绿；重点护栏：`TestEmbeddedAdminPage*`、`TestBuiltStylesheetKeepsLocalFontsAndComponentLayers`、
   `TestStaticAssetsAndSecurityHeaders`、`TestFrontendCoversAllProviderFields`、`TestFrontendCoversAllRouteFields`、
   `TestFrontendSentinelLiteralsMatchBackend`。
4. 无任何 `.go` 运行逻辑改动（本轮纯前端）；`gofmt -l ./cmd ./internal` 对 `.go` 无输出。
5. 行为零回归：每条交互路径与改动前一致（人工 + 测试双保）。
6. 每批自动检查输出由 Claude 贴出；视觉验收由用户在浏览器完成，通过才进下一批。

## 提交约定

- 分支：`dev`（非主分支，直接在此提交）。
- 每批验收通过后，按「提交确认」规则先向用户简述、等明确确认再 `git commit`，不自动提交、不 push。
- 提交信息走 Conventional Commit，如 `feat(web): UI 现代化重构（批次 N：……）`。
