# 按上游格式注入全局请求扩展：设计

状态：已实现。

## 现状

- `Provider.ExtraHeaders` 会在转发请求构造时写入，并用于模型目录和健康检测；`Provider.ExtraBody` 与路由、目标的同名字段按 `provider → route → target` 浅合并。
- `forwardAttempt` 是常规模型推理的最终请求体构造点：它完成协议转换、模型名、输出预算及 OAuth 特殊处理后，序列化成上游请求。
- 视觉识别绕过 `forwardAttempt`，自行构造一个 OpenAI Chat 请求。因此只改主转发链路会留下“所有请求”语义缺口。
- 管理页的“全局设置”已通过 `/api/config` 做全量读写；前端每次保存必须明确带回新增顶层字段，否则会悄悄丢失配置。

## 提议的数据模型

新增顶层可选字段 `globalUserAgent` 与 `globalUpstreamExtras`。专用 UA 字段沿用相同格式键：

```yaml
globalUserAgent:
  anthropic: "gateway-anthropic/1.0"
  openai: "gateway-openai/1.0"
  openai-responses: "gateway-responses/1.0"
```

UA 继续单独配置，不能通过通用请求头写入。每种格式单独按 Provider UA → 全局格式 UA → 客户端 UA → Go 默认值选择。

`globalUpstreamExtras`：

```yaml
globalUpstreamExtras:
  anthropic:
    headers:
      anthropic-beta: prompt-caching-2024-07-31
    body:
      metadata:
        gateway: ai-gateway
  openai:
    headers:
      X-Gateway-Route: shared
    body:
      enable_thinking: true
  openai-responses:
    headers:
      X-Gateway-Route: shared
    body:
      reasoning:
        effort: high
```

三个键严格固定为 Provider 已有的格式值：`anthropic`、`openai`、`openai-responses`。每项都是可选的 `headers`（`map[string]string`）和 `body`（`map[string]any`）；省略、`null` 或空对象都表示该格式不追加任何内容。

实现中建议使用具名结构，而非开放字符串键的任意 map：这样 YAML 的 `KnownFields` 能拒绝拼错格式名或字段名，并且可复用既有请求头/请求体的校验函数。

## 注入与优先级

按**实际发往上游的 Provider 格式**选取规则，而不是按客户端入口格式选取。例如 Claude 客户端经转换后访问 `openai` Provider 时，使用 `globalUpstreamExtras.openai`。

全局配置是**可选的最低优先级兜底**。为保证现有行为不变，它只插在既有局部扩展写入之前，绝不调整既有层之间的相对顺序。请求体和值冲突遵循下列顺序，后者覆盖前者：

```text
全局按格式 body → Provider extraBody → Route extraBody → Target extraBody
全局按格式 headers → Provider extraHeaders
```

全局层先写入是有意设计：它为所有同格式上游提供默认字段，但不阻止 Provider、路由或目标针对特定上游覆盖同名值。不同名字段自然共存。合并仍为浅合并，嵌套对象是一个完整 JSON 值，不做递归合并。

具体的运行时落点固定如下，括号中的既有步骤不移动、不重排：

```text
请求体：协议转换／模型与输出预算 → 全局 body →（既有 Provider → Route → Target body）→（既有 OAuth 清理）
请求头：（既有协议头、认证、User-Agent）→ 全局 headers →（既有 Provider extraHeaders）
```

因此，不配置全局项时请求字节与此前一致；配置全局项时，局部层仍能按此前相同的顺序覆盖同名默认值。全局项也不会被挪到 OAuth 清理之后，以免改变其已有的请求体安全与兼容逻辑。

网关维护的字段永远优先于上述用户配置：

- 头：`Authorization`、`x-api-key`、`Content-Type`、`User-Agent` 仍由认证/协议/Provider 设置维护；
- 体：`model`、`messages`、`input`、`stream`、各类 `max_tokens` 字段仍由路由、转换器和超时/流式处理维护；
- ChatGPT OAuth 的强制流式和 Codex Responses 字段清理保持原有安全语义。

配置校验与运行时注入都直接复用现有的请求头／请求体校验和黑名单防线，避免未来增加入口时绕过验证。**不修改**现有允许传递的字段、禁止覆盖的字段、条数/长度限制或其错误语义；全局项不能用来篡改认证、模型路由或流式状态。

## 覆盖的出站请求

| 出站路径 | 使用的格式规则 | 是否纳入 |
| --- | --- | --- |
| 常规推理转发、透传、跨格式转换 | 目标 Provider 的 `Format` | 是 |
| 故障转移、本地重试、密钥切换 | 每次实际 HTTP 尝试的目标 Provider `Format` | 是 |
| ChatGPT OAuth Responses 调用 | `openai-responses` | 是 |
| 图片识别 | 其固定的 OpenAI Chat 请求 | 是，使用 `openai` |
| `/v1/models`、健康检测、余额/目录查询、表单连接探测 | GET 或独立管理协议 | 否；它们没有可兼容的模型请求体，不伪造 body |

每次请求都从当前配置快照读取对应项；热重载后下一次请求立即使用新值。不会把 map 就地写回共享配置，避免并发请求互相污染。

## 管理页体验

在“全局设置”增加“按上游格式默认追加”区块，提供三个格式卡片或选项卡。每张卡包含：

- 请求头表格（名称/值，增删行）；
- 请求体表格（字段/JSON 值，复用 Provider 弹窗现有的 JSON 值解析体验）；
- 明确说明：该格式的每个模型推理请求都会将本项作为兜底追加；同名的 Provider / 路由 / 目标项优先；
- 明确说明网关维护字段不可配置。

归一化、保存载荷和 YAML 预览都保留空值语义：未配置不生成 YAML 噪声。原始 YAML 编辑仍可直接写该顶层字段。

## 安全、兼容与迁移

- 复用现有条数、名称长度、值大小、HTTP token、控制字符和 JSON 可编码性验证上限；必要时为“每个格式项”单独报告精确路径。
- 扩展配置与现有 Provider 私有头/请求体具有同等敏感性。配置 API、原始 YAML、导入导出沿用项目既有的本机管理面和配置保管约束，不额外写入日志或指标。
- Provider/路由迁移包继续保留目标实例的全局设置，不将该顶层项混进“Provider 与路由”迁移选择范围。
- 缺省值为 nil；存量配置完全不改变出站字节。

## 验证矩阵

1. 严格 YAML/JSON：三种格式分别能保存；未知格式、未知子字段、非法头、黑名单字段、超过限制的值均按既有规则被拒绝。
2. 真实 `httptest` 上游：Anthropic、Chat、Responses 三种目标格式都观察到正确 header/body；客户端格式与上游格式不同的转换路径也正确。
3. 冲突与顺序：局部 `extraHeaders`/`extraBody` 与全局同名时，局部值保留；不同名键全部保留；既有 Provider→Route→Target、Provider headers 与 OAuth 后处理相对顺序不变；受保护字段不被覆盖。
4. 转发行为：透传、跨格式、故障转移、本地重试、密钥切换和 ChatGPT OAuth 每次 HTTP 尝试均注入。
5. 视觉识别：请求含 `openai` 全局 header/body，且没有配置时保持原请求形状。
6. 管理页：读取、可视化编辑、保存、重新加载、YAML 预览和一次无关的全量保存均不丢失字段；重建产物后 `webbuild -check` 通过。
