// Package config 负责加载、默认化、严格校验和安全保存 Go 网关的 YAML 配置。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"
)

const (
	// APIKeyKeepSentinel 是管理 API 用于表示“保留已有密钥”的精确占位符。
	APIKeyKeepSentinel = "__AI_GATEWAY_KEEP_API_KEY__"

	maxCacheAgeDays = 3650
	maxCacheRecords = 1_000_000
	maxQueueWaitMs  = 600_000
)

var (
	saveMu           sync.Mutex
	createConfigTemp = os.CreateTemp
	renameConfigFile = os.Rename
)

// Provider 上游 provider 定义
type Provider struct {
	Name          string `yaml:"-" json:"name,omitempty"`                    // 运行时填充（map 的 key）
	BaseURL       string `yaml:"baseUrl" json:"baseUrl"`                     // 上游地址
	APIKey        string `yaml:"apiKey" json:"apiKey"`                       // 密钥，留空则从客户端请求头提取
	Format        string `yaml:"format" json:"format"`                       // anthropic | openai
	MaxConcurrent int    `yaml:"maxConcurrent" json:"maxConcurrent"`         // 最大并发
	MaxPerSecond  int    `yaml:"maxPerSecond" json:"maxPerSecond"`           // 每秒最多请求数，0 表示不限
	MaxQueueWait  int    `yaml:"maxQueueWait" json:"maxQueueWait,omitempty"` // 队列最大等待（毫秒）
}

// Vision 路由上的视觉子配置
type Vision struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// Route 路由规则，按顺序匹配，首条命中生效
type Route struct {
	Match    string  `yaml:"match" json:"match"`
	Provider string  `yaml:"provider" json:"provider"`
	Model    string  `yaml:"model" json:"model"`
	Vision   *Vision `yaml:"vision" json:"vision,omitempty"`
}

// Cache 缓存配置
type Cache struct {
	MaxAgeDays int `yaml:"maxAgeDays" json:"maxAgeDays"`
	MaxRecords int `yaml:"maxRecords" json:"maxRecords"`
}

// Config 顶层配置
type Config struct {
	Port                  int                  `yaml:"port" json:"port"`
	Host                  string               `yaml:"host" json:"host"`
	Timeout               int                  `yaml:"timeout" json:"timeout"`                             // 请求超时（毫秒）
	StreamActivityTimeout int                  `yaml:"streamActivityTimeout" json:"streamActivityTimeout"` // 流式活跃超时（毫秒）
	Cache                 Cache                `yaml:"cache" json:"cache"`
	Providers             map[string]*Provider `yaml:"providers" json:"providers"`
	Routes                []Route              `yaml:"routes" json:"routes"`
	Path                  string               `yaml:"-" json:"path,omitempty"`

	// ── 直通模式（direct mode）──────────────────────────────
	// 开启后请求不进队列：跳过并发控制、限速与排队等待，直接转发到上游。
	// 限流交给上游自己用 429 处理（网关原样透传，不重试）。仅保留超时保护。
	// 默认 false，保持原有队列行为；Node 版不识别这些字段会自动忽略，互不影响。
	DirectMode                bool `yaml:"directMode" json:"directMode"`                               // 直通开关
	DirectTimeoutNoStream     int  `yaml:"directTimeoutNoStream" json:"directTimeoutNoStream"`         // 非流式整体超时（毫秒），默认 60000
	DirectTimeoutStreamHeader int  `yaml:"directTimeoutStreamHeader" json:"directTimeoutStreamHeader"` // 流式等响应头超时（毫秒），默认 60000
	DirectTimeoutStreamActive int  `yaml:"directTimeoutStreamActive" json:"directTimeoutStreamActive"` // 流式活跃超时（毫秒，中途无数据即断），默认 120000
}

// Load 读取并校验配置，查找顺序对齐 Node 版（优先项目目录，其次 ~/.config）。
func Load() (*Config, error) {
	path := findConfigPath()
	if path == "" {
		return nil, fmt.Errorf("未找到配置文件，请将 config.example.yaml 复制为 config.yaml")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败 (%s): %w", path, err)
	}

	c, err := DecodeAndValidate(raw)
	if err != nil {
		return nil, fmt.Errorf("配置解析失败 (%s): %w", path, err)
	}
	c.Path = path
	return c, nil
}

// findConfigPath 优先用项目目录下的 config.yaml，其次 ~/.config/ai-gateway/config.yaml
func findConfigPath() string {
	candidates := []string{"config.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "ai-gateway", "config.yaml"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// applyDefaults 填充默认值并回写 provider 名称
func applyDefaults(c *Config) {
	if c.Port == 0 {
		c.Port = 7789
	}
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Timeout == 0 {
		c.Timeout = 120000
	}
	if c.StreamActivityTimeout == 0 {
		c.StreamActivityTimeout = 60000
	}
	// 直通模式超时默认值（仅 directMode 开启时实际生效）
	if c.DirectTimeoutNoStream == 0 {
		c.DirectTimeoutNoStream = 60000
	}
	if c.DirectTimeoutStreamHeader == 0 {
		c.DirectTimeoutStreamHeader = 60000
	}
	if c.DirectTimeoutStreamActive == 0 {
		c.DirectTimeoutStreamActive = 120000
	}
	if c.Cache.MaxAgeDays == 0 {
		c.Cache.MaxAgeDays = 7
	}
	if c.Cache.MaxRecords == 0 {
		c.Cache.MaxRecords = 1000
	}
	for name, p := range c.Providers {
		if p == nil {
			continue
		}
		p.Name = name
		if p.MaxConcurrent == 0 {
			p.MaxConcurrent = 5
		}
		if p.MaxQueueWait == 0 {
			p.MaxQueueWait = 30000
		}
	}
}

// ReadRaw 读取 config.yaml 原始文本（给前端 YAML 编辑器用）
func ReadRaw(c *Config) ([]byte, error) {
	if c.Path == "" {
		return nil, fmt.Errorf("配置文件路径未知")
	}
	return os.ReadFile(c.Path)
}

// Save 将配置写回文件（先写同目录唯一临时文件再 rename，保证原子性）。
func Save(c *Config) error {
	if c == nil {
		return fmt.Errorf("配置为空")
	}
	if c.Path == "" {
		return fmt.Errorf("配置文件路径未知")
	}

	// 序列化为 YAML
	out, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}

	saveMu.Lock()
	defer saveMu.Unlock()

	dir := filepath.Dir(c.Path)
	tmp, err := createConfigTemp(dir, "."+filepath.Base(c.Path)+".tmp-*")
	if err != nil {
		// Docker 单文件挂载场景下，配置文件本身可写，但 /app 目录可能不可写，
		// 因此无法创建临时文件。此时退回到受锁保护的直接覆盖写入。
		if !canDirectFallbackAfterCreate(err) {
			return fmt.Errorf("创建临时文件失败: %w", err)
		}
		if directErr := saveDirect(c.Path, out); directErr != nil {
			return fmt.Errorf("创建临时文件失败: %w；直接写入也失败: %w", err, directErr)
		}
		return nil
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if err := writeAndSync(tmp, out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := renameConfigFile(tmpPath, c.Path); err != nil {
		if !canDirectFallbackAfterRename(err) {
			keepTemp = false
			return fmt.Errorf("替换配置文件失败: %w；完整临时文件保留在 %s", err, tmpPath)
		}
		if directErr := saveDirect(c.Path, out); directErr != nil {
			keepTemp = false
			return fmt.Errorf("替换配置文件失败: %w；直接写入也失败: %w；完整临时文件保留在 %s", err, directErr, tmpPath)
		}
		return nil
	}
	keepTemp = false
	return nil
}

func canDirectFallbackAfterCreate(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

func canDirectFallbackAfterRename(err error) bool {
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV)
}

func writeAndSync(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func saveDirect(path string, data []byte) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取原配置失败: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return fmt.Errorf("定位配置文件失败: %w", err)
	}
	if err := overwriteAndSync(file, data); err != nil {
		_ = file.Close()
		return restoreDirect(path, original, err)
	}
	if err := file.Close(); err != nil {
		return restoreDirect(path, original, fmt.Errorf("关闭配置文件失败: %w", err))
	}
	return nil
}

func overwriteAndSync(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := file.Truncate(int64(len(data))); err != nil {
		return fmt.Errorf("调整配置文件长度失败: %w", err)
	}
	return file.Sync()
}

func restoreDirect(path string, original []byte, cause error) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w；恢复原配置失败: %v", cause, err)
	}
	_, restoreErr := file.Seek(0, io.SeekStart)
	if restoreErr == nil {
		restoreErr = overwriteAndSync(file, original)
	}
	closeErr := file.Close()
	if restoreErr == nil {
		restoreErr = closeErr
	}
	if restoreErr != nil {
		return fmt.Errorf("%w；恢复原配置失败: %v", cause, restoreErr)
	}
	return cause
}

// DecodeAndValidate 从单个 YAML/JSON 文档加载并严格校验配置，不设置 Path。
func DecodeAndValidate(data []byte) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var c Config
	if err := decoder.Decode(&c); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("配置内容为空")
		}
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("配置解析失败: %w", err)
		}
		return nil, fmt.Errorf("配置只能包含一个 YAML 文档")
	}
	for name, provider := range c.Providers {
		if provider == nil {
			return nil, fmt.Errorf("providers.%s 不能为空", name)
		}
	}
	applyDefaults(&c)
	if err := validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadAndValidate 保留原有调用入口，内部使用严格解码。
func LoadAndValidate(data []byte) (*Config, error) {
	return DecodeAndValidate(data)
}

// SameProviderIdentity 判断两个 provider 是否指向同一协议端点。
func SameProviderIdentity(a, b *Provider) bool {
	return a != nil && b != nil && a.BaseURL == b.BaseURL && a.Format == b.Format
}

// RedactYAML 使用 YAML AST 脱敏 providers 下所有已配置的 apiKey。
func RedactYAML(data []byte, sentinel string) ([]byte, error) {
	if sentinel == "" {
		return nil, fmt.Errorf("脱敏占位符不能为空")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("配置解析失败: %w", err)
		}
		return nil, fmt.Errorf("配置只能包含一个 YAML 文档")
	}
	if len(document.Content) == 0 {
		return nil, fmt.Errorf("配置内容为空")
	}
	root := document.Content[0]
	if err := rejectDuplicateMappingKeys(root, make(map[*yaml.Node]bool)); err != nil {
		return nil, err
	}
	providers := mappingValue(root, "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("providers 字段缺失或格式错误")
	}
	for i := 1; i < len(providers.Content); i += 2 {
		provider := providers.Content[i]
		redactAPIKeys(provider, sentinel, make(map[*yaml.Node]bool))
	}
	out, err := yaml.Marshal(&document)
	if err != nil {
		return nil, fmt.Errorf("配置脱敏序列化失败: %w", err)
	}
	return out, nil
}

func redactAPIKeys(node *yaml.Node, sentinel string, visited map[*yaml.Node]bool) {
	if node == nil || visited[node] {
		return
	}
	visited[node] = true
	if node.Kind == yaml.AliasNode {
		redactAPIKeys(node.Alias, sentinel, visited)
		return
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind == yaml.ScalarNode && key.Value == "apiKey" {
				redactAPIKeyValue(value, sentinel)
				continue
			}
			redactAPIKeys(value, sentinel, visited)
		}
		return
	}
	for _, child := range node.Content {
		redactAPIKeys(child, sentinel, visited)
	}
}

func redactAPIKeyValue(value *yaml.Node, sentinel string) {
	target := value
	seen := make(map[*yaml.Node]bool)
	for target != nil && target.Kind == yaml.AliasNode {
		if seen[target] {
			return
		}
		seen[target] = true
		target = target.Alias
	}
	if target == nil || target.Kind == yaml.ScalarNode && (target.Tag == "!!null" || target.Value == "") {
		return
	}
	target.Kind = yaml.ScalarNode
	target.Tag = "!!str"
	target.Value = sentinel
	target.Style = yaml.DoubleQuotedStyle
	target.Content = nil
	target.Alias = nil
}

func rejectDuplicateMappingKeys(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil || visited[node] {
		return nil
	}
	visited[node] = true
	if node.Kind == yaml.AliasNode {
		return rejectDuplicateMappingKeys(node.Alias, visited)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("配置包含不支持的复合 mapping key")
			}
			if seen[key.Value] {
				return fmt.Errorf("配置包含重复字段 %q，拒绝返回可能泄密的 YAML", key.Value)
			}
			seen[key.Value] = true
			if err := rejectDuplicateMappingKeys(node.Content[i+1], visited); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := rejectDuplicateMappingKeys(child, visited); err != nil {
			return err
		}
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// validate 校验当前 Go 运行时的完整配置边界；部分规则有意严于历史 Node 版。
func validate(c *Config) error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("providers 字段缺失或为空")
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("routes 字段缺失或为空")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port 应在 1-65535 之间")
	}
	if !validListenHost(c.Host) {
		return fmt.Errorf("host 须为不含端口和括号的有效 hostname、IPv4 或 IPv6 地址")
	}
	if c.Timeout < 1000 || c.Timeout > 300000 {
		return fmt.Errorf("timeout 应在 1000-300000 之间（1秒-5分钟）")
	}
	if c.StreamActivityTimeout < 5000 || c.StreamActivityTimeout > 600000 {
		return fmt.Errorf("streamActivityTimeout 应在 5000-600000 之间（5秒-10分钟）")
	}
	if c.DirectTimeoutNoStream < 1000 || c.DirectTimeoutNoStream > 300000 {
		return fmt.Errorf("directTimeoutNoStream 应在 1000-300000 之间")
	}
	if c.DirectTimeoutStreamHeader < 1000 || c.DirectTimeoutStreamHeader > 300000 {
		return fmt.Errorf("directTimeoutStreamHeader 应在 1000-300000 之间")
	}
	if c.DirectTimeoutStreamActive < 5000 || c.DirectTimeoutStreamActive > 600000 {
		return fmt.Errorf("directTimeoutStreamActive 应在 5000-600000 之间")
	}
	if c.Cache.MaxAgeDays < 1 || c.Cache.MaxAgeDays > maxCacheAgeDays {
		return fmt.Errorf("cache.maxAgeDays 应在 1-%d 之间", maxCacheAgeDays)
	}
	if c.Cache.MaxRecords < 1 || c.Cache.MaxRecords > maxCacheRecords {
		return fmt.Errorf("cache.maxRecords 应在 1-%d 之间", maxCacheRecords)
	}
	for name, p := range c.Providers {
		if p == nil {
			return fmt.Errorf("providers.%s 不能为空", name)
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("provider 名称不能为空")
		}
		parsedURL, err := url.ParseRequestURI(p.BaseURL)
		if err != nil || parsedURL.Host == "" || parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
			return fmt.Errorf("providers.%s.baseUrl 须为有效的 http/https URL", name)
		}
		if p.Format != "anthropic" && p.Format != "openai" {
			return fmt.Errorf("providers.%s.format 须为 anthropic 或 openai", name)
		}
		if p.MaxConcurrent < 1 || p.MaxConcurrent > 100 {
			return fmt.Errorf("providers.%s.maxConcurrent 应在 1-100 之间", name)
		}
		if p.MaxPerSecond < 0 || p.MaxPerSecond > 100 {
			return fmt.Errorf("providers.%s.maxPerSecond 应在 0-100 之间（0 表示不限制）", name)
		}
		if p.MaxQueueWait < 1 || p.MaxQueueWait > maxQueueWaitMs {
			return fmt.Errorf("providers.%s.maxQueueWait 应在 1-%d 之间", name, maxQueueWaitMs)
		}
	}
	for _, r := range c.Routes {
		if strings.TrimSpace(r.Match) == "" {
			return fmt.Errorf("route 缺少 match 字段")
		}
		if strings.TrimSpace(r.Provider) == "" {
			return fmt.Errorf("route %q 缺少 provider 字段", r.Match)
		}
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("route %q 引用了未定义的 provider: %s", r.Match, r.Provider)
		}
		if strings.TrimSpace(r.Model) == "" {
			return fmt.Errorf("route %q 缺少 model 字段", r.Match)
		}
		if r.Vision != nil {
			if strings.TrimSpace(r.Vision.Provider) == "" {
				return fmt.Errorf("route %q.vision 缺少 provider 字段", r.Match)
			}
			if _, ok := c.Providers[r.Vision.Provider]; !ok {
				return fmt.Errorf("route %q.vision 引用了未定义的 provider: %s", r.Match, r.Vision.Provider)
			}
			if strings.TrimSpace(r.Vision.Model) == "" {
				return fmt.Errorf("route %q.vision 缺少 model 字段", r.Match)
			}
		}
	}
	return nil
}

func validListenHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/\t\r\n []") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if strings.Contains(host, ":") {
		return false
	}
	hostname := strings.TrimSuffix(host, ".")
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if ch != '-' && (ch < '0' || ch > '9') && (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') {
				return false
			}
		}
	}
	return true
}
