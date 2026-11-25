package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cancelAtQueueAdmissionContext struct {
	errChecks int
	done      chan struct{}
	canceled  bool
}

func (c *cancelAtQueueAdmissionContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAtQueueAdmissionContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAtQueueAdmissionContext) Value(any) any               { return nil }
func (c *cancelAtQueueAdmissionContext) Err() error {
	c.errChecks++
	if c.errChecks >= 3 {
		if !c.canceled {
			close(c.done)
			c.canceled = true
		}
		return context.Canceled
	}
	return nil
}

func acquireDebugAsync(ctx context.Context, m *Manager, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		release, waitMs, err := m.AcquireForDebug(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs)
		result <- acquireResult{release: release, waitMs: waitMs, err: err}
	}()
	return result
}

func acquireGroupDebugAsync(ctx context.Context, m *Manager, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int, group string) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		release, waitMs, err := m.AcquireGroupForDebug(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs, group)
		result <- acquireResult{release: release, waitMs: waitMs, err: err}
	}()
	return result
}

func hasProviderQueue(m *Manager, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.queues[name]
	return ok
}

func hasGroupQueue(m *Manager, name string) bool {
	m.groupsMu.Lock()
	defer m.groupsMu.Unlock()
	_, ok := m.groups[name]
	return ok
}

func waitForProviderQueueRemoval(t *testing.T, m *Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !hasProviderQueue(m, name) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("限速窗口过期后 Provider 队列仍未回收：%s", name)
}

func waitForGroupQueueRemoval(t *testing.T, m *Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !hasGroupQueue(m, name) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("限速窗口过期后共享组队列仍未回收：%s", name)
}

func waitForGroupQueuedWithin(t *testing.T, m *Manager, group string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if status := m.GroupStatusOf(group); status.Queued == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待共享组请求入队超时：%+v", m.GroupStatusOf(group))
}

func TestAcquireForDebugKeepsDisabledProviderLimitsAndReclaimsQueue(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	firstRelease, _, err := m.AcquireForDebug(context.Background(), "disabled", 1, 0, 1000)
	if err != nil {
		t.Fatalf("停用 Provider 的调试 AcquireForDebug() error = %v", err)
	}
	second := acquireDebugAsync(context.Background(), m, "disabled", 1, 0, 1000)
	waitForStatus(t, m, "disabled", 1, 0, func(status Status) bool {
		return status.Running == 1 && status.Queued == 1
	})
	expectPending(t, second)
	m.Reconcile(map[string]Limits{})
	m.mu.Lock()
	q := m.queues["disabled"]
	m.mu.Unlock()
	if q == nil {
		t.Fatal("停用状态 Reconcile 回收了仍在使用的队列")
	}
	q.mu.Lock()
	retired := q.retired
	q.mu.Unlock()
	if !retired || m.StatusOf("disabled", 1, 0).Running != 1 {
		t.Fatal("停用状态 Reconcile 提前回收了在途调试队列")
	}
	if _, _, err := m.Acquire(context.Background(), "disabled", 1, 0, 1000); !errors.Is(err, ErrProviderRemoved) {
		t.Fatalf("普通 Acquire() 对停用 Provider 的 error = %v，期望 %v", err, ErrProviderRemoved)
	}

	firstRelease()
	got := awaitAcquire(t, second, time.Second)
	if got.err != nil {
		t.Fatalf("等待中的调试请求未获准入：%v", got.err)
	}
	got.release()
	if hasProviderQueue(m, "disabled") {
		t.Fatal("停用 Provider 的调试队列排空后仍留在 Manager 中")
	}

	m.Reconcile(map[string]Limits{"disabled": {MaxConcurrent: 1}})
	ordinaryRelease, _, err := m.Acquire(context.Background(), "disabled", 1, 0, 1000)
	if err != nil {
		t.Fatalf("回收后重新启用 Provider 的普通 Acquire() error = %v", err)
	}
	ordinaryRelease()
	if !hasProviderQueue(m, "disabled") {
		t.Fatal("重新启用后的普通请求未建立活动队列")
	}
}

func TestAcquireForDebugReclaimsQueueWhenCanceledAfterQueueLookup(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	ctx := &cancelAtQueueAdmissionContext{done: make(chan struct{})}
	if _, _, err := m.AcquireForDebug(ctx, "canceled", 1, 0, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("获准前取消的调试 AcquireForDebug() error = %v，期望 %v", err, context.Canceled)
	}
	if hasProviderQueue(m, "canceled") {
		t.Fatal("调试准入前取消后，已退休的 Provider 队列仍留在 Manager 中")
	}
}

func TestAcquireForDebugReconcileAndReenableKeepsQueueState(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	release, _, err := m.AcquireForDebug(context.Background(), "re-enabled", 1, 0, 1000)
	if err != nil {
		t.Fatalf("停用 Provider 的调试 AcquireForDebug() error = %v", err)
	}
	q := func() *providerQueue {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.queues["re-enabled"]
	}()
	if q == nil {
		t.Fatal("调试准入后队列不存在")
	}

	// 热重载再次确认 Provider 停用时，不应中断已获得的调试槽位。
	m.Reconcile(map[string]Limits{})
	if !hasProviderQueue(m, "re-enabled") || m.StatusOf("re-enabled", 1, 0).Running != 1 {
		t.Fatal("停用状态 Reconcile 中断或提前回收了在途调试队列")
	}

	// 在调试请求仍在途时重新启用，应复用队列并清除 retired 状态。
	m.Reconcile(map[string]Limits{"re-enabled": {MaxConcurrent: 2}})
	q.mu.Lock()
	retired, maxConcurrent := q.retired, q.maxConcurrent
	q.mu.Unlock()
	if retired || maxConcurrent != 2 {
		t.Fatalf("重新启用后的队列状态 retired=%t maxConcurrent=%d", retired, maxConcurrent)
	}
	release()

	ordinaryRelease, _, err := m.Acquire(context.Background(), "re-enabled", 2, 0, 1000)
	if err != nil {
		t.Fatalf("重新启用后的普通 Acquire() error = %v", err)
	}
	ordinaryRelease()
	if !hasProviderQueue(m, "re-enabled") {
		t.Fatal("重新启用后的普通请求未保留活动队列")
	}
}

func TestAcquireForDebugRetainsDisabledProviderRateWindow(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	firstRelease, _, err := m.AcquireForDebug(context.Background(), "rate-limited", 1, 1, 1000)
	if err != nil {
		t.Fatalf("首次调试准入失败：%v", err)
	}
	firstRelease()
	if !hasProviderQueue(m, "rate-limited") {
		t.Fatal("仍有有效限速记录时，停用 Provider 队列被提前回收")
	}

	second := acquireDebugAsync(context.Background(), m, "rate-limited", 1, 1, 1500)
	waitForStatus(t, m, "rate-limited", 1, 1, func(status Status) bool { return status.Queued == 1 })
	expectPending(t, second)
	got := awaitAcquire(t, second, 2*time.Second)
	if got.err != nil {
		t.Fatalf("限速窗口到期后调试请求未获准：%v", got.err)
	}
	got.release()
	waitForProviderQueueRemoval(t, m, "rate-limited")
}

func TestAcquireForDebugRateWindowSurvivesProviderReenable(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	firstRelease, _, err := m.AcquireForDebug(context.Background(), "reenabled-rate-limited", 1, 1, 1000)
	if err != nil {
		t.Fatalf("首次调试准入失败：%v", err)
	}
	firstRelease()
	m.Reconcile(map[string]Limits{"reenabled-rate-limited": {MaxConcurrent: 1, MaxPerSecond: 1}})

	ordinary := acquireAsync(context.Background(), m, "reenabled-rate-limited", 1, 1, 1500)
	waitForStatus(t, m, "reenabled-rate-limited", 1, 1, func(status Status) bool { return status.Queued == 1 })
	expectPending(t, ordinary)
	got := awaitAcquire(t, ordinary, 2*time.Second)
	if got.err != nil {
		t.Fatalf("重新启用后的普通请求未在限速窗口到期后获准：%v", got.err)
	}
	got.release()
}

func TestAcquireForDebugRateWindowSurvivesRepeatedReconcile(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	firstRelease, _, err := m.AcquireForDebug(context.Background(), "reconciled-rate-limited", 1, 1, 1000)
	if err != nil {
		t.Fatalf("首次调试准入失败：%v", err)
	}
	firstRelease()
	m.Reconcile(map[string]Limits{"reconciled-rate-limited": {MaxConcurrent: 1, MaxPerSecond: 1}})
	m.Reconcile(map[string]Limits{})
	if !hasProviderQueue(m, "reconciled-rate-limited") {
		t.Fatal("重新启用后再次停用时，仍有效的调试限速窗口被回收")
	}

	second := acquireDebugAsync(context.Background(), m, "reconciled-rate-limited", 1, 1, 1500)
	waitForStatus(t, m, "reconciled-rate-limited", 1, 1, func(status Status) bool { return status.Queued == 1 })
	expectPending(t, second)
	got := awaitAcquire(t, second, 2*time.Second)
	if got.err != nil {
		t.Fatalf("重新启用后再次停用的调试请求未遵守限速窗口：%v", got.err)
	}
	got.release()
	waitForProviderQueueRemoval(t, m, "reconciled-rate-limited")
}

func TestAcquireForDebugCanceledRateWaitEventuallyReclaimsQueue(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})

	firstRelease, _, err := m.AcquireForDebug(context.Background(), "canceled-rate-wait", 1, 1, 1000)
	if err != nil {
		t.Fatalf("首次调试准入失败：%v", err)
	}
	firstRelease()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := acquireDebugAsync(ctx, m, "canceled-rate-wait", 1, 1, 1500)
	waitForStatus(t, m, "canceled-rate-wait", 1, 1, func(status Status) bool { return status.Queued == 1 })
	cancel()
	got := awaitAcquire(t, waiting, time.Second)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("取消的限速等待返回 %v，期望 %v", got.err, context.Canceled)
	}
	waitForProviderQueueRemoval(t, m, "canceled-rate-wait")
}

func TestAcquireGroupForDebugAllowsDisabledProviderWithLimits(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	if _, _, err := m.AcquireGroup(context.Background(), "disabled-group", 1, 0, 1000, "account"); !errors.Is(err, ErrProviderRemoved) {
		t.Fatalf("普通 AcquireGroup() 对停用 Provider 的 error = %v，期望 %v", err, ErrProviderRemoved)
	}

	firstRelease, _, err := m.AcquireGroupForDebug(context.Background(), "disabled-group", 1, 0, 1000, "account")
	if err != nil {
		t.Fatalf("停用 Provider 的调试 AcquireGroupForDebug() error = %v", err)
	}
	second := acquireGroupDebugAsync(context.Background(), m, "disabled-group", 1, 0, 1000, "account")
	waitForGroup(t, m, "account", func(status GroupStatus) bool {
		return status.Running == 1 && status.Queued == 1
	})
	expectPending(t, second)

	firstRelease()
	got := awaitAcquire(t, second, time.Second)
	if got.err != nil {
		t.Fatalf("共享组中的调试请求未获准入：%v", got.err)
	}
	got.release()
	if hasProviderQueue(m, "disabled-group") {
		t.Fatal("停用 Provider 的共享调试队列排空后仍留在 Manager 中")
	}
}

func TestAcquireGroupForDebugRetainsProviderRateWindowAndReclaimsQueues(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})

	firstRelease, _, err := m.AcquireGroupForDebug(context.Background(), "group-rate-limited", 1, 1, 1000, "rate-group")
	if err != nil {
		t.Fatalf("首次共享组调试准入失败：%v", err)
	}
	firstRelease()
	if !hasProviderQueue(m, "group-rate-limited") {
		t.Fatal("共享组调试请求释放后，Provider 的有效限速记录被提前回收")
	}

	second := acquireGroupDebugAsync(context.Background(), m, "group-rate-limited", 1, 1, 1500, "rate-group")
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && m.GroupStatusOf("rate-group").Queued != 1 {
		time.Sleep(time.Millisecond)
	}
	if status := m.GroupStatusOf("rate-group"); status.Queued != 1 {
		t.Fatalf("共享组速率等待未进入队列：%+v", status)
	}
	expectPending(t, second)
	got := awaitAcquire(t, second, 2*time.Second)
	if got.err != nil {
		t.Fatalf("共享组限速窗口到期后调试请求未获准：%v", got.err)
	}
	got.release()
	waitForProviderQueueRemoval(t, m, "group-rate-limited")
	waitForGroupQueueRemoval(t, m, "rate-group")
}

func TestIdleGroupRateWindowEventuallyReclaimsGroupQueue(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1, MaxPerSecond: 1})

	release, _, err := m.AcquireGroupForDebug(context.Background(), "group-window-provider", 1, 0, 1000, "idle-rate-group")
	if err != nil {
		t.Fatalf("共享组调试准入失败：%v", err)
	}
	release()
	if !hasGroupQueue(m, "idle-rate-group") {
		t.Fatal("共享组有效限速窗口尚未结束时，组队列被提前回收")
	}
	waitForGroupQueueRemoval(t, m, "idle-rate-group")
}

func TestAcquireGroupForDebugCanceledRateWaitReclaimsProviderQueue(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})

	release, _, err := m.AcquireGroupForDebug(context.Background(), "disabled", 1, 1, 1000, "account")
	if err != nil {
		t.Fatalf("首次共享组调试准入失败：%v", err)
	}
	release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := acquireGroupDebugAsync(ctx, m, "disabled", 1, 1, 1000, "account")
	waitForGroupQueuedWithin(t, m, "account", 200*time.Millisecond)
	cancel()
	got := awaitAcquire(t, waiting, time.Second)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("取消的共享组限速等待返回 %v，期望 %v", got.err, context.Canceled)
	}
	waitForProviderQueueRemoval(t, m, "disabled")
}

func TestAcquireGroupForDebugTimedOutRateWaitReclaimsProviderQueue(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})

	release, _, err := m.AcquireGroupForDebug(context.Background(), "disabled-timeout", 1, 1, 1000, "account-timeout")
	if err != nil {
		t.Fatalf("首次共享组调试准入失败：%v", err)
	}
	release()

	waiting := acquireGroupDebugAsync(context.Background(), m, "disabled-timeout", 1, 1, 500, "account-timeout")
	waitForGroupQueuedWithin(t, m, "account-timeout", 250*time.Millisecond)
	got := awaitAcquire(t, waiting, time.Second)
	if !errors.Is(got.err, ErrQueueTimeout) {
		t.Fatalf("超时的共享组限速等待返回 %v，期望 %v", got.err, ErrQueueTimeout)
	}
	waitForProviderQueueRemoval(t, m, "disabled-timeout")
}
