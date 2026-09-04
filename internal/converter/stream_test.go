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

func TestResponsesStreamMapsToChatAndAnthropic(t *testing.T) {
	lines := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-upstream","status":"in_progress","output":[]}}`,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-upstream","status":"completed","output":[]}}`,
	}

	chatOut := transformAll(t, NewStreamTransformer("openai-responses", "openai-chat"), lines...)
	chatChunks := captureChatChunks(t, chatOut)
	if len(chatChunks) < 3 {
		t.Fatalf("chat chunks = %#v", chatChunks)
	}
	var sawText, sawStop bool
	for _, chunk := range chatChunks {
		delta, finish, _ := firstChoiceDelta(chunk)
		if delta["content"] == "Hello" {
			sawText = true
		}
		if finish == "stop" {
			sawStop = true
		}
	}
	if !sawText || !sawStop {
		t.Fatalf("responses → chat text/stop = %v/%v; %#v", sawText, sawStop, chatChunks)
	}

	anthropicOut := transformAll(t, NewStreamTransformer("openai-responses", "anthropic"), lines...)
	events := captureEvents(t, anthropicOut)
	if findEvent(t, events, "message_start") == nil || findEvent(t, events, "message_delta") == nil || findEvent(t, events, "message_stop") == nil {
		t.Fatalf("responses → anthropic events = %#v", events)
	}
	foundText := false
	for _, event := range events {
		if event.event != "content_block_delta" {
			continue
		}
		delta, _ := event.data["delta"].(map[string]any)
		if delta["text"] == "Hello" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("responses → anthropic missing text delta: %#v", events)
	}
}

func TestResponsesOutputItemDoneStartsToolBeforeArguments(t *testing.T) {
	line := `data: {"type":"response.output_item.done","output_index":3,"item":{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"city\":\"Paris\"}"}}`

	t.Run("Anthropic", func(t *testing.T) {
		events := captureEvents(t, transformAll(t, NewStreamTransformer("openai-responses", "anthropic"), line))
		startAt, deltaAt := -1, -1
		var startIndex, deltaIndex any
		for i, event := range events {
			switch event.event {
			case "content_block_start":
				block, _ := event.data["content_block"].(map[string]any)
				if block["type"] == "tool_use" {
					startAt, startIndex = i, event.data["index"]
				}
			case "content_block_delta":
				delta, _ := event.data["delta"].(map[string]any)
				if delta["type"] == "input_json_delta" {
					deltaAt, deltaIndex = i, event.data["index"]
				}
			}
		}
		if startAt < 0 || deltaAt < 0 || startAt >= deltaAt || startIndex != deltaIndex {
			t.Fatalf("工具 start/delta 顺序或索引错误：%#v", events)
		}
	})

	t.Run("Chat", func(t *testing.T) {
		chunks := captureChatChunks(t, transformAll(t, NewStreamTransformer("openai-responses", "openai-chat"), line))
		startAt, argumentsAt := -1, -1
		for i, chunk := range chunks {
			delta, _, _ := firstChoiceDelta(chunk)
			calls, _ := delta["tool_calls"].([]any)
			for _, value := range calls {
				call, _ := value.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if call["id"] == "call_lookup" && function["name"] == "lookup" {
					startAt = i
				}
				if getString(function, "arguments") != "" {
					argumentsAt = i
				}
			}
		}
		if startAt < 0 || argumentsAt < 0 || startAt >= argumentsAt {
			t.Fatalf("Chat 工具首块/参数顺序错误：%#v", chunks)
		}
	})
}

// 此测试应配合 go test -count=20 运行，避免 map 迭代的偶发顺序掩盖问题。
func TestResponsesStreamTextBlocksStopInCreationOrder(t *testing.T) {
	out := transformAll(t, NewStreamTransformer("openai-responses", "anthropic"),
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"第一段"}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"第二段"}`,
		`data: {"type":"response.completed"}`,
	)
	events := captureEvents(t, out)
	var stops []int
	for _, event := range events {
		if event.event != "content_block_stop" {
			continue
		}
		index, ok := event.data["index"].(float64)
		if !ok {
			t.Fatalf("content_block_stop 索引 = %#v", event.data["index"])
		}
		stops = append(stops, int(index))
	}
	if len(stops) != 2 || stops[0] != 0 || stops[1] != 1 {
		t.Fatalf("content_block_stop 顺序 = %v，期望 [0 1]", stops)
	}
}

func TestResponsesFunctionArgumentsDoneWithoutOutputItemFails(t *testing.T) {
	line := `data: {"type":"response.function_call_arguments.done","output_index":4,"arguments":"{\"city\":\"Paris\"}"}`
	for _, target := range []string{"anthropic", "openai-chat"} {
		t.Run(target, func(t *testing.T) {
			transformer := NewStreamTransformer("openai-responses", target)
			if out := transformer.Transform(line); len(out) != 0 {
				t.Fatalf("错误事件不能产生正常输出：%#v", out)
			}
			if err := StreamError(transformer); err == nil {
				t.Fatal("缺少流式转换错误")
			}
			failure := strings.Join(StreamFailure(transformer), "")
			if failure == "" {
				t.Fatal("缺少目标协议失败终态")
			}
			if target == "anthropic" && !strings.Contains(failure, "event: error") {
				t.Fatalf("Anthropic 失败终态 = %q", failure)
			}
			if target == "openai-chat" && (!strings.Contains(failure, `"error"`) || !strings.Contains(failure, "data: [DONE]")) {
				t.Fatalf("Chat 失败终态 = %q", failure)
			}
		})
	}
}

func TestResponsesStreamRejectsUnsupportedSemanticEvents(t *testing.T) {
	for _, target := range []string{"openai-chat", "anthropic"} {
		t.Run(target, func(t *testing.T) {
			transformer := NewStreamTransformer("openai-responses", target)
			transformer.Transform(`data: {"type":"response.created","response":{"model":"gpt-upstream","status":"in_progress","output":[]}}`)
			// 用真正没有映射的事件。reasoning 相关事件现在是已知的，
			// 见 TestResponsesReasoningStreamMapsPerTarget。
			transformer.Transform(`data: {"type":"response.image_generation_call.partial_image","output_index":0}`)
			if err := StreamError(transformer); err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("StreamError() = %v", err)
			}
			failure := StreamFailure(transformer)
			if len(failure) == 0 {
				t.Fatal("unsupported Responses event must generate target failure terminal")
			}
		})
	}
}

// TestResponsesReasoningStreamMapsPerTarget 覆盖 r00178 那条链路：
// openai-chat 客户端 + openai-responses 上游，上游返回 reasoning item。
//
// 两个目标处理不同，都要锁住：
//   - Chat：映射成 reasoning_content（与 Anthropic→Chat 对称）
//   - Anthropic：丢弃。伪造 thinking 块要编造 signature，客户端把这段 assistant
//     历史回传给 Anthropic 上游时验签会失败；报错则让任何带推理的上游都不可用。
func TestResponsesReasoningStreamMapsPerTarget(t *testing.T) {
	lines := []string{
		`data: {"type":"response.created","response":{"model":"gpt-upstream","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"先拆解问题。"}`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"先拆解问题。"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"先拆解问题。"}]}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案是 42"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"答案是 42"}]}}`,
		`data: {"type":"response.completed","response":{"model":"gpt-upstream","status":"completed","output":[]}}`,
	}

	t.Run("openai-chat", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "openai-chat")
		out := transformAll(t, transformer, lines...)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("stream error = %v, want nil", err)
		}
		var reasoning, content string
		for _, chunk := range captureChatChunks(t, out) {
			choices, _ := chunk["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if value, ok := delta["reasoning_content"].(string); ok {
				reasoning += value
			}
			if value, ok := delta["content"].(string); ok {
				content += value
			}
		}
		// 增量已经推过，output_item.done 里的完整 summary 不能再推一遍。
		if reasoning != "先拆解问题。" {
			t.Fatalf("reasoning_content = %q, want 先拆解问题。（不得重复）", reasoning)
		}
		if content != "答案是 42" {
			t.Fatalf("content = %q, want 答案是 42", content)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "anthropic")
		out := transformAll(t, transformer, lines...)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("stream error = %v, want nil", err)
		}
		joined := strings.Join(out, "")
		if strings.Contains(joined, "先拆解问题。") {
			t.Fatalf("Anthropic 目标不应带出推理内容: %s", joined)
		}
		if strings.Contains(joined, "thinking") {
			t.Fatalf("Anthropic 目标不应伪造 thinking 块: %s", joined)
		}
		if !strings.Contains(joined, "答案是 42") {
			t.Fatalf("正文丢失: %s", joined)
		}
	})
}

// TestResponsesReasoningTextPartMapsPerTarget 覆盖 r00113 那条链路：
// deepseek-v4-flash 用的是 content 数组 + reasoning_text 这套较新约定，
// 而不是 summary 数组 + summary_text。此前 content_part 分支只放行
// output_text / refusal，reasoning_text 落进 default 报
// `unsupported responses stream content part "reasoning_text"`。
func TestResponsesReasoningTextPartMapsPerTarget(t *testing.T) {
	lines := []string{
		`data: {"type":"response.created","response":{"model":"deepseek-v4-flash","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","content":[]}}`,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":""}}`,
		`data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"先拆解问题。"}`,
		`data: {"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"先拆解问题。"}`,
		`data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"先拆解问题。"}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"先拆解问题。"}]}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
		`data: {"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案是 42"}`,
		`data: {"type":"response.completed","response":{"model":"deepseek-v4-flash","status":"completed","output":[]}}`,
	}

	t.Run("openai-chat", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "openai-chat")
		out := transformAll(t, transformer, lines...)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("stream error = %v, want nil", err)
		}
		var reasoning, content string
		for _, chunk := range captureChatChunks(t, out) {
			choices, _ := chunk["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if value, ok := delta["reasoning_content"].(string); ok {
				reasoning += value
			}
			if value, ok := delta["content"].(string); ok {
				content += value
			}
		}
		// 增量已推过，content_part.done 与 output_item.done 里的全文都不能再推。
		if reasoning != "先拆解问题。" {
			t.Fatalf("reasoning_content = %q, want 先拆解问题。（不得重复）", reasoning)
		}
		if content != "答案是 42" {
			t.Fatalf("content = %q, want 答案是 42", content)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "anthropic")
		out := transformAll(t, transformer, lines...)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("stream error = %v, want nil", err)
		}
		joined := strings.Join(out, "")
		if strings.Contains(joined, "先拆解问题。") {
			t.Fatalf("Anthropic 目标不应带出推理内容: %s", joined)
		}
		if !strings.Contains(joined, "答案是 42") {
			t.Fatalf("正文丢失: %s", joined)
		}
	})
}

// 只有 content_part 事件、没有 reasoning_text.delta 的上游，正文得从 part.text 补出来。
func TestResponsesReasoningTextOnlyInPartIsStillMapped(t *testing.T) {
	transformer := NewStreamTransformer("openai-responses", "openai-chat")
	out := transformAll(t, transformer,
		`data: {"type":"response.created","response":{"model":"gpt-upstream","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","content":[]}}`,
		`data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"只在 part 里给"}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案"}`,
		`data: {"type":"response.completed","response":{"model":"gpt-upstream","status":"completed","output":[]}}`,
	)
	if err := StreamError(transformer); err != nil {
		t.Fatalf("stream error = %v, want nil", err)
	}
	if !strings.Contains(strings.Join(out, ""), "只在 part 里给") {
		t.Fatalf("part.text 未补发: %s", strings.Join(out, ""))
	}
}

// 有些上游不发增量、只在 output_item.done 里给完整 summary，这时必须补发。
func TestResponsesReasoningOnlyInDoneIsStillMapped(t *testing.T) {
	transformer := NewStreamTransformer("openai-responses", "openai-chat")
	out := transformAll(t, transformer,
		`data: {"type":"response.created","response":{"model":"gpt-upstream","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"只在 done 里给"}]}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案"}`,
		`data: {"type":"response.completed","response":{"model":"gpt-upstream","status":"completed","output":[]}}`,
	)
	if err := StreamError(transformer); err != nil {
		t.Fatalf("stream error = %v, want nil", err)
	}
	if !strings.Contains(strings.Join(out, ""), "只在 done 里给") {
		t.Fatalf("done 里的 summary 未补发: %s", strings.Join(out, ""))
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

// TestResponsesStreamCarriesUpstreamUsageToAnthropic 锁住 message_delta 里的 usage
// 必须来自上游终态事件。Anthropic 协议里这个字段必有，硬编码 0 会让客户端的
// 成本核算与配额统计全部失真。
func TestResponsesStreamCarriesUpstreamUsageToAnthropic(t *testing.T) {
	cases := []struct {
		name  string
		final string
		want  float64
	}{
		{
			name:  "completed 带 usage",
			final: `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":42}}}`,
			want:  42,
		},
		{
			name:  "incomplete 带 usage",
			final: `data: {"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","output":[],"usage":{"input_tokens":11,"output_tokens":7}}}`,
			want:  7,
		},
		{
			name:  "上游未给 usage 时退回 0",
			final: `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}`,
			want:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := transformAll(t, NewStreamTransformer("openai-responses", "anthropic"),
				`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
				tc.final,
			)
			delta := findEvent(t, captureEvents(t, out), "message_delta")
			usage, ok := delta["usage"].(map[string]any)
			if !ok {
				t.Fatalf("message_delta 缺少 usage 字段: %#v", delta)
			}
			if got := usage["output_tokens"]; got != tc.want {
				t.Fatalf("output_tokens = %v, want %v", got, tc.want)
			}
		})
	}
}

// 上游会不请自来地给思考块（中转常给 -max 模型强开 thinking），这里曾经直接
// addError 把整条 200 响应作废，且响应头已写出、无法转移候选。
func TestAnthropicThinkingStreamMapsToChatReasoningContent(t *testing.T) {
	transformer := NewStreamTransformer("anthropic", "openai-chat")
	out := transformAll(t, transformer,
		`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先拆解问题。"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIk"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案是 42"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	)
	if err := StreamError(transformer); err != nil {
		t.Fatalf("stream error = %v, want nil", err)
	}
	var reasoning, content string
	for _, chunk := range captureChatChunks(t, out) {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if value, ok := delta["reasoning_content"].(string); ok {
			reasoning += value
		}
		if value, ok := delta["content"].(string); ok {
			content += value
		}
	}
	if reasoning != "先拆解问题。" {
		t.Fatalf("reasoning_content = %q, want 先拆解问题。", reasoning)
	}
	// signature 是 Anthropic 的验签串，混进 reasoning_content 就是一串乱码。
	if strings.Contains(reasoning, "EqQBCgIYAhIk") {
		t.Fatalf("reasoning_content 混入了 signature: %q", reasoning)
	}
	if content != "答案是 42" {
		t.Fatalf("content = %q, want 答案是 42", content)
	}
}

func TestAnthropicThinkingStreamMapsToResponsesReasoningItem(t *testing.T) {
	transformer := NewStreamTransformer("anthropic", "openai-responses")
	out := transformAll(t, transformer,
		`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先拆解问题。"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIk"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案是 42"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	)
	if err := StreamError(transformer); err != nil {
		t.Fatalf("stream error = %v, want nil", err)
	}
	events := captureEvents(t, out)
	summaryDone := findEvent(t, events, "response.reasoning_summary_text.done")
	if got := summaryDone["text"]; got != "先拆解问题。" {
		t.Fatalf("reasoning summary text = %v, want 先拆解问题。", got)
	}
	// 终态 output 必须含 reasoning item：流里已经发过它的 output_item.added，
	// 终态漏掉会让客户端按 output_index 对不上账。
	completed := findEvent(t, events, "response.completed")
	response, _ := completed["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output 长度 = %d, want 2: %#v", len(output), output)
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Fatalf("output[0].type = %v, want reasoning", first["type"])
	}
	summary, _ := first["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary 长度 = %d, want 1: %#v", len(summary), summary)
	}
	entry, _ := summary[0].(map[string]any)
	if entry["type"] != "summary_text" || entry["text"] != "先拆解问题。" {
		t.Fatalf("summary[0] = %#v", entry)
	}
	second, _ := output[1].(map[string]any)
	if second["type"] != "message" {
		t.Fatalf("output[1].type = %v, want message", second["type"])
	}
}

// redacted_thinking 的 data 是加密 blob，没有明文可呈现：不能报错，也不能建出
// 空 reasoning item 或把 blob 当正文推给客户端。
func TestAnthropicRedactedThinkingStreamIsDropped(t *testing.T) {
	for _, target := range []string{"openai-chat", "openai-responses"} {
		t.Run(target, func(t *testing.T) {
			transformer := NewStreamTransformer("anthropic", target)
			out := transformAll(t, transformer,
				`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"EroCCkYIAxgCKkBmm"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案是 42"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`data: {"type":"message_stop"}`,
			)
			if err := StreamError(transformer); err != nil {
				t.Fatalf("stream error = %v, want nil", err)
			}
			joined := strings.Join(out, "")
			if strings.Contains(joined, "EroCCkYIAxgCKkBmm") {
				t.Fatalf("加密 blob 被推给了客户端: %s", joined)
			}
			if strings.Contains(joined, "reasoning") {
				t.Fatalf("redacted_thinking 建出了 reasoning 产物: %s", joined)
			}
			if !strings.Contains(joined, "答案是 42") {
				t.Fatalf("正文丢失: %s", joined)
			}
		})
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

// TestTruncatedToolArgumentsDoNotFailStream 是这组行为的核心回归：
// 上游把工具参数砍在 JSON 中间（撞 max_tokens 的典型表现）时，四条跨格式路径都
// 不得作废整条响应，而要把已收到的半截参数原样交给客户端并标出「被截断」。
//
// 起因是线上一条 200 响应：Codex 走 /v1/responses → anthropic 上游，已经流了
// 467 KB / 63 秒，最后因 `arguments: invalid JSON: unexpected end of JSON input`
// 被整条判失败。而真实 Anthropic 在这种情况下只给 stop_reason: max_tokens。
func TestTruncatedToolArgumentsDoNotFailStream(t *testing.T) {
	t.Run("anthropic-to-responses", func(t *testing.T) {
		transformer := NewStreamTransformer("anthropic", "openai-responses")
		out := transformAll(t, transformer,
			`data: {"type":"message_start","message":{"model":"glm-5.2"}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1f23","name":"Write"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/x\",\"content\":\"aaa"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
			`data: {"type":"message_stop"}`,
		)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("截断参数不应产生转换错误: %v", err)
		}
		if !StreamCompleted(transformer) {
			t.Fatal("流应正常完成，而不是失败终态")
		}
		joined := strings.Join(out, "")
		if !strings.Contains(joined, "response.incomplete") {
			t.Fatalf("应给 response.incomplete 终态:\n%s", joined)
		}
		if !strings.Contains(joined, "max_output_tokens") {
			t.Fatalf("incomplete_details.reason 应为 max_output_tokens:\n%s", joined)
		}
		// 半截参数必须原样透传：arguments 在 Responses 里是字符串字段，截断值协议合法
		if !strings.Contains(joined, `{\"path\":\"/x\",\"content\":\"aaa`) {
			t.Fatalf("截断参数应原样透传:\n%s", joined)
		}
		if strings.Contains(joined, "response.failed") {
			t.Fatalf("不应出现失败终态:\n%s", joined)
		}
	})

	t.Run("anthropic-to-chat", func(t *testing.T) {
		transformer := NewStreamTransformer("anthropic", "openai-chat")
		out := transformAll(t, transformer,
			`data: {"type":"message_start","message":{"model":"glm-5.2"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1f23","name":"Write"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			// 上游自报 tool_use，但参数其实是半截的——终态必须改报 length
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("截断参数不应产生转换错误: %v", err)
		}
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"finish_reason":"length"`) {
			t.Fatalf("finish_reason 应改报 length:\n%s", joined)
		}
		if strings.Contains(joined, `"finish_reason":"tool_calls"`) {
			t.Fatalf("参数被截断时不应报 tool_calls:\n%s", joined)
		}
	})

	t.Run("openai-to-anthropic", func(t *testing.T) {
		transformer := NewStreamTransformer("openai", "anthropic")
		out := transformAll(t, transformer,
			`data: {"model":"gpt-upstream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Write","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			`data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("截断参数不应产生转换错误: %v", err)
		}
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"stop_reason":"max_tokens"`) {
			t.Fatalf("stop_reason 应改报 max_tokens:\n%s", joined)
		}
		if strings.Contains(joined, `"stop_reason":"tool_use"`) {
			t.Fatalf("参数被截断时不应报 tool_use:\n%s", joined)
		}
		if strings.Contains(joined, "event: error") {
			t.Fatalf("不应出现错误终态:\n%s", joined)
		}
	})

	t.Run("responses-to-anthropic", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "anthropic")
		out := transformAll(t, transformer,
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-up"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"Write"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("截断参数不应产生转换错误: %v", err)
		}
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"stop_reason":"max_tokens"`) {
			t.Fatalf("stop_reason 应改报 max_tokens:\n%s", joined)
		}
		if strings.Contains(joined, "event: error") {
			t.Fatalf("不应出现错误终态:\n%s", joined)
		}
	})

	t.Run("responses-to-chat", func(t *testing.T) {
		transformer := NewStreamTransformer("openai-responses", "openai-chat")
		out := transformAll(t, transformer,
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-up"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"Write"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		)
		if err := StreamError(transformer); err != nil {
			t.Fatalf("截断参数不应产生转换错误: %v", err)
		}
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"finish_reason":"length"`) {
			t.Fatalf("finish_reason 应改报 length:\n%s", joined)
		}
	})
}

// TestWrongTypedToolArgumentsStillFailStream 划清容忍边界：
// `[]` / `"s"` / `42` 是**完整且合法**的 JSON，只是不是对象——那是上游协议违规，
// 不是被截断，必须保持硬失败。把它标成 incomplete 等于替上游把「我给错了」
// 说成「我没说完」，客户端会拿一个类型错误的参数去执行工具。
func TestWrongTypedToolArgumentsStillFailStream(t *testing.T) {
	for _, args := range []string{"[]", `"str"`, "42", "null", "true"} {
		t.Run("responses/"+args, func(t *testing.T) {
			transformer := NewStreamTransformer("anthropic", "openai-responses")
			transformAll(t, transformer,
				`data: {"type":"message_start","message":{"model":"glm-5.2"}}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1f23","name":"Write"}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonQuote(args)+`}}`,
				`data: {"type":"content_block_stop","index":1}`,
			)
			err := StreamError(transformer)
			if err == nil {
				t.Fatalf("参数 %s 是完整但非对象的 JSON，应硬失败", args)
			}
			if !strings.Contains(err.Error(), "must be a JSON object") {
				t.Fatalf("错误应说明必须是 JSON 对象，实际: %v", err)
			}
		})
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
