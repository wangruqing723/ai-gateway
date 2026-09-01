package converter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// StreamTransformer 逐行转换上游 SSE 为目标格式（对齐 Node 的闭包 transform 函数）。
// Transform 输入一行（已 trimEnd），返回 0~N 行目标格式输出。
type StreamTransformer interface {
	Transform(line string) []string
}

const maxStreamAggregateBytes = 8 << 20

// NewStreamTransformer 按 provider/client 格式创建流式转换器。
// 相同格式（含 openai→openai-chat）直接透传。
func NewStreamTransformer(providerFormat, clientFormat string) StreamTransformer {
	if IsPassthrough(providerFormat, clientFormat) {
		return passthrough{}
	}
	st := &streamState{
		msgID:      "chatcmpl-" + shortID(),
		respID:     "resp_" + shortID(),
		itemID:     "msg_" + shortID(),
		texts:      make(map[int]*streamText),
		tools:      make(map[int]*streamTool),
		reasonings: make(map[int]*streamReasoning),
	}
	switch {
	case providerFormat == "anthropic" && clientFormat == "openai-chat":
		return &anthropicToChat{s: st, toolIndexes: make(map[int]int), reasoningBlocks: make(map[int]string)}
	case providerFormat == "anthropic" && clientFormat == "openai-responses":
		return &anthropicToResponses{s: st, reasoningBlocks: make(map[int]string)}
	case providerFormat == "openai" && clientFormat == "anthropic":
		return &openAIToAnthropic{s: st, tools: make(map[int]*streamTool)}
	case providerFormat == "openai" && clientFormat == "openai-responses":
		return &openAIToResponses{s: st}
	case providerFormat == "openai-responses" && clientFormat == "openai-chat":
		return &responsesToChat{s: st, tools: make(map[int]*streamTool), toolIndexes: make(map[int]int)}
	case providerFormat == "openai-responses" && clientFormat == "anthropic":
		return &responsesToAnthropic{s: st, tools: make(map[int]*streamTool), blocks: make(map[int]int), textStarted: make(map[int]bool), textStopped: make(map[int]bool)}
	}
	return passthrough{}
}

type streamState struct {
	msgID       string
	respID      string
	itemID      string
	model       string
	started     bool
	completed   bool
	failureSent bool
	failureType string
	sequence    int
	nextOutput  int
	texts       map[int]*streamText
	textOrder   []int
	tools       map[int]*streamTool
	toolOrder   []int
	// reasonings 是 Responses 的 reasoning output item，与 texts / tools 共用
	// nextOutput 分配 output_index，三者一起决定最终 response.output 的顺序。
	reasonings     map[int]*streamReasoning
	reasoningOrder []int
	aggregate      int
	incomplete     bool
	err            error
}

type streamText struct {
	itemID      string
	outputIndex int
	text        strings.Builder
	done        bool
}

// streamReasoning 是 Anthropic thinking 块在 Responses 侧对应的 reasoning item。
type streamReasoning struct {
	itemID      string
	outputIndex int
	text        strings.Builder
	done        bool
}

type streamTool struct {
	sourceIndex int
	outputIndex int
	blockIndex  int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	done        bool
}

type passthrough struct{}

func (passthrough) Transform(line string) []string { return []string{line} }

// StreamError 返回流转换过程中发现的协议错误；旧 Transform 签名保持不变。
func StreamError(transformer StreamTransformer) error {
	reporter, ok := transformer.(interface{ Err() error })
	if !ok {
		return nil
	}
	return reporter.Err()
}

// StreamFailure 使用 transformer 自身状态生成目标协议的唯一失败终态。
func StreamFailure(transformer StreamTransformer) []string {
	reporter, ok := transformer.(interface{ Failure() []string })
	if !ok {
		return nil
	}
	return reporter.Failure()
}

// AbortStream 将代理层发现的 EOF、超时或读取错误写入 transformer 状态，并生成目标协议失败终态。
func AbortStream(transformer StreamTransformer, errType, message string) []string {
	reporter, ok := transformer.(interface{ Abort(string, string) []string })
	if !ok {
		return nil
	}
	return reporter.Abort(errType, message)
}

// StreamCompleted 表示 transformer 已见到源协议的成功终态。
func StreamCompleted(transformer StreamTransformer) bool {
	reporter, ok := transformer.(interface{ Completed() bool })
	return ok && reporter.Completed()
}

func (state *streamState) addError(err error) {
	state.err = errors.Join(state.err, err)
}

func (state *streamState) abort(errType, message string) {
	if state.completed || state.failureSent || state.err != nil {
		return
	}
	state.failureType = errType
	state.addError(errors.New(message))
}

func (state *streamState) appendAggregate(builder *strings.Builder, value string) bool {
	if value == "" {
		return true
	}
	if len(value) > maxStreamAggregateBytes-state.aggregate {
		state.addError(fmt.Errorf("流式响应累计内容超过大小限制 (%d bytes)", maxStreamAggregateBytes))
		return false
	}
	state.aggregate += len(value)
	builder.WriteString(value)
	return true
}

func responsesStreamFailure(state *streamState) []string {
	if state.err == nil || state.failureSent || state.completed {
		return nil
	}
	state.failureSent = true
	errType := state.failureType
	if errType == "" {
		errType = "conversion_error"
	}
	return []string{responseEvent(state, "response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": state.respID, "object": "response", "status": "failed", "model": state.model,
			"output": []any{},
			"error":  map[string]any{"type": errType, "code": errType, "message": state.err.Error()},
		},
	})}
}

func anthropicStreamFailure(state *streamState) []string {
	if state.err == nil || state.failureSent || state.completed {
		return nil
	}
	state.failureSent = true
	errType := state.failureType
	if errType == "" {
		errType = "api_error"
	}
	return []string{sseEvent("error", map[string]any{
		"type": "error", "error": map[string]any{"type": errType, "message": state.err.Error()},
	})}
}

func chatStreamFailure(state *streamState) []string {
	if state.err == nil || state.failureSent || state.completed {
		return nil
	}
	state.failureSent = true
	errType := state.failureType
	code := errType
	if errType == "" {
		errType = "conversion_error"
		code = "converter_error"
	}
	return []string{
		dataLine(map[string]any{"error": map[string]any{"type": errType, "code": code, "message": state.err.Error()}}),
		"data: [DONE]\n\n",
	}
}

// ── SSE 解析/构造辅助 ─────────────────────────

type sseParsed struct {
	done bool
	data map[string]any
	ok   bool
}

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

func sseEvent(event string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

func responseEvent(state *streamState, event string, data map[string]any) string {
	data["sequence_number"] = state.sequence
	state.sequence++
	return sseEvent(event, data)
}

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

func valueIndex(value any) int {
	switch index := value.(type) {
	case float64:
		return int(index)
	case int:
		return index
	default:
		return 0
	}
}

func openAIToolCallDeltas(delta map[string]any) []any {
	calls, _ := delta["tool_calls"].([]any)
	return calls
}

// ── Anthropic → OpenAI Chat ──────────────────

type anthropicToChat struct {
	s           *streamState
	toolIndexes map[int]int
	// reasoningBlocks 记下思考块的下标及其原始类型（thinking / redacted_thinking），
	// 用于让后续 delta 与 content_block_stop 走对分支。不能只存布尔：两种块的增量
	// 处理不同，thinking 转 reasoning_content，redacted 的加密 blob 丢弃。
	// 也不能只看 s.tools[index] == nil：那个条件对思考块和未知块一样成立，
	// 区分不出「已知的思考块」与「压根没见过」。
	reasoningBlocks map[int]string
	nextTool        int
	doneSent        bool
}

func (t *anthropicToChat) Err() error        { return t.s.err }
func (t *anthropicToChat) Failure() []string { return chatStreamFailure(t.s) }
func (t *anthropicToChat) Completed() bool   { return t.s.completed }
func (t *anthropicToChat) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

func (t *anthropicToChat) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	if p.done {
		if t.doneSent {
			return nil
		}
		t.doneSent = true
		return []string{"data: [DONE]\n\n"}
	}
	data := p.data
	if data == nil {
		return nil
	}
	switch getString(data, "type") {
	case "message_start":
		if msg, ok := data["message"].(map[string]any); ok {
			t.s.model = getString(msg, "model")
		}
		return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"role": "assistant"}, nil)}
	case "content_block_start":
		block, _ := data["content_block"].(map[string]any)
		blockType := getString(block, "type")
		if blockType == "text" {
			return nil
		}
		// 思考块映射到 reasoning_content（DeepSeek 引入、OpenRouter/vLLM 跟进的事实
		// 标准，internal/vision 读取的也是这个字段）。上游是否思考由它自己决定——中转
		// 常给 -max 模型强开，网关从不在请求里写 thinking 也照样会收到——报错等于把
		// 一次本来成功的 200 响应作废，且响应头已写出、无法再转移候选。
		//
		// 记下块类型而不只是布尔：redacted_thinking 的 data 是加密 blob，没有明文可
		// 呈现，它的增量必须丢弃，而 thinking 的增量要转成 reasoning_content。
		if isReasoningBlockType(blockType) {
			sourceIndex := valueIndex(data["index"])
			t.reasoningBlocks[sourceIndex] = blockType
			if blockType != "thinking" {
				return nil
			}
			// content_block_start 可以自带首段思考文本，非流式回放时整段都在这里。
			if initial := getString(block, "thinking"); initial != "" {
				return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"reasoning_content": initial}, nil)}
			}
			return nil
		}
		if blockType != "tool_use" {
			t.s.addError(fmt.Errorf("unsupported anthropic stream content block %q", blockType))
			return nil
		}
		sourceIndex := valueIndex(data["index"])
		toolIndex := t.nextTool
		t.nextTool++
		t.toolIndexes[sourceIndex] = toolIndex
		arguments := ""
		if inputValue, exists := block["input"]; exists {
			input, ok := inputValue.(map[string]any)
			if !ok {
				t.s.addError(fmt.Errorf("anthropic stream tool_use %q input must be a JSON object", getString(block, "id")))
				return nil
			}
			if len(input) > 0 {
				arguments = marshalFunctionArguments(input)
			}
		}
		tool := &streamTool{sourceIndex: sourceIndex, callID: getString(block, "id"), name: getString(block, "name")}
		if !t.s.appendAggregate(&tool.arguments, arguments) {
			return nil
		}
		t.s.tools[sourceIndex] = tool
		t.s.toolOrder = append(t.s.toolOrder, sourceIndex)
		return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"tool_calls": []any{map[string]any{
			"index": toolIndex,
			"id":    getString(block, "id"),
			"type":  "function",
			"function": map[string]any{
				"name":      getString(block, "name"),
				"arguments": arguments,
			},
		}}}, nil)}
	case "content_block_delta":
		// 思考块的增量跟着整块一起丢；不能落到下面的 default 分支报错。
		if isReasoningDeltaType(deltaType(data)) {
			return t.reasoningDelta(data)
		}
		return t.contentDelta(data)
	case "content_block_stop":
		sourceIndex := valueIndex(data["index"])
		if t.reasoningBlocks[sourceIndex] != "" {
			// 思考块没有需要收尾的聚合状态，reasoning_content 是逐段直接推出去的。
			return nil
		}
		tool := t.s.tools[sourceIndex]
		if tool == nil {
			return nil
		}
		arguments := tool.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		if _, err := parseFunctionArguments(arguments); err != nil {
			t.s.addError(fmt.Errorf("stream function call %q arguments: %w", tool.callID, err))
		}
	case "message_delta":
		if delta, ok := data["delta"].(map[string]any); ok {
			if stop := getString(delta, "stop_reason"); stop != "" {
				finish := anthropicStopReasonToChat(stop)
				t.doneSent = true
				t.s.completed = true
				return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{}, finish), "data: [DONE]\n\n"}
			}
		}
	}
	return nil
}

// reasoningDelta 把 thinking_delta 转成 reasoning_content 增量。
//
// signature_delta 一律丢弃：那是 Anthropic 对思考内容的加密签名，只有它自己能验签，
// 拼进 reasoning_content 会变成一串乱码。redacted_thinking 的增量同理。
func (t *anthropicToChat) reasoningDelta(data map[string]any) []string {
	if deltaType(data) != "thinking_delta" {
		return nil
	}
	// 块类型未记录时按 thinking 处理：漏了 content_block_start 也不该静默吞掉正文。
	if blockType := t.reasoningBlocks[valueIndex(data["index"])]; blockType != "" && blockType != "thinking" {
		return nil
	}
	delta, _ := data["delta"].(map[string]any)
	text := getString(delta, "thinking")
	if text == "" {
		return nil
	}
	return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"reasoning_content": text}, nil)}
}

func (t *anthropicToChat) contentDelta(data map[string]any) []string {
	switch deltaType(data) {
	case "text_delta":
		return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"content": deltaText(data)}, nil)}
	case "input_json_delta":
		delta, _ := data["delta"].(map[string]any)
		sourceIndex := valueIndex(data["index"])
		partial := getString(delta, "partial_json")
		if tool := t.s.tools[sourceIndex]; tool != nil {
			if !t.s.appendAggregate(&tool.arguments, partial) {
				return nil
			}
		}
		toolIndex := t.toolIndexes[sourceIndex]
		return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"tool_calls": []any{map[string]any{
			"index":    toolIndex,
			"function": map[string]any{"arguments": partial},
		}}}, nil)}
	default:
		t.s.addError(fmt.Errorf("unsupported anthropic stream delta %q", deltaType(data)))
	}
	return nil
}

// ── Responses SSE 共用状态 ────────────────────

func ensureResponseStarted(state *streamState) []string {
	if state.started {
		return nil
	}
	state.started = true
	return []string{responseEvent(state, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": state.respID, "object": "response", "status": "in_progress", "model": state.model, "output": []any{},
		},
	})}
}

func ensureResponseText(state *streamState, sourceIndex int) (*streamText, []string) {
	if text := state.texts[sourceIndex]; text != nil {
		return text, nil
	}
	itemID := "msg_" + shortID()
	if len(state.textOrder) == 0 {
		itemID = state.itemID
	}
	text := &streamText{itemID: itemID, outputIndex: state.nextOutput}
	state.texts[sourceIndex] = text
	state.textOrder = append(state.textOrder, sourceIndex)
	state.nextOutput++
	return text, []string{
		responseEvent(state, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": text.outputIndex,
			"item": map[string]any{"type": "message", "id": text.itemID, "role": "assistant", "status": "in_progress", "content": []any{}},
		}),
		responseEvent(state, "response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": text.itemID,
			"output_index": text.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": ""},
		}),
	}
}

func appendResponseText(state *streamState, sourceIndex int, value string) []string {
	text, out := ensureResponseText(state, sourceIndex)
	if text.done {
		state.addError(fmt.Errorf("responses text item %s received delta after done", text.itemID))
		return nil
	}
	if !state.appendAggregate(&text.text, value) {
		return nil
	}
	if value != "" {
		out = append(out, responseEvent(state, "response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": text.itemID,
			"output_index": text.outputIndex, "content_index": 0, "delta": value,
		}))
	}
	return out
}

// ensureResponseReasoning 按需建出 reasoning item 并发 output_item.added。
//
// 结构与 ensureResponseText 平行，但没有 content_part：reasoning item 的文本挂在
// summary 数组里，Responses 协议不给它发 content_part.added。
func ensureResponseReasoning(state *streamState, sourceIndex int) (*streamReasoning, []string) {
	if reasoning := state.reasonings[sourceIndex]; reasoning != nil {
		return reasoning, nil
	}
	reasoning := &streamReasoning{itemID: "rs_" + shortID(), outputIndex: state.nextOutput}
	state.reasonings[sourceIndex] = reasoning
	state.reasoningOrder = append(state.reasoningOrder, sourceIndex)
	state.nextOutput++
	return reasoning, []string{responseEvent(state, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": reasoning.outputIndex,
		"item": streamReasoningItem(reasoning, "in_progress"),
	})}
}

func appendResponseReasoning(state *streamState, sourceIndex int, value string) []string {
	reasoning, out := ensureResponseReasoning(state, sourceIndex)
	if reasoning.done {
		state.addError(fmt.Errorf("responses reasoning item %s received delta after done", reasoning.itemID))
		return nil
	}
	if !state.appendAggregate(&reasoning.text, value) {
		return nil
	}
	if value != "" {
		out = append(out, responseEvent(state, "response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": reasoning.itemID,
			"output_index": reasoning.outputIndex, "summary_index": 0, "delta": value,
		}))
	}
	return out
}

func finishResponseReasoning(state *streamState, sourceIndex int) []string {
	reasoning := state.reasonings[sourceIndex]
	if reasoning == nil || reasoning.done {
		return nil
	}
	reasoning.done = true
	fullText := reasoning.text.String()
	return []string{
		responseEvent(state, "response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": reasoning.itemID,
			"output_index": reasoning.outputIndex, "summary_index": 0, "text": fullText,
		}),
		responseEvent(state, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": reasoning.outputIndex,
			"item": streamReasoningItem(reasoning, "completed"),
		}),
	}
}

// streamReasoningItem 组装 reasoning item。summary 为空时给空数组而不是省略字段：
// 客户端按 item.summary 遍历，缺字段会拿到 undefined。
func streamReasoningItem(reasoning *streamReasoning, status string) map[string]any {
	summary := []any{}
	if text := reasoning.text.String(); text != "" {
		summary = append(summary, map[string]any{"type": "summary_text", "text": text})
	}
	return map[string]any{
		"type": "reasoning", "id": reasoning.itemID, "summary": summary, "status": status,
	}
}

func ensureResponseTool(state *streamState, sourceIndex int, callID, name string) (*streamTool, []string) {
	if tool := state.tools[sourceIndex]; tool != nil {
		if callID != "" {
			tool.callID = callID
		}
		if name != "" {
			tool.name = name
		}
		return tool, nil
	}
	if callID == "" {
		callID = "call_" + shortID()
	}
	tool := &streamTool{
		sourceIndex: sourceIndex,
		outputIndex: state.nextOutput,
		itemID:      "fc_" + shortID(),
		callID:      callID,
		name:        name,
	}
	state.nextOutput++
	state.tools[sourceIndex] = tool
	state.toolOrder = append(state.toolOrder, sourceIndex)
	return tool, []string{responseEvent(state, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": tool.outputIndex,
		"item": streamFunctionCallItem(tool, "in_progress"),
	})}
}

func appendResponseToolArguments(state *streamState, tool *streamTool, partial string) []string {
	if tool.done {
		state.addError(fmt.Errorf("responses function item %s received delta after done", tool.itemID))
		return nil
	}
	if !state.appendAggregate(&tool.arguments, partial) {
		return nil
	}
	if partial == "" {
		return nil
	}
	return []string{responseEvent(state, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": tool.itemID,
		"output_index": tool.outputIndex, "delta": partial,
	})}
}

func finishResponseText(state *streamState, sourceIndex int) []string {
	text := state.texts[sourceIndex]
	if text == nil || text.done {
		return nil
	}
	text.done = true
	fullText := text.text.String()
	part := map[string]any{"type": "output_text", "text": fullText}
	item := map[string]any{
		"type": "message", "id": text.itemID, "role": "assistant", "status": "completed", "content": []any{part},
	}
	return []string{
		responseEvent(state, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": text.itemID,
			"output_index": text.outputIndex, "content_index": 0, "text": fullText,
		}),
		responseEvent(state, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": text.itemID,
			"output_index": text.outputIndex, "content_index": 0, "part": part,
		}),
		responseEvent(state, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": text.outputIndex, "item": item,
		}),
	}
}

func finishResponseTool(state *streamState, tool *streamTool) []string {
	if tool == nil || tool.done {
		return nil
	}
	arguments, err := normalizeStreamToolArguments(state, tool)
	if err != nil {
		state.addError(fmt.Errorf("stream function call %q arguments: %w", tool.callID, err))
		return nil
	}
	tool.done = true
	return []string{
		responseEvent(state, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": tool.itemID,
			"output_index": tool.outputIndex, "name": tool.name, "arguments": arguments,
		}),
		responseEvent(state, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": tool.outputIndex,
			"item": streamFunctionCallItem(tool, "completed"),
		}),
	}
}

func normalizeStreamToolArguments(state *streamState, tool *streamTool) (string, error) {
	arguments := tool.arguments.String()
	if arguments == "" {
		arguments = "{}"
		if !state.appendAggregate(&tool.arguments, arguments) {
			return "", state.err
		}
	}
	if _, err := parseFunctionArguments(arguments); err != nil {
		return "", err
	}
	return arguments, nil
}

func validateAllResponseToolArguments(state *streamState) error {
	for _, sourceIndex := range state.toolOrder {
		tool := state.tools[sourceIndex]
		if tool == nil || tool.done {
			continue
		}
		if _, err := normalizeStreamToolArguments(state, tool); err != nil {
			return fmt.Errorf("stream function call %q arguments: %w", tool.callID, err)
		}
	}
	return nil
}

func finishAllResponseItems(state *streamState) []string {
	var out []string
	for _, sourceIndex := range state.reasoningOrder {
		out = append(out, finishResponseReasoning(state, sourceIndex)...)
	}
	for _, sourceIndex := range state.textOrder {
		out = append(out, finishResponseText(state, sourceIndex)...)
	}
	for _, sourceIndex := range state.toolOrder {
		out = append(out, finishResponseTool(state, state.tools[sourceIndex])...)
	}
	return out
}

func streamFunctionCallItem(tool *streamTool, status string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": tool.itemID, "call_id": tool.callID,
		"name": tool.name, "arguments": tool.arguments.String(), "status": status,
	}
}

func completedResponseOutput(state *streamState) []any {
	ordered := make([]any, state.nextOutput)
	for _, sourceIndex := range state.reasoningOrder {
		reasoning := state.reasonings[sourceIndex]
		ordered[reasoning.outputIndex] = streamReasoningItem(reasoning, "completed")
	}
	for _, sourceIndex := range state.textOrder {
		text := state.texts[sourceIndex]
		ordered[text.outputIndex] = map[string]any{
			"type": "message", "id": text.itemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text.text.String()}},
		}
	}
	for _, sourceIndex := range state.toolOrder {
		tool := state.tools[sourceIndex]
		ordered[tool.outputIndex] = streamFunctionCallItem(tool, "completed")
	}
	out := make([]any, 0, len(ordered))
	for _, item := range ordered {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

func completeResponse(state *streamState) []string {
	if state.completed || state.err != nil {
		return nil
	}
	state.completed = true
	eventType := "response.completed"
	status := "completed"
	response := map[string]any{
		"id": state.respID, "object": "response", "status": status,
		"model": state.model, "output": completedResponseOutput(state),
	}
	if state.incomplete {
		eventType = "response.incomplete"
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return []string{responseEvent(state, eventType, map[string]any{
		"type": eventType, "response": response,
	})}
}

// ── Anthropic → OpenAI Responses ─────────────

type anthropicToResponses struct {
	s *streamState
	// reasoningBlocks 见 anthropicToChat.reasoningBlocks。这里漏记的话
	// content_block_stop 会落到 finishResponseText，凭思考块的下标凭空建出一个
	// 空 output_text item 推给客户端。
	reasoningBlocks map[int]string
}

func (t *anthropicToResponses) Err() error        { return t.s.err }
func (t *anthropicToResponses) Failure() []string { return responsesStreamFailure(t.s) }
func (t *anthropicToResponses) Completed() bool   { return t.s.completed }
func (t *anthropicToResponses) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

// reasoningDelta 把 thinking_delta 累进 reasoning item 的 summary。
// signature_delta 与 redacted_thinking 的增量丢弃，理由见 anthropicToChat.reasoningDelta。
func (t *anthropicToResponses) reasoningDelta(data map[string]any) []string {
	if deltaType(data) != "thinking_delta" {
		return nil
	}
	sourceIndex := valueIndex(data["index"])
	if blockType := t.reasoningBlocks[sourceIndex]; blockType != "" && blockType != "thinking" {
		return nil
	}
	delta, _ := data["delta"].(map[string]any)
	text := getString(delta, "thinking")
	if text == "" {
		return nil
	}
	out := ensureResponseStarted(t.s)
	return append(out, appendResponseReasoning(t.s, sourceIndex, text)...)
}

func (t *anthropicToResponses) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent || t.s.completed {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok || p.done || p.data == nil {
		return nil
	}
	data := p.data
	switch getString(data, "type") {
	case "message_start":
		if msg, ok := data["message"].(map[string]any); ok {
			t.s.model = getString(msg, "model")
		}
		return ensureResponseStarted(t.s)
	case "content_block_start":
		block, _ := data["content_block"].(map[string]any)
		sourceIndex := valueIndex(data["index"])
		blockType := getString(block, "type")
		// 思考块映射成 reasoning output item，文本走 summary_text。理由同
		// anthropicToChat：上游会不请自来地给思考块，报错等于作废一次成功响应。
		// redacted_thinking 只记类型不建 item：加密 blob 没有明文可放进 summary，
		// 建了就是个空 item。
		if isReasoningBlockType(blockType) {
			t.reasoningBlocks[sourceIndex] = blockType
			out := ensureResponseStarted(t.s)
			if blockType != "thinking" {
				return out
			}
			reasoning, added := ensureResponseReasoning(t.s, sourceIndex)
			out = append(out, added...)
			if initial := getString(block, "thinking"); initial != "" {
				if !t.s.appendAggregate(&reasoning.text, initial) {
					return nil
				}
				out = append(out, responseEvent(t.s, "response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "item_id": reasoning.itemID,
					"output_index": reasoning.outputIndex, "summary_index": 0, "delta": initial,
				}))
			}
			return out
		}
		if blockType != "text" && blockType != "tool_use" {
			t.s.addError(fmt.Errorf("unsupported anthropic stream content block %q", blockType))
			return nil
		}
		var toolInput map[string]any
		if blockType == "tool_use" {
			if inputValue, exists := block["input"]; exists {
				var ok bool
				toolInput, ok = inputValue.(map[string]any)
				if !ok {
					t.s.addError(fmt.Errorf("anthropic stream tool_use %q input must be a JSON object", getString(block, "id")))
					return nil
				}
			}
		}
		out := ensureResponseStarted(t.s)
		switch blockType {
		case "text":
			_, added := ensureResponseText(t.s, sourceIndex)
			out = append(out, added...)
		case "tool_use":
			tool, added := ensureResponseTool(t.s, sourceIndex, getString(block, "id"), getString(block, "name"))
			out = append(out, added...)
			if len(toolInput) > 0 {
				out = append(out, appendResponseToolArguments(t.s, tool, marshalFunctionArguments(toolInput))...)
			}
		}
		return out
	case "content_block_delta":
		// 必须在 appendResponseText 之前拦掉：思考正文流进 text item 会被当成回复
		// 正文，混进客户端看到的答案里。
		if isReasoningDeltaType(deltaType(data)) {
			return t.reasoningDelta(data)
		}
		if deltaType(data) != "text_delta" && deltaType(data) != "input_json_delta" {
			t.s.addError(fmt.Errorf("unsupported anthropic stream delta %q", deltaType(data)))
			return nil
		}
		out := ensureResponseStarted(t.s)
		switch deltaType(data) {
		case "text_delta":
			out = append(out, appendResponseText(t.s, valueIndex(data["index"]), deltaText(data))...)
		case "input_json_delta":
			delta, _ := data["delta"].(map[string]any)
			tool, added := ensureResponseTool(t.s, valueIndex(data["index"]), "", "")
			out = append(out, added...)
			out = append(out, appendResponseToolArguments(t.s, tool, getString(delta, "partial_json"))...)
		}
		return out
	case "content_block_stop":
		sourceIndex := valueIndex(data["index"])
		if t.reasoningBlocks[sourceIndex] != "" {
			return finishResponseReasoning(t.s, sourceIndex)
		}
		if tool := t.s.tools[sourceIndex]; tool != nil {
			return finishResponseTool(t.s, tool)
		}
		return finishResponseText(t.s, sourceIndex)
	case "message_delta":
		if delta, ok := data["delta"].(map[string]any); ok && getString(delta, "stop_reason") == "max_tokens" {
			t.s.incomplete = true
		}
		return nil
	case "message_stop":
		if err := validateAllResponseToolArguments(t.s); err != nil {
			t.s.addError(err)
			return nil
		}
		out := finishAllResponseItems(t.s)
		return append(out, completeResponse(t.s)...)
	}
	return nil
}

// ── OpenAI → Anthropic ───────────────────────

type openAIToAnthropic struct {
	s              *streamState
	tools          map[int]*streamTool
	toolOrder      []int
	nextBlock      int
	textStarted    bool
	textStopped    bool
	finishSent     bool
	messageStopped bool
}

func (t *openAIToAnthropic) Err() error        { return t.s.err }
func (t *openAIToAnthropic) Failure() []string { return anthropicStreamFailure(t.s) }
func (t *openAIToAnthropic) Completed() bool   { return t.s.completed }
func (t *openAIToAnthropic) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

func (t *openAIToAnthropic) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	if p.done {
		if t.messageStopped {
			return nil
		}
		out := t.finishBlocks()
		if t.s.err != nil {
			return nil
		}
		out = append(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		t.messageStopped = true
		t.s.completed = true
		return out
	}
	data := p.data
	if data == nil {
		return nil
	}
	delta, finish, hasContent := firstChoiceDelta(data)
	if model := getString(data, "model"); model != "" {
		t.s.model = model
	}
	if err := rejectOpenAIChatRefusal(delta); err != nil {
		t.s.addError(err)
		return nil
	}
	var out []string

	if !t.s.started {
		out = append(out, sseEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_" + shortID(), "type": "message", "role": "assistant", "model": t.s.model,
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
		t.s.started = true
	}
	content, contentIsString := delta["content"].(string)
	if hasContent && contentIsString {
		if !t.textStarted {
			t.textStarted = true
			out = append(out, sseEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": t.nextBlock,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
			t.nextBlock++
		}
		if content != "" {
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": content},
			}))
		}
	}

	for _, value := range openAIToolCallDeltas(delta) {
		call, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if t.textStarted && !t.textStopped {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
			t.textStopped = true
		}
		index := valueIndex(call["index"])
		function, _ := call["function"].(map[string]any)
		tool := t.tools[index]
		if tool == nil {
			tool = &streamTool{
				sourceIndex: index,
				blockIndex:  t.nextBlock,
				callID:      getString(call, "id"),
				name:        getString(function, "name"),
			}
			if tool.callID == "" {
				tool.callID = "call_" + shortID()
			}
			t.nextBlock++
			t.tools[index] = tool
			t.toolOrder = append(t.toolOrder, index)
			out = append(out, sseEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": tool.blockIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": tool.callID, "name": tool.name, "input": map[string]any{},
				},
			}))
		} else {
			if id := getString(call, "id"); id != "" {
				tool.callID = id
			}
			if name := getString(function, "name"); name != "" {
				tool.name = name
			}
		}
		if arguments := getString(function, "arguments"); arguments != "" {
			if !t.s.appendAggregate(&tool.arguments, arguments) {
				return nil
			}
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": tool.blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}))
		}
	}

	if finish != nil && finish != "" && !t.finishSent {
		out = append(out, t.finishBlocks()...)
		if t.s.err != nil {
			return nil
		}
		stop := chatFinishReasonToAnthropic(fmt.Sprint(finish))
		out = append(out, sseEvent("message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 0},
		}))
		t.finishSent = true
	}
	return out
}

func (t *openAIToAnthropic) finishBlocks() []string {
	for _, index := range t.toolOrder {
		tool := t.tools[index]
		if tool == nil || tool.done {
			continue
		}
		if _, err := normalizeStreamToolArguments(t.s, tool); err != nil {
			t.s.addError(fmt.Errorf("stream function call %q arguments: %w", tool.callID, err))
			return nil
		}
	}
	var out []string
	if t.textStarted && !t.textStopped {
		out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
		t.textStopped = true
	}
	for _, index := range t.toolOrder {
		tool := t.tools[index]
		if !tool.done {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.blockIndex}))
			tool.done = true
		}
	}
	return out
}

// ── OpenAI → OpenAI Responses ────────────────

type openAIToResponses struct{ s *streamState }

func (t *openAIToResponses) Err() error        { return t.s.err }
func (t *openAIToResponses) Failure() []string { return responsesStreamFailure(t.s) }
func (t *openAIToResponses) Completed() bool   { return t.s.completed }
func (t *openAIToResponses) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

func (t *openAIToResponses) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent || t.s.completed {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok {
		return nil
	}
	if p.done {
		if err := validateAllResponseToolArguments(t.s); err != nil {
			t.s.addError(err)
			return nil
		}
		out := ensureResponseStarted(t.s)
		out = append(out, finishAllResponseItems(t.s)...)
		return append(out, completeResponse(t.s)...)
	}
	data := p.data
	if data == nil {
		return nil
	}
	if model := getString(data, "model"); model != "" {
		t.s.model = model
	}
	delta, finish, hasContent := firstChoiceDelta(data)
	if err := rejectOpenAIChatRefusal(delta); err != nil {
		t.s.addError(err)
		return nil
	}
	out := ensureResponseStarted(t.s)
	if content, ok := delta["content"].(string); hasContent && ok {
		out = append(out, appendResponseText(t.s, -1, content)...)
	}
	for _, value := range openAIToolCallDeltas(delta) {
		call, ok := value.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		tool, added := ensureResponseTool(
			t.s,
			valueIndex(call["index"]),
			getString(call, "id"),
			getString(function, "name"),
		)
		out = append(out, added...)
		out = append(out, appendResponseToolArguments(t.s, tool, getString(function, "arguments"))...)
	}
	if finish != nil && finish != "" {
		if finish == "length" {
			t.s.incomplete = true
		}
		if err := validateAllResponseToolArguments(t.s); err != nil {
			t.s.addError(err)
			return nil
		}
		out = append(out, finishAllResponseItems(t.s)...)
	}
	return out
}

// ── OpenAI Responses → OpenAI Chat ──────────

// responsesToChat 将 Responses 的 typed SSE events 映射为 Chat Completions chunks。
// 它刻意不把 Responses event JSON 原样转发：两种 API 的流式载荷结构不同。
type responsesToChat struct {
	s           *streamState
	tools       map[int]*streamTool
	toolIndexes map[int]int
	nextTool    int
	roleSent    bool
	doneSent    bool
	refused     bool
	// reasoningSent 记住思考内容是否已通过增量事件推出去，避免 output_item.done
	// 里的完整 summary 造成重复推送。
	reasoningSent bool
}

// reasoningFromSummary 把 reasoning item 自带的 summary 文本转成 reasoning_content。
//
// summary 是 [{type:"summary_text", text:"…"}] 形式的数组；只认 summary_text，
// 其他条目跳过而不报错——OpenAI 后续加新条目类型不该让整条流作废。
func (t *responsesToChat) reasoningFromSummary(item map[string]any) []string {
	text := responsesSummaryText(item)
	if text == "" {
		return nil
	}
	t.reasoningSent = true
	return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"reasoning_content": text}, nil)}
}

// responsesSummaryText 拼出 reasoning item 的 summary 正文，供流式与非流式共用。
func responsesSummaryText(item map[string]any) string {
	summary, _ := item["summary"].([]any)
	var text string
	for _, value := range summary {
		part, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if getString(part, "type") == "summary_text" {
			text += getString(part, "text")
		}
	}
	return text
}

func (t *responsesToChat) Err() error        { return t.s.err }
func (t *responsesToChat) Failure() []string { return chatStreamFailure(t.s) }
func (t *responsesToChat) Completed() bool   { return t.s.completed }
func (t *responsesToChat) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

func (t *responsesToChat) start(model string) []string {
	if model != "" {
		t.s.model = model
	}
	if t.roleSent {
		return nil
	}
	t.roleSent = true
	return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"role": "assistant"}, nil)}
}

func (t *responsesToChat) ensureTool(outputIndex int, item map[string]any) (*streamTool, []string) {
	if tool := t.tools[outputIndex]; tool != nil {
		if callID := getString(item, "call_id"); callID != "" {
			tool.callID = callID
		}
		if name := getString(item, "name"); name != "" {
			tool.name = name
		}
		return tool, nil
	}
	callID := getString(item, "call_id")
	if callID == "" {
		callID = "call_" + shortID()
	}
	tool := &streamTool{sourceIndex: outputIndex, callID: callID, name: getString(item, "name")}
	t.tools[outputIndex] = tool
	t.toolIndexes[outputIndex] = t.nextTool
	t.nextTool++
	return tool, []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"tool_calls": []any{map[string]any{
		"index": t.toolIndexes[outputIndex], "id": tool.callID, "type": "function",
		"function": map[string]any{"name": tool.name, "arguments": ""},
	}}}, nil)}
}

func (t *responsesToChat) appendToolArguments(outputIndex int, value string) []string {
	tool := t.tools[outputIndex]
	if tool == nil {
		t.s.addError(fmt.Errorf("responses function arguments delta arrived before output item %d", outputIndex))
		return nil
	}
	if !t.s.appendAggregate(&tool.arguments, value) {
		return nil
	}
	if value == "" {
		return nil
	}
	return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{"tool_calls": []any{map[string]any{
		"index": t.toolIndexes[outputIndex], "function": map[string]any{"arguments": value},
	}}}, nil)}
}

func (t *responsesToChat) finish(finish string) []string {
	if t.doneSent || t.s.completed {
		return nil
	}
	for _, tool := range t.tools {
		if _, err := normalizeStreamToolArguments(t.s, tool); err != nil {
			t.s.addError(fmt.Errorf("responses function call %q arguments: %w", tool.callID, err))
			return nil
		}
	}
	if finish == "" {
		finish = "stop"
		if len(t.tools) > 0 {
			finish = "tool_calls"
		}
		if t.refused {
			finish = "content_filter"
		}
	}
	t.doneSent = true
	t.s.completed = true
	return []string{chatDelta(t.s.msgID, t.s.model, map[string]any{}, finish), "data: [DONE]\n\n"}
}

func (t *responsesToChat) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent || t.s.completed {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok || p.done || p.data == nil {
		return nil
	}
	data := p.data
	if response, ok := data["response"].(map[string]any); ok {
		if model := getString(response, "model"); model != "" {
			t.s.model = model
		}
	}
	switch getString(data, "type") {
	case "response.created", "response.in_progress":
		return t.start(t.s.model)
	case "response.output_item.added":
		out := t.start(t.s.model)
		item, _ := data["item"].(map[string]any)
		outputIndex := valueIndex(data["output_index"])
		switch getString(item, "type") {
		case "message":
			return out
		case "function_call":
			tool, added := t.ensureTool(outputIndex, item)
			out = append(out, added...)
			if arguments := getString(item, "arguments"); arguments != "" {
				out = append(out, t.appendToolArguments(outputIndex, arguments)...)
			}
			_ = tool
			return out
		case "reasoning":
			// reasoning item 映射到 reasoning_content，与 Anthropic→Chat 一致。
			// item 自带的 summary 在 added 事件里通常是空数组，正文随后由
			// response.reasoning_summary_text.delta 送来。
			return append(out, t.reasoningFromSummary(item)...)
		default:
			t.s.addError(fmt.Errorf("unsupported responses stream output item %q", getString(item, "type")))
			return nil
		}
	case "response.output_text.delta":
		out := t.start(t.s.model)
		return append(out, chatDelta(t.s.msgID, t.s.model, map[string]any{"content": getString(data, "delta")}, nil))
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		out := t.start(t.s.model)
		if delta := getString(data, "delta"); delta != "" {
			t.reasoningSent = true
			out = append(out, chatDelta(t.s.msgID, t.s.model, map[string]any{"reasoning_content": delta}, nil))
		}
		return out
	case "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.done":
		part, _ := data["part"].(map[string]any)
		if part != nil {
			switch getString(part, "type") {
			case "output_text", "refusal":
			default:
				t.s.addError(fmt.Errorf("unsupported responses stream content part %q", getString(part, "type")))
			}
		}
		return nil
	case "response.refusal.delta":
		t.refused = true
		out := t.start(t.s.model)
		return append(out, chatDelta(t.s.msgID, t.s.model, map[string]any{"refusal": getString(data, "delta")}, nil))
	case "response.function_call_arguments.delta":
		out := t.start(t.s.model)
		return append(out, t.appendToolArguments(valueIndex(data["output_index"]), getString(data, "delta"))...)
	case "response.function_call_arguments.done":
		outputIndex := valueIndex(data["output_index"])
		if tool := t.tools[outputIndex]; tool == nil {
			t.s.addError(fmt.Errorf("Responses 函数参数完成事件早于输出项 %d 到达", outputIndex))
			return nil
		} else if tool.arguments.Len() == 0 {
			return t.appendToolArguments(outputIndex, getString(data, "arguments"))
		}
	case "response.output_item.done":
		item, _ := data["item"].(map[string]any)
		switch getString(item, "type") {
		case "message":
		case "function_call":
			outputIndex := valueIndex(data["output_index"])
			tool, added := t.ensureTool(outputIndex, item)
			if tool.arguments.Len() == 0 && getString(item, "arguments") != "" {
				return append(added, t.appendToolArguments(outputIndex, getString(item, "arguments"))...)
			}
			return added
		case "reasoning":
			// 只有在增量事件一次都没来过时才补发：有些上游只在 done 里给完整
			// summary，不发 delta。已经流过就不能重复推，否则思考内容出现两遍。
			if t.reasoningSent {
				return nil
			}
			return t.reasoningFromSummary(item)
		default:
			t.s.addError(fmt.Errorf("unsupported responses stream output item %q", getString(item, "type")))
			return nil
		}
	case "response.completed":
		return t.finish("")
	case "response.incomplete":
		return t.finish("length")
	case "response.failed", "error":
		t.s.failureType = "upstream_error"
		message := getString(data, "message")
		if response, ok := data["response"].(map[string]any); ok {
			if e, ok := response["error"].(map[string]any); ok && getString(e, "message") != "" {
				message = getString(e, "message")
			}
		}
		if message == "" {
			message = "Responses 上游流式响应失败"
		}
		t.s.addError(errors.New(message))
	default:
		eventType := getString(data, "type")
		// reasoning item 的生命周期事件（summary part 增删、summary text 完成等）
		// 没有目标协议等价物，但它们伴随 reasoning item 必然出现，报错等于任何带
		// 推理的上游都用不了。正文已在上面的显式分支里取走。
		if isResponsesReasoningEvent(eventType) {
			return nil
		}
		if strings.HasPrefix(eventType, "response.") {
			t.s.addError(fmt.Errorf("unsupported responses stream event %q", eventType))
		}
	}
	return nil
}

// ── OpenAI Responses → Anthropic ────────────

type responsesToAnthropic struct {
	s           *streamState
	tools       map[int]*streamTool
	blocks      map[int]int
	toolOrder   []int
	textOrder   []int
	nextBlock   int
	textStarted map[int]bool
	textStopped map[int]bool
	started     bool
	completed   bool
	refused     bool
}

func (t *responsesToAnthropic) Err() error        { return t.s.err }
func (t *responsesToAnthropic) Failure() []string { return anthropicStreamFailure(t.s) }
func (t *responsesToAnthropic) Completed() bool   { return t.s.completed }
func (t *responsesToAnthropic) Abort(kind, message string) []string {
	t.s.abort(kind, message)
	return t.Failure()
}

func (t *responsesToAnthropic) start(model string) []string {
	if model != "" {
		t.s.model = model
	}
	if t.started {
		return nil
	}
	t.started = true
	t.s.started = true
	return []string{sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + shortID(), "type": "message", "role": "assistant", "model": t.s.model,
			"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})}
}

func (t *responsesToAnthropic) ensureText(outputIndex int) []string {
	if t.textStarted[outputIndex] {
		return nil
	}
	blockIndex := t.nextBlock
	t.nextBlock++
	t.blocks[outputIndex] = blockIndex
	t.textStarted[outputIndex] = true
	t.textOrder = append(t.textOrder, outputIndex)
	return []string{sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})}
}

func (t *responsesToAnthropic) ensureTool(outputIndex int, item map[string]any) (*streamTool, []string) {
	if tool := t.tools[outputIndex]; tool != nil {
		if callID := getString(item, "call_id"); callID != "" {
			tool.callID = callID
		}
		if name := getString(item, "name"); name != "" {
			tool.name = name
		}
		return tool, nil
	}
	callID := getString(item, "call_id")
	if callID == "" {
		callID = "call_" + shortID()
	}
	tool := &streamTool{sourceIndex: outputIndex, blockIndex: t.nextBlock, callID: callID, name: getString(item, "name")}
	t.nextBlock++
	t.tools[outputIndex] = tool
	t.toolOrder = append(t.toolOrder, outputIndex)
	return tool, []string{sseEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": tool.blockIndex,
		"content_block": map[string]any{"type": "tool_use", "id": tool.callID, "name": tool.name, "input": map[string]any{}},
	})}
}

func (t *responsesToAnthropic) appendToolArguments(outputIndex int, value string) []string {
	tool := t.tools[outputIndex]
	if tool == nil {
		t.s.addError(fmt.Errorf("responses function arguments delta arrived before output item %d", outputIndex))
		return nil
	}
	if !t.s.appendAggregate(&tool.arguments, value) {
		return nil
	}
	if value == "" {
		return nil
	}
	return []string{sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tool.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": value},
	})}
}

// responsesEventUsage 从 response.completed / response.incomplete 事件里取 usage。
// Responses 把终态快照放在事件的 response 字段下，usage 是它的子字段。
func responsesEventUsage(data map[string]any) map[string]any {
	response, _ := data["response"].(map[string]any)
	usage, _ := response["usage"].(map[string]any)
	return usage
}

// anthropicStreamUsage 把上游 usage 折成 Anthropic message_delta 的 usage 形状。
//
// 只带 output_tokens：Anthropic 原生流式在 message_delta 里只报增量产出，
// input_tokens 已在 message_start 里给过。上游没给 usage 时退回 0，
// 保持字段恒在，避免客户端读到缺字段。
func anthropicStreamUsage(usage map[string]any) map[string]any {
	return map[string]any{"output_tokens": getIntDefault(usage, "output_tokens", 0)}
}

// finish 收尾 Responses 流。usage 取自触发收尾的 response.completed /
// response.incomplete 事件；Responses 协议在这两个事件里必然带 usage，
// 硬编码 0 会让客户端侧的成本核算和配额统计全部失真。
func (t *responsesToAnthropic) finish(incomplete bool, usage map[string]any) []string {
	if t.completed || t.s.completed {
		return nil
	}
	var out []string
	for _, outputIndex := range t.textOrder {
		if !t.textStopped[outputIndex] {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blocks[outputIndex]}))
			t.textStopped[outputIndex] = true
		}
	}
	for _, outputIndex := range t.toolOrder {
		tool := t.tools[outputIndex]
		if _, err := normalizeStreamToolArguments(t.s, tool); err != nil {
			t.s.addError(fmt.Errorf("responses function call %q arguments: %w", tool.callID, err))
			return nil
		}
		if !tool.done {
			out = append(out, sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.blockIndex}))
			tool.done = true
		}
	}
	stop := "end_turn"
	if incomplete {
		stop = "max_tokens"
	} else if len(t.tools) > 0 {
		stop = "tool_use"
	} else if t.refused {
		stop = "refusal"
	}
	out = append(out,
		sseEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": anthropicStreamUsage(usage)}),
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	t.completed = true
	t.s.completed = true
	return out
}

func (t *responsesToAnthropic) Transform(line string) []string {
	if t.s.err != nil || t.s.failureSent || t.s.completed {
		return nil
	}
	p := parseSseLine(line)
	if !p.ok || p.done || p.data == nil {
		return nil
	}
	data := p.data
	if response, ok := data["response"].(map[string]any); ok {
		if model := getString(response, "model"); model != "" {
			t.s.model = model
		}
	}
	switch getString(data, "type") {
	case "response.created", "response.in_progress":
		return t.start(t.s.model)
	case "response.output_item.added":
		out := t.start(t.s.model)
		item, _ := data["item"].(map[string]any)
		outputIndex := valueIndex(data["output_index"])
		switch getString(item, "type") {
		case "message":
			return out
		case "function_call":
			_, added := t.ensureTool(outputIndex, item)
			out = append(out, added...)
			if arguments := getString(item, "arguments"); arguments != "" {
				out = append(out, t.appendToolArguments(outputIndex, arguments)...)
			}
			return out
		case "reasoning":
			// 丢弃而不是转成 thinking 块：thinking 的 signature 是 Anthropic 的加密
			// 签名，只有它自己能签发。伪造一个，客户端（如 Claude Code）把这段
			// assistant 历史回传给 Anthropic 上游时验签会失败，下一轮直接 400。
			// 报错也不行：那会让任何带推理的 Responses 上游彻底不可用。
			return out
		default:
			t.s.addError(fmt.Errorf("unsupported responses stream output item %q", getString(item, "type")))
			return nil
		}
	case "response.output_text.delta", "response.refusal.delta":
		outputIndex := valueIndex(data["output_index"])
		out := t.start(t.s.model)
		out = append(out, t.ensureText(outputIndex)...)
		if getString(data, "type") == "response.refusal.delta" {
			t.refused = true
		}
		if delta := getString(data, "delta"); delta != "" {
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": t.blocks[outputIndex],
				"delta": map[string]any{"type": "text_delta", "text": delta},
			}))
		}
		return out
	case "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.done":
		part, _ := data["part"].(map[string]any)
		if part != nil {
			switch getString(part, "type") {
			case "output_text", "refusal":
				out := t.start(t.s.model)
				return append(out, t.ensureText(valueIndex(data["output_index"]))...)
			default:
				t.s.addError(fmt.Errorf("unsupported responses stream content part %q", getString(part, "type")))
			}
		}
	case "response.function_call_arguments.delta":
		out := t.start(t.s.model)
		return append(out, t.appendToolArguments(valueIndex(data["output_index"]), getString(data, "delta"))...)
	case "response.function_call_arguments.done":
		outputIndex := valueIndex(data["output_index"])
		if tool := t.tools[outputIndex]; tool == nil {
			t.s.addError(fmt.Errorf("Responses 函数参数完成事件早于输出项 %d 到达", outputIndex))
			return nil
		} else if tool.arguments.Len() == 0 {
			return t.appendToolArguments(outputIndex, getString(data, "arguments"))
		}
	case "response.output_item.done":
		item, _ := data["item"].(map[string]any)
		switch getString(item, "type") {
		case "message":
		case "function_call":
			outputIndex := valueIndex(data["output_index"])
			tool, added := t.ensureTool(outputIndex, item)
			if tool.arguments.Len() == 0 && getString(item, "arguments") != "" {
				return append(added, t.appendToolArguments(outputIndex, getString(item, "arguments"))...)
			}
			return added
		case "reasoning":
			// 丢弃，理由见 output_item.added 的同名分支。
		default:
			t.s.addError(fmt.Errorf("unsupported responses stream output item %q", getString(item, "type")))
			return nil
		}
	case "response.completed":
		return t.finish(false, responsesEventUsage(data))
	case "response.incomplete":
		return t.finish(true, responsesEventUsage(data))
	case "response.failed", "error":
		t.s.failureType = "upstream_error"
		message := getString(data, "message")
		if response, ok := data["response"].(map[string]any); ok {
			if e, ok := response["error"].(map[string]any); ok && getString(e, "message") != "" {
				message = getString(e, "message")
			}
		}
		if message == "" {
			message = "Responses 上游流式响应失败"
		}
		t.s.addError(errors.New(message))
	default:
		eventType := getString(data, "type")
		// reasoning item 的生命周期事件（summary part 增删、summary text 完成等）
		// 没有目标协议等价物，但它们伴随 reasoning item 必然出现，报错等于任何带
		// 推理的上游都用不了。正文已在上面的显式分支里取走。
		if isResponsesReasoningEvent(eventType) {
			return nil
		}
		if strings.HasPrefix(eventType, "response.") {
			t.s.addError(fmt.Errorf("unsupported responses stream event %q", eventType))
		}
	}
	return nil
}
