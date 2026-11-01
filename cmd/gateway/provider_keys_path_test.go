package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/providerhealth"
)

func TestProviderModelsUsesBackupAfterPrimary401(t *testing.T) {
	var received []string
	var receivedMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		receivedMu.Lock()
		received = append(received, key)
		receivedMu.Unlock()
		if key == "Bearer models-primary-test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer upstream.Close()

	provider := modelsTestProvider(upstream.URL)
	pool := keypool.New(nil)
	cfg := &config.Config{Providers: map[string]*config.Provider{"models": provider}}
	pool.Reconcile(cfg)
	srv := newBoundaryTestServer()
	srv.cfg = cfg
	srv.keyPool = pool
	srv.resolveHTTPClient = testClientResolver(upstream.Client())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/models?provider=models", nil)
	srv.handleProviderModels(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "model-a") {
		t.Fatalf("模型查询结果 = %d/%s", recorder.Code, recorder.Body.String())
	}
	want := []string{"Bearer models-primary-test-secret", "Bearer models-backup-test-secret"}
	receivedMu.Lock()
	gotKeys := append([]string(nil), received...)
	receivedMu.Unlock()
	if len(gotKeys) != len(want) || gotKeys[0] != want[0] || gotKeys[1] != want[1] {
		t.Fatalf("上游密钥顺序 = %#v，期望 %#v", gotKeys, want)
	}
	if ticket, _ := pool.Select("models", provider, nil); ticket == nil || ticket.Key.ID != "primary" {
		t.Fatalf("模型查询不应更新冷却，后续首选密钥 = %#v", ticket)
	}
}

func TestProviderModelsAllCoolingReturns503(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	provider := modelsTestProvider(upstream.URL)
	pool := keypool.New(nil)
	cfg := &config.Config{Providers: map[string]*config.Provider{"models": provider}}
	pool.Reconcile(cfg)
	excluded := make(map[string]struct{})
	for range provider.APIKeys {
		ticket, _ := pool.Select("models", provider, excluded)
		if ticket == nil {
			t.Fatal("未取得用于设置冷却的密钥 ticket")
		}
		pool.Report(ticket, http.StatusTooManyRequests, time.Minute)
		excluded[ticket.Key.ID] = struct{}{}
	}
	srv := newBoundaryTestServer()
	srv.cfg = cfg
	srv.keyPool = pool
	srv.resolveHTTPClient = testClientResolver(upstream.Client())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/models?provider=models", nil)
	srv.handleProviderModels(recorder, request)
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || body.Error.Type != "no_selectable_provider_key" {
		t.Fatalf("全部冷却响应 = %d/%s", recorder.Code, recorder.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("全部冷却时仍向上游发送 %d 次请求", got)
	}
}

func TestProviderProbeTestKeyIDOnlyTestsSelectedKeyAndKeepsCooldown(t *testing.T) {
	var received []string
	var receivedMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMu.Lock()
		received = append(received, r.Header.Get("Authorization"))
		receivedMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	now := time.Date(2026, time.October, 31, 0, 0, 0, 0, time.UTC)
	pool := keypool.New(func() time.Time { return now })
	provider := &config.Provider{
		Name:    "saved",
		BaseURL: upstream.URL,
		Format:  "openai",
		APIKeys: []config.ProviderKey{
			{ID: "primary", Value: "probe-primary-test-secret"},
			{ID: "selected", Value: "probe-selected-test-secret"},
		},
	}
	cfg := &config.Config{Providers: map[string]*config.Provider{"saved": provider}}
	pool.Reconcile(cfg)
	primary, _ := pool.Select("saved", provider, nil)
	selected, _ := pool.Select("saved", provider, map[string]struct{}{"primary": {}})
	if primary == nil || selected == nil {
		t.Fatal("未取得用于准备冷却的密钥 ticket")
	}
	pool.Report(selected, http.StatusTooManyRequests, time.Minute)
	disabled := false
	provider.APIKeys[1].Enabled = &disabled
	pool.Reconcile(cfg)
	before := pool.Snapshot(cfg)

	srv := newBoundaryTestServer()
	srv.cfg = cfg
	srv.keyPool = pool
	srv.providerHealth = providerhealth.NewChecker()
	srv.resolveHTTPClient = testClientResolver(upstream.Client())
	recorder := callProviderProbe(t, srv, map[string]any{
		"name": "saved", "baseUrl": upstream.URL, "format": "openai",
		"apiKeys": []map[string]any{
			{"id": "primary", "value": config.APIKeyKeepSentinel},
			{"id": "selected", "value": config.APIKeyKeepSentinel, "enabled": false},
		},
		"testKeyId": "selected",
	})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("指定密钥探测 = %d/%s", recorder.Code, recorder.Body.String())
	}
	receivedMu.Lock()
	gotRequests := append([]string(nil), received...)
	receivedMu.Unlock()
	if len(gotRequests) != 1 || gotRequests[0] != "Bearer probe-selected-test-secret" {
		t.Fatalf("testKeyId 探测请求 = %#v，期望只请求指定密钥", gotRequests)
	}
	if after := pool.Snapshot(cfg); !reflect.DeepEqual(after, before) {
		t.Fatalf("连接测试改变了冷却状态：before=%#v after=%#v", before, after)
	}
}

func TestProviderProbeRejectsUnknownKeySentinelIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	srv, _ := newProbeTestServer(upstream)

	recorder := callProviderProbe(t, srv, map[string]any{
		"name": "saved", "baseUrl": upstream.URL, "format": "openai",
		"apiKeys": []map[string]any{{"id": "missing", "value": config.APIKeyKeepSentinel}},
	})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "没有该身份的已存密钥") {
		t.Fatalf("未知身份 sentinel 响应 = %d/%s", recorder.Code, recorder.Body.String())
	}
}

func TestProviderProbeRedactsEverySubmittedKeyFromError(t *testing.T) {
	const first = "probe-redact-first-test-secret"
	const second = "probe-redact-second-test-secret"
	srv := newBoundaryTestServer()
	srv.providerHealth = providerhealth.NewChecker()
	srv.resolveHTTPClient = func(string) (*http.Client, error) {
		return nil, fmt.Errorf("探测错误回显 %s 与 %s", first, second)
	}
	recorder := callProviderProbe(t, srv, map[string]any{
		"name": "temporary", "baseUrl": "https://probe.example", "format": "openai",
		"apiKeys": []map[string]any{
			{"id": "first", "value": first},
			{"id": "second", "value": second},
		},
		"testKeyId": "first",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("连接测试状态 = %d/%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), first) || strings.Contains(recorder.Body.String(), second) {
		t.Fatalf("连接测试错误消息泄漏密钥明文：%s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), config.APIKeyKeepSentinel) {
		t.Fatalf("连接测试错误消息未显示 sentinel：%s", recorder.Body.String())
	}
}

func TestProviderProbeListSentinelInheritsStoredSingleKey(t *testing.T) {
	var received string
	var receivedMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMu.Lock()
		received = r.Header.Get("Authorization")
		receivedMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	srv := newBoundaryTestServer()
	srv.cfg.Providers["saved"] = &config.Provider{
		Name: "saved", BaseURL: upstream.URL, Format: "openai", APIKey: "probe-single-test-secret",
	}
	srv.providerHealth = providerhealth.NewChecker()
	srv.resolveHTTPClient = testClientResolver(upstream.Client())
	recorder := callProviderProbe(t, srv, map[string]any{
		"name": "saved", "baseUrl": upstream.URL, "format": "openai",
		"apiKeys": []map[string]any{{"id": "converted", "value": config.APIKeyKeepSentinel}},
	})
	receivedMu.Lock()
	gotAuth := received
	receivedMu.Unlock()
	if recorder.Code != http.StatusOK || gotAuth != "Bearer probe-single-test-secret" {
		t.Fatalf("旧单值 sentinel 转列表探测 = %d/%s，Authorization=%q", recorder.Code, recorder.Body.String(), gotAuth)
	}
}

func modelsTestProvider(baseURL string) *config.Provider {
	return &config.Provider{
		Name:    "models",
		BaseURL: baseURL,
		Format:  "openai",
		APIKeys: []config.ProviderKey{
			{ID: "primary", Value: "models-primary-test-secret"},
			{ID: "backup", Value: "models-backup-test-secret"},
		},
	}
}

func newProbeTestServer(upstream *httptest.Server) (*server, *keypool.Pool) {
	provider := &config.Provider{
		Name: "saved", BaseURL: upstream.URL, Format: "openai",
		APIKeys: []config.ProviderKey{{ID: "existing", Value: "probe-existing-test-secret"}},
	}
	pool := keypool.New(nil)
	cfg := &config.Config{Providers: map[string]*config.Provider{"saved": provider}}
	pool.Reconcile(cfg)
	srv := newBoundaryTestServer()
	srv.cfg = cfg
	srv.keyPool = pool
	srv.providerHealth = providerhealth.NewChecker()
	srv.resolveHTTPClient = testClientResolver(upstream.Client())
	return srv, pool
}
