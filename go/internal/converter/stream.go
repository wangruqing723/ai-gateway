package converter

import (
	"encoding/json"
	"strings"
)

// StreamTransformer 逐行转换上游 SSE 为目标格式（对齐 Node 的闭包 transform 函数）。
// Transform 输入一行（已 trimEnd），返回 0~N 行目标格式输出。
type StreamTransformer interface {
	Transform(line string) []string
}

// NewStreamTransformer 按 provider/client 格式创建流式转换器。
// 相同格式（含 openai→openai-chat）直接透传。
func NewStreamTransformer(providerFormat, clientFormat string) StreamTransformer {
	if providerFormat == clientFormat || (providerFormat == "openai" && clientFormat == "openai-chat") {
		return passthrough{}
	}
	st := &streamState{msgID: "chatcmpl-" + shortID(), respID: "resp_" + shortID(), itemID: "msg_" + shortID()}
	switch {
	case providerFormat == "anthropic" && clientFormat == "openai-chat":
		return &anthropicToChat{s: st}
	case providerFormat == "anthropic" && clientFormat == "openai-responses":
		return &anthropicToResponses{s: st}
	case providerFormat == "openai" && clientFormat == "anthropic":
		return &openAIToAnthropic{s: st}
	case providerFormat == "openai" && clientFormat == "openai-responses":
		return &openAIToResponses{s: st}
	}
	return passthrough{}
}

type streamState struct {
	msgID   string
	respID  string
	itemID  string
	model   string
	started bool
}

type passthrough struct{}

func (passthrough) Transform(line string) []string { return []string{line} }

// ── SSE 解析/构造辅助 ─────────────────────────

type sseParsed struct {
	done bool
	data map[string]any
	ok   bool // 是否成功解析出可处理内容
}

// parseSseLine 对齐 Node：仅处理 "data: " 行，[DONE] 标记结束。
func parseSseLine(line string) sseParsed {
	if !strings.HasPrefix(line, "data: ") {
		return sseParsed{}
	}
	raw := line[len("data: "):]
	if raw == "[DONE]" {
		return sseParsed{done: true, ok: true}
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return sseParsed{}
	}
	return sseParsed{data: data, ok: true}
}

// sseEvent 构造 "event: X\ndata: {json}\n\n"。
func sseEvent(event string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

// dataLine 构造 "data: {json}\n\n"。
func dataLine(data any) string {
	b, _ := json.Marshal(data)
	return "data: " + string(b) + "\n\n"
}

func chatDelta(id, model string, delta map[string]any, finishReason any) string {
	return dataLine(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": nowUnix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	})
}

func deltaType(data map[string]any) string {
	d, _ := data["delta"].(map[string]any)
	return getString(d, "type")
}

func deltaText(data map[string]any) string {
	d, _ := data["delta"].(map[string]any)
	return getString(d, "text")
}

// firstChoiceDelta 取 choices[0].delta 与 finish_reason
func firstChoiceDelta(data map[string]any) (delta map[string]any, finish any, hasContent bool) {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}, nil, false
	}
	c, _ := choices[0].(map[string]any)
	delta, _ = c["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}
	_, hasContent = delta["content"]
	return delta, c["finish_reason"], hasContent
}

// ── Anthropic → OpenAI Chat ──────────────────

type anthropicToChat struct{ s *streamState }

func (t *anthropicToChat) Transform(line string) []string {
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	if p.done {
		return []string{"data: [DONE]\n\n"}
	}
	data := p.data
	if data == nil {
		return nil
	}
	switch data["type"] {
	case "message_start":
		if msg, ok := data["message"].(map[string]any); ok {
			t.s.model = getString(msg, "model")
		}
		return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"role": "assistant"}, nil)}
	case "content_block_delta":
		if deltaType(data) == "text_delta" {
			return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"content": deltaText(data)}, nil)}
		}
	case "message_delta":
		if d, ok := data["delta"].(map[string]any); ok {
			if stop := getString(d, "stop_reason"); stop != "" {
				fr := stop
				if stop == "end_turn" {
					fr = "stop"
				}
				return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{}, fr), "data: [DONE]\n\n"}
			}
		}
	}
	return nil
}

// ── Anthropic → OpenAI Responses ─────────────

type anthropicToResponses struct{ s *streamState }

func (t *anthropicToResponses) Transform(line string) []string {
	p := parseSseLine(line)
	if !p.ok || p.done {
		return nil
	}
	data := p.data
	if data == nil {
		return nil
	}
	id := t.s.itemID
	switch data["type"] {
	case "message_start":
		if msg, ok := data["message"].(map[string]any); ok {
			t.s.model = getString(msg, "model")
		}
		return []string{
			sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": t.s.respID, "object": "response", "status": "in_progress", "model": t.s.model, "output": []any{}}}),
			sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{}}}),
			sseEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}}),
		}
	case "content_block_delta":
		if deltaType(data) == "text_delta" {
			return []string{sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": deltaText(data)})}
		}
	case "message_stop":
		return []string{
			sseEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": id, "output_index": 0, "content_index": 0, "text": ""}),
			sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": ""}}}}),
			sseEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": t.s.respID, "object": "response", "status": "completed", "model": t.s.model}}),
		}
	}
	return nil
}

// ── OpenAI → Anthropic ───────────────────────

type openAIToAnthropic struct {
	s            *streamState
	blockStarted bool
}

func (t *openAIToAnthropic) Transform(line string) []string {
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	if p.done {
		return []string{"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
	}
	data := p.data
	if data == nil {
		return nil
	}
	delta, finish, hasContent := firstChoiceDelta(data)
	var out []string

	if !t.s.started {
		t.s.model = getString(data, "model")
		out = append(out, sseEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + shortID(), "type": "message", "role": "assistant", "model": t.s.model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}))
		t.s.started = true
	}
	if hasContent && !t.blockStarted {
		out = append(out, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		t.blockStarted = true
	}
	if c := getString(delta, "content"); c != "" {
		out = append(out, sseEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": c}}))
	}
	if finish != nil && finish != "" {
		out = append(out, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		stop := finish
		if finish == "stop" {
			stop = "end_turn"
		}
		out = append(out, sseEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}}))
	}
	return out
}

// ── OpenAI → OpenAI Responses ────────────────

type openAIToResponses struct{ s *streamState }

func (t *openAIToResponses) Transform(line string) []string {
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	id := t.s.itemID
	if p.done {
		return []string{
			sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{}}}),
			sseEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": t.s.respID, "status": "completed", "model": t.s.model}}),
		}
	}
	data := p.data
	if data == nil {
		return nil
	}
	delta, finish, _ := firstChoiceDelta(data)
	var out []string

	if !t.s.started {
		t.s.model = getString(data, "model")
		out = append(out, sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": t.s.respID, "object": "response", "status": "in_progress", "model": t.s.model, "output": []any{}}}))
		out = append(out, sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{}}}))
		out = append(out, sseEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}}))
		t.s.started = true
	}
	if c := getString(delta, "content"); c != "" {
		out = append(out, sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": c}))
	}
	if finish != nil && finish != "" {
		out = append(out, sseEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": id, "output_index": 0, "content_index": 0, "text": ""}))
	}
	return out
}
