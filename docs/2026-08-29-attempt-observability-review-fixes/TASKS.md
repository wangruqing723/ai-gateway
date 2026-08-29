# TASKS：修复 code-review 15 项发现

日期：2026-08-29
DESIGN：见同目录 `DESIGN.md`
基线：HEAD `2b5754b`，修复前 `gofmt`/`vet`/`go test ./...` 全绿，**修完必须仍全绿**

任务顺序已按依赖排好，**请按编号顺序执行**，不要并行跳步。

---

## P0：客户端路径硬失败（当前有三条协议路径完全不可用）

```
[ ] T1 修判定谓词 + 三分法收窄 deny-list + 补字段映射（发现 1/2/3） | P0 | 3h | 无
[ ] T2 tool_choice 三协议归一化（发现 4）                          | P0 | 1.5h | T1
[ ] T3 reasoning output item 改为忽略（发现 5）                    | P0 | 1h | 无
[ ] T4 修 ensureTool 丢弃 content_block_start 事件（发现 6）        | P0 | 1.5h | 无
[ ] T5 错误正文读取时序重构（发现 7 + 8，同一处）                   | P0 | 2.5h | 无
```

## P1：正确性与可观测性缺陷

```
[ ] T6 修 map 迭代顺序导致输出不可复现（发现 9）                    | P1 | 1h | 无（须早于任何多文本块测试）
[ ] T7 function_call_arguments.done 的 nil 分支静默吞参数（发现 13） | P1 | 1h | T4
[ ] T8 UTF-8 多字节截断（发现 14）                                 | P1 | 0.5h | T5
[ ] T9 队列错误 Reason 误分类（发现 10）                           | P1 | 0.5h | 无
[ ] T10 恢复 RequestLog.Format 客户端格式语义（发现 11）            | P1 | 0.5h | 无
```

## P2：前端展示与清理

```
[ ] T11 responseStarted 在成功行误显示失败提示（发现 12）           | P2 | 0.5h | 无
[ ] T12 删死代码 + 未使用参数 + 英文注释改中文（发现 15）            | P2 | 1h | T5
```

合计估时约 16h。

---

## 任务详情

### T1 修判定谓词 + 三分法收窄 deny-list + 补字段映射

**目标**：修掉「显式写默认值被当成用了不可转换能力」的误判，把 deny-list 收到最短，并补上能无损往返的字段映射。

**文件**：`internal/converter/converter.go`（`validateOpenAIChatTargetExtras:878`、`validateAnthropicTargetExtras:909`、`validateResponsesTargetExtras:929`，以及对应的 `ToOpenAIChatBody` / `ToAnthropicBody`）

**接口约束**：新增 `isMeaningfulExtra(key string, value any) bool`，不导出。判定「该值是否代表用户真的用了某项能力」：`store=false`、`n=1`、`parallel_tool_calls=true`、`stream_options={"include_usage":true}` 一律视为等价默认值 → 返回 false（放行并丢弃）；`store=true`、`previous_response_id`、`conversation` → 返回 true（走拒绝）。

三类字段清单见 `DESIGN.md` §1.2，**严格照抄，不要自行增删**。

要点：
- `validateOpenAIChatTargetExtras` 的 key 列表删掉 `stop_sequences`、`top_k`
- `ToOpenAIChatBody` 加 `in.Extra["stop_sequences"]` → `body["stop"]` 映射；`top_k` 静默丢弃（恢复改动前行为）
- `validateAnthropicTargetExtras` 把 `tool_choice`、`parallel_tool_calls`、`stream_options` 移出拒绝名单；`ToAnthropicBody` 里映射：`tool_choice` 三态 → Anthropic `{"type":"auto"|"any"|"tool","name":...}`，`parallel_tool_calls=false` → `tool_choice.disable_parallel_tool_use=true`，`stream_options` 直接丢
- `n`/`seed`/`logprobs` 等**保留拒绝**，但改用新谓词判定
- `reasoning` 的处理从「非 effort 即拒」改为「`summary` 丢弃、`encrypted_content` 才拒」
- 顺带修错误文案：`stop_sequences` 是 Anthropic 字段，现在的错误信息写成 `"Responses field"` 是错的
- `stop_sequences` → **Responses** 上游保留拒绝（Responses 不暴露 stop），只有到 Chat 才映射

**验收**：
- 正例：`ToOpenAIChatBodyChecked` 对带 `stop_sequences`/`top_k`/`metadata` 的 Anthropic 输入返回 nil error，且 `body["stop"]` 等于原 `stop_sequences`；对带 `store:false`/`include`/`reasoning.summary` 的 Responses 输入返回 nil error
- 正例：`ToAnthropicBodyChecked` 对带 `tool_choice`/`parallel_tool_calls` 的 Chat 输入返回 nil error 且映射值正确
- **负例（防收窄过头，必须有）**：`previous_response_id`、`conversation`、`background`、`store:true`、`n:2` 仍返回非 nil error
- 表驱动测试写在 `internal/converter/converter_test.go`

---

### T2 tool_choice 三协议归一化

**目标**：`copyResponsesCompatibleExtras` 把 `tool_choice` 原样拷进 Responses 请求体，导致上游收到不认的结构。

**文件**：`internal/converter/converter.go:946`

**接口约束**：新增 `toolChoiceToResponses(any) (any, error)`。归一规则见 `DESIGN.md` §二表格。

**关键约束**：引入该函数的同时**必须**把 `tool_choice` 从 `copyResponsesCompatibleExtras` 的原样拷贝 key 列表里摘掉。否则原始结构覆盖映射结果，等于没修。

仅 `openai-responses` 客户端直通 Responses 上游时才原样保留。

**验收**：断言 `ToOpenAIResponsesBodyChecked` 输出的 `tool_choice`，Anthropic `{"type":"tool","name":"x"}` 与 Chat `{"type":"function","function":{"name":"x"}}` 都等于 `{"type":"function","name":"x"}`；`"auto"` 保持字符串。

---

### T3 reasoning output item 改为忽略

**目标**：gpt-5 / o 系列的 Responses 响应必带 reasoning item，当前直接报 unsupported → 502，Responses 上游在推理模型下 100% 不可用。

**文件**：`internal/converter/response.go:312`（`parseResponsesOutput`）

**接口约束**：`switch` 增加 `case "reasoning"`，忽略该 item（仅计入 `parsed` 的可选 reasoning 字段），不报错。**不要**映射为 Anthropic thinking 块 —— 决策见 `DESIGN.md` §三。

同时删掉 `case "reasoning","computer_call",...` 那一整行：它与 `default` 返回完全相同的错误，是死代码，只保留 `default`。

**不要动** `internal/converter/stream_test.go:170` 的 `TestResponsesStreamRejectsUnsupportedSemanticEvents`。本任务只改非流式 output item 解析，不改流式 reasoning 语义。若发现二者实际耦合，**记入 `KNOWN_ISSUES.md` 等决策，不擅自改测试**。

**验收**：output 含 `reasoning` + `message` 两个 item 时，转 Anthropic 与转 Chat 均 error 为 nil，且文本完整保留、usage 正确。

---

### T4 修 ensureTool 丢弃 content_block_start 事件

**目标**：`response.output_item.done` 分支用 `tool, _ := t.ensureTool(...)` 丢弃了新建工具时产出的首块事件，客户端收到没有 start 的 delta，工具调用解析失败。

**文件**：`internal/converter/stream.go:1402`（`responsesToAnthropic`）、`:1160`（`responsesToChat`），**两处都要改**

**接口约束**：改成 `tool, added := t.ensureTool(outputIndex, item)`，把 `added` **前置**拼进返回切片，再追加 `appendToolArguments` 的结果。`tool.arguments.Len()==0` 判定不变。

**验收**：「只发 `output_item.done` 的 function_call」场景下，Anthropic 侧 `content_block_start` 出现在 `content_block_delta` 之前且 index 一致；Chat 侧带 `id`/`name` 的首个 tool_calls chunk 出现在 arguments chunk 之前。

---

### T5 错误正文读取时序重构（发现 7 + 8）

**目标**：两个发现是同一处读取时序的两个面，**必须一次改完**。分开改会把 8 的上限判断写进 7 要删掉的代码里。

**文件**：`internal/proxy/proxy.go`（流式 `:354`、非流式 `:408`、`readErrorBody:110`、`readStreamErrorBody:137`）

**接口约束**：给 `readErrorBody` 增加 `limit` 参数，由调用方按「是否可能回传客户端」选择上限。

- 放弃分支：只读 `maxObservedErrorBodyBytes`(8 KiB) 观测片段，带**独立的短计时器**（不要用 `StreamActivityTimeout`），`recordErrorBody` 后**立刻** `abandonAttempt`
- 回传分支：确认不转移时才按 `maxErrorBodyBytes`(1 MiB) 读全量，交给 `handleStreamErrorPayload` / `handleErrorPayload`

必须继续 drain 并关闭 response body，不能因观测破坏连接复用（计划 §5.4）。

**验收**：假上游先写 429 响应头再阻塞 body，断言 `Forward` 在**远小于** `StreamActivityTimeout` 的时间内返回 `ErrAttemptAbandoned`，且 `OnErrorBody` 收到的长度 ≤ 8 KiB。这条测试同时守住 7 的时延和 8 的读取量。

---

### T6 修 map 迭代顺序导致输出不可复现

**目标**：`finish` 遍历 map 生成 `content_block_stop`，Go map 顺序随机，多文本块时输出不稳定。

**文件**：`internal/converter/stream.go:1295`（`responsesToAnthropic.finish`）

**接口约束**：给 `responsesToAnthropic` 增加 `textOrder []int` 字段，`ensureText` 里 append 记录首次出现顺序（与已有 `toolOrder` 同构）；`finish` 改为遍历 `textOrder` 而非 `range t.textStarted`。

**验收**：构造两个不同 `output_index` 的文本 item，断言 `content_block_stop` 的 index 严格升序。**必须配 `-count=20` 跑**，否则测不出 map 随机性。

---

### T7 function_call_arguments.done 的 nil 分支静默吞参数

**目标**：`t.tools[outputIndex]` 为 nil 时既不建工具也不记错误，函数调用及其 arguments 完全不出现在下游流，客户端认为模型没调工具。

**文件**：`internal/converter/stream.go:1397`、`:1155`，**两处都要改**

**接口约束**：nil 分支与 `appendToolArguments` 保持一致，调用 `t.s.addError(fmt.Errorf("responses function arguments done arrived before output item %d", outputIndex))`，让 `StreamFailure` 产出目标协议失败终态，而不是静默成功。

**验收**：只发 `function_call_arguments.done`（无 added、无 `output_item.done`）时，输出为目标协议的失败终态，不是正常完成。

---

### T8 UTF-8 多字节截断

**目标**：字节下标截断可能切断 UTF-8 序列，导致 `ErrorBody` 含非法 UTF-8，`json.Marshal` 替换为 U+FFFD，前端显示尾部乱码。

**文件**：`internal/proxy/proxy.go:177` 附近，**两处截断都要处理**（观测片段截断、脱敏后长度兜底）

**接口约束**：截断后调用 `strings.ToValidUTF8(s, "")`，或回退到最后一个 `utf8.RuneStart` 边界再切。

**验收**：上游返回大于 8 KiB 的中文错误正文、第 8192 字节落在汉字中间时，`AttemptDetail.ErrorBody` 是合法 UTF-8。

---

### T9 队列错误 Reason 误分类

**目标**：`Reason` 无条件写 `queue_timeout`，但该分支也覆盖客户端断开，日志把客户端主动断开误报成网关队列超时。

**文件**：`cmd/gateway/main.go:828`

**接口约束**：把 `Reason` 赋值挪进判定内，三分支：`errors.Is(err, queue.ErrQueueTimeout)` → `"queue_timeout"`；`r.Context().Err() != nil` → `"client_disconnected"`；其余 → `"queue_error"`。**与下方已有的 if/else if/else 三分支共用同一判定**，避免两处漂移。

**验收**：让 `Acquire` 因客户端 ctx 取消而失败，断言 `AttemptDetail.Reason` 不等于 `"queue_timeout"`、`Outcome` 为 `"skipped"`。

---

### T10 恢复 RequestLog.Format 客户端格式语义

**目标**：新增的赋值把日志 `format` 字段语义从客户端格式翻转为上游 provider 格式，新旧日志不可比。

**文件**：`cmd/gateway/main.go:815`

**接口约束**：**删掉 `in.reqLog.Format = p.Format` 这一行**。不要动 `metrics.go:294-295` 的回落逻辑。

理由见 `DESIGN.md` §四：上游格式已由 `AttemptDetail.ProviderFormat` 表达。

**验收**：Anthropic 客户端命中 openai 上游时，`RequestLog.Format` 为 `"anthropic"`（客户端格式），不是 `"openai"`。

---

### T11 responseStarted 在成功行误显示失败提示

**目标**：`markResponseStarted` 在成功路径同样触发，前端把它当失败提示渲染，与设计文档的排障语义相反。

**文件**：`cmd/gateway/web/src/index.template.html`（`x-show="detail.responseStarted"` 那一行）

**接口约束**：条件改为 `detail.responseStarted && detail.outcome !== 'success'`。**不要**改后端复位标记 —— 前端改动更小且不丢信息。

**改完必须重新生成产物**：`make web-html`（或 `go run ./cmd/webbuild`）。`cmd/gateway/web/index.html` 是构建产物，直接改产物会被 `webbuild -check` 拦下。

**验收**：`go run ./cmd/webbuild -check` 通过（产物与源一致）；成功尝试行不显示「已开始响应，不可转移」。

---

### T12 删死代码 + 未使用参数 + 英文注释改中文

**目标**：改动留下三处不可达函数、一个未使用参数、一段违反项目约定的英文注释。`go vet` 不报未使用的包级函数，所以 CI 全绿也留得住。

**文件**：
- `internal/proxy/proxy.go:777`、`:820`、`:863` —— 删 `handleStreamError`、`handleError`（已被 `handleStreamErrorPayload` / `handleErrorPayload` 取代，全仓无调用）
- `internal/converter/response.go:61` —— 删 `ConvertOpenAIResponsesResponse`（无任何调用方）
- `handleStreamErrorPayload` 签名去掉未使用的 `cancel` 参数
- `internal/converter/converter.go:925` —— `validateResponsesTargetExtras` 的纯英文注释改写为中文（CLAUDE.md：Go 源码注释、日志主要使用中文）

**依赖 T5**：`handleStreamError` 里 timer + `timedOut` + `clientRequestCanceled` 的顺序正是 T5 重构要参照的，先删就得从 git 里捞。

**删除前必须确认无调用**：`grep -rn '<函数名>' cmd/ internal/`，确认只剩定义处。

**验收**：`go build ./...` exit=0，`go vet ./...` 无输出，`go test ./...` 全绿。

---

## 统一约束（所有任务适用）

- 注释、日志、错误信息用**中文**（CLAUDE.md 约定）
- 提交前 `gofmt -w ./cmd ./internal`
- **不新增配置字段**
- **不顺手清理无关代码** —— 除 T12 明确列出的项
- 在 `ai-gateway-dev-verify` 容器内验证，命令见 `DESIGN.md` §八
- 发现设计问题记入本目录 `KNOWN_ISSUES.md`，**不擅自改架构**
- 偏差在提交信息注明
- **默认不自动提交**，由用户拍板 `git commit`
