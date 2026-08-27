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
