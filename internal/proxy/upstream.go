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
	// 放在最后：允许覆盖上面的 anthropic-version / accept 这类协议头，
	// 鉴权与 User-Agent 由黑名单挡住，不会被配置改坏。
	ApplyExtraHeaders(req.Header, p.ExtraHeaders)
}

// ApplyExtraHeaders 把 provider 配置的自定义头写入 h，跳过黑名单内的头。
//
// 三条出网路径（转发、模型列表查询、健康检测）共用本函数：上游按头做准入时
// 不区分请求由谁发起，漏掉任一条都会让该 provider 的探测被误判。
// 黑名单在这里再挡一遍而不只依赖配置校验：校验只在加载时跑，运行时兜底能保证
// 即使将来多出别的配置入口，鉴权头也绝不会被覆盖。
func ApplyExtraHeaders(h http.Header, extra map[string]string) {
	for key, value := range extra {
		name := strings.TrimSpace(key)
		if name == "" || config.ExtraHeaderBlocked(name) {
			continue
		}
		h.Set(name, value)
	}
}
