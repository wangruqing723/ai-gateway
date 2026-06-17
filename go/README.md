# ai-gateway — Go PoC

这是 Node 版 `ai-gateway` 迁移到 Go 的概念验证（Proof of Concept），用于评估"性能/稳定性/体积"迁移收益。
已实现与 Node 版功能对等的全部核心能力（converter / vision / cache 均已补齐并经 Docker 实测）。
**仍建议在生产切流前用真实流量对 Node 版做响应对拍**，确认 converter 完全等价。

## 设计目标

聚焦评估时最关心的三件事，验证 Go 相对 Node 的改善：

1. **per-provider 并发/限速队列** — `internal/queue`，用带缓冲 channel 做并发信号量 + 滑动窗口限速，`defer release()` 杜绝 slot 泄漏。
2. **流式转发 + 活跃超时** — `internal/proxy`，用 `context` 统一管理请求超时与流式活跃超时，取代 Node 版手搓的 `setTimeout` + `destroy` + `streamCtx` 标志。
3. **资源占用** — 静态二进制 + distroless 镜像，对比 Node 镜像体积。

## 目录结构

```
go/
├── cmd/gateway/        # 主入口：HTTP 服务、请求处理、健康检查、启动 banner
├── internal/config/    # YAML 配置加载与校验（对齐 lib/config.js）
├── internal/router/    # 路由匹配 + API Key 解析（对齐 lib/router.js）
├── internal/queue/     # per-provider 并发/限速队列（对齐 lib/queue.js）
├── internal/proxy/     # 流式/非流式转发 + context 超时（对齐 lib/proxy.js）
├── internal/converter/ # 三格式请求/响应/流式 SSE 互转（对齐 lib/converter.js）
├── internal/vision/    # 图片识别 + 缓存复用 + singleflight 去重（对齐 lib/vision.js）
├── internal/cache/     # SQLite 图片缓存，纯 Go modernc.org/sqlite（对齐 lib/cache.js）
├── Dockerfile          # 多阶段构建 → distroless 静态镜像
└── go.mod
```

## 功能实现状态

| 能力 | 状态 | 说明 |
|------|------|------|
| 配置加载/校验 | ✅ | 字段、默认值、区间与 Node 版一致 |
| 路由匹配 | ✅ | `*`/`?` 通配，nocase |
| 并发/限速队列 | ✅ | channel 信号量 + 滑动窗口 + 队列等待超时 |
| 流式转发 + 活跃超时 | ✅ | context 统一收口，超时补发合规 SSE 收尾 |
| 非流式转发 | ✅ | 三格式互转后回写 |
| 格式互转 converter | ✅ | 三格式请求/响应/流式 SSE 双向转换，对齐 Node 版逐函数实现 |
| vision 图片识别 | ✅ | 调用视觉模型 + SQLite 缓存复用 + 同图并发去重 + tool_result 递归 |
| SQLite 图片缓存 | ✅ | 纯 Go modernc.org/sqlite，`CGO_ENABLED=0` 可编译，保持静态二进制 |
| 健康检查 /health | ✅ | 队列状态 + 缓存统计 + 内存 |

> 已验证链路（Docker 实测）：
> - anthropic→anthropic 透传（含流式）
> - anthropic 上游→openai-chat 客户端（非流式 + 流式 SSE 转换），请求体 model 改写正确
> - 图片请求：第 1 次走视觉识别并写缓存，第 2 次命中缓存（`图片: 1 缓存`），`/health` 显示 `cache.total=1`

## 本地运行

```bash
cd go
# 复用项目根目录的 config.yaml（findConfigPath 优先查当前目录）
go run ./cmd/gateway
```

## 构建与运行镜像

```bash
cd go
docker build -t ai-gateway-go .
# 实测镜像体积约 20.5 MB（distroless + sqlite 驱动），对比 Node 版约 150-200 MB
```

运行（distroless 以 nonroot/UID 65532 运行，data 卷需对该用户可写）：

```bash
docker run -d -p 7789:7789 \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v "$PWD/data:/app/data" \
  ai-gateway-go
```

### SQLite 数据卷注意事项

Go 版用真正的 SQLite（`modernc.org/sqlite`），会用文件锁保证并发安全。只要 `data` 目录所在的文件系统支持
正常的文件锁语义，bind mount 与 named volume 都可用。实测情况如下：

| 部署方式 | 数据位置 | 结果 |
|----------|----------|------|
| 直接运行二进制（不经 Docker） | Mac 原生 APFS | ✅ 正常 |
| Docker named volume | Docker 管理的卷 | ✅ 正常 |
| **Docker bind mount（家目录范围内）** | 如 `~/docker/ai-gateway/data` | ✅ 正常（在 Colima + virtiofs 上实测：写入、命中、重启后从磁盘读回均正常） |
| Docker bind mount（共享范围外的路径） | 如 `/tmp/...` | ❌ 报 `unable to open database file: out of memory (14)` |

要点：

- **data 目录要放在容器运行时（Colima / Docker Desktop）共享给虚拟机的路径下**（通常是用户家目录范围内）。
  放在 `/tmp` 等未共享的路径会导致 SQLite 打不开数据库文件。
- **目录要对容器用户（distroless nonroot / UID 65532）可写**，最简单 `chmod 777 data`。
- 这与 Go 代码无关，是文件系统挂载/权限问题；Node 版 sql.js 整库读写 buffer、不用文件锁，所以没暴露这一点。

最省心的两种方式：

```bash
# 方式一（推荐用于 Mac 本机自用）：直接跑静态二进制，数据写在原生 APFS，零坑
./ai-gateway

# 方式二：Docker + named volume（与文件系统挂载方式无关，始终正常）
docker volume create aigw-data
docker run -d -p 7789:7789 \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v aigw-data:/app/data \
  ai-gateway-go
```

生产部署（Linux）用普通 bind mount 或 named volume 均正常，无上述路径限制。

## 迁移评估结论

功能已与 Node 版对等。实测收益：
- **镜像体积** 约 20.5 MB（Node 版约 150-200 MB，≈ 1/8）
- **运行内存** 启动后约 6 MB（Node 版约 50-80 MB）
- **稳定性** 队列 slot 用 `defer release()` 释放、超时用 `context` 统一收口，从语言层面规避了 Node 版易出的泄漏/卡死类问题

切流前建议：用真实流量对 Node 版与 Go 版做响应对拍，重点覆盖 converter 的跨格式与流式 SSE 分支。
