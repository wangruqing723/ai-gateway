> 实现方：Claude（文档模式）

# 错误来源标识与上游错误保留

状态：实现与验收完成；改动位于 dev 工作区，未提交、未部署。
创建日期：2026-11-12

## 目标

让用户在请求日志列表中立即区分本地网关错误、上游明确返回的错误、连接异常与客户端取消，并在尝试详情中查看上游错误证据。来源说明描述错误产生位置，不等同于责任归属。

复现依据：r00035 的 xingchi Responses 流在输出开始后发送 type=error、code=upstream_http2_stream_error、message=Upstream HTTP/2 stream failed；现有日志将其记录为 conversion_error，导致误判为本地协议转换故障。

## 任务

[x] T1 固化错误来源、分类与兼容契约 | P0 | 1h | 无
[x] T2 转换器保留上游显式错误的结构化证据 | P0 | 2h | T1
[x] T3 代理与入口按尝试传播来源并生成请求终态摘要 | P0 | 3h | T2
[x] T4 上游错误正文统一限量、脱敏与截断标识 | P0 | 2h | T2、T3
[x] T5 日志列表来源标签、详情原文与错误码展示 | P0 | 2h | T3、T4
[x] T6 覆盖错误矩阵与原有转移、熔断行为回归 | P0 | 3h | T2、T3、T4
[x] T7 前端产物重建、容器验证与浏览器验收 | P0 | 2h | T5、T6

## 验收标准

- r00035 的脱敏最小事件夹具显示「上游返回错误」，保留原错误码、原消息，不再归为本地转换失败。
- HTTP 错误、开场流内错误、中途流内错误和非流式错误均有明确来源或「来源未确定」。
- 上游错误证据按失败尝试记录；最终成功的请求不因之前候选失败而被标成失败。
- HTTP 200 之后的失败显著标为失败，并同时保留真实 HTTP 状态。
- 本地不支持的转换与上游无效响应分开；无法判断原因时不强行归责。
- 上游错误展示不执行 HTML，不泄露已识别的凭据，不无界缓存整个响应。
- 不改变重试、密钥冷却、熔断、超时及响应已提交后禁止故障转移的规则。
- Docker 内 go test ./...、go vet ./... 通过；共享状态有变更则运行竞态测试。
- 前端改动后重建 CSS 与 HTML，并确认 webbuild -check 通过。
- 使用本地模拟上游复现故障，不依赖收费或不稳定的真实上游；实际请求另行授权。

## 边界与待确认

详见 DESIGN.md。默认提案只增强现有请求日志和尝试详情，不新增全量会话录制，不变更客户端错误响应契约，不引入持久化日志数据库。

用户已确认：仅保留脱敏限量错误正文/失败事件，沿用内存日志保留期限，仅增强管理页面与容器日志。客户端错误响应契约保持不变。

## 实施与验收记录

- 前后端改动已整合至主工作区 dev，未提交、未部署。
- 主会话已独立运行全量 Go 测试、vet、webbuild -check，通过；随后补充本地来源测试并运行全仓 race。
- 主会话独立执行无依赖 Chrome CDP 验收，43 项通过，覆盖桌面与窄屏。已知现有移动布局侧栏过宽，本期不重设计。
- CSS 已由固定 Tailwind 3.4.17 构建，HTML 已容器重建。主会话 CSS 一致性复验曾被权限分类器拒绝；随后用户在 web 目录亲自执行同一固定版本构建与 cmp 比对，成功完成且无差异输出。Browserslist 数据过期提示不影响本次构建与比对。
- 首轮 race 发现大非 JSON 错误正文脱敏导致超时回归，已限制此类正文的观测处理；最终全仓 go test -race ./...、go vet ./...、webbuild -check 均通过（exit=0）。
- 不可靠的响应转换错误保持 unknown；不支持的请求构建记录 gateway_conversion_unsupported；非流式非法 JSON 记录 upstream_invalid_response。详情见 DESIGN.md。

## 验证命令

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -e GOPROXY=https://goproxy.cn,direct -v "$PWD":/work -w /work golang:1.27-alpine go test ./...
docker run --pull never --rm --name ai-gateway-dev-verify -e GOPROXY=https://goproxy.cn,direct -v "$PWD":/work -w /work golang:1.27-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -e GOPROXY=https://goproxy.cn,direct -v "$PWD":/work -w /work golang:1.27-alpine go run ./cmd/webbuild -check
```

## 本地 Dev 端到端验收（2026-11-13）

- 从当前工作区构建 ai-gateway:error-e2e，启动 ai-gateway-error-dev，地址 127.0.0.1:7790；模拟上游 aigw-error-upstream 仅在 Docker 网络 ai-gateway-error-e2e 内监听，无宿主端口。
- 使用独立假配置和假密钥，数据目录为容器内 tmpfs；未挂载生产配置、账户或数据，未请求真实付费上游。
- 通过真实 HTTP 验证 11 次请求：无路由、HTTP错误、凭据脱敏、大正文截断、开场流错误、中途HTTP200流错误、连接拒绝、响应头超时、候选转移成功、普通成功、客户端取消。
- r00006 复现原 HTTP/2 报错：HTTP200，errorSource=upstream，upstreamErrorCode=upstream_http2_stream_error，原消息保留，嵌套 api_key 脱敏。
- /api/metrics 累计11请求，2成功、9失败，HTTP200流错误和客户端取消均计失败。
- 独立 Chrome 访问真实 Dev 页面，来源、错误码、原消息、HTTP200失败标记、原文脱敏断言通过；截图 ~/.cache/aigw-error-e2e-20261112/live-ui.png。
- 测试材料位于 ~/.cache/aigw-error-e2e-20261112；Dev和模拟上游保留运行供查看，生产服务7789未重启且健康检查200。
- 启动时临时目录挂载和非root缓存目录权限问题均已解决：采用宿主缓存目录只读配置挂载和独立可写tmpfs。未修改业务代码。

## 状态列简化（用户确认，2026-11-13）

- 普通成功/HTTP错误只显示状态码；已确认响应开始后的流式失败显示「流中断」，非流式200失败显示「失败」，客户端取消显示灰色「已取消」。
- 详情明确显示 HTTP 状态原值，尝试明细保留数字状态码，不重复「失败 · HTTP」。
- 六类格式化断言、HTML重建及一致性、webbuild测试和桌面/窄屏CDP验收通过。
- 当前隔离Dev容器使用旧镜像嵌入页面，本轮未重建该容器；尝试复制至/app/web时因目录不存在而未成功，不能视为已更新运行页面。
