package chatgptauth

import (
	"context"
	"sync"
)

// contextMutex 保留按账户串行化，但等待锁时可被请求取消或时间预算中止。
// 零值可直接使用；只在持锁者完成后释放，不为取消的等待者另起 goroutine。
type contextMutex struct {
	once sync.Once
	slot chan struct{}
}

func (m *contextMutex) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.once.Do(func() { m.slot = make(chan struct{}, 1) })
	select {
	case m.slot <- struct{}{}:
		// 取消和授予锁同时就绪时，以取消为准，不能继续交换刷新凭据。
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryLock 只在锁空闲时立即取得，不等待。
func (m *contextMutex) TryLock() bool {
	m.once.Do(func() { m.slot = make(chan struct{}, 1) })
	select {
	case m.slot <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *contextMutex) Unlock() { <-m.slot }
