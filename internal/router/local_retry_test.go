package router

import (
	"testing"

	"ai-gateway/internal/config"
)

func localRetryValue(maxRetries, intervalMs int) *config.LocalRetry {
	return &config.LocalRetry{MaxRetries: &maxRetries, IntervalMs: &intervalMs}
}

func TestResolveLocalRetryUsesFieldLevelTargetRouteProviderPrecedence(t *testing.T) {
	provider := baseConfig().Providers["alpha"]
	provider.LocalRetry = localRetryValue(7, 11)
	route := config.Route{
		Match:      "*",
		LocalRetry: &config.LocalRetry{MaxRetries: intPtrForRouter(3)},
		Targets: []config.Target{{
			Provider:   "alpha",
			Model:      "model-a",
			LocalRetry: &config.LocalRetry{IntervalMs: intPtrForRouter(5)},
		}},
	}
	cfg := &config.Config{Providers: map[string]*config.Provider{"alpha": provider}, Routes: []config.Route{route}}

	providerFallbackRoute := config.Route{Match: "*", Targets: []config.Target{{Provider: "alpha", Model: "model-a"}}}
	cfg.Routes = []config.Route{providerFallbackRoute}
	candidate := MatchRoute("model-a", cfg).Candidates[0]
	if candidate.LocalRetryMax != 7 || candidate.LocalRetryIntervalMs != 11 {
		t.Fatalf("provider localRetry fallback = (%d, %d), 期望 (7, 11)", candidate.LocalRetryMax, candidate.LocalRetryIntervalMs)
	}

	cfg.Routes = []config.Route{route}
	matched := MatchRoute("model-a", cfg)
	if matched == nil || len(matched.Candidates) != 1 {
		t.Fatalf("MatchRoute() = %#v, 期望单候选命中", matched)
	}
	candidate = matched.Candidates[0]
	if candidate.LocalRetryMax != 3 || candidate.LocalRetryIntervalMs != 5 {
		t.Fatalf("router localRetry = (%d, %d), 期望字段级解析后 (3, 5)", candidate.LocalRetryMax, candidate.LocalRetryIntervalMs)
	}

	targetMax := 4
	route.Targets[0].LocalRetry.MaxRetries = &targetMax
	cfg.Routes = []config.Route{route}
	candidate = MatchRoute("model-a", cfg).Candidates[0]
	if candidate.LocalRetryMax != 4 || candidate.LocalRetryIntervalMs != 5 {
		t.Fatalf("target 覆盖后的 router localRetry = (%d, %d), 期望 (4, 5)", candidate.LocalRetryMax, candidate.LocalRetryIntervalMs)
	}

	// target.maxRetries 显式 0 覆盖 route/provider 的开启值；interval 是独立覆盖字段。
	zero := 0
	route.Targets[0].LocalRetry.MaxRetries = &zero
	cfg.Routes = []config.Route{route}
	candidate = MatchRoute("model-a", cfg).Candidates[0]
	if candidate.LocalRetryMax != 0 || candidate.LocalRetryIntervalMs != 5 {
		t.Fatalf("显式关闭后的 router localRetry = (%d, %d), 期望 (0, 5)", candidate.LocalRetryMax, candidate.LocalRetryIntervalMs)
	}
}

func TestResolveLocalRetryDefaultsIntervalAndLeavesRetriesDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "*", Provider: "alpha", Model: "model-a"}}
	candidate := MatchRoute("model-a", cfg).Candidates[0]
	if candidate.LocalRetryMax != 0 {
		t.Errorf("未配置 localRetry 的 LocalRetryMax = %d, 期望 0", candidate.LocalRetryMax)
	}
	if candidate.LocalRetryIntervalMs != config.DefaultLocalRetryIntervalMs {
		t.Errorf("未配置间隔 = %d, 期望默认 %d", candidate.LocalRetryIntervalMs, config.DefaultLocalRetryIntervalMs)
	}
}

func intPtrForRouter(value int) *int { return &value }
