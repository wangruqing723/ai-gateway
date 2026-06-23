# ai-gateway

轻量本地 API Gateway，统一接入 Claude Code、Claude Desktop 和 Codex CLI。

**当前版本**：Go 1.23（主版本） | [Node.js 版本归档](docker-compose.node.yml)

## 特性

- **多格式输入**：同时支持 Anthropic、OpenAI Chat、OpenAI Responses 三种格式
- **模型名映射**：将客户端发来的模型名（如 `claude-3-5-sonnet`）映射到实际模型
- **多 Provider 路由**：不同模型走不同 provider，支持 Anthropic / OpenAI 兼容格式
- **视觉无缝衔接**：为纯文本模型配置视觉伴侣模型后，网关自动检测请求中的图片——纯文本走主模型，含图请求走视觉模型，客户端无需手动切换
- **格式自动转换**：请求/响应在不同格式之间自动转换，流式响应完整支持

## 架构

```
Claude Code / Desktop  →  /v1/messages          (Anthropic)      ─┐
OpenAI Chat 工具        →  /v1/chat/completions  (OpenAI Chat)    ─┼→ 路由 → Provider
Codex CLI              →  /v1/responses          (OpenAI Responses)┘
```

## 安装

### Go 版本（推荐）

```bash
git clone <repo>
cd ai-gateway
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入 provider 配置
```

### Node.js 版本（归档）

Node.js 版本已归档，镜像地址：`ghcr.io/wangruqing723/ai-gateway:latest`

启动方式：`docker compose -f docker-compose.node.yml up -d`

## 配置

编辑 `config.yaml`：

```yaml
port: 7789

providers:
  mimo:
    baseUrl: "https://token-plan-cn.xiaomimimo.com"
    apiKey: "sk-xxx"      # 可选，留空则从请求头自动获取
    format: anthropic     # anthropic | openai

  mimo_vision:
    baseUrl: "https://api.xiaomimimo.com"
    apiKey: "sk-xxx"
    format: openai

routes:
  - match: "claude-3-5-sonnet*"
    provider: mimo
    model: "mimo-v2.5-pro"
    vision:               # 可选：配置视觉伴侣模型
      provider: mimo_vision
      model: "mimo-v2.5"

  - match: "*"
    provider: mimo
    model: "mimo-v2.5-pro"
```

### 视觉模型无缝衔接

当主力模型不支持图片（或图片处理成本较高）时，可以通过 `vision` 配置指定一个视觉伴侣模型。网关会自动检测请求中是否包含图片：

- **纯文本请求** → 转发到主模型（如 `mimo-v2.5-pro`）
- **含图片请求** → 自动转发到视觉模型（如 `mimo-v2.5`），并将图片描述回填到请求中

整个过程对客户端完全透明，用户始终使用同一个模型名（如 `claude-3-5-sonnet`），无需手动切换。

适用场景：主力模型更便宜/更快但不支持图片，搭配一个支持图片的模型作为补充。

## 启动

### Go 版本

```bash
cd ai-gateway/go
go run ./cmd/gateway
```

或使用 Docker（见下方 Docker 部署章节）。

### Node.js 版本

```bash
npm install
node gateway.js
```

## Docker 部署

**当前主版本为 Go 实现**，镜像体积更小（~20MB vs 150MB）、内存占用更低（128MB vs 256MB）。

镜像已通过 GitHub Actions 自动构建并发布到 GHCR，支持 `linux/amd64` 和 `linux/arm64` 架构。

### 快速开始

```bash
# 创建工作目录
mkdir -p ~/ai-gateway/data
cd ~/ai-gateway

# 创建配置文件（参考 config.example.yaml）
# 注意：Docker 环境 host 必须设为 0.0.0.0
cat > config.yaml << 'EOF'
port: 7789
host: "0.0.0.0"

providers:
  mimo:
    baseUrl: "https://your-provider.com"
    apiKey: "sk-xxx"
    format: anthropic

routes:
  - match: "*"
    provider: mimo
    model: "your-model"
EOF
```

### 使用 Docker Compose（推荐）

在项目目录下直接运行（`docker-compose.yml` 已包含在仓库中）：

```bash
cd /path/to/ai-gateway
docker compose up -d
```

这会拉取 `ghcr.io/wangruqing723/ai-gateway:latest` 镜像（Go 版本，自动匹配 `amd64`/`arm64` 架构），并自动完成：
- 端口映射 `7789:7789`
- 配置文件只读挂载 `./config.yaml`
- 数据目录挂载 `./data`（持久化 SQLite 缓存）
- 内存限制 128MB

如需本地构建镜像，取消 `docker-compose.yml` 中 `build: .` 的注释。

#### 使用 Node.js 归档版本

如需使用 Node.js 版本：

```bash
docker compose -f docker-compose.node.yml up -d
```

### 使用 Docker 命令

**Go 版本（使用 GHCR 镜像，推荐）**：

```bash
docker run -d \
  --name ai-gateway \
  -p 7789:7789 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v $(pwd)/data:/app/data \
  -e TZ=Asia/Shanghai \
  --memory=128m \
  ghcr.io/wangruqing723/ai-gateway:latest
```

**Go 版本（本地构建）**：

```bash
docker build -t ai-gateway:go .
docker run -d \
  --name ai-gateway \
  -p 7789:7789 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v $(pwd)/data:/app/data \
  -e TZ=Asia/Shanghai \
  --memory=128m \
  ai-gateway:go
```

**Node.js 版本（归档）**：

```bash
docker build -t ai-gateway:node -f Dockerfile.node .
docker run -d \
  --name ai-gateway \
  -p 7789:7789 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v $(pwd)/data:/app/data \
  -e TZ=Asia/Shanghai \
  --memory=256m \
  ai-gateway:node
```

### 验证部署

```bash
# 查看容器状态
docker compose ps

# 检查健康状态
curl http://localhost:7789/health

# 查看日志
docker compose logs -f
```

### 常用操作

| 操作 | 命令 |
|---|---|
| 停止 | `docker compose down` |
| 重启（配置变更后） | `docker compose restart` |
| 更新到最新镜像 | `docker compose pull && docker compose up -d` |
| 查看日志 | `docker compose logs -f` |

### 注意事项

- **host 配置**：Docker 环境下 `host` 必须设为 `"0.0.0.0"`，否则端口映射不会生效
- **健康检查**：Go 版本使用 distroless 镜像（无 shell），需在编排层配置健康检查：
  ```yaml
  # Docker Compose
  healthcheck:
    test: ["CMD-SHELL", "wget --no-verbose --tries=1 --spider http://localhost:7789/health || exit 1"]
    interval: 30s
    timeout: 5s
    retries: 3
    start_period: 10s
  
  # Kubernetes
  livenessProbe:
    httpGet:
      path: /health
      port: 7789
    initialDelaySeconds: 10
    periodSeconds: 30
  ```
  注意：healthcheck 的 `test` 需在宿主机或包含 `wget`/`curl` 的 sidecar 容器中执行
- **配置变更**：修改 `config.yaml` 后需重启容器（`docker compose restart`）
- **数据持久化**：`./data` 目录挂载了 SQLite 缓存数据库，请勿删除
- **开机自启**：取消 `docker-compose.yml` 中 `restart: unless-stopped` 的注释即可

## 接入客户端

### Claude Code

`~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:7789",
    "ANTHROPIC_API_KEY":  "any"
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

## 开机自启动

### Mac

```bash
chmod +x scripts/install-mac.sh
./scripts/install-mac.sh install    # 安装
./scripts/install-mac.sh status     # 查看状态
./scripts/install-mac.sh uninstall  # 卸载
```

### Windows（管理员 PowerShell）

```powershell
.\scripts\install-win.ps1 install    # 安装
.\scripts\install-win.ps1 status     # 查看状态
.\scripts\install-win.ps1 uninstall  # 卸载
```

## 路由规则说明

- `match` 支持通配符 `*`（任意字符）和 `?`（单个字符）
- 按顺序匹配，第一条命中的生效
- `vision` 为可选，配置后网关自动检测图片并路由到视觉模型，无需客户端干预

## 许可

MIT
