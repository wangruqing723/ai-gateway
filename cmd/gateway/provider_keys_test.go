package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/metrics"
)

func TestGetConfigRedactsProviderKeyListAndExposesMetadata(t *testing.T) {
	raw := providerKeyListConfigYAML(`      - id: main
        name: 主账号
        value: fake-main-value
      - id: spare
        value: fake-spare-value
        enabled: false`)
	srv := newConfigTestServer(t, raw)
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "fake-main-value") || strings.Contains(recorder.Body.String(), "fake-spare-value") {
		t.Fatalf("GET /api/config 泄露列表密钥: %s", recorder.Body.String())
	}
	var response struct {
		Providers map[string]struct {
			APIKeys []struct {
				ID              string `json:"id"`
				Value           string `json:"value"`
				ValueConfigured bool   `json:"valueConfigured"`
				Fingerprint     string `json:"fingerprint"`
			} `json:"apiKeys"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	keys := response.Providers["primary"].APIKeys
	if len(keys) != 2 {
		t.Fatalf("apiKeys length = %d, want 2", len(keys))
	}
	for i, want := range []struct {
		id          string
		fingerprint string
	}{
		{id: "main", fingerprint: keypool.Fingerprint("fake-main-value")},
		{id: "spare", fingerprint: keypool.Fingerprint("fake-spare-value")},
	} {
		if keys[i].ID != want.id || keys[i].Value != config.APIKeyKeepSentinel || !keys[i].ValueConfigured || keys[i].Fingerprint != want.fingerprint {
			t.Fatalf("apiKeys[%d] view = %+v, want id=%q configured sentinel fingerprint=%q", i, keys[i], want.id, want.fingerprint)
		}
	}
}

func TestPutConfigProviderKeySentinelGuards(t *testing.T) {
	t.Run("list identity preserves its value", func(t *testing.T) {
		srv := newConfigTestServer(t, providerKeyListConfigYAML(`      - id: main
        name: 主账号
        value: fake-main-value`))
		body := providerKeyListConfigYAML(`      - id: main
        name: 新名称
        value: "` + config.APIKeyKeepSentinel + `"`)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("PUT status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
		srv.cfgMu.RLock()
		defer srv.cfgMu.RUnlock()
		key := srv.cfg.Providers["primary"].APIKeys[0]
		if key.ID != "main" || key.Name != "新名称" || key.Value != "fake-main-value" {
			t.Fatalf("回填后的密钥 = %+v", key)
		}
	})

	t.Run("single key converts to one inherited list key", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "fake-single-value", 5))
		body := providerKeyListConfigYAML(`      - id: converted
        value: "` + config.APIKeyKeepSentinel + `"`)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("PUT status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
		srv.cfgMu.RLock()
		defer srv.cfgMu.RUnlock()
		provider := srv.cfg.Providers["primary"]
		if provider.APIKey != "" || len(provider.APIKeys) != 1 || provider.APIKeys[0].Value != "fake-single-value" {
			t.Fatalf("单值转列表后的 Provider = %+v", provider)
		}
	})

	t.Run("list sentinel requires existing identity", func(t *testing.T) {
		srv := newConfigTestServer(t, providerKeyListConfigYAML(`      - id: main
        value: fake-main-value`))
		body := providerKeyListConfigYAML(`      - id: unknown
        value: "` + config.APIKeyKeepSentinel + `"`)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "无法保留已有密钥：磁盘上没有该身份的已存密钥") {
			t.Fatalf("PUT status/body = %d/%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("single key cannot fill multiple list sentinels", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "fake-single-value", 5))
		body := providerKeyListConfigYAML(`      - id: first
        value: "` + config.APIKeyKeepSentinel + `"
      - id: second
        value: "` + config.APIKeyKeepSentinel + `"`)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "不能从单值 apiKey 同时回填多个密钥") {
			t.Fatalf("PUT status/body = %d/%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("repeated values are checked after fill", func(t *testing.T) {
		srv := newConfigTestServer(t, providerKeyListConfigYAML(`      - id: main
        value: fake-duplicate-value`))
		body := providerKeyListConfigYAML(`      - id: main
        value: "` + config.APIKeyKeepSentinel + `"
      - id: spare
        value: fake-duplicate-value`)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "providers.primary.apiKeys[1].value 与 apiKeys[0] 重复") {
			t.Fatalf("PUT status/body = %d/%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestHealthExposesProviderKeySnapshot(t *testing.T) {
	providers := map[string]*config.Provider{
		"primary": providerWithKeys("primary", "https://api.example.com",
			config.ProviderKey{ID: "main", Name: "主账号", Value: "fake-main-value"},
			config.ProviderKey{ID: "disabled", Value: "fake-disabled-value", Enabled: testBoolPtr(false)},
		),
		"legacy": {Name: "legacy", BaseURL: "https://legacy.example.com", Format: "openai", APIKey: "fake-single-value"},
	}
	srv := newKeySwitchTestServer(providers, singleCandidateRoute("primary", "upstream"), http.DefaultClient, config.Failover{})
	ticket, _ := srv.keyPool.Select("primary", providers["primary"], nil)
	if ticket == nil {
		t.Fatal("未能选择主密钥")
	}
	srv.keyPool.Report(ticket, http.StatusUnauthorized, 0)

	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ProviderKeys map[string][]keypool.Snapshot `json:"providerKeys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	keys := response.ProviderKeys["primary"]
	if len(keys) != 2 {
		t.Fatalf("providerKeys.primary = %+v", keys)
	}
	if _, exists := response.ProviderKeys["legacy"]; exists {
		t.Fatalf("单值 Provider 不应出现在 providerKeys: %+v", response.ProviderKeys)
	}
	if keys[0].State != keypool.StateCooling || keys[0].Reason != "auth" || keys[0].Status != http.StatusUnauthorized || keys[0].RemainingMs < 1 {
		t.Fatalf("冷却快照错误: %+v", keys[0])
	}
	if keys[1].State != keypool.StateDisabled || keys[1].Enabled {
		t.Fatalf("停用密钥快照错误: %+v", keys[1])
	}
}

func TestKeyCooldownResetEndpointAndSameOriginGuard(t *testing.T) {
	providers := map[string]*config.Provider{
		"primary": providerWithKeys("primary", "https://api.example.com", config.ProviderKey{ID: "main", Value: "fake-main-value"}),
	}
	srv := newKeySwitchTestServer(providers, singleCandidateRoute("primary", "upstream"), http.DefaultClient, config.Failover{})
	ticket, _ := srv.keyPool.Select("primary", providers["primary"], nil)
	srv.keyPool.Report(ticket, http.StatusPaymentRequired, 0)

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/keys/cooldown/reset?provider=primary&key=main", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	blocked := httptest.NewRecorder()
	srv.handle(blocked, request)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("跨站 reset status = %d, want 403; body=%s", blocked.Code, blocked.Body.String())
	}
	if got := srv.keyPool.Snapshot(srv.cfg)["primary"][0].State; got != keypool.StateCooling {
		t.Fatalf("跨站请求改变了冷却状态: %s", got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/keys/cooldown/reset?provider=primary&key=main", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7789")
	recorder := httptest.NewRecorder()
	srv.handle(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("同源 reset status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Provider string             `json:"provider"`
		Key      string             `json:"key"`
		Reset    bool               `json:"reset"`
		Keys     []keypool.Snapshot `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Provider != "primary" || response.Key != "main" || !response.Reset || len(response.Keys) != 1 || response.Keys[0].State != keypool.StateSelectable {
		t.Fatalf("reset response = %+v", response)
	}
	if got := srv.keyPool.Snapshot(srv.cfg)["primary"][0].State; got != keypool.StateSelectable {
		t.Fatalf("解除冷却后状态 = %s", got)
	}
}

func TestKeyPoolEventsReachEventLog(t *testing.T) {
	providers := map[string]*config.Provider{
		"primary": providerWithKeys("primary", "https://api.example.com", config.ProviderKey{ID: "main", Name: "主账号", Value: "fake-main-value"}),
	}
	srv := newKeySwitchTestServer(providers, singleCandidateRoute("primary", "upstream"), http.DefaultClient, config.Failover{})
	srv.eventLog = metrics.NewEventLog()
	srv.keyPool.SetEventHandler(srv.onKeyPoolEvent)
	ticket, _ := srv.keyPool.Select("primary", providers["primary"], nil)
	srv.keyPool.Report(ticket, http.StatusUnauthorized, 0)
	if !srv.keyPool.Release("primary", "main") {
		t.Fatal("测试清理冷却失败")
	}
	recoveredTicket, _ := srv.keyPool.Select("primary", providers["primary"], nil)
	srv.keyPool.Report(recoveredTicket, http.StatusOK, 0)

	events := srv.eventLog.Events()
	if len(events) != 2 || events[0].Kind != "key_recovered" || !events[0].Recovered || events[1].Kind != "key_cooldown" || events[0].Provider != "primary" {
		t.Fatalf("事件回调结果 = %+v", events)
	}
}

func providerKeyListConfigYAML(keys string) string {
	return `host: "127.0.0.1"
port: 7789
providers:
  primary:
    baseUrl: "https://api.example.com"
    format: openai
    apiKeys:
` + keys + `
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "*"
    provider: primary
    model: upstream
`
}
