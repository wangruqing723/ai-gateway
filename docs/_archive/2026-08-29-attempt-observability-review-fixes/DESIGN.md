# DESIGN：修复 code-review 15 项发现

日期：2026-08-29
被修复对象：`docs/plans/2026-08-29-attempt-observability-openai-protocol.md` 的落地代码（工作区未提交更改，22 改 + 2 新增，2230 行）
基线：HEAD `2b5754b`，修复前 `gofmt` 干净、`go build` exit=0、`go vet` 无输出、`go test ./...` 14 包全绿

---

## 一、核心决策：字段策略三分法

发现 1–3 是同一根因。计划文档 §10.1 把「不可转换字段是拒绝还是有损降级」列为**待确认事项**，代码替它拍板成「一律拒绝」，导致三条客户端→上游路径硬失败。

### 1.1 真正的 bug 是判定谓词

三处 deny-list（`converter.go:883`、`918`、`934`）用 `exists && value != nil` 当作「用户用了不可转换能力」的证据：

```go
if value, exists := extra[key]; exists && value != nil {
    return fmt.Errorf("cannot convert ... %q ...", key)
}
```

JSON 里 `"store": false` 得到 `exists=true`、`value=false`（非 nil）→ 被拒。而 `store:false` **就是 Chat 的原生行为**，写不写结果一样。

这与 CLAUDE.md「配置零值语义」记录的 `Failover`/`Breaker` 必须用 `*int` 是同一个问题，只是这次踩在 bool 上。

**deny-list 本身不错，错的是谓词。** 因此顺序是：先修谓词 → 再补映射 → deny-list 自然收到最短。

### 1.2 三分法

引入 `isMeaningfulExtra(key, value) bool` 前置判定，把字段分三类：

**A. 必须拒**（静默丢会改变会话或执行语义）

`previous_response_id`、`conversation`、`background`、`approval_request_id`、`store:true`、`reasoning.encrypted_content`、`logit_bias`、`prediction`、`modalities`、`audio`、`n>1`、`seed`、`logprobs`、`top_logprobs`，以及 Chat/Anthropic 目标下的 Responses 内置工具（`web_search` / `file_search` / `code_interpreter` / `computer_use`）。

**B. 必须映射**（能无损往返，拒绝是过度）

| 源 | 目标 | 映射 |
|---|---|---|
| Anthropic `stop_sequences` | Chat `stop` | 直接赋值 |
| `tool_choice` | 三协议三态互转 | 见 §二 |
| Chat `parallel_tool_calls:false` | Anthropic `tool_choice.disable_parallel_tool_use:true` | 取反嵌套 |
| `response_format` ↔ `text.format` | Chat ↔ Responses | 复用已有 `chatResponseFormatToResponses` / `responsesTextFormatToChat` |
| `reasoning_effort` ↔ `reasoning.effort` | 已实现，不动 | — |

**C. 静默丢弃**（有损但不改语义，拒绝代价远大于收益）

`top_k`（Chat/Responses 无此参数）、`stream_options.include_usage`（Anthropic 流天然带 usage）、`include`（只影响上游多返回哪些字段）、`reasoning.summary`、Chat 目标下的 `metadata`。

### 1.3 一个诚实的边界

`stop_sequences` 到 **Responses** 上游确实无处可放（Responses 不暴露 stop），这一格保留拒绝站得住。但到 **Chat** 上游必须映射成 `stop` —— 现在两个方向一起拒是过度。

### 1.4 为什么必须收窄

计划文档阶段 B 验收明写「**原有只有 anthropic / openai 的配置和行为不变**」。纯 deny-list 直接违背这条：改动前 `stop_sequences` 是静默丢弃，改动后变成硬失败 400，这是回归而非加固。

---

## 二、`tool_choice` 归一化

发现 4 与发现 2 耦合：`copyResponsesCompatibleExtras`（`converter.go:946`）把 `tool_choice` **原样拷进** Responses 请求体，而三个协议的结构互不相同。

新增 `toolChoiceToResponses(any) (any, error)`，归一规则：

| 客户端形态 | 归一为 |
|---|---|
| Chat `{"type":"function","function":{"name":"x"}}` | `{"type":"function","name":"x"}` |
| Anthropic `{"type":"tool","name":"x"}` | `{"type":"function","name":"x"}` |
| `"auto"` / `"required"` / Anthropic `"any"` | 保持字符串 |

**关键约束**：引入该函数的同时，必须把 `tool_choice` 从 `copyResponsesCompatibleExtras` 的原样拷贝列表里摘掉。否则原始结构会覆盖映射结果，等于发现 4 没修。

仅 `openai-responses` 客户端直通 Responses 上游时才走原样保留。

---

## 三、reasoning item 的处理：忽略，不映射

发现 5 指出 `parseResponsesOutput`（`response.go:312`）对 `reasoning` output item 直接报 unsupported，而 gpt-5 / o 系列的 Responses 响应**必带** reasoning item —— 等于 Responses 上游在推理模型下 100% 不可用（502 conversion_error）。

**决策：忽略该 item，不映射为 Anthropic thinking 块。**

依据计划 §6.3：「Responses 的 reasoning、audio 等事件不应在第一阶段伪装成普通文本」，以及 §6.1 第 4 条「第一阶段不宣称支持所有 Responses 专属能力」。忽略是最小且不越界的选择；映射成 thinking 块属于新增能力，应另立需求。

**连带影响**：流式侧的 `response.reasoning_summary_text.delta` 保持现状（不新增映射）。现有测试 `internal/converter/stream_test.go:170` 的 `TestResponsesStreamRejectsUnsupportedSemanticEvents` 断言「未支持事件必须产生失败终态」—— 该测试**继续保留**，因为我们不改流式侧的 reasoning 语义，只改非流式 output item 解析。若 Codex 发现二者实际耦合，记入 `KNOWN_ISSUES.md` 等决策，不擅自改测试。

同时删掉 `response.go` 里 `case "reasoning","computer_call",...` 那一整行 —— 它与 `default` 分支返回完全相同的错误，是死代码。

---

## 四、`RequestLog.Format` 语义：恢复客户端格式

发现 5 之外唯一的语义决策项。事实链：

- `metrics.go:294-295` 有回落 `if record.Format == "" { record.Format = record.ClientFormat }`
- 改动前 `Format` **从未被赋值**，所以恒等于 `ClientFormat`
- 新增的 `main.go:815` `in.reqLog.Format = p.Format` 把语义翻转成上游 provider 格式

**决策：删掉 `main.go:815` 这一行。**

理由：上游格式已由 `AttemptDetail.ProviderFormat` 表达（计划 §5.2 明确定义），不必复用 `Format` 字段。保留历史语义可避免新旧日志不可比、前端 `log.format || log.clientFormat` 显示漂移。这是最小改动且不损失信息。

评审 agent 标注这一项是 15 项里唯一可能属于「有意变更」、置信度最低。若你本意就是要在日志顶层显示上游协议，请驳回本决策 —— 那需要改成删掉 `Collector.Add` 的回落并把字段改名为 `providerFormat`，是更大的改动面。

---

## 五、错误正文读取时序重构

发现 7、8 是同一处读取时序的两个面，**必须一次重构**，分开改会把 8 的上限判断写进 7 要删掉的代码里。

现状问题：

- 流式路径（`proxy.go:354`）在 `abandonAttempt` **之前**整体读错误正文，把 failover 决策压在一个最长等于 `StreamActivityTimeout` 的阻塞读之后。上游返回 429 响应头后卡住不发 body 时，要等满数十秒才切下一个候选。改动前 `drainBody` 无计时器、最多 64 KiB 立即返回。
- 非流式路径（`proxy.go:408`）对每个 4xx/5xx 都读满 `maxErrorBodyBytes+1`（1 MiB），包括随即被放弃、正文只会被丢掉的候选。

**重构方向**：把采集拆成两步。

1. 放弃分支：只读 8 KiB 观测片段（带独立的短计时器），`recordErrorBody` 后**立刻** `abandonAttempt`
2. 回传分支：确认不转移、要写回客户端时，才按 `maxErrorBodyBytes` 继续读全量

实现上给 `readErrorBody` 增加 `limit` 参数，由调用方按「是否可能回传」选择上限。

---

## 六、其余修法要点

| 发现 | 位置 | 要点 |
|---|---|---|
| 6 | `stream.go:1402`、`1160` | `tool, _ := t.ensureTool(...)` 丢弃了新建工具时产出的 `content_block_start` / 首个 tool_calls 块。改为接收 `added` 并前置拼进返回切片 |
| 9 | `stream.go:1295` | `finish` 遍历 map `t.textStarted` 生成 `content_block_stop`，Go map 顺序随机。新增 `textOrder []int`，与已有 `toolOrder` 同构 |
| 13 | `stream.go:1397`、`1155` | `function_call_arguments.done` 在 `t.tools[idx]==nil` 时静默吞掉参数。补 `t.s.addError(...)` 产出失败终态 |
| 14 | `proxy.go:177` | 字节下标截断可能切断 UTF-8 多字节序列。截断后 `strings.ToValidUTF8` 或回退到 `utf8.RuneStart` 边界。**两处**截断都要处理 |
| 10 | `main.go:828` | `Reason` 无条件写 `queue_timeout`，但该分支也覆盖客户端断开。按 `errors.Is(err, queue.ErrQueueTimeout)` / `r.Context().Err() != nil` / 其余 三分支分别写 `queue_timeout` / `client_disconnected` / `queue_error`，与下方已有的三分支共用同一判定避免漂移 |
| 12 | 前端模板 | `markResponseStarted` 在成功路径同样触发，前端把它当失败提示渲染。改模板条件为 `detail.responseStarted && detail.outcome !== 'success'`（比复位标记改动更小且不丢信息） |
| 15 | 多处 | 删 `handleStreamError`、`handleError`、`ConvertOpenAIResponsesResponse`（全仓无调用）；`handleStreamErrorPayload` 去掉未使用的 `cancel` 参数；`converter.go:925` 英文注释改中文（违反 CLAUDE.md 约定）|

发现 15 的删除**必须排在 7/8 之后** —— `handleStreamError` 里 timer + `timedOut` + `clientRequestCanceled` 的顺序正是重构时要参照的，先删就得从 git 里捞。

---

## 七、不在本次范围

`AttemptDetails` 的 `ErrorBody` 内存占用（8 KiB × 1000 条环形缓冲 × 3 候选 ≈ 24 MB）。评审 agent 判定为「已批准设计的后果」而非缺陷，计划 §10.3 接受 8 KiB 上限。候选数增长时值得回看，不在本轮处理。

---

## 八、验证要求

每项改完都要在 `ai-gateway-dev-verify` 容器内跑四道：

```bash
docker run --pull never --rm --name ai-gateway-dev-verify \
  -v "$PWD":/work -w /work -v ai-gateway-gomod:/go/pkg/mod \
  -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 \
  golang:1.23-alpine sh -c 'gofmt -l ./cmd ./internal && go vet ./... && go test ./...'
```

`gofmt` 输出必须为空，`vet` 无输出，`go test ./...` 14 包全绿。

race detector 另跑（`golang:1.23-alpine` 不含 gcc，需 `apk add gcc musl-dev`，容器有网络）：

```bash
go test -race ./internal/proxy/ ./internal/metrics/ ./internal/queue/ \
  ./internal/breaker/ ./internal/balancer/ ./cmd/gateway/
```

发现 9 的测试必须配 `-count=20` 跑，否则测不出 map 随机性。

