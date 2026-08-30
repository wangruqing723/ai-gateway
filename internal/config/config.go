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
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"ai-gateway/internal/balancer"
)

const (
	// APIKeyKeepSentinel 是管理 API 用于表示“保留已有密钥”的精确占位符。
	APIKeyKeepSentinel = "__AI_GATEWAY_KEEP_API_KEY__"

	maxCacheAgeDays = 3650
	maxCacheRecords = 1_000_000
	maxQueueWaitMs  = 600_000

	// maxRouteTargets 单条路由的候选上限，避免最坏耗时不可控。
	maxRouteTargets = 5
	// maxFailoverAttempts failover.maxAttempts 上限。
	maxFailoverAttempts = 5
	// maxRetryAfterCapMs Retry-After 阈值上限，借 Portkey 的 60 秒。
	maxRetryAfterCapMs = 60_000

	// maxBreakerFailures breaker.consecutiveFailures 上限。
	maxBreakerFailures = 100
	// maxBreakerOpenMs 熔断打开时长上限（10 分钟），超过这个量级不如直接改配置。
	maxBreakerOpenMs = 600_000
	// maxBreakerHalfOpenProbes 半开探测放行数上限。
	maxBreakerHalfOpenProbes = 10

	// 指标统计窗口的默认值与上限（分钟）。
	// 默认 15：低流量下 1 分钟窗口只有个位数样本，P95 会被单次超时主导。
	// 上限 60：桶按秒分配且每桶带 per-provider 直方图，再大对本地网关不划算。
	defaultMetricsWindowMinutes = 15
	maxMetricsWindowMinutes     = 60

	// maxProviderNameRunes provider 名称长度上限，按字符（rune）而非字节计：
	// 名称多为中文，按字节算一个汉字占 3 字节，100 字节只剩 33 个汉字可用。
	// 名称会进日志、响应头 x-ai-gateway-provider 和前端表格，需要有个上限。
	maxProviderNameRunes = 100

	// maxProviderUserAgentRunes User-Agent 长度上限，按字符（rune）而非字节计：
	// 配置允许使用非 ASCII 的客户端标识，按字节计会不必要地缩短其可用长度。
	maxProviderUserAgentRunes = 256

	// 默认值集中在此，供 applyDefaults 与各 accessor 共用一处来源。
	defaultFailoverAttempts      = 2
	defaultMaxRetryAfterMs       = 5000
	defaultBreakerFailures       = 3
	defaultBreakerOpenMs         = 30_000
	defaultBreakerHalfOpenProbes = 1
)

var (
	saveMu           sync.Mutex
	createConfigTemp = os.CreateTemp
	renameConfigFile = os.Rename
)

// Provider 上游 provider 定义
type Provider struct {
	Name          string `yaml:"-" json:"name,omitempty"`                        // 运行时填充（map 的 key）
	BaseURL       string `yaml:"baseUrl" json:"baseUrl"`                         // 上游地址
	APIKey        string `yaml:"apiKey" json:"apiKey"`                           // 密钥，留空则从客户端请求头提取
	Format        string `yaml:"format" json:"format"`                           // anthropic | openai | openai-responses
	UserAgent     string `yaml:"userAgent,omitempty" json:"userAgent,omitempty"` // 上游请求 UA，留空则转发客户端值；两处 omitempty 见 validate 注释
	MaxConcurrent int    `yaml:"maxConcurrent" json:"maxConcurrent"`             // 最大并发
	MaxPerSecond  int    `yaml:"maxPerSecond" json:"maxPerSecond"`               // 每秒最多请求数，0 表示不限
	MaxQueueWait  int    `yaml:"maxQueueWait" json:"maxQueueWait,omitempty"`     // 队列最大等待（毫秒）
}

// Vision 路由上的视觉子配置
type Vision struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// Target 是路由的一个候选上游目标（provider + 该 provider 上的模型名）。
// 候选必须整对出现：不同上游的模型名不通用（如 mimo-v2.5-pro 与 deepseek-chat）。
type Target struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// Route 路由规则，按顺序匹配，首条命中生效。
//
// 目标有两种写法，互斥：
//   - 单目标（存量写法）：provider + model
//   - 多目标：targets 列表，按顺序作为故障转移候选
type Route struct {
	// yaml tag 一律带 omitempty：落盘走 yaml.Marshal，缺了它每次保存都会给单目标路由
	// 塞进 targets: []，给多目标路由塞进 provider: ""，把两种互斥写法混在同一条路由上。
	Match    string   `yaml:"match" json:"match"`
	Provider string   `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model    string   `yaml:"model,omitempty" json:"model,omitempty"`
	Targets  []Target `yaml:"targets,omitempty" json:"targets,omitempty"`
	Vision   *Vision  `yaml:"vision,omitempty" json:"vision,omitempty"`
	// Strategy 候选选择策略：failover | round-robin | least-queue，空等同 failover。
	//
	// 只做路由级，不设全局默认：全局默认会让「这条路由到底用哪个策略」需要看两处，
	// 而路由数量本来不多。
	//
	// 与 failover.enabled 正交：策略决定「先试谁」，failover 决定「失败了还能试谁」。
	// failover 关闭 + round-robin 是合法组合，表示纯分流、不转移。
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

// TargetList 返回统一形态的候选列表。
// 不做 applyDefaults 归一化：/api/config 的 PUT 会把结构写回磁盘，
// 归一化会把用户的单目标写法擅自重写成 targets 形态。
func (r *Route) TargetList() []Target {
	if len(r.Targets) > 0 {
		out := make([]Target, len(r.Targets))
		copy(out, r.Targets)
		return out
	}
	return []Target{{Provider: r.Provider, Model: r.Model}}
}

// Failover 故障转移配置。默认关闭，保持「不重试」的既有语义。
//
// OnXxx 一律用 *bool、数值项一律用 *int：nil 表示用户未设置，由 applyDefaults 填默认值。
// 若用值类型，零值与「用户显式写 0 / false」不可区分，applyDefaults 里的
// `if x == 0 { x = 默认值 }` 会静默改掉用户写的 0 —— 对 maxRetryAfterMs 这种
// 「0 是合法且有含义的取值」尤其致命，而对不接受 0 的项则会让 validate 里
// 对应的下界检查变成永不触发的死代码，用户手误写 0 得不到任何提示。
type Failover struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxAttempts 含首次尝试的总次数上限，1-5。默认 2。
	MaxAttempts *int `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
	// OnTransportError 连接失败 / DNS / TLS 错误时转移。默认 true。
	OnTransportError *bool `yaml:"onTransportError" json:"onTransportError,omitempty"`
	// OnServerError 上游 5xx 时转移。默认 true。
	OnServerError *bool `yaml:"onServerError" json:"onServerError,omitempty"`
	// OnRateLimit 上游 429 时转移。候选为不同厂商/账号时有效。默认 true。
	OnRateLimit *bool `yaml:"onRateLimit" json:"onRateLimit,omitempty"`
	// OnQueueTimeout 本地队列等待超时时转移。默认 true。
	OnQueueTimeout *bool `yaml:"onQueueTimeout" json:"onQueueTimeout,omitempty"`
	// OnStreamHeaderTimeout 流式等响应头超时时转移。默认 true。
	// 该阶段客户端尚未收到任何字节，转移边界干净。
	OnStreamHeaderTimeout *bool `yaml:"onStreamHeaderTimeout" json:"onStreamHeaderTimeout,omitempty"`
	// OnRequestTimeout 非流式整体超时时转移。默认 false。
	//
	// 单独一个开关而不是并入 OnTransportError：整体超时意味着已经等满了一整个
	// timeout 预算，转移会让总耗时接近翻倍（timeout × 候选数）。但把它和真正的
	// 连接失败绑在一起也不对 —— 那类错误几乎不耗时，是 failover 最该覆盖的场景，
	// 用户不该为了关掉超时转移而连带关掉它。
	//
	// 流式活跃超时仍不可配置转移：那时字节已经写给客户端，重发会污染流。
	OnRequestTimeout *bool `yaml:"onRequestTimeout" json:"onRequestTimeout,omitempty"`
	// OnAuthError 401 / 403 时转移。默认 false：那通常是配置问题，转移会掩盖它。
	OnAuthError *bool `yaml:"onAuthError" json:"onAuthError,omitempty"`
	// MaxRetryAfterMs 上游 429 携带 Retry-After 且超过该值时，该候选不消耗尝试额度。
	//
	// 语义与熔断跳过一致：上游明确告知「我这段时间不可用」，那这次失败就不该
	// 占掉 maxAttempts 里的一格，否则一个自曝限流的上游会挤掉本来还能试的健康候选。
	// 默认 5000。显式写 0 表示不设上限（所有 429 照常消耗额度），
	// 因此这里必须用 *int：值类型分不清「写了 0」和「没写」。
	MaxRetryAfterMs *int `yaml:"maxRetryAfterMs,omitempty" json:"maxRetryAfterMs,omitempty"`
}

// BoolOr 读取 *bool，nil 时返回 def。
func BoolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func boolPtr(v bool) *bool { return &v }

// IntOr 读取 *int，nil 时返回 def。
func IntOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func intPtr(v int) *int { return &v }

// TransferOnTransportError 传输错误是否转移。
func (f *Failover) TransferOnTransportError() bool { return BoolOr(f.OnTransportError, true) }

// TransferOnServerError 上游 5xx 是否转移。
func (f *Failover) TransferOnServerError() bool { return BoolOr(f.OnServerError, true) }

// TransferOnRateLimit 上游 429 是否转移。
func (f *Failover) TransferOnRateLimit() bool { return BoolOr(f.OnRateLimit, true) }

// TransferOnQueueTimeout 队列等待超时是否转移。
func (f *Failover) TransferOnQueueTimeout() bool { return BoolOr(f.OnQueueTimeout, true) }

// TransferOnStreamHeaderTimeout 流式响应头超时是否转移。
func (f *Failover) TransferOnStreamHeaderTimeout() bool {
	return BoolOr(f.OnStreamHeaderTimeout, true)
}

// TransferOnRequestTimeout 非流式整体超时是否转移。默认 false：会让总耗时接近翻倍。
func (f *Failover) TransferOnRequestTimeout() bool { return BoolOr(f.OnRequestTimeout, false) }

// TransferOnAuthError 401 / 403 是否转移。
func (f *Failover) TransferOnAuthError() bool { return BoolOr(f.OnAuthError, false) }

// AttemptLimit 含首次尝试的总次数上限。
func (f *Failover) AttemptLimit() int { return IntOr(f.MaxAttempts, defaultFailoverAttempts) }

// RetryAfterCapMs 429 的 Retry-After 上限（毫秒），0 表示不设上限。
func (f *Failover) RetryAfterCapMs() int {
	return IntOr(f.MaxRetryAfterMs, defaultMaxRetryAfterMs)
}

// Breaker 熔断配置。全局一份，不做 per-provider 覆盖。
//
// 计入失败：传输错误、5xx、超时。
// 不计入：429（上游正常限流，判故障比不熔断更糟）、401/403（配置问题，熔断修不了还会掩盖密钥过期）。
// 数值项与 Failover 同理用 *int：这三项都不接受 0，值类型会让 validate 的下界检查
// 变成死代码 —— 用户手误写 0 被 applyDefaults 静默改成默认值，永远看不到报错。
type Breaker struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ConsecutiveFailures 连续失败达该值即打开熔断，默认 3。
	// 用连续失败而非错误率：错误率需要最小样本数，否则首次失败就触发（LiteLLM #17418）。
	ConsecutiveFailures *int `yaml:"consecutiveFailures,omitempty" json:"consecutiveFailures,omitempty"`
	// OpenMs 熔断打开后的冷却时长（毫秒），默认 30000。冷却结束转半开。
	OpenMs *int `yaml:"openMs,omitempty" json:"openMs,omitempty"`
	// HalfOpenProbes 半开状态放行的探测请求数，默认 1。
	// 探针用下一个真实入站请求，不用 /v1/models：那个端点通不代表聊天端点可用。
	HalfOpenProbes *int `yaml:"halfOpenProbes,omitempty" json:"halfOpenProbes,omitempty"`
}

// FailureThreshold 连续失败开路阈值。
func (b *Breaker) FailureThreshold() int {
	return IntOr(b.ConsecutiveFailures, defaultBreakerFailures)
}

// CooldownMs 开路后的冷却时长（毫秒）。
func (b *Breaker) CooldownMs() int { return IntOr(b.OpenMs, defaultBreakerOpenMs) }

// ProbeLimit 半开状态放行的探测请求数。
func (b *Breaker) ProbeLimit() int {
	return IntOr(b.HalfOpenProbes, defaultBreakerHalfOpenProbes)
}

// Cache 缓存配置
type Cache struct {
	MaxAgeDays int `yaml:"maxAgeDays" json:"maxAgeDays"`
	MaxRecords int `yaml:"maxRecords" json:"maxRecords"`
}

// Metrics 观测指标配置。
type Metrics struct {
	// WindowMinutes 顶部指标（请求数、成功率、P95）的统计窗口分钟数。
	// 低流量下窗口太短会让分位数变成噪声，默认 15 分钟。
	WindowMinutes int `yaml:"windowMinutes" json:"windowMinutes"`
}

// Config 顶层配置
type Config struct {
	Port                  int                  `yaml:"port" json:"port"`
	Host                  string               `yaml:"host" json:"host"`
	Timeout               int                  `yaml:"timeout" json:"timeout"`                             // 请求超时（毫秒）
	StreamActivityTimeout int                  `yaml:"streamActivityTimeout" json:"streamActivityTimeout"` // 流式活跃超时（毫秒）
	Cache                 Cache                `yaml:"cache" json:"cache"`
	Metrics               Metrics              `yaml:"metrics" json:"metrics"`
	Providers             map[string]*Provider `yaml:"providers" json:"providers"`
	Routes                []Route              `yaml:"routes" json:"routes"`
	Failover              Failover             `yaml:"failover" json:"failover"`
	Breaker               Breaker              `yaml:"breaker" json:"breaker"`
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
	// 与 Cache 同样用「0 视为未写」：窗口不接受 0（0 分钟窗口没有意义），
	// 所以这里不需要 Failover/Breaker 那套 *int 区分「写了 0」和「没写」。
	if c.Metrics.WindowMinutes == 0 {
		c.Metrics.WindowMinutes = defaultMetricsWindowMinutes
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
	applyFailoverDefaults(&c.Failover)
	applyBreakerDefaults(&c.Breaker)
}

// applyFailoverDefaults 只在字段未设置时填默认值。
// 全部字段都是指针，nil 才填；用户显式写的 false / 0 必须原样保留，
// 交给 validate 判合法性（maxAttempts: 0 应当报错，maxRetryAfterMs: 0 是合法的「不设上限」）。
func applyFailoverDefaults(f *Failover) {
	if f.MaxAttempts == nil {
		f.MaxAttempts = intPtr(defaultFailoverAttempts)
	}
	if f.MaxRetryAfterMs == nil {
		f.MaxRetryAfterMs = intPtr(defaultMaxRetryAfterMs)
	}
	if f.OnRequestTimeout == nil {
		f.OnRequestTimeout = boolPtr(false)
	}
	if f.OnTransportError == nil {
		f.OnTransportError = boolPtr(true)
	}
	if f.OnServerError == nil {
		f.OnServerError = boolPtr(true)
	}
	if f.OnRateLimit == nil {
		f.OnRateLimit = boolPtr(true)
	}
	if f.OnQueueTimeout == nil {
		f.OnQueueTimeout = boolPtr(true)
	}
	if f.OnStreamHeaderTimeout == nil {
		f.OnStreamHeaderTimeout = boolPtr(true)
	}
	if f.OnAuthError == nil {
		f.OnAuthError = boolPtr(false)
	}
}

// applyBreakerDefaults 只在字段未设置时填默认值。
// 同样 nil 才填：这三项都不接受 0，用户写了 0 要能从 validate 拿到报错。
func applyBreakerDefaults(b *Breaker) {
	if b.ConsecutiveFailures == nil {
		b.ConsecutiveFailures = intPtr(defaultBreakerFailures)
	}
	if b.OpenMs == nil {
		b.OpenMs = intPtr(defaultBreakerOpenMs)
	}
	if b.HalfOpenProbes == nil {
		b.HalfOpenProbes = intPtr(defaultBreakerHalfOpenProbes)
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
			return fmt.Errorf("创建临时文件失败: %w；直接写入也失败: %w%s", err, directErr, mountPermHint(err, directErr))
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
			return fmt.Errorf("替换配置文件失败: %w；直接写入也失败: %w；完整临时文件保留在 %s%s", err, directErr, tmpPath, mountPermHint(directErr))
		}
		return nil
	}
	keepTemp = false
	return nil
}

func canDirectFallbackAfterCreate(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// mountPermHint 在错误由权限不足引起时，补充 Docker 挂载属主对齐的排查提示。
// 典型场景：容器进程以固定 UID/GID 运行，而 bind mount 的 config.yaml 属主不同
// （macOS Docker Desktop 会自动映射，原生 Linux/WSL 则严格穿透宿主属主）。
func mountPermHint(errs ...error) string {
	for _, e := range errs {
		if errors.Is(e, os.ErrPermission) {
			return "；提示：容器进程 UID/GID 可能与挂载的 config.yaml 属主不一致，" +
				"请用 scripts/up.sh 启动（自动对齐宿主用户），或手动对齐文件属主"
		}
	}
	return ""
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
	if c.Metrics.WindowMinutes < 1 || c.Metrics.WindowMinutes > maxMetricsWindowMinutes {
		return fmt.Errorf("metrics.windowMinutes 应在 1-%d 之间", maxMetricsWindowMinutes)
	}
	for name, p := range c.Providers {
		if p == nil {
			return fmt.Errorf("providers.%s 不能为空", name)
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("provider 名称不能为空")
		}
		if n := utf8.RuneCountInString(name); n > maxProviderNameRunes {
			return fmt.Errorf("provider 名称长度应不超过 %d 个字符（当前 %d）", maxProviderNameRunes, n)
		}
		parsedURL, err := url.ParseRequestURI(p.BaseURL)
		if err != nil || parsedURL.Host == "" || parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
			return fmt.Errorf("providers.%s.baseUrl 须为有效的 http/https URL", name)
		}
		if p.Format != "anthropic" && p.Format != "openai" && p.Format != "openai-responses" {
			return fmt.Errorf("providers.%s.format 须为 anthropic、openai 或 openai-responses", name)
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
		// UserAgent 的 yaml 与 json 都带 omitempty：PUT 保存会按结构体重新序列化
		// 整个配置文件，yaml 少了 omitempty 就会给每个没配该字段的 provider 落一行
		// userAgent: ""，纯噪音；json 少了则前端拿到一堆空字段。
		// 空值是合法终态（语义为「不配置」），applyDefaults 不碰它。
		if p.UserAgent != "" {
			if n := utf8.RuneCountInString(p.UserAgent); n > maxProviderUserAgentRunes {
				return fmt.Errorf("providers.%s.userAgent 长度应不超过 %d 个字符（当前 %d）", name, maxProviderUserAgentRunes, n)
			}
			for _, r := range p.UserAgent {
				if (r < 0x20 && r != '\t') || r == 0x7F {
					return fmt.Errorf("providers.%s.userAgent 不能包含 ASCII 控制字符", name)
				}
			}
		}
	}
	for _, r := range c.Routes {
		if strings.TrimSpace(r.Match) == "" {
			return fmt.Errorf("route 缺少 match 字段")
		}
		if err := validateRouteTargets(c, &r); err != nil {
			return err
		}
		if err := validateRouteStrategy(&r); err != nil {
			return err
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
	if err := validateFailover(&c.Failover); err != nil {
		return err
	}
	if err := validateBreaker(&c.Breaker); err != nil {
		return err
	}
	return nil
}

// validateRouteTargets 校验单条路由的目标写法：单目标（provider/model）与多目标（targets）互斥。
func validateRouteTargets(c *Config, r *Route) error {
	hasSingle := strings.TrimSpace(r.Provider) != ""
	hasTargets := len(r.Targets) > 0

	switch {
	case hasSingle && hasTargets:
		return fmt.Errorf("route %q 不能同时配置 provider 与 targets，二者互斥", r.Match)
	case !hasSingle && !hasTargets:
		return fmt.Errorf("route %q 缺少 provider 或 targets 字段", r.Match)
	}

	if hasSingle {
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("route %q 引用了未定义的 provider: %s", r.Match, r.Provider)
		}
		if strings.TrimSpace(r.Model) == "" {
			return fmt.Errorf("route %q 缺少 model 字段", r.Match)
		}
		return nil
	}

	if len(r.Targets) > maxRouteTargets {
		return fmt.Errorf("route %q.targets 最多 %d 个候选", r.Match, maxRouteTargets)
	}
	// 拒绝完全重复的 (provider, model) 对；同 provider 不同 model 合法（降级到便宜模型）
	seen := make(map[Target]struct{}, len(r.Targets))
	for i, t := range r.Targets {
		if strings.TrimSpace(t.Provider) == "" {
			return fmt.Errorf("route %q.targets[%d] 缺少 provider 字段", r.Match, i)
		}
		if _, ok := c.Providers[t.Provider]; !ok {
			return fmt.Errorf("route %q.targets[%d] 引用了未定义的 provider: %s", r.Match, i, t.Provider)
		}
		if strings.TrimSpace(t.Model) == "" {
			return fmt.Errorf("route %q.targets[%d] 缺少 model 字段", r.Match, i)
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("route %q.targets 存在重复候选: %s/%s", r.Match, t.Provider, t.Model)
		}
		seen[t] = struct{}{}
	}
	return nil
}

// validateRouteStrategy 校验路由的候选选择策略。
//
// 取值表向 balancer 借，不在这里另写一份：两处各写一份必然会分叉。
//
// 单候选路由上写 strategy 直接报错而不是无声忽略：配置项被静默忽略
// 正是 LiteLLM #32425 那类难查的 bug，宁可让用户在启动时就看到。
func validateRouteStrategy(r *Route) error {
	if r.Strategy == "" {
		return nil
	}
	if !balancer.ValidStrategy(r.Strategy) {
		return fmt.Errorf("route %q.strategy 须为 %s、%s 或 %s",
			r.Match, balancer.StrategyFailover, balancer.StrategyRoundRobin, balancer.StrategyLeastQueue)
	}
	if len(r.TargetList()) < 2 && r.Strategy != balancer.StrategyFailover {
		return fmt.Errorf("route %q 只有一个候选，strategy: %s 不会生效；请配置 targets 或删掉 strategy",
			r.Match, r.Strategy)
	}
	return nil
}

// validateFailover 校验 failover 的数值边界。
func validateFailover(f *Failover) error {
	// 读 accessor 而不是解指针：validate 也会被 DecodeAndValidate 之外的路径调用，
	// 此时指针可能还没被 applyDefaults 填上。
	if attempts := f.AttemptLimit(); attempts < 1 || attempts > maxFailoverAttempts {
		return fmt.Errorf("failover.maxAttempts 应在 1-%d 之间", maxFailoverAttempts)
	}
	// 0 合法，表示不设上限
	if cap := f.RetryAfterCapMs(); cap < 0 || cap > maxRetryAfterCapMs {
		return fmt.Errorf("failover.maxRetryAfterMs 应在 0-%d 之间（0 表示不设上限）", maxRetryAfterCapMs)
	}
	return nil
}

// validateBreaker 校验 breaker 的数值边界。
func validateBreaker(b *Breaker) error {
	if failures := b.FailureThreshold(); failures < 1 || failures > maxBreakerFailures {
		return fmt.Errorf("breaker.consecutiveFailures 应在 1-%d 之间", maxBreakerFailures)
	}
	if openMs := b.CooldownMs(); openMs < 1 || openMs > maxBreakerOpenMs {
		return fmt.Errorf("breaker.openMs 应在 1-%d 之间", maxBreakerOpenMs)
	}
	if probes := b.ProbeLimit(); probes < 1 || probes > maxBreakerHalfOpenProbes {
		return fmt.Errorf("breaker.halfOpenProbes 应在 1-%d 之间", maxBreakerHalfOpenProbes)
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
