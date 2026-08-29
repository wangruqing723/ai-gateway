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
	setUpstreamHeaders(req, &config.Provider{Format: "openai-responses", APIKey: "test-key"})
	if got := req.Header.Get("authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("unexpected anthropic key header = %q", got)
	}
}
