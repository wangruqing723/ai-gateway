package router

import (
	"net/http"
	"testing"

	"ai-gateway/internal/config"
)

// baseConfig 构造带三个 provider 的最小配置，供候选顺序与隔离测试复用。
func baseConfig() *config.Config {
	return &config.Config{
		Providers: map[string]*config.Provider{
			"alpha": {Name: "alpha", BaseURL: "https://alpha.example.com", Format: "anthropic"},
			"beta":  {Name: "beta", BaseURL: "https://beta.example.com", Format: "openai"},
			"gamma": {Name: "gamma", BaseURL: "https://gamma.example.com", Format: "openai"},
		},
	}
}

func TestMatchRouteSingleTargetFallsBackToProviderModel(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "claude-*", Provider: "alpha", Model: "upstream-a"}}

	m := MatchRoute("claude-opus-4", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	if len(m.Candidates) != 1 {
		t.Fatalf("候选数 = %d, 期望 1", len(m.Candidates))
	}
	if got := m.Candidates[0].Provider.Name; got != "alpha" {
		t.Errorf("候选 provider = %q, 期望 alpha", got)
	}
	if got := m.Candidates[0].TargetModel; got != "upstream-a" {
		t.Errorf("候选 model = %q, 期望 upstream-a", got)
	}
	if m.RouteMatch != "claude-*" {
		t.Errorf("RouteMatch = %q, 期望 claude-*", m.RouteMatch)
	}
}

func TestMatchRouteCandidateOrderFollowsTargets(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{
		Match: "*",
		Targets: []config.Target{
			{Provider: "gamma", Model: "m-gamma"},
			{Provider: "alpha", Model: "m-alpha"},
			{Provider: "beta", Model: "m-beta"},
		},
	}}

	m := MatchRoute("any-model", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	wantProviders := []string{"gamma", "alpha", "beta"}
	wantModels := []string{"m-gamma", "m-alpha", "m-beta"}
	if len(m.Candidates) != len(wantProviders) {
		t.Fatalf("候选数 = %d, 期望 %d", len(m.Candidates), len(wantProviders))
	}
	for i, want := range wantProviders {
		if got := m.Candidates[i].Provider.Name; got != want {
			t.Errorf("候选[%d] provider = %q, 期望 %q（顺序必须与 targets 一致）", i, got, want)
		}
		if got := m.Candidates[i].TargetModel; got != wantModels[i] {
			t.Errorf("候选[%d] model = %q, 期望 %q", i, got, wantModels[i])
		}
	}
}

func TestMatchRouteSameProviderDifferentModelsAllowed(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{
		Match: "*",
		Targets: []config.Target{
			{Provider: "alpha", Model: "expensive"},
			{Provider: "alpha", Model: "cheap"},
		},
	}}

	m := MatchRoute("x", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	if len(m.Candidates) != 2 {
		t.Fatalf("候选数 = %d, 期望 2（同 provider 不同 model 是降级场景）", len(m.Candidates))
	}
	if m.Candidates[0].Provider == m.Candidates[1].Provider {
		t.Error("同 provider 的两个候选必须各自持有独立拷贝，实际指向同一结构体")
	}
	if m.Candidates[0].TargetModel != "expensive" || m.Candidates[1].TargetModel != "cheap" {
		t.Errorf("候选 model 顺序错误: %q, %q", m.Candidates[0].TargetModel, m.Candidates[1].TargetModel)
	}
}

func TestMatchRouteProviderCopyIsolatesAPIKeyWrite(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{
		Match: "*",
		Targets: []config.Target{
			{Provider: "alpha", Model: "m1"},
			{Provider: "beta", Model: "m2"},
		},
	}}

	first := MatchRoute("x", cfg)
	second := MatchRoute("x", cfg)
	if first == nil || second == nil {
		t.Fatal("期望两次都命中路由")
	}

	// 模拟 handle 里把请求头 key 写进候选 Provider 的行为
	first.Candidates[0].Provider.APIKey = "sk-from-request-1"

	if got := cfg.Providers["alpha"].APIKey; got != "" {
		t.Errorf("写候选后原始配置被污染: APIKey = %q, 期望空", got)
	}
	if got := second.Candidates[0].Provider.APIKey; got != "" {
		t.Errorf("写候选后另一次匹配结果被污染: APIKey = %q, 期望空", got)
	}
	if first.Candidates[0].Provider == cfg.Providers["alpha"] {
		t.Error("候选 Provider 必须是值拷贝，实际与配置共享指针")
	}
}

func TestMatchRouteEmptyTargetModelInheritsRequestModel(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "*", Provider: "alpha"}}

	m := MatchRoute("requested-model", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	if got := m.Candidates[0].TargetModel; got != "requested-model" {
		t.Errorf("空 model 应沿用请求模型名，实际 = %q", got)
	}
}

func TestMatchRouteEmptyTargetModelStripsOneMSuffix(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "claude-sonnet-4-5", Provider: "alpha"}}

	m := MatchRoute("claude-sonnet-4-5 [1M]", cfg)
	if m == nil {
		t.Fatal("带 [1M] 后缀的精确模型名应命中路由")
	}
	if got := m.Candidates[0].TargetModel; got != "claude-sonnet-4-5" {
		t.Fatalf("TargetModel = %q，期望剥离 [1M] 后缀", got)
	}
	if got := m.Candidates[0].ContextWindow; got == nil || *got != OneMContextWindow {
		t.Fatalf("ContextWindow = %#v，期望 %d", got, OneMContextWindow)
	}
}

func TestStripOneMSuffix(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		want   string
		marked bool
	}{
		{name: "大写", model: "deepseek-v4-pro[1M]", want: "deepseek-v4-pro", marked: true},
		{name: "带空格", model: "deepseek-v4-pro [1m]", want: "deepseek-v4-pro", marked: true},
		{name: "无标记", model: "deepseek-v4-pro", want: "deepseek-v4-pro"},
		{name: "标记不是尾部", model: "deepseek-v4-pro[1m]-x", want: "deepseek-v4-pro[1m]-x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, marked := StripOneMSuffix(tt.model)
			if got != tt.want || marked != tt.marked {
				t.Fatalf("StripOneMSuffix(%q) = %q, %v，期望 %q, %v", tt.model, got, marked, tt.want, tt.marked)
			}
		})
	}
}

func TestMatchRouteContextWindowPriority(t *testing.T) {
	providerWindow := 100000
	routeWindow := 200000
	targetWindow := 300000
	tests := []struct {
		name     string
		target   *int
		route    *int
		provider *int
		want     *int
	}{
		{name: "target 覆盖 route", target: &targetWindow, route: &routeWindow, provider: &providerWindow, want: &targetWindow},
		{name: "route 覆盖 provider", route: &routeWindow, provider: &providerWindow, want: &routeWindow},
		{name: "仅 provider", provider: &providerWindow, want: &providerWindow},
		{name: "全部未配置", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Providers["alpha"].ContextWindow = tt.provider
			cfg.Routes = []config.Route{{
				Match: "*", Provider: "alpha", Model: "upstream",
				ContextWindow: tt.route,
				Targets:       nil,
			}}
			if tt.target != nil {
				cfg.Routes[0].Targets = []config.Target{{Provider: "alpha", Model: "upstream", ContextWindow: tt.target}}
				cfg.Routes[0].Provider = ""
				cfg.Routes[0].Model = ""
			}
			m := MatchRoute("requested", cfg)
			if m == nil {
				t.Fatal("期望命中路由")
			}
			got := m.Candidates[0].ContextWindow
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ContextWindow = %d，期望 nil", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("ContextWindow = %#v，期望 %d", got, *tt.want)
			}
		})
	}
}

func TestOneMSuffixOverridesContextWindowButNotMaxTokens(t *testing.T) {
	window := 200000
	maxTokens := 8192
	cfg := baseConfig()
	cfg.Providers["alpha"].ContextWindow = &window
	cfg.Providers["alpha"].MaxTokens = &maxTokens
	cfg.Routes = []config.Route{{Match: "model", Provider: "alpha", Model: "upstream"}}

	m := MatchRoute("model[1m]", cfg)
	if m == nil {
		t.Fatal("带后缀的模型应命中精确路由")
	}
	if got := m.Candidates[0].ContextWindow; got == nil || *got != OneMContextWindow {
		t.Fatalf("ContextWindow = %#v，期望 %d", got, OneMContextWindow)
	}
	if got := m.Candidates[0].MaxTokens; got == nil || *got != maxTokens {
		t.Fatalf("MaxTokens = %#v，期望保持 %d", got, maxTokens)
	}
}

func TestMatchRouteSkipsUndefinedProviderAndFallsThrough(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{
		{Match: "*", Targets: []config.Target{{Provider: "missing", Model: "m"}}},
		{Match: "*", Provider: "beta", Model: "m-beta"},
	}

	m := MatchRoute("x", cfg)
	if m == nil {
		t.Fatal("首条路由候选全部无效时应继续匹配后续路由，实际为 nil")
	}
	if got := m.Candidates[0].Provider.Name; got != "beta" {
		t.Errorf("命中 provider = %q, 期望 beta", got)
	}
}

func TestMatchRouteVisionProviderIsCopied(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{
		Match:    "*",
		Provider: "alpha",
		Model:    "m",
		Vision:   &config.Vision{Provider: "beta", Model: "vision-model"},
	}}

	m := MatchRoute("x", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	if m.VisionProvider == nil {
		t.Fatal("VisionProvider 为 nil，期望非 nil")
	}
	if m.VisionProvider == cfg.Providers["beta"] {
		t.Error("VisionProvider 必须是值拷贝，实际与配置共享指针")
	}
	if m.VisionModel != "vision-model" {
		t.Errorf("VisionModel = %q, 期望 vision-model", m.VisionModel)
	}
}

func TestMatchRouteNoMatchReturnsNil(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "gpt-*", Provider: "alpha", Model: "m"}}

	if m := MatchRoute("claude-opus", cfg); m != nil {
		t.Errorf("未命中时应返回 nil，实际 = %+v", m)
	}
}

func TestMatchRouteCaseInsensitive(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "Claude-*", Provider: "alpha", Model: "m"}}

	if m := MatchRoute("CLAUDE-opus-4", cfg); m == nil {
		t.Error("路由匹配应大小写不敏感，实际未命中")
	}
}

func TestResolveAPIKeyWithSource(t *testing.T) {
	tests := []struct {
		name       string
		provider   *config.Provider
		headers    map[string]string
		wantKey    string
		wantSource string
	}{
		{
			name:       "provider 配置优先",
			provider:   &config.Provider{APIKey: "sk-provider"},
			headers:    map[string]string{"x-api-key": "sk-header"},
			wantKey:    "sk-provider",
			wantSource: "provider",
		},
		{
			name:       "回落到 x-api-key",
			provider:   &config.Provider{},
			headers:    map[string]string{"x-api-key": "sk-header"},
			wantKey:    "sk-header",
			wantSource: "x-api-key",
		},
		{
			name:       "回落到 Bearer",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "Bearer sk-bearer"},
			wantKey:    "sk-bearer",
			wantSource: "authorization",
		},
		{
			name:       "Bearer 大小写不敏感",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "BEARER sk-upper"},
			wantKey:    "sk-upper",
			wantSource: "authorization",
		},
		{
			name:       "无前缀的 authorization 原样取用",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "sk-raw"},
			wantKey:    "sk-raw",
			wantSource: "authorization",
		},
		{
			name:       "全部缺失",
			provider:   &config.Provider{},
			headers:    nil,
			wantKey:    "",
			wantSource: "none",
		},
		{
			// 现有行为：先 TrimSpace 再判前缀，"Bearer " 会被收缩成 "Bearer"，
			// 不再匹配 "bearer " 前缀，于是整体当作不带前缀的 key 取用。
			name:       "只有 Bearer 前缀无内容时整体当作 key",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "Bearer "},
			wantKey:    "Bearer",
			wantSource: "authorization",
		},
		{
			name:       "Bearer 前缀后为空格填充",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "Bearer  "},
			wantKey:    "Bearer",
			wantSource: "authorization",
		},
		{
			name:       "authorization 仅空白视为缺失",
			provider:   &config.Provider{},
			headers:    map[string]string{"authorization": "   "},
			wantKey:    "",
			wantSource: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			gotKey, gotSource := ResolveAPIKeyWithSource(tt.provider, h)
			if gotKey != tt.wantKey {
				t.Errorf("key = %q, 期望 %q", gotKey, tt.wantKey)
			}
			if gotSource != tt.wantSource {
				t.Errorf("source = %q, 期望 %q", gotSource, tt.wantSource)
			}
			if plain := ResolveAPIKey(tt.provider, h); plain != tt.wantKey {
				t.Errorf("ResolveAPIKey = %q, 期望 %q", plain, tt.wantKey)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		{"*", "anything", true},
		{"claude-*", "claude-opus-4", true},
		{"claude-*", "gpt-4", false},
		{"gpt-?", "gpt-4", true},
		{"gpt-?", "gpt-40", false},
		{"*-turbo", "gpt-4-turbo", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"**", "anything", true},
		{"", "", true},
		{"", "x", false},
	}

	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.input); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, 期望 %v", tt.pattern, tt.input, got, tt.want)
		}
	}
}

// TestMatchRouteMaxTokensPrecedence 验证 maxTokens 三层优先级：target > route > provider。
func TestMatchRouteMaxTokensPrecedence(t *testing.T) {
	intp := func(v int) *int { return &v }

	tests := []struct {
		name         string
		providerMax  *int
		routeMax     *int
		targetMax    *int
		wantResolved *int
	}{
		{name: "三层都没配则不覆盖"},
		{name: "只有 provider", providerMax: intp(8192), wantResolved: intp(8192)},
		{name: "route 盖 provider", providerMax: intp(8192), routeMax: intp(16384), wantResolved: intp(16384)},
		{name: "target 盖 route 与 provider", providerMax: intp(8192), routeMax: intp(16384), targetMax: intp(32768), wantResolved: intp(32768)},
		{name: "target 盖 provider（无 route）", providerMax: intp(8192), targetMax: intp(65536), wantResolved: intp(65536)},
		{name: "只有 route", routeMax: intp(4096), wantResolved: intp(4096)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Providers["alpha"].MaxTokens = tt.providerMax
			cfg.Routes = []config.Route{{
				Match:     "claude-*",
				MaxTokens: tt.routeMax,
				Targets: []config.Target{
					{Provider: "alpha", Model: "upstream-a", MaxTokens: tt.targetMax},
				},
			}}

			m := MatchRoute("claude-opus-4", cfg)
			if m == nil {
				t.Fatal("期望命中路由，实际为 nil")
			}
			got := m.Candidates[0].MaxTokens
			switch {
			case tt.wantResolved == nil && got != nil:
				t.Fatalf("MaxTokens = %d，期望 nil（不覆盖）", *got)
			case tt.wantResolved != nil && got == nil:
				t.Fatalf("MaxTokens = nil，期望 %d", *tt.wantResolved)
			case tt.wantResolved != nil && *got != *tt.wantResolved:
				t.Fatalf("MaxTokens = %d，期望 %d", *got, *tt.wantResolved)
			}
		})
	}
}

// 单目标写法（provider + model，无 targets）下 route 级 maxTokens 必须生效——
// 那是单目标路由唯一能设该值的地方。
func TestMatchRouteSingleTargetUsesRouteMaxTokens(t *testing.T) {
	value := 16384
	cfg := baseConfig()
	cfg.Routes = []config.Route{{Match: "claude-*", Provider: "alpha", Model: "upstream-a", MaxTokens: &value}}

	m := MatchRoute("claude-opus-4", cfg)
	if m == nil {
		t.Fatal("期望命中路由，实际为 nil")
	}
	got := m.Candidates[0].MaxTokens
	if got == nil || *got != 16384 {
		t.Fatalf("单目标路由 MaxTokens = %v，期望 16384", got)
	}
}
