package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/router"
)

type globalExtrasCapture struct {
	path   string
	header http.Header
	body   map[string]any
}

func captureGlobalExtrasRequest(t *testing.T, r *http.Request) globalExtrasCapture {
	t.Helper()
	capture := globalExtrasCapture{path: r.URL.Path, header: r.Header.Clone()}
	if err := json.NewDecoder(r.Body).Decode(&capture.body); err != nil {
		t.Errorf("解析上游请求体失败: %v", err)
	}
	return capture
}

func testGlobalUpstreamExtras() *config.GlobalUpstreamExtras {
	return &config.GlobalUpstreamExtras{
		Anthropic: &config.UpstreamExtras{
			Headers: map[string]string{"x-format-default": "anthropic", "x-global-only": "anthropic"},
			Body:    map[string]any{"global_anthropic": true, "collision": "global", "model": "hijacked", "stream": true},
		},
		OpenAI: &config.UpstreamExtras{
			Headers: map[string]string{"x-format-default": "openai", "x-global-only": "openai", "User-Agent": "hijacked-agent"},
			Body:    map[string]any{"global_openai": true, "collision": "global", "model": "hijacked", "stream": true},
		},
		OpenAIResponses: &config.UpstreamExtras{
			Headers: map[string]string{"x-format-default": "openai-responses", "x-global-only": "openai-responses"},
			Body:    map[string]any{"global_openai_responses": true, "collision": "global", "model": "hijacked", "stream": true},
		},
	}
}

func upstreamResponseForFormat(format string) string {
	switch format {
	case "anthropic":
		return anthropicOKBody
	case "openai":
		return openaiOKBody
	default:
		return responsesOKBody
	}
}

func upstreamPathForFormat(format string) string {
	switch format {
	case "anthropic":
		return "/v1/messages"
	case "openai":
		return "/v1/chat/completions"
	default:
		return "/v1/responses"
	}
}

// 每种目标格式都必须只取自己的全局默认项；它们先于已有 Provider → Route → Target
// body 链写入，且不能突破既有的受保护字段边界。
func TestGlobalUpstreamExtrasUseTargetFormatAndStayLowestPriority(t *testing.T) {
	tests := []struct {
		format    string
		globalKey string
		globalUA  string
	}{
		{format: "anthropic", globalKey: "global_anthropic", globalUA: "anthropic-global-agent/1.0"},
		{format: "openai", globalKey: "global_openai", globalUA: "openai-global-agent/1.0"},
		{format: "openai-responses", globalKey: "global_openai_responses", globalUA: "responses-global-agent/1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			var captured globalExtrasCapture
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = captureGlobalExtrasRequest(t, r)
				w.Header().Set("content-type", "application/json")
				_, _ = io.WriteString(w, upstreamResponseForFormat(tt.format))
			}))
			defer upstream.Close()

			provider := &config.Provider{
				Name: "target", BaseURL: upstream.URL, APIKey: "upstream-key", Format: tt.format,
				MaxConcurrent: 1, MaxQueueWait: 1000,
				ExtraHeaders: map[string]string{"x-collision": "provider"},
				ExtraBody:    map[string]any{"provider_only": "provider", "collision": "provider"},
			}
			route := config.Route{
				Match: "*", ExtraBody: map[string]any{"route_only": "route", "collision": "route"},
				Targets: []config.Target{{
					Provider: "target", Model: "upstream-model",
					ExtraBody: map[string]any{"target_only": "target", "collision": "target"},
				}},
			}
			srv := newFailoverTestServer(map[string]*config.Provider{"target": provider}, route, upstream.Client(), defaultTestFailover())
			srv.cfg.GlobalUserAgent = &config.GlobalUserAgents{
				Anthropic:       "anthropic-global-agent/1.0",
				OpenAI:          "openai-global-agent/1.0",
				OpenAIResponses: "responses-global-agent/1.0",
			}
			srv.cfg.GlobalUpstreamExtras = testGlobalUpstreamExtras()

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "client-agent/1.0")
			srv.handle(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
			}

			if captured.path != upstreamPathForFormat(tt.format) {
				t.Errorf("upstream path = %q, want %q", captured.path, upstreamPathForFormat(tt.format))
			}
			if got := captured.header.Get("x-format-default"); got != tt.format {
				t.Errorf("x-format-default = %q, want %q", got, tt.format)
			}
			if got := captured.header.Get("x-global-only"); got != tt.format {
				t.Errorf("x-global-only = %q, want %q", got, tt.format)
			}
			if got := captured.header.Get("x-collision"); got != "provider" {
				t.Errorf("x-collision = %q, want provider override", got)
			}
			if got := captured.header.Get("User-Agent"); got != tt.globalUA {
				t.Errorf("User-Agent = %q, want %q for %s", got, tt.globalUA, tt.format)
			}
			if got := captured.header.Get("Authorization"); got != "Bearer upstream-key" {
				t.Errorf("Authorization = %q, want protected credential", got)
			}

			if got := captured.body[tt.globalKey]; got != true {
				t.Errorf("%s = %#v, want true", tt.globalKey, got)
			}
			if got := captured.body["provider_only"]; got != "provider" {
				t.Errorf("provider_only = %#v", got)
			}
			if got := captured.body["route_only"]; got != "route" {
				t.Errorf("route_only = %#v", got)
			}
			if got := captured.body["target_only"]; got != "target" {
				t.Errorf("target_only = %#v", got)
			}
			if got := captured.body["collision"]; got != "target" {
				t.Errorf("collision = %#v, want target override", got)
			}
			if got := captured.body["model"]; got != "upstream-model" {
				t.Errorf("model = %#v, want managed upstream-model", got)
			}
			if got, ok := captured.body["stream"].(bool); ok && got {
				t.Errorf("stream = true; global body must not override the client/gateway decision")
			}
			for _, key := range []string{"global_anthropic", "global_openai", "global_openai_responses"} {
				if key != tt.globalKey {
					if _, found := captured.body[key]; found {
						t.Errorf("wrong-format global body %q leaked into %s request: %#v", key, tt.format, captured.body)
					}
				}
			}
		})
	}
}

// 故障转移后每个候选都要按自己的实际目标格式重新取全局默认项，不能沿用第一次尝试的值。
func TestGlobalUpstreamExtrasFollowFailoverTargetFormat(t *testing.T) {
	var first, second globalExtrasCapture
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first = captureGlobalExtrasRequest(t, r)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"first failed"}`)
	}))
	defer firstUpstream.Close()
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second = captureGlobalExtrasRequest(t, r)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer secondUpstream.Close()

	providers := map[string]*config.Provider{
		"first":  {Name: "first", BaseURL: firstUpstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
		"second": {Name: "second", BaseURL: secondUpstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{{Provider: "first", Model: "first-model"}, {Provider: "second", Model: "second-model"}}}
	srv := newFailoverTestServer(providers, route, firstUpstream.Client(), defaultTestFailover())
	srv.cfg.GlobalUpstreamExtras = testGlobalUpstreamExtras()

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	if first.header.Get("x-format-default") != "openai" || first.body["global_openai"] != true {
		t.Errorf("first failover request did not receive openai defaults: %#v", first)
	}
	if second.header.Get("x-format-default") != "anthropic" || second.body["global_anthropic"] != true {
		t.Errorf("second failover request did not receive anthropic defaults: %#v", second)
	}
}

// 列表密钥切换会创建新的真实 HTTP 尝试；默认项必须随每一次请求重放，而不是只写第一把密钥。
func TestGlobalUpstreamExtrasAreReappliedForKeySwitch(t *testing.T) {
	var mu sync.Mutex
	var captures []globalExtrasCapture
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := captureGlobalExtrasRequest(t, r)
		mu.Lock()
		captures = append(captures, capture)
		hits++
		hit := hits
		mu.Unlock()
		if hit == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"first key rejected"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()

	provider := &config.Provider{
		Name: "keyed", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000,
		APIKeys: []config.ProviderKey{{ID: "first", Value: "first-key"}, {ID: "second", Value: "second-key"}},
	}
	srv := newKeySwitchTestServer(map[string]*config.Provider{"keyed": provider}, singleCandidateRoute("keyed", "upstream-model"), upstream.Client(), config.Failover{})
	srv.cfg.GlobalUpstreamExtras = testGlobalUpstreamExtras()

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captures) != 2 {
		t.Fatalf("upstream attempts = %d, want 2", len(captures))
	}
	for i, capture := range captures {
		if capture.header.Get("x-format-default") != "openai" || capture.body["global_openai"] != true {
			t.Errorf("attempt %d did not receive openai defaults: %#v", i+1, capture)
		}
	}
}

// OAuth Responses 调用仍要经过同一全局默认层，但 OAuth 对 stream/store 的既有后处理必须保持最终优先。
func TestForwardAttemptAppliesGlobalResponsesDefaultsToOAuth(t *testing.T) {
	var captured globalExtrasCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = captureGlobalExtrasRequest(t, r)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	mgr := newFixedChatGPTManager(t)
	provider := oauthProvider("codex", "acc_ready")
	provider.BaseURL = upstream.URL
	srv := &server{chatgptAuth: mgr, resolveHTTPClient: testClientResolver(upstream.Client())}
	cfg := &config.Config{
		DirectMode: true, DirectTimeoutNoStream: 5000, DirectTimeoutStreamHeader: 5000, DirectTimeoutStreamActive: 5000,
		GlobalUserAgent: &config.GlobalUserAgents{OpenAIResponses: "global-oauth-agent/1.0"},
		GlobalUpstreamExtras: &config.GlobalUpstreamExtras{OpenAIResponses: &config.UpstreamExtras{
			Headers: map[string]string{"x-global-oauth": "enabled"},
			Body:    map[string]any{"reasoning": map[string]any{"effort": "high"}, "stream": false, "store": true, "model": "hijacked"},
		}},
	}
	raw := map[string]any{
		"model": "gpt-6-luna", "stream": false,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
	}
	request := httptest.NewRequest(http.MethodPost, "http://client.test/v1/responses", nil)
	request.Header.Set("User-Agent", "client-agent/1.0")
	outcome := srv.forwardAttempt(httptest.NewRecorder(), request, forwardAttemptInput{
		cfg: cfg, start: time.Now(), reqID: "global-oauth", clientFormat: "openai-responses", originalModel: "gpt-6-luna",
		internal: converter.FromOpenAIResponses(raw), rawBody: raw,
		candidate: router.Candidate{Provider: provider, TargetModel: "gpt-6-luna"}, attemptNo: 1, httpAttemptNo: 1,
		reqLog: &metrics.RequestLog{}, detail: &metrics.AttemptDetail{},
	})
	if !outcome.requestStarted || outcome.buildErr != "" {
		t.Fatalf("OAuth request was not sent: %#v", outcome)
	}
	if got := captured.header.Get("x-global-oauth"); got != "enabled" {
		t.Errorf("x-global-oauth = %q", got)
	}
	if got := captured.header.Get("User-Agent"); got != "global-oauth-agent/1.0" {
		t.Errorf("User-Agent = %q, want global OAuth fallback", got)
	}
	if reasoning, ok := captured.body["reasoning"].(map[string]any); !ok || reasoning["effort"] != "high" {
		t.Errorf("global reasoning = %#v", captured.body["reasoning"])
	}
	if got := captured.body["model"]; got != "gpt-6-luna" {
		t.Errorf("model = %#v, want managed model", got)
	}
	if got := captured.body["stream"]; got != true {
		t.Errorf("stream = %#v, OAuth post-processing must force true", got)
	}
	if got := captured.body["store"]; got != false {
		t.Errorf("store = %#v, OAuth post-processing must force false", got)
	}
}

func TestPutConfigPreservesGlobalUpstreamDefaults(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
	body := strings.Replace(testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 5), "routes:\n", `globalUserAgent:
  openai: "openai-global-agent/1.0"
  anthropic: "anthropic-global-agent/1.0"
  openai-responses: "responses-global-agent/1.0"
globalUpstreamExtras:
  openai:
    headers:
      x-global-default: enabled
    body:
      enable_thinking: true
routes:
`, 1)
	if recorder := putConfig(t, srv, body, ""); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status/body = %d/%s", recorder.Code, recorder.Body.String())
	}

	srv.cfgMu.RLock()
	gotUA := srv.cfg.GlobalUserAgent.ForFormat("openai")
	gotExtras := srv.cfg.GlobalUpstreamExtras.ForFormat("openai")
	srv.cfgMu.RUnlock()
	if gotUA != "openai-global-agent/1.0" || gotExtras == nil || gotExtras.Headers["x-global-default"] != "enabled" || gotExtras.Body["enable_thinking"] != true {
		t.Fatalf("in-memory global defaults = UA=%q extras=%#v", gotUA, gotExtras)
	}

	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		GlobalUserAgent      *config.GlobalUserAgents     `json:"globalUserAgent"`
		GlobalUpstreamExtras *config.GlobalUpstreamExtras `json:"globalUpstreamExtras"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.GlobalUserAgent == nil || view.GlobalUserAgent.ForFormat("openai") != "openai-global-agent/1.0" || view.GlobalUpstreamExtras == nil || view.GlobalUpstreamExtras.OpenAI.Body["enable_thinking"] != true {
		t.Fatalf("GET view global defaults = %#v", view)
	}

	persisted, err := config.DecodeAndValidate(mustReadFile(t, gotConfigPath(srv)))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.GlobalUpstreamExtras.ForFormat("openai").Headers["x-global-default"] != "enabled" {
		t.Fatalf("disk global defaults = %#v", persisted.GlobalUpstreamExtras)
	}
	if got := persisted.GlobalUserAgent.ForFormat("openai"); got != "openai-global-agent/1.0" {
		t.Fatalf("disk global User-Agent = %q", got)
	}
}

func TestFrontendKeepsGlobalUpstreamDefaultsInConfigPaths(t *testing.T) {
	checks := map[string][]string{
		filepath.Join("web", "src", "app", "02-config-normalize.js.part"): {"globalUserAgent", "normalizeGlobalUpstreamExtras"},
		filepath.Join("web", "src", "app", "07-nav-payload.js.part"):      {"globalUserAgent", "globalUpstreamExtrasPayload"},
		filepath.Join("web", "src", "app", "10-preview-yaml.js.part"):     {"globalUpstreamExtrasYaml"},
		filepath.Join("web", "src", "app", "11-providers.js.part"):        {"globalUpstreamExtraForm"},
		filepath.Join("web", "src", "index.template.html"):                {"global-upstream-extras", "全局 User-Agent"},
	}
	for path, required := range checks {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range required {
			if !strings.Contains(string(data), token) {
				t.Errorf("%s missing %q; visual editing/save/preview must preserve global defaults", path, token)
			}
		}
	}
}
