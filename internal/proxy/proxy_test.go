package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

const (
	testMaxResponseBodyBytes = 32 << 20
	testMaxErrorBodyBytes    = 1 << 20
	testMaxSSEEventBytes     = 8 << 20
)

func TestForwardStreamContinuesAfterHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 500))
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, "data: first") || !strings.Contains(body, "data: second") {
		t.Fatalf("stream body = %q, want both SSE chunks", body)
	}
}

func TestForwardStreamHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	started := time.Now()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 40, 500))
	if err == nil {
		t.Fatal("Forward() error = nil, want header timeout")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Forward() elapsed = %s, want prompt header timeout", elapsed)
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusGatewayTimeout)
	}
	if !strings.Contains(recorder.Body.String(), "timeout_error") {
		t.Fatalf("body = %q, want timeout_error", recorder.Body.String())
	}
}

func TestForwardStreamErrorBodyTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 40)
	opts.ClientReq = opts.ClientReq.WithContext(clientCtx)
	done := make(chan error, 1)
	go func() { done <- Forward(opts) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Forward() error = nil, want error-body timeout")
		}
		if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(recorder.Body.String(), "timeout_error") {
			t.Fatalf("status/body = %d/%q, want 504 timeout_error", recorder.Code, recorder.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		cancelClient()
		close(release)
		<-done
		t.Fatal("streaming error response body held Forward past activity timeout")
	}
}

func TestForwardStreamWriteDeadlineReleasesBlockedClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: blocked-client\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	writer := newDeadlineBlockingWriter()
	done := make(chan error, 1)
	go func() {
		done <- Forward(forwardTestOptions(upstream, writer, "openai", "openai-chat", true, 500, 40))
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Forward() error = nil, want downstream write timeout")
		}
	case <-time.After(250 * time.Millisecond):
		writer.release()
		<-done
		t.Fatal("blocked downstream client held Forward without a write deadline")
	}
}

func TestForwardStreamHTTPErrorWriteDeadlineReleasesBlockedClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	defer upstream.Close()

	writer := newDeadlineBlockingWriter()
	done := make(chan error, 1)
	go func() {
		done <- Forward(forwardTestOptions(upstream, writer, "openai", "openai-chat", true, 500, 40))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Forward() error = nil, want downstream write timeout")
		}
	case <-time.After(250 * time.Millisecond):
		writer.release()
		<-done
		t.Fatal("stream HTTP error write blocked without a deadline")
	}
}

func TestForwardReturnsImmediatelyAfterTransformedSuccessTerminal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"model":"gpt-upstream","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"model":"gpt-upstream","choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	started := time.Now()
	err := Forward(forwardTestOptions(upstream, httptest.NewRecorder(), "openai", "openai-responses", true, 500, 500))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Forward() waited %s after success terminal", elapsed)
	}
}

func TestForwardTransformedEOFUsesTransformerFailureState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-upstream\"}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "anthropic", "openai-responses", true, 500, 500))
	if err == nil {
		t.Fatal("Forward() error = nil, want incomplete transformed stream error")
	}
	assertResponsesFailureContinuesCreatedState(t, recorder.Body.String())
}

func TestForwardTransformedTimeoutUsesTransformerFailureState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-upstream\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "anthropic", "openai-responses", true, 500, 40))
	if err == nil {
		t.Fatal("Forward() error = nil, want transformed stream timeout")
	}
	assertResponsesFailureContinuesCreatedState(t, recorder.Body.String())
}

func TestForwardNonStreamBodyTimeoutReturns504(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()

			recorder := httptest.NewRecorder()
			opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 0, 0)
			opts.TimeoutMs = 40
			if err := Forward(opts); err == nil {
				t.Fatal("Forward() error = nil, want body timeout")
			}
			if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(recorder.Body.String(), "timeout_error") {
				t.Fatalf("status/body = %d/%q, want 504 timeout_error", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestForwardNonStreamWriteDeadlineReleasesBlockedClient(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = io.WriteString(w, `{"id":"chatcmpl-test","model":"test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				} else {
					_, _ = io.WriteString(w, `{"error":"bad request"}`)
				}
			}))
			defer upstream.Close()

			writer := newDeadlineBlockingWriter()
			opts := forwardTestOptions(upstream, writer, "openai", "openai-chat", false, 0, 0)
			opts.TimeoutMs = 40
			done := make(chan error, 1)
			go func() { done <- Forward(opts) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Forward() error = nil, want downstream write timeout")
				}
			case <-time.After(250 * time.Millisecond):
				writer.release()
				<-done
				t.Fatal("blocked non-stream client held Forward without a write deadline")
			}
		})
	}
}

func TestForwardStreamActivityTimeoutResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "openai-responses", true, 500, 50))
	if err == nil || !strings.Contains(err.Error(), "流式传输活跃超时") {
		t.Fatalf("Forward() error = %v, want identifiable activity timeout", err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") || !strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("stream body = %q, want response.failed event", body)
	}
	if !strings.Contains(body, `"error":{"code":"timeout_error"`) {
		t.Fatalf("stream body = %q, want Responses error code", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body = %q, Responses timeout must not use [DONE]", body)
	}
}

func TestForwardRejectsOversizedResponse(t *testing.T) {
	t.Run("success response", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-test","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"`)
			writeRepeated(w, 'x', testMaxResponseBodyBytes+1)
			_, _ = io.WriteString(w, `"},"finish_reason":"stop"}]}`)
		}))
		defer upstream.Close()

		recorder := httptest.NewRecorder()
		err := Forward(forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 0, 0))
		if err == nil {
			t.Fatal("Forward() error = nil, want oversized response rejection")
		}
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
		}
	})

	t.Run("error response", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			writeRepeated(w, 'x', testMaxErrorBodyBytes+1)
		}))
		defer upstream.Close()

		recorder := httptest.NewRecorder()
		err := Forward(forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 0, 0))
		if err == nil {
			t.Fatal("Forward() error = nil, want oversized error response rejection")
		}
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
		}
	})
}

func TestForwardRejectsOversizedSSEEvent(t *testing.T) {
	tests := []struct {
		name         string
		clientFormat string
		wantEvent    string
	}{
		{name: "anthropic", clientFormat: "anthropic", wantEvent: "event: error"},
		{name: "responses", clientFormat: "openai-responses", wantEvent: "event: response.failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newSSEServer(openAISSELine(testMaxSSEEventBytes + 1))
			defer upstream.Close()

			recorder := httptest.NewRecorder()
			err := Forward(forwardTestOptions(upstream, recorder, "openai", tt.clientFormat, true, 500, 500))
			if err == nil {
				t.Fatal("Forward() error = nil, want oversized SSE event rejection")
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want already-committed 200", recorder.Code)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, tt.wantEvent) || !strings.Contains(body, "upstream_error") {
				t.Fatalf("stream body suffix missing protocol failure: %q", tail(body, 512))
			}
			if strings.Contains(body, "[DONE]") {
				t.Fatalf("stream body suffix = %q, failure must not use [DONE]", tail(body, 512))
			}
		})
	}
}

func TestForwardAcceptsSSEEventAtLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openAISSELine(testMaxSSEEventBytes)+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "anthropic", true, 500, 500))
	if err != nil {
		t.Fatalf("Forward() error = %v, want exactly 8 MiB SSE event accepted", err)
	}
	if !strings.Contains(recorder.Body.String(), "event: content_block_delta") {
		t.Fatalf("stream body missing transformed event, suffix = %q", tail(recorder.Body.String(), 512))
	}
}

func TestHandleStreamReadErrorWritesResponsesFailure(t *testing.T) {
	readErr := errors.New("upstream read failed")
	resp := &http.Response{Body: io.NopCloser(fixedErrorReader{err: readErr})}
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := &Options{
		ClientRes:             recorder,
		Provider:              &config.Provider{Format: "openai"},
		ClientFormat:          "openai-responses",
		StreamActivityTimeout: 500,
		Log:                   func(string, ...any) {},
	}

	err := handleStream(ctx, cancel, resp, opts)
	if !errors.Is(err, readErr) {
		t.Fatalf("handleStream() error = %v, want %v", err, readErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "upstream_error") {
		t.Fatalf("stream body = %q, want Responses failure terminal", body)
	}
}

func TestForwardRejectsNonStreamConversionError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "anthropic", false, 0, 0))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Forward() error = %v, want explicit conversion error", err)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "conversion_error") || strings.Contains(body, `"type":"message"`) {
		t.Fatalf("body = %q, want conversion_error without converted success payload", body)
	}
}

func TestForwardRejectsStreamConversionError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"model\":\"gpt-upstream\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_bad\",\"type\":\"function\",\"function\":{\"name\":\"bad\",\"arguments\":\"[]\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"model\":\"gpt-upstream\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "openai", "anthropic", true, 500, 500))
	if err == nil || !strings.Contains(err.Error(), "arguments") {
		t.Fatalf("Forward() error = %v, want explicit stream conversion error", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("stream body = %q, want Anthropic conversion failure terminal", body)
	}
	if strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("stream body = %q, invalid tool call must not emit a success terminal", body)
	}
}

func TestForwardResponsesConversionFailureKeepsStreamIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-upstream\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_bad\",\"name\":\"bad\",\"input\":[]}}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	err := Forward(forwardTestOptions(upstream, recorder, "anthropic", "openai-responses", true, 500, 500))
	if err == nil {
		t.Fatal("Forward() error = nil, want conversion failure")
	}
	events := proxyResponseEvents(t, recorder.Body.String())
	created := events["response.created"]
	failed := events["response.failed"]
	createdResponse, _ := created["response"].(map[string]any)
	failedResponse, _ := failed["response"].(map[string]any)
	if createdResponse["id"] == nil || failedResponse["id"] != createdResponse["id"] {
		t.Fatalf("stream response ids differ: created=%#v failed=%#v", createdResponse, failedResponse)
	}
	createdSeq, _ := created["sequence_number"].(float64)
	failedSeq, ok := failed["sequence_number"].(float64)
	if !ok || failedSeq != createdSeq+1 {
		t.Fatalf("sequence numbers = %v -> %v, want continuous", created["sequence_number"], failed["sequence_number"])
	}
}

func TestForwardClientCancelBeforeHeaders(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	writer := newTrackingStreamWriter()
	opts := forwardTestOptions(upstream, writer, "openai", "openai-chat", true, 500, 500)
	opts.ClientReq = opts.ClientReq.WithContext(clientCtx)
	go func() {
		<-started
		cancelClient()
	}()

	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v, want normal client cancellation", err)
	}
	if writer.status != 0 || writer.body.Len() != 0 {
		t.Fatalf("client response status/body = %d/%q, want no 502 write", writer.status, writer.body.String())
	}
}

func TestForwardClientCancelAfterHeaders(t *testing.T) {
	upstream := newSSEServer("data: first")
	defer upstream.Close()

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	writer := newTrackingStreamWriter()
	writer.beforeWrite = cancelClient
	writer.writeErr = io.ErrClosedPipe
	opts := forwardTestOptions(upstream, writer, "openai", "openai-chat", true, 500, 500)
	opts.ClientReq = opts.ClientReq.WithContext(clientCtx)

	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v, want normal client cancellation", err)
	}
	if writer.status != http.StatusOK || writer.body.Len() != 0 {
		t.Fatalf("client response status/body = %d/%q, want committed 200 with no further payload", writer.status, writer.body.String())
	}
}

func forwardTestOptions(upstream *httptest.Server, recorder http.ResponseWriter, providerFormat, clientFormat string, streaming bool, headerTimeoutMs, activityTimeoutMs int) *Options {
	return &Options{
		ClientReq:             httptest.NewRequest(http.MethodPost, "http://client.test/v1/messages", nil),
		ClientRes:             recorder,
		UpstreamBody:          []byte(`{}`),
		Provider:              &config.Provider{BaseURL: upstream.URL, APIKey: "test-key", Format: providerFormat},
		ClientFormat:          clientFormat,
		OriginalModel:         "test-model",
		IsStreaming:           streaming,
		Log:                   func(string, ...any) {},
		StartTime:             time.Now(),
		TimeoutMs:             2_000,
		HeaderTimeoutMs:       headerTimeoutMs,
		StreamActivityTimeout: activityTimeoutMs,
		HTTPClient:            upstream.Client(),
	}
}

func writeRepeated(w io.Writer, value byte, count int) {
	chunk := strings.Repeat(string(value), 32<<10)
	for count > 0 {
		n := min(count, len(chunk))
		if _, err := io.WriteString(w, chunk[:n]); err != nil {
			return
		}
		count -= n
	}
}

func newSSEServer(line string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, line+"\n\n")
		w.(http.Flusher).Flush()
	}))
}

func openAISSELine(size int) string {
	const prefix = `data: {"id":"chatcmpl-test","model":"test","choices":[{"index":0,"delta":{"content":"`
	const suffix = `"},"finish_reason":null}]}`
	if size < len(prefix)+len(suffix) {
		panic("SSE fixture size too small")
	}
	return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
}

func tail(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}

func proxyResponseEvents(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	events := make(map[string]map[string]any)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}
		if eventType, _ := event["type"].(string); eventType != "" {
			events[eventType] = event
		}
	}
	return events
}

func assertResponsesFailureContinuesCreatedState(t *testing.T, body string) {
	t.Helper()
	events := proxyResponseEvents(t, body)
	created := events["response.created"]
	failed := events["response.failed"]
	createdResponse, _ := created["response"].(map[string]any)
	failedResponse, _ := failed["response"].(map[string]any)
	if createdResponse["id"] == nil || failedResponse["id"] != createdResponse["id"] {
		t.Fatalf("stream response ids differ: created=%#v failed=%#v; body=%q", createdResponse, failedResponse, body)
	}
	createdSeq, _ := created["sequence_number"].(float64)
	failedSeq, ok := failed["sequence_number"].(float64)
	if !ok || failedSeq <= createdSeq {
		t.Fatalf("sequence numbers = %v -> %v, want increasing; body=%q", created["sequence_number"], failed["sequence_number"], body)
	}
}

type fixedErrorReader struct{ err error }

func (r fixedErrorReader) Read([]byte) (int, error) { return 0, r.err }

type trackingStreamWriter struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	beforeWrite func()
	writeErr    error
}

type deadlineBlockingWriter struct {
	header      http.Header
	mu          sync.Mutex
	deadline    time.Time
	releaseOnce sync.Once
	releaseCh   chan struct{}
}

func newDeadlineBlockingWriter() *deadlineBlockingWriter {
	return &deadlineBlockingWriter{header: make(http.Header), releaseCh: make(chan struct{})}
}

func (w *deadlineBlockingWriter) Header() http.Header { return w.header }
func (w *deadlineBlockingWriter) WriteHeader(int)     {}
func (w *deadlineBlockingWriter) Flush()              {}

func (w *deadlineBlockingWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	return nil
}

func (w *deadlineBlockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	deadline := w.deadline
	w.mu.Unlock()
	if deadline.IsZero() {
		<-w.releaseCh
		return len(p), nil
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-w.releaseCh:
		return len(p), nil
	case <-timer.C:
		return 0, context.DeadlineExceeded
	}
}

func (w *deadlineBlockingWriter) release() {
	w.releaseOnce.Do(func() { close(w.releaseCh) })
}

func newTrackingStreamWriter() *trackingStreamWriter {
	return &trackingStreamWriter{header: make(http.Header)}
}

func (w *trackingStreamWriter) Header() http.Header { return w.header }

func (w *trackingStreamWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *trackingStreamWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.beforeWrite != nil {
		beforeWrite := w.beforeWrite
		w.beforeWrite = nil
		beforeWrite()
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(p)
}

func (w *trackingStreamWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
