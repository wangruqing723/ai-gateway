package chatgptauth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MaxUsageBatchAccounts 是一次批量用量刷新的账户上限（T17 确认值）。
const MaxUsageBatchAccounts = 50

// errUsageBatchBudget 标记批量刷新因总预算耗尽而取消，用于区分客户端主动断开。
var errUsageBatchBudget = errors.New("批量用量刷新总预算已耗尽")

// UsageBatchResult 是批量用量刷新的逐项结论，不含令牌或上游正文。
// Status 取 success、cached、failure、cancelled、not_found。
type UsageBatchResult struct {
	AccountID  string `json:"accountId"`
	Status     string `json:"status"`
	Category   string `json:"category,omitempty"`
	Message    string `json:"message"`
	DurationMs int64  `json:"durationMs"`
}

// RefreshUsageBatch 只刷新指定账户集合，并发上限与全账户刷新相同（3），不自动重试。
// 总预算用取消而不是截止时间实现：预算耗尽或客户端断开时，进行中的查询按取消处理，
// 不写失败快照、保留原有用量；尚未开始的账户直接记为 cancelled。单个账户失败不影响其他账户。
func (m *Manager) RefreshUsageBatch(parent context.Context, ids []string, force bool, budget time.Duration) []UsageBatchResult {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	timer := time.AfterFunc(budget, func() { cancel(errUsageBatchBudget) })
	defer timer.Stop()

	results := make([]UsageBatchResult, len(ids))
	semaphore := make(chan struct{}, usageWorkers)
	var workers sync.WaitGroup
	for index, id := range ids {
		m.mu.RLock()
		item := m.accounts[id]
		m.mu.RUnlock()
		if item == nil {
			results[index] = UsageBatchResult{AccountID: id, Status: "not_found", Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
			continue
		}
		workers.Add(1)
		go func(index int, item *account) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index] = usageBatchCancelled(ctx, item.ID, "未开始查询", 0)
				return
			}
			if ctx.Err() != nil {
				results[index] = usageBatchCancelled(ctx, item.ID, "未开始查询", 0)
				return
			}
			started := time.Now()
			outcome := m.refreshAccountUsage(ctx, item, force)
			elapsed := time.Since(started).Milliseconds()
			result := UsageBatchResult{AccountID: item.ID, Category: outcome.Category, Message: outcome.Message, DurationMs: elapsed}
			switch {
			case outcome.Category == "cancelled":
				result = usageBatchCancelled(ctx, item.ID, "查询未完成", elapsed)
			case outcome.Category == "not_found":
				result.Status = "not_found"
			case outcome.Category != "":
				result.Status = "failure"
			case outcome.Cached:
				result.Status = "cached"
			default:
				result.Status = "success"
			}
			results[index] = result
		}(index, item)
	}
	workers.Wait()
	return results
}

func usageBatchCancelled(ctx context.Context, id, stage string, elapsed int64) UsageBatchResult {
	reason := "请求已取消"
	if errors.Is(context.Cause(ctx), errUsageBatchBudget) {
		reason = "批量刷新总预算已耗尽"
	}
	return UsageBatchResult{AccountID: id, Status: "cancelled", Message: reason + "，" + stage + "，保留原有快照", DurationMs: elapsed}
}
