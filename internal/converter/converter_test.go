package converter

import (
	"encoding/json"
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

	t.Run("Chat 到 Anthropic 映射工具选择和并行开关", func(t *testing.T) {
		in := FromOpenAIChat(map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "你好"}},
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
	tests := []struct {
		name string
		in   *Internal
		want any
	}{
		{
			name: "Anthropic 指定工具",
			in: FromAnthropic(map[string]any{
				"messages":    []any{map[string]any{"role": "user", "content": "你好"}},
				"tool_choice": map[string]any{"type": "tool", "name": "lookup"},
			}),
			want: map[string]any{"type": "function", "name": "lookup"},
		},
		{
			name: "Chat 指定函数",
			in: FromOpenAIChat(map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "你好"}},
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
				"tool_choice": "auto",
			}),
			want: "auto",
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
			name: "responses",
			in: FromOpenAIResponses(map[string]any{"tools": []any{
				map[string]any{"type": "function", "name": "ok", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "web_search_preview"},
			}}),
		},
		{
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
