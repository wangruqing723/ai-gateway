package converter

import "time"

// nowUnix 返回当前秒级时间戳（对齐 Node 的 Math.floor(Date.now()/1000)）。
func nowUnix() int64 { return time.Now().Unix() }

// ── 非流式响应转换 ────────────────────────────

// ConvertAnthropicResponse Anthropic 响应 → 指定客户端格式。
func ConvertAnthropicResponse(data map[string]any, clientFormat, originalModel string) map[string]any {
	switch clientFormat {
	case "anthropic":
		return data
	case "openai-chat":
		return anthropicToOpenAIChatResponse(data, originalModel)
	case "openai-responses":
		return anthropicToResponsesResponse(data, originalModel)
	}
	return data
}

// ConvertOpenAIChatResponse OpenAI Chat 响应 → 指定客户端格式。
func ConvertOpenAIChatResponse(data map[string]any, clientFormat, originalModel string) map[string]any {
	switch clientFormat {
	case "openai-chat":
		return data
	case "anthropic":
		return openAIChatToAnthropicResponse(data, originalModel)
	case "openai-responses":
		return openAIChatToResponsesResponse(data, originalModel)
	}
	return data
}

func anthropicToOpenAIChatResponse(data map[string]any, model string) map[string]any {
	usage, _ := data["usage"].(map[string]any)
	in := getIntDefault(usage, "input_tokens", 0)
	out := getIntDefault(usage, "output_tokens", 0)
	stop := getString(data, "stop_reason")
	finish := stop
	if stop == "end_turn" {
		finish = "stop"
	} else if stop == "" {
		finish = "stop"
	}
	return map[string]any{
		"id":      "chatcmpl-" + shortID(),
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": extractText(data["content"])},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": out,
			"total_tokens":      in + out,
		},
	}
}

func anthropicToResponsesResponse(data map[string]any, model string) map[string]any {
	usage, _ := data["usage"].(map[string]any)
	text := extractText(data["content"])
	return map[string]any{
		"id":         "resp_" + shortID(),
		"object":     "response",
		"created_at": nowUnix(),
		"model":      model,
		"status":     "completed",
		"output": []any{map[string]any{
			"type":    "message",
			"id":      "msg_" + shortID(),
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "input_tokens", 0),
			"output_tokens": getIntDefault(usage, "output_tokens", 0),
		},
	}
}

func openAIChatToAnthropicResponse(data map[string]any, model string) map[string]any {
	choice := firstChoice(data)
	text := ""
	if msg, ok := choice["message"].(map[string]any); ok {
		text = getString(msg, "content")
	}
	usage, _ := data["usage"].(map[string]any)
	finish := getString(choice, "finish_reason")
	stop := finish
	if finish == "stop" {
		stop = "end_turn"
	}
	return map[string]any{
		"id":          "msg_" + shortID(),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stop,
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "prompt_tokens", 0),
			"output_tokens": getIntDefault(usage, "completion_tokens", 0),
		},
	}
}

func openAIChatToResponsesResponse(data map[string]any, model string) map[string]any {
	choice := firstChoice(data)
	text := ""
	if msg, ok := choice["message"].(map[string]any); ok {
		text = getString(msg, "content")
	}
	usage, _ := data["usage"].(map[string]any)
	return map[string]any{
		"id":         "resp_" + shortID(),
		"object":     "response",
		"created_at": nowUnix(),
		"model":      model,
		"status":     "completed",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_" + shortID(), "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": map[string]any{
			"input_tokens":  getIntDefault(usage, "prompt_tokens", 0),
			"output_tokens": getIntDefault(usage, "completion_tokens", 0),
		},
	}
}

func firstChoice(data map[string]any) map[string]any {
	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return map[string]any{}
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return map[string]any{}
	}
	return c
}
