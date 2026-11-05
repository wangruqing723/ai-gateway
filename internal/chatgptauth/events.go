package chatgptauth

import (
	"fmt"
	"strings"
)

// 账户事件种类。只在状态转移时产生，同一异常持续期间不重复产生。
const (
	EventReauthRequired     = "account_reauth_required"
	EventRefreshFailed      = "account_refresh_failed"
	EventRefreshRecovered   = "account_refresh_recovered"
	EventCredentialsUpdated = "account_credentials_updated"
	EventPaused             = "account_paused"
	EventResumed            = "account_resumed"
	EventRecovered          = "account_recovered"
)

// 账户告警状态。alertReauth 比 alertRefresh 严重，不会被降级覆盖。
const (
	alertNone    = ""
	alertRefresh = "refresh_failed"
	alertReauth  = "reauthorization_required"
)

// Event 是账户状态变化的安全描述，只含固定文案、账户 ID、邮箱与 Provider 名称。
type Event struct {
	Kind       string
	AccountIDs []string
	Account    string
	Provider   string
	Detail     string
}

// SetEventHandler 设置账户事件接收者。handler 会在持有账户锁时被同步调用，
// 只能做非阻塞的记录（例如写入有界事件流），不能回调 Manager。
func (m *Manager) SetEventHandler(handler func(Event)) {
	if handler == nil {
		m.eventHandler.Store(nil)
		return
	}
	m.eventHandler.Store(&handler)
}

func (m *Manager) emit(event Event) {
	if handler := m.eventHandler.Load(); handler != nil {
		(*handler)(event)
	}
}

// accountLabelLocked 返回账户显示名。调用方至少持有 m.mu 读锁。
func accountLabelLocked(item *account) string {
	if item.Email != "" {
		return item.Email
	}
	return item.ID
}

// accountsEventLocked 把一组账户合并为一条事件的身份与显示名。调用方至少持有 m.mu 读锁。
func accountsEventLocked(items []*account) ([]string, string) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(items) == 1 {
		return ids, accountLabelLocked(items[0])
	}
	return ids, fmt.Sprintf("%d 个账户", len(items))
}

// accountsDetailSuffix 列出批量事件中的前几个账户，避免事件详情过长。调用方至少持有 m.mu 读锁。
func accountsDetailSuffix(items []*account) string {
	if len(items) <= 1 {
		return ""
	}
	const shown = 3
	labels := make([]string, 0, shown)
	for i, item := range items {
		if i == shown {
			break
		}
		labels = append(labels, accountLabelLocked(item))
	}
	suffix := "：" + strings.Join(labels, "、")
	if len(items) > shown {
		suffix += fmt.Sprintf(" 等 %d 个", len(items))
	}
	return suffix
}

// raiseAlertLocked 在账户进入授权或刷新异常时调用，只在状态变化时产生事件。
// 调用方持有 m.mu 写锁。
func (m *Manager) raiseAlertLocked(item *account, kind string) {
	if item.alertKind == kind || item.alertKind == alertReauth {
		return
	}
	item.alertKind = kind
	// 新异常出现后，之前等待的恢复证据作废：恢复要以这次异常之后的新调用为准。
	m.invocationMu.Lock()
	item.awaitingRecovery = false
	m.invocationMu.Unlock()
	if kind == alertReauth {
		m.emit(Event{Kind: EventReauthRequired, AccountIDs: []string{item.ID}, Account: accountLabelLocked(item),
			Detail: "刷新凭据已被 ChatGPT 拒绝，需要重新授权"})
		return
	}
	m.emit(Event{Kind: EventRefreshFailed, AccountIDs: []string{item.ID}, Account: accountLabelLocked(item),
		Detail: "自动刷新访问令牌失败，下次使用时会重试"})
}

// clearRefreshAlertLocked 在刷新交换成功后调用：只说明刷新链路恢复，不代表模型调用恢复。
// 调用方持有 m.mu 写锁。
func (m *Manager) clearRefreshAlertLocked(item *account) {
	if item.alertKind != alertRefresh {
		return
	}
	item.alertKind = alertNone
	m.emit(Event{Kind: EventRefreshRecovered, AccountIDs: []string{item.ID}, Account: accountLabelLocked(item),
		Detail: "自动刷新已恢复；模型调用是否正常以之后的真实调用为准"})
}

// emitPausedLocked 为一次暂停或恢复操作产生一条事件，批量操作合并。调用方持有 m.mu 写锁。
func (m *Manager) emitPausedLocked(items []*account, paused bool) {
	if len(items) == 0 {
		return
	}
	ids, label := accountsEventLocked(items)
	event := Event{Kind: EventResumed, AccountIDs: ids, Account: label, Detail: "账户已恢复，原已停用的 Provider 保持停用"}
	if paused {
		event.Kind = EventPaused
		event.Detail = "账户已暂停，新请求不再准入，绑定与 Provider 启停设置不变"
	}
	event.Detail += accountsDetailSuffix(items)
	m.emit(event)
}

// emitCredentialsUpdatedLocked 为一次显式凭据替换产生一条事件，批量导入合并。调用方持有 m.mu 写锁。
func (m *Manager) emitCredentialsUpdatedLocked(items []*account, source string) {
	if len(items) == 0 {
		return
	}
	ids, label := accountsEventLocked(items)
	m.emit(Event{Kind: EventCredentialsUpdated, AccountIDs: ids, Account: label,
		Detail: source + "已替换 OAuth 凭据；调用是否恢复以之后的真实调用为准" + accountsDetailSuffix(items)})
}
