// Package config 负责加载与校验 YAML 配置，对齐 Node 版 lib/config.js 的字段与默认值。
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Provider 上游 provider 定义
type Provider struct {
	Name          string `yaml:"-"`             // 运行时填充（map 的 key）
	BaseURL       string `yaml:"baseUrl"`       // 上游地址
	APIKey        string `yaml:"apiKey"`        // 密钥，留空则从客户端请求头提取
	Format        string `yaml:"format"`        // anthropic | openai
	MaxConcurrent int    `yaml:"maxConcurrent"` // 最大并发
	MaxPerSecond  int    `yaml:"maxPerSecond"`  // 每秒最多请求数，0 表示不限
	MaxQueueWait  int    `yaml:"maxQueueWait"`  // 队列最大等待（毫秒）
}

// Vision 路由上的视觉子配置
type Vision struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Route 路由规则，按顺序匹配，首条命中生效
type Route struct {
	Match    string  `yaml:"match"`
	Provider string  `yaml:"provider"`
	Model    string  `yaml:"model"`
	Vision   *Vision `yaml:"vision"`
}

// Cache 缓存配置
type Cache struct {
	MaxAgeDays int `yaml:"maxAgeDays"`
	MaxRecords int `yaml:"maxRecords"`
}

// Config 顶层配置
type Config struct {
	Port                  int                  `yaml:"port"`
	Host                  string               `yaml:"host"`
	Timeout               int                  `yaml:"timeout"`               // 请求超时（毫秒）
	StreamActivityTimeout int                  `yaml:"streamActivityTimeout"` // 流式活跃超时（毫秒）
	Cache                 Cache                `yaml:"cache"`
	Providers             map[string]*Provider `yaml:"providers"`
	Routes                []Route              `yaml:"routes"`
	Path                  string               `yaml:"-"`

	// ── 直通模式（direct mode）──────────────────────────────
	// 开启后请求不进队列：跳过并发控制、限速与排队等待，直接转发到上游。
	// 限流交给上游自己用 429 处理（网关原样透传，不重试）。仅保留超时保护。
	// 默认 false，保持原有队列行为；Node 版不识别这些字段会自动忽略，互不影响。
	DirectMode                bool `yaml:"directMode"`                // 直通开关
	DirectTimeoutNoStream     int  `yaml:"directTimeoutNoStream"`     // 非流式整体超时（毫秒），默认 60000
	DirectTimeoutStreamHeader int  `yaml:"directTimeoutStreamHeader"` // 流式等响应头超时（毫秒），默认 60000
	DirectTimeoutStreamActive int  `yaml:"directTimeoutStreamActive"` // 流式活跃超时（毫秒，中途无数据即断），默认 120000
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

	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("配置解析失败 (%s): %w", path, err)
	}
	c.Path = path

	applyDefaults(&c)
	if err := validate(&c); err != nil {
		return nil, fmt.Errorf("配置错误 (%s): %w", path, err)
	}
	return &c, nil
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
	for name, p := range c.Providers {
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

// Save 将配置写回文件（先写临时文件再 rename，保证原子性）
func Save(c *Config) error {
	if c.Path == "" {
		return fmt.Errorf("配置文件路径未知")
	}

	// 序列化为 YAML
	out, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}

	// 写入临时文件
	tmpPath := c.Path + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	// 原子替换
	if err := os.Rename(tmpPath, c.Path); err != nil {
		os.Remove(tmpPath) // 清理临时文件
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}

// LoadAndValidate 从 YAML 字节加载并验证（用于保存前校验），不设置 Path
func LoadAndValidate(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	applyDefaults(&c)
	if err := validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate 校验关键字段，区间与 Node 版保持一致
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
	if c.Timeout < 1000 || c.Timeout > 300000 {
		return fmt.Errorf("timeout 应在 1000-300000 之间（1秒-5分钟）")
	}
	if c.StreamActivityTimeout < 5000 || c.StreamActivityTimeout > 600000 {
		return fmt.Errorf("streamActivityTimeout 应在 5000-600000 之间（5秒-10分钟）")
	}
	for name, p := range c.Providers {
		if p.BaseURL == "" {
			return fmt.Errorf("providers.%s.baseUrl 缺失", name)
		}
		if p.Format != "anthropic" && p.Format != "openai" {
			return fmt.Errorf("providers.%s.format 须为 anthropic 或 openai", name)
		}
	}
	for _, r := range c.Routes {
		if r.Match == "" {
			return fmt.Errorf("route 缺少 match 字段")
		}
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("route %q 引用了未定义的 provider: %s", r.Match, r.Provider)
		}
		if r.Vision != nil {
			if _, ok := c.Providers[r.Vision.Provider]; !ok {
				return fmt.Errorf("route %q.vision 引用了未定义的 provider: %s", r.Match, r.Vision.Provider)
			}
		}
	}
	return nil
}
