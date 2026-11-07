package providermodels

import (
	"context"
	"sync"
	"time"
)

const CacheTTL = 5 * time.Minute

// Cache 按提供商保存有界目录；配置变化由 Reconcile 清理，账户变化由 identity 隔离。
// identity 必须可比较，不允许含秘密，也不对外序列化。
type Cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	connection string
	identity   any
	result     *Result
	at         time.Time
	failure    *QueryError
	retryUntil time.Time
	busy       chan struct{}
}

// Lease 串行化同一个提供商的目录请求，等待可以取消。
// finish 只能调用一次；查询失败传 nil，不覆盖之前的成功目录。
func (c *Cache) Lease(ctx context.Context, provider, connection string) (finish func(any, *Result), err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]*cacheEntry)
		}
		entry := c.entries[provider]
		if entry == nil || entry.connection != connection {
			entry = &cacheEntry{connection: connection}
			c.entries[provider] = entry
		}
		if entry.busy != nil {
			done := entry.busy
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		entry.busy = make(chan struct{})
		done := entry.busy
		c.mu.Unlock()
		var once sync.Once
		return func(identity any, result *Result) {
			once.Do(func() {
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.entries[provider] == entry && result != nil {
					entry.identity = identity
					entry.result = cloneResult(result)
					entry.at = time.Now()
					entry.failure = nil
					entry.retryUntil = time.Time{}
				}
				entry.busy = nil
				close(done)
			})
		}, nil
	}
}

// Lookup 返回副本，过期结果仅供查询失败时标记为旧目录。
func (c *Cache) Lookup(provider, connection string, identity any) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[provider]
	if e == nil || e.connection != connection || e.identity != identity || e.result == nil {
		return nil, false
	}
	return cloneResult(e.result), e.failure == nil && time.Since(e.at) < CacheTTL
}

// Failure 返回同身份最近失败与重试等待。错误副本不会暴露缓存内部可变指针。
func (c *Cache) Failure(provider, connection string, identity any) (*QueryError, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[provider]
	if e == nil || e.connection != connection || e.identity != identity || e.failure == nil {
		return nil, 0
	}
	copy := *e.failure
	remaining := time.Until(e.retryUntil)
	if remaining < 0 {
		remaining = 0
	}
	return &copy, remaining
}

// RecordFailure 保留同身份的成功目录及原时间；新身份失败不能继承旧目录。
// 调用方持有 Lease，配置移除后的迟到错误不重新创建缓存。
func (c *Cache) RecordFailure(provider, connection string, identity any, failure *QueryError) {
	if failure == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[provider]
	if e == nil || e.connection != connection {
		return
	}
	if e.identity != identity {
		e.result = nil
		e.at = time.Time{}
	}
	e.identity = identity
	copy := *failure
	e.failure = &copy
	wait := failure.RetryAfter
	if wait < 30*time.Second {
		wait = 30 * time.Second
	}
	e.retryUntil = time.Now().Add(wait)
}

// Reconcile 不创建新条目，只保留仍引用同一连接设置的提供商。
func (c *Cache) Reconcile(connections map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, entry := range c.entries {
		if connection, ok := connections[name]; !ok || connection != entry.connection {
			delete(c.entries, name)
		}
	}
}

func cloneResult(source *Result) *Result {
	if source == nil {
		return nil
	}
	out := *source
	out.Models = append([]string{}, source.Models...)
	return &out
}
