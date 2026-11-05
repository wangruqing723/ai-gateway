package chatgptauth

import (
	"time"

	"ai-gateway/internal/metrics"
)

// 账户统计只属于当前进程：按分钟分桶的窗口统计加进程累计，不写入账户文件。
// 分桶固定 61 个（窗口上限 60 分钟再多一桶），窗口热重载时不必重新分配。
const (
	statsBucketCount          = 61
	defaultStatsWindowMinutes = 15
	maxStatsWindowMinutes     = 60
)

// 准入跳过原因：只描述没有发出上游请求的放弃，不计入真实调用。
const (
	SkipAccountPaused = "account_paused"
	SkipAuthorization = "authorization"
	SkipQueue         = "queue"
	SkipCooldown      = "cooldown"
)

// statCounters 是一个统计单元（一分钟桶或进程累计）。
type statCounters struct {
	requests     int
	successes    int
	failures     int
	localRetries int
	rateLimited  int
	latency      metrics.LatencyHistogram

	skipPaused        int
	skipAuthorization int
	skipQueue         int
	skipCooldown      int

	diagnosticSuccesses int
	diagnosticFailures  int
	refreshSuccesses    int
	refreshFailures     int
	usageSuccesses      int
	usageFailures       int
}

func (c *statCounters) merge(other *statCounters) {
	c.requests += other.requests
	c.successes += other.successes
	c.failures += other.failures
	c.localRetries += other.localRetries
	c.rateLimited += other.rateLimited
	c.latency.Merge(other.latency)
	c.skipPaused += other.skipPaused
	c.skipAuthorization += other.skipAuthorization
	c.skipQueue += other.skipQueue
	c.skipCooldown += other.skipCooldown
	c.diagnosticSuccesses += other.diagnosticSuccesses
	c.diagnosticFailures += other.diagnosticFailures
	c.refreshSuccesses += other.refreshSuccesses
	c.refreshFailures += other.refreshFailures
	c.usageSuccesses += other.usageSuccesses
	c.usageFailures += other.usageFailures
}

type statBucket struct {
	// minute 是 Unix 分钟数；0 表示该桶从未使用。
	minute   int64
	counters statCounters
}

// accountStats 挂在账户内存对象上：删除账户即释放，删除后重建的账户 ID 不同，不继承旧统计。
// 由 Manager.statsMu 保护。
type accountStats struct {
	buckets       []statBucket
	total         statCounters
	lastSuccessAt time.Time
	lastFailureAt time.Time
}

// AccountStatsView 是账户调用统计的安全视图。
type AccountStatsView struct {
	AccountID     string              `json:"accountId"`
	WindowMinutes int                 `json:"windowMinutes"`
	Since         string              `json:"since"`
	Window        AccountStatsSummary `json:"window"`
	Total         AccountStatsSummary `json:"total"`
	LastSuccessAt string              `json:"lastSuccessAt,omitempty"`
	LastFailureAt string              `json:"lastFailureAt,omitempty"`
}

// AccountStatsSummary 是一个统计范围内的汇总。没有样本时比率和延迟为 nil。
type AccountStatsSummary struct {
	Requests       int              `json:"requests"`
	Successes      int              `json:"successes"`
	Failures       int              `json:"failures"`
	LocalRetries   int              `json:"localRetries"`
	RateLimited    int              `json:"rateLimited"`
	SuccessRate    *float64         `json:"successRate"`
	LatencySamples int              `json:"latencySamples"`
	P50LatencyMs   *int64           `json:"p50LatencyMs"`
	P95LatencyMs   *int64           `json:"p95LatencyMs"`
	Skips          AccountStatSkips `json:"skips"`
	Diagnostics    AccountStatPair  `json:"diagnostics"`
	Refresh        AccountStatPair  `json:"refresh"`
	Usage          AccountStatPair  `json:"usage"`
}

// AccountStatSkips 按原因统计准入跳过。
type AccountStatSkips struct {
	AccountPaused int `json:"accountPaused"`
	Authorization int `json:"authorization"`
	Queue         int `json:"queue"`
	Cooldown      int `json:"cooldown"`
}

// AccountStatPair 是成功、失败计数对。
type AccountStatPair struct {
	Successes int `json:"successes"`
	Failures  int `json:"failures"`
}

func summaryOf(c *statCounters) AccountStatsSummary {
	summary := AccountStatsSummary{
		Requests: c.requests, Successes: c.successes, Failures: c.failures,
		LocalRetries: c.localRetries, RateLimited: c.rateLimited,
		LatencySamples: c.latency.Count(),
		Skips:          AccountStatSkips{AccountPaused: c.skipPaused, Authorization: c.skipAuthorization, Queue: c.skipQueue, Cooldown: c.skipCooldown},
		Diagnostics:    AccountStatPair{Successes: c.diagnosticSuccesses, Failures: c.diagnosticFailures},
		Refresh:        AccountStatPair{Successes: c.refreshSuccesses, Failures: c.refreshFailures},
		Usage:          AccountStatPair{Successes: c.usageSuccesses, Failures: c.usageFailures},
	}
	if c.requests > 0 {
		rate := float64(c.successes) / float64(c.requests)
		summary.SuccessRate = &rate
	}
	if summary.LatencySamples > 0 {
		p50, p95 := c.latency.Percentile(50), c.latency.Percentile(95)
		summary.P50LatencyMs, summary.P95LatencyMs = &p50, &p95
	}
	return summary
}

// SetStatsWindow 设置账户统计窗口分钟数，超出 [1, 60] 时夹到边界，0 或负数回落默认值。
// 已有分桶保留，窗口缩小或放大只改变汇总范围。
func (m *Manager) SetStatsWindow(minutes int) {
	if minutes <= 0 {
		minutes = defaultStatsWindowMinutes
	}
	if minutes > maxStatsWindowMinutes {
		minutes = maxStatsWindowMinutes
	}
	m.statsMu.Lock()
	m.statsWindowMinutes = minutes
	m.statsMu.Unlock()
}

// recordStatLocked 把一次更新同时写入当前分钟桶和进程累计。调用方持有 m.statsMu。
func (m *Manager) recordStatLocked(item *account, at time.Time, update func(*statCounters)) {
	if item.stats == nil {
		item.stats = &accountStats{}
	}
	stats := item.stats
	if stats.buckets == nil {
		stats.buckets = make([]statBucket, statsBucketCount)
	}
	minute := at.Unix() / 60
	index := int(minute % statsBucketCount)
	if index < 0 {
		index += statsBucketCount
	}
	bucket := &stats.buckets[index]
	if bucket.minute != minute {
		*bucket = statBucket{minute: minute}
	}
	update(&bucket.counters)
	update(&stats.total)
}

func (m *Manager) recordStat(item *account, update func(*statCounters)) {
	if item == nil {
		return
	}
	at := m.now()
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	m.recordStatLocked(item, at, update)
}

// RecordAdmissionSkip 记录一次准入跳过：账户暂停、取令牌失败或队列等待失败，均未发出上游请求。
// 诊断调用由调用方排除，不进入此统计。
func (m *Manager) RecordAdmissionSkip(accountID, reason string) {
	m.mu.RLock()
	item := m.accounts[accountID]
	m.mu.RUnlock()
	m.recordStat(item, func(c *statCounters) {
		switch reason {
		case SkipAccountPaused:
			c.skipPaused++
		case SkipAuthorization:
			c.skipAuthorization++
		case SkipCooldown:
			c.skipCooldown++
		default:
			c.skipQueue++
		}
	})
}

// AccountStats 返回账户调用统计；账户不存在时返回 false。
func (m *Manager) AccountStats(id string) (AccountStatsView, bool) {
	m.mu.RLock()
	item := m.accounts[id]
	m.mu.RUnlock()
	if item == nil {
		return AccountStatsView{}, false
	}
	now := m.now()
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	windowMinutes := m.statsWindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = defaultStatsWindowMinutes
	}
	view := AccountStatsView{AccountID: id, WindowMinutes: windowMinutes, Since: m.statsSince.UTC().Format(time.RFC3339)}
	var window statCounters
	var total statCounters
	if stats := item.stats; stats != nil {
		current := now.Unix() / 60
		oldest := current - int64(windowMinutes) + 1
		for i := range stats.buckets {
			bucket := &stats.buckets[i]
			if bucket.minute != 0 && bucket.minute >= oldest && bucket.minute <= current {
				window.merge(&bucket.counters)
			}
		}
		total = stats.total
		if !stats.lastSuccessAt.IsZero() {
			view.LastSuccessAt = stats.lastSuccessAt.UTC().Format(time.RFC3339)
		}
		if !stats.lastFailureAt.IsZero() {
			view.LastFailureAt = stats.lastFailureAt.UTC().Format(time.RFC3339)
		}
	}
	view.Window = summaryOf(&window)
	view.Total = summaryOf(&total)
	return view, true
}
