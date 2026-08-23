package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/providerhealth"
	"ai-gateway/internal/queue"
)

const (
	anthropicOKBody = `{"id":"msg_test","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	openaiOKBody    = `{"id":"chatcmpl-test","model":"upstream","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
)

// newFailoverTestServer 组装一台开启 failover 的直通模式网关，候选顺序即 targets 顺序。
func newFailoverTestServer(providers map[string]*config.Provider, route config.Route, client *http.Client, failover config.Failover) *server {
	return &server{
		cfg: &config.Config{
			Host: "127.0.0.1", Port: 7789, Timeout: 1000, StreamActivityTimeout: 1000,
			DirectMode: true, DirectTimeoutNoStream: 1000, DirectTimeoutStreamHeader: 1000, DirectTimeoutStreamActive: 1000,
			Providers: providers,
			Routes:    []config.Route{route},
			Failover:  failover,
		},
		qm: queue.NewManager(), httpClient: client, metrics: metrics.NewCollector(10),
		providerHealth: providerhealth.NewChecker(),
	}
}

func defaultTestFailover() config.Failover {
	on := true
	off := false
	return config.Failover{
		Enabled: true, MaxAttempts: 2, MaxRetryAfterMs: 5000,
		OnTransportError: &on, OnServerError: &on, OnRateLimit: &on,
		OnQueueTimeout: &on, OnStreamHeaderTimeout: &on, OnAuthError: &off,
	}
}

func postInference(srv *server, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	srv.handle(recorder, request)
	return recorder
}

// TestFailoverTransfersToSecondCandidateOn502 是本阶段的核心断言：
// 首个候选 502，客户端仍拿到 200，且 trail 记录了两次尝试。
func TestFailoverTransfersToSecondCandidateOn502(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"upstream down"}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "bad", Model: "bad-model"},
		{Provider: "good", Model: "good-model"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望转移后 200", recorder.Code, recorder.Body.String())
	}
	if firstHits.Load() != 1 || secondHits.Load() != 1 {
		t.Fatalf("命中次数 first=%d second=%d, 期望各 1 次", firstHits.Load(), secondHits.Load())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "good" {
		t.Errorf("x-ai-gateway-provider = %q, 期望 good", got)
	}
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "2" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望 2", got)
	}

	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条, 期望 1 条（一个 HTTP 请求一条）", len(logs))
	}
	entry := logs[0]
	if entry.Attempts != 2 {
		t.Errorf("Attempts = %d, 期望 2", entry.Attempts)
	}
	if !strings.Contains(entry.AttemptTrail, "bad:502/server") {
		t.Errorf("AttemptTrail = %q, 期望含 bad:502/server", entry.AttemptTrail)
	}
	if !strings.Contains(entry.AttemptTrail, "good:200") {
		t.Errorf("AttemptTrail = %q, 期望含 good:200", entry.AttemptTrail)
	}
	if entry.Provider != "good" {
		t.Errorf("Provider = %q, 期望记最终成功的 good", entry.Provider)
	}
}

// TestFailoverStopsAfterMaxAttempts 候选比 maxAttempts 多时，只试 maxAttempts 次并透传最后一个错误。
func TestFailoverStopsAfterMaxAttempts(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}))
	defer down.Close()

	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: down.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"b": {Name: "b", BaseURL: down.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"c": {Name: "c", BaseURL: down.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}, {Provider: "c", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, down.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, 期望透传最后一次的 503", recorder.Code)
	}
	if hits.Load() != 2 {
		t.Fatalf("上游命中 %d 次, maxAttempts=2 期望 2 次", hits.Load())
	}
}

// TestFailoverDisabledKeepsSingleAttempt 关闭开关后必须回到「失败即终态」。
func TestFailoverDisabledKeepsSingleAttempt(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "bad", Model: "m"}, {Provider: "good", Model: "m"},
	}}
	failover := defaultTestFailover()
	failover.Enabled = false
	srv := newFailoverTestServer(providers, route, first.Client(), failover)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, 期望 502 终态", recorder.Code)
	}
	if firstHits.Load() != 1 || secondHits.Load() != 0 {
		t.Fatalf("命中次数 first=%d second=%d, 期望 1/0", firstHits.Load(), secondHits.Load())
	}
}

// TestFailoverSkipsTransferOnPlain4xx 400 是请求本身的问题，换上游无意义。
func TestFailoverSkipsTransferOnPlain4xx(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "bad", Model: "m"}, {Provider: "good", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400 直接透传", recorder.Code)
	}
	if secondHits.Load() != 0 {
		t.Fatalf("second 命中 %d 次, 普通 4xx 不应转移", secondHits.Load())
	}
}

// TestFailoverAuthErrorRespectsSwitch 401 默认不转移，显式打开后才转移。
func TestFailoverAuthErrorRespectsSwitch(t *testing.T) {
	tests := []struct {
		name           string
		onAuthError    bool
		wantStatus     int
		wantSecondHits int32
	}{
		{name: "默认关闭时 401 终态", onAuthError: false, wantStatus: http.StatusUnauthorized, wantSecondHits: 0},
		{name: "显式打开后 401 转移", onAuthError: true, wantStatus: http.StatusOK, wantSecondHits: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var secondHits atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicOKBody)
			}))
			defer second.Close()

			providers := map[string]*config.Provider{
				"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
				"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
			}
			route := config.Route{Match: "*", Targets: []config.Target{
				{Provider: "bad", Model: "m"}, {Provider: "good", Model: "m"},
			}}
			failover := defaultTestFailover()
			failover.OnAuthError = &tt.onAuthError
			srv := newFailoverTestServer(providers, route, first.Client(), failover)

			recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status/body = %d/%s, 期望 %d", recorder.Code, recorder.Body.String(), tt.wantStatus)
			}
			if secondHits.Load() != tt.wantSecondHits {
				t.Fatalf("second 命中 %d 次, 期望 %d", secondHits.Load(), tt.wantSecondHits)
			}
		})
	}
}

// TestFailoverRateLimitSkipsCandidateOverRetryAfterCap
// 429 的 Retry-After 超过 maxRetryAfterMs 时跳过该候选，不做无谓等待。
func TestFailoverRateLimitTransfersOnRetryAfter(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"limited": {Name: "limited", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"spare":   {Name: "spare", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "limited", Model: "m"}, {Provider: "spare", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 429 转移后 200", recorder.Code, recorder.Body.String())
	}
	if secondHits.Load() != 1 {
		t.Fatalf("second 命中 %d 次, 期望 1", secondHits.Load())
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 || !strings.Contains(logs[0].AttemptTrail, "limited:429/ratelimit") {
		t.Fatalf("AttemptTrail 未记录 429 分类: %#v", logs)
	}
}

// TestFailoverTransportErrorTransfers 连不上的候选应转移到下一个。
func TestFailoverTransportErrorTransfers(t *testing.T) {
	var secondHits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		// 127.0.0.1:1 上没有监听者，必然连接失败
		"dead": {Name: "dead", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"live": {Name: "live", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "dead", Model: "m"}, {Provider: "live", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, second.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望传输错误转移后 200", recorder.Code, recorder.Body.String())
	}
	if secondHits.Load() != 1 {
		t.Fatalf("live 命中 %d 次, 期望 1", secondHits.Load())
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 || !strings.Contains(logs[0].AttemptTrail, "dead:transport_error") {
		t.Fatalf("AttemptTrail 未记录传输错误: %#v", logs)
	}
}

// TestFailoverNoRetryAfterStreamStarted 是硬边界断言：
// 流式已写出字节后即便后续失败也不得换上游重放。
func TestFailoverNoRetryAfterStreamStarted(t *testing.T) {
	var secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"upstream\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 中途断开：客户端已收到字节，网关只能补收尾事件，不能重放到第二个候选
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"cut":   {Name: "cut", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"spare": {Name: "spare", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "cut", Model: "m"}, {Provider: "spare", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if secondHits.Load() != 0 {
		t.Fatalf("second 命中 %d 次, 已开写的流式不得转移", secondHits.Load())
	}
	if !strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("body = %q, 期望保留已写出的 SSE 字节", recorder.Body.String())
	}
}

// TestFailoverPassthroughSecondAttemptSeesCleanBody
// passthrough 分支每次尝试都要浅拷贝 body，否则第二次会读到被首次改写的 model。
func TestFailoverPassthroughSecondAttemptSeesCleanBody(t *testing.T) {
	captured := make(chan map[string]any, 2)
	record := func(r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			captured <- body
		}
	}
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer second.Close()

	// 客户端 openai-chat + provider openai ⇒ 走 passthrough
	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 500},
		"good": {Name: "good", BaseURL: second.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "bad", Model: "bad-model"},
		{Provider: "good", Model: "good-model"},
	}}
	srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	close(captured)
	var models []string
	for body := range captured {
		model, _ := body["model"].(string)
		models = append(models, model)
	}
	if len(models) != 2 {
		t.Fatalf("上游收到 %d 个请求, 期望 2", len(models))
	}
	// 关键：第二次必须是 good-model，而不是被首次污染的 bad-model
	if models[0] != "bad-model" || models[1] != "good-model" {
		t.Fatalf("上游收到的 model 序列 = %v, 期望 [bad-model good-model]", models)
	}
}

// TestFailoverSkipsUnbuildableCandidate 候选构建失败（目标格式不支持该请求）应跳过而不是直接 400。
func TestFailoverSingleCandidateSetsAttemptHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	providers := map[string]*config.Provider{
		"only": {Name: "only", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Provider: "only", Model: "m"}
	srv := newFailoverTestServer(providers, route, upstream.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "only" {
		t.Errorf("x-ai-gateway-provider = %q, 期望 only", got)
	}
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "1" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望 1", got)
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 {
		t.Fatal("缺少请求日志")
	}
	// 单次成功不产生 trail，避免噪声
	if logs[0].AttemptTrail != "" {
		t.Errorf("AttemptTrail = %q, 单次成功期望为空", logs[0].AttemptTrail)
	}
}
