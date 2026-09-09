package metrics

import (
	"fmt"
	"sync"
	"time"
)

const eventLogCapacity = 100

// Event 是熔断器或 Provider 健康状态变化的被动观测事件。
type Event struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	Kind      string `json:"kind"`
	Provider  string `json:"provider"`
	Detail    string `json:"detail"`
	Recovered bool   `json:"recovered"`
}

// EventLog 保存最近的运行状态事件，容量固定为 100 条。
type EventLog struct {
	mu     sync.RWMutex
	items  []Event
	next   int
	full   bool
	serial uint64
}

// NewEventLog 创建固定容量的事件环形缓冲。
func NewEventLog() *EventLog {
	return &EventLog{items: make([]Event, 0, eventLogCapacity)}
}

// Add 追加一条事件。恢复事件由 kind 的稳定命名约定自动标记。
func (e *EventLog) Add(kind, provider, detail string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cap(e.items) == 0 {
		e.items = make([]Event, 0, eventLogCapacity)
	}
	e.serial++
	event := Event{
		ID:        fmt.Sprintf("e%d", e.serial),
		Time:      time.Now().Format("2006-01-02 15:04:05"),
		Kind:      kind,
		Provider:  provider,
		Detail:    detail,
		Recovered: kind == "breaker_recovered" || kind == "health_recovered",
	}
	if len(e.items) < cap(e.items) {
		e.items = append(e.items, event)
		return
	}
	e.items[e.next] = event
	e.next = (e.next + 1) % len(e.items)
	e.full = true
}

// Events 返回按时间倒序排列的事件副本。
func (e *EventLog) Events() []Event {
	if e == nil {
		return []Event{}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	count := len(e.items)
	out := make([]Event, 0, count)
	for offset := 0; offset < count; offset++ {
		index := count - 1 - offset
		if e.full {
			index = (e.next - 1 - offset) % count
			if index < 0 {
				index += count
			}
		}
		out = append(out, e.items[index])
	}
	return out
}
