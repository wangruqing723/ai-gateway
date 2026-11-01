package config

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateProviderKeyValues(t *testing.T) {
	tests := []struct {
		name    string
		keys    []ProviderKey
		wantErr string
	}{
		{name: "valid list", keys: []ProviderKey{{ID: "key_1", Value: "fake-first"}, {ID: "key-2", Value: "fake-second"}}},
		{name: "empty value", keys: []ProviderKey{{ID: "k1", Value: ""}}, wantErr: "providers.p.apiKeys[0].value 不能为空"},
		{name: "control in value", keys: []ProviderKey{{ID: "k1", Value: "fake\nvalue"}}, wantErr: "providers.p.apiKeys[0].value 不能包含控制字符"},
		{name: "duplicate value", keys: []ProviderKey{{ID: "k1", Value: "fake-same"}, {ID: "k2", Value: "fake-same"}}, wantErr: "providers.p.apiKeys[1].value 与 apiKeys[0] 重复"},
		{name: "sentinel is checked after回填", keys: []ProviderKey{{ID: "k1", Value: APIKeyKeepSentinel}, {ID: "k2", Value: APIKeyKeepSentinel}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderKeyValues("p", &Provider{APIKeys: tt.keys})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("ValidateProviderKeyValues() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("ValidateProviderKeyValues() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateProviderAPIKeys(t *testing.T) {
	validKey := ProviderKey{ID: "k1", Value: "fake-value"}
	tests := []struct {
		name     string
		provider *Provider
		wantErr  string
	}{
		{name: "valid list", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{validKey}}},
		{name: "disabled keys remain valid", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{ID: "k1", Value: "fake-value", Enabled: boolPtr(false)}}}},
		{name: "apiKey and list conflict", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKey: "fake-single", APIKeys: []ProviderKey{validKey}}, wantErr: "providers.p 不能同时配置 apiKey 与 apiKeys"},
		{name: "too many keys", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: makeProviderKeys(11)}, wantErr: "providers.p.apiKeys 最多 10 个（当前 11）"},
		{name: "missing identity", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{Value: "fake-value"}}}, wantErr: "providers.p.apiKeys[0].id 须为 1-32 位字母、数字、下划线或连字符"},
		{name: "invalid identity", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{ID: "bad/id", Value: "fake-value"}}}, wantErr: "providers.p.apiKeys[0].id 须为 1-32 位字母、数字、下划线或连字符"},
		{name: "duplicate identity", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{ID: "k1", Value: "fake-first"}, {ID: "k1", Value: "fake-second"}}}, wantErr: "providers.p.apiKeys[1].id 重复: k1"},
		{name: "long name", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{ID: "k1", Name: strings.Repeat("名", 65), Value: "fake-value"}}}, wantErr: "providers.p.apiKeys[0].name 长度应不超过 64 个字符（当前 65）"},
		{name: "control in name", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{{ID: "k1", Name: "bad\tname", Value: "fake-value"}}}, wantErr: "providers.p.apiKeys[0].name 不能包含控制字符"},
		{name: "cooldown without list", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", KeyCooldown: &KeyCooldown{}}, wantErr: "providers.p.keyCooldown 仅可与非空 apiKeys 一起使用"},
		{name: "auth cooldown below range", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{validKey}, KeyCooldown: &KeyCooldown{AuthMs: intPtr(-1)}}, wantErr: "providers.p.keyCooldown.authMs 应在 0-86400000 之间"},
		{name: "quota cooldown above range", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{validKey}, KeyCooldown: &KeyCooldown{QuotaMs: intPtr(86_400_001)}}, wantErr: "providers.p.keyCooldown.quotaMs 应在 0-86400000 之间"},
		{name: "rate cooldown boundary accepted", provider: &Provider{BaseURL: "https://api.example.com", Format: "openai", APIKeys: []ProviderKey{validKey}, KeyCooldown: &KeyCooldown{RateLimitMs: intPtr(86_400_000)}}},
		{name: "OAuth conflict", provider: &Provider{BaseURL: "https://chatgpt.com/backend-api/codex", Format: "openai-responses", AuthType: "chatgptOAuth", AccountID: "account", APIKeys: []ProviderKey{validKey}}, wantErr: "providers.p 使用 ChatGPT OAuth 时不能同时配置 apiKeys"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProvider("p", tt.provider, false)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("validateProvider() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("validateProvider() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func makeProviderKeys(count int) []ProviderKey {
	keys := make([]ProviderKey, count)
	for i := range keys {
		keys[i] = ProviderKey{ID: fmt.Sprintf("k%d", i), Value: fmt.Sprintf("fake-%d", i)}
	}
	return keys
}

func TestProviderKeyAccessorsAndLegacyConfigRoundTrip(t *testing.T) {
	legacy, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	emptyListRaw := strings.Replace(validConfigYAML(), "    apiKey: \"\"", "    apiKey: \"\"\n    apiKeys: []", 1)
	emptyList, err := DecodeAndValidate([]byte(emptyListRaw))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Providers["primary"].APIKey != emptyList.Providers["primary"].APIKey || len(emptyList.Providers["primary"].APIKeys) != 0 {
		t.Fatalf("旧 apiKey 与空列表解析不等价: legacy=%#v empty=%#v", legacy.Providers["primary"], emptyList.Providers["primary"])
	}
	encoded, err := yaml.Marshal(emptyList)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "apiKeys:") || strings.Contains(string(encoded), "keyCooldown:") {
		t.Fatalf("空的新字段被回写到配置: %s", encoded)
	}

	provider := &Provider{}
	if got := []int{provider.KeyCooldownFor("auth"), provider.KeyCooldownFor("quota"), provider.KeyCooldownFor("rateLimit")}; fmt.Sprint(got) != "[300000 60000 30000]" {
		t.Fatalf("默认冷却时长 = %v", got)
	}
	provider.KeyCooldown = &KeyCooldown{AuthMs: intPtr(0)}
	if got := provider.KeyCooldownFor("auth"); got != 0 {
		t.Fatalf("显式零值冷却 = %d", got)
	}
}

func TestRedactYAMLRedactsProviderKeyValuesOnly(t *testing.T) {
	raw := `providers:
  p:
    baseUrl: https://api.example.com
    format: openai
    apiKey: fake-single
    apiKeys:
      - id: k1
        value: fake-list-value
    extraBody:
      value: keep-this-value
routes:
  - match: "*"
    provider: p
    model: upstream
`
	redacted, err := RedactYAML([]byte(raw), APIKeyKeepSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "fake-single") || strings.Contains(string(redacted), "fake-list-value") {
		t.Fatalf("密钥未脱敏: %s", redacted)
	}
	if !strings.Contains(string(redacted), "keep-this-value") {
		t.Fatalf("extraBody.value 被误脱敏: %s", redacted)
	}
	var decoded struct {
		Providers map[string]struct {
			APIKey    string         `yaml:"apiKey"`
			APIKeys   []ProviderKey  `yaml:"apiKeys"`
			ExtraBody map[string]any `yaml:"extraBody"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(redacted, &decoded); err != nil {
		t.Fatal(err)
	}
	provider := decoded.Providers["p"]
	if provider.APIKey != APIKeyKeepSentinel || provider.APIKeys[0].Value != APIKeyKeepSentinel || provider.ExtraBody["value"] != "keep-this-value" {
		t.Fatalf("脱敏结果不正确: %#v", provider)
	}
}
