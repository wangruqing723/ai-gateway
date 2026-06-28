// Package router 对齐 Node 版 lib/router.js：按顺序匹配路由 + 解析 API Key。
package router

import (
	"net/http"
	"strings"

	"ai-gateway/internal/config"
)

// Match 路由匹配结果
type Match struct {
	Provider       *config.Provider
	RouteMatch     string
	TargetModel    string
	VisionProvider *config.Provider // 可能为 nil
	VisionModel    string
}

// MatchRoute 根据模型名匹配路由规则，首条命中生效（对齐 minimatch nocase）。
func MatchRoute(model string, cfg *config.Config) *Match {
	for _, route := range cfg.Routes {
		if globMatch(strings.ToLower(route.Match), strings.ToLower(model)) {
			// 复制 Provider 结构体，避免并发请求修改共享指针字段（如 APIKey）
			pSrc := cfg.Providers[route.Provider]
			pCopy := *pSrc
			target := route.Model
			if target == "" {
				target = model
			}
			m := &Match{Provider: &pCopy, RouteMatch: route.Match, TargetModel: target}
			if route.Vision != nil {
				vpCopy := *cfg.Providers[route.Vision.Provider]
				m.VisionProvider = &vpCopy
				m.VisionModel = route.Vision.Model
			}
			return m
		}
	}
	return nil
}

// ResolveAPIKey 优先用 provider.apiKey，否则从请求头提取（x-api-key 或 Bearer）。
func ResolveAPIKey(p *config.Provider, h http.Header) string {
	k, _ := ResolveAPIKeyWithSource(p, h)
	return k
}

// ResolveAPIKeyWithSource 与 ResolveAPIKey 行为一致，但额外返回 key 的来源。
// 返回值:
// - string: 解析到的 key，未命中则空
// - string: key 来源，provider / x-api-key / authorization / none
func ResolveAPIKeyWithSource(p *config.Provider, h http.Header) (string, string) {
	if p.APIKey != "" {
		return p.APIKey, "provider"
	}
	if k := h.Get("x-api-key"); k != "" {
		return k, "x-api-key"
	}
	auth := h.Get("authorization")
	auth = strings.TrimSpace(auth)
	if auth == "" {
		return "", "none"
	}
	low := strings.ToLower(auth)
	if strings.HasPrefix(low, "bearer ") {
		auth = strings.TrimSpace(auth[len("bearer "):])
	}
	if auth == "" {
		return "", "none"
	}
	return auth, "authorization"
}

// globMatch 实现 minimatch 子集：支持 * 与 ?，足够覆盖路由场景（claude-opus* 等）。
func globMatch(pattern, s string) bool {
	// 经典动态规划通配匹配
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == s[si] || pattern[pi] == '?') {
			pi++
			si++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			mark = si
			pi++
		} else if star != -1 {
			pi = star + 1
			mark++
			si = mark
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
