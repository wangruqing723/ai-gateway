# ai-gateway

轻量本地 API Gateway，统一接入 Claude Code、Claude Desktop 和 Codex CLI，并在 Anthropic、OpenAI Chat、OpenAI Responses 三种客户端格式和不同上游 provider 之间路由、转换与转发。

**当前主版本**：Go 1.23。Node.js 最后版本已固化为 Git tag `v1.0.0-node`。

## 特性

- **多格式输入**：同时支持 Anthropic、OpenAI Chat、OpenAI Responses 三种格式
- **模型名映射**：将客户端发来的模型名映射到实际 provider 模型
- **多 Provider 路由**：不同模型走不同 provider，支持 Anthropic / OpenAI 兼容格式
- **故障转移**：一条路由可配多个候选 (provider, model)，前一个失败自动换下一个；只在尚未向客户端写出字节时转移
- **熔断保护**：连续失败的 provider 暂时摘掉，后续请求直接跳过它，冷却结束后用真实请求做半开探测
- **负载均衡**：多候选路由可选 `round-robin` 轮转或 `least-queue` 按在途量分流，并按可缓存前缀做会话粘性，避免上游侧 prompt cache 反复失效
- **视觉无缝衔接**：为纯文本模型配置视觉伴侣模型后，网关自动检测图片并先调用视觉模型生成描述
- **格式自动转换**：请求/响应在不同格式之间自动转换，流式响应完整支持
- **监控仪表盘**：实时请求速率、成功率、P95 延迟、Provider 队列状态、最近错误和系统资源
- **请求日志**：记录每次代理转发的完整链路（状态码、耗时、Provider、模型、错误），支持按状态/Provider/模型/流式/关键词筛选
- **Provider 健康检测**：手动触发对每个上游 Provider 的 `/v1/models` 探测，展示健康/异常/未检测状态
- **Web 配置管理**：内置深色管理页面，可查看、编辑、保存 Provider、路由、全局超时、缓存和直通模式配置
- **配置热重载**：管理页面保存配置后服务会热重载；开发模式下前端资源支持热加载
- **本地安全边界**：默认只发布到 `127.0.0.1`，并限制 method、Origin、Content-Type 与请求体大小
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
├── internal/breaker/   # per-provider 熔断状态机
├── internal/balancer/  # 候选选择策略与 prompt cache 会话粘性
├── internal/metrics/   # 内存环形请求日志与运行指标聚合
├── internal/providerhealth/ # 上游 Provider 健康探测
├── cmd/gateway/web/    # 嵌入式管理页面资源
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
host: "127.0.0.1"

providers:
  mimo:
    baseUrl: "https://your-provider.com"
    apiKey: "sk-xxx"
    format: anthropic
    # userAgent: "claude-cli/2.1.161 (external, cli)" # 可选：显式覆盖上游 User-Agent

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

### Provider 的 User-Agent

每个 Provider 可选配 `userAgent`。取值优先级固定为：`provider.userAgent` 非空时使用该值；否则原样转发客户端请求中的 `User-Agent`；客户端也未携带时完全不设置该头，由 Go 自动填入 `Go-http-client/1.1`。

这是一次有意的行为变更：过去未配置时，所有上游都会收到 `Go-http-client/1.1`；现在未配置的 Provider 会转发客户端的真实 UA。这样可兼容按客户端类型准入的上游（例如 ar-gh），但会向上游泄露客户端身份和版本。若要固定值或避免随客户端变化，请为该 Provider 显式配置 `userAgent`。

## 本地开发

```bash
go run ./cmd/gateway
CGO_ENABLED=0 go build -o ai-gateway ./cmd/gateway
go vet ./...
go test ./...
```

仓库测试覆盖 converter、queue、proxy、vision、config、metrics、Provider 健康检测和 `cmd/gateway` HTTP 边界。新增行为仍应优先补 table-driven 回归测试。

### Docker 内验证

本项目 Go 开发验证优先在 Docker 容器中执行。运行镜像是 distroless，不含 Go 工具链；格式化、测试、vet 使用 `golang:1.23-alpine` 临时容器，容器名固定为 `ai-gateway-dev-verify`：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -e GOPROXY=https://goproxy.cn,direct \
  -e GOMODCACHE=/gomodcache \
  -v "$PWD":/work \
  -v /tmp/ai-gateway-gomodcache:/gomodcache \
  -w /work \
  golang:1.23-alpine go test ./...

docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work \
  -w /work \
  golang:1.23-alpine gofmt -w ./cmd ./internal
```

## 管理页面

启动后访问：

```text
http://127.0.0.1:7789/
```

当前管理页面左侧菜单顺序为：监控仪表盘、提供商、路由、请求日志、全局设置。默认首页为监控仪表盘。监控仪表盘展示服务状态、Provider 队列、系统资源、请求速率、成功率、P95 延迟、Provider 延迟排行、最近错误和 Provider 健康状态；请求日志展示最近代理转发请求，并支持按状态、Provider、模型、流式类型和关键词筛选。配置编辑能力拆分到提供商、路由、全局设置三个菜单中。

配置菜单支持：

- 查看 Provider、路由数量、运行模式和超时概览
- 在“提供商”中可视化编辑 Provider、上游地址、协议格式、并发、限速、队列等待和 User-Agent
- 在“路由”中可视化编辑路由规则、目标模型和视觉伴随模型
- 在“全局设置”中可视化编辑监听、超时、缓存配置和直通模式
- 在“全局设置”中使用 YAML 原文编辑完整配置
- 右侧配置预览与复制；提供商、路由菜单展示对应片段，全局设置展示完整配置
- 保存配置并触发服务热重载

页面采用固定视口布局：左侧菜单保持视口高度，配置内容和预览区域在各自面板内滚动，避免配置项过多时拉长整个页面。

### 前端热加载

默认镜像构建使用 `go:embed` 嵌入 `cmd/gateway/web/index.html`。本地开发时可通过环境变量启用磁盘读取与 SSE 自动刷新：

```bash
AI_GATEWAY_WEB_DIR=cmd/gateway/web go run ./cmd/gateway
```

默认 Compose 不挂载前端源码。需要热加载时使用开发 override：

```bash
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

启用后，修改 `cmd/gateway/web/index.html` 并保存，浏览器页面会自动刷新。若热加载代码是新加的，旧页面需要先手动刷新一次以建立 `/__dev/reload` SSE 连接。

## Docker 部署

镜像通过 GitHub Actions 自动构建并发布到 GHCR，支持 `linux/amd64` 和 `linux/arm64`。

进入容器部署前，请先把 `config.yaml` 保持为 `host: "0.0.0.0"`、`port: 7789`。这只是容器内监听地址；下面的端口映射仍只发布到宿主机 `127.0.0.1`。

### Docker Compose

跨平台（macOS / Linux·WSL）推荐用启动脚本，它会以「当前宿主用户」的 UID/GID 运行容器，保证管理页面能写回 `config.yaml`：

```bash
./scripts/up.sh        # 或 make up
```

脚本做三件事：把本机 `id -u`/`id -g` 写入 `.env`（compose 自动读取）；首次运行从模板生成 `config.yaml`；在 Linux 上把 `config.yaml`、`data` 属主对齐到当前用户（首次可能需要 sudo）。

也可以直接用 compose：

```bash
docker compose up -d
```

当前 compose 默认本地构建 `ai-gateway:local`，并挂载：

- `./config.yaml:/app/config.yaml`
- `./data:/app/data`

`config.yaml` 需要可写权限，因为管理页面保存配置时会写回该文件并触发热重载。compose 默认用 macOS 用户的 UID/GID 运行容器：

```yaml
user: "${AI_GATEWAY_UID:-501}:${AI_GATEWAY_GID:-20}"
```

> **为什么 macOS 直接跑没问题、WSL/原生 Linux 会报 `permission denied`**：macOS Docker Desktop 的文件共享层会自动映射 bind mount 属主，任意容器 UID 都能写；原生 Linux/WSL 的 bind mount 严格穿透宿主属主，容器 UID（默认 501）与文件属主（常为 `root` 或你的用户）不一致时，只落到 others 只读权限，保存即失败。用 `./scripts/up.sh` 让容器 UID 与文件属主对齐即可解决。

若不用脚本、手动在其它机器运行，需自行对齐两侧：

```bash
export AI_GATEWAY_UID=$(id -u)
export AI_GATEWAY_GID=$(id -g)
sudo chown -R "$AI_GATEWAY_UID:$AI_GATEWAY_GID" config.yaml data   # 仅 Linux 需要
docker compose up -d --build --force-recreate
```

### Docker 命令

```bash
docker build -t ai-gateway:go .
mkdir -p data
docker run -d \
  --name ai-gateway \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:7789:7789 \
  -v "$(pwd)/config.yaml:/app/config.yaml" \
  -v "$(pwd)/data:/app/data" \
  -e TZ=Asia/Shanghai \
  --memory=128m \
  ai-gateway:go
```

验证：

```bash
curl http://localhost:7789/health
docker logs -f ai-gateway
```

### 本地安全边界

本项目按个人本机使用设计，不新增强制 gateway token。默认 `docker-compose.yml` 只把端口发布到 `127.0.0.1:7789`；应用同时校验推理和配置接口的 HTTP method、浏览器 Origin、`Content-Type` 与请求体大小。

不要直接把网关端口暴露到公网或不可信局域网。浏览器管理接口只接受 `localhost` 或 loopback IP 的同源请求，不支持通过远程域名反向代理后直接保存配置；跨机器使用时应通过 SSH 本地端口转发后仍以 `127.0.0.1` 访问。所有非静态请求还会校验 `Host`：只放行 loopback、配置中的 `host`；配置为 `0.0.0.0` 或 `::` 时放行 IP 字面量，以阻断 DNS rebinding。通过反向代理访问时请保留本机 Host，或把代理使用的域名写入 `host`。无 `Origin` 的非浏览器 API 客户端即使置于反向代理后，也仍需由代理提供认证、TLS、来源限制和访问日志；应用内这些本地防护不等价于公网鉴权。

配置保存带不透明 revision/ETag 冲突检测，已配置 API Key 只通过专用 sentinel 往返，不会作为展示文本写回文件。除 `host`、`port` 外，队列、direct mode、缓存策略和 Provider 健康状态会动态更新；`host` 或 `port` 变化会返回 `restartRequired`。

裸进程可在修改 `host`/`port` 后重启进程生效。Compose 部署必须保持容器内监听为 `0.0.0.0:7789`，因为默认端口映射固定为 `127.0.0.1:7789:7789`；若确需修改端口，要同步修改 Compose 的 container target，并执行 `docker compose up -d --force-recreate`，单独 `docker compose restart` 不会更新端口映射。

### SQLite 数据卷注意事项

Go 版用真正的 SQLite（`modernc.org/sqlite`），会用文件锁保证并发安全。只要 `data` 目录所在文件系统支持正常文件锁语义，bind mount 与 named volume 都可用。

要点：

- Docker 环境下 `host` 必须设为 `"0.0.0.0"`，否则端口映射不会生效。
- `data` 目录要放在容器运行时（Colima / Docker Desktop）共享给虚拟机的路径下，通常是用户家目录范围内。
- `config.yaml` 和 `data` 目录要对容器配置的 `AI_GATEWAY_UID`/`AI_GATEWAY_GID` 可写；独立 `docker run` 示例使用当前宿主 UID/GID，避免放宽为全员可写。
- 使用 Docker named volume 时仍需先用辅助容器把目录 ownership 初始化为实际运行 UID/GID；named volume 不会自动解决 nonroot 写权限。

## 功能实现状态

| 能力 | 状态 | 说明 |
|---|---|---|
| 配置加载/校验 | 已实现 | 严格字段检查、完整边界校验和安全默认值；部分规则有意严于历史 Node 版 |
| 路由匹配 | 已实现 | `*`/`?` 通配，大小写不敏感 |
| 并发/限速队列 | 已实现 | 动态 FIFO admission + 滑动窗口 + 完整等待超时 |
| 流式转发 + 活跃超时 | 已实现 | context 统一收口，超时补发合规 SSE 收尾 |
| 非流式转发 | 已实现 | 三格式互转后回写 |
| 格式互转 converter | 已实现 | 三格式请求/响应/流式 SSE 双向转换 |
| vision 图片识别 | 已实现 | 调用视觉模型 + SQLite 缓存复用 + 同图并发去重 |
| SQLite 图片缓存 | 已实现 | 纯 Go SQLite，`CGO_ENABLED=0` 可编译 |
| 健康检查 `/health` | 已实现 | 实际监听地址 + 队列状态 + Provider 状态 + 熔断状态 + 粘性映射数 + 缓存与内存 |

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
| 启动（推荐，跨平台对齐属主） | `./scripts/up.sh` 或 `make up` |
| 启动（原生 compose） | `docker compose up -d` |
| 停止 | `docker compose down` 或 `make down` |
| 重启 | `docker compose restart` |
| 重建本地镜像 | `docker compose up -d --build --force-recreate` |
| 查看日志 | `docker compose logs -f` 或 `make logs` |

开机自启动推荐使用 Docker Compose 的 `restart: unless-stopped`。

## 许可

MIT
