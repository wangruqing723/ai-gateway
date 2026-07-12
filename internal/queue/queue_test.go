package queue

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type acquireResult struct {
	release    func()
	waitMs     int64
	err        error
	panicValue any
}

type observedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *observedContext) Err() error {
	c.once.Do(func() { close(c.checked) })
	return c.Context.Err()
}

func newObservedContext(ctx context.Context) (*observedContext, <-chan struct{}) {
	checked := make(chan struct{})
	return &observedContext{Context: ctx, checked: checked}, checked
}

func acquireAsync(ctx context.Context, m *Manager, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int) <-chan acquireResult {
	result := make(chan acquireResult, 1)
	go func() {
		var got acquireResult
		defer func() {
			got.panicValue = recover()
			result <- got
		}()
		got.release, got.waitMs, got.err = m.Acquire(ctx, name, maxConcurrent, maxPerSecond, maxQueueWaitMs)
	}()
	return result
}

func waitForStatus(t *testing.T, m *Manager, name string, maxConcurrent, maxPerSecond int, ready func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	var status Status
	for time.Now().Before(deadline) {
		status = m.StatusOf(name, maxConcurrent, maxPerSecond)
		if ready(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待队列状态超时，最后状态：%+v", status)
	return Status{}
}

func awaitAcquire(t *testing.T, result <-chan acquireResult, timeout time.Duration) acquireResult {
	t.Helper()
	select {
	case got := <-result:
		if got.panicValue != nil {
			t.Fatalf("Acquire panic: %v", got.panicValue)
		}
		return got
	case <-time.After(timeout):
		t.Fatalf("等待 Acquire 结果超过 %s", timeout)
		return acquireResult{}
	}
}

func mustAcquire(t *testing.T, m *Manager, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int) func() {
	t.Helper()
	release, _, err := m.Acquire(context.Background(), name, maxConcurrent, maxPerSecond, maxQueueWaitMs)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return release
}

func TestUpdateProviderDisablingRateWakesWaiterWithoutPanic(t *testing.T) {
	m := NewManager()
	release := mustAcquire(t, m, "hot-reload", 2, 1, 0)
	release()

	result := acquireAsync(context.Background(), m, "hot-reload", 2, 1, 2000)
	waitForStatus(t, m, "hot-reload", 2, 1, func(status Status) bool {
		// 旧实现把等待速率的请求误计为 Running；新实现应将其保留在 Queued。
		return status.Queued == 1 || status.Running == 1
	})
	time.Sleep(20 * time.Millisecond)

	updatedAt := time.Now()
	m.UpdateProvider("hot-reload", 2, 0)
	got := awaitAcquire(t, result, 1500*time.Millisecond)
	if got.err != nil {
		t.Fatalf("关闭速率限制后的 Acquire() error = %v", got.err)
	}
	defer got.release()
	if elapsed := time.Since(updatedAt); elapsed > 300*time.Millisecond {
		t.Fatalf("关闭速率限制后未及时唤醒：%s", elapsed)
	}
}

func TestMaxQueueWaitCoversEntireAdmissionWait(t *testing.T) {
	m := NewManager()
	releaseFirst := mustAcquire(t, m, "deadline", 1, 1, 0)

	startedAt := time.Now()
	result := acquireAsync(context.Background(), m, "deadline", 1, 1, 150)
	waitForStatus(t, m, "deadline", 1, 1, func(status Status) bool { return status.Queued == 1 })
	time.Sleep(50 * time.Millisecond)
	releaseFirst()
	time.Sleep(30 * time.Millisecond)
	whileRateLimited := m.StatusOf("deadline", 1, 1)

	got := awaitAcquire(t, result, 1500*time.Millisecond)
	elapsed := time.Since(startedAt)
	if !errors.Is(got.err, ErrQueueTimeout) {
		if got.release != nil {
			got.release()
		}
		t.Fatalf("Acquire() error = %v，期望 %v", got.err, ErrQueueTimeout)
	}
	if elapsed < 100*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("总等待时间 = %s，期望接近 150ms", elapsed)
	}
	if got.waitMs < 100 || got.waitMs > 500 {
		t.Fatalf("waitMs = %d，期望覆盖完整 admission 等待", got.waitMs)
	}
	if whileRateLimited.Running != 0 || whileRateLimited.Queued != 1 {
		t.Fatalf("等待速率时状态 = %+v，期望 Running=0 Queued=1", whileRateLimited)
	}
}

func TestCanceledWhileWaitingForManagerLockIsNeverAdmitted(t *testing.T) {
	m := NewManager()
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx, checked := newObservedContext(baseCtx)

	m.mu.Lock()
	managerLocked := true
	defer func() {
		if managerLocked {
			m.mu.Unlock()
		}
	}()
	result := acquireAsync(ctx, m, "manager-lock", 1, 1, 0)
	select {
	case <-checked:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Acquire 未执行首次 context 检查")
	}
	cancel()
	m.mu.Unlock()
	managerLocked = false

	got := awaitAcquire(t, result, 300*time.Millisecond)
	if !errors.Is(got.err, context.Canceled) {
		if got.release != nil {
			got.release()
		}
		t.Fatalf("Acquire() error = %v，期望 context.Canceled", got.err)
	}
	status := m.StatusOf("manager-lock", 1, 1)
	if status.Running != 0 || status.Queued != 0 {
		t.Fatalf("锁等待期间取消后的状态 = %+v，期望 Running=0 Queued=0", status)
	}
	m.mu.Lock()
	q := m.queues["manager-lock"]
	m.mu.Unlock()
	if q != nil {
		q.mu.Lock()
		requestCount := len(q.requestTimes)
		q.mu.Unlock()
		if requestCount != 0 {
			t.Fatalf("锁等待期间取消仍消耗了 %d 个速率时间戳", requestCount)
		}
	}
}

func TestMaxQueueWaitIncludesProviderLockWait(t *testing.T) {
	m := NewManager()
	q := m.ensure("provider-lock", 1, 1)
	ctx, checked := newObservedContext(context.Background())

	q.mu.Lock()
	providerLocked := true
	defer func() {
		if providerLocked {
			q.mu.Unlock()
		}
	}()
	startedAt := time.Now()
	result := acquireAsync(ctx, m, "provider-lock", 1, 1, 40)
	select {
	case <-checked:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Acquire 未执行首次 context 检查")
	}
	time.Sleep(80 * time.Millisecond)
	q.mu.Unlock()
	providerLocked = false

	got := awaitAcquire(t, result, 300*time.Millisecond)
	if !errors.Is(got.err, ErrQueueTimeout) {
		if got.release != nil {
			got.release()
		}
		t.Fatalf("Acquire() error = %v，期望锁等待计入 %v", got.err, ErrQueueTimeout)
	}
	if elapsed := time.Since(startedAt); elapsed < 70*time.Millisecond {
		t.Fatalf("测试未覆盖超过 deadline 的锁等待：%s", elapsed)
	}
	q.mu.Lock()
	status := Status{Running: q.running, Queued: q.waiters.Len()}
	requestCount := len(q.requestTimes)
	q.mu.Unlock()
	if status.Running != 0 || status.Queued != 0 || requestCount != 0 {
		t.Fatalf("deadline 后首次 admission 污染状态：status=%+v requestTimes=%d", status, requestCount)
	}
}

func TestUpdateProviderGrowsConcurrencyImmediately(t *testing.T) {
	m := NewManager()
	releaseFirst := mustAcquire(t, m, "grow", 1, 0, 0)
	defer releaseFirst()

	result := acquireAsync(context.Background(), m, "grow", 1, 0, 1000)
	waitForStatus(t, m, "grow", 1, 0, func(status Status) bool { return status.Queued == 1 })
	m.UpdateProvider("grow", 2, 0)

	got := awaitAcquire(t, result, 300*time.Millisecond)
	if got.err != nil {
		t.Fatalf("扩容后的 Acquire() error = %v", got.err)
	}
	got.release()
	status := m.StatusOf("grow", 1, 0)
	if status.MaxConcurrent != 2 {
		t.Fatalf("MaxConcurrent = %d，期望 2", status.MaxConcurrent)
	}
}

func TestUpdateProviderShrinksConcurrencyWithoutPreemptingRunning(t *testing.T) {
	m := NewManager()
	releaseFirst := mustAcquire(t, m, "shrink", 2, 0, 0)
	releaseSecond := mustAcquire(t, m, "shrink", 2, 0, 0)
	defer releaseFirst()
	defer releaseSecond()

	m.UpdateProvider("shrink", 1, 0)
	result := acquireAsync(context.Background(), m, "shrink", 2, 0, 1000)
	waitForStatus(t, m, "shrink", 2, 0, func(status Status) bool { return status.Queued == 1 })

	releaseFirst()
	select {
	case got := <-result:
		if got.panicValue != nil {
			t.Fatalf("Acquire panic: %v", got.panicValue)
		}
		if got.release != nil {
			got.release()
		}
		t.Fatal("缩容后仍有一个请求运行时，新请求不应被 admission")
	case <-time.After(100 * time.Millisecond):
	}

	releaseSecond()
	got := awaitAcquire(t, result, 300*time.Millisecond)
	if got.err != nil {
		t.Fatalf("运行数降到新上限后的 Acquire() error = %v", got.err)
	}
	got.release()
}

func TestAcquireUsesFIFOOrder(t *testing.T) {
	m := NewManager()
	releaseFirst := mustAcquire(t, m, "fifo", 1, 0, 0)

	type admittedRequest struct {
		id      int
		release func()
		err     error
	}
	const waiterCount = 5
	admitted := make(chan admittedRequest, waiterCount)
	continueRequest := make([]chan struct{}, waiterCount)
	for id := 0; id < waiterCount; id++ {
		continueRequest[id] = make(chan struct{})
		go func(id int) {
			release, _, err := m.Acquire(context.Background(), "fifo", 1, 0, 2000)
			admitted <- admittedRequest{id: id, release: release, err: err}
			if err == nil {
				<-continueRequest[id]
				release()
			}
		}(id)
		wantQueued := id + 1
		waitForStatus(t, m, "fifo", 1, 0, func(status Status) bool { return status.Queued == wantQueued })
	}

	releaseFirst()
	for wantID := 0; wantID < waiterCount; wantID++ {
		select {
		case got := <-admitted:
			if got.err != nil {
				t.Fatalf("waiter %d Acquire() error = %v", got.id, got.err)
			}
			if got.id != wantID {
				t.Errorf("admission 顺序[%d] = %d，期望 %d", wantID, got.id, wantID)
			}
			close(continueRequest[got.id])
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("等待 FIFO waiter %d 超时", wantID)
		}
	}
}

func TestWaiterQueueIsNotSliceBacked(t *testing.T) {
	field, ok := reflect.TypeOf(providerQueue{}).FieldByName("waiters")
	if !ok {
		t.Fatal("providerQueue 缺少 waiters 字段")
	}
	if field.Type.Kind() == reflect.Slice {
		t.Fatalf("waiters 仍由 slice 承载，队首删除和任意取消无法保证 O(1)：%s", field.Type)
	}
}

func TestLongQueueCancellationPreservesFIFO(t *testing.T) {
	m := NewManager()
	releaseFirst := mustAcquire(t, m, "long-fifo", 1, 0, 0)

	type outcome struct {
		id      int
		release func()
		err     error
	}
	const waiterCount = 128
	outcomes := make(chan outcome, waiterCount)
	cancels := make([]context.CancelFunc, waiterCount)
	for id := 0; id < waiterCount; id++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[id] = cancel
		go func(id int, ctx context.Context) {
			release, _, err := m.Acquire(ctx, "long-fifo", 1, 0, 5000)
			outcomes <- outcome{id: id, release: release, err: err}
		}(id, ctx)
		wantQueued := id + 1
		waitForStatus(t, m, "long-fifo", 1, 0, func(status Status) bool { return status.Queued == wantQueued })
	}

	for id := 1; id < waiterCount; id += 2 {
		cancels[id]()
	}
	canceled := make(map[int]bool, waiterCount/2)
	for len(canceled) < waiterCount/2 {
		select {
		case got := <-outcomes:
			if got.id%2 == 0 || !errors.Is(got.err, context.Canceled) {
				if got.release != nil {
					got.release()
				}
				t.Fatalf("取消阶段收到异常结果：id=%d err=%v", got.id, got.err)
			}
			canceled[got.id] = true
		case <-time.After(time.Second):
			t.Fatalf("长队列取消超时：完成 %d/%d", len(canceled), waiterCount/2)
		}
	}
	waitForStatus(t, m, "long-fifo", 1, 0, func(status Status) bool { return status.Queued == waiterCount/2 })

	releaseFirst()
	for wantID := 0; wantID < waiterCount; wantID += 2 {
		select {
		case got := <-outcomes:
			if got.err != nil {
				t.Fatalf("waiter %d Acquire() error = %v", got.id, got.err)
			}
			if got.id != wantID {
				t.Errorf("长队列 admission 顺序 = %d，期望 %d", got.id, wantID)
			}
			got.release()
		case <-time.After(time.Second):
			t.Fatalf("等待长队列 waiter %d 超时", wantID)
		}
	}
	waitForStatus(t, m, "long-fifo", 1, 0, func(status Status) bool {
		return status.Running == 0 && status.Queued == 0
	})
}

func TestCanceledWaiterIsRemovedWithoutRunning(t *testing.T) {
	m := NewManager()
	release := mustAcquire(t, m, "cancel", 2, 1, 0)
	release()

	ctx, cancel := context.WithCancel(context.Background())
	result := acquireAsync(ctx, m, "cancel", 2, 1, 0)
	status := waitForStatus(t, m, "cancel", 2, 1, func(status Status) bool {
		return status.Queued == 1 || status.Running == 1
	})
	cancel()

	got := awaitAcquire(t, result, 300*time.Millisecond)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Acquire() error = %v，期望 context.Canceled", got.err)
	}
	if status.Running != 0 || status.Queued != 1 {
		t.Fatalf("取消前状态 = %+v，未 admission 的请求不应计入 Running", status)
	}
	status = waitForStatus(t, m, "cancel", 2, 1, func(status Status) bool {
		return status.Running == 0 && status.Queued == 0
	})

	m.UpdateProvider("cancel", 2, 0)
	next := awaitAcquire(t, acquireAsync(context.Background(), m, "cancel", 2, 0, 300), 300*time.Millisecond)
	if next.err != nil {
		t.Fatalf("取消后下一请求 Acquire() error = %v", next.err)
	}
	next.release()
}

func TestReleaseIsIdempotentAndConcurrentSafe(t *testing.T) {
	m := NewManager()
	release := mustAcquire(t, m, "release", 1, 0, 0)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release()
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("并发重复 release 阻塞")
	}

	status := m.StatusOf("release", 1, 0)
	if status.Running != 0 {
		t.Fatalf("重复 release 后 Running = %d，期望 0", status.Running)
	}
	next := mustAcquire(t, m, "release", 1, 0, 100)
	next()
}

func TestStatusReportsUpdatedLimits(t *testing.T) {
	m := NewManager()
	release := mustAcquire(t, m, "status", 2, 3, 0)
	defer release()

	m.UpdateProvider("status", 4, 5)
	status := m.StatusOf("status", 99, 99)
	if status.MaxConcurrent != 4 || status.MaxPerSecond != 5 {
		t.Fatalf("StatusOf() = %+v，期望动态 limits 4/5", status)
	}
}

func TestReconcileRemovesIdleDeletedProviderImmediately(t *testing.T) {
	m := NewManager()
	releaseKeep := mustAcquire(t, m, "keep", 1, 1, 0)
	releaseDrop := mustAcquire(t, m, "drop", 2, 2, 0)
	releaseKeep()
	releaseDrop()

	m.Reconcile(map[string]Limits{
		"keep": {MaxConcurrent: 3, MaxPerSecond: 4},
	})

	status := m.StatusOf("keep", 99, 99)
	if status.MaxConcurrent != 3 || status.MaxPerSecond != 4 {
		t.Fatalf("保留 provider 状态 = %+v，期望动态 limits 3/4", status)
	}
	m.mu.Lock()
	_, keepExists := m.queues["keep"]
	_, dropExists := m.queues["drop"]
	m.mu.Unlock()
	if !keepExists {
		t.Fatal("Reconcile 删除了仍存在的 provider")
	}
	if dropExists {
		t.Fatal("空闲的删除 provider 应立即从 manager map 回收")
	}
}

func TestReconcileRemovesRetiredProviderAfterInFlightDrain(t *testing.T) {
	m := NewManager()
	releaseRunning := mustAcquire(t, m, "drain", 1, 0, 0)
	m.mu.Lock()
	originalQueue := m.queues["drain"]
	m.mu.Unlock()

	waiting := acquireAsync(context.Background(), m, "drain", 1, 0, 3000)
	waitForStatus(t, m, "drain", 1, 0, func(status Status) bool {
		return status.Running == 1 && status.Queued == 1
	})
	m.Reconcile(map[string]Limits{})

	m.mu.Lock()
	retainedQueue := m.queues["drain"]
	m.mu.Unlock()
	if retainedQueue != originalQueue {
		t.Fatal("删除时有在途请求，manager 未保留原队列")
	}

	releaseRunning()
	gotWaiting := awaitAcquire(t, waiting, 300*time.Millisecond)
	if gotWaiting.err != nil {
		t.Fatalf("删除前已入队 waiter Acquire() error = %v", gotWaiting.err)
	}
	gotWaiting.release()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, exists := m.queues["drain"]
		m.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("删除 provider 的在途 running/waiter 排空后仍未回收")
}

func TestAcquireAfterDeletionIsRejectedWithoutRecreatingQueue(t *testing.T) {
	m := NewManager()
	release := mustAcquire(t, m, "deleted", 1, 0, 0)
	release()
	m.Reconcile(map[string]Limits{})

	startedAt := time.Now()
	got := awaitAcquire(t, acquireAsync(context.Background(), m, "deleted", 99, 0, 500), 300*time.Millisecond)
	if got.err == nil {
		got.release()
		t.Fatal("删除后才进入的 stale Acquire 应被拒绝")
	}
	if errors.Is(got.err, ErrQueueTimeout) {
		t.Fatalf("stale Acquire 等待到 queue timeout 才失败，期望立即拒绝：waitMs=%d", got.waitMs)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("stale Acquire 未立即拒绝：%s", elapsed)
	}
	m.mu.Lock()
	_, exists := m.queues["deleted"]
	m.mu.Unlock()
	if exists {
		t.Fatal("stale Acquire 重新创建了已删除 provider 的 map entry")
	}
}

func TestReconcileReAddBeforeDrainReusesRetiredQueue(t *testing.T) {
	m := NewManager()
	releaseRunning := mustAcquire(t, m, "re-add", 1, 0, 0)
	m.mu.Lock()
	originalQueue := m.queues["re-add"]
	m.mu.Unlock()

	waiting := acquireAsync(context.Background(), m, "re-add", 1, 0, 3000)
	waitForStatus(t, m, "re-add", 1, 0, func(status Status) bool {
		return status.Running == 1 && status.Queued == 1
	})
	m.Reconcile(map[string]Limits{})

	startedAt := time.Now()
	stale := awaitAcquire(t, acquireAsync(context.Background(), m, "re-add", 99, 0, 80), 300*time.Millisecond)
	if stale.err == nil {
		stale.release()
		t.Fatal("retired provider 的 stale Acquire 应被拒绝")
	}
	if errors.Is(stale.err, ErrQueueTimeout) || time.Since(startedAt) > 60*time.Millisecond {
		t.Fatalf("retired provider 的 stale Acquire 未立即拒绝：err=%v waitMs=%d", stale.err, stale.waitMs)
	}

	m.Reconcile(map[string]Limits{
		"re-add": {MaxConcurrent: 2, MaxPerSecond: 0},
	})
	m.mu.Lock()
	queueAfterReAdd := m.queues["re-add"]
	m.mu.Unlock()
	if queueAfterReAdd != originalQueue {
		t.Fatal("排空前重新加入 provider 时没有复用 retired 队列")
	}
	gotWaiting := awaitAcquire(t, waiting, 300*time.Millisecond)
	if gotWaiting.err != nil {
		t.Fatalf("重新加入后原 FIFO waiter Acquire() error = %v", gotWaiting.err)
	}

	releaseRunning()
	gotWaiting.release()
}

func TestReconcileReAddAfterDrainUsesRegistryLimitsForStaleFirstAcquire(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{
		"recycled": {MaxConcurrent: 2, MaxPerSecond: 0},
	})
	releaseOld := mustAcquire(t, m, "recycled", 2, 0, 0)
	releaseOld()
	m.Reconcile(map[string]Limits{})
	m.mu.Lock()
	_, existsAfterDelete := m.queues["recycled"]
	m.mu.Unlock()
	if existsAfterDelete {
		t.Fatal("测试前置失败：旧 queue 未在删除后回收")
	}

	m.Reconcile(map[string]Limits{
		"recycled": {MaxConcurrent: 1, MaxPerSecond: 1},
	})
	// 模拟延迟旧请求先于当前配置请求到达，携带显著放宽的旧 limits。
	releaseFirst := mustAcquire(t, m, "recycled", 9, 0, 0)
	status := m.StatusOf("recycled", 99, 99)
	if status.MaxConcurrent != 1 || status.MaxPerSecond != 1 {
		t.Errorf("stale 首请求创建的 queue limits = %d/%d，期望 registry 权威值 1/1", status.MaxConcurrent, status.MaxPerSecond)
	}

	second := acquireAsync(context.Background(), m, "recycled", 9, 0, 180)
	select {
	case got := <-second:
		if got.release != nil {
			got.release()
		}
		releaseFirst()
		t.Fatalf("第二请求绕过 registry 并发限制：err=%v", got.err)
	case <-time.After(50 * time.Millisecond):
	}
	waitForStatus(t, m, "recycled", 99, 99, func(status Status) bool {
		return status.Running == 1 && status.Queued == 1
	})

	releaseFirst()
	gotSecond := awaitAcquire(t, second, 300*time.Millisecond)
	if !errors.Is(gotSecond.err, ErrQueueTimeout) {
		if gotSecond.release != nil {
			gotSecond.release()
		}
		t.Fatalf("第二请求释放并发槽后未受 registry 速率限制：err=%v", gotSecond.err)
	}
}

func TestUpdateProviderUpdatesRegistryBeforeQueueCreation(t *testing.T) {
	m := NewManager()
	m.Reconcile(map[string]Limits{
		"registry-update": {MaxConcurrent: 1, MaxPerSecond: 1},
	})
	m.UpdateProvider("registry-update", 2, 3)

	release := mustAcquire(t, m, "registry-update", 9, 0, 0)
	defer release()
	status := m.StatusOf("registry-update", 99, 99)
	if status.MaxConcurrent != 2 || status.MaxPerSecond != 3 {
		t.Fatalf("UpdateProvider 后 stale 首请求创建的 limits = %d/%d，期望 registry 值 2/3", status.MaxConcurrent, status.MaxPerSecond)
	}
}
