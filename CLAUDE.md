# CLAUDE.md

This file provides guidance to AI coding assistants (Claude Code, Codex, and others) when working with code in this repository.

> 注：`AGENTS.md` 指向本文件，所有指南以 CLAUDE.md 为唯一信息源。更新指南请只编辑本文件。

---

## 项目概述

`ai-gateway` 是轻量本地 API 网关，统一接入 Claude Code、Claude Desktop、Codex CLI，并在 Anthropic、OpenAI Chat、OpenAI Responses 三种客户端格式和不同上游 provider 之间路由、转换与转发。

仓库当前以 Go 实现为主：

- **Go 版（当前主版本）**：根目录是 Go module（Go 1.23），所有新功能与修复优先在这里完成。
- **Node.js 版（历史参考）**：最后版本已固化为 Git tag `v1.0.0-node`。修改 converter、router、proxy、vision、cache 时可用该 tag 核对兼容语义；Go 版的严格配置校验、动态 FIFO、三协议转换和运行时 hardening 属于有意演进，不要求逐函数照搬历史实现。

Go 源码注释、日志、README 主要使用中文；新增说明请保持这一约定。

## Project Structure & Module Organization

- `cmd/gateway/`：主入口、HTTP 服务、健康检查、配置 API、启动辅助逻辑。
- `internal/config/`：YAML 配置加载、默认值与校验。
- `internal/router/`：模型路由匹配与 API Key 解析。
- `internal/queue/`：per-provider 并发控制、限速和排队等待。
- `internal/proxy/`：流式/非流式转发与超时控制。
- `internal/converter/`：Anthropic、OpenAI Chat、OpenAI Responses 与内部格式互转。
- `internal/vision/`：图片检测、视觉模型翻译、SQLite 缓存和 singleflight 去重。
- `internal/cache/`：基于 `modernc.org/sqlite` 的纯 Go 图片缓存。
- `internal/metrics/`：内存环形请求日志与运行指标聚合，支持 P95、成功率、Provider 排行。
- `internal/providerhealth/`：上游 Provider 健康检测，探测 `/v1/models` 端点。
- `internal/breaker/`：per-provider 熔断器，自维护连续失败计数与 closed/open/half_open 状态机，供候选循环做准入过滤。
- `internal/balancer/`：候选尝试顺序（`failover` / `round-robin` / `least-queue`）与 prompt cache 会话粘性。只认下标和身份字符串，不依赖 `router`、`config`，避免成环。
- `cmd/gateway/web/`：嵌入式前端资源与固定版本 vendor 脚本，路径需匹配 `//go:embed web/index.html web/vendor/*`。
- 前端开发热加载：本地 compose 挂载 `./cmd/gateway/web:/app/web:ro` 并设置 `AI_GATEWAY_WEB_DIR=/app/web`；该变量存在时服务从磁盘读取 `index.html` 并通过 SSE 自动刷新浏览器，生产环境不设置时使用 `go:embed`。
- `TODO-review.md`：Go 版问题修复跟踪。
- `config.example.yaml`：配置模板；本地复制为 `config.yaml`。

## Build, Test, and Development Commands

本项目 Go 开发验证必须优先在 Docker 容器内执行；不要因为宿主机缺少 `go`/`gofmt` 就判定无法验证。验证容器名固定使用 `ai-gateway-dev-verify`，避免与正式服务容器混淆。运行服务镜像是 distroless，不含 Go 工具链；格式化、测试和 vet 应使用 `golang:1.23-alpine` 临时容器运行，例如：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -w ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
```

```bash
go run ./cmd/gateway
CGO_ENABLED=0 go build -o ai-gateway ./cmd/gateway
go vet ./...
gofmt -w ./cmd ./internal
go test ./...
docker build -t ai-gateway:go .
docker compose up -d
```

Go 静态构建必须设置 `CGO_ENABLED=0`，以保持 distroless 镜像和纯 Go SQLite 驱动兼容。

## Coding Style & Naming Conventions

Go 代码使用 `gofmt` 默认格式和 idiomatic Go 命名。包名保持短小，导出标识符只在跨包需要时使用，错误信息要带足上下文。共享逻辑放在 `internal/*` 对应包内，`cmd/gateway` 只放启动、路由入口和服务组装逻辑。

配置字段使用 lower camelCase，并同步更新 `config.example.yaml`、`internal/config.applyDefaults` 与 `validate`。涉及格式转换时应保留已记录的客户端兼容语义；安全边界、错误终态和新增协议能力以当前 Go 测试与文档为准。

## Testing Guidelines

仓库已有覆盖 converter、queue、proxy、vision、config、metrics、Provider 健康检测和 `cmd/gateway` HTTP 边界的 `*_test.go` 套件。新增测试放在被测包旁边，优先使用 table-driven tests，并为并发改动运行 race detector。

运行全部测试：

```bash
go test ./...
```

运行单个测试：

```bash
go test ./internal/<pkg>/ -run TestName -v
```

流式或真实 provider 行为变更应补充明确记录命令的本地 smoke test，并在 PR 中说明所需假配置或本地配置；仓库当前没有固定的 `test-stream.sh`。

## Request Flow & Runtime Conventions

`cmd/gateway/main.go` 的 `server.handle()` 按顺序处理：

1. 特殊路由：`HEAD`、`GET /`、`/api/config*`、`GET /health`、`GET /v1/models`。
- 观测 API：`GET /api/metrics`（聚合指标）、`GET /api/logs`（请求日志，支持筛选）、`POST /api/providers/health`（触发 Provider 健康检测）、`POST /api/providers/breaker/reset`（手动闭合熔断器，可带 `?provider=`）。
2. HTTP 边界校验 method、Origin、`Content-Type` 和请求体大小；无 Origin 的本机 CLI 请求保持兼容。
3. `converter.DetectClientFormat(urlPath)` 按路径识别客户端格式。
4. `FromAnthropic`、`FromOpenAIChat`、`FromOpenAIResponses` 规范化为 `*converter.Internal`，转换错误必须在路由前返回。
5. `router.MatchRoute(model, cfg)` 按 `routes` 顺序匹配，支持 `*`/`?` 且大小写不敏感；返回 `Match.Candidates`（至少 1 个，按配置顺序）与 `Match.Strategy`，每个候选是 (Provider 值拷贝, TargetModel) 对。router 保持无状态，不做重排。
6. `balancer.StickyKey(internal.System, internal.Messages)` 取粘性键——必须在 vision 翻译之前取，翻译会把图片块换成随缓存命中情况变化的文字描述，掺进哈希会让同一会话算出两个键。随后 `server.candidateOrder` 调 `balancer.Selector.Select` 得到尝试顺序下标序列，长度恒等于候选数、不丢候选。
7. 若配置 `vision` 且消息含图片，`vision.Translator.Translate` 先调用视觉模型替换图片块为文本描述。视觉翻译在候选循环外，产出的是文字描述，与目标无关。
8. 按第 6 步的顺序遍历候选，真实尝试次数受 `failover.maxAttempts` 约束，每个候选走一次 `forwardAttempt`：
   1. 熔断准入 `breaker.Allow(name)`，被跳过的候选不消耗尝试额度。
   2. `router.ResolveAPIKey` 优先使用 provider 配置，其次从 `x-api-key` 或 `Authorization: Bearer` 读取。
   3. 按 provider `format` 通过 checked API 转为 Anthropic 或 OpenAI Chat 上游请求；该候选构建失败则跳过，全部失败才返回错误。
   4. 非直通模式通过 `queue.Manager.Acquire` 获取执行槽，`release()` 必须 `defer` 在单次尝试函数内，否则循环里累积不释放。
   5. `proxy.Forward` 转发请求，并把响应转换回客户端格式；`ShouldRetry` 判定可转移时返回 `ErrAttemptAbandoned` 且不写客户端。
   6. `breaker.Report` 上报本次结果，判据取 `OnUpstreamStatus` 回调拿到的真实上游状态码。
8. 成功或额度耗尽后写响应头 `x-ai-gateway-provider` / `x-ai-gateway-attempts`，并记一条请求日志（`Attempts`、`AttemptTrail`）。

## Key Mechanisms

- **直通模式**：`config.DirectMode=true` 时跳过队列，限流交给上游 429 透传，仅保留 `directTimeout*` 三档超时。
- **队列模式**：默认模式，使用 per-provider 动态 FIFO admission、滑动窗口限速和覆盖完整等待阶段的 `maxQueueWait`。
- **负载均衡**：路由级 `strategy` 字段，与 `failover.enabled` 正交——策略决定「先试谁」，failover 决定「失败了还能试谁」，`round-robin` + failover 关闭是合法组合（纯分流、不转移）。`round-robin` 按 per-route 计数器轮转起点；`least-queue` 按 `queue.StatusOf` 的 `running+queued` 升序，相同负载按轮转顺序打散。directMode 无队列，负载恒为 0，`least-queue` 因此自然退化成 `round-robin`，而不是静默退回配置顺序。单候选路由写非 `failover` 策略在启动校验时直接报错，不静默忽略。
- **会话粘性**：`round-robin` / `least-queue` 下对可缓存前缀（system + 首条 user 消息文本）做 SHA-256 当近似会话身份，命中则把该目标提到队首，保住上游侧 prompt cache 前缀。TTL 5 分钟、LRU 上限 1000 条、前缀短于 256 字符不参与。只在候选**成功后**才 `Remember`，绝不在选中时绑定——绑定失败过的目标会把整条会话钉在坏上游上。映射值存 `provider/model` 而不是下标，热重载改 `targets` 顺序后下标会指向另一个上游。
- **流式转发**：同格式直接透传；跨格式使用 `converter.NewStreamTransformer` 逐行转换，避免 `bufio.Scanner` 的大行限制。
- **超时控制**：`internal/proxy` 统一用 `context` 收口；流式超时后会补发合规 SSE 收尾事件。
- **配置热重载**：`/api/config` 使用严格 YAML 解码、结构化脱敏、revision/ETag 与串行事务；`host`、`port` 只报告 `restartRequired`，其他运行时组件统一动态传播。`applyRuntimeConfig` 内 `queue`、`breaker`、`selector` 三者都要 `Reconcile`：selector 按 `route.match` 归属，删掉或改名的路由要连带丢掉轮转计数器与粘性映射（`rr` 既无 TTL 也无 LRU 兜底，不清理就随历史 match 单调增长）；`match` 未变时必须保留状态，否则每次保存配置都会冲掉全部会话粘性、让上游侧 prompt cache 作废。
- **请求观测**：最近 1000 条请求日志使用环形缓冲；累计与最近一分钟指标使用独立有界秒桶和固定延迟直方图，不受日志容量限制。
- **Provider 健康检测**：`internal/providerhealth` 通过探测 `/v1/models` 判断上游是否可用；手动触发 `POST /api/providers/health` 后结果缓存在内存中，`/health` 响应携带缓存状态。

## Commit & Pull Request Guidelines

提交信息遵循 Conventional Commit 风格，例如 `feat: ...`、`fix(docker): ...`、`fix(ci): ...`。PR 应包含行为摘要、测试结果、配置或迁移说明，以及关联 issue。涉及 Docker、运行时行为或前端界面时，附必要日志或截图。

## Security & Configuration Tips

不要提交真实 API Key、token 或本地 `config.yaml`。`config.Load` 查找顺序是当前工作目录 `config.yaml`，然后 `~/.config/ai-gateway/config.yaml`。Docker 环境需保持容器内 `host: "0.0.0.0"`、`port: 7789` 与默认 Compose target 一致，并确保 `config.yaml`、`data/` 对 Compose 配置的运行 UID/GID 可写。

`/api/config` 的 `PUT` 与 `reload` 可改写磁盘配置并热重载，当前无强制 gateway token；默认 Compose 仅发布到 `127.0.0.1`。浏览器管理接口只允许 `localhost`/loopback Origin，不支持远程域名反向代理后直接操作；跨机器管理应使用 SSH 本地端口转发。无 Origin 的 API 客户端仍需由前置代理提供认证、TLS 和访问控制，应用内 method/Origin/media-type/body-limit 防护不等价于公网鉴权。
