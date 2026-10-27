package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
)

// inStreamRateLimitServer 复现 r00059：上游先回 200 与开场事件，紧接着在流里报限流。
func inStreamRateLimitServer(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"upstream\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"rate limited by upstream\"}}\n\n")
	}))
}

// healthyAnthropicStreamServer 返回一条完整的 anthropic 流，正文含 transferred。
func healthyAnthropicStreamServer(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"type":"message_start","message":{"id":"msg_2","model":"claude-opus-5","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"transferred"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`data: {"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

// 上游 200 之后在流里报限流：预读阶段识别、放弃该候选并转到下一个。
func TestFailoverTransfersOnInStreamRateLimit(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	first := inStreamRateLimitServer(&firstHits)
	defer first.Close()
	second := healthyAnthropicStreamServer(&secondHits)
	defer second.Close()

	providers := map[string]*config.Provider{
		"limited": {Name: "limited", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"healthy": {Name: "healthy", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "limited", Model: "claude-opus-5"},
		{Provider: "healthy", Model: "claude-opus-5"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages",
		`{"model":"client-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望转移后 200", recorder.Code, recorder.Body.String())
	}
	if firstHits.Load() != 1 || secondHits.Load() != 1 {
		t.Fatalf("命中次数 first=%d second=%d, 期望各 1 次", firstHits.Load(), secondHits.Load())
	}
	if !strings.Contains(recorder.Body.String(), "transferred") {
		t.Fatalf("body = %q, 期望第二个候选的正文", recorder.Body.String())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "healthy" {
		t.Errorf("x-ai-gateway-provider = %q, 期望 healthy", got)
	}
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "2" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望 2", got)
	}

	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条, 期望 1 条", len(logs))
	}
	entry := logs[0]
	if entry.Attempts != 2 {
		t.Errorf("Attempts = %d, 期望 2", entry.Attempts)
	}
	if !strings.Contains(entry.AttemptTrail, "limited:429/ratelimit") {
		t.Errorf("AttemptTrail = %q, 期望含 limited:429/ratelimit（流内限流按 429 分类）", entry.AttemptTrail)
	}
	if !strings.Contains(entry.AttemptTrail, "healthy:200") {
		t.Errorf("AttemptTrail = %q, 期望含 healthy:200", entry.AttemptTrail)
	}
	if entry.Provider != "healthy" || entry.Status != http.StatusOK {
		t.Errorf("Provider/Status = %q/%d, 期望 healthy/200", entry.Provider, entry.Status)
	}
}

// 最后一个候选（不允许转移）在流内报限流：写真实 HTTP 429，而不是 200 + SSE 失败事件。
func TestInStreamRateLimitOnLastCandidateReturnsHTTP429(t *testing.T) {
	var hits atomic.Int32
	upstream := inStreamRateLimitServer(&hits)
	defer upstream.Close()

	providers := map[string]*config.Provider{
		"limited": {Name: "limited", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{{Provider: "limited", Model: "claude-opus-5"}}}
	srv := newFailoverTestServer(providers, route, upstream.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages",
		`{"model":"client-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status/body = %d/%s, 期望 429", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event:") || !strings.Contains(body, "rate limited by upstream") {
		t.Fatalf("body = %q, want JSON 错误带上游原文", body)
	}
	if hits.Load() != 1 {
		t.Fatalf("命中次数 = %d, 期望 1", hits.Load())
	}

	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条, 期望 1 条", len(logs))
	}
	entry := logs[0]
	if entry.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, 期望 429", entry.Status)
	}
	if !strings.Contains(entry.Error, "rate limited by upstream") {
		t.Errorf("Error = %q, 期望带上游原文", entry.Error)
	}
	// 单候选时顶层 AttemptTrail 不落（只在多候选 / 跳过场景拼），
	// 失败性质从结构化的 attempt detail 查：上游真实状态码保持 200，
	// 失败语义由 reason 表达。
	if len(entry.AttemptDetails) != 1 {
		t.Fatalf("AttemptDetails = %+v, 期望 1 条", entry.AttemptDetails)
	}
	detail := entry.AttemptDetails[0]
	if detail.UpstreamStatus != http.StatusOK {
		t.Errorf("UpstreamStatus = %d, 期望 200（真实上游状态码）", detail.UpstreamStatus)
	}
	if detail.Reason != "429/ratelimit" || detail.Outcome != "final_error" {
		t.Errorf("Reason/Outcome = %q/%q, 期望 429/ratelimit 与 final_error", detail.Reason, detail.Outcome)
	}
	if !strings.Contains(detail.Error, "rate limited by upstream") {
		t.Errorf("detail.Error = %q, 期望带上游原文", detail.Error)
	}
}
