package providerbalance

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Source 是调用方已解析的查询身份，Key 只能是进程内摘要。
// 相同 Key 必须代表相同连接、凭据、类型和来源，Fetch 必须响应 context 取消。
type Source struct {
	Key        string
	Type, Mode string
	Enabled    bool
	Interval   time.Duration
	Fetch      func(context.Context) (*Snapshot, *QueryError)
}

type View struct {
	Type              string      `json:"type"`
	Source            string      `json:"source"`
	State             string      `json:"state"`
	Freshness         string      `json:"freshness"`
	Refreshing        bool        `json:"refreshing"`
	LastAttemptAt     *string     `json:"lastAttemptAt"`
	LastSuccessAt     *string     `json:"lastSuccessAt"`
	NextRefreshAt     *string     `json:"nextRefreshAt"`
	RetryAfterSeconds *int64      `json:"retryAfterSeconds"`
	Error             *QueryError `json:"error"`
	Snapshot          *Snapshot   `json:"snapshot"`
	SharedProviders   []string    `json:"sharedProviders"`
}

type Result struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Balance  View   `json:"balance"`
}

type managerBinding struct {
	state          *balanceSource
	typeName, mode string
	changed        chan struct{}
}

type balanceSource struct {
	source                             Source
	snapshot                           *Snapshot
	err                                *QueryError
	attempt, success, retryUntil, next time.Time
	interval                           time.Duration
	failures                           uint
	blocked                            bool
	job, automatic                     *balanceJob
}

type balanceJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	refs   int
	status string
}

// 包内钩子允许测试推进逻辑时间，不用等待生产周期。
type managerOptions struct {
	now                                            func() time.Time
	ttl, minimum, minInterval, maxInterval, budget time.Duration
	jitter                                         func(time.Duration) time.Duration
}

// Manager 只维护进程内余额观察，不依赖配置、转发或密钥冷却组件。
type Manager struct {
	mu        sync.Mutex
	providers map[string]*managerBinding
	sources   map[string]*balanceSource
	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	semaphore chan struct{}
	wg        sync.WaitGroup
	closed    bool
	closeDone chan struct{}
	options   managerOptions
}

func New() *Manager {
	return newManager(managerOptions{
		now: time.Now, ttl: 5 * time.Minute, minimum: 30 * time.Second,
		minInterval: 5 * time.Minute, maxInterval: 24 * time.Hour, budget: 15 * time.Second,
		jitter: func(d time.Duration) time.Duration {
			return time.Duration(rand.Float64() * float64(d) / 10)
		},
	})
}

func newManager(options managerOptions) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		providers: make(map[string]*managerBinding), sources: make(map[string]*balanceSource),
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), semaphore: make(chan struct{}, 2),
		closeDone: make(chan struct{}), options: options,
	}
	m.wg.Add(1)
	go m.schedule()
	return m
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Reconcile 在调用方的配置事务锁内执行，同摘要共享状态，身份变化立即隔离。
func (m *Manager) Reconcile(input map[string]Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	providers := make(map[string]*managerBinding, len(input))
	sources := make(map[string]*balanceSource)
	intervals := make(map[string]time.Duration)
	for name, source := range input {
		if source.Key == "" {
			continue
		}
		state := sources[source.Key]
		if state == nil {
			state = m.sources[source.Key]
			if state == nil {
				state = &balanceSource{}
			}
			// 捕获新闭包但不替换在途任务的 Fetch，后者有独立身份检查。
			state.source = source
			sources[source.Key] = state
		}
		old := m.providers[name]
		if old != nil && old.state == state {
			providers[name] = old
		} else {
			providers[name] = &managerBinding{state: state, typeName: source.Type, mode: source.Mode, changed: make(chan struct{})}
		}
		if source.Enabled && source.Interval > 0 {
			interval := source.Interval
			if interval < m.options.minInterval {
				interval = m.options.minInterval
			}
			if interval > m.options.maxInterval {
				interval = m.options.maxInterval
			}
			if intervals[source.Key] == 0 || interval < intervals[source.Key] {
				intervals[source.Key] = interval
			}
		}
	}
	for name, old := range m.providers {
		if providers[name] != old {
			close(old.changed)
		}
	}
	for key, state := range m.sources {
		if sources[key] != state {
			m.stopAutomaticLocked(state)
			if state.job != nil {
				state.job.cancel()
			}
		}
	}
	now := m.options.now()
	for key, state := range sources {
		interval := intervals[key]
		if interval == 0 {
			m.stopAutomaticLocked(state)
			state.next = time.Time{}
		} else if state.blocked {
			state.next = time.Time{}
		} else if state.automatic == nil && (interval != state.interval || state.next.IsZero()) {
			state.next = m.nextTime(state, now, interval)
		}
		state.interval = interval
	}
	m.providers, m.sources = providers, sources
	m.signal()
}

func balanceTimestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	value := t.UTC().Format(time.RFC3339Nano)
	return &value
}

func cloneBalanceError(err *QueryError) *QueryError {
	if err == nil {
		return nil
	}
	copy := *err
	return &copy
}

func (m *Manager) viewLocked(provider string) View {
	binding := m.providers[provider]
	if binding == nil {
		return View{State: "not_configured", Freshness: "none", SharedProviders: []string{}}
	}
	s := binding.state
	v := View{
		Type: binding.typeName, Source: binding.mode, State: "not_queried", Freshness: "none", Refreshing: s.job != nil,
		LastAttemptAt: balanceTimestamp(s.attempt), LastSuccessAt: balanceTimestamp(s.success), NextRefreshAt: balanceTimestamp(s.next),
		Error: cloneBalanceError(s.err), Snapshot: s.snapshot.Clone(), SharedProviders: []string{},
	}
	for name, other := range m.providers {
		if name != provider && other.state == s {
			v.SharedProviders = append(v.SharedProviders, name)
		}
	}
	sort.Strings(v.SharedProviders)
	if s.snapshot != nil {
		v.State, v.Freshness = "ok", "fresh"
		if !m.options.now().Before(s.success.Add(m.options.ttl)) {
			v.Freshness = "stale"
		}
	}
	if s.err != nil {
		v.State = "failed"
		switch s.err.Code {
		case "unsupported_endpoint":
			v.State = "unsupported"
		case "credentials_required", "query_key_unavailable":
			v.State = "credentials_required"
		}
	}
	if remaining := s.retryUntil.Sub(m.options.now()); remaining > 0 {
		seconds := int64(remaining / time.Second)
		if remaining%time.Second != 0 {
			seconds++
		}
		v.RetryAfterSeconds = &seconds
	}
	return v
}

// View 只读取本地快照，永不触发出网。
func (m *Manager) View(provider string) View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(provider)
}

func balanceManagerError(code string) *QueryError {
	if code == "configuration_changed" {
		return &QueryError{Code: code, Message: "余额查询配置已变化", HTTPStatus: 409}
	}
	return &QueryError{Code: "query_cancelled", Message: "余额查询已取消", HTTPStatus: 499}
}

func (m *Manager) Refresh(ctx context.Context, provider string) Result {
	return m.refresh(ctx, provider, "")
}

// RefreshSource 在管理器锁内核对调用方捕获的来源身份，避免配置校验与订阅之间的竞态。
// expectedKey 为空时与 Refresh 相同，不进行预期身份核验。
func (m *Manager) RefreshSource(ctx context.Context, provider, expectedKey string) Result {
	return m.refresh(ctx, provider, expectedKey)
}

func (m *Manager) refresh(ctx context.Context, provider, expectedKey string) Result {
	m.mu.Lock()
	binding := m.providers[provider]
	result := Result{Provider: provider, Balance: m.viewLocked(provider)}
	if expectedKey != "" && (binding == nil || binding.state.source.Key != expectedKey) {
		result.Status, result.Balance.Error = "skipped", balanceManagerError("configuration_changed")
		m.mu.Unlock()
		return result
	}
	if m.closed || ctx.Err() != nil {
		result.Status, result.Balance.Error = "cancelled", balanceManagerError("query_cancelled")
		m.mu.Unlock()
		return result
	}
	if binding == nil {
		result.Status = "skipped"
		m.mu.Unlock()
		return result
	}
	job, limited := m.subscribeLocked(binding.state)
	if limited {
		result.Status = "skipped"
		if binding.state.snapshot != nil {
			result.Status = "cached"
		}
		m.mu.Unlock()
		return result
	}
	m.mu.Unlock()

	cancelled := false
	select {
	case <-job.done:
	case <-binding.changed:
	case <-ctx.Done():
		// 批次结束与完成同时可见时，保留已经完成的结果。
		select {
		case <-job.done:
		default:
			cancelled = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unsubscribeLocked(job)
	result.Balance = m.viewLocked(provider)
	switch {
	case m.providers[provider] != binding:
		result.Status, result.Balance.Error = "skipped", balanceManagerError("configuration_changed")
	case cancelled || m.closed || job.status == "cancelled":
		result.Status, result.Balance.Error = "cancelled", balanceManagerError("query_cancelled")
	default:
		result.Status = job.status
	}
	return result
}

// 独立任务预算不继承首个订阅者，等待全局槽位也占用总预算。
func (m *Manager) subscribeLocked(state *balanceSource) (*balanceJob, bool) {
	if state.job != nil {
		if state.job.ctx.Err() == context.Canceled {
			return nil, true
		}
		state.job.refs++
		return state.job, false
	}
	now := m.options.now()
	if (!state.attempt.IsZero() && now.Before(state.attempt.Add(m.options.minimum))) || now.Before(state.retryUntil) {
		return nil, true
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.options.budget)
	job := &balanceJob{ctx: ctx, cancel: cancel, done: make(chan struct{}), refs: 1}
	state.job, state.attempt = job, now
	fetch := state.source.Fetch
	m.wg.Add(1)
	go m.run(state, job, fetch)
	return job, false
}

func (m *Manager) unsubscribeLocked(job *balanceJob) {
	job.refs--
	if job.refs == 0 {
		job.cancel()
	}
}

func (m *Manager) stopAutomaticLocked(state *balanceSource) {
	if state.automatic != nil {
		m.unsubscribeLocked(state.automatic)
		state.automatic = nil
	}
}

func (m *Manager) run(state *balanceSource, job *balanceJob, fetch func(context.Context) (*Snapshot, *QueryError)) {
	defer m.wg.Done()
	defer job.cancel()
	var snapshot *Snapshot
	var queryErr *QueryError
	select {
	case m.semaphore <- struct{}{}:
		if job.ctx.Err() == nil {
			if fetch == nil {
				queryErr = &QueryError{Code: "credentials_required", Message: "余额查询需要凭据", HTTPStatus: 400}
			} else {
				snapshot, queryErr = fetch(job.ctx)
			}
		}
		<-m.semaphore
	case <-job.ctx.Done():
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	job.status = "cancelled"
	if job.ctx.Err() == context.DeadlineExceeded {
		queryErr = &QueryError{Code: "upstream_timeout", Message: "余额查询超时", HTTPStatus: 504}
	}
	current := !m.closed && m.sources[state.source.Key] == state && state.job == job
	if current && job.ctx.Err() != context.Canceled && (queryErr == nil || queryErr.Code != "query_cancelled") {
		if queryErr == nil && snapshot == nil {
			queryErr = &QueryError{Code: "invalid_response", Message: "上游余额响应无效", HTTPStatus: 502}
		}
		if queryErr != nil {
			state.err = cloneBalanceError(queryErr)
			state.failures++
			if queryErr.RetryAfter > 0 {
				state.retryUntil = m.options.now().Add(queryErr.RetryAfter)
			}
			state.blocked = queryErr.Code == "credential_rejected" || queryErr.Code == "permission_denied"
			job.status = "failed"
		} else {
			state.snapshot, state.success, state.err = snapshot.Clone(), m.options.now(), nil
			state.failures, state.blocked, state.retryUntil = 0, false, time.Time{}
			job.status = "success"
		}
	}
	if state.job == job {
		state.job = nil
	}
	if state.automatic == job {
		m.stopAutomaticLocked(state)
	}
	if current {
		if state.interval > 0 && !state.blocked {
			state.next = m.nextTime(state, m.options.now(), state.interval)
		} else {
			state.next = time.Time{}
		}
	}
	close(job.done)
	m.signal()
}

func (m *Manager) nextTime(state *balanceSource, now time.Time, interval time.Duration) time.Time {
	delay := interval
	for n := uint(0); n < state.failures && delay < m.options.maxInterval; n++ {
		if delay > m.options.maxInterval/2 {
			delay = m.options.maxInterval
		} else {
			delay *= 2
		}
	}
	jitter := m.options.jitter(delay)
	if jitter < 0 {
		jitter = 0
	}
	if jitter > delay/10 {
		jitter = delay / 10
	}
	if delay > m.options.maxInterval-jitter {
		delay = m.options.maxInterval
	} else {
		delay += jitter
	}
	next := now.Add(delay)
	// 上游 Retry-After 不受本地二十四小时退避上限裁短。
	if next.Before(state.retryUntil) {
		next = state.retryUntil
	}
	if !state.attempt.IsZero() && next.Before(state.attempt.Add(m.options.minimum)) {
		next = state.attempt.Add(m.options.minimum)
	}
	return next
}

// 唯一调度循环为每个来源持有一个后台订阅，关闭自动刷新只移除此订阅。
func (m *Manager) schedule() {
	defer m.wg.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		now := m.options.now()
		var earliest time.Time
		for _, state := range m.sources {
			if state.interval == 0 || state.blocked || state.next.IsZero() {
				continue
			}
			if !now.Before(state.next) && state.automatic == nil {
				job, limited := m.subscribeLocked(state)
				if limited {
					state.next = m.nextTime(state, now, state.interval)
				} else {
					state.automatic, state.next = job, time.Time{}
				}
			}
			if !state.next.IsZero() && (earliest.IsZero() || state.next.Before(earliest)) {
				earliest = state.next
			}
		}
		wait := time.Hour
		if !earliest.IsZero() {
			wait = earliest.Sub(now)
			if wait < 0 {
				wait = 0
			}
		}
		m.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-timer.C:
		}
	}
}

// Close 可并发重复调用，取消并等待全部调度和出网函数退出。
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		return
	}
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
	close(m.closeDone)
}
