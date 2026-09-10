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
