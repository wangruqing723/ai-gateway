// Package metrics 提供轻量内存请求日志与运行指标聚合。
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultCapacity = 1000
	// DefaultWindowMinutes 是统计窗口的默认分钟数。
	// 取 15 而不是 1：低流量下 1 分钟窗口往往只有个位数样本，
	// 拿它算 P50/P95/P99 得到的是噪声——一次超时就能把 P95 拽到 30s。
	DefaultWindowMinutes = 15
	// MaxWindowMinutes 统计窗口上限。桶按秒分配，每桶还带 per-provider 直方图，
	// 60 分钟约 3601 个桶、十兆量级，再大对本地网关不划算。
	MaxWindowMinutes = 60
)

// bucketsForWindow 返回容纳 minutes 分钟所需的桶数。
// 多留一个桶：写入按 second%len 取模，满格会让当前秒和最旧一秒撞同一个下标。
func bucketsForWindow(minutes int) int {
	return minutes*60 + 1
}

var latencyBounds = [...]int64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000, 600000}

type latencyHistogram struct {
	counts [len(latencyBounds)]int
	total  int
}

func (h *latencyHistogram) add(value int64) {
	if value < 0 {
		value = 0
	}
	idx := len(latencyBounds) - 1
	for i, bound := range latencyBounds {
		if value <= bound {
			idx = i
			break
		}
	}
	h.counts[idx]++
	h.total++
}

func (h *latencyHistogram) merge(other latencyHistogram) {
	for i, count := range other.counts {
		h.counts[i] += count
	}
	h.total += other.total
}

func (h latencyHistogram) percentile(p int) int64 {
	if h.total == 0 {
		return 0
	}
	rank := (h.total*p + 99) / 100
	seen := 0
	for i, count := range h.counts {
		seen += count
		if seen >= rank {
			return latencyBounds[i]
		}
	}
	return latencyBounds[len(latencyBounds)-1]
}

type providerBucket struct {
	requests int
	errors   int
	latency  latencyHistogram
}

type metricBucket struct {
	second      int64
	requests    int
	successes   int
	statusCodes map[string]int
	latency     latencyHistogram
	providers   map[string]*providerBucket
}

// logProviderStats 是 provider 统计兜底分支所需的最小记录集合。
// 正常情况下 provider 数据直接来自时间桶，不需要读取请求日志环形缓冲。
type logProviderStats struct {
	requests  int
	errors    int
	latencies []int64
}

func (b *metricBucket) reset(second int64) {
	b.second = second
	b.requests = 0
	b.successes = 0
	b.statusCodes = make(map[string]int)
	b.latency = latencyHistogram{}
	b.providers = make(map[string]*providerBucket)
}

// Collector 保存有界请求日志，并用秒级时间桶维护不受日志容量影响的最近窗口指标。
type Collector struct {
	mu            sync.RWMutex
	records       []RequestLog
	next          int
	full          bool
	totalRequests uint64
	// buckets 是环形秒桶，长度 = 窗口分钟数×60+1，构造后不变。
	buckets       []metricBucket
	windowSeconds int64
}

// RequestLog 是单次请求的观测记录。
type RequestLog struct {
	ID             string `json:"id"`
	Time           string `json:"time"`
	StartedAt      string `json:"startedAt"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	ClientIP       string `json:"clientIp,omitempty"`
	KeySource      string `json:"keySource,omitempty"`
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	Status         int    `json:"status"`
	ClientFormat   string `json:"clientFormat"`
	Format         string `json:"format"`
	Model          string `json:"model"`
	Route          string `json:"route,omitempty"`
	Provider       string `json:"provider"`
	TargetModel    string `json:"targetModel,omitempty"`
	Stream         bool   `json:"stream"`
	DurationMs     int64  `json:"durationMs"`
	QueueWaitMs    int64  `json:"queueWaitMs,omitempty"`
	Error          string `json:"error,omitempty"`
	Vision         bool   `json:"vision,omitempty"`
	ResponseBytes  int64  `json:"responseBytes,omitempty"`
	UpstreamStatus int    `json:"upstreamStatus,omitempty"`
	// Attempts 保留既有的故障转移额度计数语义；超长 Retry-After 的 free attempt
	// 不计入其中。严格的实际 HTTP 尝试编号见 AttemptDetails.AttemptNumber。
	Attempts int `json:"attempts,omitempty"`
	// AttemptTrail 故障转移轨迹，例如 "mimo:429/ratelimit → deepseek:200"。
	AttemptTrail string `json:"attemptTrail,omitempty"`
	// AttemptDetails 保留每一个候选的请求、跳过和转移明细；仍只生成一条顶层请求日志。
	AttemptDetails []AttemptDetail `json:"attemptDetails,omitempty"`
	Started        time.Time       `json:"-"`
}

// AttemptDetail 是一次候选决策的结构化可观测性记录。Kind/Outcome/Reason 使用稳定
// 枚举，避免前端或外部诊断脚本依赖自由格式的 AttemptTrail 文本。
type AttemptDetail struct {
	Sequence           int    `json:"sequence"`
	AttemptNumber      int    `json:"attemptNumber,omitempty"`
	Kind               string `json:"kind"`
	Provider           string `json:"provider"`
	TargetModel        string `json:"targetModel,omitempty"`
	ProviderFormat     string `json:"providerFormat"`
	StartedAt          string `json:"startedAt"`
	DurationMs         int64  `json:"durationMs"`
	UpstreamStatus     int    `json:"upstreamStatus,omitempty"`
	Outcome            string `json:"outcome"`
	Reason             string `json:"reason,omitempty"`
	Error              string `json:"error,omitempty"`
	ErrorBody          string `json:"errorBody,omitempty"`
	ErrorBodyTruncated bool   `json:"errorBodyTruncated,omitempty"`
	UpstreamRequestID  string `json:"upstreamRequestId,omitempty"`
	RetryAfterMs       int64  `json:"retryAfterMs,omitempty"`
	FreeAttempt        bool   `json:"freeAttempt,omitempty"`
	ResponseStarted    bool   `json:"responseStarted,omitempty"`
}

// Summary 是仪表盘顶部指标。
type Summary struct {
	// WindowRequests 是统计窗口内的请求数；WindowMinutes 是窗口大小（分钟），
	// 供前端渲染「最近 N 分钟」而不必把窗口长度写死在文案里。
	WindowRequests int     `json:"windowRequests"`
	WindowMinutes  int     `json:"windowMinutes"`
	TotalRequests  int     `json:"totalRequests"`
	SuccessRate    float64 `json:"successRate"`
	ErrorRate      float64 `json:"errorRate"`
	P50LatencyMs   int64   `json:"p50LatencyMs"`
	P95LatencyMs   int64   `json:"p95LatencyMs"`
	P99LatencyMs   int64   `json:"p99LatencyMs"`
	// LatencySamples 是实际进入延迟直方图的窗口内样本数，可能少于 WindowRequests。
	LatencySamples int `json:"latencySamples"`
}

// ProviderStats 是按 provider 聚合的指标。
type ProviderStats struct {
	Name         string  `json:"name"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	SuccessRate  float64 `json:"successRate"`
	P50LatencyMs int64   `json:"p50LatencyMs"`
	P95LatencyMs int64   `json:"p95LatencyMs"`
	P99LatencyMs int64   `json:"p99LatencyMs"`
}

// Response 是 /api/metrics 的响应结构。
type Response struct {
	Summary      Summary         `json:"summary"`
	Providers    []ProviderStats `json:"providers"`
	StatusCodes  map[string]int  `json:"statusCodes"`
	RecentErrors []RequestLog    `json:"recentErrors"`
}

// NewCollector 创建固定容量的内存采集器，统计窗口取默认值。
func NewCollector(capacity int) *Collector {
	return NewCollectorWithWindow(capacity, DefaultWindowMinutes)
}

// NewCollectorWithWindow 创建采集器并指定统计窗口分钟数。
// windowMinutes 超出 [1, MaxWindowMinutes] 会被夹到边界，0 或负数回落默认值。
func NewCollectorWithWindow(capacity, windowMinutes int) *Collector {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	if windowMinutes <= 0 {
		windowMinutes = DefaultWindowMinutes
	}
	if windowMinutes > MaxWindowMinutes {
		windowMinutes = MaxWindowMinutes
	}
	return &Collector{
		records:       make([]RequestLog, 0, capacity),
		buckets:       make([]metricBucket, bucketsForWindow(windowMinutes)),
		windowSeconds: int64(windowMinutes) * 60,
	}
}

// WindowMinutes 返回当前统计窗口的分钟数。
func (c *Collector) WindowMinutes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return int(c.windowSeconds / 60)
}

// SetWindow 调整统计窗口，供配置热重载调用。窗口未变时直接返回。
//
// 桶数组按新窗口重建，但仍落在新窗口内的历史桶会按新下标搬过去，
// 不整份丢弃：保存一次配置就把刚才的指标清零，会让「改配置」和
// 「指标归零」在界面上难以区分。窗口缩小时超出范围的桶自然被丢掉。
func (c *Collector) SetWindow(windowMinutes int) {
	if windowMinutes <= 0 {
		windowMinutes = DefaultWindowMinutes
	}
	if windowMinutes > MaxWindowMinutes {
		windowMinutes = MaxWindowMinutes
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	windowSeconds := int64(windowMinutes) * 60
	if windowSeconds == c.windowSeconds {
		return
	}

	size := bucketsForWindow(windowMinutes)
	rebuilt := make([]metricBucket, size)
	cutoff := time.Now().Unix() - windowSeconds
	for i := range c.buckets {
		old := &c.buckets[i]
		if old.statusCodes == nil || old.requests == 0 || old.second < cutoff {
			continue
		}
		idx := int(old.second % int64(size))
		if idx < 0 {
			idx += size
		}
		// 同下标只保留较新的那个桶：窗口缩小时多个旧秒会映射到同一格
		if rebuilt[idx].statusCodes != nil && rebuilt[idx].second >= old.second {
			continue
		}
		rebuilt[idx] = *old
	}
	c.buckets = rebuilt
	c.windowSeconds = windowSeconds
}

// Add 追加一条记录。
func (c *Collector) Add(record RequestLog) {
	observedAt := time.Now()
	if record.Started.IsZero() {
		record.Started = observedAt
	}
	if record.StartedAt == "" {
		record.StartedAt = record.Started.Format(time.RFC3339Nano)
	}
	if record.Time == "" {
		record.Time = record.Started.Format("2006-01-02 15:04:05")
	}
	if record.Format == "" {
		record.Format = record.ClientFormat
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalRequests++
	c.addMetricLocked(record, observedAt)
	if len(c.records) < cap(c.records) {
		c.records = append(c.records, record)
		return
	}
	c.records[c.next] = record
	c.next = (c.next + 1) % len(c.records)
	c.full = true
}

func (c *Collector) addMetricLocked(record RequestLog, observedAt time.Time) {
	if record.Started.After(observedAt) {
		// 异常的未来开始时间保留在日志中，但不允许污染当前分钟桶。
		return
	}
	second := record.Started.Unix()
	n := int64(len(c.buckets))
	idx := int(second % n)
	if idx < 0 {
		idx += int(n)
	}
	bucket := &c.buckets[idx]
	if bucket.statusCodes != nil && bucket.second > second {
		// 慢请求可能晚于同槽的新请求完成，不能让迟到记录倒退覆盖新桶。
		return
	}
	if bucket.second != second || bucket.statusCodes == nil {
		bucket.reset(second)
	}
	bucket.requests++
	if isSuccess(record) {
		bucket.successes++
	}
	bucket.statusCodes[strconv.Itoa(record.Status)]++
	bucket.latency.add(record.DurationMs)

	name := record.Provider
	if name == "" {
		name = "(未匹配)"
	}
	provider := bucket.providers[name]
	if provider == nil {
		provider = &providerBucket{}
		bucket.providers[name] = provider
	}
	provider.requests++
	if !isSuccess(record) {
		provider.errors++
	}
	provider.latency.add(record.DurationMs)
}

// Logs 返回按时间倒序排列的记录，支持轻量筛选。
func (c *Collector) Logs(filter LogFilter) []RequestLog {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	out := make([]RequestLog, 0, limit)
	c.mu.RLock()
	c.forEachRecordNewestLocked(func(record RequestLog) bool {
		if !match(record, filter) {
			return true
		}
		if offset > 0 {
			offset--
			return true
		}
		out = append(out, record)
		return len(out) < limit
	})
	c.mu.RUnlock()
	return out
}

// Metrics 聚合最近窗口内的请求指标，并保留历史 provider 排行和最近错误摘要。
func (c *Collector) Metrics(now time.Time) Response {
	c.mu.RLock()
	resp := Response{
		Summary: Summary{
			TotalRequests: int(c.totalRequests),
			WindowMinutes: int(c.windowSeconds / 60),
		},
		StatusCodes: make(map[string]int),
	}
	cutoffSecond := now.Add(-time.Duration(c.windowSeconds) * time.Second).Unix()
	nowSecond := now.Unix()
	var latency latencyHistogram
	successes := 0
	providerTotals := make(map[string]*providerBucket)
	for i := range c.buckets {
		bucket := &c.buckets[i]
		if bucket.requests == 0 || bucket.second < cutoffSecond || bucket.second > nowSecond {
			continue
		}
		resp.Summary.WindowRequests += bucket.requests
		successes += bucket.successes
		latency.merge(bucket.latency)
		for status, count := range bucket.statusCodes {
			resp.StatusCodes[status] += count
		}
		for name, stats := range bucket.providers {
			total := providerTotals[name]
			if total == nil {
				total = &providerBucket{}
				providerTotals[name] = total
			}
			total.requests += stats.requests
			total.errors += stats.errors
			total.latency.merge(stats.latency)
		}
	}
	resp.Summary.LatencySamples = latency.total

	var fallbackProviderTotals map[string]*logProviderStats
	if len(providerTotals) == 0 {
		fallbackProviderTotals = make(map[string]*logProviderStats)
		c.forEachRecordOldestLocked(func(record RequestLog) bool {
			name := record.Provider
			if name == "" {
				name = "(未匹配)"
			}
			stats := fallbackProviderTotals[name]
			if stats == nil {
				stats = &logProviderStats{}
				fallbackProviderTotals[name] = stats
			}
			stats.requests++
			if !isSuccess(record) {
				stats.errors++
			}
			stats.latencies = append(stats.latencies, record.DurationMs)
			return true
		})
	}
	recent := c.recentErrorsLocked(8)
	c.mu.RUnlock()

	resp.Summary.SuccessRate = ratio(successes, resp.Summary.WindowRequests)
	if resp.Summary.WindowRequests > 0 {
		resp.Summary.ErrorRate = 1 - resp.Summary.SuccessRate
	}
	resp.Summary.P50LatencyMs = latency.percentile(50)
	resp.Summary.P95LatencyMs = latency.percentile(95)
	resp.Summary.P99LatencyMs = latency.percentile(99)

	resp.Providers = make([]ProviderStats, 0, len(providerTotals))
	for name, stats := range providerTotals {
		resp.Providers = append(resp.Providers, ProviderStats{
			Name:         name,
			Requests:     stats.requests,
			Errors:       stats.errors,
			SuccessRate:  ratio(stats.requests-stats.errors, stats.requests),
			P50LatencyMs: stats.latency.percentile(50),
			P95LatencyMs: stats.latency.percentile(95),
			P99LatencyMs: stats.latency.percentile(99),
		})
	}
	if len(resp.Providers) == 0 {
		for name, stats := range fallbackProviderTotals {
			resp.Providers = append(resp.Providers, providerStatsFromLogStats(name, stats))
		}
	}
	sort.Slice(resp.Providers, func(i, j int) bool {
		return resp.Providers[i].P95LatencyMs > resp.Providers[j].P95LatencyMs
	})

	resp.RecentErrors = recent
	return resp
}

// LogFilter 是 /api/logs 的查询参数。
type LogFilter struct {
	Limit    int
	Offset   int
	Provider string
	Model    string
	Status   string
	Stream   string
	Query    string
	// Attempt* 过滤器按任意一条 attempt detail 匹配；顶层 status/provider 语义不变。
	AttemptProvider string
	AttemptStatus   string
	AttemptOutcome  string
	Since           time.Time
}

// forEachRecordOldestLocked 按日志原有的从旧到新逻辑顺序遍历记录。
// 调用方必须持有 c.mu 的读锁或写锁；回调返回 false 时提前停止。
func (c *Collector) forEachRecordOldestLocked(fn func(RequestLog) bool) {
	count := len(c.records)
	for i := 0; i < count; i++ {
		index := i
		if c.full {
			index = (c.next + i) % count
		}
		if !fn(c.records[index]) {
			return
		}
	}
}

// forEachRecordNewestLocked 按时间倒序遍历记录。环形缓冲未满、刚满和回绕后
// 都通过 next 计算物理下标，避免先复制整份缓冲再反向扫描。
func (c *Collector) forEachRecordNewestLocked(fn func(RequestLog) bool) {
	count := len(c.records)
	for offset := 0; offset < count; offset++ {
		index := count - 1 - offset
		if c.full {
			index = (c.next - 1 - offset) % count
			if index < 0 {
				index += count
			}
		}
		if !fn(c.records[index]) {
			return
		}
	}
}

func (c *Collector) recentErrorsLocked(limit int) []RequestLog {
	out := make([]RequestLog, 0, limit)
	if limit <= 0 {
		return out
	}
	c.forEachRecordNewestLocked(func(record RequestLog) bool {
		if !isSuccess(record) {
			out = append(out, record)
		}
		return len(out) < limit
	})
	return out
}

func match(r RequestLog, f LogFilter) bool {
	if f.Provider != "" && r.Provider != f.Provider {
		return false
	}
	if f.Model != "" {
		model := strings.ToLower(f.Model)
		if !strings.Contains(strings.ToLower(r.Model), model) &&
			!strings.Contains(strings.ToLower(r.TargetModel), model) &&
			!strings.Contains(strings.ToLower(r.Route), model) {
			return false
		}
	}
	if !f.Since.IsZero() && r.Started.Before(f.Since) {
		return false
	}
	if f.Stream != "" {
		want := f.Stream == "true" || f.Stream == "1"
		if r.Stream != want {
			return false
		}
	}
	if f.Status != "" {
		switch f.Status {
		case "success":
			if !isSuccess(r) {
				return false
			}
		case "error":
			if isSuccess(r) {
				return false
			}
		case "4xx":
			if r.Status < 400 || r.Status >= 500 {
				return false
			}
		case "5xx":
			if r.Status < 500 {
				return false
			}
		default:
			code, err := strconv.Atoi(f.Status)
			if err == nil && r.Status != code {
				return false
			}
		}
	}
	if f.AttemptProvider != "" && !matchesAttemptProvider(r.AttemptDetails, f.AttemptProvider) {
		return false
	}
	if f.AttemptStatus != "" && !matchesAttemptStatus(r.AttemptDetails, f.AttemptStatus) {
		return false
	}
	if f.AttemptOutcome != "" && !matchesAttemptOutcome(r.AttemptDetails, f.AttemptOutcome) {
		return false
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		haystack := strings.ToLower(strings.Join([]string{r.ID, r.Path, r.Model, r.Provider, r.TargetModel, r.Error}, " "))
		for _, detail := range r.AttemptDetails {
			haystack += " " + strings.ToLower(strings.Join([]string{
				detail.Provider, detail.TargetModel, detail.ProviderFormat, detail.Reason, detail.Error, detail.UpstreamRequestID,
			}, " "))
		}
		if !strings.Contains(haystack, q) {
			return false
		}
	}
	return true
}

func matchesAttemptProvider(details []AttemptDetail, provider string) bool {
	for _, detail := range details {
		if detail.Provider == provider {
			return true
		}
	}
	return false
}

func matchesAttemptOutcome(details []AttemptDetail, outcome string) bool {
	for _, detail := range details {
		if detail.Outcome == outcome {
			return true
		}
	}
	return false
}

func matchesAttemptStatus(details []AttemptDetail, status string) bool {
	for _, detail := range details {
		switch status {
		case "error":
			if detail.Kind == "request" && detail.UpstreamStatus == 0 && detail.Error != "" {
				return true
			}
		case "4xx":
			if detail.UpstreamStatus >= 400 && detail.UpstreamStatus < 500 {
				return true
			}
		case "5xx":
			if detail.UpstreamStatus >= 500 {
				return true
			}
		default:
			code, err := strconv.Atoi(status)
			if err == nil && detail.UpstreamStatus == code {
				return true
			}
		}
	}
	return false
}

func providerStats(name string, rows []RequestLog) ProviderStats {
	latencies := make([]int64, 0, len(rows))
	errors := 0
	for _, r := range rows {
		latencies = append(latencies, r.DurationMs)
		if !isSuccess(r) {
			errors++
		}
	}
	return ProviderStats{
		Name:         name,
		Requests:     len(rows),
		Errors:       errors,
		SuccessRate:  ratio(len(rows)-errors, len(rows)),
		P50LatencyMs: percentile(latencies, 50),
		P95LatencyMs: percentile(latencies, 95),
		P99LatencyMs: percentile(latencies, 99),
	}
}

func providerStatsFromLogStats(name string, stats *logProviderStats) ProviderStats {
	return ProviderStats{
		Name:         name,
		Requests:     stats.requests,
		Errors:       stats.errors,
		SuccessRate:  ratio(stats.requests-stats.errors, stats.requests),
		P50LatencyMs: percentile(stats.latencies, 50),
		P95LatencyMs: percentile(stats.latencies, 95),
		P99LatencyMs: percentile(stats.latencies, 99),
	}
}

func recentErrors(records []RequestLog, limit int) []RequestLog {
	out := make([]RequestLog, 0, limit)
	for i := len(records) - 1; i >= 0 && len(out) < limit; i-- {
		if !isSuccess(records[i]) {
			out = append(out, records[i])
		}
	}
	return out
}

func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)*p + 99) / 100
	if idx <= 0 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

func ratio(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func isSuccess(r RequestLog) bool {
	return r.Error == "" && r.Status >= 200 && r.Status < 400
}
