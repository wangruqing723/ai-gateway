package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func queryTestProvider() *Provider {
	return &Provider{BaseURL: "https://api.example.invalid/team/v1", Format: "openai", Balance: &ProviderBalance{Type: "newapi"}}
}

func TestProviderQueryValidation(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*Provider)
		field string
	}{
		{"缺省来源与缺密钥由运行时处理", func(p *Provider) {}, ""},
		{"账户无需密钥ID", func(p *Provider) {
			p.Balance.Source = "account"
			p.Balance.AccessToken = "fake-token"
			p.APIKeys = makeProviderKeys(1)
		}, ""},
		{"auto令牌跨站", func(p *Provider) {
			p.Balance.AccessToken = "fake-token"
			p.Balance.BaseURL = "https://panel.example.invalid/team"
		}, ""},
		{"绑定停用密钥仍合法", func(p *Provider) {
			p.APIKeys = []ProviderKey{{ID: "one", Value: "fake", Enabled: boolPtr(false)}}
			p.Balance.APIKeyID = "one"
		}, ""},
		{"未知适配器", func(p *Provider) { p.Balance.Type = "other" }, "type"},
		{"未指定适配器", func(p *Provider) { p.Balance.Type = "" }, "type"},
		{"未知来源", func(p *Provider) { p.Balance.Source = "unknown" }, "source"},
		{"账户缺令牌", func(p *Provider) { p.Balance.Source = "account" }, "accessToken"},
		{"互斥令牌", func(p *Provider) { p.Balance.Source = "apiKey"; p.Balance.AccessToken = "fake-token" }, "accessToken"},
		{"令牌控制字符", func(p *Provider) { p.Balance.AccessToken = "fake-token\n" }, "accessToken"},
		{"令牌超长", func(p *Provider) { p.Balance.AccessToken = strings.Repeat("x", 16385) }, "accessToken"},
		{"令牌长度边界", func(p *Provider) { p.Balance.AccessToken = strings.Repeat("x", 16384) }, ""},
		{"账户隐藏绑定", func(p *Provider) {
			p.Balance.Source = "account"
			p.Balance.AccessToken = "fake-token"
			p.Balance.APIKeyID = "one"
		}, "apiKeyId"},
		{"auto账户隐藏绑定", func(p *Provider) { p.Balance.AccessToken = "fake-token"; p.Balance.APIKeyID = "one" }, "apiKeyId"},
		{"列表缺绑定", func(p *Provider) { p.APIKeys = makeProviderKeys(1) }, "apiKeyId"},
		{"列表未知绑定", func(p *Provider) { p.APIKeys = makeProviderKeys(1); p.Balance.APIKeyID = "unknown" }, "apiKeyId"},
		{"单值隐藏绑定", func(p *Provider) { p.Balance.APIKeyID = "one" }, "apiKeyId"},
		{"跨站密钥", func(p *Provider) { p.Balance.BaseURL = "https://other.example.invalid" }, "baseUrl"},
		{"跨协议密钥", func(p *Provider) { p.Balance.BaseURL = "http://api.example.invalid" }, "baseUrl"},
		{"跨端口密钥", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid:8443" }, "baseUrl"},
		{"默认端口同origin", func(p *Provider) { p.Balance.BaseURL = "https://API.EXAMPLE.INVALID:443/admin" }, ""},
		{"禁止URL凭据", func(p *Provider) { p.Balance.BaseURL = "https://u:fake-token@api.example.invalid" }, "baseUrl"},
		{"禁止查询", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid?token=fake-token" }, "baseUrl"},
		{"禁止空查询", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid?" }, "baseUrl"},
		{"禁止片段", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid#fake-token" }, "baseUrl"},
		{"禁止空片段", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid#" }, "baseUrl"},
		{"非法协议", func(p *Provider) { p.Balance.BaseURL = "file:///tmp/fake" }, "baseUrl"},
		{"非法端口", func(p *Provider) { p.Balance.BaseURL = "https://api.example.invalid:65536" }, "baseUrl"},
		{"用户ID上限", func(p *Provider) { p.Balance.UserID = "9223372036854775807" }, ""},
		{"用户ID溢出", func(p *Provider) { p.Balance.UserID = "9223372036854775808" }, "userId"},
		{"用户ID零", func(p *Provider) { p.Balance.UserID = "0" }, "userId"},
		{"用户ID正号", func(p *Provider) { p.Balance.UserID = "+1" }, "userId"},
		{"sub2api禁止用户ID", func(p *Provider) { p.Balance.Type = "sub2api"; p.Balance.UserID = "1" }, "userId"},
		{"sub2api禁止换算", func(p *Provider) { p.Balance.Type = "sub2api"; p.Balance.QuotaPerUnit = "1" }, "quotaPerUnit"},
		{"手动刷新", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(0) }, ""},
		{"最短周期", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(5) }, ""},
		{"最长周期", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(1440) }, ""},
		{"负周期", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(-1) }, "refreshIntervalMinutes"},
		{"过短周期", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(4) }, "refreshIntervalMinutes"},
		{"过长周期", func(p *Provider) { p.Balance.RefreshIntervalMinutes = intPtr(1441) }, "refreshIntervalMinutes"},
		{"普通提供商禁止目录", func(p *Provider) { p.ModelCatalog = &ModelCatalog{} }, "modelCatalog"},
		{"OAuth禁止余额", func(p *Provider) { p.AuthType = "chatgptOAuth" }, "balance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := queryTestProvider()
			tt.edit(p)
			err := validateProviderQueries("p", p)
			if tt.field == "" && err != nil {
				t.Fatal(err)
			}
			if tt.field != "" && (err == nil || !strings.Contains(err.Error(), tt.field)) {
				t.Fatalf("错误未指明字段 %s：%v", tt.field, err)
			}
			if err != nil && strings.Contains(err.Error(), "fake-token") {
				t.Fatal("错误泄露令牌")
			}
		})
	}
}

func TestQuotaPerUnitBounds(t *testing.T) {
	for _, value := range []string{"1", "0001.50", "1000000000000000000", "0.000000000000000001", strings.Repeat("0", 63) + "1"} {
		if !validQuotaPerUnit(value) {
			t.Errorf("有效换算值被拒绝：%q", value)
		}
	}
	for _, value := range []string{"0", "0.0", "-1", "+1", ".5", "1.", "1e3", "1/2", " 1", "1 ", "1000000000000000000.000000000000000001", "1000000000000000001", "0.0000000000000000001", strings.Repeat("0", 64) + "1"} {
		if validQuotaPerUnit(value) {
			t.Errorf("非法换算值被接受：%q", value)
		}
	}
}

func TestModelCatalogVersions(t *testing.T) {
	for _, value := range []string{"", "0.167.0", "999999.999999.999999", "000001.02.3"} {
		p := &Provider{AuthType: "chatgptOAuth", ModelCatalog: &ModelCatalog{ClientVersion: value}}
		if err := validateProviderQueries("p", p); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"1.2", "1.2.3.4", "v1.2.3", "1.2.-3", "1000000.2.3", "1.2.3\n", strings.Repeat("1", 33)} {
		p := &Provider{AuthType: "chatgptOAuth", ModelCatalog: &ModelCatalog{ClientVersion: value}}
		if err := validateProviderQueries("p", p); err == nil {
			t.Errorf("非法版本被接受：%q", value)
		}
	}
}

func TestQueryAccessorsAndNormalization(t *testing.T) {
	var empty *ProviderBalance
	var absent *Provider
	if empty.IntervalMinutes() != 0 || empty.EffectiveSource() != "" || absent.BalanceBaseURL() != "" || absent.ModelCatalogVersion() != "0.167.0" {
		t.Fatal("空配置默认值错误")
	}
	b := &ProviderBalance{Type: "newapi"}
	if b.EffectiveSource() != "apiKey" {
		t.Fatal("auto应使用密钥")
	}
	b.AccessToken = "fake-token"
	if b.EffectiveSource() != "account" {
		t.Fatal("auto应使用独立账户令牌")
	}
	b.RefreshIntervalMinutes = intPtr(5)
	if b.IntervalMinutes() != 5 {
		t.Fatal("周期未生效")
	}
	for _, tt := range []struct{ base, explicit, want string }{
		{"https://API.EXAMPLE.INVALID:443/team/v1/", "", "https://api.example.invalid/team"},
		{"http://api.example.invalid:80/v1", "", "http://api.example.invalid"},
		{"https://api.example.invalid/custom", "", "https://api.example.invalid/custom"},
		{"https://api.example.invalid/team/v1", "https://panel.example.invalid/manage/v1/", "https://panel.example.invalid/manage/v1"},
		{"https://api.example.invalid/team%2Fv1", "", "https://api.example.invalid/team%2Fv1"},
		{"http://[::1]:80/team/v1", "", "http://[::1]/team"},
	} {
		p := &Provider{BaseURL: tt.base, Balance: &ProviderBalance{Type: "newapi", BaseURL: tt.explicit}}
		if got := p.BalanceBaseURL(); got != tt.want {
			t.Errorf("基址=%q，期望 %q", got, tt.want)
		}
	}
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["primary"]
	_ = p.ModelCatalogVersion()
	_ = p.BalanceBaseURL()
	applyDefaults(cfg)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Balance != nil || p.ModelCatalog != nil || bytes.Contains(raw, []byte("modelCatalog:")) || bytes.Contains(raw, []byte("balance:")) {
		t.Fatal("未配置块被默认值物化")
	}
}

func TestQueryConfigStrictDecodeAndSecretErrors(t *testing.T) {
	for _, block := range []string{
		"balance: {type: newapi, accessToken: [fake-sensitive-token]}",
		"balance: {type: newapi, accessToken: {fake-sensitive-token: x}}",
		"balance: {type: newapi, accessToken: 123}",
		"balance: {type: newapi, fake-sensitive-token: x}",
		"balance: {type: newapi, refreshIntervalMinutes: fake-sensitive-token}",
		"balance: {type: newapi, refreshIntervalMinutes: 999999999999999999999999}",
		"balance: {type: newapi, accessToken: \"fake-sensitive-token\\n\"}",
		"modelCatalog: {clientVersion: [fake-sensitive-token]}",
		"modelCatalog: {baseUrl: fake-sensitive-token}",
	} {
		raw := strings.Replace(validConfigYAML(), "    format: openai", "    format: openai\n    "+block, 1)
		_, err := DecodeAndValidate([]byte(raw))
		if err == nil {
			t.Errorf("非法配置被接受：%s", block)
		} else if strings.Contains(err.Error(), "fake-sensitive-token") {
			t.Fatal("解析错误泄露秘密")
		}
	}
}

func TestRedactBalanceTokenYAML(t *testing.T) {
	raw := []byte(`providers:
  first:
    balance: &query
      type: newapi
      accessToken: &token fake-sensitive-token
  second:
    balance:
      <<: *query
  third:
    balance:
      type: newapi
      accessToken: *token
  empty:
    balance:
      type: newapi
      accessToken: ""
`)
	out, err := RedactYAML(raw, APIKeyKeepSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("fake-sensitive-token")) || !bytes.Contains(out, []byte(BalanceTokenKeepSentinel)) {
		t.Fatal("查询令牌未安全脱敏")
	}
	var parsed Config
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second", "third"} {
		if parsed.Providers[name].Balance.AccessToken != BalanceTokenKeepSentinel {
			t.Fatal("别名或合并字段未脱敏")
		}
	}
	if parsed.Providers["empty"].Balance.AccessToken != "" {
		t.Fatal("空令牌被物化")
	}
}

func TestTransferQuerySecretsAndClone(t *testing.T) {
	current := transferTestConfig(t)
	p := current.Providers["primary"]
	p.Balance = &ProviderBalance{Type: "newapi", Source: "account", AccessToken: "fake-sensitive-token", RefreshIntervalMinutes: intPtr(5)}
	current.Providers["oauth"] = &Provider{BaseURL: "https://chatgpt.com/backend-api/codex", Format: "openai-responses", AuthType: "chatgptOAuth", AccountID: "fake-account", ModelCatalog: &ModelCatalog{ClientVersion: "1.2.3"}, MaxConcurrent: 5, MaxQueueWait: 30000}
	exported, err := ExportTransfer(current, TransferExportRequest{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(exported, []byte("fake-sensitive-token")) {
		t.Fatal("敏感导出遗漏查询令牌")
	}
	clone, err := cloneTransferConfig(current)
	if err != nil {
		t.Fatal(err)
	}
	clone.Providers["primary"].Balance.AccessToken = "fake-other"
	*clone.Providers["primary"].Balance.RefreshIntervalMinutes = 10
	clone.Providers["oauth"].ModelCatalog.ClientVersion = "2.3.4"
	if p.Balance.AccessToken != "fake-sensitive-token" || p.Balance.IntervalMinutes() != 5 || current.Providers["oauth"].ModelCatalog.ClientVersion != "1.2.3" {
		t.Fatal("克隆共享新增配置指针")
	}
	target := transferTestConfig(t)
	plan, err := PlanTransfer(target, exported, TransferOptions{Mode: "merge", Conflicts: &TransferConflicts{Providers: map[string]string{"primary": "overwrite"}}}, map[string]bool{"fake-account": true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range plan.Catalog.Providers {
		if entry.Name == "primary" {
			found = entry.BalanceTokenConfigured
		}
	}
	if !found {
		t.Fatal("预览未标记查询令牌存在")
	}
	preview, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(preview, []byte("fake-sensitive-token")) {
		t.Fatal("普通预览泄露查询令牌")
	}
	if plan.Config == nil || plan.Config.Providers["primary"].Balance.AccessToken != "fake-sensitive-token" || plan.Config.Providers["oauth"].ModelCatalogVersion() != "1.2.3" {
		t.Fatal("迁移遗漏新增配置")
	}
	p.Balance.AccessToken = BalanceTokenKeepSentinel
	bundle := transferTestBundle(t, map[string]*Provider{"p": p}, nil)
	if _, err := decodeTransfer(bundle); err == nil {
		t.Fatal("迁移接受保留令牌占位符")
	}
}
