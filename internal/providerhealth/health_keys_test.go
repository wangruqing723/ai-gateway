package providerhealth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
)

func TestCheckProviderKeyListStatusRules(t *testing.T) {
	tests := []struct {
		name          string
		statuses      map[string][]int
		enabled       []bool
		wantStatus    string
		wantHTTPCode  int
		wantRequests  int
		wantMessageIn string
	}{
		{
			name:          "鉴权失败后备用密钥成功",
			statuses:      map[string][]int{"Bearer health-primary-test-secret": {http.StatusUnauthorized}, "Bearer health-backup-test-secret": {http.StatusOK}},
			wantStatus:    "ok",
			wantHTTPCode:  http.StatusOK,
			wantRequests:  2,
			wantMessageIn: "primary",
		},
		{
			name:         "首个非密钥失败立即结束",
			statuses:     map[string][]int{"Bearer health-primary-test-secret": {http.StatusBadGateway}, "Bearer health-backup-test-secret": {http.StatusOK}},
			wantStatus:   "error",
			wantHTTPCode: http.StatusBadGateway,
			wantRequests: 1,
		},
		{
			name:         "全部密钥相关失败采用最后结果",
			statuses:     map[string][]int{"Bearer health-primary-test-secret": {http.StatusUnauthorized}, "Bearer health-backup-test-secret": {http.StatusTooManyRequests}},
			wantStatus:   "warn",
			wantHTTPCode: http.StatusTooManyRequests,
			wantRequests: 2,
		},
		{
			name:          "全部停用不发请求",
			enabled:       []bool{false, false},
			wantStatus:    "warn",
			wantRequests:  0,
			wantMessageIn: "没有可选择的 Provider 密钥",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var received []string
			var receivedMu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("Authorization")
				receivedMu.Lock()
				received = append(received, key)
				statuses := tt.statuses[key]
				status := http.StatusOK
				if len(statuses) > 0 {
					status = statuses[0]
					tt.statuses[key] = statuses[1:]
				}
				receivedMu.Unlock()
				w.WriteHeader(status)
			}))
			defer server.Close()

			provider := healthTestProvider(server.URL)
			if len(tt.enabled) > 0 {
				for i := range provider.APIKeys {
					enabled := tt.enabled[i]
					provider.APIKeys[i].Enabled = &enabled
				}
			}
			cfg := &config.Config{Providers: map[string]*config.Provider{"p": provider}}
			status, ok := NewChecker().CheckProvider(context.Background(), cfg, testResolver(server.Client()), "p")
			if !ok {
				t.Fatal("CheckProvider() ok = false")
			}
			if status.Status != tt.wantStatus || status.HTTPCode != tt.wantHTTPCode {
				t.Fatalf("健康状态 = %#v，期望 status=%q httpCode=%d", status, tt.wantStatus, tt.wantHTTPCode)
			}
			receivedMu.Lock()
			gotRequests := append([]string(nil), received...)
			receivedMu.Unlock()
			if len(gotRequests) != tt.wantRequests {
				t.Fatalf("探测请求数 = %d，期望 %d；收到 %#v", len(gotRequests), tt.wantRequests, gotRequests)
			}
			if tt.wantMessageIn != "" && !strings.Contains(status.Message, tt.wantMessageIn) {
				t.Fatalf("健康消息 = %q，期望包含 %q", status.Message, tt.wantMessageIn)
			}
		})
	}
}

func TestCheckProviderKeyListDoesNotChangeCooldown(t *testing.T) {
	now := time.Date(2026, time.October, 31, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer health-primary-test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	pool := keypool.New(func() time.Time { return now })
	provider := healthTestProvider(server.URL)
	cfg := &config.Config{Providers: map[string]*config.Provider{"p": provider}}
	pool.Reconcile(cfg)
	before := pool.Snapshot(cfg)
	checker := NewChecker()
	checker.SetKeyPool(pool)
	status, ok := checker.CheckProvider(context.Background(), cfg, testResolver(server.Client()), "p")
	if !ok || status.Status != "ok" {
		t.Fatalf("CheckProvider() = %#v, %v，期望 ok", status, ok)
	}
	if after := pool.Snapshot(cfg); !reflect.DeepEqual(after, before) {
		t.Fatalf("健康检测改变了密钥冷却状态：before=%#v after=%#v", before, after)
	}
}

func TestCheckProviderSkipsCoolingKeyWithoutChangingCooldown(t *testing.T) {
	now := time.Date(2026, time.October, 31, 0, 0, 0, 0, time.UTC)
	var received []string
	var receivedMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMu.Lock()
		received = append(received, r.Header.Get("Authorization"))
		receivedMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	pool := keypool.New(func() time.Time { return now })
	provider := healthTestProvider(server.URL)
	cfg := &config.Config{Providers: map[string]*config.Provider{"p": provider}}
	pool.Reconcile(cfg)
	ticket, _ := pool.Select("p", provider, nil)
	if ticket == nil {
		t.Fatal("未取得主密钥 ticket")
	}
	pool.Report(ticket, http.StatusUnauthorized, 0)
	before := pool.Snapshot(cfg)
	checker := NewChecker()
	checker.SetKeyPool(pool)
	status, ok := checker.CheckProvider(context.Background(), cfg, testResolver(server.Client()), "p")
	if !ok || status.Status != "ok" {
		t.Fatalf("CheckProvider() = %#v, %v，期望 ok", status, ok)
	}
	receivedMu.Lock()
	gotRequests := append([]string(nil), received...)
	receivedMu.Unlock()
	if len(gotRequests) != 1 || gotRequests[0] != "Bearer health-backup-test-secret" {
		t.Fatalf("冷却中的主密钥仍被探测：%#v", gotRequests)
	}
	if after := pool.Snapshot(cfg); !reflect.DeepEqual(after, before) {
		t.Fatalf("健康检测改变了冷却状态：before=%#v after=%#v", before, after)
	}
}

func TestProviderFingerprintIncludesKeyIdentityValueAndEnabled(t *testing.T) {
	original := healthTestProvider("https://health.example")
	fingerprint := providerFingerprint("p", original)
	tests := []struct {
		name   string
		mutate func(*config.Provider)
	}{
		{name: "身份变化", mutate: func(p *config.Provider) { p.APIKeys[0].ID = "other" }},
		{name: "密钥值变化", mutate: func(p *config.Provider) { p.APIKeys[0].Value += "-changed" }},
		{name: "启用状态变化", mutate: func(p *config.Provider) { disabled := false; p.APIKeys[0].Enabled = &disabled }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := *original
			changed.APIKeys = append([]config.ProviderKey(nil), original.APIKeys...)
			tt.mutate(&changed)
			if got := providerFingerprint("p", &changed); got == fingerprint {
				t.Fatal("Provider 密钥列表变化后指纹没有变化")
			}
		})
	}
}

func healthTestProvider(baseURL string) *config.Provider {
	return &config.Provider{
		Name:    "p",
		BaseURL: baseURL,
		Format:  "openai",
		APIKeys: []config.ProviderKey{
			{ID: "primary", Value: "health-primary-test-secret"},
			{ID: "backup", Value: "health-backup-test-secret"},
		},
	}
}
