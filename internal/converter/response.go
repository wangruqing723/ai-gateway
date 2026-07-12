package converter

import (
	"fmt"
	"time"
)

// nowUnix 返回当前秒级时间戳（对齐 Node 的 Math.floor(Date.now()/1000)）。
func nowUnix() int64 { return time.Now().Unix() }

// ── 非流式响应转换 ────────────────────────────

// ConvertAnthropicResponse Anthropic 响应 → 指定客户端格式。
// 兼容旧调用方；无法无损转换时原样返回，错误由 Checked 版本提供。
func ConvertAnthropicResponse(data map[string]any, clientFormat, originalModel string) map[string]any {
	converted, err := ConvertAnthropicResponseChecked(data, clientFormat, originalModel)
	if err != nil {
		return data
	}
	return converted
}

// ConvertAnthropicResponseChecked 转换 Anthropic 响应并返回协议转换错误。
func ConvertAnthropicResponseChecked(data map[string]any, clientFormat, originalModel string) (map[string]any, error) {
	switch clientFormat {
	case "anthropic":
		return data, nil
	case "openai-chat":
		return anthropicToOpenAIChatResponse(data, originalModel)
	case "openai-responses":
		return anthropicToResponsesResponse(data, originalModel)
	}
	return data, nil
}

// ConvertOpenAIChatResponse OpenAI Chat 响应 → 指定客户端格式。
// 兼容旧调用方；无法无损转换时原样返回，错误由 Checked 版本提供。
func ConvertOpenAIChatResponse(data map[string]any, clientFormat, originalModel string) map[string]any {
	converted, err := ConvertOpenAIChatResponseChecked(data, clientFormat, originalModel)
	if err != nil {
		return data
	}
	return converted
}

// ConvertOpenAIChatResponseChecked 转换 Chat 响应并返回协议转换错误。
func ConvertOpenAIChatResponseChecked(data map[string]any, clientFormat, originalModel string) (map[string]any, error) {
	switch clientFormat {
	case "openai-chat":
		return data, nil
	case "anthropic":
		return openAIChatToAnthropicResponse(data, originalModel)
	case "openai-responses":
		return openAIChatToResponsesResponse(data, originalModel)
	}
	return data, nil
}

func anthropicToOpenAIChatResponse(data map[string]any, model string) (map[string]any, error) {
	usage, _ := data["usage"].(map[string]any)
	in := getIntDefault(usage, "input_tokens", 0)
	out := getIntDefault(usage, "output_tokens", 0)
	stop := getString(data, "stop_reason")
	finish := anthropicStopReasonToChat(stop)
	text, toolCalls, _, err := anthropicContentToOpenAIChat(data["content"])
	if err != nil {
		return nil, err
	}
	message := map[string]any{"role": "assistant", "content": text}
	if text == "" && len(toolCalls) > 0 {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	result := map[string]any{
		"id":      "chatcmpl-" + shortID(),
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	}
	return result, nil
}

func anthropicToResponsesResponse(data map[string]any, model string) (map[string]any, error) {
	usage, _ := data["usage"].(map[string]any)
	output, err := anthropicContentToResponsesOutput(data["content"])
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"id":         "resp_" + shortID(),
		"object":     "response",
		"created_at": nowUnix(),
		"model":      model,
		"status":     "completed",
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "input_tokens", 0),
			"output_tokens": getIntDefault(usage, "output_tokens", 0),
		},
	}
	if getString(data, "stop_reason") == "max_tokens" {
		markResponseIncomplete(result)
	}
	return result, nil
}

func openAIChatToAnthropicResponse(data map[string]any, model string) (map[string]any, error) {
	choice := firstChoice(data)
	var content []any
	if message, ok := choice["message"].(map[string]any); ok {
		if err := rejectOpenAIChatRefusal(message); err != nil {
			return nil, err
		}
		var err error
		content, _, err = normalizeOpenAIResponseContent(message["content"])
		if err != nil {
			return nil, err
		}
		toolUses, err := chatToolUses(message["tool_calls"])
		if err != nil {
			return nil, err
		}
		content = append(content, toolUses...)
	}
	usage, _ := data["usage"].(map[string]any)
	finish := getString(choice, "finish_reason")
	stop := chatFinishReasonToAnthropic(finish)
	result := map[string]any{
		"id":          "msg_" + shortID(),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stop,
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "prompt_tokens", 0),
			"output_tokens": getIntDefault(usage, "completion_tokens", 0),
		},
	}
	return result, nil
}

func markResponseIncomplete(response map[string]any) {
	response["status"] = "incomplete"
	response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
}

func anthropicStopReasonToChat(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "":
		return "stop"
	default:
		return "stop"
	}
}

func chatFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func openAIChatToResponsesResponse(data map[string]any, model string) (map[string]any, error) {
	choice := firstChoice(data)
	var content any
	var toolCalls any
	if message, ok := choice["message"].(map[string]any); ok {
		if err := rejectOpenAIChatRefusal(message); err != nil {
			return nil, err
		}
		content = message["content"]
		toolCalls = message["tool_calls"]
	}
	output, err := openAIChatContentToResponsesOutput(content, toolCalls)
	if err != nil {
		return nil, err
	}
	usage, _ := data["usage"].(map[string]any)
	result := map[string]any{
		"id":         "resp_" + shortID(),
		"object":     "response",
		"created_at": nowUnix(),
		"model":      model,
		"status":     "completed",
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "prompt_tokens", 0),
			"output_tokens": getIntDefault(usage, "completion_tokens", 0),
		},
	}
	if getString(choice, "finish_reason") == "length" {
		markResponseIncomplete(result)
	}
	return result, nil
}

func rejectOpenAIChatRefusal(message map[string]any) error {
	refusal, exists := message["refusal"]
	if !exists || refusal == nil {
		return nil
	}
	text, ok := refusal.(string)
	if !ok {
		return fmt.Errorf("unsupported openai chat refusal value %T", refusal)
	}
	return fmt.Errorf("unsupported openai chat refusal %q", text)
}

func anthropicContentToOpenAIChat(content any) (string, []any, bool, error) {
	blocks, err := checkedAnthropicResponseBlocks(content)
	if err != nil {
		return "", nil, false, err
	}
	var text string
	var toolCalls []any
	textPresent := false
	for _, block := range blocks {
		switch getString(block, "type") {
		case "text":
			textPresent = true
			text += getString(block, "text")
		case "tool_use":
			input, ok := block["input"].(map[string]any)
			if !ok {
				return "", nil, false, fmt.Errorf("anthropic tool_use %q arguments must be a JSON object", getString(block, "id"))
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": getString(block, "id"), "type": "function",
				"function": map[string]any{
					"name": getString(block, "name"), "arguments": marshalFunctionArguments(input),
				},
			})
		}
	}
	return text, toolCalls, textPresent, nil
}

func anthropicContentToResponsesOutput(content any) ([]any, error) {
	text, toolCalls, textPresent, err := anthropicContentToOpenAIChat(content)
	if err != nil {
		return nil, err
	}
	calls := make([]any, 0, len(toolCalls))
	for _, value := range toolCalls {
		call, _ := value.(map[string]any)
		function, _ := call["function"].(map[string]any)
		calls = append(calls, responsesFunctionCallItem(
			getString(call, "id"), getString(function, "name"), getString(function, "arguments"),
		))
	}
	return responsesOutput(text, calls, textPresent), nil
}

func openAIChatContentToResponsesOutput(content, toolCalls any) ([]any, error) {
	normalized, textPresent, err := normalizeOpenAIResponseContent(content)
	if err != nil {
		return nil, err
	}
	text := extractText(normalized)
	var calls []any
	if toolCalls != nil {
		arr, ok := toolCalls.([]any)
		if !ok {
			return nil, fmt.Errorf("openai chat tool_calls must be an array")
		}
		for _, value := range arr {
			call, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("unsupported openai chat tool call %T", value)
			}
			if callType := getString(call, "type"); callType != "" && callType != "function" {
				return nil, fmt.Errorf("unsupported openai chat tool call type %q", callType)
			}
			function, _ := call["function"].(map[string]any)
			arguments, err := functionArgumentString(function["arguments"])
			if err != nil {
				return nil, fmt.Errorf("openai chat tool call %q arguments: %w", getString(call, "id"), err)
			}
			calls = append(calls, responsesFunctionCallItem(getString(call, "id"), getString(function, "name"), arguments))
		}
	}
	return responsesOutput(text, calls, textPresent), nil
}

func responsesOutput(text string, calls []any, textPresent bool) []any {
	out := make([]any, 0, len(calls)+1)
	if textPresent || len(calls) == 0 {
		out = append(out, map[string]any{
			"type":    "message",
			"id":      "msg_" + shortID(),
			"role":    "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		})
	}
	return append(out, calls...)
}

func responsesFunctionCallItem(callID, name, arguments string) map[string]any {
	if callID == "" {
		callID = "call_" + shortID()
	}
	return map[string]any{
		"type": "function_call", "id": "fc_" + shortID(), "call_id": callID,
		"name": name, "arguments": arguments, "status": "completed",
	}
}

func functionArgumentString(arguments any) (string, error) {
	if value, ok := arguments.(string); ok {
		if _, err := parseFunctionArguments(value); err != nil {
			return "", err
		}
		return value, nil
	}
	object, err := parseFunctionArguments(arguments)
	if err != nil {
		return "", err
	}
	return marshalFunctionArguments(object), nil
}

func checkedAnthropicResponseBlocks(content any) ([]map[string]any, error) {
	arr, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("anthropic response content must be an array, got %T", content)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, value := range arr {
		block, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported anthropic response content block %T", value)
		}
		switch blockType := getString(block, "type"); blockType {
		case "text", "tool_use":
			out = append(out, block)
		default:
			return nil, fmt.Errorf("unsupported anthropic response content block %q", blockType)
		}
	}
	return out, nil
}

func normalizeOpenAIResponseContent(content any) ([]any, bool, error) {
	if content == nil {
		return nil, false, nil
	}
	if text, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, true, nil
	}
	arr, ok := content.([]any)
	if !ok {
		return nil, false, fmt.Errorf("openai chat response content must be a string or array, got %T", content)
	}
	out := make([]any, 0, len(arr))
	for _, value := range arr {
		block, ok := value.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("unsupported openai chat response content block %T", value)
		}
		switch blockType := getString(block, "type"); blockType {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		default:
			return nil, false, fmt.Errorf("unsupported openai chat response content block %q", blockType)
		}
	}
	return out, true, nil
}

func firstChoice(data map[string]any) map[string]any {
	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return map[string]any{}
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return map[string]any{}
	}
	return choice
}
