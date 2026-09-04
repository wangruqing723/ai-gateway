// Package converter 对齐 Node 版 lib/converter.js：三种 API 格式（Anthropic / OpenAI Chat /
// OpenAI Responses）的请求/响应/流式双向转换。
//
// 内部统一用 Anthropic-like 结构表示；为贴合 Node 的动态语义，JSON 体一律用 map[string]any / []any，
// 而非强类型结构体，以最大程度保证与原实现行为等价（这是迁移的最大风险点，需对拍测试）。
package converter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DefaultMaxTokens 是客户端未传输出上限、且 target/route/provider 三层都没配时的兜底值。
//
// 优先级：target > route > provider > 客户端传入值 > 本默认值。
//
// 取 32768 而非更小值是有代价教训的：曾用 4096，思考型上游（GLM-5.2 等）光思考就能
// 烧光预算，响应在没轮到干活时就被 max_tokens 截断。Codex 把这种只含 reasoning 的
// 响应当坏流丢弃并拿同一份会话重投，表现为「同一条命令反复执行两小时」；截断点若正好
// 落在工具参数 JSON 中间，还会让整条已流出几百 KB 的 200 响应作废成 conversion_error。
const DefaultMaxTokens = 32768

// Internal 内部统一格式（Anthropic-like）
type Internal struct {
	Model     string
	Messages  []any // 每个元素为 map[string]any: {role, content}
	System    any   // string 或 nil
	Stream    bool
	MaxTokens int
	Tools     any            // 数组或 nil
	Extra     map[string]any // 原始请求体，用于透传 temperature 等
	// ToolNamespaces 是 Codex namespace 工具展平后的「扁平名 → 原始身份」映射，
	// 仅 Responses 客户端且声明了 namespace 工具时非空。响应侧据它把上游返回的
	// 扁平 function_call 名还原成 {name, namespace}，否则 Codex 认不出自己的工具。
	ToolNamespaces map[string]NamespacedName
	Err            error // 请求协议无法无损规范化时的转换错误
}

// DetectClientFormat 按端点路径识别客户端格式（对齐 Node 版）。
func DetectClientFormat(path string) string {
	p := strings.Split(path, "?")[0]
	switch {
	case strings.HasSuffix(p, "/v1/messages"):
		return "anthropic"
	case strings.HasSuffix(p, "/v1/chat/completions"):
		return "openai-chat"
	case strings.HasSuffix(p, "/v1/responses"):
		return "openai-responses"
	}
	return ""
}

// IsPassthrough 判断 provider 和 client 格式是否可直接透传（不需逐行转换）。
// 供 proxy 和 stream 模块共用，消除判定逻辑分歧。
func IsPassthrough(providerFormat, clientFormat string) bool {
	return providerFormat == clientFormat ||
		(providerFormat == "openai" && clientFormat == "openai-chat") ||
		(providerFormat == "openai-responses" && clientFormat == "openai-responses")
}

// ── 客户端请求 → 内部格式 ──────────────────────

// FromAnthropic Anthropic 请求 → 内部格式（几乎原样保留）。
func FromAnthropic(body map[string]any) *Internal {
	messages := getSlice(body, "messages")
	tools, toolsErr := normalizeAnthropicTools(body["tools"])
	return &Internal{
		Model:     getString(body, "model"),
		Messages:  messages,
		System:    normalizeSystem(body["system"]),
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_tokens", DefaultMaxTokens),
		Tools:     tools,
		Extra:     body,
		Err:       errors.Join(toolsErr, validateAnthropicMessages(messages)),
	}
}

// FromOpenAIChat OpenAI Chat 请求 → 内部格式。
func FromOpenAIChat(body map[string]any) *Internal {
	var messages []any
	var systemParts []string
	var conversionErr error

	for _, m := range getSlice(body, "messages") {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role := getString(msg, "role")
		if role == "system" {
			if text := systemTextFromContent(msg["content"]); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		if role == "tool" {
			appendCanonicalBlocks(&messages, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": getString(msg, "tool_call_id"),
				"content":     normalizeToolResultContent(msg["content"]),
			}})
			continue
		}

		content := normalizeOpenAIContent(msg["content"])
		if role == "assistant" {
			toolUses, err := chatToolUses(msg["tool_calls"])
			if err != nil {
				conversionErr = errors.Join(conversionErr, err)
			} else {
				content = append(content, toolUses...)
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	tools, toolsErr := normalizeOpenAIChatTools(body["tools"])

	return &Internal{
		Model:     getString(body, "model"),
		Messages:  messages,
		System:    normalizeSystem(strings.Join(systemParts, "\n")),
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_tokens", DefaultMaxTokens),
		Tools:     tools,
		Extra:     body,
		Err:       errors.Join(conversionErr, toolsErr),
	}
}

// FromOpenAIResponses OpenAI Responses 请求 → 内部格式。
func FromOpenAIResponses(body map[string]any) *Internal {
	var input []any
	switch v := body["input"].(type) {
	case []any:
		input = v
	case string:
		input = []any{map[string]any{"role": "user", "content": v}}
	}

	// tools 必须先规范化：下面回放历史里的 function_call 要用它产出的 namespace
	// 映射把 {name, namespace} 改写成上游认识的扁平名。
	tools, owners, toolsErr := normalizeResponsesTools(body["tools"])

	var messages []any
	var conversionErr error
	for _, it := range input {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		switch getString(item, "type") {
		case "function_call":
			input, err := parseFunctionArguments(item["arguments"])
			if err != nil {
				conversionErr = errors.Join(conversionErr, fmt.Errorf("responses function call %q arguments: %w", getString(item, "call_id"), err))
			}
			// 带 namespace 的历史调用改用扁平名：上游收到的工具列表已被展平，
			// 保留客户端那套裸名会让历史调用与工具声明对不上。
			name := getString(item, "name")
			if namespace := strings.TrimSpace(getString(item, "namespace")); namespace != "" {
				flat := flattenNamespaceToolName(namespace, strings.TrimSpace(name))
				if entry, exists := owners[flat]; exists && entry.Namespace == namespace {
					name = flat
				}
			}
			appendCanonicalBlocks(&messages, "assistant", []any{map[string]any{
				"type":  "tool_use",
				"id":    getString(item, "call_id"),
				"name":  name,
				"input": input,
			}})
		case "function_call_output":
			content, err := normalizeResponsesToolOutput(item["output"])
			if err != nil {
				conversionErr = errors.Join(conversionErr, fmt.Errorf("responses function output %q: %w", getString(item, "call_id"), err))
			}
			appendCanonicalBlocks(&messages, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": getString(item, "call_id"),
				"content":     content,
			}})
		case "reasoning":
			// 丢弃上一轮的推理项，与 stripReasoningBlocks 处理 Anthropic thinking
			// 历史的口径一致：本轮是否思考由 reasoning_effort 决定，与历史推理无关，
			// 真正承载语义的 output_text 与 function_call 都在别的 item 里完整保留。
			//
			// 必须放行而不是报错：网关自己会向 Responses 客户端产出 reasoning item
			// （见 anthropicToResponses 与 responsesToChat 的对称处理），透传场景下
			// 上游的 reasoning item 也会原样到达客户端。客户端按协议回传完整对话
			// 历史，报错等于任何带推理的多轮会话在第二轮必然 400。
		default:
			if item["type"] != "message" && item["role"] == nil {
				conversionErr = errors.Join(conversionErr, fmt.Errorf("unsupported responses input item %q", getString(item, "type")))
				continue
			}
			messages = append(messages, map[string]any{
				"role":    item["role"],
				"content": normalizeResponsesContent(item["content"]),
			})
		}
	}

	return &Internal{
		Model:          getString(body, "model"),
		Messages:       messages,
		System:         normalizeSystem(body["instructions"]),
		Stream:         getBool(body, "stream"),
		MaxTokens:      getIntDefault(body, "max_output_tokens", DefaultMaxTokens),
		Tools:          tools,
		ToolNamespaces: owners,
		Extra:          body,
		Err:            errors.Join(conversionErr, toolsErr),
	}
}

// ── 内部格式 → 上游 Provider 请求体 ───────────

// ToAnthropicBody 内部格式 → Anthropic 请求体。
func ToAnthropicBody(in *Internal, targetModel string) map[string]any {
	messages, system := hoistInstructionMessages(in.Messages, in.System)
	body := map[string]any{
		"model":      targetModel,
		"messages":   messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if system != nil && system != "" {
		body["system"] = system
	}
	if in.Tools != nil {
		body["tools"] = in.Tools
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "stop_sequences", "metadata"} {
		if v, ok := in.Extra[k]; ok {
			body[k] = v
		}
	}
	// tool_choice 只在确实带了 tools 时才转发。Anthropic 对「有 tool_choice 无 tools」
	// 返回 400 且不可重试（"tool_choice may only be specified while providing tools"），
	// 而一个只声明了 web_search 之类内置工具的请求，规范化后 tools 会是空。
	if in.Tools != nil {
		if toolChoice, exists := in.Extra["tool_choice"]; exists && toolChoice != nil {
			if mapped, err := toolChoiceToAnthropic(toolChoice); err == nil && mapped != nil {
				body["tool_choice"] = mapped
			}
		}
		if parallel, exists := in.Extra["parallel_tool_calls"]; exists && parallel == false {
			toolChoice, _ := body["tool_choice"].(map[string]any)
			if toolChoice == nil {
				toolChoice = map[string]any{"type": "auto"}
				body["tool_choice"] = toolChoice
			}
			toolChoice["disable_parallel_tool_use"] = true
		}
	}
	return body
}

// ToAnthropicBodyChecked 在生成 Anthropic 请求体前检查 normalization 与目标协议约束。
func ToAnthropicBodyChecked(in *Internal, targetModel string) (map[string]any, error) {
	if in == nil {
		return nil, fmt.Errorf("internal request is nil")
	}
	if in.Err != nil {
		return nil, fmt.Errorf("request normalization failed: %w", in.Err)
	}
	if err := validateAnthropicTargetMessages(in.Messages); err != nil {
		return nil, err
	}
	if err := validateAnthropicTargetExtras(in.Extra); err != nil {
		return nil, err
	}
	return ToAnthropicBody(in, targetModel), nil
}

// ToOpenAIChatBody 内部格式 → OpenAI Chat 请求体。
func ToOpenAIChatBody(in *Internal, targetModel string) map[string]any {
	var messages []any
	// Chat 官方认 developer，但第三方兼容入口对它的支持不一致，而 system 是通用的；
	// 归一到 system 语义不丢、兼容面更宽。
	canonicalMessages, canonicalSystem := hoistInstructionMessages(in.Messages, in.System)
	if s := systemTextFromContent(canonicalSystem); s != "" {
		messages = append(messages, map[string]any{"role": "system", "content": s})
	}
	for _, m := range canonicalMessages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		messages = append(messages, canonicalMessageToOpenAIChat(msg)...)
	}

	body := map[string]any{
		"model":      targetModel,
		"messages":   messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.Tools != nil {
		body["tools"] = canonicalToolsToOpenAIChat(in.Tools)
	}
	for _, k := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty"} {
		if v, ok := in.Extra[k]; ok {
			body[k] = v
		}
	}
	if stopSequences, ok := in.Extra["stop_sequences"]; ok {
		body["stop"] = stopSequences
	}
	if format, err := responsesTextFormatToChat(in.Extra["text"]); err == nil && format != nil {
		body["response_format"] = format
	}
	if effort, ok := responsesReasoningEffort(in.Extra["reasoning"]); ok {
		body["reasoning_effort"] = effort
	} else if effort, ok := in.Extra["reasoning_effort"]; ok {
		body["reasoning_effort"] = effort
	}
	return body
}

// ToOpenAIChatBodyChecked 在生成 Chat 请求体前检查 normalization 与目标协议约束。
func ToOpenAIChatBodyChecked(in *Internal, targetModel string) (map[string]any, error) {
	if in == nil {
		return nil, fmt.Errorf("internal request is nil")
	}
	if in.Err != nil {
		return nil, fmt.Errorf("request normalization failed: %w", in.Err)
	}
	if err := validateOpenAIChatTargetMessages(in.Messages); err != nil {
		return nil, err
	}
	if err := validateOpenAIChatTargetExtras(in.Extra); err != nil {
		return nil, err
	}
	return ToOpenAIChatBody(in, targetModel), nil
}

// ToOpenAIResponsesBody 内部格式 → OpenAI Responses 请求体。
// 这里只生成 Responses 原生的 input/items 结构，不把 Chat 的 function 工具
// 直接塞进 tools；两种协议的工具定义和工具调用项虽然语义相近，JSON 结构并不相同。
func ToOpenAIResponsesBody(in *Internal, targetModel string) map[string]any {
	body := map[string]any{
		"model":             targetModel,
		"input":             responsesInputFromMessages(in.Messages),
		"max_output_tokens": in.MaxTokens,
		"stream":            in.Stream,
	}
	if system := systemTextFromContent(in.System); system != "" {
		body["instructions"] = system
	}
	if in.Tools != nil {
		body["tools"] = canonicalToolsToOpenAIResponses(in.Tools)
	}
	copyResponsesCompatibleExtras(body, in.Extra)
	// 同 ToAnthropicBody：工具被规范化掉之后不留悬空 tool_choice。
	if in.Tools != nil {
		if toolChoice, exists := in.Extra["tool_choice"]; exists && toolChoice != nil {
			if mapped, err := toolChoiceToResponses(toolChoice); err == nil && mapped != nil {
				body["tool_choice"] = mapped
			}
		}
	}
	return body
}

// ToOpenAIResponsesBodyChecked 在生成 Responses 请求体前检查规范化结果和
// Responses 专属字段的兼容边界。跨协议转换宁可返回 conversion_error，也不静默丢掉
// 会改变会话状态或工具语义的字段。
func ToOpenAIResponsesBodyChecked(in *Internal, targetModel string) (map[string]any, error) {
	if in == nil {
		return nil, fmt.Errorf("internal request is nil")
	}
	if in.Err != nil {
		return nil, fmt.Errorf("request normalization failed: %w", in.Err)
	}
	if err := validateResponsesTargetMessages(in.Messages); err != nil {
		return nil, err
	}
	if err := validateResponsesTargetExtras(in.Extra); err != nil {
		return nil, err
	}
	return ToOpenAIResponsesBody(in, targetModel), nil
}

// OverrideMaxTokens 按 provider 格式改写上游请求体里的输出上限字段。
//
// 硬覆盖客户端传入值，不取 min：配置里写的是「这个上游该输出多少」，
// 客户端（尤其 Codex / Claude Code 这类 CLI）往往根本不传该字段，
// 取 min 会让配置在客户端传了个更小值时形同虚设。
//
// 透传路径同样要调用：anthropic→anthropic 走的是客户端原始 body 浅拷贝，
// 不经过 ToXxxBody，漏掉这里会让该路径完全不受配置约束，两类请求行为分裂。
//
// OpenAI Chat 的字段名有两种：新版模型只认 max_completion_tokens，旧版只认
// max_tokens。这里只改写客户端已经带上的那个键，两个都没带时按旧版补
// max_tokens——同时塞两个键会被部分上游判为互斥字段冲突而 400。
func OverrideMaxTokens(body map[string]any, providerFormat string, maxTokens int) {
	if body == nil || maxTokens <= 0 {
		return
	}
	switch providerFormat {
	case "openai-responses":
		body["max_output_tokens"] = maxTokens
	case "openai":
		if _, exists := body["max_completion_tokens"]; exists {
			body["max_completion_tokens"] = maxTokens
			return
		}
		body["max_tokens"] = maxTokens
	default:
		body["max_tokens"] = maxTokens
	}
}

// EnsureMaxTokens 在输出上限字段**完全缺失**时补上 DefaultMaxTokens，字段已存在则不动。
//
// 这是优先级链的最后一环（客户端传入值 > 本默认值）：三层配置都没配时调用它，
// 客户端传了就保留客户端的值，一个都没有才落到默认值。
//
// 只有透传路径真正需要它。跨格式路径的 ToXxxBody 无条件写入 in.MaxTokens
// （已含 getIntDefault 的默认值），字段必然存在，这里恒为空操作；而透传是客户端原始
// body 的浅拷贝，客户端没传就真的没有该字段——Anthropic 的 max_tokens 是必填，
// 缺了上游直接 400，那条路径会变成「只要客户端不传就必失败」。
//
// openai-chat 的两个键任一存在即视为已配：补第二个会被部分上游判为互斥字段冲突。
func EnsureMaxTokens(body map[string]any, providerFormat string) {
	if body == nil {
		return
	}
	switch providerFormat {
	case "openai-responses":
		if _, exists := body["max_output_tokens"]; exists {
			return
		}
		body["max_output_tokens"] = DefaultMaxTokens
	case "openai":
		if _, exists := body["max_completion_tokens"]; exists {
			return
		}
		if _, exists := body["max_tokens"]; exists {
			return
		}
		body["max_tokens"] = DefaultMaxTokens
	default:
		if _, exists := body["max_tokens"]; exists {
			return
		}
		body["max_tokens"] = DefaultMaxTokens
	}
}

func normalizeSystem(system any) any {
	text := systemTextFromContent(system)
	if text == "" {
		return nil
	}
	return text
}

func normalizeAnthropicTools(tools any) (any, error) {
	if tools == nil {
		return nil, nil
	}
	arr, ok := tools.([]any)
	if !ok {
		return nil, fmt.Errorf("anthropic tools must be an array")
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported anthropic tool definition %T", value)
		}
		if toolType := getString(tool, "type"); toolType != "" {
			return nil, fmt.Errorf("unsupported anthropic tool type %q", toolType)
		}
		canonical := map[string]any{"name": tool["name"]}
		if description, exists := tool["description"]; exists {
			canonical["description"] = description
		}
		if schema, exists := tool["input_schema"]; exists {
			canonical["input_schema"] = schema
		}
		out = append(out, canonical)
	}
	return out, nil
}

func normalizeOpenAIChatTools(tools any) (any, error) {
	if tools == nil {
		return nil, nil
	}
	arr, ok := tools.([]any)
	if !ok {
		return nil, fmt.Errorf("openai chat tools must be an array")
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported openai chat tool definition %T", value)
		}
		if toolType := getString(tool, "type"); toolType != "function" {
			return nil, fmt.Errorf("unsupported openai chat tool type %q", toolType)
		}
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			return nil, fmt.Errorf("openai chat function tool is missing function definition")
		}
		canonical := map[string]any{"name": function["name"]}
		if description, exists := function["description"]; exists {
			canonical["description"] = description
		}
		if parameters, exists := function["parameters"]; exists {
			canonical["input_schema"] = parameters
		}
		if strict, exists := function["strict"]; exists {
			canonical["strict"] = strict
		}
		out = append(out, canonical)
	}
	return out, nil
}

// normalizeResponsesTools 把 Responses 的 tools 规范化成内部（Anthropic-like）形状。
//
// 返回规范化后的工具、namespace 还原映射、错误。
//
// 对未知 tool type 的处理是**丢弃而非报错**：Codex CLI 会声明 web_search 这类由
// OpenAI 服务端执行的内置工具，第三方上游既不认也无法代为执行。报错会让整个请求
// 400（实测 Codex 每一轮都发 web_search，等于完全不可用），丢掉则只损失该工具本身，
// 其余 8 个 function 工具照常可用。这也是 cc-switch 的取舍。
func normalizeResponsesTools(tools any) (any, map[string]NamespacedName, error) {
	if tools == nil {
		return nil, nil, nil
	}
	arr, ok := tools.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("responses tools must be an array")
	}
	// 先展平 namespace，后续按普通工具处理。
	flattened, owners, err := flattenNamespaceTools(arr)
	if err != nil {
		return nil, nil, err
	}
	out := make([]any, 0, len(flattened))
	for _, value := range flattened {
		tool, ok := value.(map[string]any)
		if !ok {
			// 非对象条目丢弃：无法从中读出工具名，留着也没法转换。
			continue
		}
		switch getString(tool, "type") {
		case "function", "custom":
			// custom 工具是自由文本入参的变体，没有 JSON Schema。按无参 function
			// 转换，保住工具本身可被调用，而不是整条请求失败。
		default:
			// web_search / tool_search / image_generation / local_shell 等服务端内置
			// 工具：上游无法代为执行，丢弃。
			continue
		}
		name := strings.TrimSpace(getString(tool, "name"))
		if name == "" {
			continue
		}
		canonical := map[string]any{"name": name}
		if description, exists := tool["description"]; exists {
			canonical["description"] = description
		}
		if parameters, exists := tool["parameters"]; exists {
			canonical["input_schema"] = parameters
		}
		if strict, exists := tool["strict"]; exists {
			canonical["strict"] = strict
		}
		out = append(out, canonical)
	}
	if len(out) == 0 {
		// 工具全被丢弃：返回 nil 而不是空数组。空数组会让 ToAnthropicBody 写出
		// "tools": []，而 Anthropic 在有 tool_choice 时会 400（且不可重试）。
		return nil, owners, nil
	}
	return out, owners, nil
}

func canonicalToolsToOpenAIChat(tools any) any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		function := map[string]any{"name": tool["name"]}
		if description, exists := tool["description"]; exists {
			function["description"] = description
		}
		if schema, exists := tool["input_schema"]; exists {
			function["parameters"] = schema
		}
		if strict, exists := tool["strict"]; exists {
			function["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

// canonicalToolsToOpenAIResponses 将内部统一的 function 工具定义还原为
// Responses API 的扁平结构。Chat Completions 的 function 位于 function 字段内，
// 不能复用其 JSON 结构。
func canonicalToolsToOpenAIResponses(tools any) any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		function := map[string]any{"type": "function", "name": tool["name"]}
		if description, exists := tool["description"]; exists {
			function["description"] = description
		}
		if schema, exists := tool["input_schema"]; exists {
			function["parameters"] = schema
		}
		if strict, exists := tool["strict"]; exists {
			function["strict"] = strict
		}
		out = append(out, function)
	}
	return out
}

// responsesInputFromMessages 将内部消息按 Responses input 的 item 语义展开。
// tool_use 和 tool_result 都是独立 item，不能混入 message.content。
func responsesInputFromMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		// Responses 用 developer 表达 system 语义；思考块跨厂商无意义，一并丢掉。
		role := responsesRole(getString(message, "role"))
		blocks, ok := message["content"].([]any)
		if !ok {
			out = append(out, map[string]any{
				"type": "message", "role": role,
				"content": responsesContentFromCanonical(message["content"], role),
			})
			continue
		}
		blocks = stripReasoningBlocks(blocks)

		var ordinary []any
		flushOrdinary := func() {
			if len(ordinary) == 0 {
				return
			}
			out = append(out, map[string]any{
				"type": "message", "role": role,
				"content": responsesContentFromCanonical(ordinary, role),
			})
			ordinary = nil
		}
		for _, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				ordinary = append(ordinary, blockValue)
				continue
			}
			switch getString(block, "type") {
			case "tool_use":
				flushOrdinary()
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   getString(block, "id"),
					"name":      getString(block, "name"),
					"arguments": marshalFunctionArguments(block["input"]),
				})
			case "tool_result":
				flushOrdinary()
				out = append(out, map[string]any{
					"type":    "function_call_output",
					"call_id": getString(block, "tool_use_id"),
					"output":  responsesToolOutputFromCanonical(block["content"]),
				})
			default:
				ordinary = append(ordinary, block)
			}
		}
		flushOrdinary()
	}
	return out
}

func responsesContentFromCanonical(content any, role string) []any {
	if text, ok := content.(string); ok {
		return []any{responsesTextPart(text, role)}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(blocks))
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch getString(block, "type") {
		case "text":
			out = append(out, responsesTextPart(getString(block, "text"), role))
		case "image":
			if image := canonicalImageToResponses(block); image != nil {
				out = append(out, image)
			}
		}
	}
	return out
}

func responsesTextPart(text, role string) map[string]any {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	return map[string]any{"type": partType, "text": text}
}

func canonicalImageToResponses(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	imageURL := getString(source, "url")
	if getString(source, "type") == "base64" {
		imageURL = "data:" + getString(source, "media_type") + ";base64," + getString(source, "data")
	}
	if imageURL == "" {
		return nil
	}
	return map[string]any{"type": "input_image", "image_url": imageURL}
}

func responsesToolOutputFromCanonical(content any) any {
	if text, ok := content.(string); ok {
		return text
	}
	parts := responsesContentFromCanonical(content, "user")
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok && getString(part, "type") == "input_text" {
			return getString(part, "text")
		}
	}
	return parts
}

func chatToolUses(toolCalls any) ([]any, error) {
	if toolCalls == nil {
		return nil, nil
	}
	arr, ok := toolCalls.([]any)
	if !ok {
		return nil, fmt.Errorf("openai chat tool_calls must be an array")
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		call, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported openai chat tool call %T", value)
		}
		function, _ := call["function"].(map[string]any)
		input, err := parseFunctionArguments(function["arguments"])
		if err != nil {
			return nil, fmt.Errorf("openai chat tool call %q arguments: %w", getString(call, "id"), err)
		}
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    getString(call, "id"),
			"name":  getString(function, "name"),
			"input": input,
		})
	}
	return out, nil
}

func parseFunctionArguments(arguments any) (map[string]any, error) {
	switch value := arguments.(type) {
	case map[string]any:
		return value, nil
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w; arguments must be a JSON object", err)
		}
		object, ok := parsed.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("arguments must be a JSON object, got %T", parsed)
		}
		return object, nil
	}
	return nil, fmt.Errorf("arguments must be a JSON object, got %T", arguments)
}

func marshalFunctionArguments(input any) string {
	if input == nil {
		return "{}"
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func normalizeToolResultContent(content any) any {
	if arr, ok := content.([]any); ok {
		return normalizeOpenAIContent(arr)
	}
	return content
}

func normalizeResponsesToolOutput(content any) (any, error) {
	if content == nil {
		return nil, nil
	}
	if text, ok := content.(string); ok {
		return text, nil
	}
	arr, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("function output must be a string or content array, got %T", content)
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		block, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported function output block %T", value)
		}
		switch blockType := getString(block, "type"); blockType {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		case "input_image":
			if fileID, exists := block["file_id"]; exists && fileID != nil {
				return nil, fmt.Errorf("unsupported responses input_image file_id %q", getString(block, "file_id"))
			}
			if block["image_url"] == nil && block["url"] == nil {
				return nil, fmt.Errorf("responses input_image requires image_url or url")
			}
			out = append(out, responsesImageToAnthropic(block))
		default:
			return nil, fmt.Errorf("unsupported responses function output block %q", blockType)
		}
	}
	return out, nil
}

func validateAnthropicMessages(messages []any) error {
	var conversionErr error
	for messageIndex, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		blocks, _ := message["content"].([]any)
		ordinaryBeforeResult := false
		for blockIndex, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				ordinaryBeforeResult = true
				continue
			}
			switch getString(block, "type") {
			case "tool_use":
				if _, ok := block["input"].(map[string]any); !ok {
					conversionErr = errors.Join(conversionErr, fmt.Errorf("anthropic message %d tool_use %d arguments must be a JSON object", messageIndex, blockIndex))
				}
			case "tool_result":
				if ordinaryBeforeResult {
					conversionErr = errors.Join(conversionErr, fmt.Errorf("anthropic message %d tool_result order cannot be represented in OpenAI Chat", messageIndex))
				}
			default:
				ordinaryBeforeResult = true
			}
		}
	}
	return conversionErr
}

func validateAnthropicTargetMessages(messages []any) error {
	for messageIndex, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("anthropic message %d must be an object", messageIndex)
		}
		// system / developer 由 hoistInstructionMessages 在构建阶段提到顶层 system，
		// 这里放行。其余角色 Anthropic 一律不认，且它返回的 400 不指向具体字段，
		// 与其把请求送出去换一个无从反查的上游错误，不如在构建前直接失败。
		if role := getString(message, "role"); role != "" && role != "user" && role != "assistant" && !isInstructionRole(role) {
			return fmt.Errorf("unsupported anthropic target message role %q", role)
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for blockIndex, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				return fmt.Errorf("anthropic message %d content block %d must be an object", messageIndex, blockIndex)
			}
			switch getString(block, "type") {
			// 目标同为 Anthropic 时思考块原样保留（signature 能验签），校验器必须认它。
			case "text", "image", "tool_result", "thinking", "redacted_thinking":
			case "tool_use":
				if _, ok := block["input"].(map[string]any); !ok {
					return fmt.Errorf("anthropic message %d tool_use arguments must be a JSON object", messageIndex)
				}
			default:
				return fmt.Errorf("unsupported anthropic target content block %q", getString(block, "type"))
			}
		}
	}
	return nil
}

func validateOpenAIChatTargetMessages(messages []any) error {
	for messageIndex, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("openai chat message %d must be an object", messageIndex)
		}
		// 与 Anthropic 目标同理：system / developer 会被 hoistInstructionMessages
		// 归一成顶层 system 消息，其余角色 Chat 不认。
		if role := getString(message, "role"); role != "" && role != "user" && role != "assistant" && !isInstructionRole(role) {
			return fmt.Errorf("unsupported openai chat target message role %q", role)
		}
		blocks, _ := message["content"].([]any)
		for blockIndex, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				return fmt.Errorf("openai chat message %d content block %d must be an object", messageIndex, blockIndex)
			}
			switch getString(block, "type") {
			case "text":
			// 思考块在 canonicalMessageToOpenAIChat 里会被丢弃，这里必须放行：
			// 校验跑在构建之前，不放行的话 Claude Code 的多轮历史永远转不过去。
			case "thinking", "redacted_thinking":
			case "image":
				source, _ := block["source"].(map[string]any)
				if source == nil || (getString(source, "url") == "" && getString(source, "data") == "") {
					return fmt.Errorf("openai chat message %d image block %d requires URL or base64 source", messageIndex, blockIndex)
				}
			case "tool_use":
				if _, ok := block["input"].(map[string]any); !ok {
					return fmt.Errorf("openai chat message %d tool_use arguments must be a JSON object", messageIndex)
				}
			case "tool_result":
				if err := validateOpenAIChatToolResultContent(block["content"]); err != nil {
					return fmt.Errorf("openai chat message %d tool_result block %d: %w", messageIndex, blockIndex, err)
				}
			default:
				return fmt.Errorf("unsupported openai chat target content block %q", getString(block, "type"))
			}
		}
	}
	return nil
}

func validateResponsesTargetMessages(messages []any) error {
	for messageIndex, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("openai responses message %d must be an object", messageIndex)
		}
		// system 由 responsesRole 映射成 developer，这里放行；映射发生在构建阶段。
		role := getString(message, "role")
		if role != "user" && role != "assistant" && role != "developer" && role != "system" {
			return fmt.Errorf("unsupported openai responses message role %q", role)
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			if _, ok := message["content"].(string); ok || message["content"] == nil {
				continue
			}
			return fmt.Errorf("openai responses message %d content must be text or an array", messageIndex)
		}
		for blockIndex, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				return fmt.Errorf("openai responses message %d content block %d must be an object", messageIndex, blockIndex)
			}
			switch getString(block, "type") {
			case "text":
			// 思考块在 responsesInputFromMessages 里会被丢弃，这里必须放行：
			// 校验跑在构建之前，不放行的话 Claude Code 的多轮历史永远转不过去。
			case "thinking", "redacted_thinking":
			case "image":
				source, _ := block["source"].(map[string]any)
				if source == nil || (getString(source, "url") == "" && getString(source, "data") == "") {
					return fmt.Errorf("openai responses message %d image block %d requires URL or base64 source", messageIndex, blockIndex)
				}
			case "tool_use":
				if _, ok := block["input"].(map[string]any); !ok {
					return fmt.Errorf("openai responses message %d tool_use arguments must be a JSON object", messageIndex)
				}
			case "tool_result":
				if err := validateResponsesToolResultContent(block["content"]); err != nil {
					return fmt.Errorf("openai responses message %d tool_result block %d: %w", messageIndex, blockIndex, err)
				}
			default:
				return fmt.Errorf("unsupported openai responses target content block %q", getString(block, "type"))
			}
		}
	}
	return nil
}

func validateResponsesToolResultContent(content any) error {
	if content == nil {
		return nil
	}
	if _, ok := content.(string); ok {
		return nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return fmt.Errorf("content must be text or a supported content array, got %T", content)
	}
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("content block must be an object, got %T", value)
		}
		switch getString(block, "type") {
		case "text":
		case "image":
			source, _ := block["source"].(map[string]any)
			if source == nil || (getString(source, "url") == "" && getString(source, "data") == "") {
				return fmt.Errorf("image content requires URL or base64 source")
			}
		default:
			return fmt.Errorf("tool_result %s content is unsupported", getString(block, "type"))
		}
	}
	return nil
}

// isMeaningfulExtra 判断额外字段是否真的改变了请求语义。
func isMeaningfulExtra(key string, value any) bool {
	if value == nil {
		return false
	}
	switch key {
	case "store":
		v, ok := value.(bool)
		return !ok || v
	case "n":
		switch v := value.(type) {
		case int:
			return v != 1
		case float64:
			return v != 1
		default:
			return true
		}
	case "parallel_tool_calls":
		v, ok := value.(bool)
		return !ok || !v
	case "stream_options":
		options, ok := value.(map[string]any)
		return !ok || len(options) != 1 || options["include_usage"] != true
	default:
		return true
	}
}

// validateOpenAIChatTargetExtras 拒绝会被 Chat Completions 静默丢失的
// Responses 会话状态和原生扩展。已经明确可转换的 text.format、reasoning.effort
// 会在 ToOpenAIChatBody 中映射为 Chat 字段。
func validateOpenAIChatTargetExtras(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	for _, key := range []string{
		"previous_response_id", "store", "conversation", "background", "approval_request_id",
		"logit_bias", "prediction", "modalities", "audio", "n", "seed", "logprobs", "top_logprobs",
	} {
		if value, exists := extra[key]; exists && isMeaningfulExtra(key, value) {
			return fmt.Errorf("无法将 Responses 字段 %q 转换为 OpenAI Chat Completions，避免丢失语义", key)
		}
	}
	if reasoning, exists := extra["reasoning"]; exists && reasoning != nil {
		if err := validateReasoningExtra(reasoning, "OpenAI Chat Completions"); err != nil {
			return err
		}
	}
	if text, exists := extra["text"]; exists && text != nil {
		if _, err := responsesTextFormatToChat(text); err != nil {
			return err
		}
	}
	return nil
}

// validateAnthropicTargetExtras 防止将 OpenAI Chat/Responses 的会话状态、结构化
// 输出或推理配置静默丢弃。Anthropic 的 temperature/top_p/top_k/stop_sequences/
// metadata 在 ToAnthropicBody 中有明确映射，因此不在拒绝名单中。
func validateAnthropicTargetExtras(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	for _, key := range []string{
		"previous_response_id", "store", "conversation", "background", "approval_request_id",
		"logit_bias", "prediction", "modalities", "audio", "n", "seed", "logprobs", "top_logprobs",
	} {
		if value, exists := extra[key]; exists && isMeaningfulExtra(key, value) {
			return fmt.Errorf("无法将 OpenAI 字段 %q 转换为 Anthropic Messages，避免丢失语义", key)
		}
	}
	if reasoning, exists := extra["reasoning"]; exists && reasoning != nil {
		if err := validateReasoningExtra(reasoning, "Anthropic Messages"); err != nil {
			return err
		}
	}
	if toolChoice, exists := extra["tool_choice"]; exists && toolChoice != nil {
		if _, err := toolChoiceToAnthropic(toolChoice); err != nil {
			return err
		}
	}
	return nil
}

// validateResponsesTargetExtras 校验 Anthropic/Chat 规范化后无法表达的字段。
// Responses 原生状态允许保留，因为直通请求在视觉翻译后仍可能需要重建请求体。
//
// stop_sequences 不在此名单：Responses API 无 stop 等价字段、无法映射，但它是
// 次要采样参数（与 top_k 同级），且 max_output_tokens 已兜住响应长度，丢掉不会
// 失控。原先拒绝会让带 stop_sequences 的请求（如 Claude Code）在 Responses 格式
// 候选上必然 build_error、整条候选作废——这比丢一个采样参数代价大得多，所以改为
// 在 copyResponsesCompatibleExtras 里静默不拷贝（与 top_k 处理一致）。
func validateResponsesTargetExtras(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	for _, key := range []string{"logprobs", "top_logprobs", "n", "seed", "logit_bias", "prediction", "modalities", "audio"} {
		if value, exists := extra[key]; exists && isMeaningfulExtra(key, value) {
			return fmt.Errorf("无法将 OpenAI Chat 字段 %q 转换为 OpenAI Responses，避免丢失语义", key)
		}
	}
	if responseFormat, exists := extra["response_format"]; exists && responseFormat != nil {
		if _, err := chatResponseFormatToResponses(responseFormat); err != nil {
			return err
		}
	}
	if toolChoice, exists := extra["tool_choice"]; exists && toolChoice != nil {
		if _, err := toolChoiceToResponses(toolChoice); err != nil {
			return err
		}
	}
	return nil
}

func validateReasoningExtra(reasoning any, target string) error {
	r, ok := reasoning.(map[string]any)
	if !ok {
		return fmt.Errorf("Responses reasoning 必须是对象")
	}
	if value, exists := r["encrypted_content"]; exists && isMeaningfulExtra("reasoning.encrypted_content", value) {
		return fmt.Errorf("无法将 Responses reasoning.encrypted_content 转换为 %s，避免丢失语义", target)
	}
	return nil
}

func copyResponsesCompatibleExtras(body, extra map[string]any) {
	if extra == nil {
		return
	}
	for _, key := range []string{
		"temperature", "top_p", "metadata", "parallel_tool_calls",
		"store", "previous_response_id", "conversation", "truncation", "background",
		"include", "prompt", "reasoning", "service_tier", "safety_identifier",
	} {
		if value, exists := extra[key]; exists && value != nil {
			body[key] = value
		}
	}
	if responseFormat, exists := extra["response_format"]; exists && responseFormat != nil {
		if format, err := chatResponseFormatToResponses(responseFormat); err == nil && format != nil {
			body["text"] = map[string]any{"format": format}
		}
	}
	if effort, exists := extra["reasoning_effort"]; exists && effort != nil {
		if reasoning, _ := body["reasoning"].(map[string]any); reasoning != nil {
			if _, exists := reasoning["effort"]; !exists {
				reasoning["effort"] = effort
			}
		} else {
			body["reasoning"] = map[string]any{"effort": effort}
		}
	}
}

func toolChoiceToAnthropic(value any) (map[string]any, error) {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "required", "any":
			return map[string]any{"type": "any"}, nil
		}
	case map[string]any:
		switch getString(choice, "type") {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "any", "required":
			return map[string]any{"type": "any"}, nil
		case "namespace":
			// namespace 工具已被展平，「强制使用某个命名空间」在展平后无法表达
			// （它对应多个顶层工具）。降级成 auto，而不是让整条请求失败。
			return map[string]any{"type": "auto"}, nil
		case "tool":
			if name := getString(choice, "name"); name != "" {
				return map[string]any{"type": "tool", "name": name}, nil
			}
		case "function":
			if function, ok := choice["function"].(map[string]any); ok {
				if name := getString(function, "name"); name != "" {
					return map[string]any{"type": "tool", "name": name}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("不支持转换为 Anthropic 的 tool_choice：%#v", value)
}

func toolChoiceToResponses(value any) (any, error) {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "auto", "required", "any":
			return choice, nil
		}
	case map[string]any:
		switch getString(choice, "type") {
		case "auto", "required", "any":
			return getString(choice, "type"), nil
		case "namespace":
			// 理由同 toolChoiceToAnthropic：展平后无法表达「限定某命名空间」。
			return "auto", nil
		case "tool":
			if name := getString(choice, "name"); name != "" {
				return map[string]any{"type": "function", "name": name}, nil
			}
		case "function":
			if function, ok := choice["function"].(map[string]any); ok {
				if name := getString(function, "name"); name != "" {
					return map[string]any{"type": "function", "name": name}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("不支持转换为 Responses 的 tool_choice：%#v", value)
}

func responsesTextFormatToChat(text any) (any, error) {
	if text == nil {
		return nil, nil
	}
	container, ok := text.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("responses text must be an object")
	}
	format, exists := container["format"]
	if !exists || format == nil {
		return nil, nil
	}
	f, ok := format.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("responses text.format must be an object")
	}
	switch getString(f, "type") {
	case "text", "":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		name := getString(f, "name")
		schema, exists := f["schema"]
		if name == "" || !exists {
			return nil, fmt.Errorf("responses text.format json_schema requires name and schema")
		}
		jsonSchema := map[string]any{"name": name, "schema": schema}
		if strict, exists := f["strict"]; exists {
			jsonSchema["strict"] = strict
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}, nil
	default:
		return nil, fmt.Errorf("cannot convert Responses text.format type %q to OpenAI Chat Completions", getString(f, "type"))
	}
}

func chatResponseFormatToResponses(responseFormat any) (any, error) {
	f, ok := responseFormat.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openai chat response_format must be an object")
	}
	switch getString(f, "type") {
	case "text", "":
		return map[string]any{"type": "text"}, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		jsonSchema, ok := f["json_schema"].(map[string]any)
		if !ok || getString(jsonSchema, "name") == "" || jsonSchema["schema"] == nil {
			return nil, fmt.Errorf("openai chat json_schema response_format requires json_schema.name and json_schema.schema")
		}
		out := map[string]any{"type": "json_schema", "name": jsonSchema["name"], "schema": jsonSchema["schema"]}
		if strict, exists := jsonSchema["strict"]; exists {
			out["strict"] = strict
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot convert OpenAI Chat response_format type %q to Responses", getString(f, "type"))
	}
}

func responsesReasoningEffort(reasoning any) (any, bool) {
	r, ok := reasoning.(map[string]any)
	if !ok {
		return nil, false
	}
	effort, ok := r["effort"]
	return effort, ok
}

func validateOpenAIChatToolResultContent(content any) error {
	if content == nil {
		return nil
	}
	if _, ok := content.(string); ok {
		return nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return fmt.Errorf("content must be text, got %T", content)
	}
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("content block must be text, got %T", value)
		}
		switch blockType := getString(block, "type"); blockType {
		case "text":
		case "tool_reference", "tool-reference":
			// toOpenAIContent 会把它降级成 text，转换得出来就不该在这里拦。
			// 工具搜索场景的客户端会在 tool_result.content 里回放这种占位块。
		default:
			return fmt.Errorf("tool_result %s content is unsupported", blockType)
		}
	}
	return nil
}

// isReasoningBlockType 判断块类型名是否属于 Anthropic 的思考块。
//
// 请求方向（客户端回传历史）与响应方向（上游本轮输出）共用这份名单，避免两边
// 各自维护、加一种块类型时漏改一处。流式转换按 content_block.type 判定同样走它。
func isReasoningBlockType(blockType string) bool {
	switch blockType {
	case "thinking", "redacted_thinking":
		return true
	}
	return false
}

// isReasoningDeltaType 判断流式 delta 类型是否属于思考块的增量。
//
// thinking_delta 是思考正文，signature_delta 是 Anthropic 对思考内容的加密签名。
// 两者都只在思考块内部出现，目标格式不是 Anthropic 时一并丢弃。
func isReasoningDeltaType(deltaType string) bool {
	switch deltaType {
	case "thinking_delta", "signature_delta":
		return true
	}
	return false
}

// isResponsesReasoningEvent 判断 Responses SSE 事件是否属于 reasoning item 的生命周期。
//
// OpenAI 给 reasoning item 配了一整组事件（summary part 的增删、summary text 的
// 增量与完成）。这些事件对目标协议没有等价物，但也不该报错——它们伴随 reasoning
// item 必然出现，报错等于任何带推理的上游都用不了。正文由
// response.reasoning_summary_text.delta 单独承载，调用方按需取。
func isResponsesReasoningEvent(eventType string) bool {
	switch eventType {
	case "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.delta",
		"response.reasoning_text.done":
		return true
	}
	return false
}

// isReasoningBlock 判断内容块是否属于 Anthropic 的思考块。
//
// Claude Code 在多轮对话里会把上一轮 assistant 回复原样回传，其中包含 thinking /
// redacted_thinking 块。这类块只有 Anthropic 能消费：thinking 的 signature 是
// Anthropic 对思考内容的加密签名，只有它自己能验签，跨厂商传过去纯属噪音。
func isReasoningBlock(block map[string]any) bool {
	return isReasoningBlockType(getString(block, "type"))
}

// stripReasoningBlocks 丢掉 thinking / redacted_thinking 块，用于目标不是 Anthropic 的场景。
//
// 丢弃不损失本轮推理能力：上游本轮是否思考由 reasoning_effort 决定（见 ToOpenAIChatBody），
// 与历史思考块无关；assistant 历史回复里真正承载语义的 text 与 tool_use 块都完整保留。
// 没有思考块时原样返回，不做多余拷贝。
func stripReasoningBlocks(blocks []any) []any {
	found := false
	for _, value := range blocks {
		if block, ok := value.(map[string]any); ok && isReasoningBlock(block) {
			found = true
			break
		}
	}
	if !found {
		return blocks
	}
	out := make([]any, 0, len(blocks))
	for _, value := range blocks {
		if block, ok := value.(map[string]any); ok && isReasoningBlock(block) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// responsesRole 把 canonical 消息角色映射成 Responses 允许的角色。
//
// Responses 用 developer 表达 Chat / Anthropic 的 system 语义。客户端（如 Claude Code）
// 会直接在 messages 数组里放 role:"system" 的消息，这类消息承载真实指令，
// 必须映射而不能丢。
func responsesRole(role string) string {
	if role == "system" {
		return "developer"
	}
	return role
}

// isInstructionRole 判断该角色承载的是「指令」而非对话轮次。Responses 用 developer
// 表达 system 语义（`developer` 是 OpenAI 给 system 起的新名字），Chat 两个都收。
func isInstructionRole(role string) bool {
	return role == "system" || role == "developer"
}

// hoistInstructionMessages 把 messages 里的 system / developer 消息提到顶层 system。
// Anthropic 的 messages 只认 user 和 assistant，混进第三种角色时整个请求体会被
// 判为格式非法（实测 GLM 的 Anthropic 兼容入口返回 400 "Request body format
// invalid"，且错误正文不指向具体字段，很难反查）。Codex 的 Responses 请求首个
// input item 恰好就是 `role: developer`，这条路径必然撞上。
//
// 顺序上把顶层 instructions 放在前、消息里提出来的指令放在后：Responses 的
// instructions 是全局提示，input 里的 developer item 是本轮追加的项目级指令，
// 这个先后与客户端原始请求一致。
//
// 含非文本块（图片等）的指令消息不做提升——Anthropic 的 system 只接受文本，
// 提升等于静默丢图。这类消息降级成 user，内容完整保留。
func hoistInstructionMessages(messages []any, system any) ([]any, any) {
	hasInstructionRole := false
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if ok && isInstructionRole(getString(message, "role")) {
			hasInstructionRole = true
			break
		}
	}
	if !hasInstructionRole {
		return messages, system
	}

	out := make([]any, 0, len(messages))
	parts := make([]string, 0, 2)
	if text := systemTextFromContent(system); text != "" {
		parts = append(parts, text)
	}
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			out = append(out, value)
			continue
		}
		if !isInstructionRole(getString(message, "role")) {
			out = append(out, value)
			continue
		}
		if !instructionContentIsTextOnly(message["content"]) {
			demoted := make(map[string]any, len(message))
			for key, value := range message {
				demoted[key] = value
			}
			demoted["role"] = "user"
			out = append(out, demoted)
			continue
		}
		if text := systemTextFromContent(message["content"]); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return out, nil
	}
	return out, strings.Join(parts, "\n")
}

// instructionContentIsTextOnly 判断指令消息能否无损压成一段文本。
func instructionContentIsTextOnly(content any) bool {
	switch v := content.(type) {
	case nil:
		return true
	case string:
		return true
	case []any:
		for _, value := range v {
			switch block := value.(type) {
			case string:
			case map[string]any:
				if getString(block, "type") != "text" {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func appendCanonicalBlocks(messages *[]any, role string, blocks []any) {
	if len(*messages) > 0 {
		if previous, ok := (*messages)[len(*messages)-1].(map[string]any); ok && getString(previous, "role") == role {
			if content, ok := previous["content"].([]any); ok {
				previous["content"] = append(content, blocks...)
				return
			}
		}
	}
	*messages = append(*messages, map[string]any{"role": role, "content": blocks})
}

func canonicalMessageToOpenAIChat(message map[string]any) []any {
	role := getString(message, "role")
	blocks, ok := message["content"].([]any)
	if !ok {
		return []any{map[string]any{"role": role, "content": toOpenAIContent(message["content"])}}
	}

	// 思考块只有 Anthropic 能消费，转 Chat 前先丢掉，否则会落到 toOpenAIContent
	// 的 default 分支原样透传给上游。
	blocks = stripReasoningBlocks(blocks)

	if role != "assistant" {
		var out []any
		var ordinary []any
		flushOrdinary := func() {
			if len(ordinary) == 0 {
				return
			}
			out = append(out, map[string]any{"role": role, "content": toOpenAIContent(ordinary)})
			ordinary = nil
		}
		for _, value := range blocks {
			block, ok := value.(map[string]any)
			if ok && getString(block, "type") == "tool_result" {
				flushOrdinary()
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": getString(block, "tool_use_id"),
					"content": toOpenAIContent(block["content"]),
				})
				continue
			}
			ordinary = append(ordinary, value)
		}
		flushOrdinary()
		if len(out) == 0 {
			out = append(out, map[string]any{"role": role, "content": nil})
		}
		return out
	}

	var ordinary []any
	var toolCalls []any
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			ordinary = append(ordinary, value)
			continue
		}
		switch getString(block, "type") {
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   getString(block, "id"),
				"type": "function",
				"function": map[string]any{
					"name":      getString(block, "name"),
					"arguments": marshalFunctionArguments(block["input"]),
				},
			})
		default:
			ordinary = append(ordinary, block)
		}
	}

	content := any(nil)
	if len(ordinary) > 0 {
		content = toOpenAIContent(ordinary)
	}
	chatMessage := map[string]any{"role": role, "content": content}
	if len(toolCalls) > 0 {
		chatMessage["tool_calls"] = toolCalls
	}
	return []any{chatMessage}
}

// ── 内容块归一化 ──────────────────────────────

func normalizeOpenAIContent(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	arr, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "text":
			// 兼容客户端发 text 为 JSON 数字（float64）的情况，统一转为字符串
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		case "image_url":
			out = append(out, openAIImageToAnthropic(block["image_url"]))
		default:
			out = append(out, block)
		}
	}
	return out
}

func normalizeResponsesContent(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	arr, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		case "input_image":
			out = append(out, responsesImageToAnthropic(block))
		default:
			out = append(out, block)
		}
	}
	return out
}

func openAIImageToAnthropic(imageURL any) map[string]any {
	var url string
	switch v := imageURL.(type) {
	case string:
		url = v
	case map[string]any:
		url = getString(v, "url")
	}
	// data:<media_type>;base64,<data>
	if strings.HasPrefix(url, "data:") {
		if i := strings.Index(url, ";base64,"); i > 5 {
			mediaType := url[5:i]
			data := url[i+len(";base64,"):]
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

func responsesImageToAnthropic(block map[string]any) map[string]any {
	if iu, ok := block["image_url"]; ok && iu != nil {
		return openAIImageToAnthropic(iu)
	}
	if u, ok := block["url"]; ok && u != nil {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
	}
	return block
}

func toOpenAIContent(content any) any {
	arr, ok := content.([]any)
	if !ok {
		return content
	}
	// tool_reference 先降级成 text，再判断能不能合并成字符串：顺序反了的话，
	// 「只含一个 tool_reference」会因为降级前 type 不是 text 而错过合并分支，
	// 输出数组形式的 tool 消息 content，部分上游不接受。
	arr = degradeToolReferences(arr)

	// 全是文本块则合并为字符串（对齐 Node）
	allText := true
	for _, b := range arr {
		if block, ok := b.(map[string]any); !ok || block["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		parts := make([]string, 0, len(arr))
		for _, b := range arr {
			parts = append(parts, getString(b.(map[string]any), "text"))
		}
		return strings.Join(parts, "\n")
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": block["text"]})
		case "image":
			src, _ := block["source"].(map[string]any)
			var url string
			if getString(src, "type") == "base64" {
				url = "data:" + getString(src, "media_type") + ";base64," + getString(src, "data")
			} else {
				url = getString(src, "url")
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			out = append(out, block)
		}
	}
	return out
}

// degradeToolReferences 把工具搜索（Tool Search / MCP 工具发现）场景的 tool_reference
// 占位块换成等价文本。这种块只引用某个工具定义、不承载结果文本，而 OpenAI Chat 的
// tool 消息 content 只接受字符串或 text part，原样透传必然被上游拒。
// 没有 tool_reference 时原样返回，不做多余拷贝。
func degradeToolReferences(arr []any) []any {
	found := false
	for _, b := range arr {
		if block, ok := b.(map[string]any); ok {
			if t := block["type"]; t == "tool_reference" || t == "tool-reference" {
				found = true
				break
			}
		}
	}
	if !found {
		return arr
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		if t := block["type"]; t == "tool_reference" || t == "tool-reference" {
			out = append(out, map[string]any{"type": "text", "text": toolReferenceText(block)})
			continue
		}
		out = append(out, block)
	}
	return out
}

// toolReferenceText 把 tool_reference 占位块压成一行可读文本。
// 字段名各客户端不统一（ai-sdk 用 toolName，桥接层也见过 tool_name / name），
// 依次取第一个非空的；都没有就退回类型名，不产出空 text 块。
func toolReferenceText(block map[string]any) string {
	for _, key := range []string{"toolName", "tool_name", "name"} {
		if value := getString(block, key); value != "" {
			return "[tool_reference: " + value + "]"
		}
	}
	return "[tool_reference]"
}

// extractText 提取 Anthropic content 数组里的纯文本拼接。
func extractText(content any) string {
	arr, ok := content.([]any)
	if !ok {
		if content == nil {
			return ""
		}
		return getStringValue(content)
	}
	var sb strings.Builder
	for _, b := range arr {
		if block, ok := b.(map[string]any); ok && block["type"] == "text" {
			sb.WriteString(getString(block, "text"))
		}
	}
	return sb.String()
}

// systemTextFromContent 提取并拼接 system/instructions 的所有文本块。
func systemTextFromContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, value := range arr {
			switch block := value.(type) {
			case string:
				if block != "" {
					parts = append(parts, block)
				}
			case map[string]any:
				if text := getString(block, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// shortID 生成 8 位 hex，对应 Node 的 randomUUID().slice(0,8)。
func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── map 取值辅助 ──────────────────────────────

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func getStringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func getBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// getIntDefault JSON 数字默认解析为 float64，做兼容转换。
func getIntDefault(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func getSlice(m map[string]any, key string) []any {
	s, _ := m[key].([]any)
	return s
}
