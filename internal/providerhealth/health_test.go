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

	statuses := NewChecker().CheckAll(context.Background(), cfg, server.Client())
	if statuses["ok"].Status != "ok" {
		t.Fatalf("expected ok status, got %#v", statuses["ok"])
	}
	if statuses["bad"].Status != "error" || statuses["bad"].HTTPCode != http.StatusUnauthorized {
		t.Fatalf("expected auth error, got %#v", statuses["bad"])
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
	checker.CheckAll(context.Background(), original, server.Client())

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
	checker.CheckAll(context.Background(), oldCfg, server.Client())
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
	checker.CheckAll(context.Background(), cfg, server.Client())
	checker.InvalidateChanged(cfg, cfg)
	checker.CheckAll(context.Background(), cfg, server.Client())

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
	checker.CheckAll(context.Background(), oldCfg, server.Client())
	const newSecret = "unique-health-secret-7f3a"
	newCfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", BaseURL: server.URL, APIKey: newSecret, Format: "openai"},
	}}
	checker.InvalidateChanged(oldCfg, newCfg)
	statuses := checker.CheckAll(context.Background(), newCfg, server.Client())
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
				checker.CheckAll(context.Background(), oldCfg, server.Client())
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
			statuses := checker.CheckAll(context.Background(), oldCfg, server.Client())
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
		checker.CheckAll(context.Background(), oldCfg, server.Client())
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
	if statuses := checker.CheckAll(context.Background(), newCfg, server.Client()); statuses["p"].Status != "ok" {
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

	statuses := checker.CheckAll(context.Background(), oldCfg, server.Client())
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
			statuses := checker.CheckAll(context.Background(), cfg, server.Client())
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
	go func() { leaderDone <- checker.CheckAll(leaderCtx, cfg, server.Client()) }()
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
	go func() { waiterDone <- checker.CheckAll(context.Background(), cfg, server.Client()) }()
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
			checker.checkAll(context.Background(), cfg, server.Client(), checker.generation.Load())
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
	statuses := NewChecker().CheckAll(context.Background(), cfg, server.Client())
	if statuses["nil-provider"].Status != "error" {
		t.Fatalf("nil provider status = %#v", statuses["nil-provider"])
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("nil provider unexpectedly issued %d HTTP probes", got)
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
