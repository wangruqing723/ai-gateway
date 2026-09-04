package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/breaker"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/router"
)

func contextWindowPtr(value int) *int {
	return &value
}

func contextMarginZero(cfg *config.Config) {
	cfg.ContextSafetyMargin = contextWindowPtr(0)
}

func TestResolveContextBudget(t *testing.T) {
	window := 12000
	tests := []struct {
		name           string
		window         *int
		estimatedInput int
		margin         int
		wantBudget     int
		wantOK         bool
	}{
		{name: "未配置不限制", wantBudget: 0, wantOK: true},
		{name: "正常预算", window: &window, estimatedInput: 2000, margin: 1000, wantBudget: 9000, wantOK: true},
		{name: "低于下限", window: &window, estimatedInput: 10977, margin: 0, wantBudget: 1023, wantOK: false},
		{name: "输入超窗", window: &window, estimatedInput: 20000, margin: 0, wantBudget: -8000, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBudget, gotOK := resolveContextBudget(tt.window, tt.estimatedInput, tt.margin)
			if gotBudget != tt.wantBudget || gotOK != tt.wantOK {
				t.Fatalf("resolveContextBudget() = (%d, %v)，期望 (%d, %v)", gotBudget, gotOK, tt.wantBudget, tt.wantOK)
			}
		})
	}
}

func TestContextWindowExceededReturns413(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	window := 1028 // "hi" 的估算值为 5，预算为 1023，低于 MinOutputBudget。
	providers := map[string]*config.Provider{
		"small": {Name: "small", BaseURL: upstream.URL, Format: "anthropic", ContextWindow: &window, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, singleCandidateRoute("small", "upstream"), upstream.Client(), defaultTestFailover())
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status/body = %d/%s，期望 413", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("错误响应不是 JSON: %v; body=%s", err, recorder.Body.String())
	}
	if response.Error.Type != "context_window_exceeded" {
		t.Fatalf("error.type = %q，期望 context_window_exceeded", response.Error.Type)
	}
	if !strings.Contains(response.Error.Message, "估算输入") || !strings.Contains(response.Error.Message, "small=1028") {
		t.Errorf("error.message = %q，缺少估算值或窗口信息", response.Error.Message)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("上游命中 = %d，装不下时不应发起请求", upstreamHits.Load())
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 1})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条，期望 1 条", len(logs))
	}
	entry := logs[0]
	if entry.Attempts != 0 || entry.EstimatedInputTokens != 5 {
		t.Errorf("请求日志 = %#v，期望 Attempts=0、EstimatedInputTokens=5", entry)
	}
	if len(entry.AttemptDetails) != 1 || entry.AttemptDetails[0].Kind != "context_skip" || entry.AttemptDetails[0].Reason != "context_window_exceeded" {
		t.Errorf("context skip 明细 = %#v", entry.AttemptDetails)
	}
	if !strings.Contains(entry.AttemptTrail, "small:context_exceeded") {
		t.Errorf("AttemptTrail = %q，期望含 context_exceeded", entry.AttemptTrail)
	}
}

func TestContextSkipFallsThroughToNextCandidate(t *testing.T) {
	var smallHits, largeHits atomic.Int32
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		smallHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer small.Close()
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		largeHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer large.Close()

	smallWindow, largeWindow := 1028, 8005
	providers := map[string]*config.Provider{
		"small": {Name: "small", BaseURL: small.URL, Format: "anthropic", ContextWindow: &smallWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
		"large": {Name: "large", BaseURL: large.URL, Format: "anthropic", ContextWindow: &largeWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "small", Model: "small-model"},
		{Provider: "large", Model: "large-model"},
	}}
	srv := newFailoverTestServer(providers, route, large.Client(), defaultTestFailover())
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s，期望跳过小窗口后 200", recorder.Code, recorder.Body.String())
	}
	if smallHits.Load() != 0 || largeHits.Load() != 1 {
		t.Fatalf("命中次数 small=%d large=%d，期望 0/1", smallHits.Load(), largeHits.Load())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "large" {
		t.Errorf("provider = %q，期望 large", got)
	}
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "1" {
		t.Errorf("attempts = %q，期望 1（context skip 不消耗额度）", got)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 {
		t.Fatalf("请求日志 = %#v，期望一次真实尝试和两条明细", entry)
	}
	if entry.AttemptDetails[0].Kind != "context_skip" || entry.AttemptDetails[1].Outcome != "success" {
		t.Errorf("尝试明细 = %#v", entry.AttemptDetails)
	}
}

func TestContextSkipDoesNotConsumeMaxAttempts(t *testing.T) {
	var firstHits, secondHits, thirdHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer second.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		thirdHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer third.Close()

	smallWindow, largeWindow := 1028, 8005
	providers := map[string]*config.Provider{
		"first":  {Name: "first", BaseURL: first.URL, Format: "anthropic", ContextWindow: &smallWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
		"second": {Name: "second", BaseURL: second.URL, Format: "anthropic", ContextWindow: &smallWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
		"third":  {Name: "third", BaseURL: third.URL, Format: "anthropic", ContextWindow: &largeWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "first", Model: "first-model"},
		{Provider: "second", Model: "second-model"},
		{Provider: "third", Model: "third-model"},
	}}
	failover := defaultTestFailover()
	attemptLimit := 1
	failover.MaxAttempts = &attemptLimit
	srv := newFailoverTestServer(providers, route, third.Client(), failover)
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s，期望第三候选成功", recorder.Code, recorder.Body.String())
	}
	if firstHits.Load() != 0 || secondHits.Load() != 0 || thirdHits.Load() != 1 {
		t.Fatalf("命中次数 first=%d second=%d third=%d，期望 0/0/1", firstHits.Load(), secondHits.Load(), thirdHits.Load())
	}
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "1" {
		t.Errorf("attempts = %q，期望 1", got)
	}
}

func TestContextWindowTakesPriorityOverBreakerTerminalState(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	tooSmall := 1028
	providers := map[string]*config.Provider{
		"open":      {Name: "open", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"too-small": {Name: "too-small", BaseURL: upstream.URL, Format: "anthropic", ContextWindow: &tooSmall, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "open", Model: "open-model"},
		{Provider: "too-small", Model: "small-model"},
	}}
	srv := newBreakerTestServer(providers, route, upstream.Client(), defaultTestFailover(), testBreakerSettings())
	contextMarginZero(srv.cfg)
	for i := 0; i < 3; i++ {
		srv.breaker.Report("open", breaker.OutcomeFailure)
	}

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status/body = %d/%s，熔断优先时应返回 503", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("错误响应不是 JSON: %v", err)
	}
	if response.Error.Type != "breaker_open" {
		t.Errorf("error.type = %q，期望 breaker_open", response.Error.Type)
	}
	if hits.Load() != 0 {
		t.Errorf("上游命中 = %d，两个候选都应跳过", hits.Load())
	}
}

func TestContextWindowClampsOutputAcrossFormats(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		providerFormat string
		field          string
		requestBody    string
		responseBody   string
	}{
		{
			name:           "anthropic",
			path:           "/v1/messages",
			providerFormat: "anthropic",
			field:          "max_tokens",
			requestBody:    `{"model":"client-model","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`,
			responseBody:   anthropicOKBody,
		},
		{
			name:           "openai chat completion 字段",
			path:           "/v1/chat/completions",
			providerFormat: "openai",
			field:          "max_completion_tokens",
			requestBody:    `{"model":"client-model","max_completion_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`,
			responseBody:   openaiOKBody,
		},
		{
			name:           "openai responses",
			path:           "/v1/responses",
			providerFormat: "openai-responses",
			field:          "max_output_tokens",
			requestBody:    `{"model":"client-model","max_output_tokens":64000,"input":"hi"}`,
			responseBody:   responsesOKBody,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := make(chan struct {
				body map[string]any
				err  error
			}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				err := json.NewDecoder(r.Body).Decode(&body)
				captured <- struct {
					body map[string]any
					err  error
				}{body: body, err: err}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.responseBody)
			}))
			defer upstream.Close()

			window := 8005
			providers := map[string]*config.Provider{
				"provider": {Name: "provider", BaseURL: upstream.URL, Format: tt.providerFormat, ContextWindow: &window, MaxConcurrent: 1, MaxQueueWait: 1000},
			}
			srv := newFailoverTestServer(providers, singleCandidateRoute("provider", "upstream-model"), upstream.Client(), defaultTestFailover())
			contextMarginZero(srv.cfg)

			recorder := postInference(srv, tt.path, tt.requestBody)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status/body = %d/%s，期望 200", recorder.Code, recorder.Body.String())
			}
			capture := <-captured
			if capture.err != nil {
				t.Fatalf("解析上游请求失败: %v", capture.err)
			}
			received := capture.body
			got, ok := received[tt.field].(float64)
			if !ok || int(got) != 8000 {
				t.Fatalf("上游 %s = %#v，期望 budget 8000", tt.field, received[tt.field])
			}
			if tt.field == "max_completion_tokens" {
				if _, exists := received["max_tokens"]; exists {
					t.Errorf("OpenAI Chat 同时出现 max_tokens，完整请求体 = %#v", received)
				}
			}
		})
	}
}

func TestConfiguredMaxTokensIsAlsoClampedByContextBudget(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]any
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("解析上游请求失败: %v", err)
		}
		captured <- received
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	window, configuredMax := 8005, 32768
	providers := map[string]*config.Provider{
		"provider": {Name: "provider", BaseURL: upstream.URL, Format: "anthropic", ContextWindow: &window, MaxTokens: &configuredMax, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, singleCandidateRoute("provider", "upstream-model"), upstream.Client(), defaultTestFailover())
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s，期望 200", recorder.Code, recorder.Body.String())
	}
	received := <-captured
	if got, ok := received["max_tokens"].(float64); !ok || int(got) != 8000 {
		t.Fatalf("配置 maxTokens 被下发为 %#v，期望被预算压到 8000", received["max_tokens"])
	}
}

func TestContextBudgetDoesNotRaiseDefaultOnPassthrough(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]any
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("解析上游请求失败: %v", err)
		}
		captured <- received
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	window := 65536
	providers := map[string]*config.Provider{
		"provider": {Name: "provider", BaseURL: upstream.URL, Format: "anthropic", ContextWindow: &window, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, singleCandidateRoute("provider", "upstream-model"), upstream.Client(), defaultTestFailover())
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s，期望 200", recorder.Code, recorder.Body.String())
	}
	received := <-captured
	if got, ok := received["max_tokens"].(float64); !ok || int(got) != 32768 {
		t.Fatalf("预算不应抬高透传默认 max_tokens: %#v，期望 32768", received["max_tokens"])
	}
}

func TestUnconfiguredContextWindowKeepsRequestAndOmitsEstimate(t *testing.T) {
	captured := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析上游请求失败: %v", err)
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	providers := map[string]*config.Provider{
		"provider": {Name: "provider", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, singleCandidateRoute("provider", "upstream-model"), upstream.Client(), defaultTestFailover())

	for _, body := range []string{
		`{"model":"client-model","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`,
	} {
		recorder := postInference(srv, "/v1/messages", body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status/body = %d/%s，期望 200", recorder.Code, recorder.Body.String())
		}
	}
	received := []map[string]any{<-captured, <-captured}
	if got := received[0]["max_tokens"].(float64); got != 64000 {
		t.Errorf("客户端已传 max_tokens 被改成 %v，期望 64000", got)
	}
	if got := received[1]["max_tokens"].(float64); got != 32768 {
		t.Errorf("透传缺失 max_tokens = %v，期望旧默认 32768", got)
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 2 {
		t.Fatalf("请求日志 = %d 条，期望 2 条", len(logs))
	}
	for _, entry := range logs {
		if entry.EstimatedInputTokens != 0 {
			t.Errorf("未配置窗口却记录 EstimatedInputTokens = %d", entry.EstimatedInputTokens)
		}
	}
}

func TestOneMSuffixMatchesRouteStripsUpstreamModelAndUsesMillionWindow(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]any
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("解析上游请求失败: %v", err)
		}
		captured <- received
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	configuredWindow := 200000
	providers := map[string]*config.Provider{
		"provider": {Name: "provider", BaseURL: upstream.URL, Format: "anthropic", ContextWindow: &configuredWindow, MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "model", Provider: "provider", Model: ""}
	srv := newFailoverTestServer(providers, route, upstream.Client(), defaultTestFailover())
	contextMarginZero(srv.cfg)

	recorder := postInference(srv, "/v1/messages", `{"model":"model [1M]","max_tokens":2000000,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s，期望精确路由命中后 200", recorder.Code, recorder.Body.String())
	}
	received := <-captured
	if got := received["model"].(string); got != "model" {
		t.Errorf("上游 model = %q，期望剥离 [1M]", got)
	}
	if got, ok := received["max_tokens"].(float64); !ok || int(got) != oneMillionMinusEstimateForTest() {
		t.Fatalf("上游 max_tokens = %#v，期望按 1000000 窗口计算", received["max_tokens"])
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.EstimatedInputTokens != 5 {
		t.Errorf("EstimatedInputTokens = %d，期望 5", entry.EstimatedInputTokens)
	}
}

func oneMillionMinusEstimateForTest() int {
	return router.OneMContextWindow - 5
}
