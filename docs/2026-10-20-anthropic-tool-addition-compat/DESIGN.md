# DESIGN.md：Anthropic 工具变更块的跨协议兼容

对应任务：`docs/2026-10-20-anthropic-tool-addition-compat/TASKS.md`

## 1. 已核实的根因

`r00148` 与 Claude Code 会话中的请求时间相差约 13 毫秒。对应 `shapeHash` 指向一份含 2 条 system 项和 15 个工具定义的请求形状。消息历史里有两条 `tool_addition`：

```json
{"type":"tool_addition","tool":{"type":"tool_reference","name":"mcp__firecrawl__firecrawl_scrape"}}
{"type":"tool_addition","tool":{"type":"tool_reference","name":"mcp__firecrawl__firecrawl_search"}}
```

两个名称都出现在顶层 `tools` 中。

Go 版 `FromAnthropic` 将 `messages` 保留在 `Internal.Messages`，并把顶层 `tools` 规范化到 `Internal.Tools`。随后 `ToOpenAIResponsesBodyChecked` 调用 `validateResponsesTargetMessages`；校验器对 `text`、`image`、`tool_use`、`tool_result` 和思考块有显式分支，其余类型会报 `unsupported openai responses target content block`。因此这是网关本地构建请求时失败，尚未到达上游。

`forwardAttempt` 对每个候选分别构建目标请求。同一错误会随每个 Responses 候选重复出现，候选循环把它记录为构建失败后继续尝试。该错误不表示多个上游分别拒绝了请求。

## 2. 转换语义

`tool_addition` 表达的是“从当前会话位置起可用的工具”，自身不包含工具参数 schema。对于 OpenAI Chat / Responses，网关把工具定义放在请求级 `tools` 字段，因此转换时：

1. 解析 `tool_addition.tool`，当前已知形态为 `tool_reference`。
2. 将引用名与 `Internal.Tools` 中的规范工具定义核对。
3. 引用存在时，不把该控制块放进目标消息内容；保留目标请求级 `tools` 中的完整定义。
4. 引用缺失或结构未知时，返回带消息位置和工具名的转换错误，不静默吞掉。

处理按块类型和引用机制通用，不依赖具体工具名。因此以后增加“工具二”只要遵循同一引用机制并带有顶层 schema，无需再加工具名特例。

未知内容块不能一概删除：它可能含有文本、图像、签名、状态或工具结果。新块类型需要一次明确的语义分类，并为目标协议定义保留、转换、省略或拒绝策略。之后该策略即可覆盖所有工具名和候选模型。

## 3. 兼容性预检与候选行为

内容兼容性依赖客户端输入与目标协议格式，不依赖同格式候选的上游地址或模型名。拟在候选转发前按唯一目标格式执行预检，并保留逐候选的现有顺序：

- 同格式透传候选跳过 canonical 转换预检，继续使用原始请求体。
- 某目标格式不兼容时，该格式候选记录为本地 `build_skip`，不发网络请求、不消耗 `maxAttempts`，也不向 breaker 报上游失败。
- 若还有兼容格式，继续尝试这些候选，故障转移规则保持原样。
- 若没有可用的兼容格式，返回一次请求级 `conversion_error`，不为相同本地错误重复构建每个候选。

实现通过共享的 `ValidateTargetCompatibility` 复用 checked body builder 的同一套校验；候选请求体构建时使用已预检格式对应的普通 builder，避免再次校验。计算某候选是否还有后续可转移目标时，也会排除预检不兼容的格式，避免把最后一个兼容候选的 429 等真实上游状态改写成网关合成错误。全格式不兼容时，请求日志保留逐候选原因，但不填写顶层 provider / upstream format / status，避免把没有发出的请求归到某个上游失败。

预检应复用 converter 中的目标消息 / extra 校验逻辑，避免预检规则与最终 body builder 出现两套口径。视觉翻译、每候选的模型名、token 预算和 provider extra body 继续在现有阶段处理。

## 4. 后续协议块的统一处理原则

不可能在不知道新块语义的情况下自动无损转换所有未来协议类型。可避免的是“每个新工具名修一次”：工具是定义和引用驱动的，转换器应按通用 `tool_addition` 语义解析。

对真正新增的内容块类型，保留显式的源块到目标能力策略：

| 分类 | 处理 |
|---|---|
| 目标等价块 | 原样或映射为目标块 |
| 可安全降级的展示占位 | 映射成文本，并有覆盖测试 |
| 请求级控制元数据 | 校验语义后从消息内容剥离，效果写入目标请求级字段 |
| 未知或有损块 | 返回明确 `conversion_error`，不得静默丢弃 |

兼容性预检让同一目标格式的问题一次被识别；分类策略让后续新增块只需在一个转换边界登记语义，不必对每个 provider / 模型逐个打补丁。

## 5. 风险与验证

- 工具可用时间点在 Anthropic 历史消息中，而 OpenAI 请求级工具表没有逐条消息的可用性时间线。该方案使用请求顶层工具列表作为本次目标请求的可用工具集合；对当前抓取到的请求，相关定义均已在该列表中。
- 如果未来 `tool_addition` 携带完整内联 schema，当前规则应明确拒绝或另行定义合并语义，不能只删块。
- 回归覆盖至少包括：引用工具已声明、引用未声明、多个不同工具名、未知块仍拒绝、Responses input 不含控制块但顶层 tools 保留、混合目标格式候选只尝试兼容候选、同格式透传不变。
