package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
)

func TestProviderQueryCatalogRebindAndLateResponse(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-Id") == "remote-a" {
			once.Do(func() { close(entered) })
			<-release
			_, _ = io.WriteString(w, `{"models":[{"slug":"old-model","visibility":"list"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"models":[{"slug":"new-model","visibility":"list"}]}`)
		}
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, _, second := newChatGPTGatewayServer(t, "")
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(&http.Client{Transport: rewriteTransport{target: target}})
	t.Cleanup(srv.stopProviderQueries)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("旧目录未开始")
	}
	srv.cfgMu.Lock()
	next := *srv.cfg
	next.Providers = map[string]*config.Provider{}
	for name, p := range srv.cfg.Providers {
		next.Providers[name] = cloneQueryProvider(p)
	}
	next.Providers["codex"].AccountID = second
	srv.cfg = &next
	srv.revision = "r2"
	srv.reconcileProviderQueriesLocked(&next)
	srv.cfgMu.Unlock()
	close(release)
	select {
	case old := <-done:
		if old.Code != 409 || strings.Contains(old.Body.String(), "old-model") {
			t.Fatalf("旧结果未被隔离：%d %s", old.Code, old.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("旧目录未返回")
	}
	fresh := providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "")
	if fresh.Code != 200 || !strings.Contains(fresh.Body.String(), "new-model") || strings.Contains(fresh.Body.String(), "old-model") {
		t.Fatalf("换绑目录错误：%d %s", fresh.Code, fresh.Body.String())
	}
}

func TestProviderQueryBalanceKeyDoesNotSwitchOrChangeCooldown(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("冷却查询密钥不应发请求")
	}))
	defer up.Close()
	srv := newBoundaryTestServer()
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(up.Client())
	srv.keyPool = keypool.New(nil)
	p := &config.Provider{Name: "p", BaseURL: up.URL, Format: "openai", APIKeys: []config.ProviderKey{{ID: "a", Value: "fake-a"}, {ID: "b", Value: "fake-b"}}, Balance: &config.ProviderBalance{Type: "sub2api", APIKeyID: "a"}}
	srv.cfg.Providers["p"] = p
	srv.keyPool.Reconcile(srv.cfg)
	ticket, _ := srv.keyPool.Select("p", p, nil)
	srv.keyPool.Report(ticket, 429, time.Minute)
	before := srv.keyPool.Snapshot(srv.cfg)["p"][0].Until
	t.Cleanup(srv.stopProviderQueries)
	rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"r1","providers":["p"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "query_key_unavailable") || calls.Load() != 0 {
		t.Fatalf("查询密钥发生切换：%d %s", rec.Code, rec.Body.String())
	}
	if after := srv.keyPool.Snapshot(srv.cfg)["p"][0].Until; after != before {
		t.Fatal("余额查询改变了密钥冷却")
	}
}

func TestProviderQueryBalanceExplicitTokenNeverFallsBack(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/user/self" || r.Header.Get("Authorization") != "Bearer fake-query-secret" {
			t.Error("使用了错误的账户查询路径或令牌")
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "fake-query-secret")
	}))
	defer up.Close()
	srv := newBoundaryTestServer()
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(up.Client())
	srv.cfg.Providers["p"] = &config.Provider{BaseURL: up.URL, Format: "openai", APIKey: "fake-model-secret", Balance: &config.ProviderBalance{Type: "newapi", AccessToken: "fake-query-secret"}}
	t.Cleanup(srv.stopProviderQueries)
	rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"r1","providers":["p"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "credential_rejected") || strings.Contains(rec.Body.String(), "fake-query-secret") || calls.Load() != 1 {
		t.Fatalf("独立查询令牌拒绝边界=%d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderQueryBalanceSameSourceDeduplicates(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"isValid":true,"mode":"unrestricted","balance":0,"unit":"USD"}`)
	}))
	defer up.Close()
	srv := newBoundaryTestServer()
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(up.Client())
	for _, name := range []string{"a", "b"} {
		srv.cfg.Providers[name] = &config.Provider{BaseURL: up.URL, Format: "openai", APIKey: "same-fake-key", Balance: &config.ProviderBalance{Type: "sub2api"}}
	}
	t.Cleanup(srv.stopProviderQueries)
	rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"r1","providers":["a","b"]}`)
	if rec.Code != 200 || calls.Load() != 1 || strings.Count(rec.Body.String(), `"remaining":"0"`) != 2 {
		t.Fatalf("共享来源未去重：%d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderQueryRejectsInvalidBatchBeforeNetwork(t *testing.T) {
	srv := newBoundaryTestServer()
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(blockedClient)
	srv.cfg.Providers["p"] = &config.Provider{BaseURL: "https://fake.invalid", Format: "openai"}
	t.Cleanup(srv.stopProviderQueries)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"revision":"r1","providers":[]}`, 400},
		{`{"revision":"r1","providers":["p","p"]}`, 400},
		{`{"revision":"r1","providers":[""]}`, 400},
		{`{"revision":"r1","providers":["missing"]}`, 404},
		{`{"revision":"old","providers":["p"]}`, 409},
	} {
		rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", tc.body)
		if rec.Code != tc.status {
			t.Fatalf("批次校验=%d %s", rec.Code, rec.Body.String())
		}
	}
	srv.stopProviderQueries()
	rec := providerQueryRequest(srv, http.MethodGet, "/api/providers/balances", "")
	if rec.Code != 503 {
		t.Fatalf("停止后查询状态=%d", rec.Code)
	}
}

func TestProviderQueryCatalogFailedRefreshPreservesSameIdentity(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"models":[{"slug":"saved-model","visibility":"list"}]}`)
			return
		}
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "private-error")
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, first, _ := newChatGPTGatewayServer(t, "")
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(&http.Client{Transport: rewriteTransport{target: target}})
	t.Cleanup(srv.stopProviderQueries)
	rec := providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = providerQueryRequest(srv, http.MethodPost, "/api/providers/models/refresh", `{"provider":"codex","revision":"r1"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stale":true`) || !strings.Contains(rec.Body.String(), "saved-model") || strings.Contains(rec.Body.String(), "private-error") {
		t.Fatalf("旧目录保留不正确：%d %s", rec.Code, rec.Body.String())
	}
	rec = providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stale":true`) || calls.Load() != 2 {
		t.Fatal("刷新失败后重新打开组合框不能把旧目录伪装成新鲜成功，也不能连续重试")
	}
	view, ok := srv.chatgptAuth.Account(first)
	if !ok || view.Invocation.State != "unknown" {
		t.Fatal("目录刷新写入了实际调用健康")
	}
}

func TestProviderQueryCatalogPausedAccountAllowsManagement(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"models":[]}`) }))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, first, _ := newChatGPTGatewayServer(t, "")
	srv.resolveHTTPClient = testClientResolver(&http.Client{Transport: rewriteTransport{target: target}})
	t.Cleanup(srv.stopProviderQueries)
	if _, _, _, err := srv.chatgptAuth.SetPaused(first, true, srv.chatgptAuth.Version()); err != nil {
		t.Fatal(err)
	}
	rec := providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "")
	if rec.Code != 200 || !srv.chatgptAuth.IsPaused(first) {
		t.Fatalf("暂停账户管理查询=%d %s", rec.Code, rec.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/models?provider=codex", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	srv.handle(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatal("已取消的目录查询不应继续读取缓存或出网")
	}
}
