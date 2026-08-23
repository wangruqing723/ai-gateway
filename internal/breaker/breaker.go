// Package breaker 提供 per-provider 熔断器，供候选循环在发起请求前过滤掉已知故障的上游。
//
// 不复用 internal/metrics：那套有界秒桶 + 固定延迟直方图是仪表盘语义，
// 把熔断判据混进去会让它同时背两个职责。这里自己维护极小的计数器状态。
//
// 状态机：closed →（连续失败达阈值）→ open →（冷却结束）→ half_open →（探测结果）→ closed / open。
// 半开探测直接用下一个真实入站请求当探针，不额外探 /v1/models —— 那个端点通了
// 不代表聊天端点可用。
package breaker

import (
	"sort"
	"sync"
	"time"
)

// Outcome 一次尝试对熔断计数的影响。
type Outcome int

const (
	// OutcomeSuccess 视为健康：清零连续失败计数，半开时直接闭合。
	OutcomeSuccess Outcome = iota
	// OutcomeFailure 计入失败：传输错误、5xx、超时。
	OutcomeFailure
	// OutcomeIgnored 不参与熔断判据。
	//
	// 429：上游正常限流，判成故障比不熔断更糟（它自己走 failover 的 Retry-After 路径）。
	// 401/403：配置问题，熔断修不了，还会掩盖密钥过期。
	// 本地队列超时：属于网关侧背压，与上游健康无关。
	OutcomeIgnored
)

// 状态名，同时作为 /health 与前端展示用的取值。
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"
)

// 默认参数：零值 Settings 也能安全工作。
const (
	defaultConsecutiveFailures = 3
	defaultOpenMs              = 30_000
	defaultHalfOpenProbes      = 1
)

// State 单个 provider 的熔断状态快照。
type State struct {
	State               string `json:"state"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	OpenedAt            string `json:"openedAt,omitempty"`
	RetryAfterMs        int64  `json:"retryAfterMs,omitempty"`
	ProbesInFlight      int    `json:"probesInFlight,omitempty"`
	TotalOpens          int    `json:"totalOpens,omitempty"`
}

// Settings 熔断参数。全局一份，不做 per-provider 覆盖：
// per-provider 覆盖会让「为什么这个上游被摘掉」变得难以解释。
type Settings struct {
	Enabled             bool
	ConsecutiveFailures int
	OpenMs              int
	HalfOpenProbes      int
}

func normalize(s Settings) Settings {
	if s.ConsecutiveFailures <= 0 {
		s.ConsecutiveFailures = defaultConsecutiveFailures
	}
	if s.OpenMs <= 0 {
		s.OpenMs = defaultOpenMs
	}
	if s.HalfOpenProbes <= 0 {
		s.HalfOpenProbes = defaultHalfOpenProbes
	}
	return s
}

type providerState struct {
	state          string
	failures       int
	openedAt       time.Time
	probesInFlight int
	totalOpens     int
}

// Breaker 是所有 provider 熔断状态的持有者，并发安全。
type Breaker struct {
	mu       sync.Mutex
	settings Settings
	states   map[string]*providerState
	now      func() time.Time
}

// New 按给定参数构造熔断器。
func New(s Settings) *Breaker {
	return &Breaker{
		settings: normalize(s),
		states:   make(map[string]*providerState),
		now:      time.Now,
	}
}

// SetSettings 热重载熔断参数。
// 从启用切换到禁用时清空全部状态，避免再启用时沿用过期判据。
func (b *Breaker) SetSettings(s Settings) {
	next := normalize(s)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.settings.Enabled && !next.Enabled {
		b.states = make(map[string]*providerState)
	}
	b.settings = next
}

// Allow 判断该 provider 当前能否接受请求。
// 返回 false 时第二个返回值是距离下次可尝试的剩余时间（可能为 0，表示只是探针额度已满）。
func (b *Breaker) Allow(provider string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.settings.Enabled {
		return true, 0
	}
	st := b.states[provider]
	if st == nil || st.state == StateClosed {
		return true, 0
	}

	switch st.state {
	case StateOpen:
		cooldown := time.Duration(b.settings.OpenMs) * time.Millisecond
		if elapsed := b.now().Sub(st.openedAt); elapsed < cooldown {
			return false, cooldown - elapsed
		}
		// 冷却结束：本次请求充当半开探针
		st.state = StateHalfOpen
		st.probesInFlight = 1
		return true, 0
	case StateHalfOpen:
		if st.probesInFlight < b.settings.HalfOpenProbes {
			st.probesInFlight++
			return true, 0
		}
		// 探针额度已满：让请求去别的候选，别在这排队等结论
		return false, 0
	default:
		return true, 0
	}
}

// Report 汇报一次尝试的结果。必须与返回 true 的 Allow 一一对应。
func (b *Breaker) Report(provider string, outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.settings.Enabled {
		return
	}
	st := b.states[provider]
	if st == nil {
		st = &providerState{state: StateClosed}
		b.states[provider] = st
	}
	// 探针占用先释放：无论结论如何，这次探测都已结束
	if st.probesInFlight > 0 {
		st.probesInFlight--
	}

	switch outcome {
	case OutcomeIgnored:
		// 不改判据，也不清零：半开状态保持，等下一个探针给结论
		return
	case OutcomeSuccess:
		st.state = StateClosed
		st.failures = 0
		st.probesInFlight = 0
		st.openedAt = time.Time{}
	case OutcomeFailure:
		st.failures++
		switch st.state {
		case StateHalfOpen:
			// 探测失败：重新开路，冷却从现在重算
			st.state = StateOpen
			st.openedAt = b.now()
			st.probesInFlight = 0
			st.totalOpens++
		case StateClosed:
			if st.failures >= b.settings.ConsecutiveFailures {
				st.state = StateOpen
				st.openedAt = b.now()
				st.totalOpens++
			}
		}
	}
}

// Reset 手动闭合单个 provider 的熔断器。返回是否命中已有状态。
func (b *Breaker) Reset(provider string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[provider]
	if !ok {
		return false
	}
	st.state = StateClosed
	st.failures = 0
	st.probesInFlight = 0
	st.openedAt = time.Time{}
	return true
}

// ResetAll 手动闭合全部熔断器，返回被重置的 provider 数量。
func (b *Breaker) ResetAll() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, st := range b.states {
		if st.state != StateClosed || st.failures > 0 {
			n++
		}
		st.state = StateClosed
		st.failures = 0
		st.probesInFlight = 0
		st.openedAt = time.Time{}
	}
	return n
}

// Snapshot 返回全部 provider 的状态快照，供 /health 与前端展示。
// 禁用时返回 nil，让调用方能区分「没开」和「都健康」。
func (b *Breaker) Snapshot() map[string]State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.settings.Enabled {
		return nil
	}
	now := b.now()
	cooldown := time.Duration(b.settings.OpenMs) * time.Millisecond
	out := make(map[string]State, len(b.states))
	for name, st := range b.states {
		s := State{
			State:               st.state,
			ConsecutiveFailures: st.failures,
			ProbesInFlight:      st.probesInFlight,
			TotalOpens:          st.totalOpens,
		}
		if !st.openedAt.IsZero() {
			s.OpenedAt = st.openedAt.Format(time.RFC3339)
		}
		if st.state == StateOpen {
			if remaining := cooldown - now.Sub(st.openedAt); remaining > 0 {
				s.RetryAfterMs = remaining.Milliseconds()
			}
		}
		out[name] = s
	}
	return out
}

// Reconcile 丢弃已从配置中删除的 provider 状态，避免 map 无界增长。
func (b *Breaker) Reconcile(active map[string]struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range b.states {
		if _, ok := active[name]; !ok {
			delete(b.states, name)
		}
	}
}

// OpenProviders 返回当前处于开路状态的 provider 名（已排序），用于日志与错误信息。
func (b *Breaker) OpenProviders() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.states))
	for name, st := range b.states {
		if st.state == StateOpen {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
