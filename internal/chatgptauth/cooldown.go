package chatgptauth

import (
	"errors"
	"time"
)

// ErrAccountCooldown 表示账户处于共享额度冷却中，不再准入新的请求。
var ErrAccountCooldown = errors.New("ChatGPT OAuth 账户额度冷却中")

const (
	// EventCooldown 是账户进入共享额度冷却的事件；延长已有冷却不重复产生。
	EventCooldown = "account_cooldown"
	// EventCooldownCleared 是手动解除冷却的事件；到期结束不产生事件。
	EventCooldownCleared = "account_cooldown_cleared"

	// DefaultCooldown 是上游未给出重置时间时的冷却时长。
	DefaultCooldown = 60 * time.Second
	// MaxCooldown 是单次冷却的上限，防止异常的上游时间把账户锁死。
	MaxCooldown = 30 * 24 * time.Hour
)

// SetSharedCooldown 开关账户级共享冷却。关闭时清除全部冷却且不产生事件；
// 同时抬高全部账户的请求序号下限，关闭前发出的请求迟到的额度错误不会在重新开启后生效。
func (m *Manager) SetSharedCooldown(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sharedCooldown == enabled {
		return
	}
	m.sharedCooldown = enabled
	if enabled {
		return
	}
	floor := m.invocationSeq.Load()
	for _, item := range m.accounts {
		item.cooldownUntil = time.Time{}
		item.cooldownFloor = floor
	}
}

// SharedCooldownEnabled 报告共享冷却是否开启。
func (m *Manager) SharedCooldownEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sharedCooldown
}

// ReportUsageLimit 用一次真实上游尝试的额度耗尽结果让账户进入冷却，until 是上游要求的
// 最早可重试时间（零值时用默认时长）。只延长、不缩短已有冷却；凭据替换或手动解除前发出的
// 请求结果被丢弃。返回实际生效的冷却截止时间，未生效时为零值。
func (m *Manager) ReportUsageLimit(ticket *InvocationTicket, until time.Time) time.Time {
	if ticket == nil || ticket.item == nil {
		return time.Time{}
	}
	now := m.now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(DefaultCooldown)
	}
	if limit := now.Add(MaxCooldown); until.After(limit) {
		until = limit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := ticket.item
	if !m.sharedCooldown || m.accounts[item.ID] != item || ticket.seq <= item.cooldownFloor {
		return time.Time{}
	}
	active := item.cooldownUntil.After(now)
	if active && !until.After(item.cooldownUntil) {
		return item.cooldownUntil
	}
	item.cooldownUntil = until
	wakeAdmissionWatchersLocked(item, ErrAccountCooldown)
	if !active {
		m.emit(Event{Kind: EventCooldown, AccountIDs: []string{item.ID}, Account: accountLabelLocked(item), Provider: ticket.provider,
			Detail: "上游报告用量额度已耗尽，账户冷却至 " + until.UTC().Format(time.RFC3339) + "，同账户全部 Provider 暂停准入"})
	}
	return until
}

// CooldownUntil 返回账户仍未到期的冷却截止时间；共享冷却关闭、未冷却或账户不存在时为零值。
func (m *Manager) CooldownUntil(id string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item == nil {
		return time.Time{}
	}
	return m.cooldownUntilLocked(item)
}

// cooldownUntilLocked 调用方至少持有 m.mu 读锁。
func (m *Manager) cooldownUntilLocked(item *account) time.Time {
	if !m.sharedCooldown || !item.cooldownUntil.After(m.now()) {
		return time.Time{}
	}
	return item.cooldownUntil
}

// ClearCooldown 手动解除账户冷却并记录请求序号下限。返回最新视图和是否改变。
func (m *Manager) ClearCooldown(id string) (AccountView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.accounts[id]
	if item == nil {
		return AccountView{}, false, ErrAccountNotFound
	}
	changed := !m.cooldownUntilLocked(item).IsZero()
	item.cooldownUntil = time.Time{}
	item.cooldownFloor = m.invocationSeq.Load()
	if changed {
		m.emit(Event{Kind: EventCooldownCleared, AccountIDs: []string{item.ID}, Account: accountLabelLocked(item),
			Detail: "已手动解除额度冷却；解除前发出的请求迟到的额度错误不再生效"})
	}
	return m.viewOf(item), changed, nil
}
