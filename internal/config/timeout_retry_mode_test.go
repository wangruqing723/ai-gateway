package config

import (
	"strings"
	"testing"
)

func TestDecodeAndValidateRetryModesAndEffectiveIntervals(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "provider cannot configure retry mode",
			raw:  strings.Replace(validConfigYAML(), "    maxQueueWait: 30000", "    maxQueueWait: 30000\n    localRetry:\n      mode: untilDisconnect", 1),
			want: "providers.primary.localRetry.mode 仅可在 route 或 target 上配置",
		},
		{
			name: "unknown route mode is rejected",
			raw:  strings.Replace(validConfigYAML(), "    model: upstream-model", "    model: upstream-model\n    localRetry:\n      mode: forever", 1),
			want: `route "*".localRetry.mode 须为 limited 或 untilDisconnect`,
		},
		{
			name: "continuous mode rejects an effective zero interval",
			raw:  strings.Replace(validConfigYAML(), "    model: upstream-model", "    model: upstream-model\n    localRetry:\n      mode: untilDisconnect\n      intervalMs: 0", 1),
			want: `route "*".localRetry.intervalMs 在 untilDisconnect 模式下须大于 0`,
		},
		{
			name: "target limited override allows inherited zero interval",
			raw: strings.Replace(validConfigYAML(),
				"  - match: \"*\"\n    provider: primary\n    model: upstream-model",
				"  - match: \"*\"\n    timeoutMode: client\n    localRetry:\n      mode: untilDisconnect\n      intervalMs: 0\n    targets:\n      - provider: primary\n        model: upstream-model\n        timeoutMode: global\n        localRetry:\n          mode: limited", 1),
		},
		{
			name: "target continuous mode requires positive effective interval",
			raw: strings.Replace(validConfigYAML(),
				"  - match: \"*\"\n    provider: primary\n    model: upstream-model",
				"  - match: \"*\"\n    localRetry:\n      intervalMs: 0\n    targets:\n      - provider: primary\n        model: upstream-model\n        localRetry:\n          mode: untilDisconnect", 1),
			want: `route "*".targets[0].localRetry.intervalMs 在 untilDisconnect 模式下须大于 0`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(tt.raw))
			if tt.want == "" {
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

func TestDecodeAndValidateTimeoutModes(t *testing.T) {
	for _, mode := range []string{"global", "client"} {
		t.Run(mode, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(),
				"  - match: \"*\"\n    provider: primary\n    model: upstream-model",
				"  - match: \"*\"\n    timeoutMode: "+mode+"\n    targets:\n      - provider: primary\n        model: upstream-model\n        timeoutMode: "+mode, 1)
			if _, err := DecodeAndValidate([]byte(raw)); err != nil {
				t.Fatalf("DecodeAndValidate() error = %v", err)
			}
		})
	}
	raw := strings.Replace(validConfigYAML(), "    model: upstream-model", "    model: upstream-model\n    timeoutMode: local", 1)
	if _, err := DecodeAndValidate([]byte(raw)); err == nil || !strings.Contains(err.Error(), `route "*".timeoutMode 须为 global 或 client`) {
		t.Fatalf("invalid timeoutMode error = %v", err)
	}
}

func TestTimeoutAndRetryModesSurviveTransferRoundTrip(t *testing.T) {
	source, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatalf("DecodeAndValidate(source) error = %v", err)
	}
	route := &source.Routes[0]
	route.Provider = ""
	route.Model = ""
	route.TimeoutMode = TimeoutModeClient
	route.LocalRetry = &LocalRetry{Mode: LocalRetryModeUntilDisconnect, MaxRetries: intPtr(3), IntervalMs: intPtr(2)}
	route.Targets = []Target{
		{
			Provider: "primary", Model: "first", TimeoutMode: TimeoutModeGlobal,
			LocalRetry: &LocalRetry{Mode: LocalRetryModeLimited, MaxRetries: intPtr(0), IntervalMs: intPtr(2)},
		},
		{
			Provider: "primary", Model: "second", TimeoutMode: TimeoutModeClient,
			LocalRetry: &LocalRetry{Mode: LocalRetryModeUntilDisconnect, MaxRetries: intPtr(1), IntervalMs: intPtr(3)},
		},
	}

	encoded, err := ExportTransfer(source, TransferExportRequest{Scope: "all"})
	if err != nil {
		t.Fatalf("ExportTransfer() error = %v", err)
	}
	target, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatalf("DecodeAndValidate(target) error = %v", err)
	}
	plan, err := PlanTransfer(target, encoded, TransferOptions{Mode: "replace"}, nil)
	if err != nil {
		t.Fatalf("PlanTransfer() error = %v", err)
	}
	if !plan.CanApply || plan.Config == nil || len(plan.Config.Routes) != 1 {
		t.Fatalf("导入计划不可应用或路由数量错误: canApply=%v config=%+v", plan.CanApply, plan.Config)
	}
	got := plan.Config.Routes[0]
	if got.TimeoutMode != TimeoutModeClient || got.LocalRetry == nil || got.LocalRetry.Mode != LocalRetryModeUntilDisconnect {
		t.Fatalf("route modes were not preserved: timeoutMode=%q localRetry=%+v", got.TimeoutMode, got.LocalRetry)
	}
	if len(got.Targets) != 2 || got.Targets[0].TimeoutMode != TimeoutModeGlobal || got.Targets[0].LocalRetry == nil || got.Targets[0].LocalRetry.Mode != LocalRetryModeLimited || got.Targets[1].TimeoutMode != TimeoutModeClient || got.Targets[1].LocalRetry == nil || got.Targets[1].LocalRetry.Mode != LocalRetryModeUntilDisconnect {
		t.Fatalf("target modes were not preserved: %+v", got.Targets)
	}
	if got.Targets[0].LocalRetry.MaxRetries == nil || *got.Targets[0].LocalRetry.MaxRetries != 0 {
		t.Fatalf("显式 maxRetries: 0 未被导出/导入保留: %+v", got.Targets[0].LocalRetry)
	}
}
