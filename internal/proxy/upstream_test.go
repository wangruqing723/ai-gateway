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
