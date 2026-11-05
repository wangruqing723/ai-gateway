package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func acquireGroupAsync(ctx context.Context, m *Manager, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int, group string) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		var got acquireResult
		defer func() {
			got.panicValue = recover()
			result <- got
		}()
		got.release, got.waitMs, got.err = m.AcquireGroup(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs, group)
	}()
	return result
}

func mustAcquireGroup(t *testing.T, m *Manager, name string, maxConcurrent int, group string) func() {
	t.Helper()
	release, _, err := m.AcquireGroup(context.Background(), name, maxConcurrent, 0, 1000, group)
	if err != nil {
		t.Fatalf("AcquireGroup() error = %v", err)
	}
	return release
}

func waitForGroup(t *testing.T, m *Manager, group string, ready func(GroupStatus) bool) GroupStatus {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var status GroupStatus
	for time.Now().Before(deadline) {
		status = m.GroupStatusOf(group)
		if ready(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待组状态超时，最后状态：%+v", status)
	return GroupStatus{}
}

func expectPending(t *testing.T, result <-chan acquireResult) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("请求不应已准入：err=%v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestAcquireGroupDisabledFallsBackToProviderQueue(t *testing.T) {
	m := NewManager()
	release, _, err := m.AcquireGroup(context.Background(), "p1", 1, 0, 1000, "acct")
	if err != nil {
		t.Fatalf("AcquireGroup() error = %v", err)
	}
	defer release()
	if got := m.GroupStatusOf("acct"); got != (GroupStatus{}) {
		t.Fatalf("未启用组上限时不应建组：%+v", got)
	}
	if got := m.StatusOf("p1", 1, 0); got.Running != 1 {
		t.Fatalf("provider running = %d，期望 1", got.Running)
	}
}

func TestAcquireGroupSharesConcurrencyAcrossProviders(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	first := mustAcquireGroup(t, m, "p1", 5, "acct")

	second := acquireGroupAsync(context.Background(), m, "p2", 5, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Running == 1 && s.Queued == 1 })
	if got := m.StatusOf("p2", 5, 0); got.Running != 0 || got.Queued != 1 {
		t.Fatalf("组内等待不应占用 p2 槽位，且应计入排队：%+v", got)
	}
	expectPending(t, second)

	first()
	got := awaitAcquire(t, second, time.Second)
	if got.err != nil {
		t.Fatalf("第二个请求 error = %v", got.err)
	}
	if status := m.StatusOf("p2", 5, 0); status.Running != 1 || status.Queued != 0 {
		t.Fatalf("p2 状态 = %+v", status)
	}
	got.release()
	got.release()
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s == (GroupStatus{}) })
	if status := m.StatusOf("p2", 5, 0); status.Running != 0 {
		t.Fatalf("重复释放后 p2 running = %d", status.Running)
	}
}

func TestAcquireGroupSkipsWaiterBlockedByBusyProvider(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 2})
	// p1 被组外的普通请求占满。
	busy := mustAcquire(t, m, "p1", 1, 0, 1000)

	blocked := acquireGroupAsync(context.Background(), m, "p1", 1, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 1 })
	other := acquireGroupAsync(context.Background(), m, "p2", 5, 0, 1000, "acct")
	got := awaitAcquire(t, other, time.Second)
	if got.err != nil {
		t.Fatalf("同账户另一 provider 不应被挡住：%v", got.err)
	}
	defer got.release()
	expectPending(t, blocked)
	if status := m.StatusOf("p1", 1, 0); status.Running != 1 || status.Queued != 1 {
		t.Fatalf("p1 状态 = %+v", status)
	}

	busy()
	first := awaitAcquire(t, blocked, time.Second)
	if first.err != nil {
		t.Fatalf("p1 释放后应准入：%v", first.err)
	}
	first.release()
}

func TestAcquireGroupDoesNotCutAheadOfPlainWaiters(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 5})
	holder := mustAcquire(t, m, "p1", 1, 0, 1000)
	plain := acquireAsync(context.Background(), m, "p1", 1, 0, 1000)
	waitForStatus(t, m, "p1", 1, 0, func(s Status) bool { return s.Queued == 1 })
	grouped := acquireGroupAsync(context.Background(), m, "p1", 1, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 1 })

	holder()
	plainGot := awaitAcquire(t, plain, time.Second)
	if plainGot.err != nil {
		t.Fatalf("普通等待者应先准入：%v", plainGot.err)
	}
	expectPending(t, grouped)
	plainGot.release()
	groupGot := awaitAcquire(t, grouped, time.Second)
	if groupGot.err != nil {
		t.Fatalf("组等待者随后准入：%v", groupGot.err)
	}
	groupGot.release()
}

func TestAcquireGroupFIFOWithinGroup(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	holder := mustAcquireGroup(t, m, "p1", 5, "acct")
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == i })
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, _, err := m.AcquireGroup(context.Background(), "p1", 5, 0, 2000, "acct")
			if err != nil {
				t.Errorf("AcquireGroup(%d) error = %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			release()
		}(i)
	}
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 4 })
	holder()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("组内准入顺序 = %v，期望按到达顺序", order)
		}
	}
}

func TestAcquireGroupTimeoutAndCancelReleaseNothing(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	holder := mustAcquireGroup(t, m, "p1", 5, "acct")
	defer holder()

	start := time.Now()
	_, _, err := m.AcquireGroup(context.Background(), "p2", 5, 0, 40, "acct")
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("超时 error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 35*time.Millisecond {
		t.Fatalf("应等满 maxQueueWait，实际 %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pending := acquireGroupAsync(ctx, m, "p2", 5, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 1 })
	cancel()
	got := awaitAcquire(t, pending, time.Second)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("取消 error = %v", got.err)
	}
	if status := m.GroupStatusOf("acct"); status.Running != 1 || status.Queued != 0 {
		t.Fatalf("组状态 = %+v", status)
	}
	if status := m.StatusOf("p2", 5, 0); status.Running != 0 || status.Queued != 0 {
		t.Fatalf("p2 状态 = %+v", status)
	}
}

func TestAcquireGroupWaiterRejectedWhenProviderRemoved(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{"p1": {MaxConcurrent: 5}, "p2": {MaxConcurrent: 5}})
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	holder := mustAcquireGroup(t, m, "p1", 5, "acct")
	defer holder()
	pending := acquireGroupAsync(context.Background(), m, "p2", 5, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 1 })

	m.Reconcile(map[string]Limits{"p1": {MaxConcurrent: 5}})
	holder()
	got := awaitAcquire(t, pending, time.Second)
	if !errors.Is(got.err, ErrProviderRemoved) {
		t.Fatalf("provider 删除后 error = %v", got.err)
	}
	if status := m.StatusOf("p2", 5, 0); status.Queued != 0 {
		t.Fatalf("删除后不应残留排队计数：%+v", status)
	}
}

func TestAcquireGroupLimitRemovalAdmitsRemainingWaiters(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 1})
	holder := mustAcquireGroup(t, m, "p1", 5, "acct")
	defer holder()
	pending := acquireGroupAsync(context.Background(), m, "p2", 5, 0, 1000, "acct")
	waitForGroup(t, m, "acct", func(s GroupStatus) bool { return s.Queued == 1 })

	m.SetGroupLimits(GroupLimits{})
	got := awaitAcquire(t, pending, time.Second)
	if got.err != nil {
		t.Fatalf("取消限制后残留等待者应准入：%v", got.err)
	}
	got.release()
}

func TestAcquireGroupRateLimit(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxPerSecond: 2})
	for i := 0; i < 2; i++ {
		release := mustAcquireGroup(t, m, "p1", 5, "acct")
		release()
	}
	start := time.Now()
	third := acquireGroupAsync(context.Background(), m, "p2", 5, 0, 3000, "acct")
	got := awaitAcquire(t, third, 2*time.Second)
	if got.err != nil {
		t.Fatalf("第三个请求 error = %v", got.err)
	}
	got.release()
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("组速率应跨 provider 生效，第三个请求只等了 %s", elapsed)
	}
}

func TestAcquireGroupWaitsForProviderRateWindow(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 5})
	release := mustAcquire(t, m, "p1", 5, 1, 1000)
	release()
	start := time.Now()
	got := awaitAcquire(t, acquireGroupAsync(context.Background(), m, "p1", 5, 1, 3000, "acct"), 2*time.Second)
	if got.err != nil {
		t.Fatalf("error = %v", got.err)
	}
	got.release()
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("应遵守 provider 速率窗口，实际只等 %s", elapsed)
	}
}

func TestAcquireGroupConcurrentNeverExceedsLimits(t *testing.T) {
	m := NewManager()
	m.SetGroupLimits(GroupLimits{MaxConcurrent: 3})
	providers := []string{"p1", "p2", "p3"}
	var groupRunning, peak atomic.Int64
	var providerRunning [3]atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx := i % len(providers)
			release, _, err := m.AcquireGroup(context.Background(), providers[idx], 2, 0, 5000, "acct")
			if err != nil {
				t.Errorf("AcquireGroup error = %v", err)
				return
			}
			n := groupRunning.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			if p := providerRunning[idx].Add(1); p > 2 {
				t.Errorf("%s 并发 %d 超过上限 2", providers[idx], p)
			}
			time.Sleep(time.Millisecond)
			providerRunning[idx].Add(-1)
			groupRunning.Add(-1)
			release()
			release()
		}(i)
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("组并发峰值 %d 超过上限 3", peak.Load())
	}
	if status := m.GroupStatusOf("acct"); status != (GroupStatus{}) {
		t.Fatalf("结束后组状态 = %+v", status)
	}
	for _, name := range providers {
		if status := m.StatusOf(name, 2, 0); status.Running != 0 || status.Queued != 0 {
			t.Fatalf("%s 结束后状态 = %+v", name, status)
		}
	}
}
