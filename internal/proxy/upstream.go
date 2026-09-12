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
	SetUpstreamAuthHeaders(req.Header, p.Format, p.APIKey)
	// 放在最后：允许覆盖上面的 anthropic-version / accept 这类协议头，
	// 鉴权与 User-Agent 由黑名单挡住，不会被配置改坏。
	ApplyExtraHeaders(req.Header, p.ExtraHeaders)
}

// SetUpstreamAuthHeaders 按 provider 格式设置鉴权头，三条出网路径（转发、模型列表
// 查询、健康检测）共用。
//
// anthropic 格式下同时发 x-api-key 和 Authorization: Bearer，两个头都带。
//
// 原因：第三方中转对「Anthropic 格式」的鉴权头实现并不统一。new-api / one-api 系
// （anyrouter、agentrouter 等）主要按 Authorization 取令牌——Claude Code 对接它们时
// 官方文档让用户设 ANTHROPIC_AUTH_TOKEN 而不是 ANTHROPIC_API_KEY，前者发的就是
// Authorization: Bearer。只发 x-api-key 会让这类上游认不出令牌。
//
// 两个头都带是否有副作用，三个真实端点实测过（均用假 key，只看它对头组合的反应）：
//
//	api.anthropic.com  只带 x-api-key → 401 authentication_error；两个都带 → 同一错误
//	anyrouter.top      只带 x-api-key / 只带 Bearer / 两个都带 → 均 401「无效的令牌」
//	agentrouter.org    三种组合 → 均 401 unauthorized_client_error
//
// 三处都没有出现「鉴权头冲突」这类新错误，官方端点对多余的 Authorization 直接忽略。
// 此前代码里「两个头都带可能被部分上游判为冲突」的注释是保守推断，已被上述实测证伪。
func SetUpstreamAuthHeaders(h http.Header, format, apiKey string) {
	if format == "anthropic" {
		h.Set("x-api-key", apiKey)
		// anthropic-version 官方必填，缺了会 400；中转带上无害。
		h.Set("anthropic-version", "2023-06-01")
		// key 为空时不补 Authorization，保持这条路径的原有行为。空 key 是可达状态
		// （provider.apiKey 留空且客户端也没带鉴权头，转发不会因此阻断），典型场景是
		// 本地不校验鉴权的 Anthropic 兼容服务；给它多发一个 "Bearer "（空令牌）可能
		// 被解析成非法 Authorization 而 400，比不发更糟。
		if apiKey == "" {
			return
		}
	}
	// 其余情况都带 Authorization：OpenAI 风味端点与各类中转都认这个头。
	// openai 系 key 为空时仍写空 Bearer，与改造前逐字节一致。
	h.Set("authorization", "Bearer "+apiKey)
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
