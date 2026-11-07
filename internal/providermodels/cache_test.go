package providermodels

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCacheIdentityCopyAndReconcile(t *testing.T) {
	var cache Cache
	finish, err := cache.Lease(context.Background(), "p", "connection")
	if err != nil {
		t.Fatal(err)
	}
	finish(uint64(1), &Result{Models: []string{"model"}})
	result, fresh := cache.Lookup("p", "connection", uint64(1))
	if !fresh || result == nil {
		t.Fatal("缓存未保存")
	}
	result.Models[0] = "changed"
	result, _ = cache.Lookup("p", "connection", uint64(1))
	if result.Models[0] != "model" {
		t.Fatal("缓存泄露了可变切片")
	}
	if result, _ := cache.Lookup("p", "connection", uint64(2)); result != nil {
		t.Fatal("不同凭据世代串缓存")
	}
	cache.Reconcile(map[string]string{"p": "other"})
	if result, _ := cache.Lookup("p", "connection", uint64(1)); result != nil {
		t.Fatal("配置变化没有清缓存")
	}
}

func TestCacheCancelledWaitAndLateResult(t *testing.T) {
	var cache Cache
	finish, _ := cache.Lease(context.Background(), "p", "old")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := cache.Lease(ctx, "p", "old"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等待应可取消: %v", err)
	}
	cache.Reconcile(map[string]string{"p": "new"})
	newFinish, _ := cache.Lease(context.Background(), "p", "new")
	newFinish(uint64(2), &Result{Models: []string{"new"}})
	finish(uint64(1), &Result{Models: []string{"old"}})
	finish(uint64(1), nil)
	if result, _ := cache.Lookup("p", "new", uint64(2)); result == nil || result.Models[0] != "new" {
		t.Fatal("迟到结果覆盖新配置")
	}
	finish, _ = cache.Lease(context.Background(), "p", "new")
	finish(uint64(2), nil)
	if result, _ := cache.Lookup("p", "new", uint64(2)); result == nil {
		t.Fatal("查询失败丢失旧目录")
	}
}

func TestCacheFailurePreservesTimeAndHonorsRetryAfter(t *testing.T) {
	var cache Cache
	finish, _ := cache.Lease(context.Background(), "p", "c")
	finish(uint64(1), &Result{Models: []string{"old"}, QueriedAt: "2030-01-01T00:00:00Z"})
	finish, _ = cache.Lease(context.Background(), "p", "c")
	cache.RecordFailure("p", "c", uint64(1), &QueryError{Code: "rate_limited", Message: "限流", RetryAfter: 48 * time.Hour})
	finish(nil, nil)
	old, fresh := cache.Lookup("p", "c", uint64(1))
	if old == nil || fresh || old.QueriedAt != "2030-01-01T00:00:00Z" {
		t.Fatal("失败应保留旧目录时间且标为过时")
	}
	failure, wait := cache.Failure("p", "c", uint64(1))
	if failure == nil || wait < 47*time.Hour {
		t.Fatal("长Retry-After被缩短")
	}
	failure.Message = "被修改"
	again, _ := cache.Failure("p", "c", uint64(1))
	if again.Message != "限流" {
		t.Fatal("错误对象未隔离")
	}
	cache.RecordFailure("p", "c", uint64(2), &QueryError{Code: "upstream_error", Message: "失败"})
	if old, _ := cache.Lookup("p", "c", uint64(1)); old != nil {
		t.Fatal("新身份失败继承了旧目录")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Lease(ctx, "p", "c"); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消查询仍获得目录执行权")
	}
}
