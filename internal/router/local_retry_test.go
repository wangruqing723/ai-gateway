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

func TestResolveTimeoutAndRetryModesWithExplicitTargetOverrides(t *testing.T) {
	provider := baseConfig().Providers["alpha"]
	provider.LocalRetry = &config.LocalRetry{MaxRetries: intPtrForRouter(9), IntervalMs: intPtrForRouter(21)}
	defaults := MatchRoute("model-default", &config.Config{
		Providers: map[string]*config.Provider{"alpha": provider},
		Routes:    []config.Route{{Match: "*", Provider: "alpha", Model: "model-default"}},
	})
	if defaults == nil || len(defaults.Candidates) != 1 {
		t.Fatalf("默认 MatchRoute() = %#v, 期望一个候选", defaults)
	}
	if defaults.Candidates[0].TimeoutMode != config.TimeoutModeGlobal || defaults.Candidates[0].LocalRetryMode != config.LocalRetryModeLimited {
		t.Errorf("省略策略时应保持 global/limited 默认，得到 timeout:%q retry:%q", defaults.Candidates[0].TimeoutMode, defaults.Candidates[0].LocalRetryMode)
	}

	route := config.Route{
		Match:       "*",
		TimeoutMode: config.TimeoutModeClient,
		LocalRetry:  &config.LocalRetry{Mode: config.LocalRetryModeUntilDisconnect, MaxRetries: intPtrForRouter(3), IntervalMs: intPtrForRouter(10)},
		Targets: []config.Target{
			{
				Provider:    "alpha",
				Model:       "model-a",
				TimeoutMode: config.TimeoutModeGlobal,
				LocalRetry:  &config.LocalRetry{Mode: config.LocalRetryModeLimited, MaxRetries: intPtrForRouter(0), IntervalMs: intPtrForRouter(0)},
			},
			{Provider: "alpha", Model: "model-b"},
		},
	}
	cfg := &config.Config{Providers: map[string]*config.Provider{"alpha": provider}, Routes: []config.Route{route}}
	matched := MatchRoute("model-a", cfg)
	if matched == nil || len(matched.Candidates) != 2 {
		t.Fatalf("MatchRoute() = %#v, 期望两个候选", matched)
	}
	overridden, inherited := matched.Candidates[0], matched.Candidates[1]
	if overridden.TimeoutMode != config.TimeoutModeGlobal || overridden.LocalRetryMode != config.LocalRetryModeLimited {
		t.Errorf("target 显式覆盖后的模式 = timeout:%q retry:%q", overridden.TimeoutMode, overridden.LocalRetryMode)
	}
	if overridden.LocalRetryMax != 0 || overridden.LocalRetryIntervalMs != 0 {
		t.Errorf("target 覆盖后的有限次数/间隔 = %d/%d", overridden.LocalRetryMax, overridden.LocalRetryIntervalMs)
	}
	if inherited.TimeoutMode != config.TimeoutModeClient || inherited.LocalRetryMode != config.LocalRetryModeUntilDisconnect {
		t.Errorf("继承路由模式后的值 = timeout:%q retry:%q", inherited.TimeoutMode, inherited.LocalRetryMode)
	}
	if inherited.LocalRetryMax != 3 || inherited.LocalRetryIntervalMs != 10 {
		t.Errorf("路由字段继承后的有限参数 = %d/%d", inherited.LocalRetryMax, inherited.LocalRetryIntervalMs)
	}
}
