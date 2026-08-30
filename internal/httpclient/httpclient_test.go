package httpclient

import (
	"sync"
	"testing"
)

func TestPoolForCachesNormalizedProxy(t *testing.T) {
	pool := NewPool()
	first, err := pool.For("http://proxy.example.com:7890")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.For("http://proxy.example.com:7890/")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("同一规范化代理没有复用同一个 client")
	}
}

func TestPoolForEmptyReturnsDefaultPointer(t *testing.T) {
	pool := NewPool()
	got, err := pool.For("")
	if err != nil {
		t.Fatal(err)
	}
	if got != pool.Default() {
		t.Fatal("For(\"\") 没有返回 Default 的同一个指针")
	}
}

func TestPoolForInvalidProxyDoesNotFallBackToDefault(t *testing.T) {
	pool := NewPool()
	// 用 ftp:// 当反例：socks5h 已实测被 Go 接受，属于白名单内的合法值。
	got, err := pool.For("ftp://proxy.example.com:1080")
	if err == nil {
		t.Fatal("For() error = nil, want invalid proxy error")
	}
	if got != nil {
		t.Fatalf("For() client = %p, want nil instead of Default %p", got, pool.Default())
	}
}

func TestPoolReconcileDropsUnusedClient(t *testing.T) {
	pool := NewPool()
	oldClient, err := pool.For("http://old.example.com:7890")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.For("http://active.example.com:7890"); err != nil {
		t.Fatal(err)
	}

	pool.Reconcile(map[string]struct{}{"http://active.example.com:7890": {}})
	newClient, err := pool.For("http://old.example.com:7890")
	if err != nil {
		t.Fatal(err)
	}
	if oldClient == newClient {
		t.Fatal("Reconcile 后被丢弃的代理仍复用了旧 client")
	}
}

func TestPoolForConcurrent(t *testing.T) {
	pool := NewPool()
	const workers = 64
	clients := make(chan any, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := pool.For("http://proxy.example.com:7890")
			if err != nil {
				clients <- err
				return
			}
			clients <- client
		}()
	}
	wg.Wait()
	close(clients)

	var want any
	for got := range clients {
		if err, ok := got.(error); ok {
			t.Fatal(err)
		}
		if want == nil {
			want = got
			continue
		}
		if got != want {
			t.Fatal("并发 For 返回了不同 client")
		}
	}
}
