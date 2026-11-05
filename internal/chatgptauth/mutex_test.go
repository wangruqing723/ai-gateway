package chatgptauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContextMutexCancellationDoesNotTakeHeldLock(t *testing.T) {
	var lock contextMutex
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lock.Lock(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("锁取消 = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("持锁期间取消无法返回")
	}
	lock.Unlock()
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := lock.Lock(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消仍取锁 = %v", err)
	}
}

func TestContextMutexDeadlineWhileWaiting(t *testing.T) {
	var lock contextMutex
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lock.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("锁超时 = %v", err)
	}
}

func TestDiagnoseCancelsWhileAnotherAccountOperationHoldsLock(t *testing.T) {
	for _, kind := range []string{"refresh", "usage"} {
		t.Run(kind, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.addAccount("remote-held", "held@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
			env.m.mu.RLock()
			item := env.m.accounts[id]
			env.m.mu.RUnlock()
			lock := &item.refreshMu
			if kind == "usage" {
				lock = &item.usageMu
			}
			if err := lock.Lock(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer lock.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			var result CheckOutcome
			if kind == "usage" {
				result = env.m.DiagnoseUsage(ctx, id)
			} else {
				result = env.m.DiagnoseRefresh(ctx, id, true)
			}
			if result.Category != "timeout" || result.Exchanged {
				t.Fatalf("等待取消 = %#v", result)
			}
			view, _ := env.m.Account(id)
			if view.Usage != nil || view.Authorization.LastRefreshError != "" || env.up.tokenCalls.Load()+env.up.usageCalls.Load() != 0 {
				t.Fatal("等待锁超时不应出网或污染账户状态")
			}
		})
	}
}
