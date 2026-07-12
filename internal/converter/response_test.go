package converter

import (
	"strings"
	"testing"
)

func TestNonStreamTruncationReasonMappings(t *testing.T) {
	anthropic := map[string]any{
		"model": "claude-upstream", "content": []any{map[string]any{"type": "text", "text": "partial"}},
		"stop_reason": "max_tokens", "usage": map[string]any{"input_tokens": 1, "output_tokens": 2},
	}
	chat := ConvertAnthropicResponse(anthropic, "openai-chat", "client-model")
	if got := firstChoice(chat)["finish_reason"]; got != "length" {
		t.Fatalf("Anthropic max_tokens -> Chat finish_reason = %#v, want length", got)
	}
	assertIncompleteResponse(t, ConvertAnthropicResponse(anthropic, "openai-responses", "client-model"))

	openAI := map[string]any{
		"model": "gpt-upstream",
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "partial"}, "finish_reason": "length",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
	}
	converted := ConvertOpenAIChatResponse(openAI, "anthropic", "client-model")
	if got := converted["stop_reason"]; got != "max_tokens" {
		t.Fatalf("Chat length -> Anthropic stop_reason = %#v, want max_tokens", got)
	}
	assertIncompleteResponse(t, ConvertOpenAIChatResponse(openAI, "openai-responses", "client-model"))
}

func TestNonStreamStopReasonMappingsStayWithinTargetEnums(t *testing.T) {
	for _, source := range []string{"stop_sequence", "refusal", "unknown_reason"} {
		converted := ConvertAnthropicResponse(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "done"}}, "stop_reason": source,
		}, "openai-chat", "client-model")
		if got := firstChoice(converted)["finish_reason"]; got != "stop" {
			t.Fatalf("Anthropic %q -> Chat finish_reason = %#v, want stop", source, got)
		}
	}

	for source, want := range map[string]string{
		"content_filter": "refusal",
		"unknown_reason": "end_turn",
	} {
		converted := ConvertOpenAIChatResponse(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "done"}, "finish_reason": source}},
		}, "anthropic", "client-model")
		if got := converted["stop_reason"]; got != want {
			t.Fatalf("Chat %q -> Anthropic stop_reason = %#v, want %q", source, got, want)
		}
	}
}

func assertIncompleteResponse(t *testing.T, response map[string]any) {
	t.Helper()
	if response["status"] != "incomplete" {
		t.Fatalf("response status = %#v, want incomplete", response["status"])
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %#v", details)
	}
}

func TestAnthropicToolUseResponseToOpenAIChat(t *testing.T) {
	data := map[string]any{
		"model": "claude-upstream",
		"content": []any{
			map[string]any{"type": "text", "text": "Checking."},
			map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 4, "output_tokens": 7},
	}

	got := ConvertAnthropicResponse(data, "openai-chat", "client-model")
	choice := firstChoice(got)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "Checking." {
		t.Fatalf("content = %#v", message["content"])
	}
	requireJSONEqual(t, []any{map[string]any{
		"id": "call_weather", "type": "function",
		"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
	}}, message["tool_calls"])
}

func TestOpenAIChatToolCallsResponseToAnthropic(t *testing.T) {
	data := map[string]any{
		"model": "gpt-upstream",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": "Checking.",
				"tool_calls": []any{map[string]any{
					"id": "call_weather", "type": "function",
					"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 7},
	}

	got := ConvertOpenAIChatResponse(data, "anthropic", "client-model")
	if got["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %#v, want tool_use", got["stop_reason"])
	}
	requireJSONEqual(t, []any{
		map[string]any{"type": "text", "text": "Checking."},
		map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
	}, got["content"])
}

func TestToolCallResponsesContainFunctionCallOutputItems(t *testing.T) {
	inputs := []struct {
		name string
		got  map[string]any
	}{
		{
			name: "anthropic",
			got: ConvertAnthropicResponse(map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "Checking."},
					map[string]any{"type": "tool_use", "id": "call_weather", "name": "weather", "input": map[string]any{"city": "Paris"}},
				},
			}, "openai-responses", "client-model"),
		},
		{
			name: "openai-chat",
			got: ConvertOpenAIChatResponse(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{
					"content": "Checking.",
					"tool_calls": []any{map[string]any{
						"id": "call_weather", "type": "function",
						"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`},
					}},
				}}},
			}, "openai-responses", "client-model"),
		},
	}

	for _, tt := range inputs {
		t.Run(tt.name, func(t *testing.T) {
			output, _ := tt.got["output"].([]any)
			if len(output) != 2 {
				t.Fatalf("output length = %d, want text message and function call: %#v", len(output), output)
			}
			message, _ := output[0].(map[string]any)
			content, _ := message["content"].([]any)
			requireJSONEqual(t, []any{map[string]any{"type": "output_text", "text": "Checking."}}, content)
			functionCall, _ := output[1].(map[string]any)
			if functionCall["type"] != "function_call" || functionCall["call_id"] != "call_weather" || functionCall["name"] != "weather" || functionCall["arguments"] != `{"city":"Paris"}` {
				t.Fatalf("function call = %#v", functionCall)
			}
		})
	}
}

func TestOrdinaryEmptyTextResponsesKeepMessageOutput(t *testing.T) {
	responses := []struct {
		name string
		got  map[string]any
	}{
		{
			name: "anthropic",
			got: ConvertAnthropicResponse(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": ""}},
			}, "openai-responses", "client-model"),
		},
		{
			name: "openai-chat",
			got: ConvertOpenAIChatResponse(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": ""}}},
			}, "openai-responses", "client-model"),
		},
	}

	for _, tt := range responses {
		t.Run(tt.name, func(t *testing.T) {
			output, _ := tt.got["output"].([]any)
			if len(output) != 1 {
				t.Fatalf("output = %#v, want one empty text message", output)
			}
			message, _ := output[0].(map[string]any)
			requireJSONEqual(t, []any{map[string]any{"type": "output_text", "text": ""}}, message["content"])
		})
	}
}

func TestCheckedResponsesRejectNonObjectFunctionArguments(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "chat-malformed-to-anthropic",
			run: func() error {
				_, err := ConvertOpenAIChatResponseChecked(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{
						"tool_calls": []any{map[string]any{
							"id": "call_bad", "type": "function",
							"function": map[string]any{"name": "bad", "arguments": `{"x":`},
						}},
					}}},
				}, "anthropic", "client-model")
				return err
			},
		},
		{
			name: "chat-array-to-responses",
			run: func() error {
				_, err := ConvertOpenAIChatResponseChecked(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{
						"tool_calls": []any{map[string]any{
							"id": "call_bad", "type": "function",
							"function": map[string]any{"name": "bad", "arguments": `[]`},
						}},
					}}},
				}, "openai-responses", "client-model")
				return err
			},
		},
		{
			name: "anthropic-array-to-chat",
			run: func() error {
				_, err := ConvertAnthropicResponseChecked(map[string]any{
					"content": []any{map[string]any{
						"type": "tool_use", "id": "call_bad", "name": "bad", "input": []any{},
					}},
				}, "openai-chat", "client-model")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil || !strings.Contains(err.Error(), "arguments") || !strings.Contains(err.Error(), "object") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCheckedResponsesRejectUnknownContentBlocks(t *testing.T) {
	data := map[string]any{
		"content": []any{map[string]any{"type": "thinking", "thinking": "secret"}},
	}
	for _, target := range []string{"openai-chat", "openai-responses"} {
		t.Run("anthropic-to-"+target, func(t *testing.T) {
			_, err := ConvertAnthropicResponseChecked(data, target, "client-model")
			if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "thinking") {
				t.Fatalf("error = %v", err)
			}
			requireJSONEqual(t, data, ConvertAnthropicResponse(data, target, "client-model"))
		})
	}

	chatData := map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"content": []any{map[string]any{"type": "refusal", "refusal": "no"}},
		}}},
	}
	for _, target := range []string{"anthropic", "openai-responses"} {
		t.Run("chat-to-"+target, func(t *testing.T) {
			_, err := ConvertOpenAIChatResponseChecked(chatData, target, "client-model")
			if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "refusal") {
				t.Fatalf("error = %v", err)
			}
			requireJSONEqual(t, chatData, ConvertOpenAIChatResponse(chatData, target, "client-model"))
		})
	}
}

func TestCheckedResponsesRejectTopLevelChatRefusal(t *testing.T) {
	data := map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": nil, "refusal": "I cannot help with that.",
		}}},
	}
	for _, target := range []string{"anthropic", "openai-responses"} {
		t.Run(target, func(t *testing.T) {
			_, err := ConvertOpenAIChatResponseChecked(data, target, "client-model")
			if err == nil || !strings.Contains(err.Error(), "refusal") {
				t.Fatalf("error = %v", err)
			}
			requireJSONEqual(t, data, ConvertOpenAIChatResponse(data, target, "client-model"))
		})
	}
}
