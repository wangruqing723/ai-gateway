package converter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func requireJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("JSON mismatch\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
}

func TestResponsesStrictFunctionPreservedWhenConvertingToChat(t *testing.T) {
	internal := FromOpenAIResponses(map[string]any{
		"model": "client-model",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "strict": true,
			"parameters": map[string]any{"type": "object"},
		}},
	})
	body, err := ToOpenAIChatBodyChecked(internal, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	if function["strict"] != true {
		t.Fatalf("strict flag was dropped: %#v", body)
	}
}

func TestAnthropicRequestToResponsesMapsMessagesToolsAndImages(t *testing.T) {
	in := FromAnthropic(map[string]any{
		"model":  "claude-client",
		"system": "Be concise.",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Look at this."},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "YWJj"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": "sunny"},
			}},
		},
		"tools": []any{map[string]any{"name": "weather", "input_schema": map[string]any{"type": "object"}}},
	})
	body, err := ToOpenAIResponsesBodyChecked(in, "gpt-upstream")
	if err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "Be concise." {
		t.Fatalf("instructions = %#v", body["instructions"])
	}
	input, _ := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", body["input"])
	}
	message, _ := input[0].(map[string]any)
	content, _ := message["content"].([]any)
	image, _ := content[1].(map[string]any)
	if image["image_url"] != "data:image/png;base64,YWJj" {
		t.Fatalf("image = %#v", image)
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_weather" || call["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("function call = %#v", call)
	}
	result, _ := input[2].(map[string]any)
	if result["type"] != "function_call_output" || result["output"] != "sunny" {
		t.Fatalf("function output = %#v", result)
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["parameters"] == nil {
		t.Fatalf("responses tool = %#v", tool)
	}
}

func TestResponsesRequestMapsChatResponseFormatAndReasoningEffort(t *testing.T) {
	in := FromOpenAIChat(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "json please"}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "answer", "schema": map[string]any{"type": "object"}, "strict": true,
		}},
		"reasoning_effort": "high",
	})
	body, err := ToOpenAIResponsesBodyChecked(in, "gpt-upstream")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := body["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" {
		t.Fatalf("responses text format = %#v", text)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("responses reasoning = %#v", reasoning)
	}
}

func TestCheckedBodiesApplyExtraFieldPolicy(t *testing.T) {
	t.Run("Anthropic 到 Chat 映射 stop_sequences", func(t *testing.T) {
		in := FromAnthropic(map[string]any{
			"messages":       []any{map[string]any{"role": "user", "content": "你好"}},
			"stop_sequences": []any{"结束"},
			"top_k":          3,
			"metadata":       map[string]any{"user_id": "u_1"},
		})
		body, err := ToOpenAIChatBodyChecked(in, "gpt-upstream")
		if err != nil {
			t.Fatalf("转换失败：%v", err)
		}
		requireJSONEqual(t, []any{"结束"}, body["stop"])
	})

	t.Run("Responses 默认字段和可丢弃字段到 Chat", func(t *testing.T) {
		in := FromOpenAIResponses(map[string]any{
			"input":     []any{map[string]any{"role": "user", "content": "你好"}},
			"store":     false,
			"include":   []any{"reasoning.encrypted_content"},
			"reasoning": map[string]any{"summary": "auto"},
		})
		if _, err := ToOpenAIChatBodyChecked(in, "gpt-upstream"); err != nil {
			t.Fatalf("转换失败：%v", err)
		}
	})

	t.Run("Anthropic 到 Responses 丢弃 stop_sequences", func(t *testing.T) {
		in := FromAnthropic(map[string]any{
			"messages":       []any{map[string]any{"role": "user", "content": "你好"}},
			"stop_sequences": []any{"结束"},
			"temperature":    0.7,
		})
		body, err := ToOpenAIResponsesBodyChecked(in, "glm-upstream")
		if err != nil {
			t.Fatalf("带 stop_sequences 转 Responses 不应报错（应静默丢弃）: %v", err)
		}
		if _, exists := body["stop_sequences"]; exists {
			t.Errorf("Responses 体不应包含 stop_sequences（应丢弃）")
		}
		if _, exists := body["stop"]; exists {
			t.Errorf("Responses API 无 stop 字段，不应映射")
		}
		// temperature 仍正常透传，确认丢弃只针对 stop_sequences
		if body["temperature"] != 0.7 {
			t.Errorf("temperature 应透传，得到 %v", body["temperature"])
		}
	})

	t.Run("Chat 到 Anthropic 映射工具选择和并行开关", func(t *testing.T) {
		in := FromOpenAIChat(map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "你好"}},
			// 必须带 tools：tool_choice 只在有工具时才转发，否则 Anthropic 对
			// 「有 tool_choice 无 tools」返回不可重试的 400。
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "lookup", "parameters": map[string]any{"type": "object"},
				},
			}},
			"tool_choice": map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "lookup"},
			},
			"parallel_tool_calls": false,
		})
		body, err := ToAnthropicBodyChecked(in, "claude-upstream")
		if err != nil {
			t.Fatalf("转换失败：%v", err)
		}
		requireJSONEqual(t, map[string]any{
			"type":                      "tool",
			"name":                      "lookup",
			"disable_parallel_tool_use": true,
		}, body["tool_choice"])
	})

	for _, tt := range []struct {
		name  string
		key   string
		value any
	}{
		{name: "previous_response_id", key: "previous_response_id", value: "resp_123"},
		{name: "conversation", key: "conversation", value: "conv_123"},
		{name: "background", key: "background", value: true},
		{name: "store true", key: "store", value: true},
		{name: "n 大于一", key: "n", value: 2},
	} {
		t.Run("拒绝 "+tt.name, func(t *testing.T) {
			in := FromOpenAIResponses(map[string]any{
				"input": []any{map[string]any{"role": "user", "content": "你好"}},
				tt.key:  tt.value,
			})
			if _, err := ToOpenAIChatBodyChecked(in, "gpt-upstream"); err == nil {
				t.Fatalf("字段 %q 应被拒绝", tt.key)
			}
		})
	}
}

func TestToolChoiceToResponses(t *testing.T) {
	// 每个用例都要带 tools：tool_choice 只在确实有工具时才转发，否则上游会因
	// 「有 tool_choice 无 tools」返回不可重试的 400。见 ToOpenAIResponsesBody。
	lookupTool := []any{map[string]any{
		"name": "lookup", "input_schema": map[string]any{"type": "object"},
	}}
	chatTool := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "lookup", "parameters": map[string]any{"type": "object"},
		},
	}}
	tests := []struct {
		name string
		in   *Internal
		want any
	}{
		{
			name: "Anthropic 指定工具",
			in: FromAnthropic(map[string]any{
				"messages":    []any{map[string]any{"role": "user", "content": "你好"}},
				"tools":       lookupTool,
				"tool_choice": map[string]any{"type": "tool", "name": "lookup"},
			}),
			want: map[string]any{"type": "function", "name": "lookup"},
		},
		{
			name: "Chat 指定函数",
			in: FromOpenAIChat(map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "你好"}},
				"tools":    chatTool,
				"tool_choice": map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "lookup"},
				},
			}),
			want: map[string]any{"type": "function", "name": "lookup"},
		},
		{
			name: "auto 保持字符串",
			in: FromOpenAIChat(map[string]any{
				"messages":    []any{map[string]any{"role": "user", "content": "你好"}},
				"tools":       chatTool,
				"tool_choice": "auto",
			}),
			want: "auto",
		},
		{
			name: "无 tools 时不转发 tool_choice",
			in: FromOpenAIChat(map[string]any{
				"messages":    []any{map[string]any{"role": "user", "content": "你好"}},
				"tool_choice": "auto",
			}),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := ToOpenAIResponsesBodyChecked(tt.in, "gpt-upstream")
			if err != nil {
				t.Fatalf("转换失败：%v", err)
			}
			requireJSONEqual(t, tt.want, body["tool_choice"])
		})
	}
}

func TestAnthropicRequestToOpenAIChatMapsToolsCallsResultsAndSystem(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
	}
	in := FromAnthropic(map[string]any{
		"model": "claude-test",
		"system": []any{
			map[string]any{"type": "text", "text": "policy one"},
			map[string]any{"type": "text", "text": "policy two"},
		},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "Checking."},
				map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": "sunny"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "weather", "description": "Get weather", "input_schema": schema},
		},
		"max_tokens": 100,
	})

	if in.System != "policy one\npolicy two" {
		t.Fatalf("system = %#v, want joined text", in.System)
	}

	got := ToOpenAIChatBody(in, "gpt-test")
	requireJSONEqual(t, []any{
		map[string]any{"role": "system", "content": "policy one\npolicy two"},
		map[string]any{
			"role":    "assistant",
			"content": "Checking.",
			"tool_calls": []any{map[string]any{
				"id": "call_weather", "type": "function",
				"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
			}},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_weather", "content": "sunny"},
	}, got["messages"])
	requireJSONEqual(t, []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "weather", "description": "Get weather", "parameters": schema,
			},
		},
	}, got["tools"])
}

func TestOpenAIChatRequestToAnthropicNormalizesToolsCallsResultsAndSystem(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"city"}}
	in := FromOpenAIChat(map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "policy one"},
				map[string]any{"type": "text", "text": "policy two"},
			}},
			map[string]any{"role": "system", "content": "policy three"},
			map[string]any{
				"role": "assistant", "content": "Checking.",
				"tool_calls": []any{map[string]any{
					"id": "call_weather", "type": "function",
					"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_weather", "content": "sunny"},
		},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "weather", "description": "Get weather", "parameters": schema,
			},
		}},
	})

	if in.System != "policy one\npolicy two\npolicy three" {
		t.Fatalf("system = %#v, want all system blocks", in.System)
	}
	requireJSONEqual(t, []any{
		map[string]any{"name": "weather", "description": "Get weather", "input_schema": schema},
	}, in.Tools)
	requireJSONEqual(t, []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Checking."},
			map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": "sunny"},
		}},
	}, in.Messages)

	got := ToAnthropicBody(in, "claude-test")
	requireJSONEqual(t, in.Messages, got["messages"])
	requireJSONEqual(t, in.Tools, got["tools"])
}

func TestResponsesRequestToOpenAIChatMapsToolsCallsAndResults(t *testing.T) {
	schema := map[string]any{"type": "object", "additionalProperties": false}
	in := FromOpenAIResponses(map[string]any{
		"model": "gpt-test",
		"instructions": []any{
			map[string]any{"type": "input_text", "text": "policy one"},
			map[string]any{"type": "input_text", "text": "policy two"},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "Weather?"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_weather", "name": "weather", "arguments": `{"city":"Paris"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_weather", "output": "sunny"},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "weather", "description": "Get weather", "parameters": schema,
		}},
	})

	if in.System != "policy one\npolicy two" {
		t.Fatalf("system = %#v, want joined instructions", in.System)
	}
	requireJSONEqual(t, []any{
		map[string]any{"name": "weather", "description": "Get weather", "input_schema": schema},
	}, in.Tools)

	got := ToOpenAIChatBody(in, "gpt-upstream")
	requireJSONEqual(t, []any{
		map[string]any{"role": "system", "content": "policy one\npolicy two"},
		map[string]any{"role": "user", "content": "Weather?"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{
				"id": "call_weather", "type": "function",
				"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
			},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_weather", "content": "sunny"},
	}, got["messages"])
	requireJSONEqual(t, []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "weather", "description": "Get weather", "parameters": schema,
		},
	}}, got["tools"])
}

func TestOpenAIChatRequestWithoutSystemKeepsCanonicalSystemNil(t *testing.T) {
	in := FromOpenAIChat(map[string]any{
		"model":    "gpt-test",
		"messages": []any{map[string]any{"role": "user", "content": "Hello"}},
	})
	if in.System != nil {
		t.Fatalf("system = %#v, want nil", in.System)
	}
}

func TestAnthropicToolResultThenTextPreservesValidChatOrder(t *testing.T) {
	in := FromAnthropic(map[string]any{
		"model": "claude-test",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": "sunny"},
				map[string]any{"type": "text", "text": "Summarize it."},
			}},
		},
	})

	got := ToOpenAIChatBody(in, "gpt-test")
	messages, _ := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	requireJSONEqual(t, map[string]any{
		"role": "assistant", "content": nil,
		"tool_calls": []any{map[string]any{
			"id": "call_weather", "type": "function",
			"function": map[string]any{"name": "weather", "arguments": `{}`},
		}},
	}, messages[0])
	requireJSONEqual(t, map[string]any{"role": "tool", "tool_call_id": "call_weather", "content": "sunny"}, messages[1])
	requireJSONEqual(t, map[string]any{"role": "user", "content": "Summarize it."}, messages[2])
}

func TestResponsesFunctionCallOutputArrayNormalizesForChat(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{
		"model": "gpt-test",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_weather", "name": "weather", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_weather", "output": []any{
				map[string]any{"type": "input_text", "text": "sunny"},
				map[string]any{"type": "input_text", "text": "warm"},
			}},
		},
	})

	requireJSONEqual(t, []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": []any{
				map[string]any{"type": "text", "text": "sunny"},
				map[string]any{"type": "text", "text": "warm"},
			}},
		}},
	}, in.Messages)

	body := ToOpenAIChatBody(in, "gpt-upstream")
	messages, _ := body["messages"].([]any)
	requireJSONEqual(t, map[string]any{
		"role": "tool", "tool_call_id": "call_weather", "content": "sunny\nwarm",
	}, messages[1])
}

func requireInternalError(t *testing.T, in *Internal, contains ...string) {
	t.Helper()
	if in.Err == nil {
		t.Fatal("expected request conversion error")
	}
	for _, part := range contains {
		if !strings.Contains(in.Err.Error(), part) {
			t.Fatalf("error %q does not contain %q", in.Err, part)
		}
	}
}

func TestRequestFunctionArgumentsMustBeJSONObject(t *testing.T) {
	tests := []struct {
		name string
		in   *Internal
	}{
		{
			name: "chat-malformed",
			in: FromOpenAIChat(map[string]any{"messages": []any{map[string]any{
				"role": "assistant", "tool_calls": []any{map[string]any{
					"id": "call_bad", "type": "function",
					"function": map[string]any{"name": "bad", "arguments": `{"x":`},
				}},
			}}}),
		},
		{
			name: "chat-array",
			in: FromOpenAIChat(map[string]any{"messages": []any{map[string]any{
				"role": "assistant", "tool_calls": []any{map[string]any{
					"id": "call_bad", "type": "function",
					"function": map[string]any{"name": "bad", "arguments": `[]`},
				}},
			}}}),
		},
		{
			name: "responses-array",
			in: FromOpenAIResponses(map[string]any{"input": []any{
				map[string]any{"type": "function_call", "call_id": "call_bad", "name": "bad", "arguments": `[]`},
			}}),
		},
		{
			name: "anthropic-array",
			in: FromAnthropic(map[string]any{"messages": []any{map[string]any{
				"role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": "call_bad", "name": "bad", "input": []any{},
				}},
			}}}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireInternalError(t, tt.in, "arguments", "object")
		})
	}
}

func TestUnsupportedRequestToolsReturnExplicitError(t *testing.T) {
	tests := []struct {
		name string
		in   *Internal
	}{
		{
			name: "chat",
			in: FromOpenAIChat(map[string]any{"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "ok", "parameters": map[string]any{"type": "object"}}},
				map[string]any{"type": "web_search"},
			}}),
		},
		{
			// Responses 侧的内置工具改为丢弃，见
			// TestResponsesHostedToolsAreDroppedNotRejected；这里只保留 chat 与
			// anthropic 两条仍应报错的路径。
			name: "anthropic",
			in: FromAnthropic(map[string]any{"tools": []any{
				map[string]any{"type": "computer_20241022", "name": "computer"},
			}}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireInternalError(t, tt.in, "unsupported", "tool")
		})
	}
}

// Responses 的服务端内置工具（web_search / tool_search / image_generation 等）由
// OpenAI 后端执行，第三方上游既不认也无法代为执行。必须丢弃而非报错：Codex CLI
// 每一轮都声明 web_search，报错等于整个 Codex 完全不可用（实测 400
// `unsupported responses tool type "namespace"` / web_search）。
func TestResponsesHostedToolsAreDroppedNotRejected(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "ok", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "web_search", "external_web_access": true},
		map[string]any{"type": "web_search_preview"},
		map[string]any{"type": "image_generation"},
		map[string]any{"type": "local_shell"},
	}})
	if in.Err != nil {
		t.Fatalf("Err = %v, want nil（内置工具应被丢弃）", in.Err)
	}
	tools, _ := in.Tools.([]any)
	if len(tools) != 1 {
		t.Fatalf("Tools = %#v, want 只保留 1 个 function", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if getString(tool, "name") != "ok" {
		t.Fatalf("保留的工具 = %#v, want name=ok", tool)
	}
}

// 工具全是内置类型时 Tools 必须为 nil 而非空数组，且不得留下悬空 tool_choice：
// Anthropic 对「有 tool_choice 无 tools」返回不可重试的 400。
func TestResponsesAllHostedToolsLeaveNoDanglingToolChoice(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{
		"tools":       []any{map[string]any{"type": "web_search"}},
		"tool_choice": "auto",
	})
	if in.Err != nil {
		t.Fatalf("Err = %v, want nil", in.Err)
	}
	if in.Tools != nil {
		t.Fatalf("Tools = %#v, want nil", in.Tools)
	}
	for _, target := range []string{"anthropic", "openai-responses"} {
		var body map[string]any
		var err error
		if target == "anthropic" {
			body, err = ToAnthropicBodyChecked(in, "claude-upstream")
		} else {
			body, err = ToOpenAIResponsesBodyChecked(in, "gpt-upstream")
		}
		if err != nil {
			t.Fatalf("%s: error = %v", target, err)
		}
		if _, exists := body["tools"]; exists {
			t.Errorf("%s: 不应写出 tools: %#v", target, body["tools"])
		}
		if _, exists := body["tool_choice"]; exists {
			t.Errorf("%s: 不应留下悬空 tool_choice: %#v", target, body["tool_choice"])
		}
	}
}

func TestAnthropicTextBeforeToolResultRecordsUnrepresentableChatOrder(t *testing.T) {
	in := FromAnthropic(map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "premature"},
			map[string]any{"type": "tool_result", "tool_use_id": "call_weather", "content": "sunny"},
		},
	}}})
	requireInternalError(t, in, "tool_result", "order")
}

func TestUnsupportedResponsesFunctionOutputBlockReturnsError(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call_file", "output": []any{
			map[string]any{"type": "input_file", "file_id": "file_123"},
		}},
	}})
	requireInternalError(t, in, "unsupported", "input_file")
}

// TestResponsesReasoningInputIsDroppedNotRejected
//
// 原先这里断言 reasoning input item 必须报错，理由是「不得静默转换」。但网关自己
// 就会向 Responses 客户端产出 reasoning item（anthropicToResponses），透传场景下
// 上游的 reasoning item 也会原样到达客户端；客户端按协议回传完整对话历史，报错
// 等于任何带推理的多轮会话在第二轮必然 400（实测 /v1/responses 返回
// conversion_error）。改为丢弃，与 stripReasoningBlocks 处理 Anthropic thinking
// 历史的口径一致。
func TestResponsesReasoningInputIsDroppedNotRejected(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "1+1"},
		}},
		map[string]any{"type": "reasoning", "id": "rs_123", "summary": []any{
			map[string]any{"type": "summary_text", "text": "上一轮的思考"},
		}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "2"},
		}},
	}})
	if in.Err != nil {
		t.Fatalf("Err = %v, want nil（reasoning 历史应被丢弃而非报错）", in.Err)
	}
	// 三个 item 里只有两条消息该进 messages，reasoning 不占位。
	if len(in.Messages) != 2 {
		t.Fatalf("Messages = %d 条, want 2: %#v", len(in.Messages), in.Messages)
	}
	for _, target := range []string{"chat", "anthropic"} {
		var body map[string]any
		var err error
		if target == "chat" {
			body, err = ToOpenAIChatBodyChecked(in, "gpt-upstream")
		} else {
			body, err = ToAnthropicBodyChecked(in, "claude-upstream")
		}
		if err != nil {
			t.Fatalf("%s: error = %v, want nil", target, err)
		}
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatalf("marshal: %v", marshalErr)
		}
		if strings.Contains(string(encoded), "上一轮的思考") {
			t.Fatalf("%s: 推理历史不应转发给上游: %s", target, encoded)
		}
		if !strings.Contains(string(encoded), "2") {
			t.Fatalf("%s: assistant 正文丢失: %s", target, encoded)
		}
	}
}

func TestResponsesInputFileIsRejectedForOpenAIChatTarget(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"input": []any{map[string]any{
		"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_file", "file_id": "file_123"}},
	}}})
	if _, err := ToOpenAIChatBodyChecked(in, "gpt-upstream"); err == nil || !strings.Contains(err.Error(), "input_file") {
		t.Fatalf("checked body error = %v, want unsupported input_file", err)
	}
}

func TestResponsesFunctionOutputFileIDImageReturnsError(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call_image", "output": []any{
			map[string]any{"type": "input_image", "file_id": "file_123"},
		}},
	}})
	requireInternalError(t, in, "input_image", "file_id")
}

func TestResponsesURLImageToolOutputUsesTargetAwareCheckedBodies(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{"input": []any{
		map[string]any{"type": "function_call", "call_id": "call_image", "name": "inspect", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_image", "output": []any{
			map[string]any{"type": "input_image", "image_url": "https://example.com/result.png"},
		}},
	}})
	if in.Err != nil {
		t.Fatalf("URL image normalization error = %v", in.Err)
	}

	body, err := ToAnthropicBodyChecked(in, "claude-upstream")
	if err != nil {
		t.Fatalf("anthropic checked body error = %v", err)
	}
	messages, _ := body["messages"].([]any)
	resultMessage, _ := messages[1].(map[string]any)
	resultContent, _ := resultMessage["content"].([]any)
	resultBlock, _ := resultContent[0].(map[string]any)
	imageContent, _ := resultBlock["content"].([]any)
	requireJSONEqual(t, []any{map[string]any{
		"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/result.png"},
	}}, imageContent)

	if _, err := ToOpenAIChatBodyChecked(in, "gpt-upstream"); err == nil || !strings.Contains(err.Error(), "tool_result") || !strings.Contains(err.Error(), "image") {
		t.Fatalf("chat checked body error = %v", err)
	}
	if legacy := ToOpenAIChatBody(in, "gpt-upstream"); legacy == nil {
		t.Fatal("legacy ToOpenAIChatBody must remain available")
	}
}

// TestToolResultToolReferenceDegradesToText 锁定工具搜索场景的 tool_reference 占位块
// 不再让整条请求失败：校验放行，转换时降级成一行文本。
// 原来这里会报 "tool_result tool_reference content is unsupported" 并跳过候选。
func TestToolResultToolReferenceDegradesToText(t *testing.T) {
	newInternal := func(block map[string]any) *Internal {
		return &Internal{
			Model: "any",
			Messages: []any{
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "call_search", "name": "ToolSearch", "input": map[string]any{}},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_search", "content": []any{block}},
				}},
			},
		}
	}

	tests := []struct {
		name  string
		block map[string]any
		want  string
	}{
		{"toolName", map[string]any{"type": "tool_reference", "toolName": "get_weather"}, "[tool_reference: get_weather]"},
		{"tool_name", map[string]any{"type": "tool_reference", "tool_name": "get_forecast"}, "[tool_reference: get_forecast]"},
		{"name", map[string]any{"type": "tool_reference", "name": "get_temp"}, "[tool_reference: get_temp]"},
		{"hyphenated", map[string]any{"type": "tool-reference", "toolName": "hyphen_tool"}, "[tool_reference: hyphen_tool]"},
		{"no name field", map[string]any{"type": "tool_reference"}, "[tool_reference]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInternal(tt.block)
			body, err := ToOpenAIChatBodyChecked(in, "gpt-upstream")
			if err != nil {
				t.Fatalf("checked body error = %v, want nil", err)
			}
			messages, _ := body["messages"].([]any)
			if len(messages) < 2 {
				t.Fatalf("messages = %#v, want at least 2", messages)
			}
			// 单个 tool_reference 降级成唯一的 text 块后，toOpenAIContent 的
			// 「全是文本就合并成字符串」分支会把它压成纯字符串。
			requireJSONEqual(t, map[string]any{
				"role": "tool", "tool_call_id": "call_search", "content": tt.want,
			}, messages[1])
		})
	}
}

// TestToolResultToolReferenceMixedWithText 覆盖 tool_reference 与真实文本混排：
// 两者都要保留，顺序不变。
func TestToolResultToolReferenceMixedWithText(t *testing.T) {
	in := &Internal{
		Model: "any",
		Messages: []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_search", "content": []any{
					map[string]any{"type": "text", "text": "found 2 tools"},
					map[string]any{"type": "tool_reference", "toolName": "get_weather"},
				}},
			}},
		},
	}
	body, err := ToOpenAIChatBodyChecked(in, "gpt-upstream")
	if err != nil {
		t.Fatalf("checked body error = %v, want nil", err)
	}
	messages, _ := body["messages"].([]any)
	requireJSONEqual(t, map[string]any{
		"role": "tool", "tool_call_id": "call_search",
		"content": "found 2 tools\n[tool_reference: get_weather]",
	}, messages[0])
}

// TestToolResultUnknownBlockStillRejected 确认放行只针对 tool_reference，
// 其他不认识的 block 仍要在转换前报错，而不是塞给上游让它 400。
func TestToolResultUnknownBlockStillRejected(t *testing.T) {
	in := &Internal{
		Model: "any",
		Messages: []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_x", "content": []any{
					map[string]any{"type": "some_future_block"},
				}},
			}},
		},
	}
	if _, err := ToOpenAIChatBodyChecked(in, "gpt-upstream"); err == nil || !strings.Contains(err.Error(), "some_future_block") {
		t.Fatalf("checked body error = %v, want unsupported some_future_block", err)
	}
}

// TestAnthropicThinkingBlocksDroppedForNonAnthropicTargets 覆盖 Claude Code 多轮历史里的
// thinking / redacted_thinking 块：转 Chat 与 Responses 时必须丢掉（signature 是 Anthropic
// 专有加密签名，跨厂商无法验签），同时 text 与 tool_use 必须完整保留。
func TestAnthropicThinkingBlocksDroppedForNonAnthropicTargets(t *testing.T) {
	newBody := func() map[string]any {
		return map[string]any{
			"model": "claude-opus-5",
			"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "问题"},
				}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "thinking", "thinking": "内部思考", "signature": "sig-abc"},
					map[string]any{"type": "redacted_thinking", "data": "opaque"},
					map[string]any{"type": "text", "text": "回答"},
					map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"q": "x"}},
				}},
			},
		}
	}

	t.Run("chat 目标丢弃思考块", func(t *testing.T) {
		in := FromAnthropic(newBody())
		if in.Err != nil {
			t.Fatalf("FromAnthropic err = %v", in.Err)
		}
		body, err := ToOpenAIChatBodyChecked(in, "glm-5.2")
		if err != nil {
			t.Fatalf("ToOpenAIChatBodyChecked err = %v，思考块应被丢弃而不是报错", err)
		}
		raw, _ := json.Marshal(body)
		for _, banned := range []string{"thinking", "redacted_thinking", "sig-abc", "内部思考", "opaque"} {
			if strings.Contains(string(raw), banned) {
				t.Fatalf("Chat 请求体仍含 %q: %s", banned, raw)
			}
		}
		if !strings.Contains(string(raw), "回答") || !strings.Contains(string(raw), "lookup") {
			t.Fatalf("text / tool_use 未保留: %s", raw)
		}
	})

	t.Run("responses 目标丢弃思考块", func(t *testing.T) {
		in := FromAnthropic(newBody())
		body, err := ToOpenAIResponsesBodyChecked(in, "glm-5.3")
		if err != nil {
			t.Fatalf("ToOpenAIResponsesBodyChecked err = %v，思考块应被丢弃而不是报错", err)
		}
		raw, _ := json.Marshal(body)
		for _, banned := range []string{"redacted_thinking", "sig-abc", "内部思考", "opaque"} {
			if strings.Contains(string(raw), banned) {
				t.Fatalf("Responses 请求体仍含 %q: %s", banned, raw)
			}
		}
		if !strings.Contains(string(raw), "回答") || !strings.Contains(string(raw), "lookup") {
			t.Fatalf("text / tool_use 未保留: %s", raw)
		}
	})

	t.Run("anthropic 目标保留思考块与签名", func(t *testing.T) {
		in := FromAnthropic(newBody())
		body, err := ToAnthropicBodyChecked(in, "claude-opus-5")
		if err != nil {
			t.Fatalf("ToAnthropicBodyChecked err = %v，同厂商应原样保留", err)
		}
		raw, _ := json.Marshal(body)
		for _, kept := range []string{"thinking", "redacted_thinking", "sig-abc", "内部思考"} {
			if !strings.Contains(string(raw), kept) {
				t.Fatalf("Anthropic 请求体丢了 %q: %s", kept, raw)
			}
		}
	})
}

// TestClientSystemMessageMapsToDeveloperForResponses 覆盖 Claude Code 直接在 messages
// 数组里放 role:"system" 的情况：Responses 只认 developer，必须映射而不是拒绝。
func TestClientSystemMessageMapsToDeveloperForResponses(t *testing.T) {
	in := FromAnthropic(map[string]any{
		"model": "claude-opus-5",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "遵守规则"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "你好"},
			}},
		},
	})
	if in.Err != nil {
		t.Fatalf("FromAnthropic err = %v", in.Err)
	}
	body, err := ToOpenAIResponsesBodyChecked(in, "glm-5.3")
	if err != nil {
		t.Fatalf("ToOpenAIResponsesBodyChecked err = %v，system 应映射为 developer", err)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 {
		t.Fatalf("input = %#v", body["input"])
	}
	first, _ := input[0].(map[string]any)
	if got := getString(first, "role"); got != "developer" {
		t.Fatalf("首条 role = %q, want developer", got)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "遵守规则") {
		t.Fatalf("system 指令内容丢失: %s", raw)
	}
	if strings.Contains(string(raw), `"role":"system"`) {
		t.Fatalf("Responses 请求体仍含 role:system: %s", raw)
	}
}

// TestInstructionRoleMessagesHoistToSystem 锁定 Codex → Anthropic 的实际故障：
// Codex 的 Responses 请求首个 input item 是 role: developer，而 Anthropic 的
// messages 只认 user / assistant。实测 GLM 的 Anthropic 兼容入口收到第三种角色时
// 返回 400 "Request body format invalid"，正文不指向具体字段。
func TestInstructionRoleMessagesHoistToSystem(t *testing.T) {
	for _, role := range []string{"developer", "system"} {
		t.Run(role, func(t *testing.T) {
			in := FromOpenAIResponses(map[string]any{
				"model":        "gpt-5.6-sol",
				"instructions": "全局指令",
				"input": []any{
					map[string]any{"type": "message", "role": role, "content": []any{
						map[string]any{"type": "input_text", "text": "项目指令"},
					}},
					map[string]any{"type": "message", "role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "你好"},
					}},
				},
			})
			if in.Err != nil {
				t.Fatalf("FromOpenAIResponses err = %v", in.Err)
			}

			body, err := ToAnthropicBodyChecked(in, "glm-5.2")
			if err != nil {
				t.Fatalf("ToAnthropicBodyChecked err = %v", err)
			}
			messages, _ := body["messages"].([]any)
			if len(messages) != 1 {
				t.Fatalf("messages 长度 = %d, want 1（指令消息应提升到 system）: %#v", len(messages), messages)
			}
			first, _ := messages[0].(map[string]any)
			if got := getString(first, "role"); got != "user" {
				t.Fatalf("messages[0].role = %q, want user", got)
			}
			// 顶层 instructions 在前、消息里的指令在后，与客户端原始顺序一致
			if got := body["system"]; got != "全局指令\n项目指令" {
				t.Fatalf("system = %#v, want \"全局指令\\n项目指令\"", got)
			}

			// Chat 目标做同样的归一：官方虽认 developer，第三方兼容入口支持不一致。
			chat, err := ToOpenAIChatBodyChecked(in, "glm-5.2")
			if err != nil {
				t.Fatalf("ToOpenAIChatBodyChecked err = %v", err)
			}
			chatMessages, _ := chat["messages"].([]any)
			if len(chatMessages) != 2 {
				t.Fatalf("chat messages 长度 = %d, want 2: %#v", len(chatMessages), chatMessages)
			}
			head, _ := chatMessages[0].(map[string]any)
			if getString(head, "role") != "system" || head["content"] != "全局指令\n项目指令" {
				t.Fatalf("chat 首条 = %#v, want role=system 且含两段指令", head)
			}
			if raw, _ := json.Marshal(chat); strings.Contains(string(raw), `"role":"developer"`) {
				t.Fatalf("Chat 请求体仍含 role:developer: %s", raw)
			}
		})
	}
}

// TestInstructionRoleWithImageDemotesToUser 覆盖含非文本块的指令消息：Anthropic 的
// system 只接受文本，提升等于静默丢图，这类消息要降级成 user 并完整保留内容。
func TestInstructionRoleWithImageDemotesToUser(t *testing.T) {
	in := FromOpenAIResponses(map[string]any{
		"model": "gpt-5.6-sol",
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{
				map[string]any{"type": "input_text", "text": "看图"},
				map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
			}},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "这是什么"},
			}},
		},
	})
	if in.Err != nil {
		t.Fatalf("FromOpenAIResponses err = %v", in.Err)
	}
	body, err := ToAnthropicBodyChecked(in, "glm-5.2")
	if err != nil {
		t.Fatalf("ToAnthropicBodyChecked err = %v", err)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages 长度 = %d, want 2（降级而非丢弃）: %#v", len(messages), messages)
	}
	first, _ := messages[0].(map[string]any)
	if got := getString(first, "role"); got != "user" {
		t.Fatalf("messages[0].role = %q, want user", got)
	}
	if _, exists := body["system"]; exists {
		t.Fatalf("system = %#v, 含图消息不应被提升", body["system"])
	}
	if raw, _ := json.Marshal(body); !strings.Contains(string(raw), "example.com/a.png") {
		t.Fatalf("图片内容丢失: %s", raw)
	}
}

// TestUnsupportedTargetMessageRoleRejectedBeforeSend 确认放行只覆盖
// system / developer：其他角色要在构建阶段失败，而不是送出去换一个
// 不指向字段的上游 400。
func TestUnsupportedTargetMessageRoleRejectedBeforeSend(t *testing.T) {
	in := &Internal{
		Model:     "glm-5.2",
		MaxTokens: 64,
		Messages: []any{
			map[string]any{"role": "tool", "content": []any{
				map[string]any{"type": "text", "text": "结果"},
			}},
		},
	}
	if _, err := ToAnthropicBodyChecked(in, "glm-5.2"); err == nil || !strings.Contains(err.Error(), `role "tool"`) {
		t.Fatalf("Anthropic 目标 err = %v, want unsupported role \"tool\"", err)
	}
	if _, err := ToOpenAIChatBodyChecked(in, "glm-5.2"); err == nil || !strings.Contains(err.Error(), `role "tool"`) {
		t.Fatalf("Chat 目标 err = %v, want unsupported role \"tool\"", err)
	}
}

// TestUnknownContentBlockStillRejected 确认这次放行只针对思考块，
// 其他不认识的块仍要在转换前报错，不能借机把 deny-list 打穿。
func TestUnknownContentBlockStillRejected(t *testing.T) {
	in := FromAnthropic(map[string]any{
		"model": "claude-opus-5",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "some_future_block", "data": "x"},
			}},
		},
	})
	if _, err := ToOpenAIChatBodyChecked(in, "glm-5.2"); err == nil || !strings.Contains(err.Error(), "some_future_block") {
		t.Fatalf("Chat 目标 err = %v, want unsupported some_future_block", err)
	}
	if _, err := ToOpenAIResponsesBodyChecked(in, "glm-5.3"); err == nil || !strings.Contains(err.Error(), "some_future_block") {
		t.Fatalf("Responses 目标 err = %v, want unsupported some_future_block", err)
	}
}

// TestOverrideMaxTokensByProviderFormat 验证按 provider 格式改写正确的字段名。
func TestOverrideMaxTokensByProviderFormat(t *testing.T) {
	tests := []struct {
		name           string
		providerFormat string
		body           map[string]any
		maxTokens      int
		wantKey        string
		wantValue      any
		wantAbsentKeys []string
	}{
		{
			name:           "anthropic 用 max_tokens",
			providerFormat: "anthropic",
			body:           map[string]any{"max_tokens": 4096},
			maxTokens:      32768,
			wantKey:        "max_tokens",
			wantValue:      32768,
		},
		{
			name:           "openai-responses 用 max_output_tokens",
			providerFormat: "openai-responses",
			body:           map[string]any{"max_output_tokens": 4096},
			maxTokens:      32768,
			wantKey:        "max_output_tokens",
			wantValue:      32768,
		},
		{
			name:           "openai chat 默认写 max_tokens",
			providerFormat: "openai",
			body:           map[string]any{"max_tokens": 4096},
			maxTokens:      16384,
			wantKey:        "max_tokens",
			wantValue:      16384,
			// 不能顺手补上 max_completion_tokens：部分上游把两者视为互斥字段，同时出现直接 400
			wantAbsentKeys: []string{"max_completion_tokens"},
		},
		{
			name:           "openai chat 客户端带 max_completion_tokens 时只改它",
			providerFormat: "openai",
			body:           map[string]any{"max_completion_tokens": 4096},
			maxTokens:      16384,
			wantKey:        "max_completion_tokens",
			wantValue:      16384,
			wantAbsentKeys: []string{"max_tokens"},
		},
		{
			name:           "硬覆盖：客户端值比配置大也照改",
			providerFormat: "anthropic",
			body:           map[string]any{"max_tokens": 200000},
			maxTokens:      8192,
			wantKey:        "max_tokens",
			wantValue:      8192,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			OverrideMaxTokens(tt.body, tt.providerFormat, tt.maxTokens)
			if got := tt.body[tt.wantKey]; got != tt.wantValue {
				t.Fatalf("body[%q] = %#v，期望 %#v", tt.wantKey, got, tt.wantValue)
			}
			for _, key := range tt.wantAbsentKeys {
				if _, exists := tt.body[key]; exists {
					t.Fatalf("body 不应含 %q，实际 = %#v", key, tt.body[key])
				}
			}
		})
	}
}

// maxTokens <= 0 或 body 为 nil 时必须完全不动，避免把「未配置」写成非法值。
func TestOverrideMaxTokensIgnoresNonPositiveAndNilBody(t *testing.T) {
	body := map[string]any{"max_tokens": 4096}
	OverrideMaxTokens(body, "anthropic", 0)
	if body["max_tokens"] != 4096 {
		t.Fatalf("maxTokens=0 时被改写: %#v", body["max_tokens"])
	}
	OverrideMaxTokens(body, "anthropic", -1)
	if body["max_tokens"] != 4096 {
		t.Fatalf("maxTokens=-1 时被改写: %#v", body["max_tokens"])
	}
	OverrideMaxTokens(nil, "anthropic", 32768) // 不能 panic
}

// 客户端未传输出上限时，三种客户端格式都应落到全局默认 32768。
// 4096 是曾把 Codex 推进死循环的旧默认值，这条测试就是防它回归。
func TestClientOmittedMaxTokensDefaultsTo32768(t *testing.T) {
	tests := []struct {
		name string
		in   *Internal
	}{
		{"anthropic", FromAnthropic(map[string]any{
			"model": "claude", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})},
		{"openai-chat", FromOpenAIChat(map[string]any{
			"model": "gpt", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})},
		{"openai-responses", FromOpenAIResponses(map[string]any{
			"model": "gpt", "input": []any{map[string]any{"role": "user", "content": "hi"}},
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.in.MaxTokens != 32768 {
				t.Fatalf("MaxTokens = %d，期望 32768", tt.in.MaxTokens)
			}
		})
	}
}

// TestEnsureMaxTokensOnlyFillsWhenAbsent 验证优先级链的最后一环：
// 客户端传了就保留客户端的值（客户端传入值 > 全局默认），一个都没传才补默认值。
func TestEnsureMaxTokensOnlyFillsWhenAbsent(t *testing.T) {
	tests := []struct {
		name           string
		providerFormat string
		body           map[string]any
		wantBody       map[string]any
	}{
		{
			name: "anthropic 缺失则补默认", providerFormat: "anthropic",
			body:     map[string]any{},
			wantBody: map[string]any{"max_tokens": DefaultMaxTokens},
		},
		{
			name: "anthropic 客户端传了就不动", providerFormat: "anthropic",
			body:     map[string]any{"max_tokens": 64000},
			wantBody: map[string]any{"max_tokens": 64000},
		},
		{
			name: "anthropic 客户端传的比默认小也保留", providerFormat: "anthropic",
			body:     map[string]any{"max_tokens": 512},
			wantBody: map[string]any{"max_tokens": 512},
		},
		{
			name: "responses 缺失则补默认", providerFormat: "openai-responses",
			body:     map[string]any{},
			wantBody: map[string]any{"max_output_tokens": DefaultMaxTokens},
		},
		{
			name: "responses 客户端传了就不动", providerFormat: "openai-responses",
			body:     map[string]any{"max_output_tokens": 8000},
			wantBody: map[string]any{"max_output_tokens": 8000},
		},
		{
			name: "openai-chat 两个键都没有则补 max_tokens", providerFormat: "openai",
			body:     map[string]any{},
			wantBody: map[string]any{"max_tokens": DefaultMaxTokens},
		},
		{
			// 补第二个键会被部分上游判为互斥字段冲突而 400
			name: "openai-chat 已有 max_completion_tokens 则不补 max_tokens", providerFormat: "openai",
			body:     map[string]any{"max_completion_tokens": 8000},
			wantBody: map[string]any{"max_completion_tokens": 8000},
		},
		{
			name: "openai-chat 已有 max_tokens 则不动", providerFormat: "openai",
			body:     map[string]any{"max_tokens": 8000},
			wantBody: map[string]any{"max_tokens": 8000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			EnsureMaxTokens(tt.body, tt.providerFormat)
			if len(tt.body) != len(tt.wantBody) {
				t.Fatalf("body 键数 = %d，期望 %d: %#v", len(tt.body), len(tt.wantBody), tt.body)
			}
			for key, want := range tt.wantBody {
				if got := tt.body[key]; got != want {
					t.Fatalf("body[%q] = %#v，期望 %#v", key, got, want)
				}
			}
		})
	}
	EnsureMaxTokens(nil, "anthropic") // 不能 panic
}

func TestLimitMaxTokensClampsOnlyWhenBudgetRequiresIt(t *testing.T) {
	tests := []struct {
		name           string
		providerFormat string
		body           map[string]any
		budget         int
		want           map[string]any
	}{
		{
			name:           "anthropic 压制客户端值",
			providerFormat: "anthropic",
			body:           map[string]any{"max_tokens": float64(64000)},
			budget:         8000,
			want:           map[string]any{"max_tokens": 8000},
		},
		{
			name:           "anthropic 保留较小客户端值",
			providerFormat: "anthropic",
			body:           map[string]any{"max_tokens": float64(4000)},
			budget:         8000,
			want:           map[string]any{"max_tokens": float64(4000)},
		},
		{
			name:           "缺失字段使用预算",
			providerFormat: "openai-responses",
			body:           map[string]any{},
			budget:         8000,
			want:           map[string]any{"max_output_tokens": 8000},
		},
		{
			name:           "缺失字段不抬高全局默认",
			providerFormat: "anthropic",
			body:           map[string]any{},
			budget:         65536,
			want:           map[string]any{"max_tokens": DefaultMaxTokens},
		},
		{
			name:           "chat 只改 max_completion_tokens",
			providerFormat: "openai",
			body:           map[string]any{"max_completion_tokens": float64(64000)},
			budget:         8000,
			want:           map[string]any{"max_completion_tokens": 8000},
		},
		{
			name:           "chat 默认使用 max_tokens",
			providerFormat: "openai",
			body:           map[string]any{},
			budget:         8000,
			want:           map[string]any{"max_tokens": 8000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			LimitMaxTokens(tt.body, tt.providerFormat, tt.budget)
			if !reflect.DeepEqual(tt.body, tt.want) {
				t.Fatalf("LimitMaxTokens() = %#v，期望 %#v", tt.body, tt.want)
			}
		})
	}
	body := map[string]any{"max_tokens": 64000}
	LimitMaxTokens(body, "anthropic", 0)
	if !reflect.DeepEqual(body, map[string]any{"max_tokens": 64000}) {
		t.Fatalf("budget=0 不应改写请求体: %#v", body)
	}
}
