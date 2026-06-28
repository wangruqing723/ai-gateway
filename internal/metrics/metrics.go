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
	windowDuration  = time.Minute
)

// Collector 保存最近一段请求记录。写入只做环形追加，聚合在读取时基于快照计算。
type Collector struct {
	mu      sync.RWMutex
	records []RequestLog
	next    int
	full    bool
}

// RequestLog 是单次请求的观测记录。
type RequestLog struct {
	ID             string    `json:"id"`
	Time           string    `json:"time"`
	StartedAt      string    `json:"startedAt"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	ClientIP       string    `json:"clientIp,omitempty"`
	KeySource      string    `json:"keySource,omitempty"`
	KeyFingerprint string    `json:"keyFingerprint,omitempty"`
	Status         int       `json:"status"`
	ClientFormat   string    `json:"clientFormat"`
	Format         string    `json:"format"`
	Model          string    `json:"model"`
	Route          string    `json:"route,omitempty"`
	Provider       string    `json:"provider"`
	TargetModel    string    `json:"targetModel,omitempty"`
	Stream         bool      `json:"stream"`
	DurationMs     int64     `json:"durationMs"`
	QueueWaitMs    int64     `json:"queueWaitMs,omitempty"`
	Error          string    `json:"error,omitempty"`
	Vision         bool      `json:"vision,omitempty"`
	ResponseBytes  int64     `json:"responseBytes,omitempty"`
	UpstreamStatus int       `json:"upstreamStatus,omitempty"`
	Started        time.Time `json:"-"`
}

// Summary 是仪表盘顶部指标。
type Summary struct {
	RequestsPerMinute int     `json:"requestsPerMinute"`
	TotalRequests     int     `json:"totalRequests"`
	SuccessRate       float64 `json:"successRate"`
	ErrorRate         float64 `json:"errorRate"`
	P50LatencyMs      int64   `json:"p50LatencyMs"`
	P95LatencyMs      int64   `json:"p95LatencyMs"`
	P99LatencyMs      int64   `json:"p99LatencyMs"`
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

// NewCollector 创建固定容量的内存采集器。
func NewCollector(capacity int) *Collector {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	return &Collector{records: make([]RequestLog, 0, capacity)}
}

// Add 追加一条记录。
func (c *Collector) Add(record RequestLog) {
	if record.Started.IsZero() {
		record.Started = time.Now()
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
	if len(c.records) < cap(c.records) {
		c.records = append(c.records, record)
		return
	}
	c.records[c.next] = record
	c.next = (c.next + 1) % len(c.records)
	c.full = true
}

// Logs 返回按时间倒序排列的记录，支持轻量筛选。
func (c *Collector) Logs(filter LogFilter) []RequestLog {
	records := c.snapshot()
	out := make([]RequestLog, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		if match(records[i], filter) {
			out = append(out, records[i])
		}
	}
	if filter.Offset > 0 {
		if filter.Offset >= len(out) {
			return []RequestLog{}
		}
		out = out[filter.Offset:]
	}
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 100
	}
	if len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}

// Metrics 聚合最近一分钟的请求指标，并保留历史 provider 排行和最近错误摘要。
func (c *Collector) Metrics(now time.Time) Response {
	records := c.snapshot()
	cutoff := now.Add(-windowDuration)
	window := make([]RequestLog, 0, len(records))
	for _, r := range records {
		if !r.Started.IsZero() && !r.Started.Before(cutoff) {
			window = append(window, r)
		}
	}
	providerBase := window
	if len(providerBase) == 0 {
		providerBase = records
	}

	resp := Response{
		Summary: Summary{
			RequestsPerMinute: len(window),
			TotalRequests:     len(records),
		},
		StatusCodes: make(map[string]int),
	}
	if len(records) == 0 {
		resp.Providers = []ProviderStats{}
		resp.RecentErrors = []RequestLog{}
		return resp
	}

	latencies := make([]int64, 0, len(window))
	successes := 0
	for _, r := range window {
		latencies = append(latencies, r.DurationMs)
		if isSuccess(r) {
			successes++
		}
		resp.StatusCodes[strconv.Itoa(r.Status)]++
	}
	resp.Summary.SuccessRate = ratio(successes, len(window))
	if len(window) > 0 {
		resp.Summary.ErrorRate = 1 - resp.Summary.SuccessRate
	}
	resp.Summary.P50LatencyMs = percentile(latencies, 50)
	resp.Summary.P95LatencyMs = percentile(latencies, 95)
	resp.Summary.P99LatencyMs = percentile(latencies, 99)

	providerMap := make(map[string][]RequestLog)
	for _, r := range providerBase {
		name := r.Provider
		if name == "" {
			name = "(未匹配)"
		}
		providerMap[name] = append(providerMap[name], r)
	}

	resp.Providers = make([]ProviderStats, 0, len(providerMap))
	for name, rows := range providerMap {
		resp.Providers = append(resp.Providers, providerStats(name, rows))
	}
	sort.Slice(resp.Providers, func(i, j int) bool {
		return resp.Providers[i].P95LatencyMs > resp.Providers[j].P95LatencyMs
	})

	resp.RecentErrors = recentErrors(records, 8)
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
	Since    time.Time
}

func (c *Collector) snapshot() []RequestLog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.records) == 0 {
		return []RequestLog{}
	}
	out := make([]RequestLog, 0, len(c.records))
	if c.full {
		out = append(out, c.records[c.next:]...)
		out = append(out, c.records[:c.next]...)
		return out
	}
	out = append(out, c.records...)
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
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		haystack := strings.ToLower(strings.Join([]string{r.ID, r.Path, r.Model, r.Provider, r.TargetModel, r.Error}, " "))
		if !strings.Contains(haystack, q) {
			return false
		}
	}
	return true
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
