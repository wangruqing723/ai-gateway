package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrAccountPaused 表示账户已被用户暂停，不再准入新的请求。
	ErrAccountPaused = errors.New("ChatGPT OAuth 账户已暂停")
	// ErrAccountVersionConflict 表示调用方携带的账户控制版本已过期。
	ErrAccountVersionConflict = errors.New("ChatGPT OAuth 账户版本已变化")
	// ErrAccountStatePersist 表示暂停或恢复写盘失败，内存状态已回滚。
	ErrAccountStatePersist = errors.New("保存 ChatGPT OAuth 账户状态失败")
)

// pauseWatchers 记录正在等待准入的请求。账户被暂停时逐个取消，唤醒未准入的等待者。
// 已开始发送的请求不登记在这里，因此不会被暂停截断（D01）。
type pauseWatchers struct {
	mu      sync.Mutex
	nextID  uint64
	cancels map[uint64]context.CancelCauseFunc
}

// AdmissionWatch 是一次尝试从取令牌到获得执行槽之间的暂停监听。
// Context 只能用于等待（排队），不能用于令牌交换或上游请求本身。
type AdmissionWatch struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	item    *account
	id      uint64
	release sync.Once
	m       *Manager
}

// Context 返回会在账户暂停时以 ErrAccountPaused 为原因取消的派生 context。
func (w *AdmissionWatch) Context() context.Context { return w.ctx }

// Paused 判断监听期间账户是否被暂停。未取消即表示登记之后没有发生暂停。
func (w *AdmissionWatch) Paused() bool {
	return w != nil && errors.Is(context.Cause(w.ctx), ErrAccountPaused)
}

// CooledDown 判断监听期间账户是否进入共享额度冷却。
func (w *AdmissionWatch) CooledDown() bool {
	return w != nil && errors.Is(context.Cause(w.ctx), ErrAccountCooldown)
}

// Release 结束监听。尝试获得执行槽、开始发送或放弃时调用；可重复调用。
func (w *AdmissionWatch) Release() {
	if w == nil {
		return
	}
	w.release.Do(func() {
		if w.m != nil {
			w.m.activeWatches.Add(-1)
		}
		w.item.watchers.mu.Lock()
		delete(w.item.watchers.cancels, w.id)
		w.item.watchers.mu.Unlock()
		w.cancel(nil)
	})
}

// WatchAdmission 在尝试取令牌之前登记暂停与冷却监听。账户已暂停时直接返回 ErrAccountPaused，
// 冷却中返回 ErrAccountCooldown，账户不存在返回 ErrAccountNotFound。检查与登记在同一把读锁内完成，暂停写入持有写锁，
// 因此不会漏掉两者之间发生的暂停。
func (m *Manager) WatchAdmission(parent context.Context, id string) (*AdmissionWatch, error) {
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item == nil {
		return nil, ErrAccountNotFound
	}
	if item.Paused {
		return nil, ErrAccountPaused
	}
	if !m.cooldownUntilLocked(item).IsZero() {
		return nil, ErrAccountCooldown
	}
	ctx, cancel := context.WithCancelCause(parent)
	item.watchers.mu.Lock()
	if item.watchers.cancels == nil {
		item.watchers.cancels = make(map[uint64]context.CancelCauseFunc)
	}
	item.watchers.nextID++
	watchID := item.watchers.nextID
	item.watchers.cancels[watchID] = cancel
	item.watchers.mu.Unlock()
	m.startWatchPoll()
	return &AdmissionWatch{ctx: ctx, cancel: cancel, item: item, id: watchID, m: m}, nil
}

// startWatchPoll 登记一个等待者；有等待者期间每秒检查一次共享文件，
// 让其他实例的暂停在约 1 秒内唤醒本进程排队中的请求（DESIGN 7.1）。
func (m *Manager) startWatchPoll() {
	if m.activeWatches.Add(1) != 1 {
		return
	}
	m.watchPollMu.Lock()
	defer m.watchPollMu.Unlock()
	if m.watchPolling {
		return
	}
	m.watchPolling = true
	go m.watchPollLoop()
}

func (m *Manager) watchPollLoop() {
	interval := sharedSyncInterval
	switch {
	case m.syncIntervalOverride > 0:
		interval = m.syncIntervalOverride
	case m.syncIntervalOverride < 0:
		// 测试中每次都检查共享文件，轮询也相应加快。
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		m.watchPollMu.Lock()
		if m.activeWatches.Load() <= 0 {
			m.watchPolling = false
			m.watchPollMu.Unlock()
			return
		}
		m.watchPollMu.Unlock()
		m.syncShared()
	}
}

// IsPaused 只在账户存在且已暂停时返回 true；不存在的账户交给取令牌路径报错。
func (m *Manager) IsPaused(id string) bool {
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	return item != nil && item.Paused
}

// wakeAdmissionWatchersLocked 以 cause 唤醒该账户全部未准入的等待者。调用方持有 m.mu 写锁。
func wakeAdmissionWatchersLocked(item *account, cause error) {
	item.watchers.mu.Lock()
	cancels := item.watchers.cancels
	item.watchers.cancels = nil
	item.watchers.mu.Unlock()
	for _, cancel := range cancels {
		cancel(cause)
	}
}

// SetPaused 暂停或恢复账户。expectedVersion 是调用方读到的账户控制版本（不含引号），
// 不一致时返回 ErrAccountVersionConflict。目标状态与当前相同时不写盘、不推进版本。
// 返回最新视图、是否改变和新版本。不修改任何 Provider 配置。
func (m *Manager) SetPaused(id string, paused bool, expectedVersion string) (AccountView, bool, string, error) {
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return AccountView{}, false, "", fmt.Errorf("%w: %w", ErrAccountStatePersist, err)
	}
	defer unlock()
	item := m.accounts[id]
	if item == nil {
		return AccountView{}, false, "", ErrAccountNotFound
	}
	if expectedVersion != m.versionLocked() {
		return AccountView{}, false, "", ErrAccountVersionConflict
	}
	if item.Paused == paused {
		return m.viewOf(item), false, m.versionLocked(), nil
	}
	if err := m.setPausedLocked([]*account{item}, paused); err != nil {
		return AccountView{}, false, "", err
	}
	return m.viewOf(item), true, m.versionLocked(), nil
}

// setPausedLocked 一次写盘修改多个账户的暂停状态，失败时全部回滚。成功后推进版本，
// 暂停的账户在写盘成功后才唤醒等待者：写盘失败时账户仍是启用状态，不能误唤醒。
func (m *Manager) setPausedLocked(items []*account, paused bool) error {
	type undo struct {
		paused   bool
		pausedAt *time.Time
	}
	previous := make([]undo, len(items))
	now := m.now().UTC()
	for i, item := range items {
		previous[i] = undo{paused: item.Paused, pausedAt: item.PausedAt}
		item.Paused = paused
		if paused {
			at := now
			item.PausedAt = &at
		} else {
			item.PausedAt = nil
		}
	}
	if err := m.persistLocked(); err != nil {
		for i, item := range items {
			item.Paused, item.PausedAt = previous[i].paused, previous[i].pausedAt
		}
		return fmt.Errorf("%w: %v", ErrAccountStatePersist, err)
	}
	m.bumpVersionLocked()
	if paused {
		for _, item := range items {
			wakeAdmissionWatchersLocked(item, ErrAccountPaused)
		}
	}
	m.emitPausedLocked(items, paused)
	return nil
}
