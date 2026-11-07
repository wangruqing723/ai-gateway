package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ai-gateway/internal/config"
)

func TestProviderQueryConfigSecretsRoundTrip(t *testing.T) {
	raw := testConfigYAML("127.0.0.1", 7789, "https://fake.example.invalid/v1", "fake-model-key", 5)
	raw = strings.Replace(raw, "    format: openai", "    format: openai\n    balance:\n      type: newapi\n      source: account\n      accessToken: fake-balance-secret\n      userId: \"123\"", 1)
	srv := newConfigTestServer(t, raw)
	srv.resolveHTTPClient = testClientResolver(blockedClient)
	t.Cleanup(srv.stopProviderQueries)
	rec := providerQueryRequest(srv, http.MethodGet, "/api/config", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "fake-balance-secret") || !strings.Contains(rec.Body.String(), config.BalanceTokenKeepSentinel) || !strings.Contains(rec.Body.String(), `"accessTokenConfigured":true`) {
		t.Fatalf("配置查询脱敏失败: %d %s", rec.Code, rec.Body.String())
	}
	rawRec := providerQueryRequest(srv, http.MethodGet, "/api/config/raw", "")
	if rawRec.Code != 200 || strings.Contains(rawRec.Body.String(), "fake-balance-secret") || !strings.Contains(rawRec.Body.String(), config.BalanceTokenKeepSentinel) {
		t.Fatalf("原始配置未脱敏: %d %s", rawRec.Code, rawRec.Body.String())
	}
	// 只从安全结构生成写入载荷，GET专用元数据不能直接回传。
	snapshot, _, _, _, err := srv.configViewSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Path = ""
	snapshot.Providers["primary"].MaxConcurrent = 7
	payload, _ := json.Marshal(snapshot)
	saved := providerQueryRequest(srv, http.MethodPut, "/api/config", string(payload))
	if saved.Code != 200 {
		t.Fatalf("保留查询令牌保存失败: %d %s", saved.Code, saved.Body.String())
	}
	p, _ := srv.providerQuerySnapshot("primary")
	if p.Balance.AccessToken != "fake-balance-secret" || p.MaxConcurrent != 7 {
		t.Fatal("无关配置保存丢失查询令牌")
	}
	snapshot, _, _, _, err = srv.configViewSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Path = ""
	snapshot.Providers["primary"].Balance.BaseURL = "https://different.example.invalid"
	payload, _ = json.Marshal(snapshot)
	rejected := providerQueryRequest(srv, http.MethodPut, "/api/config", string(payload))
	if rejected.Code != 400 || strings.Contains(rejected.Body.String(), "fake-balance-secret") {
		t.Fatalf("换目标未拒绝旧令牌保留: %d %s", rejected.Code, rejected.Body.String())
	}
	snapshot.Providers["primary"].Balance.BaseURL = ""
	snapshot.Providers["primary"].Balance.Source = "auto"
	snapshot.Providers["primary"].Balance.AccessToken = ""
	payload, _ = json.Marshal(snapshot)
	cleared := providerQueryRequest(srv, http.MethodPut, "/api/config", string(payload))
	if cleared.Code != 200 {
		t.Fatalf("明确清除失败: %d %s", cleared.Code, cleared.Body.String())
	}
	p, _ = srv.providerQuerySnapshot("primary")
	if p.Balance.AccessToken != "" {
		t.Fatal("查询令牌没有清除")
	}
}

func TestProviderQueryRefillRequiresSameTarget(t *testing.T) {
	old := &config.Provider{BaseURL: "https://api.example.invalid/v1", Balance: &config.ProviderBalance{Type: "newapi", Source: "account", AccessToken: "fake-secret"}}
	for _, tc := range []struct {
		name string
		edit func(*config.Provider)
		bad  bool
	}{
		{"同目标", func(*config.Provider) {}, false},
		{"不同类型", func(p *config.Provider) { p.Balance.Type = "sub2api" }, true},
		{"不同路径", func(p *config.Provider) { p.Balance.BaseURL = "https://api.example.invalid/team" }, true},
		{"不同域名", func(p *config.Provider) { p.Balance.BaseURL = "https://else.example.invalid" }, true},
		{"默认端口等价", func(p *config.Provider) { p.Balance.BaseURL = "https://API.EXAMPLE.INVALID:443" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cloneQueryProvider(old)
			p.Balance.AccessToken = config.BalanceTokenKeepSentinel
			tc.edit(p)
			err := refillBalanceToken("p", p, old)
			if (err != nil) != tc.bad {
				t.Fatalf("回填结果=%v", err)
			}
			if err == nil && p.Balance.AccessToken != "fake-secret" {
				t.Fatal("同目标未回填")
			}
		})
	}
	p := cloneQueryProvider(old)
	p.Balance.AccessToken = config.BalanceTokenKeepSentinel
	if err := refillBalanceToken("new", p, nil); err == nil {
		t.Fatal("新提供商接受占位符")
	}
}
