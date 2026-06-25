# CLAUDE.md

## 项目概述

`ai-gateway` 是轻量本地 API 网关，统一接入 Claude Code、Claude Desktop、Codex CLI，并在 Anthropic、OpenAI Chat、OpenAI Responses 三种客户端格式和不同上游 provider 之间路由、转换与转发。

仓库当前以 Go 实现为主：

- **Go 版（当前主版本）**：根目录是 Go module（Go 1.23），所有新功能与修复优先在这里完成。
- **Node.js 版（历史参考）**：最后版本已固化为 Git tag `v1.0.0-node`。Go 内部包刻意与历史 Node 对应模块逐函数对齐，修改 converter、router、queue、proxy、vision、cache、config 时可通过该 tag 参考行为等价性。

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
- `cmd/gateway/web/`：嵌入式前端资源，路径需匹配 `//go:embed web/index.html`。
- `TODO-review.md`：Go 版问题修复跟踪。
- `config.example.yaml`：配置模板；本地复制为 `config.yaml`。

## Build, Test, and Development Commands

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

配置字段使用 lower camelCase，并同步更新 `config.example.yaml`、`internal/config.applyDefaults` 与 `validate`。涉及格式转换时，优先保持与 tag `v1.0.0-node` 中对应模块的行为对齐。

## Testing Guidelines

当前仓库没有既有 `*_test.go` 测试套件。新增测试时放在被测包旁边，文件名使用 `*_test.go`，优先写 table-driven tests。重点覆盖 converter 跨格式转换、流式 SSE、router 通配匹配、config 校验、queue 限速/并发和 proxy 超时边界。

运行全部测试：

```bash
go test ./...
```

运行单个测试：

```bash
go test ./internal/<pkg>/ -run TestName -v
```

流式或真实 provider 行为变更应补充手工 smoke test，例如 `./test-stream.sh`，并在 PR 中说明所需本地配置。

## Request Flow & Runtime Conventions

`cmd/gateway/main.go` 的 `server.handle()` 按顺序处理：

1. 特殊路由：`HEAD`、`GET /`、`/api/config*`、`GET /health`、`GET /v1/models`。
2. `converter.DetectClientFormat(urlPath)` 按路径识别客户端格式。
3. `FromAnthropic`、`FromOpenAIChat`、`FromOpenAIResponses` 规范化为 `*converter.Internal`。
4. `router.MatchRoute(model, cfg)` 按 `routes` 顺序匹配，支持 `*`/`?` 且大小写不敏感。
5. `router.ResolveAPIKey` 优先使用 provider 配置，其次从 `x-api-key` 或 `Authorization: Bearer` 读取。
6. 若配置 `vision` 且消息含图片，`vision.Translator.Translate` 先调用视觉模型替换图片块为文本描述。
7. 按 provider `format` 转为 Anthropic 或 OpenAI Chat 上游请求。
8. 非直通模式通过 `queue.Manager.Acquire` 获取执行槽并 `defer release()`。
9. `proxy.Forward` 转发请求，并把响应转换回客户端格式。

## Key Mechanisms

- **直通模式**：`config.DirectMode=true` 时跳过队列，限流交给上游 429 透传，仅保留 `directTimeout*` 三档超时。
- **队列模式**：默认模式，使用 per-provider 并发信号量、滑动窗口限速和 `maxQueueWait`。
- **流式转发**：同格式直接透传；跨格式使用 `converter.NewStreamTransformer` 逐行转换，避免 `bufio.Scanner` 的大行限制。
- **超时控制**：`internal/proxy` 统一用 `context` 收口；流式超时后会补发合规 SSE 收尾事件。
- **配置热重载**：`/api/config` 支持脱敏读取、校验落盘、从磁盘 reload；`server.cfg` 由 `cfgMu` 保护。

## Commit & Pull Request Guidelines

提交信息遵循 Conventional Commit 风格，例如 `feat: ...`、`fix(docker): ...`、`fix(ci): ...`。PR 应包含行为摘要、测试结果、配置或迁移说明，以及关联 issue。涉及 Docker、运行时行为或前端界面时，附必要日志或截图。

## Security & Configuration Tips

不要提交真实 API Key、token 或本地 `config.yaml`。`config.Load` 查找顺序是当前工作目录 `config.yaml`，然后 `~/.config/ai-gateway/config.yaml`。Docker 环境需设置 `host: "0.0.0.0"`，并确保挂载的 `data/` 对 distroless nonroot 用户（UID 65532）可写。

`/api/config` 的 `PUT` 与 `reload` 可改写磁盘配置并热重载，当前无内置鉴权；当监听地址不止 `127.0.0.1` 时，必须在部署层增加访问控制。
