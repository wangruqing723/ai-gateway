package metrics

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const eventLogCapacity = 100

// Event 是熔断器、Provider 健康、密钥或 ChatGPT OAuth 账户状态变化的被动观测事件。
type Event struct {
	ID       string `json:"id"`
	Time     string `json:"time"`
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	// AccountIDs 是事件涉及的本地 OAuth 账户 ID；单账户事件只有一项，批量操作合并为一条事件。
	AccountIDs []string `json:"accountIds,omitempty"`
	// Account 是供页面展示的账户名称（邮箱或「N 个账户」），不含凭据。
	Account   string `json:"account,omitempty"`
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
	e.AddEvent(Event{Kind: kind, Provider: provider, Detail: detail})
}

// AddEvent 追加一条带可选账户维度的事件。ID、时间和恢复标记由事件流统一生成，
// 调用方传入的同名字段会被覆盖。
func (e *EventLog) AddEvent(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cap(e.items) == 0 {
		e.items = make([]Event, 0, eventLogCapacity)
	}
	e.serial++
	event.ID = fmt.Sprintf("e%d", e.serial)
	event.Time = time.Now().Format("2006-01-02 15:04:05")
	// 所有恢复类事件统一以 _recovered 结尾：breaker、health、key 与账户事件共用这一约定。
	event.Recovered = strings.HasSuffix(event.Kind, "_recovered")
	event.AccountIDs = append([]string(nil), event.AccountIDs...)
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

// AccountEvents 返回含账户维度的事件副本，按时间倒序。accountID 为空时返回全部账户事件；
// limit 不大于 0 表示不限制条数。
func (e *EventLog) AccountEvents(accountID string, limit int) []Event {
	out := []Event{}
	for _, event := range e.Events() {
		if len(event.AccountIDs) == 0 {
			continue
		}
		if accountID != "" {
			found := false
			for _, id := range event.AccountIDs {
				if id == accountID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, event)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
