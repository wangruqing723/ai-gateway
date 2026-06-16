// Package queue 对齐 Node 版 lib/queue.js：per-provider 并发控制 + 每秒速率限制 + 队列等待超时。
//
// Node 版用单线程事件循环 + 手工维护 running/queue 数组；Go 版用带缓冲 channel 做并发信号量，
// 用互斥保护的时间戳滑动窗口做速率限制，天然不会有 slot 泄漏（defer 释放）。
package queue

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueTimeout 队列等待超时
var ErrQueueTimeout = errors.New("队列等待超时，请稍后重试")

// providerQueue 单个 provider 的队列状态
type providerQueue struct {
	sem          chan struct{} // 并发信号量，容量 = maxConcurrent
	maxPerSecond int

	mu           sync.Mutex
	requestTimes []time.Time // 最近 1 秒内的请求时间戳（速率限制滑动窗口）

	running int // 运行中计数（仅用于状态查询）
	waiting int // 排队中计数（仅用于状态查询）
}

// Manager 管理所有 provider 的队列
type Manager struct {
	mu     sync.Mutex
	queues map[string]*providerQueue
}

// NewManager 创建队列管理器
func NewManager() *Manager {
	return &Manager{queues: make(map[string]*providerQueue)}
}

// Status 队列状态快照，用于 /health
type Status struct {
	Running       int `json:"running"`
	Queued        int `json:"queued"`
	MaxConcurrent int `json:"maxConcurrent"`
	MaxPerSecond  int `json:"maxPerSecond"`
}

func (m *Manager) ensure(name string, maxConcurrent, maxPerSecond int) *providerQueue {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.queues[name]
	if !ok {
		if maxConcurrent <= 0 {
			maxConcurrent = 5
		}
		q = &providerQueue{
			sem:          make(chan struct{}, maxConcurrent),
			maxPerSecond: maxPerSecond,
		}
		m.queues[name] = q
	}
	return q
}

// Acquire 获取一个执行 slot。返回 release 函数，调用方必须 defer release()。
// 行为对齐 Node：先并发控制（最多等待 maxQueueWait），再速率限制（窗口满则等待到窗口滑出）。
// ctx 取消（如客户端断开）会提前返回。
func (m *Manager) Acquire(ctx context.Context, name string, maxConcurrent, maxPerSecond, maxQueueWaitMs int) (release func(), err error) {
	q := m.ensure(name, maxConcurrent, maxPerSecond)

	q.mu.Lock()
	q.waiting++
	q.mu.Unlock()

	// 1) 并发信号量：带队列等待超时
	var waitCh <-chan time.Time
	if maxQueueWaitMs > 0 {
		t := time.NewTimer(time.Duration(maxQueueWaitMs) * time.Millisecond)
		defer t.Stop()
		waitCh = t.C
	}

	select {
	case q.sem <- struct{}{}:
		// 拿到并发槽位
	case <-waitCh:
		q.mu.Lock()
		q.waiting--
		q.mu.Unlock()
		return nil, ErrQueueTimeout
	case <-ctx.Done():
		q.mu.Lock()
		q.waiting--
		q.mu.Unlock()
		return nil, ctx.Err()
	}

	q.mu.Lock()
	q.waiting--
	q.running++
	q.mu.Unlock()

	// 2) 速率限制：滑动窗口，必要时等待
	if err := q.waitForRate(ctx); err != nil {
		<-q.sem
		q.mu.Lock()
		q.running--
		q.mu.Unlock()
		return nil, err
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		<-q.sem
		q.mu.Lock()
		q.running--
		q.mu.Unlock()
	}, nil
}

// waitForRate 实现每秒最多 maxPerSecond 个请求的滑动窗口限速。
func (q *providerQueue) waitForRate(ctx context.Context) error {
	if q.maxPerSecond <= 0 {
		return nil
	}
	for {
		q.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-time.Second)
		// 清理 1 秒前的记录
		kept := q.requestTimes[:0]
		for _, t := range q.requestTimes {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		q.requestTimes = kept

		if len(q.requestTimes) < q.maxPerSecond {
			q.requestTimes = append(q.requestTimes, now)
			q.mu.Unlock()
			return nil
		}
		// 窗口已满，计算需等待到最早请求滑出窗口的时间
		oldest := q.requestTimes[0]
		delay := oldest.Add(time.Second + 10*time.Millisecond).Sub(now)
		q.mu.Unlock()

		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			// 继续循环重新检查
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// StatusOf 返回某 provider 的队列状态快照
func (m *Manager) StatusOf(name string, maxConcurrent, maxPerSecond int) Status {
	m.mu.Lock()
	q, ok := m.queues[name]
	m.mu.Unlock()
	if !ok {
		return Status{MaxConcurrent: maxConcurrent, MaxPerSecond: maxPerSecond}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return Status{
		Running:       q.running,
		Queued:        q.waiting,
		MaxConcurrent: cap(q.sem),
		MaxPerSecond:  q.maxPerSecond,
	}
}
