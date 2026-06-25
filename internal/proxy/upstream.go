package proxy

import (
	"net/http"
	"strings"

	"ai-gateway/internal/config"
)

// buildUpstreamURL 对齐 Node 版：去掉末尾 /v1，按 format 拼接 /v1/messages 或 /v1/chat/completions。
func buildUpstreamURL(p *config.Provider) (base, path string) {
	base = p.BaseURL
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	if p.Format == "anthropic" {
		return base, "/v1/messages"
	}
	return base, "/v1/chat/completions"
}

// setUpstreamHeaders 按 provider 格式设置鉴权头。
func setUpstreamHeaders(req *http.Request, p *config.Provider) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	if p.Format == "anthropic" {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("authorization", "Bearer "+p.APIKey)
	}
}
