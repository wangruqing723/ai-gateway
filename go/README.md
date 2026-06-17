# ai-gateway — Go PoC

这是 Node 版 `ai-gateway` 迁移到 Go 的概念验证（Proof of Concept），用于评估"性能/稳定性/体积"迁移收益。
**不是生产可用版本**，仅打通核心骨架，部分能力以 `TODO` 标注待补全。

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
├── Dockerfile          # 多阶段构建 → distroless 静态镜像
└── go.mod
```

## 已实现 vs 待补全

| 能力 | 状态 | 说明 |
|------|------|------|
| 配置加载/校验 | ✅ | 字段、默认值、区间与 Node 版一致 |
| 路由匹配 | ✅ | `*`/`?` 通配，nocase |
| 并发/限速队列 | ✅ | channel 信号量 + 滑动窗口 + 队列等待超时 |
| 流式转发 + 活跃超时 | ✅ | context 统一收口，超时补发合规 SSE 收尾 |
| 非流式转发 | ✅ | 三格式互转后回写 |
| 健康检查 /health | ✅ | 队列状态 + 内存 |
| **格式互转 converter** | ✅ | 三格式请求/响应/流式 SSE 双向转换（`internal/converter`），对齐 Node 版逐函数实现 |
| **vision 图片识别** | ⏳ TODO | 调用视觉模型 + 并发去重 |
| **SQLite 图片缓存** | ⏳ TODO | Go 用 modernc.org/sqlite（纯 Go，保持静态二进制） |

> 已验证链路（Docker 实测）：anthropic→anthropic 透传、anthropic 上游→openai-chat 客户端（非流式 + 流式 SSE 转换），请求体 model 改写正确。

## 本地运行

```bash
cd go
# 复用项目根目录的 config.yaml（findConfigPath 优先查当前目录）
go run ./cmd/gateway
```

## 构建镜像

```bash
cd go
docker build -t ai-gateway-go .
# 实测镜像体积约 15 MB（distroless），对比 Node 版约 150-200 MB
```

## 迁移工作量评估

剩余 TODO 中 **converter 是大头**（Node 版 483 行格式转换逻辑），建议用真实流量对 Node 版与 Go 版做响应对拍，保证等价后再切流。整体预计 3-5 人天可达到功能对等。
