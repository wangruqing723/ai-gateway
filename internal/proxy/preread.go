package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UpstreamStreamError 表示上游先返回了 2xx，随后在 SSE 流里给出失败事件。
//
// Status 是网关按错误类型换算出的「等效 HTTP 状态码」：上游真实状态码仍是 200
// （OnUpstreamStatus 报的也是 200），但这次失败在语义上等价于上游直接回 429/5xx。
// 故障转移、本地重试与熔断都按 Status 分类，与 header 阶段的同类失败保持一致。
type UpstreamStreamError struct {
	Status  int
	Type    string
	Code    string
	Message string
	// Raw 是触发失败的那条 SSE data 载荷，仅供排查记录，不写回客户端。
	Raw []byte
}

func (e *UpstreamStreamError) Error() string {
	message := e.Message
	if message == "" {
		message = "上游流式响应失败"
	}
	return fmt.Sprintf("上游在 200 之后于流内报错（等效 HTTP %d）: %s", e.Status, message)
}

// streamChunk 是后台读取 goroutine 交给主循环的一帧：要么带数据，要么带结束原因。
type streamChunk struct {
	data []byte
	err  error
}

// streamPrereadHold 是预读阶段在「只有开场事件、还没有内容」时最多等待的时长。
//
// 实测上游的流内失败事件出现在开场后 2ms~475ms，3s 留足余量；正常请求里第一个
// 内容事件一到就立即提交，不会等满。声明成变量而不是常量，测试要调小它。
var streamPrereadHold = 3 * time.Second

// readStreamChunks 是流式响应唯一的读取方，预读阶段与正文阶段共用同一路数据，
// 避免两个 goroutine 同时读 resp.Body。客户端断开或活跃超时后 ctx 被取消，
// 这里要么被 Body 关闭唤醒，要么在发送时看到 ctx 结束而退出。
func readStreamChunks(ctx context.Context, body io.Reader, out chan<- streamChunk) {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case out <- streamChunk{data: chunk}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case out <- streamChunk{err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

// prereadResult 是预读阶段的结论。三种出口互斥：
//   - aborted：预读期客户端断开（ctx 结束），一个字节都没写给客户端；
//   - failure：命中上游流内失败事件，同样一个字节都没写；
//   - 其余：可以提交，buffered 是要原样写给客户端的开场字节，readErr 是提交时
//     已经拿到的结束原因（EOF / 读错误），没有则为 nil。
type prereadResult struct {
	buffered []byte
	failure  *UpstreamStreamError
	readErr  error
	aborted  bool
}

// prereadStream 暂存上游开场事件，直到能判断这次流是否值得转发。
//
// 提交条件（任一）：
//   - 读到第一个既不是开场、也不是失败的事件（内容 / 推理 / 工具调用 / 完成）；
//   - 等待超过 streamPrereadHold；
//   - 暂存超过 maxSSEEventBytes；
//   - 上游 EOF 或读错误（按原有收尾逻辑处理，语义不变）。
//
// 只有「失败事件」这一种出口会让调用方放弃本次尝试。
func prereadStream(ctx context.Context, chunks <-chan streamChunk, opts *Options) prereadResult {
	hold := time.NewTimer(streamPrereadHold)
	defer hold.Stop()

	parser := &streamPreamble{format: opts.Provider.Format}
	var buffered []byte
	for {
		select {
		case <-ctx.Done():
			return prereadResult{buffered: buffered, aborted: true}
		case <-hold.C:
			return prereadResult{buffered: buffered}
		case part, ok := <-chunks:
			if !ok {
				return prereadResult{buffered: buffered, readErr: io.EOF}
			}
			if len(part.data) > 0 {
				buffered = append(buffered, part.data...)
				commit, failure := parser.feed(part.data)
				if failure != nil {
					return prereadResult{buffered: buffered, failure: failure}
				}
				if commit || len(buffered) > maxSSEEventBytes {
					return prereadResult{buffered: buffered, readErr: part.err}
				}
			}
			if part.err != nil {
				return prereadResult{buffered: buffered, readErr: part.err}
			}
		}
	}
}

// streamPreamble 按上游格式识别「开场事件」与「失败事件」。
//
// 只看 data: 行的 JSON 载荷，与 event: 行无关（openai-chat 没有 event: 行）。
// 看不懂的行一律视为「非开场」并立即提交：网关不理解的协议不该被扣在这里。
type streamPreamble struct {
	format string
	line   []byte
}

// feed 处理一段原始字节，返回 (是否提交, 失败事件)。
func (p *streamPreamble) feed(chunk []byte) (bool, *UpstreamStreamError) {
	for {
		idx := bytes.IndexByte(chunk, '\n')
		if idx < 0 {
			if len(p.line)+len(chunk) > maxSSEEventBytes {
				return true, nil
			}
			p.line = append(p.line, chunk...)
			return false, nil
		}
		line := string(p.line) + string(chunk[:idx])
		p.line = p.line[:0]
		chunk = chunk[idx+1:]
		line = strings.TrimSpace(line)
		// 空行是事件分隔符，':' 开头是 keep-alive 注释，都不影响判定。
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			return true, nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			return true, nil
		}
		commit, failure := p.classify(data, []byte(payload))
		if commit || failure != nil {
			return commit, failure
		}
	}
}

func (p *streamPreamble) classify(data map[string]any, raw []byte) (bool, *UpstreamStreamError) {
	switch p.format {
	case "openai-responses":
		switch prereadString(data, "type") {
		case "response.created", "response.in_progress":
			return false, nil
		case "error", "response.failed":
			return false, responsesStreamError(data, raw)
		default:
			return true, nil
		}
	case "anthropic":
		switch prereadString(data, "type") {
		case "message_start", "ping":
			return false, nil
		case "error":
			return false, anthropicStreamError(data, raw)
		default:
			return true, nil
		}
	default:
		// openai / openai-chat：顶层 error 对象即失败；只带 role 的 delta、
		// 空 choices 这类没有内容的事件算开场。
		if obj, ok := data["error"].(map[string]any); ok {
			return false, chatStreamError(obj, raw)
		}
		if isChatPreamble(data) {
			return false, nil
		}
		return true, nil
	}
}

// isChatPreamble 判断 openai-chat 的一个 chunk 是否不含任何内容。
func isChatPreamble(data map[string]any) bool {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		// 空 choices 的块（keep-alive 或仅 usage）没有内容可发。
		_, hasMessage := data["message"]
		return !hasMessage
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return true
	}
	if finish, ok := choice["finish_reason"]; ok && finish != nil && finish != "" {
		return false
	}
	if _, ok := choice["message"]; ok {
		return false
	}
	delta, _ := choice["delta"].(map[string]any)
	for _, key := range []string{"content", "tool_calls", "function_call", "reasoning_content", "reasoning", "refusal"} {
		if value, ok := delta[key]; ok && value != nil && value != "" {
			return false
		}
	}
	return true
}

func responsesStreamError(data map[string]any, raw []byte) *UpstreamStreamError {
	e := &UpstreamStreamError{Raw: raw}
	// 官方 response.failed 把错误放在 response.error；兼容 OpenAI 的上游在流中途
	// 直接发 error 事件，错误在扁平的 error 对象里。两种都要认。
	if response, ok := data["response"].(map[string]any); ok {
		if obj, ok := response["error"].(map[string]any); ok {
			e.Type = prereadString(obj, "type")
			e.Code = prereadString(obj, "code")
			e.Message = prereadString(obj, "message")
		}
		// 部分上游把 type/code 放在 response 顶层。
		if e.Code == "" {
			e.Code = prereadString(response, "code")
		}
	}
	if obj, ok := data["error"].(map[string]any); ok {
		if e.Type == "" {
			e.Type = prereadString(obj, "type")
		}
		if e.Code == "" {
			e.Code = prereadString(obj, "code")
		}
		if e.Message == "" {
			e.Message = prereadString(obj, "message")
		}
	}
	if e.Message == "" {
		e.Message = prereadString(data, "message")
	}
	if e.Message == "" {
		e.Message = "Responses 上游流式响应失败"
	}
	e.Status = upstreamStreamStatus(e.Type, e.Code)
	return e
}

func anthropicStreamError(data map[string]any, raw []byte) *UpstreamStreamError {
	e := &UpstreamStreamError{Raw: raw, Type: prereadString(data, "type")}
	if obj, ok := data["error"].(map[string]any); ok {
		e.Type = prereadString(obj, "type")
		e.Message = prereadString(obj, "message")
	}
	if e.Message == "" {
		e.Message = "Anthropic 上游流式响应失败"
	}
	e.Status = upstreamStreamStatus(e.Type, "")
	return e
}

func chatStreamError(obj map[string]any, raw []byte) *UpstreamStreamError {
	e := &UpstreamStreamError{
		Raw:     raw,
		Type:    prereadString(obj, "type"),
		Code:    prereadString(obj, "code"),
		Message: prereadString(obj, "message"),
	}
	if e.Message == "" {
		e.Message = "OpenAI 上游流式响应失败"
	}
	e.Status = upstreamStreamStatus(e.Type, e.Code)
	return e
}

// upstreamStreamStatus 把流内错误的 type / code 换算成等效 HTTP 状态码。
//
// 拿不准的一律按 502 处理：既算上游故障（计入熔断），也能被 onServerError 转移。
func upstreamStreamStatus(errType, errCode string) int {
	key := strings.ToLower(strings.TrimSpace(errCode))
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(errType))
	}
	switch key {
	case "rate_limit_exceeded", "rate_limit_error", "too_many_requests", "insufficient_quota", "quota_exceeded":
		return http.StatusTooManyRequests
	case "overloaded_error", "overloaded", "slow_down":
		return 529
	case "service_unavailable":
		return http.StatusServiceUnavailable
	case "server_error", "api_error", "internal_server_error", "internal_error":
		return http.StatusInternalServerError
	case "authentication_error", "invalid_api_key", "invalid_authentication":
		return http.StatusUnauthorized
	case "permission_error", "insufficient_permissions":
		return http.StatusForbidden
	case "invalid_request_error", "invalid_request", "bad_request", "context_length_exceeded":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// writeStreamErrorAsHTTP 把尚未提交的流内失败写成真实的 HTTP 错误响应。
//
// 此时客户端还没收到任何字节，写真实状态码比「200 + SSE 失败事件」更准确：
// 客户端自己的 429 退避重试能生效，日志与指标也不会把这次请求记成 200。
func writeStreamErrorAsHTTP(opts *Options, e *UpstreamStreamError) error {
	timeout := time.Duration(opts.StreamActivityTimeout) * time.Millisecond
	if opts.StreamActivityTimeout <= 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	payload := map[string]any{
		"error": map[string]any{
			"type":    clientStreamErrorType(e.Status),
			"message": e.Message,
		},
	}
	if opts.ClientFormat == "anthropic" {
		// Anthropic 的错误响应在顶层也带 type: error，客户端按这个字段区分。
		payload["type"] = "error"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":{"type":"api_error","message":"上游流式响应失败"}}`)
	}
	deadline := time.Now()
	if timeout > 0 {
		deadline = deadline.Add(timeout)
	}
	return writeResponseAtDeadline(opts.ClientRes, e.Status, body, deadline)
}

func clientStreamErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func prereadString(m map[string]any, key string) string {
	value, ok := m[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
