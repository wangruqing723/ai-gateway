package proxy

import (
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
)

func TestBuildUpstreamURLSelectsProtocolEndpoint(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{"anthropic", "/v1/messages"},
		{"openai", "/v1/chat/completions"},
		{"openai-responses", "/v1/responses"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			base, path := buildUpstreamURL(&config.Provider{BaseURL: "https://upstream.example/v1", Format: tt.format})
			if base != "https://upstream.example" || path != tt.want {
				t.Fatalf("base/path = %q/%q, want https://upstream.example/%q", base, path, tt.want)
			}
		})
	}
}

func TestResponsesProviderUsesBearerAuthentication(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{Format: "openai-responses", APIKey: "test-key"}, "")
	if got := req.Header.Get("authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("unexpected anthropic key header = %q", got)
	}
}

func TestSetUpstreamHeadersAppliesExtraHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{
		Format: "anthropic",
		APIKey: "real-key",
		ExtraHeaders: map[string]string{
			"anthropic-beta": "context-1m-2025-08-07",
			"x-trace":        "abc",
			// 覆盖协议头是允许的
			"anthropic-version": "2099-01-01",
		},
	}, "")

	if got := req.Header.Get("anthropic-beta"); got != "context-1m-2025-08-07" {
		t.Errorf("anthropic-beta = %q，期望自定义头被写入", got)
	}
	if got := req.Header.Get("x-trace"); got != "abc" {
		t.Errorf("x-trace = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2099-01-01" {
		t.Errorf("anthropic-version = %q，期望允许被 extraHeaders 覆盖", got)
	}
}

// TestSetUpstreamHeadersExtraHeadersCannotBreakAuth 锁住运行时兜底：
// 即使配置校验被绕过（热重载、未来新入口），鉴权与 UA 也绝不能被覆盖。
func TestSetUpstreamHeadersExtraHeadersCannotBreakAuth(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{
		Format:    "anthropic",
		APIKey:    "real-key",
		UserAgent: "provider-agent/1.0",
		ExtraHeaders: map[string]string{
			"x-api-key":     "hijacked",
			"Authorization": "Bearer hijacked",
			"content-type":  "text/plain",
			"User-Agent":    "hijacked-agent",
			"":              "empty-name",
			"   ":           "blank-name",
		},
	}, "")

	if got := req.Header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q，期望保持 real-key", got)
	}
	if got := req.Header.Get("authorization"); got != "" {
		t.Errorf("authorization = %q，anthropic 格式不应出现该头", got)
	}
	if got := req.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q，期望保持 application/json", got)
	}
	if got := req.Header.Get("User-Agent"); got != "provider-agent/1.0" {
		t.Errorf("User-Agent = %q，期望保持 provider 配置值", got)
	}
}

func TestApplyExtraHeadersNilAndEmpty(t *testing.T) {
	// nil 与空 map 都必须是空操作：逐字节兼容未配置该字段的既有行为。
	for _, extra := range []map[string]string{nil, {}} {
		req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
		before := len(req.Header)
		ApplyExtraHeaders(req.Header, extra)
		if len(req.Header) != before {
			t.Fatalf("extra=%#v 改变了头数量：%d → %d", extra, before, len(req.Header))
		}
	}
}

// TestSetModelsAuthHeadersSendsBothForAnthropic 锁住 /v1/models 探测路径与转发路径的
// 有意分裂：anthropic 格式下探测请求必须同时带 x-api-key 和 Authorization。
//
// 判据来自实测 anyrouter.top：只带 x-api-key 返回 401，带 Authorization 返回 200。
// new-api / one-api 系中转把 /v1/models 暴露在 OpenAI 风味路由上，只认 Authorization；
// 少了它，这类 provider 转发正常却查不出模型列表，健康检测还会误判成「鉴权失败」。
func TestSetModelsAuthHeadersSendsBothForAnthropic(t *testing.T) {
	h := make(map[string][]string)
	header := httptest.NewRequest("GET", "http://gateway.invalid", nil).Header
	for k := range header {
		delete(header, k)
	}
	_ = h

	SetModelsAuthHeaders(header, "anthropic", "real-key")

	if got := header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q，期望 real-key（Anthropic 官方只读这个头）", got)
	}
	if got := header.Get("authorization"); got != "Bearer real-key" {
		t.Errorf("authorization = %q，期望 Bearer real-key（中转只认这个头）", got)
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望 2023-06-01（官方缺了会 400）", got)
	}
}

// TestSetModelsAuthHeadersOpenAIFormatsUnchanged 锁住非 anthropic 格式不受本次改动影响：
// 只发 Authorization，不得多出 x-api-key / anthropic-version。
func TestSetModelsAuthHeadersOpenAIFormatsUnchanged(t *testing.T) {
	for _, format := range []string{"openai", "openai-responses"} {
		t.Run(format, func(t *testing.T) {
			header := httptest.NewRequest("GET", "http://gateway.invalid", nil).Header
			for k := range header {
				delete(header, k)
			}

			SetModelsAuthHeaders(header, format, "test-key")

			if got := header.Get("authorization"); got != "Bearer test-key" {
				t.Errorf("authorization = %q", got)
			}
			if got := header.Get("x-api-key"); got != "" {
				t.Errorf("x-api-key = %q，非 anthropic 格式不应出现该头", got)
			}
			if got := header.Get("anthropic-version"); got != "" {
				t.Errorf("anthropic-version = %q，非 anthropic 格式不应出现该头", got)
			}
		})
	}
}

// TestForwardPathKeepsAnthropicAuthOnly 与上面两条配对，把「转发路径不跟着改」写死。
// /v1/messages 是真正的 Anthropic 协议端点，两个鉴权头都带可能被部分上游判为冲突；
// 而用户实测转发路径本来就是通的，没有理由冒这个风险。
func TestForwardPathKeepsAnthropicAuthOnly(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{Format: "anthropic", APIKey: "real-key"}, "")

	if got := req.Header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := req.Header.Get("authorization"); got != "" {
		t.Errorf("authorization = %q，转发路径不应带该头（与 /v1/models 探测路径有意不同）", got)
	}
}

func TestSetUpstreamHeadersUserAgentPriority(t *testing.T) {
	tests := []struct {
		name            string
		providerUA      string
		clientUA        string
		want            string
		wantHeaderExist bool
	}{
		{
			name:            "provider user agent overrides client user agent",
			providerUA:      "provider-agent/1.0",
			clientUA:        "client-agent/2.0",
			want:            "provider-agent/1.0",
			wantHeaderExist: true,
		},
		{
			name:            "client user agent is forwarded when provider is not configured",
			clientUA:        "client-agent/2.0",
			want:            "client-agent/2.0",
			wantHeaderExist: true,
		},
		{
			name:            "user agent remains unset when neither source provides one",
			wantHeaderExist: false,
		},
		{
			name:            "provider user agent is used without a client user agent",
			providerUA:      "provider-agent/1.0",
			want:            "provider-agent/1.0",
			wantHeaderExist: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
			setUpstreamHeaders(req, &config.Provider{Format: "openai", UserAgent: tt.providerUA}, tt.clientUA)

			_, gotHeader := req.Header["User-Agent"]
			if gotHeader != tt.wantHeaderExist {
				t.Fatalf("User-Agent header present = %v, want %v", gotHeader, tt.wantHeaderExist)
			}
			if got := req.Header.Get("User-Agent"); got != tt.want {
				t.Fatalf("User-Agent = %q, want %q", got, tt.want)
			}
		})
	}
}
