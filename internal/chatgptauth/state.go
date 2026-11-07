package chatgptauth

import (
	"fmt"
	"time"
)

// AuthorizationView 描述本地凭据条件。它只说明「能否拿到令牌」，不证明模型调用可用。
type AuthorizationView struct {
	// State 取值：valid（访问令牌未过期）、refreshable（访问令牌已过期或缺失，
	// 但有刷新凭据，使用时会尝试刷新）、reauthorization_required（需要重新授权）。
	State      string `json:"state"`
	Message    string `json:"message"`
	CanRefresh bool   `json:"canRefresh"`
	// LastRefreshFailedAt / LastRefreshError 只描述最近一次自动刷新失败；刷新成功后清空。
	LastRefreshFailedAt string `json:"lastRefreshFailedAt,omitempty"`
	LastRefreshError    string `json:"lastRefreshError,omitempty"`
	// CredentialsUnsaved 表示刷新得到的新凭据只在内存中，尚未成功写入账户文件。
	CredentialsUnsaved bool `json:"credentialsUnsaved,omitempty"`
}

// UsageStateView 描述用量查询结果与缓存新鲜度。额度数值仍在 AccountView.Usage 中。
type UsageStateView struct {
	// State 取值：unknown（从未查询）、ok（最近一次查询成功）、error（最近一次查询失败）。
	State string `json:"state"`
	// Freshness 取值：none（没有成功数据）、fresh（最近成功查询在缓存周期内）、stale（已超过缓存周期）。
	Freshness     string `json:"freshness"`
	Message       string `json:"message"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
	// ResetPendingWindows 列出重置时间已过、但还没有更新数据的额度窗口名称。
	// 重置时间到达只表示需要复核，不代表额度已经恢复。
	ResetPendingWindows []string `json:"resetPendingWindows,omitempty"`
}

// InvocationView 描述最近一次经网关发出的真实上游调用结果。只在当前进程内有效。
type InvocationView struct {
	// State 取值：unknown（没有真实调用记录）、success、failure。
	State      string `json:"state"`
	Message    string `json:"message"`
	Provider   string `json:"provider,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Model      string `json:"model,omitempty"`
	ObservedAt string `json:"observedAt,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// LastSuccessAt 只出现在账户整体视图：来自账户统计的最后一次真实调用成功时间，
	// 不含诊断调用；重新授权后保留，最近一次是失败时也不清空。Provider 维度的视图不填。
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
}

// InvocationTicket 在一次真实上游尝试开始时捕获账户身份和顺序号。
// 结束时凭它写回结果，避免换绑、删除后重建或重新授权后把结果记到错误的凭据上。
type InvocationTicket struct {
	item     *account
	seq      uint64
	provider string
	model    string
	// label 是开始时的账户显示名，供恢复事件使用，避免结束时再读受 m.mu 保护的字段。
	label  string
	source InvocationSource
}

// InvocationSource 描述真实上游尝试的来源，决定它进入哪一类账户统计。
type InvocationSource struct {
	// Diagnostic 表示管理诊断发起的调用：单独计数，不进入真实调用、成功率与延迟。
	Diagnostic bool
	// LocalRetry 表示同一候选内的本地重试；仍计入真实调用，另计重试次数。
	LocalRetry bool
	// AccountPreflight 表示独立账户预检，不归属任何 Provider。
	AccountPreflight bool
}

// InvocationResult 是一次真实上游尝试的安全结论，不含请求或响应正文。
type InvocationResult struct {
	Success    bool
	HTTPStatus int
	// Reason 是网关内部固定的失败类别，例如 429/ratelimit、transport_error。
	Reason string
	// HeaderLatency 是发出请求到收到上游响应头的耗时；HeadersReceived 为 false 时不进入延迟样本。
	HeaderLatency   time.Duration
	HeadersReceived bool
}

type invocationRecord struct {
	mode       string
	seq        uint64
	at         time.Time
	provider   string
	model      string
	success    bool
	httpStatus int
	reason     string
}

// BeginInvocation 在真实上游尝试开始前调用。账户不存在时返回 nil，之后的 Finish 为空操作。
func (m *Manager) BeginInvocation(accountID, provider, model string) *InvocationTicket {
	return m.BeginInvocationWith(accountID, provider, model, InvocationSource{})
}

// BeginInvocationWith 与 BeginInvocation 相同，额外记录调用来源。
func (m *Manager) BeginInvocationWith(accountID, provider, model string, source InvocationSource) *InvocationTicket {
	m.mu.RLock()
	item := m.accounts[accountID]
	label := ""
	if item != nil {
		label = accountLabelLocked(item)
	}
	m.mu.RUnlock()
	if item == nil {
		return nil
	}
	if !source.Diagnostic {
		// 诊断调用不让账户变成后台刷新的活跃账户。
		m.noteAccountUsed(item)
	}
	return &InvocationTicket{item: item, seq: m.invocationSeq.Add(1), provider: provider, model: model, label: label, source: source}
}

// FinishInvocation 写回真实上游尝试结果。只有比已记录结果更新的尝试才会覆盖，
// 旧请求的迟到成功不能盖掉更新的失败；凭据重新授权前开始的尝试结果直接丢弃。
func (m *Manager) FinishInvocation(ticket *InvocationTicket, result InvocationResult) {
	if ticket == nil || ticket.item == nil {
		return
	}
	at := m.now().UTC()
	record := &invocationRecord{
		seq: ticket.seq, at: at, provider: ticket.provider, model: ticket.model,
		success: result.Success, httpStatus: result.HTTPStatus, reason: result.Reason,
	}
	if ticket.source.AccountPreflight {
		record.mode = "account"
	} else if ticket.source.Diagnostic {
		record.mode = "provider"
	}
	item := ticket.item
	// 统计按尝试口径计数，与下面「最近调用」能否被覆盖无关：凭据替换前发出的尝试也真实发生过。
	m.recordStat(item, func(c *statCounters) {
		if ticket.source.Diagnostic {
			if result.Success {
				c.diagnosticSuccesses++
			} else {
				c.diagnosticFailures++
			}
			return
		}
		c.requests++
		if ticket.source.LocalRetry {
			c.localRetries++
		}
		if result.Success {
			c.successes++
		} else {
			c.failures++
		}
		if result.HTTPStatus == 429 {
			c.rateLimited++
		}
		if result.HeadersReceived {
			c.latency.Add(result.HeaderLatency.Milliseconds())
		}
	})
	if !ticket.source.Diagnostic {
		m.statsMu.Lock()
		if item.stats != nil {
			if result.Success {
				item.stats.lastSuccessAt = at
			} else {
				item.stats.lastFailureAt = at
			}
		}
		m.statsMu.Unlock()
	}
	recovered := false
	defer func() {
		// 在释放 invocationMu 之后再产生事件，事件接收者不会与调用记录锁嵌套。
		if recovered {
			m.emit(Event{Kind: EventRecovered, AccountIDs: []string{item.ID}, Account: ticket.label, Provider: ticket.provider,
				Detail: "凭据替换后的新调用已成功"})
		}
	}()
	m.invocationMu.Lock()
	defer m.invocationMu.Unlock()
	if ticket.seq <= item.invocationFloor {
		return
	}
	if result.Success && item.awaitingRecovery {
		// 只有凭据替换之后发出的尝试（序号大于 floor）才能作为恢复证据。
		item.awaitingRecovery = false
		recovered = true
	}
	if item.invocation == nil || item.invocation.seq < ticket.seq {
		item.invocation = record
	}
	if ticket.source.AccountPreflight {
		return
	}
	if item.providerInvocations == nil {
		item.providerInvocations = make(map[string]*invocationRecord)
	}
	if current := item.providerInvocations[ticket.provider]; current == nil || current.seq < ticket.seq {
		item.providerInvocations[ticket.provider] = record
	}
}

// ProviderInvocation 返回指定账户经指定 Provider 的最近一次真实调用结果。
func (m *Manager) ProviderInvocation(accountID, provider string) (InvocationView, bool) {
	m.mu.RLock()
	item := m.accounts[accountID]
	m.mu.RUnlock()
	if item == nil {
		return InvocationView{}, false
	}
	m.invocationMu.Lock()
	defer m.invocationMu.Unlock()
	return invocationViewOf(item.providerInvocations[provider]), true
}

// resetInvocationsLocked 在凭据被外部替换时调用，隔离旧凭据的在途结果。调用方持有 m.mu 写锁。
// 替换前账户处于授权或刷新异常时，清除告警并等待替换后的第一次成功调用作为恢复证据。
func (m *Manager) resetInvocationsLocked(item *account) {
	alerted := item.alertKind != alertNone
	item.alertKind = alertNone
	m.invocationMu.Lock()
	defer m.invocationMu.Unlock()
	item.invocationFloor = m.invocationSeq.Load()
	item.invocation = nil
	item.providerInvocations = nil
	item.awaitingRecovery = item.awaitingRecovery || alerted
}

func (m *Manager) invocationViewLocked(item *account) InvocationView {
	m.invocationMu.Lock()
	view := invocationViewOf(item.invocation)
	m.invocationMu.Unlock()
	// statsMu 是叶子锁，在 m.mu 之下单独获取，不与 invocationMu 嵌套。
	m.statsMu.Lock()
	if item.stats != nil && !item.stats.lastSuccessAt.IsZero() {
		view.LastSuccessAt = item.stats.lastSuccessAt.UTC().Format(time.RFC3339)
	}
	m.statsMu.Unlock()
	return view
}

func invocationViewOf(record *invocationRecord) InvocationView {
	if record == nil {
		return InvocationView{State: "unknown", Message: "尚无经网关发出的真实调用记录"}
	}
	view := InvocationView{
		Provider: record.provider, Model: record.model, Mode: record.mode,
		ObservedAt: record.at.Format(time.RFC3339), HTTPStatus: record.httpStatus, Reason: record.reason,
	}
	if record.success {
		view.State = "success"
		view.Message = "最近一次真实调用成功"
		return view
	}
	view.State = "failure"
	view.Message = invocationFailureMessage(record.httpStatus, record.reason)
	return view
}

func invocationFailureMessage(status int, reason string) string {
	switch {
	case reason == "conversion_error":
		return "最近一次真实调用中网关无法转换上游响应"
	case reason == "stream_interrupted":
		return "最近一次真实调用在上游开始响应后中断，可能是网络、代理或超时问题"
	case status == 401 || status == 403:
		return fmt.Sprintf("最近一次真实调用被上游拒绝（HTTP %d），请检查账户授权或权限", status)
	case status == 429:
		return "最近一次真实调用被上游限流（HTTP 429）"
	case status >= 500:
		return fmt.Sprintf("最近一次真实调用上游服务异常（HTTP %d）", status)
	case status > 0:
		return fmt.Sprintf("最近一次真实调用失败（HTTP %d），可能与模型或请求参数有关", status)
	default:
		return "最近一次真实调用没有得到上游响应，可能是网络、代理或超时问题"
	}
}

// authorizationViewOf 按凭据和已知刷新结果给出授权状态。调用方持有 m.mu 读锁。
func authorizationViewOf(item *account, now time.Time) AuthorizationView {
	view := AuthorizationView{CanRefresh: item.RefreshToken != "", CredentialsUnsaved: item.credentialsUnsaved}
	if failure := item.lastRefreshFailure; failure != nil {
		view.LastRefreshFailedAt = failure.At.UTC().Format(time.RFC3339)
		view.LastRefreshError = refreshFailureMessage(failure)
	}
	accessValid := item.AccessToken != "" && !item.ExpiresAt.IsZero() && item.ExpiresAt.After(now)
	switch {
	case item.NeedsReauth:
		view.State = "reauthorization_required"
		view.CanRefresh = false
		view.Message = "刷新凭据已失效，需要重新授权"
	case item.RefreshToken == "" && !accessValid:
		view.State = "reauthorization_required"
		view.Message = "访问令牌已过期且没有刷新凭据，需要重新授权"
	case item.RefreshToken == "":
		view.State = "valid"
		view.Message = "访问令牌有效；没有刷新凭据，到期后需要重新授权"
	case accessValid:
		view.State = "valid"
		view.Message = "访问令牌有效，到期前会在使用时自动刷新"
	default:
		view.State = "refreshable"
		view.Message = "访问令牌已过期，下次使用时会尝试用刷新凭据续期"
	}
	if view.State != "reauthorization_required" && item.lastRefreshFailure != nil {
		view.Message = view.LastRefreshError
	}
	return view
}

// usageStateViewOf 汇总用量查询结果和缓存新鲜度，不修改原快照。
func usageStateViewOf(usage *UsageSnapshot, now time.Time) UsageStateView {
	if usage == nil {
		return UsageStateView{State: "unknown", Freshness: "none", Message: "尚未查询用量"}
	}
	lastAttempt := usage.LastAttemptAt
	if lastAttempt == "" {
		lastAttempt = usage.LastCheckedAt
	}
	lastSuccess := usage.LastSuccessAt
	if lastSuccess == "" && usage.Error == "" {
		// 早期快照没有 lastSuccessAt；没有错误说明最近一次尝试就是成功的那次。
		lastSuccess = lastAttempt
	}
	view := UsageStateView{LastAttemptAt: lastAttempt, LastSuccessAt: lastSuccess}
	successAt, successErr := time.Parse(time.RFC3339, lastSuccess)
	switch {
	case lastSuccess == "" || successErr != nil:
		view.Freshness = "none"
	case now.Sub(successAt) < usageCacheTTL:
		view.Freshness = "fresh"
	default:
		view.Freshness = "stale"
	}
	switch {
	case lastAttempt == "" && lastSuccess == "":
		view.State = "unknown"
		view.Message = "尚未查询用量"
	case usage.Error != "" && view.Freshness == "none":
		view.State = "error"
		view.Message = "用量查询失败，尚无成功数据"
	case usage.Error != "":
		view.State = "error"
		view.Message = "最近一次用量查询失败，显示的是上次成功查询的数据"
	case view.Freshness == "stale":
		view.State = "ok"
		view.Message = "用量数据已超过缓存周期，可能已过时"
	default:
		view.State = "ok"
		view.Message = "用量数据来自最近一次成功查询"
	}
	if view.Freshness != "none" {
		for _, window := range usage.Windows {
			resetAt, err := time.Parse(time.RFC3339, window.ResetAt)
			if err == nil && !resetAt.After(now) && !resetAt.Before(successAt) {
				view.ResetPendingWindows = append(view.ResetPendingWindows, window.Name)
			}
		}
	}
	return view
}
