package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func validConfigYAML() string {
	return `port: 7789
host: 127.0.0.1
timeout: 120000
streamActivityTimeout: 60000
directMode: false
directTimeoutNoStream: 60000
directTimeoutStreamHeader: 60000
directTimeoutStreamActive: 120000
cache:
  maxAgeDays: 7
  maxRecords: 1000
providers:
  primary:
    baseUrl: https://api.example.com
    apiKey: ""
    format: openai
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "*"
    provider: primary
    model: upstream-model
`
}

func TestDecodeAndValidateRejectsNilProvider(t *testing.T) {
	raw := strings.Replace(validConfigYAML(), `    baseUrl: https://api.example.com
    apiKey: ""
    format: openai
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000`, "", 1)

	if _, err := DecodeAndValidate([]byte(raw)); err == nil || !strings.Contains(err.Error(), "providers.primary") {
		t.Fatalf("DecodeAndValidate() error = %v, want nil provider error", err)
	}
}

func TestDecodeAndValidateRejectsUnknownFields(t *testing.T) {
	raw := strings.Replace(validConfigYAML(), "timeout: 120000", "timeout: 120000\nunknownOption: true", 1)

	if _, err := DecodeAndValidate([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknownOption") {
		t.Fatalf("DecodeAndValidate() error = %v, want unknown field error", err)
	}
}

func TestDecodeAndValidateRejectsTrailingDocument(t *testing.T) {
	raw := validConfigYAML() + "---\nport: 7789\n"

	if _, err := DecodeAndValidate([]byte(raw)); err == nil {
		t.Fatal("DecodeAndValidate() error = nil, want trailing document error")
	}
}

func TestDecodeAndValidateAppliesDefaults(t *testing.T) {
	raw := `providers:
  primary:
    baseUrl: https://api.example.com
    format: openai
routes:
  - match: "*"
    provider: primary
    model: upstream-model
`

	cfg, err := DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeAndValidate() error = %v", err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 7789 || cfg.Timeout != 120000 || cfg.StreamActivityTimeout != 60000 {
		t.Fatalf("global defaults = %#v", cfg)
	}
	if cfg.Cache.MaxAgeDays != 7 || cfg.Cache.MaxRecords != 1000 {
		t.Fatalf("cache defaults = %#v", cfg.Cache)
	}
	p := cfg.Providers["primary"]
	if p.Name != "primary" || p.MaxConcurrent != 5 || p.MaxQueueWait != 30000 {
		t.Fatalf("provider defaults = %#v", p)
	}
}

func TestDecodeAndValidateNumericBounds(t *testing.T) {
	tests := []struct {
		name string
		old  string
		bad  string
		want string
	}{
		{name: "blank host", old: "host: 127.0.0.1", bad: `host: "   "`, want: "host"},
		{name: "host with port", old: "host: 127.0.0.1", bad: `host: "127.0.0.1:9000"`, want: "host"},
		{name: "bracketed ipv6", old: "host: 127.0.0.1", bad: `host: "[::1]"`, want: "host"},
		{name: "invalid colon", old: "host: 127.0.0.1", bad: `host: ":"`, want: "host"},
		{name: "port below", old: "port: 7789", bad: "port: -1", want: "port"},
		{name: "port above", old: "port: 7789", bad: "port: 65536", want: "port"},
		{name: "timeout below", old: "timeout: 120000", bad: "timeout: 999", want: "timeout"},
		{name: "timeout above", old: "timeout: 120000", bad: "timeout: 300001", want: "timeout"},
		{name: "activity below", old: "streamActivityTimeout: 60000", bad: "streamActivityTimeout: 4999", want: "streamActivityTimeout"},
		{name: "activity above", old: "streamActivityTimeout: 60000", bad: "streamActivityTimeout: 600001", want: "streamActivityTimeout"},
		{name: "direct no stream below", old: "directTimeoutNoStream: 60000", bad: "directTimeoutNoStream: 999", want: "directTimeoutNoStream"},
		{name: "direct no stream above", old: "directTimeoutNoStream: 60000", bad: "directTimeoutNoStream: 300001", want: "directTimeoutNoStream"},
		{name: "direct header below", old: "directTimeoutStreamHeader: 60000", bad: "directTimeoutStreamHeader: 999", want: "directTimeoutStreamHeader"},
		{name: "direct header above", old: "directTimeoutStreamHeader: 60000", bad: "directTimeoutStreamHeader: 300001", want: "directTimeoutStreamHeader"},
		{name: "direct active below", old: "directTimeoutStreamActive: 120000", bad: "directTimeoutStreamActive: 4999", want: "directTimeoutStreamActive"},
		{name: "direct active above", old: "directTimeoutStreamActive: 120000", bad: "directTimeoutStreamActive: 600001", want: "directTimeoutStreamActive"},
		{name: "cache age below", old: "maxAgeDays: 7", bad: "maxAgeDays: -1", want: "cache.maxAgeDays"},
		{name: "cache age above", old: "maxAgeDays: 7", bad: "maxAgeDays: 3651", want: "cache.maxAgeDays"},
		{name: "cache records below", old: "maxRecords: 1000", bad: "maxRecords: -1", want: "cache.maxRecords"},
		{name: "cache records above", old: "maxRecords: 1000", bad: "maxRecords: 1000001", want: "cache.maxRecords"},
		{name: "concurrency below", old: "maxConcurrent: 5", bad: "maxConcurrent: -1", want: "maxConcurrent"},
		{name: "concurrency above", old: "maxConcurrent: 5", bad: "maxConcurrent: 101", want: "maxConcurrent"},
		{name: "rate below", old: "maxPerSecond: 0", bad: "maxPerSecond: -1", want: "maxPerSecond"},
		{name: "rate above", old: "maxPerSecond: 0", bad: "maxPerSecond: 101", want: "maxPerSecond"},
		{name: "queue wait below", old: "maxQueueWait: 30000", bad: "maxQueueWait: -1", want: "maxQueueWait"},
		{name: "queue wait above", old: "maxQueueWait: 30000", bad: "maxQueueWait: 600001", want: "maxQueueWait"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(), tt.old, tt.bad, 1)
			if raw == validConfigYAML() {
				t.Fatalf("test replacement %q did not match", tt.old)
			}
			_, err := DecodeAndValidate([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDecodeAndValidateAcceptsExactNumericBounds(t *testing.T) {
	tests := []struct {
		name string
		old  string
		good string
	}{
		{name: "port min", old: "port: 7789", good: "port: 1"},
		{name: "port max", old: "port: 7789", good: "port: 65535"},
		{name: "timeout min", old: "timeout: 120000", good: "timeout: 1000"},
		{name: "timeout max", old: "timeout: 120000", good: "timeout: 300000"},
		{name: "activity min", old: "streamActivityTimeout: 60000", good: "streamActivityTimeout: 5000"},
		{name: "activity max", old: "streamActivityTimeout: 60000", good: "streamActivityTimeout: 600000"},
		{name: "direct no stream min", old: "directTimeoutNoStream: 60000", good: "directTimeoutNoStream: 1000"},
		{name: "direct no stream max", old: "directTimeoutNoStream: 60000", good: "directTimeoutNoStream: 300000"},
		{name: "direct header min", old: "directTimeoutStreamHeader: 60000", good: "directTimeoutStreamHeader: 1000"},
		{name: "direct header max", old: "directTimeoutStreamHeader: 60000", good: "directTimeoutStreamHeader: 300000"},
		{name: "direct active min", old: "directTimeoutStreamActive: 120000", good: "directTimeoutStreamActive: 5000"},
		{name: "direct active max", old: "directTimeoutStreamActive: 120000", good: "directTimeoutStreamActive: 600000"},
		{name: "cache age min", old: "maxAgeDays: 7", good: "maxAgeDays: 1"},
		{name: "cache age max", old: "maxAgeDays: 7", good: "maxAgeDays: 3650"},
		{name: "cache records min", old: "maxRecords: 1000", good: "maxRecords: 1"},
		{name: "cache records max", old: "maxRecords: 1000", good: "maxRecords: 1000000"},
		{name: "concurrency min", old: "maxConcurrent: 5", good: "maxConcurrent: 1"},
		{name: "concurrency max", old: "maxConcurrent: 5", good: "maxConcurrent: 100"},
		{name: "rate min", old: "maxPerSecond: 0", good: "maxPerSecond: 0"},
		{name: "rate max", old: "maxPerSecond: 0", good: "maxPerSecond: 100"},
		{name: "queue wait min", old: "maxQueueWait: 30000", good: "maxQueueWait: 1"},
		{name: "queue wait max", old: "maxQueueWait: 30000", good: "maxQueueWait: 600000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(), tt.old, tt.good, 1)
			if _, err := DecodeAndValidate([]byte(raw)); err != nil {
				t.Fatalf("DecodeAndValidate() error = %v", err)
			}
		})
	}
}

func TestDecodeAndValidateListenHosts(t *testing.T) {
	for _, host := range []string{"localhost", "0.0.0.0", "127.0.0.1", "::", "::1", "host.docker.internal"} {
		t.Run(host, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(), "host: 127.0.0.1", fmt.Sprintf("host: %q", host), 1)
			if _, err := DecodeAndValidate([]byte(raw)); err != nil {
				t.Fatalf("host %q rejected: %v", host, err)
			}
		})
	}
}

func TestDecodeAndValidateProviderAndRouteFields(t *testing.T) {
	tests := []struct {
		name string
		old  string
		bad  string
		want string
	}{
		{name: "invalid base URL", old: "baseUrl: https://api.example.com", bad: "baseUrl: not-a-url", want: "baseUrl"},
		{name: "invalid format", old: "format: openai", bad: "format: custom", want: "format"},
		{name: "blank match", old: `match: "*"`, bad: `match: "   "`, want: "match"},
		{name: "blank provider", old: "provider: primary", bad: `provider: "   "`, want: "provider"},
		{name: "unknown provider", old: "provider: primary", bad: "provider: missing", want: "未定义"},
		{name: "missing model", old: "model: upstream-model", bad: `model: ""`, want: "model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(validConfigYAML(), tt.old, tt.bad, 1)
			_, err := DecodeAndValidate([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// provider 名称长度按 rune 计：中文名按字节算会只剩三分之一额度。
// 边界两侧都要测，否则把 > 写成 >= 也能过。
func TestDecodeAndValidateProviderNameLength(t *testing.T) {
	tests := []struct {
		name    string
		runes   int
		char    string
		wantErr bool
	}{
		{name: "ascii at limit", runes: maxProviderNameRunes, char: "a"},
		{name: "ascii over limit", runes: maxProviderNameRunes + 1, char: "a", wantErr: true},
		{name: "cjk at limit", runes: maxProviderNameRunes, char: "名"},
		{name: "cjk over limit", runes: maxProviderNameRunes + 1, char: "名", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 同时替换 providers 的键和 routes 的引用，否则会撞上「provider 未定义」
			name := strings.Repeat(tt.char, tt.runes)
			raw := strings.ReplaceAll(validConfigYAML(), "primary", name)
			_, err := DecodeAndValidate([]byte(raw))
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "名称长度") {
					t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, "名称长度")
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeAndValidate() error = %v, want nil for %d 字符名称", err, tt.runes)
			}
		})
	}
}

func TestDecodeAndValidateVisionAndNestedKnownFields(t *testing.T) {
	withVision := strings.Replace(validConfigYAML(), "    model: upstream-model", `    model: upstream-model
    vision:
      provider: primary
      model: vision-model`, 1)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "blank vision provider", raw: strings.Replace(withVision, "provider: primary\n      model: vision-model", `provider: "   "
      model: vision-model`, 1), want: "vision"},
		{name: "unknown vision provider", raw: strings.Replace(withVision, "provider: primary\n      model: vision-model", "provider: missing\n      model: vision-model", 1), want: "未定义"},
		{name: "blank vision model", raw: strings.Replace(withVision, "model: vision-model", `model: "   "`, 1), want: "model"},
		{name: "provider unknown field", raw: strings.Replace(validConfigYAML(), "    format: openai", "    format: openai\n    extraProviderOption: true", 1), want: "extraProviderOption"},
		{name: "route unknown field JSON", raw: strings.Replace(validConfigYAML(), "    model: upstream-model", "    model: upstream-model\n    extraRouteOption: true", 1), want: "extraRouteOption"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAndValidate([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DecodeAndValidate() error = %v, want containing %q", err, tt.want)
			}
		})
	}

	jsonConfig := `{"providers":{"primary":{"baseUrl":"https://api.example.com","format":"openai","unknown":true}},"routes":[{"match":"*","provider":"primary","model":"m"}]}`
	if _, err := DecodeAndValidate([]byte(jsonConfig)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("strict JSON error = %v", err)
	}
}

func TestRedactYAMLHandlesProviderKeyStyles(t *testing.T) {
	raw := `providers:
  block:
    baseUrl: https://block.example.com
    format: openai
    apiKey: block-secret
  quoted: {baseUrl: https://quoted.example.com, format: anthropic, apiKey: "quoted-secret"}
  multiline:
    baseUrl: https://multiline.example.com
    format: openai
    apiKey: |-
      multi-secret-one
      multi-secret-two
  empty:
    baseUrl: https://empty.example.com
    format: openai
    apiKey: ""
routes:
  - match: "*"
    provider: block
    model: model
`

	redacted, err := RedactYAML([]byte(raw), APIKeyKeepSentinel)
	if err != nil {
		t.Fatalf("RedactYAML() error = %v", err)
	}
	for _, secret := range []string{"block-secret", "quoted-secret", "multi-secret-one", "multi-secret-two"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted YAML still contains %q:\n%s", secret, redacted)
		}
	}

	var decoded map[string]any
	if err := yaml.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redacted YAML is invalid: %v\n%s", err, redacted)
	}
	providers := decoded["providers"].(map[string]any)
	for _, name := range []string{"block", "quoted", "multiline"} {
		provider := providers[name].(map[string]any)
		if provider["apiKey"] != APIKeyKeepSentinel {
			t.Fatalf("providers.%s.apiKey = %#v, want sentinel", name, provider["apiKey"])
		}
	}
	if provider := providers["empty"].(map[string]any); provider["apiKey"] != "" {
		t.Fatalf("empty apiKey = %#v, want empty", provider["apiKey"])
	}
}

func TestRedactYAMLRejectsDuplicateKeysWithoutLeaking(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "duplicate apiKey",
			raw: `providers:
  p:
    baseUrl: https://api.example.com
    format: openai
    apiKey: first-secret
    apiKey: second-secret
`,
		},
		{
			name: "duplicate providers",
			raw: `providers:
  p: {baseUrl: https://api.example.com, format: openai, apiKey: first-secret}
providers:
  q: {baseUrl: https://api.example.com, format: openai, apiKey: second-secret}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redacted, err := RedactYAML([]byte(tt.raw), APIKeyKeepSentinel)
			if err == nil {
				t.Fatalf("RedactYAML() unexpectedly returned potentially ambiguous YAML: %s", redacted)
			}
			if len(redacted) != 0 {
				t.Fatalf("RedactYAML() returned data on error: %q", redacted)
			}
		})
	}
}

func TestRedactYAMLHandlesMergedProviderSecret(t *testing.T) {
	raw := `providers:
  primary:
    <<: {baseUrl: https://api.example.com, format: openai, apiKey: merged-secret}
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "*"
    provider: primary
    model: upstream
`
	redacted, err := RedactYAML([]byte(raw), APIKeyKeepSentinel)
	if err != nil {
		t.Fatalf("RedactYAML() error = %v", err)
	}
	if strings.Contains(string(redacted), "merged-secret") || !strings.Contains(string(redacted), APIKeyKeepSentinel) {
		t.Fatalf("merged provider secret leaked:\n%s", redacted)
	}
	if _, err := DecodeAndValidate(redacted); err != nil {
		t.Fatalf("redacted merged YAML is invalid: %v\n%s", err, redacted)
	}
}

func TestRedactYAMLPreservesAliasedEmptyAPIKey(t *testing.T) {
	raw := `providers:
  first:
    baseUrl: https://first.example.com
    format: openai
    apiKey: &empty ""
  second:
    baseUrl: https://second.example.com
    format: openai
    apiKey: *empty
routes:
  - match: "*"
    provider: second
    model: upstream
`
	redacted, err := RedactYAML([]byte(raw), APIKeyKeepSentinel)
	if err != nil {
		t.Fatalf("RedactYAML() error = %v", err)
	}
	if strings.Contains(string(redacted), APIKeyKeepSentinel) {
		t.Fatalf("empty aliased keys were changed to configured sentinels:\n%s", redacted)
	}
	cfg, err := DecodeAndValidate(redacted)
	if err != nil {
		t.Fatalf("redacted YAML is invalid: %v", err)
	}
	if cfg.Providers["first"].APIKey != "" || cfg.Providers["second"].APIKey != "" {
		t.Fatalf("empty alias semantics changed: %#v", cfg.Providers)
	}
}

func TestSaveUsesUniqueAtomicFilesAndMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyTemp := path + ".tmp"
	if err := os.WriteFile(legacyTemp, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}

	const writers = 24
	start := make(chan struct{})
	errCh := make(chan error, writers)
	readerStop := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-readerStop:
				readerDone <- nil
				return
			default:
				data, err := os.ReadFile(path)
				if err != nil {
					readerDone <- err
					return
				}
				if _, err := DecodeAndValidate(data); err != nil {
					readerDone <- fmt.Errorf("reader observed partial config: %w", err)
					return
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
			if err != nil {
				errCh <- err
				return
			}
			cfg.Path = path
			cfg.Providers["primary"].BaseURL = fmt.Sprintf("https://api-%d.example.com", i)
			errCh <- Save(cfg)
		}(i)
	}
	close(start)
	wg.Wait()
	close(readerStop)
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Save() error = %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAndValidate(data); err != nil {
		t.Fatalf("saved config is partial or invalid: %v\n%s", err, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary files leaked: %#v", entries)
	}
	if got := string(mustReadConfigTestFile(t, legacyTemp)); got != "do-not-touch" {
		t.Fatalf("legacy fixed temp file was reused: %q", got)
	}
}

func TestSaveCreateTempFailureFallsBackToDirectWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = path
	cfg.Port = 7790

	originalCreateTemp := createConfigTemp
	createConfigTemp = func(string, string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrPermission}
	}
	defer func() { createConfigTemp = originalCreateTemp }()
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() fallback error = %v", err)
	}
	saved, err := DecodeAndValidate(mustReadConfigTestFile(t, path))
	if err != nil || saved.Port != 7790 {
		t.Fatalf("direct fallback content = %#v, %v", saved, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("direct fallback mode = %04o", info.Mode().Perm())
	}
}

func TestSaveRenameFailureFallsBackAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = path
	cfg.Port = 7791

	originalRename := renameConfigFile
	renameConfigFile = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EBUSY}
	}
	defer func() { renameConfigFile = originalRename }()
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() rename fallback error = %v", err)
	}
	saved, err := DecodeAndValidate(mustReadConfigTestFile(t, path))
	if err != nil || saved.Port != 7791 {
		t.Fatalf("rename fallback content = %#v, %v", saved, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("rename fallback leaked temp files: %#v", entries)
	}
}

func TestSaveUnexpectedCreateTempFailurePreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte(validConfigYAML())
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeAndValidate(original)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = path
	cfg.Port = 7792

	originalCreateTemp := createConfigTemp
	createConfigTemp = func(string, string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: dir, Err: syscall.ENOSPC}
	}
	defer func() { createConfigTemp = originalCreateTemp }()
	if err := Save(cfg); err == nil || !strings.Contains(err.Error(), "创建临时文件失败") {
		t.Fatalf("Save() error = %v, want unexpected create failure", err)
	}
	if got := mustReadConfigTestFile(t, path); string(got) != string(original) {
		t.Fatalf("unexpected create failure changed original config:\n%s", got)
	}
}

func TestSaveUnexpectedRenameFailurePreservesOriginalAndTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte(validConfigYAML())
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeAndValidate(original)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = path
	cfg.Port = 7793

	originalRename := renameConfigFile
	renameConfigFile = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EIO}
	}
	defer func() { renameConfigFile = originalRename }()
	if err := Save(cfg); err == nil || !strings.Contains(err.Error(), "替换配置文件失败") {
		t.Fatalf("Save() error = %v, want unexpected rename failure", err)
	}
	if got := mustReadConfigTestFile(t, path); string(got) != string(original) {
		t.Fatalf("unexpected rename failure changed original config:\n%s", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("complete recovery temp was not preserved: %#v", entries)
	}
}

func TestSaveUnexpectedRenameFailureDoesNotUseDirectFallback(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = destination

	err = Save(cfg)
	if err == nil || !strings.Contains(err.Error(), "替换配置文件失败") || strings.Contains(err.Error(), "直接写入") {
		t.Fatalf("Save() error = %v, want unexpected rename failure without direct fallback", err)
	}
}

func mustReadConfigTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
