# AI Gateway Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复审查确认的流式、队列、协议转换、配置、安全、观测与部署问题，并建立覆盖这些路径的自动化回归测试。

**Architecture:** 保持现有 package 边界；核心转发、队列、converter、vision 各自在包内闭环。`cmd/gateway` 负责 HTTP 边界、配置事务与运行时组装，`metrics/providerhealth` 提供独立的有界状态组件。部署默认仅本机可达，开发热加载通过 override 启用。

**Tech Stack:** Go 1.23、`net/http`、`gopkg.in/yaml.v3`、modernc SQLite、嵌入式 HTML/Alpine、Docker Compose、GitHub Actions。

## Global Constraints

- 所有实现先写失败测试，再写最小修复。
- 不读取或修改真实 `config.yaml`；测试配置全部使用临时目录。
- 不新增强制 gateway token；保持现有客户端请求头兼容。
- 不执行 `git commit`，完成后先交用户审阅。
- Go 验证优先使用仓库规定的 `golang:1.23-alpine` 容器；race 使用具备 gcc 的 Linux Go 环境。
- 不设置全局 `WriteTimeout`，避免截断长 SSE。

---

### Task 1: 修复流式转发与响应边界

**Files:**
- Modify: `internal/proxy/proxy.go`
- Create: `internal/proxy/proxy_test.go`

**Interfaces:**
- 保持 `proxy.Forward(*proxy.Options) error` 不变。
- 新增包内有限读取和 Responses 流终止辅助函数；不向 `cmd/gateway` 暴露新协议细节。

- [x] **Step 1: 写失败测试**
  - `TestForwardStreamContinuesAfterHeaders`：上游 flush header 后分两次写 SSE，客户端必须收到两段。
  - `TestForwardStreamHeaderTimeout`：上游不返回 header，得到 504。
  - `TestForwardStreamActivityTimeoutResponses`：Responses 客户端得到 `response.failed`，不得得到 `[DONE]`。
  - `TestForwardRejectsOversizedResponse` 与 `TestForwardRejectsOversizedSSEEvent`。
- [x] **Step 2: 运行 `go test ./internal/proxy -run TestForward -v`，确认当前实现失败。**
- [x] **Step 3: 用 header timer + request cancel 重写流式 Do 阶段；拿到 header 后停止 timer，Forward 返回前才 cancel。**
- [x] **Step 4: 将活动 timer 收口为单 goroutine watcher，并为非流式、错误体和跨格式单事件添加上限。**
- [x] **Step 5: 运行 proxy 包测试和 `go test ./...`。**

### Task 2: 用动态 FIFO admission 替换固定 channel queue

**Files:**
- Modify: `internal/queue/queue.go`
- Create: `internal/queue/queue_test.go`

**Interfaces:**
- 保持 `Manager.Acquire(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs)` 和 `StatusOf` 调用方式。
- `UpdateProvider` 必须动态更新并发与速率；新增 `Reconcile(map[string]Limits)` 可清理已删除 provider。

- [x] **Step 1: 写失败测试**：热改 `maxPerSecond 1→0`、并发扩缩、完整 `maxQueueWait`、FIFO、ctx 取消、重复 release、删除 provider。
- [x] **Step 2: 运行 queue 测试，确认 panic/超时语义等失败。**
- [x] **Step 3: 实现 mutex 保护的 waiter FIFO、running、滑动窗口和单个 rate wake timer。**
- [x] **Step 4: release 使用 `sync.Once`；所有取消路径移除 waiter，未 admission 不增加 running。**
- [x] **Step 5: 运行 `go test ./internal/queue -count=20` 与 race。**

### Task 3: 补齐三协议工具、system 与 Responses SSE 转换

**Files:**
- Modify: `internal/converter/converter.go`
- Modify: `internal/converter/response.go`
- Modify: `internal/converter/stream.go`
- Create: `internal/converter/converter_test.go`
- Create: `internal/converter/response_test.go`
- Create: `internal/converter/stream_test.go`

**Interfaces:**
- `Internal.Tools` 保存 Anthropic-like canonical tool definitions。
- canonical content blocks 使用 `tool_use` / `tool_result`；所有现有导出函数签名保持不变。

- [x] **Step 1: 写请求矩阵失败测试**：Anthropic↔Chat、Responses→Chat 的 tool definitions、tool calls、tool results；多块 system。
- [x] **Step 2: 写非流式响应矩阵失败测试**：Anthropic tool_use 与 Chat tool_calls 双向并转换到 Responses。
- [x] **Step 3: 写 SSE 失败测试**：`input_json_delta`、`delta.tool_calls`、Responses function arguments delta 与最终完整 output。
- [x] **Step 4: 实现 canonical tool/system 规范化和各协议映射。**
- [x] **Step 5: 在 transformer state 累积正文/工具参数，生成完整 done/completed。**
- [x] **Step 6: 运行 converter 全部测试与全仓测试。**

### Task 4: 修复 vision 取消与 directMode 热更新

**Files:**
- Modify: `internal/vision/vision.go`
- Create: `internal/vision/vision_test.go`

**Interfaces:**
- 新增 `(*Translator).SetDirectMode(bool)`。
- `recognition` 保存共享 context cancel、done、waiter count；缓存写入仍由共享任务执行一次。

- [x] **Step 1: 写失败测试**：多图中途取消不处理后续图片；全部 waiter 取消会取消上游；一个 waiter 取消不污染另一个；direct mode 可动态切换。
- [x] **Step 2: 运行 vision 测试确认失败。**
- [x] **Step 3: 将 shared recognition 放到 goroutine，调用者用自身 ctx 等待并维护 waiter count。**
- [x] **Step 4: directMode 改为 atomic，处理每个 content block 前检查原 ctx。**
- [x] **Step 5: 运行 vision 测试和 race。**

### Task 5: 修复配置解析、脱敏、保存事务与 HTTP 安全边界

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Modify: `cmd/gateway/helpers.go`
- Modify: `cmd/gateway/main.go`
- Create: `cmd/gateway/main_test.go`
- Modify: `cmd/gateway/web/index.html`

**Interfaces:**
- `config.DecodeAndValidate([]byte)` 使用 KnownFields 并返回完整边界错误。
- `config.RedactYAML([]byte, sentinel string)` 返回合法脱敏 YAML。
- server 新增 `configOpMu`、实际 listen 地址与统一 `applyRuntimeConfig`。
- 配置响应包含 `revision`、`restartRequired` 和 provider `apiKeyConfigured` 展示状态。

- [x] **Step 1: 写 config 失败测试**：nil provider、未知字段、所有数值边界、空 key、文件权限、并发 Save、Raw YAML block/flow/quoted/multiline 脱敏。
- [x] **Step 2: 写 handler 失败测试**：method、media type、cross-site、1MiB/32MiB body、secret preserve 身份约束、revision 冲突、restartRequired。
- [x] **Step 3: 实现有限 body 读取、same-origin/method/media-type guard 和 HTTP 安全 headers。**
- [x] **Step 4: 实现 yaml.Node 脱敏、精确 sentinel、事务锁、唯一 tempfile/fsync/0600 保存。**
- [x] **Step 5: 实现统一 runtime apply；host/port 只报告需重启，动态传播 queue/vision/cache/health。**
- [x] **Step 6: 前端仅配置页显示保存/重载，增加 dirty/revision、key configured 展示和正确 YAML Content-Type。**
- [x] **Step 7: 运行 config/cmd 测试与 race。**

### Task 6: 修复 metrics 与 provider health

**Files:**
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`
- Modify: `internal/providerhealth/health.go`
- Modify: `internal/providerhealth/health_test.go`

**Interfaces:**
- Collector 日志容量保持 1000，但总数与一分钟聚合不依赖日志 ring。
- Checker 增加 `InvalidateChanged(oldCfg, newCfg)`；`CheckAll` 共享全局并发与 singleflight。

- [x] **Step 1: 写 >1000 请求、分钟滚动、状态码/成功率/分位数失败测试。**
- [x] **Step 2: 实现有界秒级 buckets/延迟 histogram 和独立累计计数。**
- [x] **Step 3: 写健康 fingerprint 失效与并发 CheckAll 合并失败测试。**
- [x] **Step 4: 实现 fingerprint、全局 semaphore、singleflight 与冷却窗口。**
- [x] **Step 5: 运行两个包测试与 race。**

### Task 7: 优雅停机、静态前端依赖、Compose 与 CI

**Files:**
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/gateway/web/index.html`
- Create: `cmd/gateway/web/vendor/alpine.min.js`
- Create: `cmd/gateway/web/vendor/tailwindcss.js`
- Modify: `docker-compose.yml`
- Create: `docker-compose.dev.yml`
- Modify: `.github/workflows/docker.yml`
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `TODO-review.md`

**Interfaces:**
- `http.Server` 使用 ReadHeaderTimeout/IdleTimeout/MaxHeaderBytes；signal path 调用 `Shutdown(30s)` 后关闭 cache。
- 默认 Compose 仅发布本机端口；开发 override 才启用 web volume/hot reload。

- [x] **Step 1: 增加 server shutdown 与静态资源 handler 测试。**
- [x] **Step 2: vendor 固定版本脚本并将页面改为 self-hosted；设置 CSP/no-store/nosniff/referrer-policy。**
- [x] **Step 3: 拆分默认/开发 Compose，运行两套 `docker compose config --quiet`。**
- [x] **Step 4: CI 增加 test/race/vet/compose gate，再进入多平台镜像构建。**
- [x] **Step 5: 同步 README、CLAUDE、TODO 状态，不写未验证的生产就绪结论。**

### Task 8: 集成验证与审查

**Files:**
- Review all modified files

- [x] **Step 1: 运行 `gofmt` 并检查 `git diff --check`。**
- [x] **Step 2: Docker Go 1.23 运行 `go test ./...`、`go vet ./...`、`go test -race ./...`。**
- [x] **Step 3: 运行 `docker compose config --quiet`、开发 override 校验和 `docker build -t ai-gateway:hardening-verify .`。**
- [x] **Step 4: 重跑最初两个隔离复现，确认 SSE 读到完整两段且 queue 热更新不 panic/锁死。**
- [x] **Step 5: 独立代码审查安全、协议与测试覆盖，修复发现的问题后重复全量验证。**
- [x] **Step 6: 向用户汇报变更、测试和残余风险，等待明确 commit 指令。**
