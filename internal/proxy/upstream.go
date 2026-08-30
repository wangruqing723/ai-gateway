package proxy

import (
	"net/http"
	"strings"

	"ai-gateway/internal/config"
)

// buildUpstreamURL 去掉末尾 /v1，再按 provider format 选择对应的上游 API 端点。
func buildUpstreamURL(p *config.Provider) (base, path string) {
	base = p.BaseURL
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	switch p.Format {
	case "anthropic":
		return base, "/v1/messages"
	case "openai-responses":
		return base, "/v1/responses"
	default: // 配置校验已保证其余合法值只能是 openai。
		return base, "/v1/chat/completions"
	}
}

// setUpstreamHeaders 按 provider 格式设置鉴权头，并按优先级设置 User-Agent。
func setUpstreamHeaders(req *http.Request, p *config.Provider, clientUserAgent string) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	} else if clientUserAgent != "" {
		req.Header.Set("User-Agent", clientUserAgent)
	}
	if p.Format == "anthropic" {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("authorization", "Bearer "+p.APIKey)
	}
}
