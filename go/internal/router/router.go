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
	TargetModel    string
	VisionProvider *config.Provider // 可能为 nil
	VisionModel    string
}

// MatchRoute 根据模型名匹配路由规则，首条命中生效（对齐 minimatch nocase）。
func MatchRoute(model string, cfg *config.Config) *Match {
	for _, route := range cfg.Routes {
		if globMatch(strings.ToLower(route.Match), strings.ToLower(model)) {
			p := cfg.Providers[route.Provider]
			target := route.Model
			if target == "" {
				target = model
			}
			m := &Match{Provider: p, TargetModel: target}
			if route.Vision != nil {
				m.VisionProvider = cfg.Providers[route.Vision.Provider]
				m.VisionModel = route.Vision.Model
			}
			return m
		}
	}
	return nil
}

// ResolveAPIKey 优先用 provider.apiKey，否则从请求头提取（x-api-key 或 Bearer）。
func ResolveAPIKey(p *config.Provider, h http.Header) string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if k := h.Get("x-api-key"); k != "" {
		return k
	}
	auth := h.Get("authorization")
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
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
