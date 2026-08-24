package router

import (
	"testing"

	"ai-gateway/internal/config"
)

// Match.Strategy 必须原样透传路由上的取值：router 只负责搬运，
// 具体重排交给 balancer，这里锁住「不会在中途被改写或丢掉」。
func TestMatchRouteCarriesStrategy(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
	}{
		{"未配置", ""},
		{"failover", "failover"},
		{"round-robin", "round-robin"},
		{"least-queue", "least-queue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Routes = []config.Route{{
				Match:    "*",
				Strategy: tc.strategy,
				Targets: []config.Target{
					{Provider: "alpha", Model: "m-alpha"},
					{Provider: "beta", Model: "m-beta"},
				},
			}}

			m := MatchRoute("any-model", cfg)
			if m == nil {
				t.Fatal("期望命中路由，实际为 nil")
			}
			if m.Strategy != tc.strategy {
				t.Errorf("Match.Strategy = %q, 期望 %q", m.Strategy, tc.strategy)
			}
		})
	}
}

// 多条路由各自带策略时不能互相串味，命中哪条就带哪条的策略。
func TestMatchRouteStrategyIsPerRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{
		{
			Match:    "claude-*",
			Strategy: "round-robin",
			Targets: []config.Target{
				{Provider: "alpha", Model: "m-alpha"},
				{Provider: "beta", Model: "m-beta"},
			},
		},
		{
			Match:    "*",
			Strategy: "least-queue",
			Targets: []config.Target{
				{Provider: "beta", Model: "m-beta"},
				{Provider: "gamma", Model: "m-gamma"},
			},
		},
	}

	if m := MatchRoute("claude-opus-4", cfg); m == nil || m.Strategy != "round-robin" {
		t.Fatalf("claude-* 应带 round-robin, 实际 = %+v", m)
	}
	if m := MatchRoute("gpt-5", cfg); m == nil || m.Strategy != "least-queue" {
		t.Fatalf("* 应带 least-queue, 实际 = %+v", m)
	}
}

// 单目标写法同样要透传策略：router 不做「单候选就清空策略」这类隐式修正，
// 这类组合的合法性由 config 校验拦，不在匹配阶段悄悄改语义。
func TestMatchRouteStrategyWithSingleTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "*", Provider: "alpha", Model: "m-alpha", Strategy: "failover"}}

	m := MatchRoute("any-model", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	if m.Strategy != "failover" {
		t.Errorf("Match.Strategy = %q, 期望 failover", m.Strategy)
	}
	if len(m.Candidates) != 1 {
		t.Errorf("候选数 = %d, 期望 1", len(m.Candidates))
	}
}
