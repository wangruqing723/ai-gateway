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
}
