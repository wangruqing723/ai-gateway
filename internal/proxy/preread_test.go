package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// responsesRateLimitStream 复现 r00059：上游 200 之后先发开场事件，再发限流错误。
const responsesRateLimitStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_test","status":"in_progress","model":"upstream"}}

event: error
data: {"type":"error","error":{"type":"too_many_requests","code":"rate_limit_exceeded","message":"Your requests have exceeded token rate limit."},"sequence_number":1}

`

func newStreamServer(check func(*http.Request), handler func(w http.ResponseWriter)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		handler(w)
	}))
}

// firstWriteRecorder 记录第一次真正写给客户端的时刻。
// 预读提交时会先写响应头再补写开场字节，只有后者才算「首字节到达」。
type firstWriteRecorder struct {
	*httptest.ResponseRecorder
	mu         sync.Mutex
	firstWrite time.Time
}

func (r *firstWriteRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	if r.firstWrite.IsZero() {
		r.firstWrite = time.Now()
	}
	r.mu.Unlock()
	return r.ResponseRecorder.Write(p)
}

func (r *firstWriteRecorder) Flush() { r.ResponseRecorder.Flush() }

func (r *firstWriteRecorder) firstWriteAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstWrite
}

func TestPrereadAbandonsOnResponsesRateLimit(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, responsesRateLimitStream)
	})
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai-responses", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if calls[0].upstreamCode != http.StatusTooManyRequests {
		t.Fatalf("upstreamCode = %d, want 429（流内限流换算）", calls[0].upstreamCode)
	}
	var streamErr *UpstreamStreamError
	if !errors.As(calls[0].err, &streamErr) {
		t.Fatalf("err = %v, want *UpstreamStreamError", calls[0].err)
	}
	if !strings.Contains(streamErr.Message, "exceeded token rate limit") {
		t.Fatalf("Message = %q, want 上游原文", streamErr.Message)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

func TestPrereadAbandonsOnAnthropicOverloaded(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"upstream\"}}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	})
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "anthropic", "anthropic", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 || calls[0].upstreamCode != 529 {
		t.Fatalf("ShouldRetry calls = %+v, want 一次且 code 529", calls)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

func TestPrereadAbandonsOnChatQuotaError(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"error\":{\"type\":\"insufficient_quota\",\"message\":\"quota exhausted\"}}\n\n")
	})
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 || calls[0].upstreamCode != http.StatusTooManyRequests {
		t.Fatalf("ShouldRetry calls = %+v, want 一次且 code 429", calls)
	}
}

func TestPrereadCommitsOnFirstContent(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"upstream\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":\"upstream\"}}\n\n")
	})
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai-responses", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 0（已提交，不得转移）", len(calls))
	}
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "hello") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("status/body = %d/%q, want 200 且含内容与终止标记", recorder.Code, body)
	}
}

// 同格式透传必须逐字节保持上游内容：预读只是推迟，不能改写。
func TestPrereadPassthroughBytesUnchanged(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\"}}\n\n"
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, stream)
	})
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai-responses", "openai-responses", true, 500, 500)
	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if got := recorder.Body.String(); got != stream {
		t.Fatalf("透传字节被改写:\n got %q\nwant %q", got, stream)
	}
}

// 只有开场、迟迟没有内容：等满保持上限后原样提交，不得扣住客户端。
func TestPrereadHoldCommitsPreambleOnly(t *testing.T) {
	previous := streamPrereadHold
	streamPrereadHold = 40 * time.Millisecond
	t.Cleanup(func() { streamPrereadHold = previous })

	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// 内容故意晚于保持上限到达：客户端必须先拿到开场部分，不能被扣住。
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"},\"finish_reason\":\"stop\"}]}\n\n")
	})
	defer upstream.Close()

	recorder := &firstWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 2000)
	started := time.Now()
	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	first := recorder.firstWriteAt()
	if first.IsZero() || first.Sub(started) > 150*time.Millisecond {
		t.Fatalf("首字节耗时 = %s, want 在保持上限附近提交、不等上游内容", first.Sub(started))
	}
	if body := recorder.Body.String(); !strings.Contains(body, "late") {
		t.Fatalf("body = %q, want 保持上限之后的内容", body)
	}
}

// 不允许转移时写真实 HTTP 状态码，而不是 200 + SSE 失败事件。
func TestPrereadWritesHTTPErrorWhenNotAbandoned(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, responsesRateLimitStream)
	})
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai-responses", "anthropic", true, 500, 500)
	opts.ShouldRetry = recordingHook(false, &calls)

	err := Forward(opts)
	var streamErr *UpstreamStreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("Forward() error = %v, want *UpstreamStreamError", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "exceeded token rate limit") || strings.Contains(body, "event:") {
		t.Fatalf("body = %q, want Anthropic 错误 JSON 带上游原文", body)
	}
}

// 预读期客户端断开：一个字节都不写，也不算上游故障。
func TestPrereadClientDisconnectWritesNothing(t *testing.T) {
	upstream := newStreamServer(nil, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-time.After(2 * time.Second)
	})
	defer upstream.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 5_000)
	opts.ClientReq = opts.ClientReq.WithContext(clientCtx)

	done := make(chan error, 1)
	go func() { done <- Forward(opts) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Forward() error = %v, want nil（客户端断开不算错误）", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("客户端断开后 Forward 未返回")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("客户端断开后写入了 %d 字节，want 0", recorder.Body.Len())
	}
}
