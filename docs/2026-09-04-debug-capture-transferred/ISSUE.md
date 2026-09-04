# ISSUE：被 failover 转移的 4xx/5xx 抓不到转发请求体

**发现日期**：2026-09-04
**状态**：待修（等 `docs/2026-09-04-max-context-window/` 的 Codex 委托交付后再动，两者都改 `cmd/gateway/main.go` 的 `forwardAttempt`）
**严重性**：不影响转发正确性，但让排查上游不透明错误的唯一手段在最需要它的场景下失效

> 本文档不记行号。`forwardAttempt` 正在被另一个需求改动，行号会失效——一律用函数名与
> 语义位置定位。

## 现象

线上一条真实请求（`r00095`，2026-09-04 22:34:49）的轨迹：

```
gor3:header_timeout → ar-ld:500/server → ar-ld-cn:200
```

第二个候选 `ar-ld` / `glm-5.3`（anthropic 透传）返回 500，上游错误正文是：

```json
{"error":{"message":"Upstream rejected the request as invalid","type":"invalid_request_error"},"type":"error"}
```

`AI_GATEWAY_DEBUG_UPSTREAM_BODY` 开关**已启用**（`docker-compose.yml` 里设为
`/app/data/upstream_debug.jsonl`，`docker inspect` 确认容器 env 也生效），但
`data/upstream_debug.jsonl` **根本不存在**，`data/` 下只有 `image-cache.db`。
这条 500 的转发体没被抓到。

## 根因

`captureUpstreamBody` 的唯一调用点在 `forwardAttempt` 的**终态返回段**
（`if upstreamStatus >= 400 { captureUpstreamBody(...) }`，紧邻函数末尾那个
`return forwardAttemptOutcome{trail: ...}`）。

而 `errors.Is(forwardErr, proxy.ErrAttemptAbandoned)` 分支在它**之前**就
`return forwardAttemptOutcome{abandoned: true, ...}` 了。于是：

- 不转移的终态 4xx/5xx → 抓到 ✅
- **被 failover 转移掉的 4xx/5xx → 永远抓不到** ❌

`ar-ld` 那次的 `结果/原因` 正是 `transferred`，走的就是漏掉的那条路径。

### 讽刺之处

`captureUpstreamBody` 自己的注释写明了它的起因：排查 GLM-5.3 对真实 Claude Code
请求返回 `[1210]` 的不透明错误，「触发字段藏在真实转发体里」。而 GLM-5.3 所在的路由
配了多候选 + failover，它的错误**必然**被转移——这个功能对它唯一的设计目标恰好失效。

## 缺的是「我们发出去的请求体」，不是「上游的响应体」

容易混淆，写清楚：转移路径里 `internal/proxy` 已经调过
`readObservedErrorBody` + `recordErrorBody`，所以**上游返回的错误正文有被记录**
（截图里那段 `invalid_request_error` JSON 就是这么来的）。

漏掉的只是**网关转发给上游的那个请求体**——也就是判断「哪个字段触发了拒绝」唯一
需要的东西。

## 修法（已验证可行，两个前置条件都成立）

在 `ErrAttemptAbandoned` 分支 `return` 之前补一次抓包，条件与终态分支一致
（`>= 400`，把传输错误/超时那类 `attemptStatus == 0` 的情况排除掉）。

两个前置条件都已核对：

1. **`upstreamBody` 可用**：它在 `json.Marshal(upstreamMap)` 处赋值，位于
   `proxy.Forward` 调用之前，转移分支拿得到。
2. **状态码可用，且不需要新变量**：`internal/proxy/proxy.go` 里
   `recordUpstreamHeaders(opts, resp)` 在 `abandonAttempt(...)` 判定**之前**执行
   （流式与非流式两条路径都是这个顺序），所以 `OnUpstreamStatus` 回调已经把
   `attemptStatus = code` 写好了。直接用 `attemptStatus` 即可。

   注意现有的 `abandonReason` / `abandonBreaker` / `abandonFree` 三个变量**没有**
   存状态码，容易误以为要再加一个——不用。

## 验收要点

- 双候选路由，首个上游返 500 且被转移 → `upstream_debug.jsonl` 里出现该候选的
  转发体记录，`upstreamStatus` 为 500。
- 传输错误/超时被转移（`attemptStatus == 0`）→ **不**写记录，与终态分支的
  `>= 400` 条件保持一致。
- 开关未设（env 为空）→ 完全跳过，不产生任何 I/O。
- 终态 4xx/5xx 的既有抓包行为不变（回归保护）。

## 顺带记录：两个不是 bug 的观察

**`gor3` 那 90s 是配置，不是缺陷。** `config.yaml` 顶层 `timeout: 90000`，那次
header_timeout 等满了整个预算，占 105s 总耗时的 86%。要不要调小是取舍：调小转移更快，
但会误杀慢启动的思考型模型。未决，不在本 issue 范围。

**截图与当前配置对不上，已排除。** 截图首个候选是 `gor3 / claude-opus-5-thinking`，
但当前 `claude-opus-4-8` 路由的 targets 里没有 `gor3`（首个是 `justDoWork`）。
`config.yaml` 在 22:51 被修改过（截图是 22:34），属于人工调整，非代码问题。

## 待确认（本 issue 修完才有条件查）

`ar-ld` / `glm-5.3` 那个 `invalid_request_error` 的触发字段仍然未知，**不要猜**。

已排除的一条：`config.yaml` 里 `maxTokens` 三层一个都没配（`grep -c` = 0），所以走
`EnsureMaxTokens`——它只在字段完全缺失时补 `DefaultMaxTokens`。Claude Code 会传
`max_tokens`，因此 `88434a5` 的 32768 默认值在这条请求上大概率没生效，不能把这个 500
归到 max_tokens 头上。

补上抓包、复现一次拿到真实转发体，再定性。
