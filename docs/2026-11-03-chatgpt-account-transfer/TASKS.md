> 实现方：Claude（文档模式）

# ChatGPT OAuth 账户统一导入导出与文本导入

创建日期：2026-11-03
状态：实现及验收完成，等待用户确认提交。

## 目标与范围

- 将 ChatGPT OAuth 账户区顶部的「导入账户」「导出账户」合并为「导入 / 导出」。
- 参照提供商「配置导入 / 导出」弹窗，在同一弹窗内切换导入、导出。
- 保留 JSON / ZIP 文件导入，增加直接输入或粘贴 JSON 原文的导入入口。
- 保留既有导出格式、账户选择、全选、清空、下载行为，以及账户行内的单账户快捷导出。
- 不新增后端接口，不扩展支持的凭据格式，不改变重复账户跳过、令牌存储或 Provider 绑定语义。

## 现状与实现落点

- `cmd/gateway/web/src/index.template.html`：顶部入口、现有两个独立账户弹窗；提供商统一弹窗是交互与样式参照。
- `cmd/gateway/web/src/app/00-state.js.part`：弹窗开关、文件、结果、导出选择等状态。
- `cmd/gateway/web/src/app/11-providers.js.part`：账户导入、导出与生命周期处理。
- `cmd/gateway/web/src/app/06-config-persist.js.part`：保存快捷键对上层弹窗的拦截，合并后须同步。
- `cmd/gateway/main.go`：现有 `POST /api/chatgpt/accounts/import` 已接受 `application/json` 原文和 `application/zip`，请求体上限 16 MiB。
- `cmd/gateway/web/index.html`：须由构建工具生成，不手改产物。
- `scripts/verify-provider-route-transfer.cjs`：可参照其假数据与本地请求拦截方式补充账户交互验证。

## 交互方案

### 统一弹窗

- 标题「ChatGPT 账户导入 / 导出」，沿用既有深色样式与提供商弹窗的导出、导入切换按钮。
- 首次打开默认导出页，与提供商弹窗一致；没有账户时显示明确空态，禁用导出提交，但仍可切到导入。
- 打开时默认选择全部现有账户；保留四种导出格式及既有下载文件名规则。
- 导入、导出请求进行时禁止重复提交、关闭弹窗和切换操作，避免异步结果写入已关闭或不相符的页面。
- 关闭按钮与 Esc 使用统一清理方法；保存快捷键只拦截，不触发底层配置保存。
- 导出成功沿用关闭弹窗行为；导入成功保留新增数、跳过数及警告反馈并刷新账户列表。
- 弹窗在窄屏和矮屏下可滚动，操作入口可达；按钮及输入保留明确标签、焦点反馈。

### 文件与文本输入

- 导入页同时展示「JSON 或 ZIP 文件（最大 16 MiB）」及「或粘贴 JSON」文本域，沿用提供商上传或粘贴的布局。
- 两种来源互斥：选择文件清空文本；输入非空文本清空已选文件及文件控件。只提交当前来源，不隐式拼接。
- 文本为既有支持格式的完整 JSON，不支持单独填写裸 token、YAML 或 ZIP 文本。
- 文本以 `application/json` 直接作为请求体发送，不额外包装或二次 JSON 编码；文件保留现有媒体类型选择逻辑。
- 空白文本不可提交；文件与文本均在前端校验 16 MiB 上限，文本按 UTF-8 字节数计算，后端仍是最终校验边界。
- 修改输入后清空旧结果和旧错误；格式识别、账户校验与重复账户判定复用后端。

### 凭据生命周期与错误

- 明确提示文件和文本含 OAuth 凭据，仅在可信本机操作。
- 原文与文件只保存在本次弹窗内存中，不写浏览器持久化存储、不写日志、不附加到 URL。
- 提交结束（成功或失败）清空文件及文本，延续现有文件导入结束即清理的语义；失败时提示重新选择或粘贴。
- 关闭、Esc、重新打开时清空导入源、结果和错误；从导入切到导出时同样清空导入凭据及反馈。
- 错误反馈不拼接输入原文或 token；复用现有安全错误处理。
- 行内单账户导出仍直接导出该账户，不被新的多选或页签状态误影响。

## 任务清单

- [x] T1 合并入口、弹窗及状态生命周期，保留导出能力并同步保存快捷键拦截 | P0 | 40 分钟 | 无
- [x] T2 增加 JSON 文本导入、输入互斥、字节限制、提交保护与凭据清理 | P0 | 30 分钟 | T1
- [x] T3 补充使用假凭据的账户导入导出交互回归验证 | P0 | 40 分钟 | T1、T2
- [x] T4 重建前端产物，执行容器内验证及界面检查，记录实际结果 | P0 | 30 分钟 | T3

## 验收标准

1. 顶部只有一个「导入 / 导出」入口；同一弹窗可切换导入和导出，无账户时也能进入导入。
2. 粘贴有效 JSON 后，接口收到原文与正确媒体类型；成功显示统计并刷新账户列表。
3. JSON 文件、ZIP 文件继续按现有请求协议提交；支持格式与去重语义不变。
4. 空白输入、UTF-8 字节超限被前端拦截；格式错误、接口失败均显示安全错误，按钮恢复可用。
5. 文件和文本切换不会误提交旧来源；修改输入不会残留旧成功反馈。
6. 提交期间无法关闭、切页或重复提交；提交结束与关闭后不保留导入凭据。
7. 四种导出格式、全选/清空、多账户导出、行内单账户导出保持正常。
8. Esc、关闭按钮及保存快捷键正确处理统一弹窗，不误触底层配置保存。
9. 桌面及窄视口下布局可用，长文本与账户列表不撑破弹窗。
10. 前端产物与源码一致；验证只使用假账户与假凭据，不读取本机 OAuth 凭据库、不向真实上游发送请求。

## 验证计划

- 新增 `scripts/verify-chatgpt-account-transfer.cjs`，参照已有浏览器验证脚本，用本机假 origin 和请求拦截覆盖上述交互；检查请求体、媒体类型、按钮禁用、输入清理及导出选择。
- 按仓库构建入口在 Docker Go 工具链容器内重建前端，并运行 `go run ./cmd/webbuild -check`（实施前核对构建入口参数）。
- 使用固定容器名 `ai-gateway-dev-verify`、`golang:1.27-alpine` 执行 `go test ./internal/webbuild ./cmd/gateway ./internal/chatgptauth`；依赖下载代理遵守项目验证约定。
- 执行浏览器交互验证，批量检查桌面与窄视口；运行一次 Impeccable 检测，仅处理本次改动相关问题。
- 记录实际命令、退出状态与测试结果；若环境阻断则说明未验证范围，不能以静态检查替代实跑通过。

## 验收记录（2026-11-03）

- 前端已重建，`git diff --check` 通过。
- 容器验证命令（退出码 0）：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -v ai-gateway-gomod:/go/pkg/mod -w /work \
  -e GOPROXY=https://goproxy.cn,direct golang:1.27-alpine \
  sh -c 'go run ./cmd/webbuild -check && go test ./internal/webbuild ./cmd/gateway ./internal/chatgptauth'
```

```text
cmd/gateway/web/index.html 与源码一致（760591 字节）
ok  ai-gateway/internal/webbuild  0.008s
ok  ai-gateway/cmd/gateway  3.107s
?   ai-gateway/internal/chatgptauth  [no test files]
```

- 首次浏览器启动因 Playwright 默认所需的 Chromium 1228 未安装而失败。经用户同意，测试脚本增加 `PLAYWRIGHT_EXECUTABLE_PATH` 支持，使用已有 Chromium 1234；未安装依赖。
- 浏览器回归命令（退出码 0）：

```bash
PLAYWRIGHT_MODULE=/Users/wyong/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/.pnpm/playwright@1.61.1/node_modules/playwright \
PLAYWRIGHT_EXECUTABLE_PATH=/Users/wyong/Library/Caches/ms-playwright/chromium_headless_shell-1234/chrome-headless-shell-mac-arm64/chrome-headless-shell \
node scripts/verify-chatgpt-account-transfer.cjs
```

```text
通过：文本原文提交、空白拦截、忙碌保护、成功统计与清理
通过：JSON/ZIP、输入互斥、错误清理、UTF-8 超限与切页清理
通过：四种格式、多选、行内快捷导出、Esc 与保存快捷键
通过：无账户导入、桌面/窄屏布局、关闭清理；无页面脚本错误
```

- 已检查 `/tmp/chatgpt-transfer-1440.png` 与 `/tmp/chatgpt-transfer-390.png`：弹窗未横向溢出，输入和操作入口正常可见。截图仅含假凭据，不纳入仓库。
- Impeccable 机械检查执行一次，报告整页对比度、小字号及设计令牌告警（退出码 2），不能记为通过。告警未精确定位到本次修改；本次未改全局配色、字号或设计体系，也不以截图检查宣称全页无障碍合规。
- 验证范围为前端假请求交互与上述 Go 包；未访问真实账户或上游，未执行全仓测试、race 或真实 OAuth 导入。

## 图标修正复验

用户截图指出入口将 `import_export` 渲染为文字。本地图标字体按名称裁剪，该图标未收录；改为提供商导入导出入口已使用的 `sync`，不新增字体资源。浏览器脚本新增字体加载后的图标名称与宽度断言，并保存入口截图 `/tmp/chatgpt-transfer-entry.png`，已人工检查图标正常。

修正后重新构建并检查：产物 760601 字节且与源码一致，`internal/webbuild`、`cmd/gateway` 测试通过；浏览器全部回归通过（新增「入口图标使用本地字体并正常渲染」），`git diff --check` 通过。此前弹窗截图验收未覆盖顶部入口，已用本项补齐。

## 确认与提交

用户已确认方案并授权实现及继续验证；任务已按实际验收结果勾选。尚未提交、合并、推送或归档，等待用户另行确认。
