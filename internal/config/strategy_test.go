package config

import (
	"strings"
	"testing"
)

// twoTargetRoute 复用 failoverConfigYAML 的 primary / backup 两个 provider，
// strategy 由调用方拼在后面（缩进按 route 层级对齐）。
func twoTargetRoute(strategy string) string {
	routes := `routes:
  - match: "*"
    targets:
      - provider: primary
        model: model-a
      - provider: backup
        model: model-b
`
	if strategy != "" {
		routes += "    strategy: " + strategy + "\n"
	}
	return routes
}

// singleTargetRoute 单候选路由，用于验证「策略写了但不会生效」的报错。
func singleTargetRoute(strategy string) string {
	routes := `routes:
  - match: "*"
    provider: primary
    model: model-a
`
	if strategy != "" {
		routes += "    strategy: " + strategy + "\n"
	}
	return routes
}

// 三个合法取值在多候选路由上都要能解析出来，且原样落到 Route.Strategy。
func TestRouteStrategyAccepted(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"不写 strategy", ""},
		{"failover", "failover"},
		{"round-robin", "round-robin"},
		{"least-queue", "least-queue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := DecodeAndValidate([]byte(failoverConfigYAML(twoTargetRoute(tc.want), "")))
			if err != nil {
				t.Fatalf("strategy=%q 应合法: %v", tc.want, err)
			}
			if got := c.Routes[0].Strategy; got != tc.want {
				t.Errorf("Route.Strategy = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestRouteStrategyRejected(t *testing.T) {
	cases := []struct {
		name    string
		routes  string
		wantErr string
	}{
		{
			name:    "未知取值",
			routes:  twoTargetRoute("weighted"),
			wantErr: "须为",
		},
		{
			name:    "大小写不匹配也算未知取值",
			routes:  twoTargetRoute("Round-Robin"),
			wantErr: "须为",
		},
		{
			name:    "单候选 + round-robin",
			routes:  singleTargetRoute("round-robin"),
			wantErr: "只有一个候选",
		},
		{
			name:    "单候选 + least-queue",
			routes:  singleTargetRoute("least-queue"),
			wantErr: "只有一个候选",
		},
		{
			name: "单元素 targets + round-robin 同样报错",
			routes: `routes:
  - match: "*"
    targets:
      - provider: primary
        model: model-a
    strategy: round-robin
`,
			wantErr: "只有一个候选",
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

// 单候选路由上显式写 failover 是合法的：它表达的就是「按配置顺序」，
// 与不写等价，不该因为「只有一个候选」被拒。
func TestRouteStrategySingleCandidateFailoverAccepted(t *testing.T) {
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(singleTargetRoute("failover"), "")))
	if err != nil {
		t.Fatalf("单候选 + failover 应合法: %v", err)
	}
	if c.Routes[0].Strategy != "failover" {
		t.Errorf("Route.Strategy = %q, 期望 failover", c.Routes[0].Strategy)
	}
}

// strategy 与 failover.enabled 正交：关掉 failover 仍可用 round-robin 做纯分流。
func TestRouteStrategyIndependentOfFailoverEnabled(t *testing.T) {
	const failover = `failover:
  enabled: false
`
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(twoTargetRoute("round-robin"), failover)))
	if err != nil {
		t.Fatalf("round-robin + failover 关闭应合法: %v", err)
	}
	if c.Failover.Enabled {
		t.Error("failover.enabled 应保持 false")
	}
	if c.Routes[0].Strategy != "round-robin" {
		t.Errorf("Route.Strategy = %q, 期望 round-robin", c.Routes[0].Strategy)
	}
}

// 每条路由各自带策略，互不影响。
func TestRouteStrategyPerRoute(t *testing.T) {
	const routes = `routes:
  - match: "claude-*"
    targets:
      - provider: primary
        model: model-a
      - provider: backup
        model: model-b
    strategy: round-robin
  - match: "*"
    targets:
      - provider: primary
        model: model-c
      - provider: backup
        model: model-d
    strategy: least-queue
`
	c, err := DecodeAndValidate([]byte(failoverConfigYAML(routes, "")))
	if err != nil {
		t.Fatalf("多路由各带策略应合法: %v", err)
	}
	if len(c.Routes) != 2 {
		t.Fatalf("路由数量 = %d, 期望 2", len(c.Routes))
	}
	if c.Routes[0].Strategy != "round-robin" {
		t.Errorf("Routes[0].Strategy = %q, 期望 round-robin", c.Routes[0].Strategy)
	}
	if c.Routes[1].Strategy != "least-queue" {
		t.Errorf("Routes[1].Strategy = %q, 期望 least-queue", c.Routes[1].Strategy)
	}
}
