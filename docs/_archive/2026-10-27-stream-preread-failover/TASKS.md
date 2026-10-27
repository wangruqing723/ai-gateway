> 实现方：Claude（文档模式）

# 流式开场预读：让「200 之后的流内错误」可故障转移

背景、方案与默认取舍见 [DESIGN.md](DESIGN.md)。触发案例：r00059（ar-ld / gpt-6.1-sol，上游 200 后流内 `rate_limit_exceeded`，未转移）。

## 任务

[x] T1 proxy：handleStream 拆出预读阶段，`markResponseStarted` + `WriteHeader(200)` 推迟到提交点；读取改为单一后台读取 goroutine，预读与正文阶段共用，活跃超时 watcher 覆盖两阶段 | P0 | 1.5h | -
[x] T2 proxy：按上游格式识别开场事件与失败事件（openai-responses / anthropic / openai-chat），失败事件映射为 `UpstreamStreamError{Status, Type, Code, Message, Raw}` | P0 | 1h | T1
[x] T3 proxy：预读命中失败事件 → `ShouldRetry(等效状态码, 0, err)`；放弃则 `OnErrorBody` 记原始事件并返回 `ErrAttemptAbandoned`；不放弃则写真实 HTTP 错误状态 + JSON 错误正文（不再 200 + SSE 失败事件） | P0 | 1h | T2
[x] T4 proxy：透传路径（同格式、含 namespace 还原）接入同一预读；提交时原样冲刷缓冲字节，之后恢复原有透传 / 逐行还原，字节逐一不变 | P0 | 1h | T1
[x] T5 main.go：`classifyFailure` / `breakerOutcomeFor` / `attemptFailureReason` 识别 `UpstreamStreamError`，按等效状态码分类；AttemptDetail 的 reason / error 带上游原文 | P0 | 45m | T2
[x] T6 proxy 单测：三种格式「开场 + error → 放弃」「开场 + 内容 → 正常提交且输出不变」「仅开场 → 保持上限到时提交」「透传字节不变」「预读期客户端断开」「不放弃时写真实状态码」 | P0 | 1.5h | T1-T4
[x] T7 cmd/gateway 集成测试：候选 1 流内 429 → 转到候选 2；最后一个候选返回真实 429；本地重试生效；熔断 429 不计、5xx 计 | P0 | 1h | T5
[x] T8 文档：CLAUDE.md「Request Flow」第 6 步与「可配置的转移边界」补充预读语义 | P1 | 20m | T3
[x] T9 验证：容器内 `gofmt` / `go vet ./...` / `go test ./...`；假上游 smoke 复现 r00059 场景并贴输出 | P0 | 30m | T1-T8

## 验证记录（2026-10-27）

- `gofmt -l ./cmd ./internal`：仅剩 `internal/breaker/breaker.go`（本轮未改动的既有问题）。
- `go vet ./...`：无输出。
- `go test ./...`：全部 ok（proxy 12.6s，其余见运行输出）。
- 集成测试 `TestFailoverTransfersOnInStreamRateLimit` 的日志复现了目标行为：
  `← 流内错误（等效 HTTP 429），放弃本候选` → `候选 limited 放弃（limited:429/ratelimit），尝试下一个` → 第二个候选 200。

## 不在本需求内

- 提交后（已写响应头）的流内错误仍记 `conversion_error`、不计熔断，见 DESIGN.md「范围外」。
- 预读期上游 EOF / 读错误语义不变：先提交再走原有收尾逻辑。
- 非流式路径不涉及（本来就整体读完再判定）。