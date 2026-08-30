package providerhealth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func testResolver(client *http.Client) ClientResolver {
	return func(string) (*http.Client, error) { return client, nil }
}

func TestCheckAllMapsStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"ok":  {Name: "ok", BaseURL: server.URL + "/v1", APIKey: "good", Format: "openai"},
		"bad": {Name: "bad", BaseURL: server.URL, APIKey: "bad", Format: "openai"},
	}}

	statuses := NewChecker().CheckAll(context.Background(), cfg, testResolver(server.Client()))
	if statuses["ok"].Status != "ok" {
		t.Fatalf("expected ok status, got %#v", statuses["ok"])
	}
	if statuses["bad"].Status != "error" || statuses["bad"].HTTPCode != http.StatusUnauthorized {
		t.Fatalf("expected auth error, got %#v", statuses["bad"])
	}
}

// 健康检测探测 /v1/models，而部分上游按 User-Agent 做准入（实测 agentrouter.org
// 在 Go 默认 UA 下返回 401）。不带配置的 UA 会把这类 provider 永久判成不健康，
// 而它的转发路径其实是好的——那是误判，不是真故障。
func TestCheckSendsConfiguredUserAgent(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("authorization")] = r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"gated": {Name: "gated", BaseURL: server.URL, APIKey: "k-gated", Format: "openai",
			UserAgent: "claude-cli/2.1.161 (external, cli)"},
		"plain": {Name: "plain", BaseURL: server.URL, APIKey: "k-plain", Format: "openai"},
	}}

	if statuses := NewChecker().CheckAll(context.Background(), cfg, testResolver(server.Client())); len(statuses) != 2 {
		t.Fatalf("statuses = %#v", statuses)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := seen["Bearer k-gated"]; got != "claude-cli/2.1.161 (external, cli)" {
		t.Fatalf("configured provider User-Agent = %q, want the configured value", got)
	}
	// 未配置时不设该头，由 Go 填默认值——不能是空字符串（那等于把头删掉）。
	if got := seen["Bearer k-plain"]; got != "Go-http-client/1.1" {
		t.Fatalf("unconfigured provider User-Agent = %q, want Go default", got)
	}
}

// CheckProvider 只探被点的那一个，且结果要进缓存（Snapshot 能读到），
// 其余 provider 保持未检测——整表检测在 provider 多时太慢，这是单点检测存在的理由。
func TestCheckProviderChecksOnlyRequestedProvider(t *testing.T) {
	var mu sync.Mutex
	probed := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probed[r.Header.Get("authorization")]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"a": {Name: "a", BaseURL: server.URL, APIKey: "key-a", Format: "openai"},
		"b": {Name: "b", BaseURL: server.URL, APIKey: "key-b", Format: "openai"},
	}}

	checker := NewChecker()
	status, ok := checker.CheckProvider(context.Background(), cfg, testResolver(server.Client()), "a")
	if !ok {
		t.Fatal("CheckProvider() ok = false, want true for existing provider")
	}
	if status.Status != "ok" {
		t.Fatalf("status = %#v, want ok", status)
	}

	mu.Lock()
	countB := probed["Bearer key-b"]
	mu.Unlock()
	if countB != 0 {
		t.Fatalf("provider b probed %d times, want 0", countB)
	}

	snap := checker.Snapshot(cfg)
	if snap["a"].Status != "ok" {
		t.Fatalf("snapshot for a = %#v, want cached ok", snap["a"])
	}
	if snap["b"].Status != "unchecked" {
		t.Fatalf("snapshot for b = %#v, want unchecked", snap["b"])
	}
}

// 未知名字必须报 false，让 HTTP 层能回 404 而不是静默当成一次成功检测。
func TestCheckProviderReportsMissingProvider(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.Provider{}}
	if _, ok := NewChecker().CheckProvider(context.Background(), cfg, testResolver(http.DefaultClient), "nope"); ok {
		t.Fatal("CheckProvider() ok = true for unknown provider, want false")
	}
}

// 单点检测不受整表检测的冷却影响：用户对着一行连点两次，第二次也要真探。
// CheckAll 的冷却是为「整表刷新」设的，套到单行上会让界面毫无反应。
func TestCheckProviderIgnoresCheckAllCooldown(t *testing.T) {
	var probes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"a": {Name: "a", BaseURL: server.URL, APIKey: "key-a", Format: "openai"},
	}}

	checker := NewChecker()
	checker.CheckAll(context.Background(), cfg, testResolver(server.Client()))
	before := probes.Load()

	// 紧接着两次单点检测，都应真的发出请求
	for i := 0; i < 2; i++ {
		if _, ok := checker.CheckProvider(context.Background(), cfg, testResolver(server.Client()), "a"); !ok {
			t.Fatalf("CheckProvider() call %d ok = false", i+1)
		}
	}
	if added := probes.Load() - before; added != 2 {
		t.Fatalf("单点检测发出 %d 次探测，want 2（不应被 CheckAll 冷却挡住）", added)
	}
}

func TestSnapshotInvalidatesStatusWhenProviderIdentityChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	original := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, APIKey: "key-a", Format: "openai"},
	}}
	checker.CheckAll(context.Background(), original, testResolver(server.Client()))

	changed := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: "https://changed.invalid", APIKey: "key-b", Format: "anthropic"},
	}}
	status := checker.Snapshot(changed)["p"]
	if status.Status != "unchecked" {
		t.Fatalf("changed provider must not inherit stale health: %#v", status)
	}
}

func TestInvalidateChangedRemovesDeletedAndChangedProviders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	oldCfg := &config.Config{Providers: map[string]*config.Provider{
		"changed": {Name: "changed", BaseURL: server.URL, APIKey: "a", Format: "openai"},
		"deleted": {Name: "deleted", BaseURL: server.URL, APIKey: "b", Format: "openai"},
	}}
	checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
	newCfg := &config.Config{Providers: map[string]*config.Provider{
		"changed": {Name: "changed", BaseURL: server.URL + "/other", APIKey: "a", Format: "openai"},
	}}
	checker.InvalidateChanged(oldCfg, newCfg)
	if status := checker.Snapshot(newCfg)["changed"]; status.Status != "unchecked" {
		t.Fatalf("changed provider must be invalidated: %#v", status)
	}
}

func TestInvalidateChangedKeepsCooldownForSameProviderFingerprint(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, APIKey: "key", Format: "openai"},
	}}
	checker.CheckAll(context.Background(), cfg, testResolver(server.Client()))
	checker.InvalidateChanged(cfg, cfg)
	checker.CheckAll(context.Background(), cfg, testResolver(server.Client()))

	if got := calls.Load(); got != 1 {
		t.Fatalf("unchanged provider fingerprint must keep the completed-round cooldown, got %d probes", got)
	}
}

func TestChangedFingerprintBypassesCooldownWithoutLeakingSecret(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	oldCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, APIKey: "old-key", Format: "openai"},
	}}
	checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
	const newSecret = "unique-health-secret-7f3a"
	newCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, APIKey: newSecret, Format: "openai"},
	}}
	checker.InvalidateChanged(oldCfg, newCfg)
	statuses := checker.CheckAll(context.Background(), newCfg, testResolver(server.Client()))
	if got := calls.Load(); got != 2 {
		t.Fatalf("changed provider fingerprint must bypass cooldown, got %d probes", got)
	}
	if statuses["p"].Status != "ok" {
		t.Fatalf("changed provider status = %#v", statuses["p"])
	}
	visible, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(visible), newSecret) {
		t.Fatalf("visible health status leaked provider API key: %s", visible)
	}
}

func TestProviderFingerprintCoversUserAgentAndProxy(t *testing.T) {
	base := &config.Provider{
		Name: "p", BaseURL: "https://api.example.com", Format: "openai", APIKey: "secret",
		UserAgent: "client-a", Proxy: "http://first-proxy.example:7890",
	}
	baseFingerprint := providerFingerprint("p", base)
	for _, changed := range []*config.Provider{
		{Name: "p", BaseURL: base.BaseURL, Format: base.Format, APIKey: base.APIKey, UserAgent: "client-b", Proxy: base.Proxy},
		{Name: "p", BaseURL: base.BaseURL, Format: base.Format, APIKey: base.APIKey, UserAgent: base.UserAgent, Proxy: "http://second-proxy.example:7890"},
	} {
		if got := providerFingerprint("p", changed); got == baseFingerprint {
			t.Fatalf("出网配置变更没有改变健康检测 fingerprint: %#v", changed)
		}
	}
}

func TestInvalidateChangedRejectsResultsFromOlderInflightGeneration(t *testing.T) {
	tests := []struct {
		name   string
		newCfg func(baseURL string) *config.Config
	}{
		{
			name: "deleted provider",
			newCfg: func(string) *config.Config {
				return &config.Config{Providers: map[string]*config.Provider{}}
			},
		},
		{
			name: "changed provider",
			newCfg: func(baseURL string) *config.Config {
				return &config.Config{Providers: map[string]*config.Provider{
					"p": {Name: "p", BaseURL: baseURL + "/changed", APIKey: "new", Format: "anthropic"},
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var startOnce sync.Once
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				startOnce.Do(func() { close(started) })
				<-release
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			checker := NewChecker()
			oldCfg := &config.Config{Providers: map[string]*config.Provider{
				"p": {Name: "p", BaseURL: server.URL, APIKey: "old", Format: "openai"},
			}}
			checkDone := make(chan struct{})
			go func() {
				defer close(checkDone)
				checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("old provider probe did not start")
			}
			newCfg := tt.newCfg(server.URL)
			checker.InvalidateChanged(oldCfg, newCfg)
			// 在旧轮次结束前又切回原配置，旧轮次仍不能冒充新 generation 的完成结果。
			checker.InvalidateChanged(newCfg, oldCfg)
			close(release)
			select {
			case <-checkDone:
			case <-time.After(time.Second):
				t.Fatal("old provider probe did not finish")
			}

			checker.mu.RLock()
			_, resurrected := checker.statuses["p"]
			checker.mu.RUnlock()
			if resurrected {
				t.Fatal("old in-flight run resurrected invalidated provider status")
			}
			statuses := checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
			if got := calls.Load(); got != 2 {
				t.Fatalf("re-added provider must start a fresh probe after stale run, got %d calls", got)
			}
			if statuses["p"].Status != "ok" {
				t.Fatalf("fresh re-added provider probe status = %#v", statuses["p"])
			}
		})
	}
}

func TestInvalidateChangedCancelsStaleInflightProbe(t *testing.T) {
	oldStarted := make(chan struct{})
	oldCanceled := make(chan struct{})
	var startOnce sync.Once
	var cancelOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old/") {
			startOnce.Do(func() { close(oldStarted) })
			<-r.Context().Done()
			cancelOnce.Do(func() { close(oldCanceled) })
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	oldCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL + "/old", APIKey: "old-key", Format: "openai"},
	}}
	newCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL + "/new", APIKey: "new-key", Format: "openai"},
	}}
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
	}()

	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("stale provider probe did not start")
	}
	checker.InvalidateChanged(oldCfg, newCfg)
	select {
	case <-oldCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("provider change did not cancel stale probe")
	}
	select {
	case <-oldDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stale probe round did not finish after cancellation")
	}
	if statuses := checker.CheckAll(context.Background(), newCfg, testResolver(server.Client())); statuses["p"].Status != "ok" {
		t.Fatalf("fresh provider probe status = %#v", statuses["p"])
	}
}

func TestInvalidateChangedRejectsStaleConfigProbe(t *testing.T) {
	var oldCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old/") {
			oldCalls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewChecker()
	oldCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL + "/old", APIKey: "old-key", Format: "openai"},
	}}
	newCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL + "/new", APIKey: "new-key", Format: "openai"},
	}}
	checker.InvalidateChanged(oldCfg, newCfg)

	statuses := checker.CheckAll(context.Background(), oldCfg, testResolver(server.Client()))
	if got := oldCalls.Load(); got != 0 {
		t.Fatalf("stale config started %d provider probes after invalidation", got)
	}
	if statuses["p"].Status != "unchecked" {
		t.Fatalf("stale config status = %#v, want unchecked", statuses["p"])
	}
}

func TestConcurrentCheckAllSharesOneProbeRound(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"a": {Name: "a", BaseURL: server.URL, Format: "openai"},
		"b": {Name: "b", BaseURL: server.URL, Format: "openai"},
	}}
	checker := NewChecker()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses := checker.CheckAll(context.Background(), cfg, testResolver(server.Client()))
			if statuses["a"].Status != "ok" || statuses["b"].Status != "ok" {
				t.Errorf("unexpected statuses: %#v", statuses)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 2 {
		t.Fatalf("expected one shared round (2 probes), got %d calls", got)
	}
}

func TestCanceledLeaderDoesNotCancelSharedProbe(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, Format: "openai"},
	}}
	checker := NewChecker()
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan map[string]Status, 1)
	go func() { leaderDone <- checker.CheckAll(leaderCtx, cfg, testResolver(server.Client())) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shared probe did not start")
	}
	cancelLeader()
	select {
	case statuses := <-leaderDone:
		if statuses["p"].Status != "unchecked" {
			t.Fatalf("canceled leader status = %#v", statuses["p"])
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("canceled leader did not return promptly")
	}

	waiterDone := make(chan map[string]Status, 1)
	go func() { waiterDone <- checker.CheckAll(context.Background(), cfg, testResolver(server.Client())) }()
	close(release)
	select {
	case statuses := <-waiterDone:
		if statuses["p"].Status != "ok" {
			t.Fatalf("shared waiter status = %#v", statuses["p"])
		}
	case <-time.After(time.Second):
		t.Fatal("shared waiter did not receive completed probe")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("leader cancellation started duplicate probes: %d", got)
	}
}

func TestCheckAllUsesCheckerWideConcurrencyLimit(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	configs := make([]*config.Config, 2)
	for i, names := range [][]string{{"a", "b", "c", "d"}, {"e", "f", "g", "h"}} {
		providers := make(map[string]*config.Provider)
		for _, name := range names {
			providers[name] = &config.Provider{Name: name, BaseURL: server.URL, Format: "openai"}
		}
		configs[i] = &config.Config{Providers: providers}
	}
	checker := NewChecker()
	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg *config.Config) {
			defer wg.Done()
			checker.checkAll(context.Background(), cfg, testResolver(server.Client()), checker.generation.Load())
		}(cfg)
	}
	for i := 0; i < checkConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("checker-wide semaphore did not fill all four slots")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := maximum.Load(); got != checkConcurrency {
		close(release)
		t.Fatalf("checker-wide maximum concurrency = %d, want %d", got, checkConcurrency)
	}
	close(release)
	wg.Wait()
}

func TestCheckAllHandlesNilProviderWithoutProbe(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := &config.Config{Providers: map[string]*config.Provider{"nil-provider": nil}}
	statuses := NewChecker().CheckAll(context.Background(), cfg, testResolver(server.Client()))
	if statuses["nil-provider"].Status != "error" {
		t.Fatalf("nil provider status = %#v", statuses["nil-provider"])
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("nil provider unexpectedly issued %d HTTP probes", got)
	}
}

func TestCheckAllResolvesEachProviderProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	var mu sync.Mutex
	called := make(map[string]int)
	resolve := func(proxyURL string) (*http.Client, error) {
		mu.Lock()
		called[proxyURL]++
		mu.Unlock()
		return upstream.Client(), nil
	}
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"first":  {Name: "first", BaseURL: upstream.URL, Format: "openai", Proxy: "http://first-proxy.example:7890"},
		"second": {Name: "second", BaseURL: upstream.URL, Format: "openai", Proxy: "socks5://second-proxy.example:1080"},
	}}
	statuses := NewChecker().CheckAll(context.Background(), cfg, resolve)
	if statuses["first"].Status != "ok" || statuses["second"].Status != "ok" {
		t.Fatalf("statuses = %#v", statuses)
	}
	mu.Lock()
	defer mu.Unlock()
	if called["http://first-proxy.example:7890"] != 1 || called["socks5://second-proxy.example:1080"] != 1 {
		t.Fatalf("resolver calls = %#v", called)
	}
}

func TestProbeAdHocDoesNotWriteStatuses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	checker := NewChecker()
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"saved": {Name: "saved", BaseURL: upstream.URL, Format: "openai"},
	}}
	if _, ok := checker.CheckProvider(context.Background(), cfg, testResolver(upstream.Client()), "saved"); !ok {
		t.Fatal("建立已落盘健康缓存失败")
	}
	checker.mu.RLock()
	before, exists := checker.statuses["saved"]
	countBefore := len(checker.statuses)
	checker.mu.RUnlock()
	if !exists {
		t.Fatal("前置条件：saved 健康缓存不存在")
	}

	status, ok := checker.ProbeAdHoc(context.Background(), &config.Provider{
		Name: "temporary", BaseURL: upstream.URL, Format: "openai", Proxy: "http://temporary-proxy.example:7890",
	}, testResolver(upstream.Client()))
	if !ok || status.Status != "ok" {
		t.Fatalf("ProbeAdHoc() = %#v, %v", status, ok)
	}
	checker.mu.RLock()
	after, stillExists := checker.statuses["saved"]
	countAfter := len(checker.statuses)
	_, temporaryExists := checker.statuses["temporary"]
	checker.mu.RUnlock()
	if !stillExists || after != before || countAfter != countBefore || temporaryExists {
		t.Fatalf("ProbeAdHoc 污染了 statuses：before=%#v/%d after=%#v/%d temporary=%v", before, countBefore, after, countAfter, temporaryExists)
	}
}

func TestSnapshotIncludesUncheckedProviders(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"pending": {Name: "pending", BaseURL: "example.com", Format: "openai"},
	}}

	statuses := NewChecker().Snapshot(cfg)
	if statuses["pending"].Status != "unchecked" {
		t.Fatalf("expected unchecked status, got %#v", statuses["pending"])
	}
}
