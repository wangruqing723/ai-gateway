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

// Internal 内部统一格式（Anthropic-like）
type Internal struct {
	Model     string
	Messages  []any // 每个元素为 map[string]any: {role, content}
	System    any   // string 或 nil
	Stream    bool
	MaxTokens int
	Tools     any            // 数组或 nil
	Extra     map[string]any // 原始请求体，用于透传 temperature 等
	Err       error          // 请求协议无法无损规范化时的转换错误
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
		(providerFormat == "openai" && clientFormat == "openai-chat")
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
		MaxTokens: getIntDefault(body, "max_tokens", 4096),
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
		MaxTokens: getIntDefault(body, "max_tokens", 4096),
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
			appendCanonicalBlocks(&messages, "assistant", []any{map[string]any{
				"type":  "tool_use",
				"id":    getString(item, "call_id"),
				"name":  getString(item, "name"),
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
		default:
			if item["type"] != "message" && item["role"] == nil {
				continue
			}
			messages = append(messages, map[string]any{
				"role":    item["role"],
				"content": normalizeResponsesContent(item["content"]),
			})
		}
	}
	tools, toolsErr := normalizeResponsesTools(body["tools"])

	return &Internal{
		Model:     getString(body, "model"),
		Messages:  messages,
		System:    normalizeSystem(body["instructions"]),
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_output_tokens", 4096),
		Tools:     tools,
		Extra:     body,
		Err:       errors.Join(conversionErr, toolsErr),
	}
}

// ── 内部格式 → 上游 Provider 请求体 ───────────

// ToAnthropicBody 内部格式 → Anthropic 请求体。
func ToAnthropicBody(in *Internal, targetModel string) map[string]any {
	body := map[string]any{
		"model":      targetModel,
		"messages":   in.Messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.System != nil && in.System != "" {
		body["system"] = in.System
	}
	if in.Tools != nil {
		body["tools"] = in.Tools
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "stop_sequences", "metadata"} {
		if v, ok := in.Extra[k]; ok {
			body[k] = v
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
	return ToAnthropicBody(in, targetModel), nil
}

// ToOpenAIChatBody 内部格式 → OpenAI Chat 请求体。
func ToOpenAIChatBody(in *Internal, targetModel string) map[string]any {
	var messages []any
	if s := systemTextFromContent(in.System); s != "" {
		messages = append(messages, map[string]any{"role": "system", "content": s})
	}
	for _, m := range in.Messages {
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
	return ToOpenAIChatBody(in, targetModel), nil
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

func normalizeResponsesTools(tools any) (any, error) {
	if tools == nil {
		return nil, nil
	}
	arr, ok := tools.([]any)
	if !ok {
		return nil, fmt.Errorf("responses tools must be an array")
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported responses tool definition %T", value)
		}
		if toolType := getString(tool, "type"); toolType != "function" {
			return nil, fmt.Errorf("unsupported responses tool type %q", toolType)
		}
		canonical := map[string]any{"name": tool["name"]}
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
	return out, nil
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
			case "text", "image", "tool_result":
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
		blocks, _ := message["content"].([]any)
		for blockIndex, blockValue := range blocks {
			block, ok := blockValue.(map[string]any)
			if !ok {
				continue
			}
			if getString(block, "type") != "tool_result" {
				continue
			}
			if err := validateOpenAIChatToolResultContent(block["content"]); err != nil {
				return fmt.Errorf("openai chat message %d tool_result block %d: %w", messageIndex, blockIndex, err)
			}
		}
	}
	return nil
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
