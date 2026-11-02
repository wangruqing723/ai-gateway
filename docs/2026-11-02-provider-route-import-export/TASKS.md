> 实现方：Claude（文档模式）

# 提供商与路由配置导入导出

- 创建日期：2026-11-02
- 状态：实现及常规验证已完成；用户明确授权先提交、推送并同步 `dev`／`master`，race 验证延后，不视为完整验收通过。
- 工作分支：`dev`。
- 设计：[DESIGN.md](DESIGN.md)
- 接口与文件格式：[API_SPEC.md](API_SPEC.md)
- 领域术语：[GLOSSARY.md](../../GLOSSARY.md)
- 可复用决策：[ADR-0001](../adr/0001-local-sensitive-transfer-boundary.md)

## 目标与范围

在提供商页、路由页增加共用的配置导入／导出弹窗，用于备份、恢复和跨实例迁移。

纳入范围：

- 提供商全部持久化字段，包括单值 API Key、全部列表密钥、代理密码、自定义请求头／请求体、启停状态及列表顺序。
- 路由全部持久化字段，包括单／多目标写法、候选顺序、策略、覆盖配置、路由视觉配置及启停状态。
- 版本化 YAML 迁移包，兼容从原生配置提取提供商与路由。
- 全部／选择导出，合并／整体替换导入，脱敏预览，确认后一次性保存并热重载。

明确不做：

- 网关全局设置、OAuth 账户令牌、日志、用量、缓存、密钥冷却、熔断等运行状态的迁移。
- 专用 CLI、无 Origin 的脚本调用、第三方格式、ZIP、多文档 YAML、加密包及含密钥的剪贴板导出。
- 自动测试上游、自动改名、拼接密钥列表、自动整理重复路由、完整 glob 遮挡分析。
- 新增登录／鉴权系统、通用导入导出框架，或顺带改造既有 OAuth 导入导出。

## 已确认的核心规则

| 主题 | 约定 |
|---|---|
| 配置来源 | 后端当前已保存且已加载的配置，不导出页面草稿，也不另读可能尚未重载的磁盘文件。页面有未保存修改时要求先保存或取消。 |
| 文件 | 带 `kind`／`version` 的 YAML，仅载入提供商与路由；明文下载前逐次提示敏感风险。 |
| 导出依赖 | 选择路由自动携带所有候选和路由视觉依赖提供商；选择提供商不反向携带路由，不裁剪候选。 |
| 导出校验 | 所选路由存在忽略大小写重复的 `match` 时阻止下载；仅导出提供商或不重复的路由仍允许。 |
| 凭据 | 单值、列表中启用／停用／冷却密钥的真实值及代理密码全部保留；空单值密钥不从外部补齐。普通查询与预览继续脱敏。 |
| 合并 | 默认全部选中，可取消部分条目；提供商名精确匹配、路由 `match` 按现有匹配语义忽略大小写比较。冲突默认保留，可逐项整体覆盖。 |
| 顺序 | 已有路由顺序不变，覆盖项保留原位置；新增路由按源文件顺序整批插到前／后，默认后。明显被已有 `*` 遮挡时提示，不自动重排。 |
| 整体替换 | 必须导入完整、非空的提供商和路由集合，不允许局部勾选；清楚展示删除范围，不影响全局配置。 |
| 文件校验 | 所有条目的结构、数值及凭据占位符均校验，不能通过取消勾选绕过；目标引用和 OAuth 绑定只裁决实际新增／覆盖条目及最终配置。 |
| 重复路由 | 源文件忽略大小写重复 `match` 一律拒绝；目标中不涉及的遗留重复保留，涉及重复模式的合并不得猜测覆盖对象。 |
| OAuth | 不携带账户令牌；相同源引用共用目标账户映射，相同本地 ID 可匹配，否则手动选择已有账户，不按邮箱猜测、不新建账户。 |
| 预览 | 只展示名称、匹配模式、依赖、顺序、动作、变化字段名、密钥数量和受影响路由；不返回配置原值或凭据指纹。 |
| 应用 | 必须凭有效预览凭证一次性保存／热重载；配置版本变化后拒绝，不能自动合并到新版本。 |
| 限制 | 迁移文件和最终完整配置均不超过 1 MiB；预览有效期 10 分钟，改内容或选项须重新预览，服务重启使凭证失效。 |
| 敏感留存 | 当次内存处理，关闭弹窗清空；无导入历史、额外明文临时文件或自动备份。覆盖／替换前提示先手动导出。 |
| 新接口 | 三个显式 POST，仅接受非空、loopback 同源 Origin；原有配置和客户端转发端点保持原行为。 |

## 任务清单

以下估时只用于拆分工作量，不是交付时间承诺。每项仅在完成并验证后勾选。

[x] T01 专用迁移 Module 与严格文件解码：版本化封装、原生配置提取、全量条目形状校验及安全错误 | 优先级(P0) | 估时 2–3 小时 | 依赖 无

[x] T02 导出：配置快照、全部／选择、路由依赖闭包、真实凭据、顺序保留、重复与体积校验 | 优先级(P0) | 估时 1–2 小时 | 依赖 T01

[x] T03 合并／整体替换规划：选择、逐项冲突、原位覆盖、批量插入、依赖与删除影响、遗留重复规则 | 优先级(P0) | 估时 3–4 小时 | 依赖 T01

[x] T04 OAuth 账户绑定映射及脱敏变更预览：实际导入项校验、字段名差异、密钥数量、受影响路由和遮挡提示 | 优先级(P0) | 估时 1–2 小时 | 依赖 T03

[x] T05 敏感接口公共防护与预览凭证：严格 Origin／媒体类型／版本前置条件、进程级签名、10 分钟有效期及内容绑定 | 优先级(P0) | 估时 1–2 小时 | 依赖 T01、T03

[x] T06 三个 HTTP 端点及保存事务：显式导出、只读预览、凭证复验、串行保存／热重载、失败不部分应用 | 优先级(P0) | 估时 2–3 小时 | 依赖 T02、T04、T05

[x] T07 共用管理页弹窗：两页入口、选择导出、文件／粘贴、冲突与映射、预览确认、草稿拦截、取消和敏感内存清理 | 优先级(P1) | 估时 3–4 小时 | 依赖 T06

[x] T08 Module 单元测试：文件兼容、指针零值、凭据、顺序、依赖、选择、冲突、重复模式、替换和体积边界 | 优先级(P0) | 估时 2–3 小时 | 依赖 T02、T03、T04

[x] T09 HTTP／事务测试：Origin 与脱敏回归、凭证篡改／过期／重启／并发、保存失败、全局保留及热重载传播 | 优先级(P0) | 估时 2–3 小时 | 依赖 T06

[x] T10 Playwright 交互与本地假配置 smoke：基线、下载内容、导入选择／冲突／映射、草稿／过期／取消、零上游请求 | 优先级(P1) | 估时 1–2 小时 | 依赖 T07

[ ] T11 重建并核验前端产物，容器内格式／全量测试／vet／race／静态构建，记录实跑命令和输出 | 优先级(P0) | 估时 1–2 小时 | 依赖 T07、T08、T09、T10

[x] T12 更新使用与安全说明，汇总验收结果、未验证项及待提交文件；用户已确认先提交推送，race 延后 | 优先级(P1) | 估时 0.5 小时 | 依赖 T11（race 经用户授权延后）

## 预期实现落点

- `internal/config/transfer.go`、`transfer_shape.go`、`transfer_test.go`：专用迁移 Module，类型／别名图检查与规划分开，复用本包默认值和校验，返回结果，不执行磁盘或网络动作。
- `internal/config/config.go`：仅按需最小拆分私有形状／引用校验，普通配置语义不变。
- `cmd/gateway/config_transfer.go`、`config_transfer_test.go`、`config_transfer_smoke_test.go`：HTTP Adapter、签名凭证、保存事务及临时假配置的真实 HTTP smoke。
- `cmd/gateway/main.go`、`helpers.go`：分派新路径、组装进程内凭证依赖，复用现有锁和防护；不堆入迁移规则。
- `cmd/gateway/web/src/app/13-config-transfer.js.part`、`00-state.js.part`、`index.template.html`：共用交互与临时状态。
- `cmd/gateway/web/index.html`、`web/vendor/tailwind.css`：源码变更后重建产物。
- `scripts/verify-provider-route-transfer.cjs`：可复跑的交互验证脚本，只用假数据，不读取真实本地配置。
- `README.md`：明确迁移范围、含密钥文件保管、OAuth 前置迁移及本机访问要求。

实施时如果发现确需改变设计，先更新本目录设计与任务说明；涉及未授权的产品取舍，先向使用者澄清。不得用实现中的猜测替代已经确认的规则。

## 验收闸门

1. 导出产物可被本功能严格解析；纯提供商片段允许合并，不允许整体替换。
2. 全部导入和选择导入均保留明确约定的顺序、凭据、启停、指针零值及全局配置。
3. 前端预览、JSON 响应、错误、日志中无真实凭据或自定义配置原值；只有明确导出响应包含明文。
4. 未取得有效预览凭证、凭证内容不符、过期、重启、配置修订冲突或绑定未解决时不能保存。
5. 失败不得部分合并运行时配置，普通保存／重载、OAuth 功能和客户端无 Origin 转发行为无回归。
6. `webbuild -check` 必须真实 exit=0；样式表单独重建并比对；浏览器交互必须实跑。
7. 容器内全量测试、vet、race 和静态构建均提供真实结果。环境阻断的项目明确标为未验证，不将其他工具的自述作为验收证据。

## 执行与提交约束

- 用户已于 2026-11-02 确认文档并要求开始实现；按下列任务继续，完成后仍需另行确认提交。
- Go 验证优先 Docker；实施前核对本地镜像，不因宿主缺少 Go 判定无法验证，也不默默降级项目工具链。
- 工具被拒或环境需介入时暂停说明具体命令、原因和影响，不无限重试。
- 不读取或修改真实 `config.yaml`、`.env`、OAuth 凭据库或其他本地敏感文件来制作测试。
- 2026-11-02：用户明确要求先提交推送，并同步 `dev` 与主分支 `master`；授权本轮提交／合并／推送，race 等后续验证，不自动部署或归档。

## 验证记录

- 2026-11-02：仅落地规划与领域文档；业务实现、Go 测试、浏览器验证尚未开始。
- 2026-11-02：完成 T01–T10。主会话独立复验，不采用代理自报测试作为验收依据。
- 实施文件拆分：严格类型／别名图检查放在 `transfer_shape.go`；真实 HTTP smoke 放在 `config_transfer_smoke_test.go`；产品规则和接口数量不变。

### 已执行验证

| 验证 | 实跑命令 | 结果 |
|---|---|---|
| 工具链 | `docker exec ai-gateway-dev-verify go version` | `go version go1.27.3 linux/amd64`。 |
| 迁移规则 | `docker exec ai-gateway-dev-verify go test ./internal/config -run TestTransfer -count=1 -v` | 通过，包括严格类型／锚点／循环、全局保留、凭据／顺序、重复／选择与体积。 |
| HTTP 与事务 | `docker exec ai-gateway-dev-verify go test ./cmd/gateway -run TestConfigTransfer -count=1 -v` | 通过，包括 Origin、精确 ETag、凭证／并发、只读预览、保存失败及真实 HTTP smoke。 |
| 真实本机 smoke | `docker exec ai-gateway-dev-verify go test ./cmd/gateway -run TestConfigTransferSmokeLocalHTTP -count=1 -v`（已随上述命令执行） | 通过：临时假配置、隔离 HTTP 端口、本地假上游，确认零上游请求与真实落盘。 |
| 全量测试 | `docker exec ai-gateway-dev-verify go test ./...` | 通过；无测试文件的包由 Go 明确标记，不把它们算作新增覆盖。 |
| 静态检查 | `docker exec ai-gateway-dev-verify go vet ./...` | exit=0，无诊断输出。 |
| 静态构建 | `docker exec ai-gateway-dev-verify env CGO_ENABLED=0 go build -o /tmp/ai-gateway-transfer-build ./cmd/gateway` | exit=0，产物只在验证容器临时目录，不覆盖服务。 |
| 本次 Go 格式 | `docker exec ai-gateway-dev-verify gofmt -l internal/config/config.go internal/config/transfer.go internal/config/transfer_shape.go internal/config/transfer_test.go cmd/gateway/main.go cmd/gateway/config_transfer.go cmd/gateway/config_transfer_test.go cmd/gateway/config_transfer_smoke_test.go` | 无输出。本次 Go 文件格式正确；全树扫描发现既有 `internal/breaker/breaker.go` 有格式差异，未改动无关文件。 |
| HTML | `docker exec ai-gateway-dev-verify go run ./cmd/webbuild`；`docker exec ai-gateway-dev-verify go run ./cmd/webbuild -check` | 正式构建器重建，check exit=0。 |
| 浏览器基线 | `env -u http_proxy -u https_proxy -u all_proxy -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY node scripts/verify-provider-route-transfer.cjs --baseline` | 旧产物两个入口均为 0，确认功能前基线。 |
| 浏览器正式产物 | 同上，去掉 `--baseline`；最终文案与预览竞态修正后再次重跑 | 21 组断言通过；包含双入口、三类草稿拦截、选择／依赖、冲突、顺序编号、OAuth、缺依赖、过期、关闭清理与替换确认。 |
| 脚本语法／差异 | `node --check scripts/verify-provider-route-transfer.cjs`；`git diff --check` | 通过，无输出。 |
| race | `docker pull golang:1.27` | 拉取长时间未完成，已通过 TaskStop 停止该命令。race 未执行、未验证；用户已确认本轮先提交推送，后续再验证。 |

### 样式验证的失败与修正

1. 从仓库根执行 Tailwind，出现「未检测到 utility classes」，diff=1；该命令的工作目录有误，结果不作为有效验收。
2. 正确目录访问 npm registry 超时，构建未完成。
3. 离线缓存挂载为 `/opt/tailwind` 后 Node 报 `Cannot find module 'arg'`；只读核对确认依赖存在，但路径丢掉了 `node_modules` 层。
4. 挂载保留 `node_modules` 后，固定 Tailwind 3.4.17 离线重建及二次比对 exit=0。实跑命令如下；未联网、不自动删除容器：

```bash
docker run --pull never --network none --name "ai-gateway-dev-verify-webcss-local-$(date +%s)" \
  -v /opt/project/ai-gateway:/work \
  -v /root/.npm/_npx/a040215083095e7d/node_modules:/opt/tailwind/node_modules:ro \
  -w /work/cmd/gateway/web node:24-alpine \
  sh -c 'node /opt/tailwind/node_modules/tailwindcss/lib/cli.js -c tailwind.config.js -i src/tailwind.css -o vendor/tailwind.css -m && node /opt/tailwind/node_modules/tailwindcss/lib/cli.js -c tailwind.config.js -i src/tailwind.css -o /tmp/tailwind-rebuilt.css -m && diff -q /tmp/tailwind-rebuilt.css vendor/tailwind.css'
```

构建仅有既有 Browserslist 缓存版本提示，没有 utility 缺失警告；未顺带更新依赖。

### 后续验证与本轮授权

- T11 的常规测试、vet、静态构建、HTML／CSS 和本次文件格式均已通过；race 拉取已停止、未执行。用户明确要求后续再验证，T11 保持未勾选，不将延后视为通过。
- T12 的使用说明、验证汇总及提交确认已完成；按用户本轮授权在 `dev` 提交，通过普通分支合并同步 `master` 并推送两者，不执行强推、自动部署或归档。
