# DESIGN.md：tool_reference 降级补齐 + znrx 400 试探开关

对应 TASKS.md：`docs/2026-10-19-tool-reference-and-znrx-probe/`

---

## 一、T2：tool_reference 在 openai-responses 路径漏接降级

### 现状（已核实，非推测）

`tool_reference` 是 Anthropic 工具搜索（Tool Search）场景客户端回放的占位块，出现在 `tool_result.content` 数组里，只引用某个工具定义、不承载结果文本。

仓库里**已经有一套完整的降级实现**，但只接在了 OpenAI Chat 路径上：

- `internal/converter/converter.go:1909` `degradeToolReferences(arr []any) []any` —— 把 `tool_reference` / `tool-reference` 块换成 `{"type":"text","text": toolReferenceText(block)}`。
- `internal/converter/converter.go:1941` `toolReferenceText(block)` —— 按 `toolName` → `tool_name` → `name` 依次取第一个非空名，都没有则退回 `"[tool_reference]"`。
- `internal/converter/converter.go:1492` —— `validateOpenAIChatToolResultContent` 已放行 `tool_reference` / `tool-reference`，注释明确写着「toOpenAIContent 会把它降级成 text，转换得出来就不该在这里拦」。
- `internal/converter/converter.go:1862` —— `toOpenAIContent` 在合并文本前先调 `degradeToolReferences`。

**responses 路径没接上这套**，两处都缺：

| 位置 | 函数 | 现状 |
|---|---|---|
| 校验 | `validateResponsesToolResultContent`（`converter.go:1105-1133`） | `switch` 只有 `text` / `image`，`default` 直接报 `tool_result %s content is unsupported`（`converter.go:1129`） |
| 构建 | `responsesContentFromCanonical`（`converter.go:783-807`） | `switch` 只有 `text` / `image`，其它块**静默丢弃**（`return` 到循环下一轮，不 append） |

调用链：`ToOpenAIResponsesBodyChecked`（`converter.go:374`）→ `validateResponsesTargetMessages`（`converter.go:1056`）→ `validateResponsesToolResultContent` → 报错。

日志实证（`r00258`，`data/upstream_debug.jsonl` 同源）：
```
ar-ld / openai-responses / claude-opus-5-5  build_skip  conversion_error
error: openai responses message 15 tool_result block 1: tool_result tool_reference content is unsupported
```
`r00256` / `r00259` / `r00260` 同样命中，候选是 `shangtang`。

### 为什么必须两侧同时改

这是本任务最容易做错的地方：

- **只改校验放行**：`tool_reference` 通过了校验，但进入 `responsesContentFromCanonical` 后被静默丢弃 → 该 tool_result 变成空内容。上游看到的是「工具返回了空结果」，而不是报错。**故障从显性变隐性**，比现在更难查。
- **只改构建降级**：校验在构建之前跑，`tool_reference` 在校验处就报错返回，构建代码根本执行不到，问题依旧。

所以两处必须一起改，且修改后要验证的是**端到端产出**，不是单侧行为。

### 改动方案

**改动 1**：`validateResponsesToolResultContent` 的 `switch` 增加放行分支，文案与 chat 版本对齐：

```go
case "tool_reference", "tool-reference":
    // 与 validateOpenAIChatToolResultContent 同口径：构建阶段会降级成 text，
    // 转换得出来就不该在这里拦。工具搜索场景的客户端会在 tool_result.content
    // 里回放这种占位块。
```

**改动 2**：`responsesContentFromCanonical` 的 `switch` 增加降级分支：

```go
case "tool_reference", "tool-reference":
    out = append(out, responsesTextPart(toolReferenceText(block), role))
```

要点：
- 复用现有 `toolReferenceText`，不另写一份字段名兜底逻辑（三个字段名的兜底已在 `converter.go:1941` 收敛）。
- 用 `responsesTextPart(text, role)` 而不是手写 map，保持与同函数里 `text` 分支一致的 part 类型（`input_text` / `output_text`）。
- `responsesToolOutputFromCanonical`（`converter.go:832`）会调 `responsesContentFromCanonical(content, "user")`，降级后单块情形会被合并成字符串返回，与 chat 路径的「单块合并成字符串」行为天然对齐，无需额外改动。

### 不动的东西

- 改 `responsesContentFromCanonical` 会同时影响 `messages[].content` 的普通消息路径（`converter.go:735`、`746`）。这是**期望的**：`tool_reference` 出现在普通 content 里同样应降级而非丢弃。但要注意该函数对未知块是「静默丢弃」语义，**不要**顺手把 `default` 改成报错——那会把现有的宽容行为变成硬失败，影响面远超本任务。
- `toOpenAIContent` / chat 路径完全不动，避免回归。

### 测试

放 `internal/converter/converter_test.go`，紧邻现有 `TestToolResultToolReferenceDegradesToText`（`converter_test.go:774`）与 `TestToolResultToolReferenceMixedWithText`（`:824`）等 chat 路径测试，形成对称覆盖。

覆盖点：
- responses 路径：`tool_reference` 单块降级、与真实文本混排、`toolName` / `tool_name` / `name` / 无名字段、连字符 `tool-reference`。
- 回归锁定：`tool_reference` 以外的未知块**仍然报错**（对齐 `TestToolResultUnknownBlockStillRejected` 的意图），防止把放行做成一刀切。
- 端到端：`ToOpenAIResponsesBodyChecked` 对含 `tool_reference` 的历史不再返回 error，且产出里能找到降级文本。

---

## 二、T1：znrx 试探开关（成功请求体也落盘）

### 为什么需要它

znrx 的 400 根因**当前未坐实**，已核实并排除了多个假设：

| 假设 | 反证 |
|---|---|
| `stream=true` 导致 | `r00375` 是 `stream=false`、112 条消息，**同样 400** |
| 请求体带 `context_management` 等 Anthropic 专有字段导致 | `r00649` 是 2 条消息、`max_tokens=64`、`thinking.type=disabled`、**完全没有 `context_management`**，**也是 400** |
| 消息太长导致 | `msgs=1` 到 `msgs=616` 的样本**全部 400**，无长度阈值特征 |
| 消息太短导致 | `msgs=2`、`msgs=5` 的样本也 400 |

101 条 400 样本覆盖了上述所有组合，**唯一成功样本 `r00393` 反而拿不到请求体**——因为 `captureUpstreamBody` 只在 `upstreamStatus >= 400` 时被调用（`cmd/gateway/main.go:1452`、`:1520` 两个调用点都带 `if attemptStatus/upstreamStatus >= 400` 条件），成功请求不落盘。

没有成功样本的请求体，任何「删掉某字段再试」都是猜测，无法区分「是字段问题」还是「上游按时间/配额波动」。

### 改动方案

**改动点**：`captureUpstreamBody`（`cmd/gateway/main.go:1549`）内部增加第二个开关判断，两个调用点的 `>= 400` 条件也要相应放宽（否则函数根本收不到成功的请求）。

现状：
```go
func captureUpstreamBody(reqID, provider, targetModel string, upstreamStatus int, body []byte) {
    path := os.Getenv("AI_GATEWAY_DEBUG_UPSTREAM_BODY")
    if path == "" {
        return
    }
    ...
}
```

目标语义：
- `AI_GATEWAY_DEBUG_UPSTREAM_BODY` 非空 → **保持现状**，只抓 4xx/5xx（调用点的 `>= 400` 条件不动）。
- 新开关 `AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL` 非空 → 所有转发尝试都落盘，包括 2xx 成功。

设计要点：
- **两个开关都指向同一个路径来源**（`AI_GATEWAY_DEBUG_UPSTREAM_BODY` 的目标文件），新增开关只放宽**触发条件**，不引入第二个文件。这样新旧样本在同一份 JSONL 里，可直接 `jq` 按 `provider` + `upstreamStatus` 分组 diff。
- 调用点条件改写为「`>= 400` 或 新开关非空」。建议抽一个小 helper（如 `shouldCaptureUpstreamBody(upstreamStatus int) bool`）让两个调用点共用，避免两处各写一遍条件后将来只改一处。
- 关闭时（两个 env 都为空）：`captureUpstreamBody` 在第一次 `path == ""` 判断就返回，**零开销**，与现在完全一致。
- 成功样本落盘会让文件快速增长（每条请求体可达数百 KB）。这是调试开关，文档里必须写明「用完关闭」，并考虑在开关开启时同时打印一行醒目日志提示。

### 测试

在 `cmd/gateway/main_test.go` 现有 `AI_GATEWAY_DEBUG_UPSTREAM_BODY` 测试（`:2801`、`:2833`）旁补：
- 新开关开启 + 2xx → 落盘。
- 新开关关闭 + 2xx → 不落盘。
- 新开关关闭 + 400 → 仍落盘（原有行为不回归）。

---

## 三、T3：试探执行方法（供实施时参考）

1. 容器内设 `AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL`（指向现有 `upstream_debug.jsonl`），重启或按其加载方式生效。
2. 触发若干次 znrx `glm-5.3` 请求，**同时制造成功与失败**。已知非流式短请求（如 `r00393` 的 2 条消息、`stream=false`）更容易成功。
3. 按 `provider` + `upstreamStatus` 分组，对 2xx 与 400 的 `body` 做字段级 diff（`jq -S` 排序后 `diff`）。
4. 判读：
   - 若存在稳定的字段差异 → 该字段即嫌疑，按 T4 做定向剔除/改写。
   - 若请求体**完全相同**却一成一败 → 结论为「上游侧状态决定」，网关侧无可修点，关闭 T4，转而考虑候选排序/熔断等运行时策略。
5. 抓够样本后**立即关闭开关**。

### 风险提示

- 上游错误体是不透明的 `bad_response_status_code`，**不要**把它当作字段级诊断依据。
- znrx 配置的 `maxConcurrent: 100`，并发很高；试探期间建议单独用低并发发请求，避免压测式打上游。
- 若上游按配额/时间窗波动，样本可能需要跨多个时间点采集才有判别力。

---

## 四、T3 结论（2026-10-19 已实测完成）

### 结论

**znrx 的 400 与网关构造的请求体无关，是上游侧的时段性状态（限流/容量），网关侧无可修点。拒绝 T4。**

### 实验过程

用 dev 容器（`ai-gateway-dev`，7790）配合 `AI_GATEWAY_DEBUG_UPSTREAM_BODY_ALL` 做了逐字段单变量试探，
成功与失败样本都落了盘（`data/upstream_debug.jsonl`）。

**第一阶段：逐字段排除**（全部 1 条消息的最小请求，逐次只加一个字段）

| 试探 | 字段 | 结果 |
|---|---|---|
| A | 基线（无扩展字段） | 200 |
| B | + `stream=true` | 200 |
| C | + `output_config` | 400（但见下） |
| D | + `system` 数组 | 200 |
| E | + `context_management` | 200 |
| F | + `thinking` | 200 |
| G | + `metadata` | 200 |

C 组的 400 后来被证明是时段性抖动，不是 `output_config` 导致——同一字段组合在后续实验中稳定 200。

**第二阶段：组合复现失败**

试探 5 曾 400 过的完整字段组合（`thinking` + `output_config` + `context_management` + `system` 数组 + `stream=true`），
重新发送 **全部 200**。同一请求体一成一败，说明字段不是决定因素。

**第三阶段：控制变量对照（决定性）**

同一时段内交替发送最小请求与大请求：

| 时间 | 请求 | 结果 |
|---|---|---|
| 17:28:30–17:29:16 | 1 msg / 0 tool ×12 | **200 ×12** |
| 17:31:18–17:31:59 | 41/44/47/50 msg / 15 tool | **400 ×4** |
| **17:32:05–17:32:35** | **79 msg / 15 tool ×4** | **200 ×4** |

17:31 的 400 与 17:32 的 200 之间**请求体结构同量级、只隔几分钟**，结果翻转。
其中最干净的一组：**同一个 79msg/15tool 的请求体连续 4 次全部 200**。

另外，12 次完全相同的最小请求体连发，结果全部 200（无抖动）；而 17:26 的 12 秒窗口内
几乎每个请求都在 88–189ms 被快速拒绝——**亚秒级 400 是上游入口层拒绝的特征**，
与成功样本 0.7–13s 的推理耗时形成鲜明对比。

### 判读

- 400 的分布呈**时段聚集**（某窗口内密集出现，随后恢复正常），不随请求体内容变化。
- 拒绝速度极快（<200ms），说明在认证/准入层就被挡下，未进入推理。
- 上游错误体是不透明的 `bad_response_status_code`，不指向任何字段——与「准入层拒绝」一致。

### 对 T4 的影响

**T4 关闭，不做任何 znrx 请求体改写。** 依据：

1. 不存在「删掉某字段就能过」的修复——同一请求体可成功可失败。
2. 任何字段过滤/删除都会**损害正常时段的请求**（丢掉 `thinking` / `context_management`
   会让 GLM 的思考能力与上下文管理失效），却换不来失败时段的成功。
3. 网关侧能做的是**运行时策略**，而非请求体改写。可选方向（本次不实施，另行评估）：
   - 把 znrx 从关键路由的首候选降位，或用 `maxAttempts` 之外的健康度策略减少无效尝试；
   - 依赖现有 failover 兜底（实测 znrx 400 后转移到 muyuan 均能成功，见 `r00258` / `r00392`）。

### 实测副作用记录

- `data/upstream_debug.jsonl` 在开关开启期间增长到约 2 GB（3354+ 行，单条最大 5 MB）。
  容器 `ai-gateway-dev` 与 `ai-gateway` **共享 `./data` 挂载**，两边的记录混在同一文件，
  且**两个容器的 reqID 各自从 r00001 计数、会重复**。分析时必须用 `time` 时间窗锁定样本，
  不能只凭 `reqID`。建议后续按 provider + 时间窗过滤。
- 开关用完需从 `docker-compose.dev.yml` 移除或注释，避免持续全量落盘。

