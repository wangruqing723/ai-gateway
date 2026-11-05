package chatgptauth

// 额度分层只读取内存中的用量快照，不发出网络请求。
const (
	QuotaNormal    = 0 // 剩余充足或数据未知
	QuotaLow       = 1 // 任一窗口剩余不高于 LowQuotaPercent
	QuotaExhausted = 2 // 任一窗口剩余为 0

	// LowQuotaPercent 与页面「额度偏低」提醒使用同一阈值。
	LowQuotaPercent = 10.0
)

// QuotaTier 按账户普通模型的 primary、secondary 窗口返回额度分层。快照不新鲜、
// 最近一次查询失败、窗口已到重置待复查或缺失时视为未知，归入 QuotaNormal。
func (m *Manager) QuotaTier(id string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item == nil || item.Usage == nil || item.Usage.Error != "" {
		return QuotaNormal
	}
	state := usageStateViewOf(item.Usage, m.now())
	if state.Freshness != "fresh" {
		return QuotaNormal
	}
	pending := make(map[string]bool, len(state.ResetPendingWindows))
	for _, name := range state.ResetPendingWindows {
		pending[name] = true
	}
	tier := QuotaNormal
	for _, window := range item.Usage.Windows {
		if (window.Name != "primary" && window.Name != "secondary") || pending[window.Name] {
			continue
		}
		switch {
		case window.RemainingPercent <= 0:
			return QuotaExhausted
		case window.RemainingPercent <= LowQuotaPercent:
			tier = QuotaLow
		}
	}
	return tier
}
