package config

import (
	"strings"
	"testing"
)

func TestDecodeAndValidateLocalRetryBounds(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		valid bool
		want  string
	}{
		{name: "zero is valid at provider", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      maxRetries: 0\n      intervalMs: 0", 1), valid: true},
		{name: "upper bounds are valid", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      maxRetries: 10\n      intervalMs: 60000", 1), valid: true},
		{name: "negative retries", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      maxRetries: -1", 1), want: "providers.primary.localRetry.maxRetries"},
		{name: "too many retries", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      maxRetries: 11", 1), want: "providers.primary.localRetry.maxRetries"},
		{name: "negative interval", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      intervalMs: -1", 1), want: "providers.primary.localRetry.intervalMs"},
		{name: "too long interval", raw: strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      intervalMs: 60001", 1), want: "providers.primary.localRetry.intervalMs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(tt.raw))
			if tt.valid {
				if err != nil {
					t.Fatalf("DecodeAndValidate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateLocalRetryAtRouteAndTargetLevels(t *testing.T) {
	routeLevel := strings.Replace(validConfigYAML(), "    model: upstream-model", "    model: upstream-model\n    localRetry:\n      maxRetries: 11", 1)
	if _, err := DecodeAndValidate([]byte(routeLevel)); err == nil || !strings.Contains(err.Error(), `route "*".localRetry.maxRetries`) {
		t.Fatalf("路由级 localRetry 校验错误 = %v", err)
	}

	targets := "  - match: \"*\"\n    targets:\n      - provider: primary\n        model: upstream-model\n        localRetry:\n          intervalMs: 60001"
	targetLevel := strings.Replace(validConfigYAML(), "  - match: \"*\"\n    provider: primary\n    model: upstream-model", targets, 1)
	if _, err := DecodeAndValidate([]byte(targetLevel)); err == nil || !strings.Contains(err.Error(), `route "*".targets[0].localRetry.intervalMs`) {
		t.Fatalf("候选级 localRetry 校验错误 = %v", err)
	}
}

func TestApplyDefaultsDoesNotMaterializeLocalRetry(t *testing.T) {
	raw := strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      maxRetries: 2", 1)
	cfg, err := DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeAndValidate() error = %v", err)
	}
	retry := cfg.Providers["primary"].LocalRetry
	if retry == nil || retry.MaxRetries == nil || *retry.MaxRetries != 2 || retry.IntervalMs != nil {
		t.Fatalf("localRetry defaults were materialized or values changed: %#v", retry)
	}
	if cfg.Routes[0].LocalRetry != nil {
		t.Fatalf("未配置的 route.localRetry 被物化: %#v", cfg.Routes[0].LocalRetry)
	}
}
