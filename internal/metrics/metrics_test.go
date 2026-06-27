package metrics

import (
	"testing"
	"time"
)

func TestCollectorLogsFiltersAndRing(t *testing.T) {
	c := NewCollector(2)
	base := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	c.Add(RequestLog{ID: "old", Started: base, Status: 200, Provider: "a", Model: "m1", DurationMs: 10})
	c.Add(RequestLog{ID: "keep1", Started: base.Add(time.Second), Status: 500, Provider: "b", Model: "m2", DurationMs: 20, Error: "boom"})
	c.Add(RequestLog{ID: "keep2", Started: base.Add(2 * time.Second), Status: 200, Provider: "b", Model: "m3", Route: "claude-*", TargetModel: "claude-sonnet-4", DurationMs: 30})

	all := c.Logs(LogFilter{Limit: 10})
	if len(all) != 2 {
		t.Fatalf("expected 2 logs after ring overwrite, got %d", len(all))
	}
	if all[0].ID != "keep2" || all[1].ID != "keep1" {
		t.Fatalf("logs should be newest first, got %#v", all)
	}

	errors := c.Logs(LogFilter{Status: "error", Limit: 10})
	if len(errors) != 1 || errors[0].ID != "keep1" {
		t.Fatalf("expected only error log keep1, got %#v", errors)
	}

	provider := c.Logs(LogFilter{Provider: "b", Query: "m3", Limit: 10})
	if len(provider) != 1 || provider[0].ID != "keep2" {
		t.Fatalf("expected provider/query filter to return keep2, got %#v", provider)
	}

	targetModel := c.Logs(LogFilter{Model: "sonnet-4", Limit: 10})
	if len(targetModel) != 1 || targetModel[0].ID != "keep2" {
		t.Fatalf("expected target model filter to return keep2, got %#v", targetModel)
	}
}

func TestCollectorMetrics(t *testing.T) {
	c := NewCollector(10)
	now := time.Date(2026, 6, 27, 10, 1, 0, 0, time.UTC)
	c.Add(RequestLog{ID: "a", Started: now.Add(-10 * time.Second), Status: 200, Provider: "p1", DurationMs: 10})
	c.Add(RequestLog{ID: "b", Started: now.Add(-9 * time.Second), Status: 200, Provider: "p1", DurationMs: 20, Error: "stream failed"})
	c.Add(RequestLog{ID: "c", Started: now.Add(-8 * time.Second), Status: 503, Provider: "p2", DurationMs: 100, Error: "upstream"})

	m := c.Metrics(now)
	if m.Summary.RequestsPerMinute != 3 {
		t.Fatalf("expected rpm 3, got %d", m.Summary.RequestsPerMinute)
	}
	if m.Summary.SuccessRate != 1.0/3.0 {
		t.Fatalf("unexpected success rate: %f", m.Summary.SuccessRate)
	}
	if m.Summary.P95LatencyMs != 100 {
		t.Fatalf("expected p95 100, got %d", m.Summary.P95LatencyMs)
	}
	if len(m.RecentErrors) != 2 || m.RecentErrors[0].ID != "c" || m.RecentErrors[1].ID != "b" {
		t.Fatalf("expected recent errors c and b, got %#v", m.RecentErrors)
	}
}

func TestCollectorMetricsKeepsSummaryRecent(t *testing.T) {
	c := NewCollector(10)
	now := time.Date(2026, 6, 27, 10, 2, 0, 0, time.UTC)
	c.Add(RequestLog{ID: "old", Started: now.Add(-2 * time.Minute), Status: 200, Provider: "p1", DurationMs: 75})

	m := c.Metrics(now)
	if m.Summary.RequestsPerMinute != 0 {
		t.Fatalf("expected empty recent window, got rpm %d", m.Summary.RequestsPerMinute)
	}
	if m.Summary.SuccessRate != 0 || m.Summary.P95LatencyMs != 0 {
		t.Fatalf("summary should not use stale history: %#v", m.Summary)
	}
	if len(m.Providers) != 1 || m.Providers[0].Name != "p1" {
		t.Fatalf("provider history should still be available, got %#v", m.Providers)
	}
}
