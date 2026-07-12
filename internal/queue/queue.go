// Package queue 对齐 Node 版 lib/queue.js：per-provider 并发控制 + 每秒速率限制 + 队列等待超时。
package queue

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueTimeout 队列等待超时
var ErrQueueTimeout = errors.New("队列等待超时，请稍后重试")

// ErrProviderRemoved provider 已从当前配置删除
var ErrProviderRemoved = errors.New("provider 已删除")

const defaultMaxConcurrent = 5

// Limits 是 provider 可动态更新的队列限制。
type Limits struct {
	MaxConcurrent int
	MaxPerSecond  int
}

type waiterState uint8

const (
	waiterQueued waiterState = iota
	waiterAdmitted
	waiterCanceled
)

type waiter struct {
	ready   chan struct{}
	state   waiterState
	element *list.Element
}

// providerQueue 单个 provider 的队列状态。
// 所有字段都由 mu 保护；waiters 中只有尚未 admission 的请求。
type providerQueue struct {
	mu   sync.Mutex
	m    *Manager
	name string

	maxConcurrent int
	maxPerSecond  int
	running       int
	waiters       list.List
	requestTimes  []time.Time
	rateTimer     *time.Timer
	retired       bool
}

// Manager 管理所有 provider 的队列
type Manager struct {
	mu              sync.Mutex
	queues          map[string]*providerQueue
	activeProviders map[string]Limits
}

// NewManager 创建队列管理器
func NewManager() *Manager {
	return &Manager{queues: make(map[string]*providerQueue)}
}

// Status 队列状态快照，用于 /health
type Status struct {
	Running       int `json:"running"`
	Queued        int `json:"queued"`
	MaxConcurrent int `json:"maxConcurrent"`
	MaxPerSecond  int `json:"maxPerSecond"`
}

func normalizeMaxConcurrent(value int) int {
	if value <= 0 {
		return defaultMaxConcurrent
	}
	return value
}

func (m *Manager) ensure(name string, maxConcurrent, maxPerSecond int) *providerQueue {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLocked(name, maxConcurrent, maxPerSecond)
}

func (m *Manager) ensureLocked(name string, maxConcurrent, maxPerSecond int) *providerQueue {
	q, ok := m.queues[name]
	if !ok {
		q = &providerQueue{
			m:             m,
			name:          name,
			maxConcurrent: normalizeMaxConcurrent(maxConcurrent),
			maxPerSecond:  maxPerSecond,
		}
		m.queues[name] = q
	}
	return q
}

// lockQueueForAcquire 在固定的 Manager -> provider 锁顺序下完成 active 校验和取队列。
// 返回成功时 providerQueue.mu 仍由调用方持有，避免 Reconcile 在入队前回收该对象。
func (m *Manager) lockQueueForAcquire(ctx context.Context, name string, maxConcurrent, maxPerSecond int, deadline time.Time) (*providerQueue, error) {
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		m.mu.Unlock()
		return nil, ErrQueueTimeout
	}
	if m.activeProviders != nil {
		limits, active := m.activeProviders[name]
		if !active {
			m.mu.Unlock()
			return nil, ErrProviderRemoved
		}
		maxConcurrent = limits.MaxConcurrent
		maxPerSecond = limits.MaxPerSecond
	}

	q := m.ensureLocked(name, maxConcurrent, maxPerSecond)
	q.mu.Lock()
	m.mu.Unlock()
	return q, nil
}

// Acquire 等待 FIFO admission。并发和速率限制共享同一个 maxQueueWait deadline；
// ctx 取消或超时时，请求会从 waiters 中移除，且不会增加 running。
func (m *Manager) Acquire(ctx context.Context, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int) (release func(), waitMs int64, err error) {
	start := time.Now()
	var deadline time.Time
	if maxQueueWaitMs > 0 {
		deadline = start.Add(time.Duration(maxQueueWaitMs) * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}

	q, err := m.lockQueueForAcquire(ctx, name, maxConcurrent, maxPerSecond, deadline)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}
	w := &waiter{ready: make(chan struct{}), state: waiterQueued}

	now := time.Now()
	if err := ctx.Err(); err != nil {
		q.mu.Unlock()
		return nil, time.Since(start).Milliseconds(), err
	}
	if !deadline.IsZero() && !now.Before(deadline) {
		q.mu.Unlock()
		return nil, time.Since(start).Milliseconds(), ErrQueueTimeout
	}
	w.element = q.waiters.PushBack(w)
	q.admitLocked(now)
	q.mu.Unlock()

	var timeout <-chan time.Time
	var timer *time.Timer
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		timeout = timer.C
		defer timer.Stop()
	}

	for {
		select {
		case <-w.ready:
			return q.newRelease(), time.Since(start).Milliseconds(), nil
		case <-ctx.Done():
			if q.cancel(w) {
				return nil, time.Since(start).Milliseconds(), ctx.Err()
			}
			// admission 与取消同时发生时，以已经完成的 admission 为准。
			return q.newRelease(), time.Since(start).Milliseconds(), nil
		case <-timeout:
			if q.cancel(w) {
				return nil, time.Since(start).Milliseconds(), ErrQueueTimeout
			}
			// admission 与 deadline 同时发生时，以已经完成的 admission 为准。
			return q.newRelease(), time.Since(start).Milliseconds(), nil
		}
	}
}

func (q *providerQueue) newRelease() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			q.running--
			q.admitLocked(time.Now())
			q.mu.Unlock()
			q.cleanupRetired()
		})
	}
}

// cancel 只取消仍在队列中的 waiter。若 waiter 已 admission，调用方必须返回 release。
func (q *providerQueue) cancel(target *waiter) bool {
	q.mu.Lock()

	if target.state != waiterQueued {
		q.mu.Unlock()
		return false
	}
	if target.element == nil {
		q.mu.Unlock()
		return false
	}
	q.waiters.Remove(target.element)
	target.element = nil
	target.state = waiterCanceled
	q.admitLocked(time.Now())
	q.mu.Unlock()
	q.cleanupRetired()
	return true
}

// admitLocked 仅从队首 admission，确保并发和限速等待都遵守 FIFO。
func (q *providerQueue) admitLocked(now time.Time) {
	q.pruneRequestTimesLocked(now)

	for q.waiters.Len() > 0 {
		if q.running >= q.maxConcurrent {
			q.stopRateTimerLocked()
			return
		}
		if q.maxPerSecond > 0 && len(q.requestTimes) >= q.maxPerSecond {
			q.scheduleRateWakeLocked(q.requestTimes[0].Add(time.Second).Sub(now))
			return
		}

		q.stopRateTimerLocked()
		front := q.waiters.Front()
		w := front.Value.(*waiter)
		q.waiters.Remove(front)
		w.element = nil
		w.state = waiterAdmitted
		q.running++
		if q.maxPerSecond > 0 {
			q.requestTimes = append(q.requestTimes, now)
		}
		close(w.ready)
	}

	q.stopRateTimerLocked()
}

func (q *providerQueue) pruneRequestTimesLocked(now time.Time) {
	if q.maxPerSecond <= 0 {
		q.requestTimes = nil
		return
	}

	cutoff := now.Add(-time.Second)
	firstKept := 0
	for firstKept < len(q.requestTimes) && !q.requestTimes[firstKept].After(cutoff) {
		firstKept++
	}
	if firstKept == len(q.requestTimes) {
		q.requestTimes = nil
	} else if firstKept > 0 {
		copy(q.requestTimes, q.requestTimes[firstKept:])
		q.requestTimes = q.requestTimes[:len(q.requestTimes)-firstKept]
	}
}

func (q *providerQueue) scheduleRateWakeLocked(delay time.Duration) {
	if q.rateTimer != nil {
		return
	}
	if delay < 0 {
		delay = 0
	}

	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		q.mu.Lock()
		if q.rateTimer != timer {
			q.mu.Unlock()
			return
		}
		q.rateTimer = nil
		q.admitLocked(time.Now())
		q.mu.Unlock()
		q.cleanupRetired()
	})
	q.rateTimer = timer
}

func (q *providerQueue) stopRateTimerLocked() {
	if q.rateTimer == nil {
		return
	}
	q.rateTimer.Stop()
	q.rateTimer = nil
}

func (q *providerQueue) drainedLocked() bool {
	return q.running == 0 && q.waiters.Len() == 0 && q.rateTimer == nil
}

// cleanupRetired 必须在未持有 q.mu 时调用，内部始终按 Manager -> provider 加锁。
func (q *providerQueue) cleanupRetired() {
	if q.m == nil {
		return
	}
	q.m.mu.Lock()
	current, ok := q.m.queues[q.name]
	if !ok || current != q {
		q.m.mu.Unlock()
		return
	}
	q.mu.Lock()
	if q.retired && q.drainedLocked() {
		delete(q.m.queues, q.name)
	}
	q.mu.Unlock()
	q.m.mu.Unlock()
}

// StatusOf 返回某 provider 的队列状态快照
func (m *Manager) StatusOf(name string, maxConcurrent, maxPerSecond int) Status {
	m.mu.Lock()
	q, ok := m.queues[name]
	m.mu.Unlock()
	if !ok {
		return Status{MaxConcurrent: maxConcurrent, MaxPerSecond: maxPerSecond}
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return Status{
		Running:       q.running,
		Queued:        q.waiters.Len(),
		MaxConcurrent: q.maxConcurrent,
		MaxPerSecond:  q.maxPerSecond,
	}
}

// UpdateProvider 动态更新 provider 的并发与速率限制。
func (m *Manager) UpdateProvider(name string, maxConcurrent, maxPerSecond int) {
	m.mu.Lock()
	limits := Limits{MaxConcurrent: maxConcurrent, MaxPerSecond: maxPerSecond}
	if m.activeProviders != nil {
		if _, active := m.activeProviders[name]; !active {
			m.mu.Unlock()
			return
		}
		m.activeProviders[name] = limits
	}
	q, ok := m.queues[name]
	if ok {
		q.mu.Lock()
		q.retired = false
		q.updateLimitsLocked(limits)
		q.mu.Unlock()
	}
	m.mu.Unlock()
}

// Reconcile 记录当前 active provider 集。删除项拒绝新的 Acquire；有在途状态时
// 暂时保留原队列，排空后按 map 指针身份回收；排空前重新加入则原位复用。
func (m *Manager) Reconcile(limits map[string]Limits) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.activeProviders = make(map[string]Limits, len(limits))
	for name, providerLimits := range limits {
		m.activeProviders[name] = providerLimits
	}

	for name, q := range m.queues {
		providerLimits, ok := limits[name]
		q.mu.Lock()
		q.retired = !ok
		if ok {
			q.updateLimitsLocked(providerLimits)
		} else if q.drainedLocked() {
			delete(m.queues, name)
		}
		q.mu.Unlock()
	}
}

func (q *providerQueue) updateLimitsLocked(limits Limits) {
	q.maxConcurrent = normalizeMaxConcurrent(limits.MaxConcurrent)
	q.maxPerSecond = limits.MaxPerSecond
	q.stopRateTimerLocked()
	q.admitLocked(time.Now())
}
