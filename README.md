# ai-gateway

轻量本地 API Gateway，统一接入 Claude Code、Claude Desktop 和 Codex CLI，并在 Anthropic、OpenAI Chat、OpenAI Responses 三种客户端格式和不同上游 provider 之间路由、转换与转发。

**当前主版本**：Go 1.23。Node.js 最后版本已固化为 Git tag `v1.0.0-node`。

## 特性

- **多格式输入**：同时支持 Anthropic、OpenAI Chat、OpenAI Responses 三种格式
- **模型名映射**：将客户端发来的模型名映射到实际 provider 模型
- **多 Provider 路由**：不同模型走不同 provider，支持 Anthropic / OpenAI 兼容格式
- **视觉无缝衔接**：为纯文本模型配置视觉伴侣模型后，网关自动检测图片并先调用视觉模型生成描述
- **格式自动转换**：请求/响应在不同格式之间自动转换，流式响应完整支持
- **轻量部署**：静态 Go 二进制 + distroless 镜像，镜像和内存占用显著低于历史 Node 版

## 架构

```text
Claude Code / Desktop  ->  /v1/messages           (Anthropic)       ┐
OpenAI Chat 工具        ->  /v1/chat/completions   (OpenAI Chat)     ├-> 路由 -> Provider
Codex CLI              ->  /v1/responses          (OpenAI Responses) ┘
```

## 目录结构

```text
.
├── cmd/gateway/        # 主入口：HTTP 服务、请求处理、健康检查、配置 API
├── internal/config/    # YAML 配置加载、默认值与校验
├── internal/router/    # 模型路由匹配与 API Key 解析
├── internal/queue/     # per-provider 并发控制、限速和排队等待
├── internal/proxy/     # 流式/非流式转发与超时控制
├── internal/converter/ # Anthropic / OpenAI Chat / OpenAI Responses 互转
├── internal/vision/    # 图片检测、视觉模型翻译、SQLite 缓存和 singleflight 去重
├── internal/cache/     # 基于 modernc.org/sqlite 的纯 Go 图片缓存
├── Dockerfile          # Go 主版本镜像构建入口
├── docker-compose.yml  # Go 主版本部署入口
├── go.mod
└── config.example.yaml
```

## 安装与配置

```bash
git clone <repo>
cd ai-gateway
cp config.example.yaml config.yaml
```

编辑 `config.yaml`：

```yaml
port: 7789
host: "0.0.0.0"

providers:
  mimo:
    baseUrl: "https://your-provider.com"
    apiKey: "sk-xxx"
    format: anthropic

  mimo_vision:
    baseUrl: "https://your-vision-provider.com/v1"
    apiKey: "sk-xxx"
    format: openai

routes:
  - match: "claude-3-5-sonnet*"
    provider: mimo
    model: "your-main-model"
    vision:
      provider: mimo_vision
      model: "your-vision-model"

  - match: "*"
    provider: mimo
    model: "your-main-model"
```

## 本地开发

```bash
go run ./cmd/gateway
CGO_ENABLED=0 go build -o ai-gateway ./cmd/gateway
go vet ./...
go test ./...
```

当前仓库没有既有 `*_test.go` 测试套件。新增测试时放在被测包旁边，优先写 table-driven tests，重点覆盖 converter、router、config、queue、proxy 和流式 SSE 边界。

## Docker 部署

镜像通过 GitHub Actions 自动构建并发布到 GHCR，支持 `linux/amd64` 和 `linux/arm64`。

### Docker Compose

```bash
docker compose up -d
```

默认使用 `ghcr.io/wangruqing723/ai-gateway:latest`，并挂载：

- `./config.yaml:/app/config.yaml:ro`
- `./data:/app/data`

如需本地构建镜像，取消 `docker-compose.yml` 中 `build: .` 的注释。

### Docker 命令

```bash
docker build -t ai-gateway:go .
docker run -d \
  --name ai-gateway \
  -p 7789:7789 \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -v "$(pwd)/data:/app/data" \
  -e TZ=Asia/Shanghai \
  --memory=128m \
  ai-gateway:go
```

验证：

```bash
curl http://localhost:7789/health
docker compose logs -f
```

### SQLite 数据卷注意事项

Go 版用真正的 SQLite（`modernc.org/sqlite`），会用文件锁保证并发安全。只要 `data` 目录所在文件系统支持正常文件锁语义，bind mount 与 named volume 都可用。

要点：

- Docker 环境下 `host` 必须设为 `"0.0.0.0"`，否则端口映射不会生效。
- `data` 目录要放在容器运行时（Colima / Docker Desktop）共享给虚拟机的路径下，通常是用户家目录范围内。
- `data` 目录要对 distroless nonroot 用户（UID 65532）可写，最简单是 `chmod 777 data`。
- Docker named volume 通常最省心：`docker volume create aigw-data` 后挂载到 `/app/data`。

## 功能实现状态

| 能力 | 状态 | 说明 |
|---|---|---|
| 配置加载/校验 | 已实现 | 字段、默认值、区间与历史 Node 版一致 |
| 路由匹配 | 已实现 | `*`/`?` 通配，大小写不敏感 |
| 并发/限速队列 | 已实现 | channel 信号量 + 滑动窗口 + 队列等待超时 |
| 流式转发 + 活跃超时 | 已实现 | context 统一收口，超时补发合规 SSE 收尾 |
| 非流式转发 | 已实现 | 三格式互转后回写 |
| 格式互转 converter | 已实现 | 三格式请求/响应/流式 SSE 双向转换 |
| vision 图片识别 | 已实现 | 调用视觉模型 + SQLite 缓存复用 + 同图并发去重 |
| SQLite 图片缓存 | 已实现 | 纯 Go SQLite，`CGO_ENABLED=0` 可编译 |
| 健康检查 `/health` | 已实现 | 队列状态 + 缓存统计 + 内存 |

实测收益：镜像约 20MB 级别，启动后内存约个位数 MB；历史 Node 版镜像约 150MB 级别。

## 接入客户端

### Claude Code

`~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:7789",
    "ANTHROPIC_API_KEY": "any"
  },
  "model": "claude-3-5-sonnet-20241022"
}
```

### Claude Desktop

**Mac**：`~/Library/Application Support/Claude/claude_desktop_config.json`

```json
{
  "apiBaseUrl": "http://127.0.0.1:7789"
}
```

**Windows**：`%APPDATA%\Claude\claude_desktop_config.json`（同上）

### Codex CLI

```bash
export OPENAI_BASE_URL="http://127.0.0.1:7789"
export OPENAI_API_KEY="any"
codex
```

## Node.js 历史版本

Node.js 最后版本保留在 Git tag `v1.0.0-node`，镜像地址为 `ghcr.io/wangruqing723/ai-gateway:1.0.0-node`。主分支只维护 Go 版本。

## 常用操作

| 操作 | 命令 |
|---|---|
| 启动 | `docker compose up -d` |
| 停止 | `docker compose down` |
| 重启 | `docker compose restart` |
| 更新镜像 | `docker compose pull && docker compose up -d` |
| 查看日志 | `docker compose logs -f` |

开机自启动推荐使用 Docker Compose 的 `restart: unless-stopped`。

## 许可

MIT
