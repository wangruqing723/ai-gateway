package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/providerbalance"
)

// 查询测试只用本机假上游，OAuth 官方域名通过 rewriteTransport 重写到测试服务器。
func providerQueryRequest(s *server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:7789"+path, strings.NewReader(body))
	req.Header.Set("Origin", "http://127.0.0.1:7789")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec
}

func TestProviderQueryOAuthCatalogAndCache(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/backend-api/codex/models" || r.URL.Query().Get("client_version") != "0.167.0" {
			t.Errorf("模型目录路径不正确: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fake-access-a" || r.Header.Get("ChatGPT-Account-Id") != "remote-a" {
			t.Error("目录查询未使用绑定账户")
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"catalog-model","visibility":"list","supported_in_api":false}]}`)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, _, _ := newChatGPTGatewayServer(t, "")
	srv.revision = "query-revision"
	srv.resolveHTTPClient = testClientResolver(&http.Client{Transport: rewriteTransport{target: target}})
	t.Cleanup(srv.stopProviderQueries)
	for i := 0; i < 2; i++ {
		rec := providerQueryRequest(srv, http.MethodGet, "/api/providers/models?provider=codex", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "catalog-model") {
			t.Fatalf("目录响应=%d %s", rec.Code, rec.Body.String())
		}
		for _, secret := range []string{"fake-access-a", "fake-refresh-a", "remote-a"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatal("目录响应泄露身份凭据")
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatal("目录缓存没有复用")
	}
	rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/models/refresh", `{"provider":"codex","revision":"query-revision"}`)
	if rec.Code != 200 || calls.Load() != 2 {
		t.Fatalf("明确刷新未请求目录：%d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderQueryRefreshBoundaries(t *testing.T) {
	for _, path := range []string{"/api/providers/models/refresh", "/api/providers/balances/refresh"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name, method, origin, contentType, body string
				status                                  int
			}{
				{"方法", http.MethodGet, "http://127.0.0.1:7789", "application/json", "{}", 405},
				{"缺Origin", http.MethodPost, "", "application/json", "{}", 403},
				{"跨站", http.MethodPost, "https://foreign.invalid", "application/json", "{}", 403},
				{"媒体类型", http.MethodPost, "http://127.0.0.1:7789", "text/plain", "{}", 415},
				{"未知字段", http.MethodPost, "http://127.0.0.1:7789", "application/json", `{"secret":"fake-secret"}`, 400},
				{"null", http.MethodPost, "http://127.0.0.1:7789", "application/json", `{"revision":null}`, 400},
				{"重复字段", http.MethodPost, "http://127.0.0.1:7789", "application/json", `{"revision":"a","revision":"b"}`, 400},
				{"尾随", http.MethodPost, "http://127.0.0.1:7789", "application/json", `{} {}`, 400},
				{"超限", http.MethodPost, "http://127.0.0.1:7789", "application/json", strings.Repeat("x", maxProviderQueryRequestBytes+1), 413},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv := newBoundaryTestServer()
					srv.resolveHTTPClient = testClientResolver(blockedClient)
					t.Cleanup(srv.stopProviderQueries)
					req := httptest.NewRequest(tc.method, "http://127.0.0.1:7789"+path, strings.NewReader(tc.body))
					req.Header.Set("Origin", tc.origin)
					req.Header.Set("Content-Type", tc.contentType)
					rec := httptest.NewRecorder()
					srv.handle(rec, req)
					if rec.Code != tc.status {
						t.Fatalf("状态=%d 响应=%s", rec.Code, rec.Body.String())
					}
					if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
						t.Fatal("错误响应缺安全头")
					}
					if strings.Contains(rec.Body.String(), "fake-secret") {
						t.Fatal("错误泄露输入")
					}
				})
			}
		})
	}
}

func TestProviderQueryBalanceReadOnlyAndRefresh(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer fake-model-key" {
			t.Error("余额请求路径或凭据错误")
		}
		_, _ = io.WriteString(w, `{"isValid":true,"mode":"unrestricted","balance":12.5,"remaining":12.5,"unit":"USD"}`)
	}))
	defer up.Close()
	srv := newBoundaryTestServer()
	srv.revision = "r1"
	srv.resolveHTTPClient = testClientResolver(up.Client())
	srv.cfg.Providers["p"] = &config.Provider{BaseURL: up.URL + "/v1", Format: "openai", APIKey: "fake-model-key", Balance: &config.ProviderBalance{Type: "sub2api"}}
	t.Cleanup(srv.stopProviderQueries)
	for i := 0; i < 2; i++ {
		rec := providerQueryRequest(srv, http.MethodGet, "/api/providers/balances", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not_queried") {
			t.Fatalf("缓存初态=%d %s", rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("读取余额缓存触发了上游")
	}
	rec := providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"r1","providers":["p"]}`)
	var result struct {
		Results []providerbalance.Result `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(result.Results) != 1 || result.Results[0].Status != "success" {
		t.Fatalf("刷新=%d %s", rec.Code, rec.Body.String())
	}
	snap := result.Results[0].Balance.Snapshot
	if snap == nil || len(snap.Items) != 1 || snap.Items[0].Kind != "account_balance" || snap.Items[0].Remaining == nil || *snap.Items[0].Remaining != "12.5" {
		t.Fatalf("余额快照=%#v", snap)
	}
	rec = providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"r1","providers":["p"]}`)
	if rec.Code != 200 || calls.Load() != 1 || !strings.Contains(rec.Body.String(), `"status":"cached"`) {
		t.Fatal("短间隔重复刷新未合并")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("fake-model-key")) {
		t.Fatal("余额泄露凭据")
	}
	rec = providerQueryRequest(srv, http.MethodPost, "/api/providers/balances/refresh", `{"revision":"old","providers":["p"]}`)
	if rec.Code != 409 || calls.Load() != 1 {
		t.Fatal("过期版本仍发起余额查询")
	}
}
