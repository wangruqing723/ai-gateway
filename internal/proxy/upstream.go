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

// SetModelsAuthHeaders 为 /v1/models 探测请求设置鉴权头（模型列表查询与健康检测共用）。
//
// 与转发路径 setUpstreamHeaders 的关键差异：anthropic 格式下这里把 x-api-key 和
// Authorization 两个头都带上，而转发路径只带 x-api-key。
//
// 原因：/v1/models 不是 Anthropic 协议端点，是 OpenAI 风味的元数据端点。Anthropic
// 官方只读 x-api-key、忽略多余的 Authorization；但 new-api / one-api 系的第三方中转
// （anyrouter、agentrouter 等）用同一套路由暴露 /v1/models，只认 Authorization。
// 实测 anyrouter.top：x-api-key 返回 401，Authorization: Bearer 返回 200。
//
// 只带 x-api-key 的话，这类中转的转发路径好好的、模型列表却永远查不出来，健康检测
// 还会把它误判成「鉴权失败」。转发路径不受影响也不该跟着改：/v1/messages 是真正的
// Anthropic 端点，两个头都带可能被部分上游判为冲突。
func SetModelsAuthHeaders(h http.Header, format, apiKey string) {
	// accept 与 anthropic-version 按格式设：官方 Anthropic 要求 anthropic-version，
	// 缺了会 400；OpenAI 风味端点带上无害，故 anthropic 格式下一并设置。
	if format == "anthropic" {
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", "2023-06-01")
	}
	// 所有格式都带 Authorization：OpenAI 风味端点（含各类中转）认这个头。
	h.Set("authorization", "Bearer "+apiKey)
}
