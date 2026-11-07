package queue

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// GroupLimits 是共享资源组（如同一 OAuth 账户）的上限，0 表示该项不限制。
type GroupLimits struct {
	MaxConcurrent int
	MaxPerSecond  int
}

// Enabled 报告是否有任一项上限生效。
func (l GroupLimits) Enabled() bool { return l.MaxConcurrent > 0 || l.MaxPerSecond > 0 }

// GroupStatus 是共享资源组的准入快照。
type GroupStatus struct {
	Running int `json:"running"`
	Queued  int `json:"queued"`
}

// groupQueue 是一个共享资源组。锁顺序固定为
// Manager.groupsMu -> groupQueue.mu -> Manager.mu -> providerQueue.mu。
type groupQueue struct {
	mu   sync.Mutex
	name string

	running      int
	waiters      list.List // *groupWaiter
	requestTimes []time.Time
	timer        *time.Timer
	timerAt      time.Time
}

type groupWaiter struct {
	provider      string
	groupOnly     bool
	maxConcurrent int
	maxPerSecond  int

	ready   chan struct{}
	state   waiterState
	element *list.Element
	q       *providerQueue // 准入成功后持有的 provider 队列
	err     error          // 准入被拒绝（provider 已删除）
}

// SetGroupLimits 更新所有共享资源组的上限，只影响之后的准入判断，不打断在途请求。
func (m *Manager) SetGroupLimits(limits GroupLimits) {
	m.groupsMu.Lock()
	m.groupLimits = limits
	m.groupsMu.Unlock()
	m.kickGroups()
}

// GroupLimitsSnapshot 返回当前共享资源组上限。
func (m *Manager) GroupLimitsSnapshot() GroupLimits {
	m.groupsMu.Lock()
	defer m.groupsMu.Unlock()
	return m.groupLimits
}

// GroupStatusOf 返回共享资源组的在途与排队数；组不存在时为零值。
func (m *Manager) GroupStatusOf(group string) GroupStatus {
	m.groupsMu.Lock()
	g, ok := m.groups[group]
	m.groupsMu.Unlock()
	if !ok {
		return GroupStatus{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return GroupStatus{Running: g.running, Queued: g.waiters.Len()}
}

// AcquireGroup 在共享资源组与 provider 同时有空位时一次授予执行槽，等待期间不占用任何一方。
// group 为空或组上限未启用时退化为普通 Acquire。组内按到达顺序挑选第一个 provider
// 也能准入的等待者；provider 已有普通等待者时不插队。等待预算与 Acquire 相同。
func (m *Manager) AcquireGroup(ctx context.Context, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int, group string) (release func(), waitMs int64, err error) {
	if group == "" || !m.GroupLimitsSnapshot().Enabled() {
		return m.Acquire(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs)
	}
	return m.acquireGroup(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs, group, false)
}

// AcquireGroupOnly 只占共享账户槽，不创建或占用 Provider 队列；未设组限制时仍跟踪在途数。
func (m *Manager) AcquireGroupOnly(ctx context.Context, group string, maxQueueWaitMs int) (func(), int64, error) {
	return m.acquireGroup(ctx, "", 0, 0, maxQueueWaitMs, group, true)
}

func (m *Manager) acquireGroup(ctx context.Context, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int, group string, groupOnly bool) (release func(), waitMs int64, err error) {
	start := time.Now()
	var deadline time.Time
	if maxQueueWaitMs > 0 {
		deadline = start.Add(time.Duration(maxQueueWaitMs) * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}

	w := &groupWaiter{
		provider:      name,
		groupOnly:     groupOnly,
		maxConcurrent: maxConcurrent,
		maxPerSecond:  maxPerSecond,
		ready:         make(chan struct{}),
		state:         waiterQueued,
	}
	m.groupsMu.Lock()
	if m.groups == nil {
		m.groups = make(map[string]*groupQueue)
	}
	g, ok := m.groups[group]
	if !ok {
		g = &groupQueue{name: group}
		m.groups[group] = g
	}
	g.mu.Lock()
	limits := m.groupLimits
	m.groupsMu.Unlock()

	m.mu.Lock()
	if m.groupWaiting == nil {
		m.groupWaiting = make(map[string]int)
	}
	if !groupOnly {
		m.groupWaiting[name]++
	}
	m.mu.Unlock()
	m.groupWaiters.Add(1)
	w.element = g.waiters.PushBack(w)
	m.admitGroupLocked(g, limits, time.Now())
	g.mu.Unlock()

	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		timeout = timer.C
		defer timer.Stop()
	}

	select {
	case <-w.ready:
	case <-ctx.Done():
		if m.cancelGroupWaiter(g, w) {
			return nil, time.Since(start).Milliseconds(), ctx.Err()
		}
		<-w.ready
	case <-timeout:
		if m.cancelGroupWaiter(g, w) {
			return nil, time.Since(start).Milliseconds(), ErrQueueTimeout
		}
		<-w.ready
	}
	// 准入与取消同时发生时，以已经完成的准入为准。
	if w.err != nil {
		m.cleanupGroup(g)
		return nil, time.Since(start).Milliseconds(), w.err
	}
	return m.newGroupRelease(g, w.q), time.Since(start).Milliseconds(), nil
}

func (m *Manager) newGroupRelease(g *groupQueue, q *providerQueue) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if q != nil {
				q.mu.Lock()
				q.running--
				q.admitLocked(time.Now())
				q.mu.Unlock()
				q.cleanupRetired()
			}

			g.mu.Lock()
			g.running--
			g.mu.Unlock()
			m.kickGroups()
			m.cleanupGroup(g)
		})
	}
}

// cancelGroupWaiter 只取消仍在排队的等待者；已准入或已拒绝时返回 false。
func (m *Manager) cancelGroupWaiter(g *groupQueue, w *groupWaiter) bool {
	g.mu.Lock()
	if w.state != waiterQueued || w.element == nil {
		g.mu.Unlock()
		return false
	}
	g.waiters.Remove(w.element)
	w.element = nil
	w.state = waiterCanceled
	m.mu.Lock()
	m.leaveGroupWaitingLocked(w.provider)
	m.mu.Unlock()
	g.mu.Unlock()
	m.cleanupGroup(g)
	return true
}

// admitGroupLocked 必须持有 g.mu。按到达顺序检查组内等待者：组上限满时停止；
// 某个等待者被它的 provider 挡住时跳过它，继续看后面的等待者。
func (m *Manager) admitGroupLocked(g *groupQueue, limits GroupLimits, now time.Time) {
	g.pruneLocked(now)
	if limits.MaxPerSecond <= 0 {
		g.requestTimes = nil
	}
	wake := time.Duration(-1)
	for e := g.waiters.Front(); e != nil; {
		if limits.MaxConcurrent > 0 && g.running >= limits.MaxConcurrent {
			break // 组内释放时会重新唤醒
		}
		if limits.MaxPerSecond > 0 && len(g.requestTimes) >= limits.MaxPerSecond {
			wake = minWake(wake, g.requestTimes[0].Add(time.Second).Sub(now))
			break
		}
		next := e.Next()
		w := e.Value.(*groupWaiter)
		q, retry, err := m.tryAdmitProvider(w, now)
		switch {
		case err != nil:
			g.waiters.Remove(e)
			w.element = nil
			w.state = waiterAdmitted
			w.err = err
			close(w.ready)
		case q != nil || w.groupOnly:
			g.waiters.Remove(e)
			w.element = nil
			w.state = waiterAdmitted
			w.q = q
			g.running++
			if limits.MaxPerSecond > 0 {
				g.requestTimes = append(g.requestTimes, now)
			}
			close(w.ready)
		case retry > 0:
			wake = minWake(wake, retry)
		}
		e = next
	}
	m.setGroupTimerLocked(g, wake, now)
}

// tryAdmitProvider 在 Manager -> provider 锁下尝试占用 provider 槽位。
// 返回 q 表示已占用；retry > 0 表示被 provider 速率挡住，需在该时长后重试；
// err 表示 provider 已删除，等待者应放弃。其余情况等 provider 释放后再唤醒。
func (m *Manager) tryAdmitProvider(w *groupWaiter, now time.Time) (*providerQueue, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.groupOnly {
		m.leaveGroupWaitingLocked("")
		return nil, 0, nil
	}
	maxConcurrent, maxPerSecond := w.maxConcurrent, w.maxPerSecond
	if m.activeProviders != nil {
		limits, active := m.activeProviders[w.provider]
		if !active {
			m.leaveGroupWaitingLocked(w.provider)
			return nil, 0, ErrProviderRemoved
		}
		maxConcurrent, maxPerSecond = limits.MaxConcurrent, limits.MaxPerSecond
	}
	q := m.ensureLocked(w.provider, maxConcurrent, maxPerSecond)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneRequestTimesLocked(now)
	if q.waiters.Len() > 0 || q.running >= q.maxConcurrent {
		return nil, 0, nil // 不插队普通等待者
	}
	if q.maxPerSecond > 0 && len(q.requestTimes) >= q.maxPerSecond {
		return nil, q.requestTimes[0].Add(time.Second).Sub(now), nil
	}
	q.running++
	if q.maxPerSecond > 0 {
		q.requestTimes = append(q.requestTimes, now)
	}
	m.leaveGroupWaitingLocked(w.provider)
	return q, 0, nil
}

func minWake(current, candidate time.Duration) time.Duration {
	if candidate < time.Millisecond {
		candidate = time.Millisecond
	}
	if current < 0 || candidate < current {
		return candidate
	}
	return current
}

// setGroupTimerLocked 必须持有 g.mu。wake < 0 时停止定时器；已有更早的定时器时保留。
func (m *Manager) setGroupTimerLocked(g *groupQueue, wake time.Duration, now time.Time) {
	if wake < 0 {
		if g.timer != nil {
			g.timer.Stop()
			g.timer = nil
		}
		return
	}
	at := now.Add(wake)
	if g.timer != nil && !g.timerAt.After(at) {
		return
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(wake, func() {
		limits := m.GroupLimitsSnapshot()
		g.mu.Lock()
		if g.timer != timer {
			g.mu.Unlock()
			return
		}
		g.timer = nil
		m.admitGroupLocked(g, limits, time.Now())
		g.mu.Unlock()
		m.cleanupGroup(g)
	})
	g.timer = timer
	g.timerAt = at
}

// kickGroups 在 provider 槽位、速率或配置变化后重新检查所有组的等待者。
// 调用方不得持有任何队列锁。
func (m *Manager) kickGroups() {
	if m == nil || m.groupWaiters.Load() == 0 {
		return
	}
	m.groupsMu.Lock()
	limits := m.groupLimits
	groups := make([]*groupQueue, 0, len(m.groups))
	for _, g := range m.groups {
		groups = append(groups, g)
	}
	m.groupsMu.Unlock()
	now := time.Now()
	for _, g := range groups {
		g.mu.Lock()
		if g.waiters.Len() > 0 {
			m.admitGroupLocked(g, limits, now)
		}
		g.mu.Unlock()
	}
}

// cleanupGroup 在组完全空闲且没有仍在速率窗口内的记录时回收它。
func (m *Manager) cleanupGroup(g *groupQueue) {
	m.groupsMu.Lock()
	defer m.groupsMu.Unlock()
	if current, ok := m.groups[g.name]; !ok || current != g {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(time.Now())
	if g.running == 0 && g.waiters.Len() == 0 && g.timer == nil && len(g.requestTimes) == 0 {
		delete(m.groups, g.name)
	}
}

func (g *groupQueue) pruneLocked(now time.Time) {
	cutoff := now.Add(-time.Second)
	firstKept := 0
	for firstKept < len(g.requestTimes) && !g.requestTimes[firstKept].After(cutoff) {
		firstKept++
	}
	if firstKept == len(g.requestTimes) {
		g.requestTimes = nil
	} else if firstKept > 0 {
		g.requestTimes = append(g.requestTimes[:0], g.requestTimes[firstKept:]...)
	}
}

// leaveGroupWaitingLocked 必须持有 m.mu。
func (m *Manager) leaveGroupWaitingLocked(provider string) {
	if m.groupWaiting[provider] <= 1 {
		delete(m.groupWaiting, provider)
	} else {
		m.groupWaiting[provider]--
	}
	m.groupWaiters.Add(-1)
}
