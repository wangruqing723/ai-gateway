# AI Gateway 全量问题修复设计

## 目标

在不改变 Claude Code、Claude Desktop 与 Codex CLI 现有接入方式的前提下，关闭本轮审查确认的安全、并发、流式、协议转换、配置管理、观测与部署问题，并用自动化测试覆盖每条高风险回归路径。

## 约束

- 网关只用于本机，不设计公网多租户鉴权。
- Docker 对宿主机仅发布 `127.0.0.1:7789`；容器内仍监听 `0.0.0.0`。
- 不引入强制 gateway token，避免改变现有客户端请求头。
- 管理与代理接口执行同源、HTTP method、`Content-Type` 和请求体大小校验，阻断浏览器跨站滥用与意外资源耗尽。
- Go 1.23、纯 Go SQLite、`CGO_ENABLED=0` 静态构建约束保持不变。
- 不提交真实 `config.yaml`、缓存数据或任何凭据。
- 所有代码提交仍遵循“先给用户审阅，再明确确认提交”。

## 修复架构

### 1. 本地安全边界

生产 `docker-compose.yml` 将端口改为 `127.0.0.1:7789:7789`，并移除前端热加载挂载；新增开发 override 单独启用 `AI_GATEWAY_WEB_DIR`。代理端点只接受 `POST application/json`，配置 PUT 接受 JSON/YAML 媒体类型；带非同源 `Origin` 或 `Sec-Fetch-Site: cross-site` 的状态变更与推理请求直接拒绝。无 `Origin` 的本机 CLI/curl 请求保持兼容。

应用不承诺公网安全。浏览器管理只允许 loopback Origin；跨机器管理需通过 SSH 本地端口转发后仍以 loopback 地址访问。README 同时明确无 Origin 的远程 API 客户端仍必须由反向代理提供认证、TLS 和访问控制。

### 2. HTTP 与内存边界

服务配置 `ReadHeaderTimeout=10s`、`IdleTimeout=120s`、`MaxHeaderBytes=1MiB`，不设置会截断 SSE 的全局 `WriteTimeout`。配置体上限为 1MiB，推理请求体和正常上游响应上限为 32MiB，上游错误体为 1MiB，单个跨格式 SSE 事件为 8MiB；超限分别返回 413、502 或合规 SSE error/终止事件。

读取辅助函数区分“超过限制”和普通 I/O 错误，确保客户端得到稳定的 JSON 错误结构。

### 3. 流式转发生命周期

流式请求只在等待响应头阶段启用定时取消：用独立 timer 在 `Do` 尚未返回时取消请求，拿到响应头后停止 timer，但保持请求 context 到 response body 完成。活动超时使用单一 watcher 管理 timer reset，避免 reset/触发竞态；客户端取消、响应头超时、活动超时与上游读取错误分别记录和返回。

OpenAI Responses 流的正常终态、超时终态和工具调用终态必须使用 Responses 事件，不再发送 Chat Completions 的 `[DONE]`。

### 4. 动态队列调度

以 FIFO waiter 调度器替换固定容量 channel。每个 provider queue 在同一 mutex 下维护动态 `maxConcurrent`、`maxPerSecond`、running、waiters 与滑动窗口；`maxQueueWait` 覆盖从入队到真正放行的完整 admission 时间。配置更新、取消、速率 timer 与 release 都调用同一个调度入口。

该结构消除锁外配置读取、空切片 panic、panic 后 mutex/slot 泄漏，并允许热更新并发与速率限制。release 使用 `sync.Once`，保证重复释放安全。

### 5. 三协议规范化

内部格式继续使用 Anthropic-like message blocks，但补齐明确的规范化函数：

- Tool definition 统一为 `{name, description, input_schema}`。
- Tool call 统一为 `{type: tool_use, id, name, input}`。
- Tool result 统一为 `{type: tool_result, tool_use_id, content, is_error}`。
- System prompt 将 string 或多 text block 完整拼接，不静默丢弃后续块。

Anthropic、OpenAI Chat、OpenAI Responses 的请求、非流式响应与 SSE 分别做双向 tool schema/call/result 映射。无法无损映射的未知块显式保留或返回协议错误，不再静默删除。Responses transformer 累积正文与工具参数，生成包含最终内容的 `output_text.done`、`output_item.done` 和 `response.completed`。

### 6. 视觉 singleflight 生命周期

共享识别任务由独立 goroutine 执行，`recognition` 保存 cancel 与 waiter 数。每个调用者只等待自己的 context；最后一个 waiter 离开时取消上游识别。处理多图时每个块前检查原请求 context，避免客户端断开后继续逐图调用。`directMode` 改为原子动态状态，可随配置热重载。

### 7. 配置安全往返与事务

JSON 配置响应不再把展示文案写入 `apiKey`。前端使用独立 `apiKeyConfigured` 状态展示“已配置”，空 key 仍表示从客户端请求头取 key。保留已有 secret 使用精确 sentinel，并且只有 provider 的 `baseUrl` 与 `format` 未变化时才允许保留；身份字段变化必须显式提供新 key 或清空。

Raw YAML 用 `yaml.Node` 遍历并替换所有 provider `apiKey`，重新 marshal 为合法 YAML，覆盖 block/flow/quoted/multiline 写法。保存使用 server 级配置事务锁、唯一临时文件、`fsync`、不宽于 `0600` 的权限和 rename；单文件挂载退回直接写时也受同一锁保护。

YAML decoder 开启 `KnownFields`，在应用默认值前拒绝 nil provider，并校验 provider、cache、queue 与 direct timeout 的完整数值边界。

### 8. 热重载语义

新增统一 `applyRuntimeConfig`：原子替换请求配置、动态更新/删除 provider queue、更新视觉 direct mode、使变更 provider 的健康缓存失效，并按新 cache 策略执行清理。`host` 与 `port` 属于不可热更新字段；响应返回 `restartRequired` 列表，健康接口同时报告实际监听地址，避免把磁盘配置误报为运行状态。

并发 PUT/reload 由同一事务锁串行化，并可选携带 revision，避免多标签页静默覆盖。前端只在配置页面显示保存/重载按钮，并仅在 dirty 时允许保存。

### 9. 指标与健康检查

请求日志仍使用 1000 条环形缓冲；累计请求数和最近一分钟统计改用独立计数/时间桶，不再受日志容量限制。延迟分位数采用固定桶直方图，保证内存有界。Provider 健康记录携带 endpoint/format 指纹，配置变化后旧结果显示为 unchecked。

健康检查使用 Checker 级全局 semaphore、singleflight 与短冷却窗口，避免并发 POST 成倍放大探测请求。

### 10. 生命周期、前端依赖与 CI

SIGTERM/SIGINT 先停止接入并调用 `http.Server.Shutdown`，等待最多 30 秒后再关闭 SQLite。管理页脚本改为仓库内固定版本静态资源并设置安全响应头；字体等非特权展示资源允许失败降级。

CI 在镜像构建前执行 `go test ./...`、`go test -race ./...`、`go vet ./...` 与 `docker compose config --quiet`。README、CLAUDE.md 和 TODO-review.md 同步当前测试状态、开发 override、本地安全边界和热重载限制。

## 错误处理

- 客户端 method/media type/origin/body 超限：返回 405/415/403/413 的统一 JSON 错误。
- 上游响应体或 SSE 事件超限：在尚未写响应头时返回 502；已经进入流式响应时发送对应客户端协议的 error/failed 终态并关闭。
- 配置并发、revision 冲突：返回 409，不写磁盘、不替换运行时配置。
- 配置保存成功但包含 host/port 变化：返回 200，并携带 `restartRequired`，不声称监听地址已热更新。
- Provider queue 取消或超时：确保 waiter 被移除、timer 停止、running 不增加。

## 测试策略

每个修复先写失败测试，再写最小实现：

- `proxy`：响应头后继续读取两段 SSE、header/activity timeout、超大事件、Responses 终态。
- `queue`：动态启停限速、并发缩放、完整 maxQueueWait、取消、FIFO、重复 release、race。
- `converter`：三协议 tool definition/call/result 的请求、响应和 SSE 表驱动矩阵；多块 system。
- `config/cmd`：空 key、已配置 key、Raw YAML 各语法、身份变化禁止 secret preserve、nil provider、数值边界、并发 PUT/reload、文件权限。
- `vision`：所有 waiter 取消后停止上游、多图片中途取消、direct mode 热更新。
- `metrics/providerhealth`：超过 1000 请求、分钟窗口滚动、健康指纹失效、并发探测合并。
- 集成：本地端口发布、method/media type/origin/body limit、优雅停机、Compose 与 Docker build。

最终验证以 Docker Go 1.23 工具链执行全量 test、race、vet、格式检查、镜像构建和 Compose 校验；真实 provider 不参与自动化测试。
