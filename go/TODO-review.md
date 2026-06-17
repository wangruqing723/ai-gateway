# Go PoC 待修复问题清单

> 来源：`/code-review` 对 `poc/go-rewrite` 分支（相对 `dev`）全部提交的多角度审查。
> 10 个角度发现约 50 个候选，逐个验证 + 补充扫描后，保留以下 15 个非 REFUTED 结论。
> 行号为验证器校正后的位置。对 PoC 而言无致命崩溃 bug，主要是并发竞争、与 Node 的边界行为偏差、超时语义差异。

## 修复优先级建议

- **第一批（并发，影响最大）**：#1 #2 #3 —— 生产并发下的真实隐患，`go test -race` 可复现。
- **第二批（超时/流式语义对齐 Node）**：#4 #5 #6 #7 #8。
- **第三批（迁移等价性，可用真实流量对拍发现）**：#9 #10 #11 #12。
- **清理（非 bug，可顺手做）**：#13 #14 #15。

---

## 🔴 并发 / 数据竞争

### 1. 共享 `*config.Provider` 被逐请求修改
- **位置**：`cmd/gateway/main.go:163`（+ `internal/router/router.go:23,28`）
- **问题**：`MatchRoute` 直接从 `cfg.Providers` map 返回共享指针，handler 随后 `p.APIKey = ResolveAPIKey(...)`。同一 provider 的并发请求写同一结构体的 `APIKey`。
- **后果**：数据竞争；当 key 来自客户端请求头时，一个请求可能用了另一个请求的 key 转发。
- **修法**：按请求复制 Provider，或把解析出的 key 单独传给 proxy，不回写共享结构体。

### 2. `timedOut` 数据竞争 + 定时器竞争
- **位置**：`internal/proxy/proxy.go:136–140`
- **问题**：`timedOut` 为普通 `bool`，超时 goroutine 写、主 goroutine 在 `finishStream` 读，无同步；另读循环 `timer.Reset()` 与 goroutine 消费 `timer.C` 并发，是经典 racy 模式。
- **后果**：数据竞争；可能漏触发或误触发流式活跃超时。
- **修法**：改用 `atomic.Bool`；超时信号改为基于 context（如单独的 timeout context）而非裸 timer + bool。

### 3. singleflight leader 把自己的 ctx 错误广播给所有等待者
- **位置**：`internal/vision/vision.go:219`
- **问题**：leader 用自己的请求 ctx 调用 `Acquire(ctx,…)` 和 `doRecognize(ctx,…)`，再把结果/错误写进所有等待者共享的 `recognition`。
- **后果**：leader 客户端断开时，其它同图在途请求即便自身 ctx 存活也收到取消错误。相比 Node 是回归。
- **修法**：识别用与单个请求解耦的 context（如 `context.Background()` + 自身超时），或 leader 取消时让等待者重新选举/各自重试。

---

## 🟠 超时 / 流式正确性

### 4. 流式路径没有整体超时
- **位置**：`internal/proxy/proxy.go:52`
- **问题**：请求 deadline 仅 `if !opts.IsStreaming` 时挂载。流式打到"接受连接却不发字节"的上游时，活跃计时器要等 `Do` 返回响应头才启动，`HTTPClient.Do` 无限阻塞。
- **后果**：流式请求可永久挂起；与 Node 对两条路径都施加 `timeout` 不一致。
- **修法**：给流式加 `http.Transport.ResponseHeaderTimeout`，或对 `Do` 阶段套一个 header 超时 context。

### 5. `bufio.Scanner` 4MB 单行上限静默截断流
- **位置**：`internal/proxy/proxy.go:171`
- **问题**：单行 SSE >4MB 时 scanner 以 `bufio.ErrTooLong` 停止，`finishStream` 把非 EOF 错误（`timedOut=false`）当客户端断开——不发 `gracefulSSEClose`，流截断、原因误标。
- **后果**：大图片/工具调用 delta 触发时客户端收到截断流且无合规收尾。
- **修法**：区分 `ErrTooLong` 与客户端断开；或改用无单行上限的逐字节/分块读取 + 自定义分行。

### 6. 非流式超时返回 502/proxy_error，而非 504/timeout_error
- **位置**：`internal/proxy/proxy.go:66`
- **问题**：非流式超时时 `Do` 返回 `DeadlineExceeded`，落到通用分支写 502 `proxy_error`。
- **后果**：状态码与错误类型都与 Node（504 `timeout_error`）不一致。
- **修法**：用 `errors.Is(err, context.DeadlineExceeded)` 单独分支，返回 504 + `timeout_error`。

### 7. 非超时类 Acquire 失败时不写任何响应
- **位置**：`cmd/gateway/main.go:201`
- **问题**：客户端在队列等待中断开时 `Acquire` 返回 `ctx.Err()`（非 `ErrQueueTimeout`），handler 只记日志就 return，未调 `writeJSONError`，net/http 默认补发空 200。
- **后果**：客户端已断开时影响有限，但控制流缺陷真实存在。
- **修法**：对 `ctx.Err()` 分支显式不写响应（注释说明客户端已断开），或统一兜底响应逻辑。

### 8. 格式转换流被客户端断开时误报"转发异常"
- **位置**：`internal/proxy/proxy.go:208`
- **问题**：非透传流式中客户端断开 → ctx 取消 → `scanner.Err()` 返回 `context.Canceled` → `finishStream` 当转发错误返回 → `main.go` 打假的"转发结束（异常）"日志。
- **后果**：每次客户端断开都被误记为异常；Node 把断开当正常结束。
- **修法**：在 `finishStream` 里识别 `context.Canceled`/客户端断开，按正常结束处理。

---

## 🟠 行为偏离 Node 原版

### 9. 缺 `media_type` 时图片哈希与 Node 不一致
- **位置**：`internal/cache/cache.go:76`
- **问题**：base64 块缺 `media_type` 时 Go 哈希 `"_<data>"`，Node 因模板插值 `undefined` 得 `"undefined_<data>"`。
- **后果**：Node/Go 混合或迁移部署时同图缓存互相命中不了，重复识别。
- **修法**：缺失时显式拼 `"undefined"` 以对齐，或（更优）双方统一改为只用 data 哈希并接受不向后兼容。

### 10. `blocksHaveImage` 递归深度与 Node 不一致
- **位置**：`internal/vision/vision.go:84`
- **问题**：Go 递归进入嵌套 `tool_result` 任意深度，Node 的 `hasImages` 只查一层。
- **后果**：图片嵌套两层时 Go 触发视觉翻译、Node 跳过 → 同输入产生不同上游请求体。
- **修法**：对齐 Node 只查一层（注意 Node 的 `processBlocks` 实际会递归，gate 与处理深度不一致是 Node 自身特性，需决定以哪边为准）。

### 11. content 块 `text` 为 JSON 数字时类型断言隐患
- **位置**：`internal/converter/converter.go:203`
- **问题**：客户端发 `content:[{type:"text", text:123}]`（数字解析为 float64），归一化时 `block["text"]` 透传，下游 `getString` 取不到字符串。
- **后果**：文本丢失/为空。
- **修法**：归一化时对 `text` 做数字→字符串兼容（如 `fmt.Sprint`）。

### 12. `nextReqID` 读改写竞争产生重复请求 ID
- **位置**：`cmd/gateway/main.go:254`
- **问题**：`atomic.AddUint64` 后跟非原子的 `if n>99999 { StoreUint64(1) }`，两步不构成整体原子。
- **后果**：计数接近 99999 时并发可让两个请求拿到同一 `r00001`，破坏日志关联。
- **修法**：改用 CAS 循环，或直接 `n % 100000` 取模生成 ID。

---

## 🟡 清理 / 一致性（非 bug）

### 13. passthrough 判定逻辑三处重复且已分歧
- **位置**：`internal/proxy/proxy.go:147` vs `internal/converter/stream.go:17`
- **问题**：proxy 用 2 分句窄规则，transformer 用含 `providerFormat == clientFormat` 的宽规则。`openai-responses`→`openai-responses` 等相同格式对，proxy 走逐行 scanner、transformer 认为 passthrough，两模块对帧处理分歧。
- **修法**：抽出单一 `isPassthrough(provider, client)` 函数，两处共用。

### 14. `writeJSONError` 等多处重复定义
- **位置**：`cmd/gateway/helpers.go:39` 与 `internal/proxy/proxy.go`（逐字节相同）；另 `buildVisionURL`/`buildUpstreamURL`、vision 与 upstream 的 header 设置（vision 已漂移，缺 `accept` 头）。
- **问题**：重复实现，改一处易漏另一处。
- **修法**：提取到共享内部包（如 `internal/httputil`）。

### 15. 每条日志都重新加载时区
- **位置**：`cmd/gateway/helpers.go:17`
- **问题**：`beijingTime()` 被每次 `logf` 调用，每次都 `time.LoadLocation("Asia/Shanghai")`。
- **后果**：热路径上重复解析，浪费。
- **修法**：包级 `var beijingLoc = ...` 缓存一次。

---

## 已 REFUTED（不修，仅备查）

- `internal/queue/queue.go:135` 切片 `[:0]` 复用"无界增长"——候选自相矛盾，锁内安全。
- `internal/converter/response.go:45` 空 `stop_reason` 处理——Go 已正确兜底为 `"stop"`。
- 若干 vision waiter `fromCache=false` 候选——定位错行或属故意行为。
