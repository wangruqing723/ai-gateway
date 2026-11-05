package metrics

import "testing"

func TestEventLogKeepsNewestHundredEvents(t *testing.T) {
	log := NewEventLog()
	for i := 1; i <= eventLogCapacity+5; i++ {
		kind := "breaker_open"
		if i == eventLogCapacity {
			kind = "health_recovered"
		}
		log.Add(kind, "provider", "detail")
	}

	got := log.Events()
	if len(got) != eventLogCapacity {
		t.Fatalf("event count = %d, want %d", len(got), eventLogCapacity)
	}
	if got[0].ID != "e105" || got[len(got)-1].ID != "e6" {
		t.Fatalf("event order/ring coverage = first %#v, last %#v", got[0], got[len(got)-1])
	}
	for _, event := range got {
		if event.ID == "e100" {
			if !event.Recovered || event.Kind != "health_recovered" {
				t.Fatalf("recovery marker = %#v", event)
			}
			break
		}
	}

	log.Add("health_recovered", "provider", "recovered")
	if got := log.Events()[0]; !got.Recovered || got.Kind != "health_recovered" {
		t.Fatalf("recovery event = %#v", got)
	}

	log.Add("key_recovered", "provider", "key recovered")
	if got := log.Events()[0]; !got.Recovered || got.Kind != "key_recovered" {
		t.Fatalf("key recovery event = %#v", got)
	}
}

func TestEventLogAccountDimension(t *testing.T) {
	log := NewEventLog()
	ids := []string{"acc_a", "acc_b"}
	log.AddEvent(Event{ID: "forged", Time: "forged", Recovered: true, Kind: "account_paused", AccountIDs: ids, Account: "2 个账户", Detail: "批量暂停"})
	ids[0] = "mutated"
	log.Add("breaker_open", "codex", "熔断")
	log.AddEvent(Event{Kind: "account_refresh_recovered", AccountIDs: []string{"acc_a"}, Account: "a@example.invalid"})
	log.AddEvent(Event{Kind: "account_binding_changed", Provider: "codex", AccountIDs: []string{"acc_c"}})

	events := log.Events()
	paused := events[len(events)-1]
	if paused.ID != "e1" || paused.Time == "forged" || paused.Recovered {
		t.Fatalf("ID、时间与恢复标记应由事件流生成: %#v", paused)
	}
	if paused.AccountIDs[0] != "acc_a" {
		t.Fatalf("事件应复制调用方的账户切片: %#v", paused.AccountIDs)
	}
	if !events[1].Recovered || events[1].Kind != "account_refresh_recovered" {
		t.Fatalf("账户恢复事件 = %#v", events[1])
	}

	all := log.AccountEvents("", 0)
	if len(all) != 3 || all[0].Kind != "account_binding_changed" || all[2].Kind != "account_paused" {
		t.Fatalf("全部账户事件 = %#v", all)
	}
	forA := log.AccountEvents("acc_a", 0)
	if len(forA) != 2 || forA[0].Kind != "account_refresh_recovered" || forA[1].Kind != "account_paused" {
		t.Fatalf("账户 acc_a 的事件 = %#v", forA)
	}
	if limited := log.AccountEvents("acc_a", 1); len(limited) != 1 || limited[0].Kind != "account_refresh_recovered" {
		t.Fatalf("限制条数 = %#v", limited)
	}
	if none := log.AccountEvents("acc_missing", 0); none == nil || len(none) != 0 {
		t.Fatalf("没有事件时应返回空切片: %#v", none)
	}
	var nilLog *EventLog
	if got := nilLog.AccountEvents("", 0); got == nil || len(got) != 0 {
		t.Fatalf("nil 事件流 = %#v", got)
	}
	nilLog.AddEvent(Event{Kind: "account_paused"})
}
