package converter

import (
	"encoding/json"
	"strings"
	"testing"
)

type capturedSSE struct {
	event string
	data  map[string]any
}

func transformAll(t *testing.T, transformer StreamTransformer, lines ...string) []string {
	t.Helper()
	var out []string
	for _, line := range lines {
		out = append(out, transformer.Transform(line)...)
	}
	return out
}

func captureEvents(t *testing.T, output []string) []capturedSSE {
	t.Helper()
	var events []capturedSSE
	for _, chunk := range output {
		var event string
		var raw string
		for _, line := range strings.Split(strings.TrimSpace(chunk), "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				raw = strings.TrimPrefix(line, "data: ")
			}
		}
		if event == "" || raw == "" || raw == "[DONE]" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatalf("decode event %q: %v", event, err)
		}
		events = append(events, capturedSSE{event: event, data: data})
	}
	return events
}

func findEvent(t *testing.T, events []capturedSSE, name string) map[string]any {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].event == name {
			return events[i].data
		}
	}
	t.Fatalf("event %q not found in %#v", name, events)
	return nil
}

func captureChatChunks(t *testing.T, output []string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, chunk := range output {
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(chunk), "data: "))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatalf("decode chat chunk: %v", err)
		}
		chunks = append(chunks, data)
	}
	return chunks
}

func TestAnthropicInputJSONDeltaMapsToOpenAIChatToolCalls(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("anthropic", "openai-chat"),
		`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking. "}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_weather","name":"weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	)

	chunks := captureChatChunks(t, out)
	var toolDeltas []any
	var arguments strings.Builder
	for _, chunk := range chunks {
		delta, _, _ := firstChoiceDelta(chunk)
		calls, _ := delta["tool_calls"].([]any)
		for _, callValue := range calls {
			call, _ := callValue.(map[string]any)
			toolDeltas = append(toolDeltas, call)
			function, _ := call["function"].(map[string]any)
			arguments.WriteString(getString(function, "arguments"))
		}
	}
	if len(toolDeltas) < 3 {
		t.Fatalf("tool call deltas = %#v", toolDeltas)
	}
	first, _ := toolDeltas[0].(map[string]any)
	if first["id"] != "call_weather" {
		t.Fatalf("tool call id = %#v", first["id"])
	}
	function, _ := first["function"].(map[string]any)
	if function["name"] != "weather" {
		t.Fatalf("tool name = %#v", function["name"])
	}
	if arguments.String() != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q", arguments.String())
	}
}

func TestOpenAIToolCallDeltasMapToAnthropicInputJSONDelta(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("openai", "anthropic"),
		`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"content":"Checking. "},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	events := captureEvents(t, out)
	var toolStart map[string]any
	var arguments strings.Builder
	for _, event := range events {
		if event.event == "content_block_start" {
			block, _ := event.data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolStart = block
			}
		}
		if event.event == "content_block_delta" {
			delta, _ := event.data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				arguments.WriteString(getString(delta, "partial_json"))
			}
		}
	}
	if toolStart == nil || toolStart["id"] != "call_weather" || toolStart["name"] != "weather" {
		t.Fatalf("tool start = %#v", toolStart)
	}
	if arguments.String() != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q", arguments.String())
	}
}

func TestAnthropicToResponsesCompletedContainsFullTextAndFunctionCall(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("anthropic", "openai-responses"),
		`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking. "}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_weather","name":"weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	)

	events := captureEvents(t, out)
	argsDone := findEvent(t, events, "response.function_call_arguments.done")
	if argsDone["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("done arguments = %#v", argsDone["arguments"])
	}
	completed := findEvent(t, events, "response.completed")
	response, _ := completed["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output = %#v", output)
	}
	message, _ := output[0].(map[string]any)
	requireJSONEqual(t, []any{map[string]any{"type": "output_text", "text": "Checking. "}}, message["content"])
	functionCall, _ := output[1].(map[string]any)
	if functionCall["type"] != "function_call" || functionCall["call_id"] != "call_weather" || functionCall["name"] != "weather" || functionCall["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("completed function call = %#v", functionCall)
	}
}

func TestOpenAIToResponsesCompletedContainsFullTextAndFunctionCall(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("openai", "openai-responses"),
		`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"content":"Checking. "},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	events := captureEvents(t, out)
	var argumentDeltas strings.Builder
	for _, event := range events {
		if event.event == "response.function_call_arguments.delta" {
			argumentDeltas.WriteString(getString(event.data, "delta"))
		}
	}
	if argumentDeltas.String() != `{"city":"Paris"}` {
		t.Fatalf("argument deltas = %q", argumentDeltas.String())
	}
	completed := findEvent(t, events, "response.completed")
	response, _ := completed["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output = %#v", output)
	}
	message, _ := output[0].(map[string]any)
	requireJSONEqual(t, []any{map[string]any{"type": "output_text", "text": "Checking. "}}, message["content"])
	functionCall, _ := output[1].(map[string]any)
	if functionCall["call_id"] != "call_weather" || functionCall["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("completed function call = %#v", functionCall)
	}
}

func TestOpenAIToResponsesToolOnlyContentNullOmitsEmptyMessage(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("openai", "openai-responses"),
		`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	completed := findEvent(t, captureEvents(t, out), "response.completed")
	response, _ := completed["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output = %#v, want only function call", output)
	}
	functionCall, _ := output[0].(map[string]any)
	if functionCall["type"] != "function_call" || functionCall["call_id"] != "call_weather" {
		t.Fatalf("function call = %#v", functionCall)
	}
}

func TestOpenAIToolOnlyContentNullDoesNotStartAnthropicTextBlock(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("openai", "anthropic"),
		`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	for _, event := range captureEvents(t, out) {
		if event.event != "content_block_start" {
			continue
		}
		block, _ := event.data["content_block"].(map[string]any)
		if block["type"] == "text" {
			t.Fatalf("unexpected empty text block: %#v", event.data)
		}
	}
}

func TestResponsesSSESequenceNumbersAreContinuous(t *testing.T) {
	cases := []struct {
		name        string
		transformer StreamTransformer
		lines       []string
	}{
		{
			name:        "anthropic",
			transformer: NewStreamTransformer("anthropic", "openai-responses"),
			lines: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name:        "openai",
			transformer: NewStreamTransformer("openai", "openai-responses"),
			lines: []string{
				`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`data: {"model":"gpt-upstream","choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
				`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			events := captureEvents(t, transformAll(t, tt.transformer, tt.lines...))
			if len(events) == 0 {
				t.Fatal("no Responses events")
			}
			for i, event := range events {
				if event.data["sequence_number"] != float64(i) {
					t.Fatalf("event %d (%s) sequence_number = %#v", i, event.event, event.data["sequence_number"])
				}
			}
		})
	}
}

func TestAnthropicMultipleContentBlocksHaveConsistentResponsesDoneAndFinalOutput(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		wantTypes []any
		wantTexts []string
	}{
		{
			name: "text-text",
			lines: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"B"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"message_stop"}`,
			},
			wantTypes: []any{"message", "message"},
			wantTexts: []string{"A", "B"},
		},
		{
			name: "text-tool-text",
			lines: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_weather","name":"weather","input":{}}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"B"}}`,
				`data: {"type":"content_block_stop","index":2}`,
				`data: {"type":"message_stop"}`,
			},
			wantTypes: []any{"message", "function_call", "message"},
			wantTexts: []string{"A", "B"},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			events := captureEvents(t, transformAll(t, NewStreamTransformer("anthropic", "openai-responses"), tt.lines...))
			doneItems := make(map[int]any)
			doneIDs := make(map[string]bool)
			for _, event := range events {
				if event.event == "response.output_item.done" {
					index := valueIndex(event.data["output_index"])
					doneItems[index] = event.data["item"]
					item, _ := event.data["item"].(map[string]any)
					doneIDs[getString(item, "id")] = true
				}
				if strings.HasSuffix(event.event, ".delta") {
					if id := getString(event.data, "item_id"); id != "" && doneIDs[id] {
						t.Fatalf("delta %s emitted after item %s was done", event.event, id)
					}
				}
			}

			completed := findEvent(t, events, "response.completed")
			response, _ := completed["response"].(map[string]any)
			output, _ := response["output"].([]any)
			if len(output) != len(tt.wantTypes) {
				t.Fatalf("completed output = %#v", output)
			}
			var texts []string
			for i, value := range output {
				item, _ := value.(map[string]any)
				if item["type"] != tt.wantTypes[i] {
					t.Fatalf("output[%d].type = %#v", i, item["type"])
				}
				requireJSONEqual(t, doneItems[i], value)
				if item["type"] == "message" {
					content, _ := item["content"].([]any)
					part, _ := content[0].(map[string]any)
					texts = append(texts, getString(part, "text"))
				}
			}
			requireJSONEqual(t, tt.wantTexts, texts)
		})
	}
}

func TestStreamFunctionArgumentsMustBeJSONObject(t *testing.T) {
	tests := []struct {
		name        string
		transformer StreamTransformer
		lines       []string
	}{
		{
			name:        "anthropic-to-responses",
			transformer: NewStreamTransformer("anthropic", "openai-responses"),
			lines: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_bad","name":"bad","input":{}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"[]"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			},
		},
		{
			name:        "openai-to-anthropic",
			transformer: NewStreamTransformer("openai", "anthropic"),
			lines: []string{
				`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"bad","arguments":"[]"}}]},"finish_reason":null}]}`,
				`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transformAll(t, tt.transformer, tt.lines...)
			err := StreamError(tt.transformer)
			if err == nil || !strings.Contains(err.Error(), "arguments") || !strings.Contains(err.Error(), "object") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAnthropicStreamStartToolInputMustBeObject(t *testing.T) {
	for _, target := range []string{"openai-chat", "openai-responses"} {
		t.Run(target, func(t *testing.T) {
			transformer := NewStreamTransformer("anthropic", target)
			chunks := transformer.Transform(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_bad","name":"bad","input":[]}}`)
			if len(chunks) != 0 {
				t.Fatalf("error transform returned normal chunks: %#v", chunks)
			}
			err := StreamError(transformer)
			if err == nil || !strings.Contains(err.Error(), "input") || !strings.Contains(err.Error(), "object") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenAIStreamRejectsTopLevelDeltaRefusal(t *testing.T) {
	for _, target := range []string{"anthropic", "openai-responses"} {
		t.Run(target, func(t *testing.T) {
			transformer := NewStreamTransformer("openai", target)
			chunks := transformer.Transform(`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":null,"refusal":"I cannot help with that."},"finish_reason":null}]}`)
			if len(chunks) != 0 {
				t.Fatalf("refusal transform returned normal chunks: %#v", chunks)
			}
			err := StreamError(transformer)
			if err == nil || !strings.Contains(err.Error(), "refusal") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenAIStreamEmptyStringCreatesTextButNullDoesNot(t *testing.T) {
	t.Run("empty-to-anthropic", func(t *testing.T) {
		transformer := NewStreamTransformer("openai", "anthropic")
		out := transformAll(t, transformer,
			`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		events := captureEvents(t, out)
		var textStart, textStop bool
		for _, event := range events {
			if event.event == "content_block_start" {
				block, _ := event.data["content_block"].(map[string]any)
				textStart = block["type"] == "text"
			}
			if event.event == "content_block_stop" {
				textStop = true
			}
		}
		if !textStart || !textStop {
			t.Fatalf("empty string text lifecycle missing: %#v", events)
		}
	})

	t.Run("empty-to-responses", func(t *testing.T) {
		transformer := NewStreamTransformer("openai", "openai-responses")
		out := transformAll(t, transformer,
			`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		completed := findEvent(t, captureEvents(t, out), "response.completed")
		response, _ := completed["response"].(map[string]any)
		output, _ := response["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("completed output = %#v", output)
		}
		message, _ := output[0].(map[string]any)
		requireJSONEqual(t, []any{map[string]any{"type": "output_text", "text": ""}}, message["content"])
	})

	t.Run("null-without-tool-to-responses", func(t *testing.T) {
		transformer := NewStreamTransformer("openai", "openai-responses")
		out := transformAll(t, transformer,
			`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
			`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		completed := findEvent(t, captureEvents(t, out), "response.completed")
		response, _ := completed["response"].(map[string]any)
		output, _ := response["output"].([]any)
		if len(output) != 0 {
			t.Fatalf("null content created text output: %#v", output)
		}
	})
}

func TestResponsesStreamFailureReusesIDAndNextSequence(t *testing.T) {
	tests := []struct {
		name        string
		transformer StreamTransformer
		before      []string
		errorLine   string
	}{
		{
			name:        "anthropic",
			transformer: NewStreamTransformer("anthropic", "openai-responses"),
			before: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
			},
			errorLine: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_bad","name":"bad","input":[]}}`,
		},
		{
			name:        "openai",
			transformer: NewStreamTransformer("openai", "openai-responses"),
			before: []string{
				`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"bad","arguments":"[]"}}]},"finish_reason":null}]}`,
			},
			errorLine: `data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priorEvents := captureEvents(t, transformAll(t, tt.transformer, tt.before...))
			created := findEvent(t, priorEvents, "response.created")
			createdResponse, _ := created["response"].(map[string]any)
			responseID := getString(createdResponse, "id")

			if chunks := tt.transformer.Transform(tt.errorLine); len(chunks) != 0 {
				t.Fatalf("error transform returned normal chunks: %#v", chunks)
			}
			if StreamError(tt.transformer) == nil {
				t.Fatal("missing stream error")
			}

			failureEvents := captureEvents(t, StreamFailure(tt.transformer))
			if len(failureEvents) != 1 || failureEvents[0].event != "response.failed" {
				t.Fatalf("failure events = %#v", failureEvents)
			}
			failed := failureEvents[0].data
			if failed["sequence_number"] != float64(len(priorEvents)) {
				t.Fatalf("failure sequence = %#v, want %d", failed["sequence_number"], len(priorEvents))
			}
			failedResponse, _ := failed["response"].(map[string]any)
			if getString(failedResponse, "id") != responseID || failedResponse["status"] != "failed" {
				t.Fatalf("failed response = %#v", failedResponse)
			}
			if again := StreamFailure(tt.transformer); len(again) != 0 {
				t.Fatalf("duplicate failure terminal = %#v", again)
			}
			if after := tt.transformer.Transform(`data: [DONE]`); len(after) != 0 {
				t.Fatalf("transform after failure = %#v", after)
			}
		})
	}
}

func TestStreamFailureUsesTargetProtocolWithoutSuccessTerminal(t *testing.T) {
	t.Run("anthropic-target", func(t *testing.T) {
		transformer := NewStreamTransformer("openai", "anthropic")
		transformAll(t, transformer,
			`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"name":"bad","arguments":"[]"}}]},"finish_reason":null}]}`,
		)
		if chunks := transformer.Transform(`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`); len(chunks) != 0 {
			t.Fatalf("error transform returned success terminal: %#v", chunks)
		}
		failure := strings.Join(StreamFailure(transformer), "")
		if !strings.Contains(failure, "event: error") || strings.Contains(failure, "message_stop") || strings.Contains(failure, "message_delta") {
			t.Fatalf("anthropic failure terminal = %q", failure)
		}
	})

	t.Run("chat-target", func(t *testing.T) {
		transformer := NewStreamTransformer("anthropic", "openai-chat")
		if chunks := transformer.Transform(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_bad","name":"bad","input":[]}}`); len(chunks) != 0 {
			t.Fatalf("error transform returned success chunks: %#v", chunks)
		}
		failure := strings.Join(StreamFailure(transformer), "")
		if !strings.Contains(failure, `"error"`) || !strings.Contains(failure, "data: [DONE]") || strings.Contains(failure, "finish_reason") {
			t.Fatalf("chat failure terminal = %q", failure)
		}
	})
}

func TestAbortStreamPreservesExternalFailureType(t *testing.T) {
	tests := []struct {
		name        string
		transformer StreamTransformer
		kind        string
		want        string
	}{
		{name: "chat timeout", transformer: NewStreamTransformer("anthropic", "openai-chat"), kind: "timeout_error", want: `"type":"timeout_error"`},
		{name: "anthropic upstream", transformer: NewStreamTransformer("openai", "anthropic"), kind: "upstream_error", want: `"type":"upstream_error"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := strings.Join(AbortStream(tt.transformer, tt.kind, "external failure"), "")
			if !strings.Contains(failure, tt.want) {
				t.Fatalf("abort failure = %q, want %s", failure, tt.want)
			}
		})
	}
}

func TestStreamTruncationReasonMappings(t *testing.T) {
	t.Run("anthropic to chat", func(t *testing.T) {
		out := transformAll(t, NewStreamTransformer("anthropic", "openai-chat"),
			`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
		)
		if joined := strings.Join(out, ""); !strings.Contains(joined, `"finish_reason":"length"`) {
			t.Fatalf("stream output = %q, want finish_reason length", joined)
		}
	})

	t.Run("chat to anthropic", func(t *testing.T) {
		out := transformAll(t, NewStreamTransformer("openai", "anthropic"),
			`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"length"}]}`,
		)
		if joined := strings.Join(out, ""); !strings.Contains(joined, `"stop_reason":"max_tokens"`) {
			t.Fatalf("stream output = %q, want stop_reason max_tokens", joined)
		}
	})

	tests := []struct {
		name        string
		transformer StreamTransformer
		lines       []string
	}{
		{
			name: "anthropic to responses", transformer: NewStreamTransformer("anthropic", "openai-responses"),
			lines: []string{
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "chat to responses", transformer: NewStreamTransformer("openai", "openai-responses"),
			lines: []string{
				`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"length"}]}`,
				`data: [DONE]`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := findEvent(t, captureEvents(t, transformAll(t, tt.transformer, tt.lines...)), "response.incomplete")
			response, _ := event["response"].(map[string]any)
			assertIncompleteResponse(t, response)
		})
	}
}

func TestStreamStopReasonMappingsStayWithinTargetEnums(t *testing.T) {
	for _, source := range []string{"stop_sequence", "refusal", "unknown_reason"} {
		out := transformAll(t, NewStreamTransformer("anthropic", "openai-chat"),
			`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
			dataLine(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": source}}),
		)
		if joined := strings.Join(out, ""); !strings.Contains(joined, `"finish_reason":"stop"`) {
			t.Fatalf("Anthropic %q stream output = %q", source, joined)
		}
	}

	for source, want := range map[string]string{"content_filter": "refusal", "unknown_reason": "end_turn"} {
		out := transformAll(t, NewStreamTransformer("openai", "anthropic"),
			`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			dataLine(map[string]any{"model": "gpt-upstream", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": source}}}),
		)
		if joined := strings.Join(out, ""); !strings.Contains(joined, `"stop_reason":"`+want+`"`) {
			t.Fatalf("Chat %q stream output = %q", source, joined)
		}
	}
}

func TestStreamAggregateBufferLimit(t *testing.T) {
	transformer := NewStreamTransformer("openai", "openai-responses")
	transformer.Transform(`data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`)
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 33 && StreamError(transformer) == nil; i++ {
		transformer.Transform(dataLine(map[string]any{
			"model":   "gpt-upstream",
			"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}, "finish_reason": nil}},
		}))
	}
	if err := StreamError(transformer); err == nil || !strings.Contains(err.Error(), "累计") {
		t.Fatalf("aggregate stream error = %v, want cumulative size rejection", err)
	}
}
