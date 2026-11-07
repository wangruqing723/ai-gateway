package providerbalance

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type managerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *managerClock) read() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *managerClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func testBalanceManager(t *testing.T) (*Manager, *managerClock) {
	t.Helper()
	clock := &managerClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newManager(managerOptions{now: clock.read, ttl: 5 * time.Minute, minimum: 30 * time.Second, minInterval: 5 * time.Minute, maxInterval: 24 * time.Hour, budget: time.Second, jitter: func(time.Duration) time.Duration { return 0 }})
	t.Cleanup(m.Close)
	return m, clock
}
func awaitManager(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("等待余额管理器状态超时")
		default:
			runtime.Gosched()
		}
	}
}
func receiveManager[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("等待余额任务超时")
		var zero T
		return zero
	}
}
func startManagerRefresh(m *Manager, ctx context.Context, provider string) <-chan Result {
	ch := make(chan Result, 1)
	go func() { ch <- m.Refresh(ctx, provider) }()
	return ch
}
func managerSnapshot() *Snapshot {
	return &Snapshot{QueriedAt: "2030-01-01T00:00:00Z", Warnings: []string{"原始单位"}}
}

func TestManagerViewReadOnlyAndClone(t *testing.T) {
	m, clock := testBalanceManager(t)
	var calls atomic.Int32
	original := managerSnapshot()
	source := Source{Key: "digest", Type: "newapi", Mode: "account", Fetch: func(context.Context) (*Snapshot, *QueryError) { calls.Add(1); return original, nil }}
	m.Reconcile(map[string]Source{"a": source})
	if v := m.View("a"); v.State != "not_queried" || v.Snapshot != nil {
		t.Fatalf("初态错误：%+v", v)
	}
	if m.View("missing").State != "not_configured" || calls.Load() != 0 {
		t.Fatal("只读查询出网或缺省状态错误")
	}
	if r := m.Refresh(context.Background(), "a"); r.Status != "success" {
		t.Fatalf("刷新失败：%+v", r)
	}
	original.Warnings[0] = "外部修改"
	v := m.View("a")
	if v.Snapshot.Warnings[0] != "原始单位" {
		t.Fatal("写入快照未克隆")
	}
	v.Snapshot.Warnings[0] = "界面修改"
	if m.View("a").Snapshot.Warnings[0] != "原始单位" {
		t.Fatal("读取快照未克隆")
	}
	if r := m.Refresh(context.Background(), "a"); r.Status != "cached" || calls.Load() != 1 {
		t.Fatal("未遵守最短刷新间隔")
	}
	clock.advance(5 * time.Minute)
	if m.View("a").Freshness != "stale" {
		t.Fatal("五分钟边界未过期")
	}
}

func TestManagerFailurePreservesSnapshotAndRetryAfter(t *testing.T) {
	m, clock := testBalanceManager(t)
	var calls atomic.Int32
	source := Source{Key: "digest", Fetch: func(context.Context) (*Snapshot, *QueryError) {
		if calls.Add(1) == 1 {
			return managerSnapshot(), nil
		}
		return nil, &QueryError{Code: "rate_limited", Message: "限流", HTTPStatus: 429, RetryAfter: 48 * time.Hour}
	}}
	m.Reconcile(map[string]Source{"a": source})
	first := m.Refresh(context.Background(), "a")
	clock.advance(31 * time.Second)
	r := m.Refresh(context.Background(), "a")
	if r.Status != "failed" || r.Balance.State != "failed" || r.Balance.Snapshot == nil || *r.Balance.LastSuccessAt != *first.Balance.LastSuccessAt {
		t.Fatalf("失败覆盖旧快照：%+v", r)
	}
	if r.Balance.RetryAfterSeconds == nil || *r.Balance.RetryAfterSeconds != 172800 {
		t.Fatal("长 Retry-After 被裁短")
	}
	r.Balance.Error.Code = "修改"
	clock.advance(time.Hour)
	if r = m.Refresh(context.Background(), "a"); r.Status != "cached" || calls.Load() != 2 || r.Balance.Error.Code != "rate_limited" {
		t.Fatal("手动刷新绕过 Retry-After 或错误指针未隔离")
	}
}

func TestManagerSharedCancellation(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	source := Source{Key: "shared", Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		calls.Add(1)
		started <- ctx
		select {
		case <-release:
			return managerSnapshot(), nil
		case <-ctx.Done():
			return nil, balanceManagerError("query_cancelled")
		}
	}}
	m.Reconcile(map[string]Source{"a": source, "b": source})
	ctx, cancel := context.WithCancel(context.Background())
	first := startManagerRefresh(m, ctx, "a")
	jobContext := receiveManager(t, started)
	second := startManagerRefresh(m, context.Background(), "b")
	awaitManager(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.sources["shared"].job.refs == 2 })
	cancel()
	if r := receiveManager(t, first); r.Status != "cancelled" {
		t.Fatal("订阅取消未返回取消")
	}
	if jobContext.Err() != nil {
		t.Fatal("首个订阅取消影响其他等待者")
	}
	close(release)
	if r := receiveManager(t, second); r.Status != "success" || calls.Load() != 1 {
		t.Fatalf("同源未合并：%+v", r)
	}
	if m.View("a").Snapshot == nil {
		t.Fatal("共享来源快照未发布")
	}
}

func TestManagerLastSubscriberCancelsWithoutFailure(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan context.Context, 1)
	m.Reconcile(map[string]Source{"a": {Key: "one", Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		started <- ctx
		<-ctx.Done()
		return nil, balanceManagerError("query_cancelled")
	}}})
	ctx, cancel := context.WithCancel(context.Background())
	result := startManagerRefresh(m, ctx, "a")
	jobContext := receiveManager(t, started)
	cancel()
	receiveManager(t, result)
	awaitManager(t, func() bool { return jobContext.Err() != nil && !m.View("a").Refreshing })
	if v := m.View("a"); v.Error != nil || v.State != "not_queried" {
		t.Fatalf("取消写入失败状态：%+v", v)
	}
}

func TestManagerReconcileSharedIdentity(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	old := Source{Key: "old", Fetch: func(context.Context) (*Snapshot, *QueryError) {
		started <- struct{}{}
		<-release
		return managerSnapshot(), nil
	}}
	m.Reconcile(map[string]Source{"a": old, "b": old})
	first := startManagerRefresh(m, context.Background(), "a")
	receiveManager(t, started)
	second := startManagerRefresh(m, context.Background(), "b")
	awaitManager(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.sources["old"].job.refs == 2 })
	m.Reconcile(map[string]Source{"a": {Key: "new"}, "b": old})
	r := receiveManager(t, first)
	if r.Balance.Error == nil || r.Balance.Error.Code != "configuration_changed" {
		t.Fatalf("旧订阅未返回配置变化：%+v", r)
	}
	close(release)
	if receiveManager(t, second).Status != "success" {
		t.Fatal("其他提供商仍引用旧来源时任务被取消")
	}
	if m.View("a").Snapshot != nil || m.View("b").Snapshot == nil {
		t.Fatal("旧结果串入新身份")
	}
}

func TestManagerRemovedSourceCancelsAndCannotPublish(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan context.Context, 1)
	m.Reconcile(map[string]Source{"a": {Key: "old", Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		started <- ctx
		<-ctx.Done()
		return managerSnapshot(), nil
	}}})
	r := startManagerRefresh(m, context.Background(), "a")
	ctx := receiveManager(t, started)
	m.Reconcile(map[string]Source{"a": {Key: "new"}})
	result := receiveManager(t, r)
	if result.Balance.Error == nil || result.Balance.Error.Code != "configuration_changed" {
		t.Fatal("移除来源未报告配置变化")
	}
	awaitManager(t, func() bool { return ctx.Err() != nil })
	if m.View("a").Snapshot != nil {
		t.Fatal("忽略取消的旧结果被发布")
	}
}

func TestManagerGlobalConcurrencyAndQueuedCancellation(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan string, 3)
	var calls atomic.Int32
	input := make(map[string]Source)
	for _, name := range []string{"a", "b", "c"} {
		input[name] = Source{Key: name, Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
			calls.Add(1)
			started <- name
			<-ctx.Done()
			return nil, balanceManagerError("query_cancelled")
		}}
	}
	m.Reconcile(input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startManagerRefresh(m, ctx, "a")
	b := startManagerRefresh(m, ctx, "b")
	receiveManager(t, started)
	receiveManager(t, started)
	queuedCtx, queuedCancel := context.WithCancel(context.Background())
	c := startManagerRefresh(m, queuedCtx, "c")
	awaitManager(t, func() bool { return m.View("c").Refreshing })
	if calls.Load() != 2 {
		t.Fatal("超过全局并发二")
	}
	queuedCancel()
	if receiveManager(t, c).Status != "cancelled" {
		t.Fatal("槽位等待不能取消")
	}
	cancel()
	receiveManager(t, a)
	receiveManager(t, b)
	if calls.Load() != 2 {
		t.Fatal("取消后的排队项仍出网")
	}
}

func TestManagerAutomaticOptInSharedIntervalAndDisable(t *testing.T) {
	m, clock := testBalanceManager(t)
	started := make(chan context.Context, 2)
	source := Source{Key: "auto", Enabled: true, Interval: time.Minute, Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		started <- ctx
		<-ctx.Done()
		return nil, balanceManagerError("query_cancelled")
	}}
	long := source
	long.Interval = time.Hour
	m.Reconcile(map[string]Source{"a": source, "b": long})
	m.mu.Lock()
	interval := m.sources["auto"].interval
	m.mu.Unlock()
	if interval != 5*time.Minute {
		t.Fatal("共享周期未选择启用最小值或未限制最短周期")
	}
	clock.advance(5 * time.Minute)
	m.signal()
	jobCtx := receiveManager(t, started)
	manual := startManagerRefresh(m, context.Background(), "b")
	awaitManager(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.sources["auto"].job.refs == 2 })
	source.Enabled = false
	long.Enabled = false
	m.Reconcile(map[string]Source{"a": source, "b": long})
	if jobCtx.Err() != nil {
		t.Fatal("关闭自动刷新取消了手动订阅")
	}
	m.Close()
	if receiveManager(t, manual).Status != "cancelled" || jobCtx.Err() == nil {
		t.Fatal("关闭服务未取消手动在途查询")
	}
}

func TestManagerAutomaticDefaultOffAndClose(t *testing.T) {
	m, clock := testBalanceManager(t)
	var calls atomic.Int32
	m.Reconcile(map[string]Source{"a": {Key: "one", Enabled: true, Fetch: func(context.Context) (*Snapshot, *QueryError) { calls.Add(1); return managerSnapshot(), nil }}})
	clock.advance(48 * time.Hour)
	m.signal()
	if v := m.View("a"); v.NextRefreshAt != nil || calls.Load() != 0 {
		t.Fatal("默认自动刷新未关闭")
	}
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() { defer group.Done(); m.Close() }()
	}
	group.Wait()
	if m.Refresh(context.Background(), "a").Status != "cancelled" {
		t.Fatal("关闭后仍受理查询")
	}
}

func TestManagerAutomaticBackoffAuthPauseManualRecovery(t *testing.T) {
	m, clock := testBalanceManager(t)
	var calls atomic.Int32
	source := Source{Key: "one", Enabled: true, Interval: 5 * time.Minute, Fetch: func(context.Context) (*Snapshot, *QueryError) {
		switch calls.Add(1) {
		case 1:
			return nil, &QueryError{Code: "upstream_unreachable", Message: "不可达", HTTPStatus: 502}
		case 2:
			return nil, &QueryError{Code: "credential_rejected", Message: "被拒绝", HTTPStatus: 401}
		default:
			return managerSnapshot(), nil
		}
	}}
	m.Reconcile(map[string]Source{"a": source})
	clock.advance(5 * time.Minute)
	m.signal()
	awaitManager(t, func() bool { v := m.View("a"); return v.Error != nil && v.NextRefreshAt != nil })
	v := m.View("a")
	next, err := time.Parse(time.RFC3339Nano, *v.NextRefreshAt)
	if err != nil || next.Sub(clock.read()) != 10*time.Minute {
		t.Fatal("失败未倍增退避")
	}
	clock.advance(10 * time.Minute)
	m.signal()
	awaitManager(t, func() bool {
		v := m.View("a")
		return v.Error != nil && v.Error.Code == "credential_rejected" && !v.Refreshing
	})
	if m.View("a").NextRefreshAt != nil {
		t.Fatal("拒绝凭据后仍调度自动刷新")
	}
	clock.advance(time.Hour)
	m.signal()
	if calls.Load() != 2 {
		t.Fatal("被拒凭据仍自动重试")
	}
	if r := m.Refresh(context.Background(), "a"); r.Status != "success" || r.Balance.NextRefreshAt == nil {
		t.Fatalf("手动恢复未重启自动计划：%+v", r)
	}
}

func TestManagerLongRetryAfterAndBackoffCap(t *testing.T) {
	m, clock := testBalanceManager(t)
	s := &balanceSource{failures: 20, retryUntil: clock.read().Add(48 * time.Hour)}
	if next := m.nextTime(s, clock.read(), time.Hour); next != s.retryUntil {
		t.Fatal("Retry-After 被本地上限裁短")
	}
	s.retryUntil = time.Time{}
	if delay := m.nextTime(s, clock.read(), time.Hour).Sub(clock.read()); delay != 24*time.Hour {
		t.Fatal("失败退避超过二十四小时")
	}
}

func TestManagerJobTimeoutDoesNotUseSubscriberDeadline(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan context.Context, 1)
	m.Reconcile(map[string]Source{"a": {Key: "one", Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		started <- ctx
		<-ctx.Done()
		return nil, balanceManagerError("query_cancelled")
	}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	result := startManagerRefresh(m, ctx, "a")
	jobCtx := receiveManager(t, started)
	deadline, ok := jobCtx.Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		t.Fatal("任务未使用独立预算")
	}
	r := receiveManager(t, result)
	if r.Status != "failed" || r.Balance.Error == nil || r.Balance.Error.Code != "upstream_timeout" {
		t.Fatalf("任务预算超时未形成固定错误：%+v", r)
	}
}

func TestManagerRefreshSourceRejectsChangedIdentityBeforeSubscribe(t *testing.T) {
	m, _ := testBalanceManager(t)
	var calls atomic.Int32
	source := Source{Key: "old", Fetch: func(context.Context) (*Snapshot, *QueryError) {
		calls.Add(1)
		return managerSnapshot(), nil
	}}
	m.Reconcile(map[string]Source{"a": source})
	// 模拟配置锁内捕获身份，释放锁后配置立即变更。
	expectedKey := source.Key
	source.Key = "new"
	m.Reconcile(map[string]Source{"a": source})
	for _, provider := range []string{"a", "removed"} {
		result := m.RefreshSource(context.Background(), provider, expectedKey)
		if result.Status != "skipped" || result.Balance.Error == nil || result.Balance.Error.Code != "configuration_changed" {
			t.Fatalf("身份变化未拒绝订阅：%+v", result)
		}
	}
	if calls.Load() != 0 || m.View("a").LastAttemptAt != nil {
		t.Fatal("预期身份不符仍发起任务或写入尝试时间")
	}
	if result := m.RefreshSource(context.Background(), "a", "new"); result.Status != "success" || calls.Load() != 1 {
		t.Fatalf("相同预期身份不能刷新：%+v", result)
	}
}

func TestManagerRefreshSourceEmptyKeyUsesCurrentIdentity(t *testing.T) {
	m, _ := testBalanceManager(t)
	var calls atomic.Int32
	m.Reconcile(map[string]Source{"a": {Key: "current", Fetch: func(context.Context) (*Snapshot, *QueryError) {
		calls.Add(1)
		return managerSnapshot(), nil
	}}})
	if result := m.RefreshSource(context.Background(), "a", ""); result.Status != "success" || calls.Load() != 1 {
		t.Fatalf("空身份应复用普通刷新行为：%+v", result)
	}
	if result := m.Refresh(context.Background(), "a"); result.Status != "cached" || calls.Load() != 1 {
		t.Fatalf("普通刷新未复用相同限频状态：%+v", result)
	}
}

func TestManagerRefreshSourceDetectsIdentityChangeWhileRunning(t *testing.T) {
	m, _ := testBalanceManager(t)
	started := make(chan context.Context, 1)
	m.Reconcile(map[string]Source{"a": {Key: "old", Fetch: func(ctx context.Context) (*Snapshot, *QueryError) {
		started <- ctx
		<-ctx.Done()
		return nil, balanceManagerError("query_cancelled")
	}}})
	results := make(chan Result, 1)
	go func() { results <- m.RefreshSource(context.Background(), "a", "old") }()
	receiveManager(t, started)
	m.Reconcile(map[string]Source{"a": {Key: "new"}})
	result := receiveManager(t, results)
	if result.Status != "skipped" || result.Balance.Error == nil || result.Balance.Error.Code != "configuration_changed" {
		t.Fatalf("执行中身份变更未被识别：%+v", result)
	}
}

func TestManagerSharedProvidersAreSafeAndSorted(t *testing.T) {
	m, _ := testBalanceManager(t)
	source := Source{Key: "shared"}
	m.Reconcile(map[string]Source{"c": source, "a": source, "b": source, "other": {Key: "separate"}})
	view := m.View("a")
	if len(view.SharedProviders) != 2 || view.SharedProviders[0] != "b" || view.SharedProviders[1] != "c" {
		t.Fatalf("共享来源说明=%v", view.SharedProviders)
	}
	view.SharedProviders[0] = "changed"
	if m.View("a").SharedProviders[0] != "b" {
		t.Fatal("共享名称切片未隔离")
	}
	if len(m.View("other").SharedProviders) != 0 || m.View("missing").SharedProviders == nil {
		t.Fatal("独立或未配置来源说明错误")
	}
}
