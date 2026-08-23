package config

import (
	"strings"
	"testing"
)

// failoverConfigYAML 生成一份带自定义 routes / failover 片段的合法配置。
func failoverConfigYAML(routes, failover string) string {
	var b strings.Builder
	b.WriteString(`port: 7789
host: 127.0.0.1
timeout: 120000
streamActivityTimeout: 60000
cache:
  maxAgeDays: 7
  maxRecords: 1000
providers:
  primary:
    baseUrl: https://a.example.com
    apiKey: ""
    format: openai
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
  backup:
    baseUrl: https://b.example.com
    apiKey: ""
    format: anthropic
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
`)
	b.WriteString(routes)
	b.WriteString(failover)
	return b.String()
}

func TestRouteTargetsDecode(t *testing.T) {
	const routes = `routes:
  - match: "claude-*"
    targets:
      - provider: primary
        model: model-a
      - provider: backup
        model: model-b
`
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(routes, "")))
	if err != nil {
		t.Fatalf("解析 targets 配置失败: %v", err)
	}
	list := c.Routes[0].TargetList()
	if len(list) != 2 {
		t.Fatalf("候选数量 = %d, 期望 2", len(list))
	}
	if list[0].Provider != "primary" || list[0].Model != "model-a" {
		t.Errorf("首个候选 = %+v", list[0])
	}
	if list[1].Provider != "backup" || list[1].Model != "model-b" {
		t.Errorf("次个候选 = %+v", list[1])
	}
}

// TargetList 对单目标写法要退化成一个候选，且不得修改原 Route。
func TestTargetListSingleTargetFallback(t *testing.T) {
	r := &Route{Match: "*", Provider: "primary", Model: "model-a"}
	list := r.TargetList()
	if len(list) != 1 || list[0].Provider != "primary" || list[0].Model != "model-a" {
		t.Fatalf("单目标退化结果 = %+v", list)
	}
	if len(r.Targets) != 0 {
		t.Errorf("TargetList 不应写回 Targets, 实际 = %+v", r.Targets)
	}
}

// TargetList 返回的切片必须是拷贝，调用方改动不能影响配置本身。
func TestTargetListReturnsCopy(t *testing.T) {
	r := &Route{Match: "*", Targets: []Target{{Provider: "primary", Model: "model-a"}}}
	list := r.TargetList()
	list[0].Model = "tampered"
	if r.Targets[0].Model != "model-a" {
		t.Fatalf("TargetList 返回的切片与配置共享底层数组")
	}
}

func TestRouteTargetsValidation(t *testing.T) {
	cases := []struct {
		name    string
		routes  string
		wantErr string
	}{
		{
			name: "provider 与 targets 互斥",
			routes: `routes:
  - match: "*"
    provider: primary
    model: model-a
    targets:
      - provider: backup
        model: model-b
`,
			wantErr: "二者互斥",
		},
		{
			name: "两者都不写",
			routes: `routes:
  - match: "*"
    model: model-a
`,
			wantErr: "缺少 provider 或 targets",
		},
		{
			name: "targets 为空列表按缺失处理",
			routes: `routes:
  - match: "*"
    targets: []
`,
			wantErr: "缺少 provider 或 targets",
		},
		{
			name: "候选完全重复",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: model-a
      - provider: primary
        model: model-a
`,
			wantErr: "重复候选",
		},
		{
			name: "候选引用未定义 provider",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: model-a
      - provider: ghost
        model: model-b
`,
			wantErr: "未定义的 provider",
		},
		{
			name: "候选缺少 model",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: ""
`,
			wantErr: "缺少 model",
		},
		{
			name: "候选缺少 provider",
			routes: `routes:
  - match: "*"
    targets:
      - provider: ""
        model: model-a
`,
			wantErr: "缺少 provider",
		},
		{
			name: "候选超过上限",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: m1
      - provider: primary
        model: m2
      - provider: primary
        model: m3
      - provider: primary
        model: m4
      - provider: primary
        model: m5
      - provider: primary
        model: m6
`,
			wantErr: "最多 5 个候选",
		},
		{
			name: "targets 内未知字段被拒绝",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: model-a
        weight: 3
`,
			wantErr: "weight",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(failoverConfigYAML(tc.routes, "")))
			if err == nil {
				t.Fatalf("期望报错包含 %q, 实际通过校验", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// 同 provider 不同 model 是合法的降级写法，不能被当成重复候选。
func TestRouteTargetsSameProviderDifferentModel(t *testing.T) {
	const routes = `routes:
  - match: "*"
    targets:
      - provider: primary
        model: expensive
      - provider: primary
        model: cheap
`
	if _, err := DecodeAndValidate([]byte(failoverConfigYAML(routes, ""))); err != nil {
		t.Fatalf("同 provider 不同 model 应合法: %v", err)
	}
}

const singleRoute = `routes:
  - match: "*"
    provider: primary
    model: model-a
`

func TestFailoverDefaults(t *testing.T) {
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(singleRoute, "")))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	f := &c.Failover
	if f.Enabled {
		t.Error("failover 默认应关闭")
	}
	if f.MaxAttempts != 2 {
		t.Errorf("maxAttempts = %d, 期望 2", f.MaxAttempts)
	}
	if f.MaxRetryAfterMs != 5000 {
		t.Errorf("maxRetryAfterMs = %d, 期望 5000", f.MaxRetryAfterMs)
	}
	if !f.TransferOnTransportError() {
		t.Error("onTransportError 默认应为 true")
	}
	if !f.TransferOnServerError() {
		t.Error("onServerError 默认应为 true")
	}
	if !f.TransferOnRateLimit() {
		t.Error("onRateLimit 默认应为 true")
	}
	if !f.TransferOnQueueTimeout() {
		t.Error("onQueueTimeout 默认应为 true")
	}
	if !f.TransferOnStreamHeaderTimeout() {
		t.Error("onStreamHeaderTimeout 默认应为 true")
	}
	if f.TransferOnAuthError() {
		t.Error("onAuthError 默认应为 false")
	}
}

// 核心回归：用户显式写 false 不能被 applyDefaults 静默改回 true（LiteLLM #32425 那类 bug）。
func TestFailoverExplicitFalseNotOverridden(t *testing.T) {
	const failover = `failover:
  enabled: true
  onTransportError: false
  onServerError: false
  onRateLimit: false
  onQueueTimeout: false
  onStreamHeaderTimeout: false
`
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(singleRoute, failover)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	f := &c.Failover
	checks := []struct {
		name string
		got  bool
	}{
		{"onTransportError", f.TransferOnTransportError()},
		{"onServerError", f.TransferOnServerError()},
		{"onRateLimit", f.TransferOnRateLimit()},
		{"onQueueTimeout", f.TransferOnQueueTimeout()},
		{"onStreamHeaderTimeout", f.TransferOnStreamHeaderTimeout()},
	}
	for _, c := range checks {
		if c.got {
			t.Errorf("%s 显式 false 被覆盖成 true", c.name)
		}
	}
}

// 显式 true 也要保留（onAuthError 默认 false，显式打开必须生效）。
func TestFailoverExplicitTrueRetained(t *testing.T) {
	const failover = `failover:
  enabled: true
  onAuthError: true
`
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(singleRoute, failover)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !c.Failover.TransferOnAuthError() {
		t.Error("onAuthError 显式 true 未生效")
	}
}

func TestBoolOr(t *testing.T) {
	yes, no := true, false
	if got := BoolOr(nil, true); !got {
		t.Error("nil + def=true 应返回 true")
	}
	if got := BoolOr(nil, false); got {
		t.Error("nil + def=false 应返回 false")
	}
	if got := BoolOr(&no, true); got {
		t.Error("显式 false 应压过 def=true")
	}
	if got := BoolOr(&yes, false); !got {
		t.Error("显式 true 应压过 def=false")
	}
}

func TestFailoverNumericBounds(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"maxAttempts 为负", "failover:\n  maxAttempts: -1\n", "maxAttempts"},
		{"maxAttempts 超上限", "failover:\n  maxAttempts: 6\n", "maxAttempts"},
		{"maxRetryAfterMs 为负", "failover:\n  maxRetryAfterMs: -1\n", "maxRetryAfterMs"},
		{"maxRetryAfterMs 超上限", "failover:\n  maxRetryAfterMs: 60001\n", "maxRetryAfterMs"},
		{"未知字段", "failover:\n  onFooError: true\n", "onFooError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(failoverConfigYAML(singleRoute, tc.body)))
			if err == nil {
				t.Fatalf("期望报错包含 %q, 实际通过校验", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestFailoverAcceptsExactBounds(t *testing.T) {
	for _, body := range []string{
		"failover:\n  maxAttempts: 1\n  maxRetryAfterMs: 60000\n",
		"failover:\n  maxAttempts: 5\n  maxRetryAfterMs: 1\n",
	} {
		if _, err := DecodeAndValidate([]byte(failoverConfigYAML(singleRoute, body))); err != nil {
			t.Fatalf("边界值应合法 (%s): %v", body, err)
		}
	}
}
