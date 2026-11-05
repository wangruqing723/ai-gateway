package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 第五阶段 T26：账户级共享约束与路由额度感知开关的零值、边界和落盘形状。

func TestChatGPTAccountLimitsDefaultsAndBounds(t *testing.T) {
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatGPTAccountLimits.Enabled() || cfg.ChatGPTAccountLimits.SharedCooldown || cfg.ChatGPTAccountLimits.MaxConcurrent != nil {
		t.Fatalf("未配置时不应启用账户约束: %#v", cfg.ChatGPTAccountLimits)
	}
	cases := []struct {
		name    string
		section string
		wantErr string
		enabled bool
		limit   int
	}{
		{"并发上限", "  maxConcurrent: 2\n", "", true, 2},
		{"只开速率", "  maxPerSecond: 3\n", "", true, 0},
		{"只开冷却", "  sharedCooldown: true\n", "", false, 0},
		{"速率 0 不启用", "  maxPerSecond: 0\n", "", false, 0},
		{"显式并发 0 报错", "  maxConcurrent: 0\n", "chatgptAccountLimits.maxConcurrent", false, 0},
		{"并发超上限", "  maxConcurrent: 101\n", "chatgptAccountLimits.maxConcurrent", false, 0},
		{"速率为负", "  maxPerSecond: -1\n", "chatgptAccountLimits.maxPerSecond", false, 0},
		{"速率超上限", "  maxPerSecond: 101\n", "chatgptAccountLimits.maxPerSecond", false, 0},
		{"未知字段", "  maxQueueWait: 1000\n", "maxQueueWait", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(), "providers:", "chatgptAccountLimits:\n"+tc.section+"providers:", 1)
			cfg, err := DecodeAndValidate([]byte(raw))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			limits := cfg.ChatGPTAccountLimits
			if limits.Enabled() != tc.enabled || limits.ConcurrencyLimit() != tc.limit {
				t.Fatalf("limits = %#v enabled=%v", limits, limits.Enabled())
			}
		})
	}
}

// 整段都是零值时落盘省略，避免给每份配置添一个空节；写了任意项则原样保留。
func TestChatGPTAccountLimitsRoundTrip(t *testing.T) {
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "chatgptAccountLimits") {
		t.Fatalf("未配置时不应落盘: %s", out)
	}
	raw := strings.Replace(validConfigYAML(), "providers:", "chatgptAccountLimits:\n  maxConcurrent: 3\n  sharedCooldown: true\nproviders:", 1)
	cfg, err = DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeAndValidate(out)
	if err != nil {
		t.Fatalf("落盘结果应能重新加载: %v\n%s", err, out)
	}
	if again.ChatGPTAccountLimits.ConcurrencyLimit() != 3 || !again.ChatGPTAccountLimits.SharedCooldown || again.ChatGPTAccountLimits.MaxPerSecond != 0 {
		t.Fatalf("往返后 = %#v", again.ChatGPTAccountLimits)
	}
}

func TestRouteQuotaAwareRequiresMultipleTargets(t *testing.T) {
	single := strings.Replace(validConfigYAML(), "    model: upstream-model\n", "    model: upstream-model\n    quotaAware: true\n", 1)
	if _, err := DecodeAndValidate([]byte(single)); err == nil || !strings.Contains(err.Error(), "quotaAware") {
		t.Fatalf("单候选 quotaAware 应报错: %v", err)
	}
	multi := strings.Replace(validConfigYAML(), `  - match: "*"
    provider: primary
    model: upstream-model
`, `  - match: "*"
    quotaAware: true
    targets:
      - provider: primary
        model: a
      - provider: primary
        model: b
`, 1)
	cfg, err := DecodeAndValidate([]byte(multi))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Routes[0].QuotaAware {
		t.Fatal("多候选 quotaAware 应保留")
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "quotaAware: true") {
		t.Fatalf("quotaAware 应落盘: %s", out)
	}
}
