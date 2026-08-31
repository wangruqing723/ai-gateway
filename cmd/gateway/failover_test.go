package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/providerhealth"
	"ai-gateway/internal/queue"
)

const (
	anthropicOKBody = `{"id":"msg_test","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	openaiOKBody    = `{"id":"chatcmpl-test","model":"upstream","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	responsesOKBody = `{"id":"resp_test","object":"response","status":"completed","model":"upstream","output":[{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
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
		qm: queue.NewManager(), resolveHTTPClient: testClientResolver(client), metrics: metrics.NewCollector(10),
		providerHealth: providerhealth.NewChecker(),
	}
}

func defaultTestFailover() config.Failover {
	on := true
	off := false
	attempts := 2
	retryAfterCap := 5000
	return config.Failover{
		Enabled: true, MaxAttempts: &attempts, MaxRetryAfterMs: &retryAfterCap,
		OnTransportError: &on, OnServerError: &on, OnRateLimit: &on,
		OnQueueTimeout: &on, OnStreamHeaderTimeout: &on, OnAuthError: &off,
		// 非流式整体超时默认不转移，与 config.applyDefaults 一致
		OnRequestTimeout: &off,
	}
}

func postInference(srv *server, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789"+path, strings.NewReader(body))
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
	if len(entry.AttemptDetails) != 2 {
		t.Fatalf("AttemptDetails = %#v, 期望两条", entry.AttemptDetails)
	}
	failed, succeeded := entry.AttemptDetails[0], entry.AttemptDetails[1]
	if failed.AttemptNumber != 1 || failed.Kind != "request" || failed.Outcome != "transferred" || failed.Reason != "502/server" || failed.UpstreamStatus != 502 {
		t.Errorf("失败 detail = %#v", failed)
	}
	if succeeded.AttemptNumber != 2 || succeeded.Outcome != "success" || succeeded.UpstreamStatus != 200 || succeeded.Provider != "good" {
		t.Errorf("成功 detail = %#v", succeeded)
	}
}

func TestFailoverAttemptDetailCapturesErrorBodyAndRequestID(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "upstream-500")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"bad","api_key":"must-not-leak"}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()
	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, config.Route{Match: "*", Targets: []config.Target{{Provider: "bad", Model: "m"}, {Provider: "good", Model: "m"}}}, first.Client(), defaultTestFailover())
	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	detail := entry.AttemptDetails[0]
	if detail.UpstreamRequestID != "upstream-500" || detail.ErrorBody == "" || !strings.Contains(detail.ErrorBody, "[REDACTED]") || strings.Contains(detail.ErrorBody, "must-not-leak") {
		t.Fatalf("error observability detail = %#v", detail)
	}
}

func TestFailoverAttemptDetailErrorBodyKeepsUTF8Boundary(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// 第 8192 字节落在“你”的中间，覆盖观测片段按字节截断的边界。
		_, _ = io.WriteString(w, strings.Repeat("x", 8191)+"你后续错误正文")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer second.Close()

	providers := map[string]*config.Provider{
		"bad":  {Name: "bad", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"good": {Name: "good", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, config.Route{Match: "*", Targets: []config.Target{{Provider: "bad", Model: "m"}, {Provider: "good", Model: "m"}}}, first.Client(), defaultTestFailover())
	if recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	detail := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0].AttemptDetails[0]
	if !utf8.ValidString(detail.ErrorBody) {
		t.Fatalf("错误正文不是合法 UTF-8：%q", detail.ErrorBody)
	}
}

func TestFailoverCanTransferBetweenChatAndResponsesProviders(t *testing.T) {
	var chatPath, responsesPath string
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatPath = r.URL.Path
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"chat unavailable"}`)
	}))
	defer chat.Close()
	responses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responsesPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesOKBody)
	}))
	defer responses.Close()
	providers := map[string]*config.Provider{
		"chat":      {Name: "chat", BaseURL: chat.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
		"responses": {Name: "responses", BaseURL: responses.URL, Format: "openai-responses", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, config.Route{Match: "*", Targets: []config.Target{{Provider: "chat", Model: "gpt-chat"}, {Provider: "responses", Model: "gpt-responses"}}}, chat.Client(), defaultTestFailover())
	recorder := postInference(srv, "/v1/responses", `{"model":"client-model","input":"hi"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	if chatPath != "/v1/chat/completions" || responsesPath != "/v1/responses" {
		t.Fatalf("upstream paths chat/responses = %q/%q", chatPath, responsesPath)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if len(entry.AttemptDetails) != 2 || entry.AttemptDetails[0].ProviderFormat != "openai" || entry.AttemptDetails[1].ProviderFormat != "openai-responses" {
		t.Fatalf("attempt details = %#v", entry.AttemptDetails)
	}
	// 顶层 UpstreamFormat 必须跟着实际成功的候选走，而不是停在首候选的 openai，
	// 否则详情面板的 Provider 与「上游格式」会来自不同候选。
	if entry.Provider != "responses" || entry.UpstreamFormat != "openai-responses" {
		t.Fatalf("顶层归属 = %q/%q，期望 responses/openai-responses", entry.Provider, entry.UpstreamFormat)
	}
}

func TestRequestLogFormatKeepsClientFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()
	providers := map[string]*config.Provider{
		"openai": {Name: "openai", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, config.Route{Match: "*", Provider: "openai", Model: "gpt-upstream"}, upstream.Client(), defaultTestFailover())
	if recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Format != "anthropic" || entry.ClientFormat != "anthropic" {
		t.Fatalf("日志格式 = %q/%q，期望客户端格式 anthropic", entry.Format, entry.ClientFormat)
	}
	// UpstreamFormat 必须是 provider 的格式，不能跟着 Format 一起变成客户端格式：
	// 前端详情面板的「上游格式」读它，曾误读 Format 导致 openai 上游显示成 anthropic。
	if entry.UpstreamFormat != "openai" {
		t.Fatalf("上游格式 = %q，期望 provider 格式 openai", entry.UpstreamFormat)
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

// TestFailoverRateLimitTransfersOnRetryAfter 429 且 Retry-After 在 maxRetryAfterMs 之内：
// 正常转移，并且**消耗**一次尝试额度。
//
// 这里的 Retry-After: 1（1 秒，低于默认 5000ms 上限）就是「阈值之内」那一档；
// 超阈值的不计额度路径由 TestFailoverRateLimitOverCapDoesNotConsumeAttempt 覆盖。
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
	// 阈值之内：这次 429 要计入额度，attempts 为 2
	if logs[0].Attempts != 2 {
		t.Errorf("Attempts = %d, 期望 2（阈值内的 429 消耗额度）", logs[0].Attempts)
	}
}

// TestFailoverRateLimitOverCapDoesNotConsumeAttempt 429 的 Retry-After 超过
// maxRetryAfterMs 时，该候选不消耗尝试额度。
//
// 回归点：maxRetryAfterMs 原先完全没有运行时读取点，配了等于没配。
// 语义与熔断跳过一致 —— 上游明确说了这段时间不可用，那这次失败不该占掉
// maxAttempts 里的一格，否则一个自曝限流的上游会挤掉本来还能试的健康候选。
func TestFailoverRateLimitOverCapDoesNotConsumeAttempt(t *testing.T) {
	var secondHits, thirdHits atomic.Int32
	// Retry-After: 45 秒，远超默认 5000ms 上限
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer limited.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"bad gateway"}`)
	}))
	defer broken.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		thirdHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer healthy.Close()

	providers := map[string]*config.Provider{
		"limited": {Name: "limited", BaseURL: limited.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"broken":  {Name: "broken", BaseURL: broken.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"healthy": {Name: "healthy", BaseURL: healthy.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "limited", Model: "m"}, {Provider: "broken", Model: "m"}, {Provider: "healthy", Model: "m"},
	}}
	// maxAttempts = 2：若 limited 消耗额度，broken 失败后就没机会试 healthy
	failover := defaultTestFailover()
	attempts := 2
	failover.MaxAttempts = &attempts
	srv := newFailoverTestServer(providers, route, limited.Client(), failover)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 200（limited 不占额度，broken 之后还能试 healthy）", recorder.Code, recorder.Body.String())
	}
	if secondHits.Load() != 1 || thirdHits.Load() != 1 {
		t.Fatalf("broken 命中 %d 次 / healthy 命中 %d 次, 期望各 1 次", secondHits.Load(), thirdHits.Load())
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 {
		t.Fatal("没有请求日志")
	}
	if !strings.Contains(logs[0].AttemptTrail, "limited:429/retry_after_too_long") {
		t.Errorf("AttemptTrail 未记录超阈值分类: %q", logs[0].AttemptTrail)
	}
	// limited 不计入，broken 与 healthy 各算一次
	if logs[0].Attempts != 2 {
		t.Errorf("Attempts = %d, 期望 2（limited 不消耗额度）", logs[0].Attempts)
	}
	if logs[0].Provider != "healthy" {
		t.Errorf("Provider = %q, 期望 healthy", logs[0].Provider)
	}
}

// TestFailoverRateLimitCapZeroMeansNoCap maxRetryAfterMs 显式写 0 表示不设上限，
// 此时所有 429 照常消耗额度。
//
// 回归点：applyDefaults 原先用 `== 0` 判「未设置」，会把用户显式写的 0 改成 5000。
func TestFailoverRateLimitCapZeroMeansNoCap(t *testing.T) {
	var spareHits atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer limited.Close()
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		spareHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer spare.Close()

	providers := map[string]*config.Provider{
		"limited": {Name: "limited", BaseURL: limited.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"spare":   {Name: "spare", BaseURL: spare.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "limited", Model: "m"}, {Provider: "spare", Model: "m"},
	}}
	failover := defaultTestFailover()
	noCap := 0
	failover.MaxRetryAfterMs = &noCap
	srv := newFailoverTestServer(providers, route, limited.Client(), failover)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 200", recorder.Code, recorder.Body.String())
	}
	if spareHits.Load() != 1 {
		t.Fatalf("spare 命中 %d 次, 期望 1", spareHits.Load())
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 {
		t.Fatal("没有请求日志")
	}
	// 不设上限：45 秒的 Retry-After 也走普通 429 分类并消耗额度
	if !strings.Contains(logs[0].AttemptTrail, "limited:429/ratelimit") {
		t.Errorf("AttemptTrail = %q, 期望普通 429 分类", logs[0].AttemptTrail)
	}
	if logs[0].Attempts != 2 {
		t.Errorf("Attempts = %d, 期望 2", logs[0].Attempts)
	}
}

// TestFailoverAllCandidatesRateLimitedReturns429 全部候选都自报超阈值限流时，
// 客户端必须收到 429 而不是被误报成协议错误 400。
//
// 实际走的是「最后一个候选原样透传」这条路径：最后一个候选 allowRetry=false，
// ShouldRetry 不会触发，proxy 直接把上游的 429 连同 Retry-After 写给客户端 ——
// 比网关自己合成一个终态更准确。main.go 里的 all_candidates_rate_limited 分支
// 只是防御性兜底。
func TestFailoverAllCandidatesRateLimitedReturns429(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}
	a := httptest.NewServer(http.HandlerFunc(handler))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(handler))
	defer b.Close()

	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: a.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"b": {Name: "b", BaseURL: b.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"},
	}}
	srv := newFailoverTestServer(providers, route, a.Client(), defaultTestFailover())

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status/body = %d/%s, 期望 429", recorder.Code, recorder.Body.String())
	}
	// 注：proxy.handleError 只回传状态码与响应体，不复制上游响应头，
	// 因此上游的 Retry-After 目前不会到客户端手上。这是既有行为，与本次改动无关。
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) == 0 {
		t.Fatal("没有请求日志")
	}
	// 第一个候选不计额度，第二个候选真实尝试一次
	if logs[0].Attempts != 1 {
		t.Errorf("Attempts = %d, 期望 1", logs[0].Attempts)
	}
	if !strings.Contains(logs[0].AttemptTrail, "a:429/retry_after_too_long") {
		t.Errorf("AttemptTrail 未记录首个候选的超阈值跳过: %q", logs[0].AttemptTrail)
	}
}

// TestFailoverRequestTimeoutHonorsOwnSwitch 非流式整体超时由独立的 onRequestTimeout
// 控制，默认不转移。
//
// 回归点：原先它走 onTransportError（默认 true），与 config.go 里「非流式整体超时
// 不可配置转移：会让总耗时翻倍」的注释直接矛盾，且想关掉就只能连带关掉真正的
// 连接失败转移 —— 后者几乎不耗时，恰恰是 failover 最该覆盖的场景。
func TestFailoverRequestTimeoutHonorsOwnSwitch(t *testing.T) {
	cases := []struct {
		name             string
		onRequestTimeout bool
		wantStatus       int
		wantSpareHits    int32
	}{
		{"默认不转移：客户端拿到超时终态", false, http.StatusGatewayTimeout, 0},
		{"显式开启后转移到下一个候选", true, http.StatusOK, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var spareHits atomic.Int32
			release := make(chan struct{})
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 挂住直到测试结束或客户端超时断开，制造非流式整体超时
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer slow.Close()
			defer close(release)
			spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				spareHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicOKBody)
			}))
			defer spare.Close()

			providers := map[string]*config.Provider{
				"slow":  {Name: "slow", BaseURL: slow.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
				"spare": {Name: "spare", BaseURL: spare.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
			}
			route := config.Route{Match: "*", Targets: []config.Target{
				{Provider: "slow", Model: "m"}, {Provider: "spare", Model: "m"},
			}}
			failover := defaultTestFailover()
			failover.OnRequestTimeout = &tc.onRequestTimeout
			srv := newFailoverTestServer(providers, route, slow.Client(), failover)
			// 缩短超时，避免测试等满 1 秒
			srv.cfg.DirectTimeoutNoStream = 150

			recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status/body = %d/%s, 期望 %d", recorder.Code, recorder.Body.String(), tc.wantStatus)
			}
			if spareHits.Load() != tc.wantSpareHits {
				t.Fatalf("spare 命中 %d 次, 期望 %d", spareHits.Load(), tc.wantSpareHits)
			}
		})
	}
}

// TestFailoverTransportErrorStillTransfersWithTimeoutOff 关掉超时转移不影响
// 真正的连接失败转移 —— 这正是两者拆成独立开关的理由。
func TestFailoverTransportErrorStillTransfersWithTimeoutOff(t *testing.T) {
	var spareHits atomic.Int32
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		spareHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer spare.Close()
	// 起一个立刻关掉的服务拿到必然连不上的地址
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	providers := map[string]*config.Provider{
		"dead":  {Name: "dead", BaseURL: deadURL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
		"spare": {Name: "spare", BaseURL: spare.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 500},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "dead", Model: "m"}, {Provider: "spare", Model: "m"},
	}}
	failover := defaultTestFailover()
	off := false
	failover.OnRequestTimeout = &off
	srv := newFailoverTestServer(providers, route, spare.Client(), failover)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 200（连接失败仍应转移）", recorder.Code, recorder.Body.String())
	}
	if spareHits.Load() != 1 {
		t.Fatalf("spare 命中 %d 次, 期望 1", spareHits.Load())
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
