package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGlobalUpstreamExtrasDecodeAndRoundTrip(t *testing.T) {
	raw := strings.Replace(validConfigYAML(), "cache:\n", `globalUserAgent:
  anthropic: "gateway-anthropic/1.0"
  openai: "gateway-openai/1.0"
  openai-responses: "gateway-responses/1.0"
globalUpstreamExtras:
  anthropic:
    headers:
      anthropic-beta: prompt-caching-2024-07-31
    body:
      cache_control:
        type: ephemeral
  openai:
    headers:
      x-openai-default: chat
    body:
      enable_thinking: true
  openai-responses:
    headers:
      x-responses-default: responses
    body:
      reasoning:
        effort: high
cache:
`, 1)

	cfg, err := DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeAndValidate() error = %v", err)
	}
	for format, want := range map[string]string{
		"anthropic": "gateway-anthropic/1.0", "openai": "gateway-openai/1.0", "openai-responses": "gateway-responses/1.0",
	} {
		if got := cfg.GlobalUserAgent.ForFormat(format); got != want {
			t.Errorf("GlobalUserAgent.ForFormat(%q) = %q, want %q", format, got, want)
		}
	}

	for _, tt := range []struct {
		format    string
		headerKey string
		bodyKey   string
	}{
		{format: "anthropic", headerKey: "anthropic-beta", bodyKey: "cache_control"},
		{format: "openai", headerKey: "x-openai-default", bodyKey: "enable_thinking"},
		{format: "openai-responses", headerKey: "x-responses-default", bodyKey: "reasoning"},
	} {
		extra := cfg.GlobalUpstreamExtras.ForFormat(tt.format)
		if extra == nil {
			t.Fatalf("ForFormat(%q) = nil", tt.format)
		}
		if extra.Headers[tt.headerKey] == "" {
			t.Errorf("ForFormat(%q).Headers = %#v, 缺少 %q", tt.format, extra.Headers, tt.headerKey)
		}
		if _, ok := extra.Body[tt.bodyKey]; !ok {
			t.Errorf("ForFormat(%q).Body = %#v, 缺少 %q", tt.format, extra.Body, tt.bodyKey)
		}
	}
	if got := cfg.GlobalUpstreamExtras.ForFormat("unknown"); got != nil {
		t.Fatalf("ForFormat(unknown) = %#v, want nil", got)
	}

	marshaled, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	if !strings.Contains(string(marshaled), "globalUpstreamExtras:") || !strings.Contains(string(marshaled), "globalUserAgent:") {
		t.Fatalf("全局默认项未写回 YAML:\n%s", marshaled)
	}
	reloaded, err := DecodeAndValidate(marshaled)
	if err != nil {
		t.Fatalf("写回后的 DecodeAndValidate() error = %v", err)
	}
	if got := reloaded.GlobalUpstreamExtras.ForFormat("openai").Body["enable_thinking"]; got != true {
		t.Fatalf("round-trip openai body = %#v, want true", got)
	}
}

func TestGlobalUpstreamExtrasRejectInvalidEntries(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
	}{
		{
			name: "unknown format is rejected by strict decode",
			block: `globalUpstreamExtras:
  mistral:
    body:
      parallel_tool_calls: true
`,
			wantErr: "mistral",
		},
		{
			name: "unknown nested field is rejected",
			block: `globalUpstreamExtras:
  openai:
    unexpected: true
`,
			wantErr: "unexpected",
		},
		{
			name: "managed user agent requires dedicated global field",
			block: `globalUpstreamExtras:
  openai:
    headers:
      User-Agent: hijacked
`,
			wantErr: "globalUserAgent",
		},
		{
			name: "managed body fields stay blocked",
			block: `globalUpstreamExtras:
  openai-responses:
    body:
      model: hijacked
`,
			wantErr: "globalUpstreamExtras.openai-responses.body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(tt.block + validConfigYAML()))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestGlobalUpstreamExtrasAndUserAgentKeepEmptyConfigClean(t *testing.T) {
	raw := "globalUpstreamExtras:\n  openai: {}\n" + validConfigYAML()
	cfg, err := DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeAndValidate() error = %v", err)
	}
	if cfg.GlobalUpstreamExtras != nil {
		t.Fatalf("空 globalUpstreamExtras = %#v, want nil", cfg.GlobalUpstreamExtras)
	}
	if cfg.GlobalUserAgent != nil {
		t.Fatalf("未配置 GlobalUserAgent = %#v, want nil", cfg.GlobalUserAgent)
	}

	marshaled, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	if strings.Contains(string(marshaled), "globalUpstreamExtras") || strings.Contains(string(marshaled), "globalUserAgent") {
		t.Fatalf("空全局项不应写回 YAML:\n%s", marshaled)
	}

	cfg.GlobalUserAgent = &GlobalUserAgents{OpenAI: strings.Repeat("名", maxProviderUserAgentRunes+1)}
	if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "globalUserAgent.openai 长度应不超过") {
		t.Fatalf("超长 globalUserAgent 校验错误 = %v", err)
	}
	cfg.GlobalUserAgent = &GlobalUserAgents{Anthropic: "bad\nagent"}
	if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "globalUserAgent.anthropic 不能包含 ASCII 控制字符") {
		t.Fatalf("非法 globalUserAgent 校验错误 = %v", err)
	}
}
