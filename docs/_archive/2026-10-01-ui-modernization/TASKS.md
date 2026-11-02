# TASKS.md — 前端 UI 现代化重构

> 状态：已完成；2026-11-01 文档按代码同步并获准归档。
> 主实现提交：2026-10-02 的 `09eb4b4`；现行描述按 `46a72c6` 核对。原各批未勾选是清单状态陈旧，不代表实现仍未完成。

格式：`[x] 已完成的任务名 | 批次 | 依赖`。
对齐 `DESIGN.md`；以下红线约束的是 2026-10-01 这轮纯 UI 实施，后续需求的功能与布局演进另行说明，不倒推为本轮违规。每批当时先改源文件再重建 CSS/HTML 两个产物；本次只维护 Markdown，不重建产物。

## 最终完成与验收记录（2026-11-01）

- 批次 1 的历史浏览器验收记录保留。**2026-11-01 使用者确认此前验收完成：批次 2–5 浏览器视觉验收已完成**，不再保留「待用户一次性验收」作为当前状态；这不是本次文档维护重新执行浏览器验收。
- 本会话主会话 Claude 在 `golang:1.27-alpine` 容器确认 `webbuild -check`、`go vet ./...` 退出 0，`go test -count=1 ./...` 全部通过（包括产物字符串、样式层叠、字段覆盖与 sentinel 护栏）。CSS 重建逐字节比对沿用 2026-10-02 历史记录，本次未重新运行 Tailwind CLI；HTML 产物历史字节数不作为现行验收值。
- 本次格式检查唯一既有例外是 `internal/breaker/breaker.go`，与本轮 UI 和此次 Markdown 维护无关，未顺带修改，不再声称全仓库 `gofmt -l` 无输出。
- 当前圆角令牌为 `lg/xl/2xl = 12/16/20px`；当前观测区采用自然高度与 `max-height` 封顶，不是固定 `xl:h-[26rem]`。
- 现行主区为 Provider 3/4 + 右栏 1/4；右栏包括运行时状态、状态码分布和配置健康。观测区首行为延迟排行 1/3 + 运行异常 2/3（各 `xl:max-h-[26rem]`），次行为最近错误通栏（`xl:max-h-[22rem]`）。同行由 Grid 默认 stretch 对齐，内容少时自然收缩，滚动留在内容区。
- 2026-10-03 的 `a1ea575` 增加流式首字耗时，2026-10-05 的 `35fe068` 调整「首字/耗时」表头与列宽；2026-10-17 的 `f3561ab` 精修自然高度布局与列表/抽屉交互。它们属于本需求后演进，不是本轮承诺遗漏。
- 1024–1280px 已不属于支持目标，原 S2-8 于 2026-10-01 关闭；统一已知问题不再混入这条已关闭事项。
- 所有批次任务已完成，无剩余视觉验收门槛；历史返工记录保留用于解释方案演进，当前口径以本节与 DESIGN.md 为准。

> 贯穿本轮实施的红线（DESIGN §4）：
> - 不改任何 `.js.part` 的逻辑成员（Q6 行为零回归）；只改 `index.template.html` 结构/class 与
>   `src/tailwind.css` / `tailwind.config.js` 的令牌与组件取值。
> - 组件类（`.btn*` `.field` `.chip` `.glass` `.material-symbols-outlined` `.icon-fill` `.gw-*`）
>   只改值、不改名、不删、不挪层。
> - 产物字符串护栏（`health?.listenAddress`、`!canSave()`、`isDirty()`、`restartSuffix` ×2、
>   两条 vendor `<link>/<script>`、图标 data URI）必须在产物里保留。
> - 不新增 Material Symbols 图标名（否则要重新子集化字体，需外网）。

## 历史实施与返工记录（2026-10-01–10-02，非当前状态）

- **批次 1**:已完成并通过验证(CSS/HTML 产物重建 + `cmd/gateway`、`webbuild` 测试全绿),用户浏览器验收通过。
- **批次 2–5**：源码与产物在 2026-10-02 已落地并通过当时自动检查（见下）；2026-11-01 使用者确认此前浏览器验收完成，当前不再等待验收。
  - 自检结果(2026-10-02):`vendor/tailwind.css` 重建后与提交版逐字节一致(CI 同款 diff);
    `index.html` 的原文自检记录为 412215 字节且 `webbuild -check` 判定与源码一致；同日 `09eb4b4` 的提交说明记为 412285 字节，历史两处数值有冲突，保留来源差异而不选一个当现行产物大小；
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
  - **后续布局调整（2026-10-17，`f3561ab`）**：以上固定 26rem 与两行四面板方案已被自然高度 + 最大高度封顶取代；配置健康并入上方右栏，最近错误改通栏。保留前两轮反馈的历史解释，不把旧固定高度当成现行验收值。
  - 批 3 日志:三个操作按钮(高级诊断/刷新/清空)迁入卡片标题行,主筛选网格收为 5 列;表格与展开详情结构未动(Q10)。
  - 批 4 提供商/路由:材质由批 1 令牌覆盖,结构与行内交互未动;「添加」按钮本就在卡片标题行。
  - 批 5 弹窗改抽屉:Provider / 路由两个弹窗由居中模态改右缘抽屉(`fixed inset-0 flex justify-end` + `h-full max-w-[40rem] border-l`),表单字段与绑定一字未动;设置页材质由令牌覆盖;KNOWN_ISSUES 窄窗口条目已关闭。

## 批次 1：设计令牌 + 导航壳

- [x] T1-1 色板/圆角/阴影令牌 | 批1 | —
  - `tailwind.config.js`：按 DESIGN §5.1 改 13 个颜色值（只改值不改名）；
    §5.2 圆角 `lg/xl/2xl` 改 12/16/20px；§5.3 `boxShadow.glow` 改柔和投影。
- [x] T1-2 字重轴 + 背景渐变 | 批1 | —
  - `src/tailwind.css`：`.material-symbols-outlined` 的 `wght 500→300`、`.icon-fill` 的 `wght 600→400`；
    `@layer base` 的 `body` 背景渐变按 §5.5 调偏蓝。
- [x] T1-3 删全局顶栏与移动端抽屉 | 批1 | —
  - `index.template.html`：删 `<header>`（64px 顶栏）、`mobileNavOpen` 遮罩与抽屉、汉堡按钮。
  - 顶栏内的健康胶囊、配置预览/重载/保存按钮不是删除而是搬迁（见 T1-4、T1-5）。
- [x] T1-4 侧边栏通栏 + 底部状态卡 | 批1 | T1-3
  - 侧边栏去掉 `hidden lg:flex`，桌面端恒显通栏到底。
  - 底部新增状态卡：在线圆点 + `health?.listenAddress` + 队列/直通模式；保留「配置文件」路径卡；
    页脚一行「本地安全模式」（不显示版本）。
  - **必须保留** `health?.listenAddress` 表达式（护栏）。
- [x] T1-5 配置三页顶部操作条 | 批1 | T1-3
  - 配置区（providers/routes/settings）顶部放一行右对齐按钮簇：配置预览、重载、保存 + 「需重启」提示。
  - 搬迁时**原样保留** `x-show="isConfigTab()"`、`:disabled="loading || !canSave()"`、`isDirty()`、
    `restartSuffix(result)` 相关 DOM 与绑定（护栏，`restartSuffix` 在产物里需出现 ≥2 次）。
  - 监控/日志页的操作按钮（检测/重置熔断/刷新；自动滚动/重载/清空/高级诊断）移入各自卡片标题行。
- [x] T1-6 未保存浮层材质 | 批1 | T1-1
  - 可拖拽 dirty toast 行为不变，仅随新令牌更新材质（圆角/描边/背景）。
- [x] T1-7 重建产物 + 自检 | 批1 | T1-1..T1-6
  - `make web-css` 重建 CSS；`go run ./cmd/webbuild` 重建 HTML；`go run ./cmd/webbuild -check` exit=0。
  - 容器内 `go test ./...` 全绿（重点 `TestEmbeddedAdminPage*`、`TestBuiltStylesheet*`、
    `TestStaticAssetsAndSecurityHeaders`）。
  - **使用者浏览器验收已完成（历史记录）**：整体色调、导航、操作条与侧栏状态卡；本次保留原批次 1 验收事实。

## 批次 2：监控仪表盘完整重排（Q15=B）

- [x] T2-1 替换绝对定位等高 | 批2 | 批1
  - 去掉 Provider 运行状态、延迟排行等面板的 `xl:absolute xl:inset-0` 脱流 hack，
    改 CSS Grid `items-stretch` + 面板内 `flex flex-col` + 滚动区 `flex-1 min-h-0` 自然等高。
- [x] T2-2 KPI 卡与主区重排 | 批2 | T2-1
  - 4 张 KPI 卡顶部横排；主区左大右小两栏，Provider 运行状态为主视觉。
  - 保留每张卡的 window chip、`gw-skeleton` 骨架、`_firstLoadDone` 空/载分离逻辑（不动 JS）。
- [x] T2-3 观测面板编排 | 批2 | T2-1
  - 延迟排行 / 运行异常采用 1/3 + 2/3，首行 `xl:max-h-[26rem]` 封顶并随内容自然收缩；最近错误在次行通栏、`xl:max-h-[22rem]`；配置健康在上方右栏。
  - Grid 默认 stretch 保持同行对齐，内容区 `min-h-0 flex-1 overflow-y-auto` 承接滚动；不是固定四面板等高 26rem。
  - 表格 min-w、sticky thead、tooltip 行为保留；后续布局调整见历史记录。
- [x] T2-4 重建产物 + 自检 | 批2 | T2-1..T2-3
  - `make web-css` + `webbuild` + `-check` exit=0；`go test ./...` 全绿。
  - **使用者浏览器验收已确认完成**：面板对齐、等高与信息密度；2026-11-01 使用者确认此前验收完成，非本次重新实跑。

## 批次 3：请求日志（Q10 只换材质）

- [x] T3-1 日志卡与筛选区材质 | 批3 | 批1
  - 卡片与高级诊断面板换材质；主筛选从原 6 格收为桌面 5 列，筛选字段与 `x-model` 保留，不把「筛选网格未动」写成现行事实。
  - 高级诊断 / 刷新 / 清空迁入卡片标题行，既有日志筛选逻辑保持。
- [x] T3-2 日志表格材质 | 批3 | T3-1
  - 本轮完成时 9 列定宽表格、colgroup、sticky thead 结构保持，只换表头/行 hover/分隔/徽章材质；`min-w-[930px]` 横向滚动保留。
  - 现行仍为 9 列，但后续首字耗时需求调整了列序与列宽，耗时列为「首字/耗时」；不把当前列宽写成从本轮起永未变化。
- [x] T3-3 展开详情材质 | 批3 | T3-2
  - 本轮对当时 24 对字段 dl、6 列尝试明细、错误全文/转移轨迹只换材质，不改展开/收起逻辑；后续首字耗时、多密钥等需求增加明细，历史字段数量不是当前硬上限。
- [x] T3-4 重建产物 + 自检 | 批3 | T3-1..T3-3
  - `make web-css` + `webbuild` + `-check`；`go test ./...` 全绿。
  - **使用者浏览器验收已确认完成**：日志页材质与本轮列结构保留；2026-11-01 使用者确认此前验收完成，后续首字耗时布局另行演进。

## 批次 4：提供商 + 路由列表页（只换材质）

- [x] T4-1 Provider 列表材质 | 批4 | 批1
  - 10 列表格结构不变；重做卡片/表头/行分隔/徽章/按钮/hover；禁用行 `opacity-50` 保留。
  - 「添加 Provider」按钮进卡片标题行。启用开关、编辑/复制/删除、tooltip 行为不动。
- [x] T4-2 路由列表材质 | 批4 | 批1
  - 5 列表格结构不变；拖拽排序（`gw-drag-*`）、就地改候选模型名、行内模型下拉行为全保留，只换材质。
  - 候选 chip（provider → model）、视觉 chip 配色随新令牌。
- [x] T4-3 重建产物 + 自检 | 批4 | T4-1,T4-2
  - `make web-css` + `webbuild` + `-check`；`go test ./...` 全绿（含 `TestFrontendCoversAll*` 护栏）。
  - **使用者浏览器验收已确认完成**：两个列表页材质与行内交互；2026-11-01 使用者确认此前验收完成，非本次重新实跑。

## 批次 5：弹窗改抽屉（Q12=B）+ 全局设置

- [x] T5-1 Provider 弹窗改右侧抽屉 | 批5 | 批1
  - 外层 `fixed inset-0 flex justify-end`，内层 `h-full w-full max-w-[40rem] flex flex-col border-l`；固定头尾，表单区 `min-h-0 flex-1 overflow-y-auto`。
  - 本轮只改外壳/材质，保留当时字段、4 个分区、动态行与绑定；后续高级设置折叠/账户/密钥功能属于另行演进，当前字段覆盖由护栏检查。
  - 本轮保留当时 ESC 与关闭行为；后续 `f3561ab` 增加遮罩点击关闭、焦点与未保存保护，因此当前不再是「无点击外部关闭」。
- [x] T5-2 路由弹窗改右侧抽屉 | 批5 | T5-1
  - 同上外壳改抽屉；候选行、限额展开面板、高级设置、视觉伴随区的字段与绑定全保留。
  - 弹窗内模型下拉（`absolute` 锚定）定位逻辑不动，只换材质。
- [x] T5-3 全局设置页材质 | 批5 | 批1
  - 两张卡（全局设置 / 故障转移与熔断）+ YAML 编辑器只换材质；字段栅格/分区/徽章/checkbox 组不动。
- [x] T5-4 确认框/tooltip/toast/下拉材质 | 批5 | 批1
  - 这些悬浮层保持居中/锚定定位，只换材质；拖拽行内编辑的 inline 下拉定位逻辑不动。
- [x] T5-5 重建产物 + 自检 + 收尾 | 批5 | T5-1..T5-4
  - `make web-css` + `webbuild` + `-check` 的历史产物记录保留；本会话容器 `webbuild -check`、全量测试与 vet 再验通过。格式唯一既有例外 `internal/breaker/breaker.go` 见文首，不声称全仓库 `gofmt -l` 无输出。
  - 1024–1280px 已不属于支持目标，原 S2-8 于 2026-10-01 关闭；此处保留历史说明，不再作为未完成项。
  - **使用者浏览器验收已确认完成**：2026-11-01 使用者确认此前批次 2–5 的视觉验收完成，整轮完成；非本次文档维护实跑。

## 验收口径（原每批与整轮标准，当前结果见文首）

以下保留原标准，均已完成或由使用者确认；不是当前待执行门槛。CSS 重建幂等是历史记录，未冒充本次重新运行。

1. `go run ./cmd/webbuild -check` exit=0（`TestRepoArtifactMatchesSources` 不失败）。
2. `vendor/tailwind.css` 用 `npx tailwindcss@3.4.17` 重建后与提交版逐字节一致（CI「Verify built stylesheet」同款比对）。
3. 容器内 `go test ./...` 全绿；重点护栏：`TestEmbeddedAdminPage*`、`TestBuiltStylesheetKeepsLocalFontsAndComponentLayers`、
   `TestStaticAssetsAndSecurityHeaders`、`TestFrontendCoversAllProviderFields`、`TestFrontendCoversAllRouteFields`、
   `TestFrontendSentinelLiteralsMatchBackend`。
4. 本轮没有 `.go` 运行逻辑改动；2026-11-01 文档维护也不改代码。当前全仓库 gofmt 唯一既有例外已明确记录，不倒推为本需求改了 Go。
5. 行为零回归：每条交互路径与改动前一致（人工 + 测试双保）。
6. 原交付方式为每批自动检查后使用者浏览器验收；最终使用者确认已完成，见文首，不再要求重新逐批确认。

## 提交约定

- 分支：`dev`（非主分支，直接在此提交）。
- 每批验收通过后，按「提交确认」规则先向用户简述、等明确确认再 `git commit`，不自动提交、不 push。
- 提交信息走 Conventional Commit，如 `feat(web): UI 现代化重构（批次 N：……）`。

## 最终归档状态（2026-11-02）

- 已按使用者授权移入 `docs/_archive/2026-10-01-ui-modernization/`，需求创建日期保留。
- 文档按当前代码和使用者已确认的验收同步，历史失败或阶段记录不再表示当前归档阻断；仍未修复的缺陷和专项测试欠账保留在 [统一已知问题](../../KNOWN_ISSUES.md)。
- 本次仅维护和移动文档，没有修改业务代码、真实配置或账户，没有提交、合并或推送。
