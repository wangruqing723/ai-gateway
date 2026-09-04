// Package router 对齐 Node 版 lib/router.js：按顺序匹配路由 + 解析 API Key。
package router

import (
	"net/http"
	"strings"

	"ai-gateway/internal/config"
)

// Candidate 是一次路由匹配得到的单个候选上游目标。
// Provider 为值拷贝，隔离并发请求对 APIKey 等字段的写入。
type Candidate struct {
	Provider    *config.Provider
	TargetModel string
	// MaxTokens 该候选的输出上限，nil 表示不覆盖客户端值。
	// 优先级：target > route > provider，由 MatchRoute 合成。
	MaxTokens *int
}

// Match 路由匹配结果。
// Candidates 至少 1 个，按配置顺序排列。顺序是「默认尝试顺序」，
// 真实尝试顺序由 balancer 按 Strategy 重排，router 本身保持无状态。
type Match struct {
	RouteMatch string
	// Strategy 该路由的候选选择策略，空字符串等同 failover（按配置顺序）。
	Strategy       string
	Candidates     []Candidate
	VisionProvider *config.Provider // 可能为 nil
	VisionModel    string
}

// MatchRoute 根据模型名匹配路由规则，首条命中生效（对齐 minimatch nocase）。
func MatchRoute(model string, cfg *config.Config) *Match {
	for _, route := range cfg.Routes {
		if !globMatch(strings.ToLower(route.Match), strings.ToLower(model)) {
			continue
		}
		targets := route.TargetList()
		candidates := make([]Candidate, 0, len(targets))
		for _, target := range targets {
			src := cfg.Providers[target.Provider]
			if src == nil {
				// 校验期已拦截未定义 provider；此处保守跳过，避免运行时 panic。
				continue
			}
			// 复制 Provider 结构体，避免并发请求修改共享指针字段（如 APIKey）
			pCopy := *src
			targetModel := target.Model
			if targetModel == "" {
				targetModel = model
			}
			candidates = append(candidates, Candidate{
				Provider:    &pCopy,
				TargetModel: targetModel,
				MaxTokens:   resolveMaxTokens(target, route, src),
			})
		}
		if len(candidates) == 0 {
			continue
		}
		m := &Match{RouteMatch: route.Match, Strategy: route.Strategy, Candidates: candidates}
		if route.Vision != nil {
			if vSrc := cfg.Providers[route.Vision.Provider]; vSrc != nil {
				vpCopy := *vSrc
				m.VisionProvider = &vpCopy
				m.VisionModel = route.Vision.Model
			}
		}
		return m
	}
	return nil
}

// resolveMaxTokens 按优先级 target > route > provider 合成该候选的输出上限。
// 三层都未配置时返回 nil，由网关全局默认 32768 兜底。
func resolveMaxTokens(target config.Target, route config.Route, provider *config.Provider) *int {
	if target.MaxTokens != nil {
		return target.MaxTokens
	}
	if route.MaxTokens != nil {
		return route.MaxTokens
	}
	return provider.MaxTokens
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
