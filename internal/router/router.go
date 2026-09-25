// Package router 对齐 Node 版 lib/router.js：按顺序匹配路由 + 解析 API Key。
package router

import (
	"net/http"
	"strings"
	"unicode"

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
	// ContextWindow 该候选的上下文窗口，nil 表示未配置（不启用预算裁决）。
	// 优先级：target > route > provider，由 MatchRoute 合成。
	ContextWindow *int
	// ExtraBody 合并进上游请求体的自定义字段，nil 表示无。
	// 按键叠加 provider → route → target，同名键后者胜出，由 MatchRoute 合成。
	ExtraBody map[string]any
	// LocalRetryMax 是同一候选的额外重试次数，0 表示关闭。
	LocalRetryMax int
	// LocalRetryIntervalMs 是同一候选每次重试前等待的毫秒数。
	LocalRetryIntervalMs int
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
	// VisionDisabled 表示路由配置了 vision，但 vision provider 被禁用。
	// 此时 VisionProvider 为 nil，main.go 据此把图片块记为识别失败（软降级）。
	VisionDisabled bool
}

// MatchRoute 根据模型名匹配路由规则，首条命中生效（对齐 minimatch nocase）。
func MatchRoute(model string, cfg *config.Config) *Match {
	upstreamModel, oneMMarker, hasOneM := SplitOneMSuffix(model)
	for _, route := range cfg.Routes {
		if !globMatch(strings.ToLower(route.Match), strings.ToLower(upstreamModel)) {
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
			// 被禁用的 provider 不在这里剔除：候选过滤落在 main.go 的候选循环，
			// 那里能记 AttemptDetail(provider_disabled) 并给出 503 all_candidates_disabled
			// 终态。在 router 剔除会让候选空时静默 continue 到下一条路由，请求可能
			// 落到 catch-all 上一个完全不相关的模型，且诊断信息全无。
			// 复制 Provider 结构体，避免并发请求修改共享指针字段（如 APIKey）
			pCopy := *src
			targetModel := target.Model
			if targetModel == "" {
				targetModel = upstreamModel
			}
			contextWindow := resolveContextWindow(target, route, src)
			if hasOneM {
				// [1M] 是客户端声明的本地标记，优先级高于配置窗口，但不改变
				// maxTokens：大上下文不等于允许更大的输出。
				oneMWindow := OneMContextWindow
				contextWindow = &oneMWindow
				// 上游侧识别与本地预算裁决是两件独立的事：窗口覆盖对两档都生效
				// （网关得按 1M 算预算），而标记只在 preserve 档拼回 model 名。
				// TargetModel 是发给上游 model 字段的唯一出口（透传路径与三条
				// 跨格式转换都取它），在这里拼好，下游无需再感知该标记。
				if pCopy.OneMContext == config.OneMContextPreserve {
					targetModel += oneMMarker
				}
			}
			localRetryMax, localRetryIntervalMs := resolveLocalRetry(target, route, src)
			candidates = append(candidates, Candidate{
				Provider:             &pCopy,
				TargetModel:          targetModel,
				MaxTokens:            resolveMaxTokens(target, route, src),
				ContextWindow:        contextWindow,
				ExtraBody:            resolveExtraBody(target, route, src),
				LocalRetryMax:        localRetryMax,
				LocalRetryIntervalMs: localRetryIntervalMs,
			})
		}
		if len(candidates) == 0 {
			continue
		}
		m := &Match{RouteMatch: route.Match, Strategy: route.Strategy, Candidates: candidates}
		if route.Vision != nil {
			if vSrc := cfg.Providers[route.Vision.Provider]; vSrc != nil {
				if vSrc.IsEnabled() {
					vpCopy := *vSrc
					m.VisionProvider = &vpCopy
					m.VisionModel = route.Vision.Model
				} else {
					// vision provider 被禁用：标记后让 main.go 把图片块
					// 记为识别失败。vision 不走候选循环，这是它的唯一落点。
					m.VisionDisabled = true
					m.VisionModel = route.Vision.Model
				}
			}
		}
		return m
	}
	return nil
}

// resolveLocalRetry 按字段级优先级 target > route > provider 合成本地重试配置。
// 显式 MaxRetries: 0 会覆盖下层开启值；未指定 intervalMs 时使用固定默认间隔。
func resolveLocalRetry(target config.Target, route config.Route, provider *config.Provider) (maxRetries int, intervalMs int) {
	intervalMs = config.DefaultLocalRetryIntervalMs

	for _, retry := range []*config.LocalRetry{target.LocalRetry, route.LocalRetry, provider.LocalRetry} {
		if retry != nil && retry.MaxRetries != nil {
			if *retry.MaxRetries > 0 {
				maxRetries = *retry.MaxRetries
			}
			break
		}
	}
	for _, retry := range []*config.LocalRetry{target.LocalRetry, route.LocalRetry, provider.LocalRetry} {
		if retry != nil && retry.IntervalMs != nil {
			intervalMs = *retry.IntervalMs
			break
		}
	}
	return maxRetries, intervalMs
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

// resolveContextWindow 按优先级 target > route > provider 合成该候选的上下文窗口。
// 三层都未配置时返回 nil，由调用方跳过预算估算与裁决。
func resolveContextWindow(target config.Target, route config.Route, provider *config.Provider) *int {
	if target.ContextWindow != nil {
		return target.ContextWindow
	}
	if route.ContextWindow != nil {
		return route.ContextWindow
	}
	return provider.ContextWindow
}

// resolveExtraBody 按键叠加 provider → route → target 合成该候选的 extraBody，
// 同名键后者胜出（target > route > provider）。与 maxTokens 的整值覆盖不同：
// extraBody 是一袋独立字段，叠加让 provider 声明的公共字段（如 enable_thinking）
// 无需在每个 target 重抄，target 只写它要额外覆盖的键。
//
// 返回全新 map，不引用任何一层的原始 map：候选的 Provider 是值拷贝但 map 是
// 引用，就地改会污染共享配置，也会让并发请求互相看到对方的写入。三层都为空时
// 返回 nil，与「未配置」逐字节等价。
func resolveExtraBody(target config.Target, route config.Route, provider *config.Provider) map[string]any {
	if len(provider.ExtraBody) == 0 && len(route.ExtraBody) == 0 && len(target.ExtraBody) == 0 {
		return nil
	}
	merged := make(map[string]any, len(provider.ExtraBody)+len(route.ExtraBody)+len(target.ExtraBody))
	for k, v := range provider.ExtraBody {
		merged[k] = v
	}
	for k, v := range route.ExtraBody {
		merged[k] = v
	}
	for k, v := range target.ExtraBody {
		merged[k] = v
	}
	return merged
}

// OneMContextMarker 是 Claude Code 声明 100 万上下文的本地标记。
const OneMContextMarker = "[1m]"

// OneMContextWindow 是带该标记时视为的窗口值。
const OneMContextWindow = 1_000_000

// StripOneMSuffix 剥离模型名尾部的 [1M] 标记（大小写不敏感，容忍标记前的空格）。
// 返回 (剥离后的模型名, 是否带有该标记)。无标记时原样返回。
func StripOneMSuffix(model string) (string, bool) {
	stripped, _, has := SplitOneMSuffix(model)
	return stripped, has
}

// SplitOneMSuffix 在 StripOneMSuffix 之外额外返回客户端原样书写的标记文本。
//
// 需要原始文本而非固定常量：转发给 preserve 型上游时要把标记拼回 model 名，
// 而中转站的识别逻辑不可知，客户端写 [1M] 就还它 [1M]、写 [1m] 就还 [1m]，
// 不做大小写归一化，最大保真。标记前的空格不保留——那只是容错，不是语义。
// 无标记时 marker 为空字符串。
func SplitOneMSuffix(model string) (stripped, marker string, has bool) {
	if !strings.HasSuffix(strings.ToLower(model), OneMContextMarker) {
		return model, "", false
	}
	cut := len(model) - len(OneMContextMarker)
	marker = model[cut:]
	stripped = strings.TrimRightFunc(model[:cut], unicode.IsSpace)
	return stripped, marker, true
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
