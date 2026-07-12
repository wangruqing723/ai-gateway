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

func TestCollectorMetricsAreIndependentFromLogCapacity(t *testing.T) {
	c := NewCollector(10)
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 1500; i++ {
		status := 200
		errText := ""
		if i%5 == 0 {
			status = 503
			errText = "upstream"
		}
		c.Add(RequestLog{
			ID:         "request",
			Started:    now.Add(-10 * time.Second),
			Status:     status,
			Provider:   "provider-a",
			DurationMs: int64((i % 100) + 1),
			Error:      errText,
		})
	}

	m := c.Metrics(now)
	if m.Summary.TotalRequests != 1500 {
		t.Fatalf("total requests must not be capped by the log ring: %d", m.Summary.TotalRequests)
	}
	if m.Summary.RequestsPerMinute != 1500 {
		t.Fatalf("rpm must not be capped by the log ring: %d", m.Summary.RequestsPerMinute)
	}
	if m.StatusCodes["200"] != 1200 || m.StatusCodes["503"] != 300 {
		t.Fatalf("unexpected status aggregation: %#v", m.StatusCodes)
	}
	if m.Summary.SuccessRate != 0.8 {
		t.Fatalf("unexpected success rate: %f", m.Summary.SuccessRate)
	}
	if len(m.Providers) != 1 || m.Providers[0].Requests != 1500 || m.Providers[0].Errors != 300 {
		t.Fatalf("provider aggregation must include every request: %#v", m.Providers)
	}
	if m.Summary.P50LatencyMs != 50 || m.Summary.P95LatencyMs != 100 || m.Summary.P99LatencyMs != 100 {
		t.Fatalf("summary histogram must include every request: %#v", m.Summary)
	}
	provider := m.Providers[0]
	if provider.P50LatencyMs != 50 || provider.P95LatencyMs != 100 || provider.P99LatencyMs != 100 {
		t.Fatalf("provider histogram must include every request: %#v", provider)
	}
	if logs := c.Logs(LogFilter{Limit: 100}); len(logs) != 10 {
		t.Fatalf("log ring should remain bounded, got %d", len(logs))
	}
}

func TestCollectorMetricsMinuteBoundaryAndZeroStarted(t *testing.T) {
	t.Run("minute boundary", func(t *testing.T) {
		c := NewCollector(10)
		now := time.Now().Truncate(time.Second)
		c.Add(RequestLog{ID: "included", Started: now.Add(-60 * time.Second), Status: 200, Provider: "p", DurationMs: 10})
		c.Add(RequestLog{ID: "excluded", Started: now.Add(-61 * time.Second), Status: 503, Provider: "p", DurationMs: 20})

		m := c.Metrics(now)
		if m.Summary.TotalRequests != 2 || m.Summary.RequestsPerMinute != 1 {
			t.Fatalf("minute boundary totals = %#v", m.Summary)
		}
		if m.StatusCodes["200"] != 1 || m.StatusCodes["503"] != 0 {
			t.Fatalf("minute boundary status codes = %#v", m.StatusCodes)
		}
	})

	t.Run("zero started", func(t *testing.T) {
		c := NewCollector(10)
		c.Add(RequestLog{ID: "zero", Status: 201, Provider: "p", DurationMs: 10})
		logs := c.Logs(LogFilter{Limit: 10})
		if len(logs) != 1 || logs[0].Started.IsZero() || logs[0].StartedAt == "" {
			t.Fatalf("zero Started was not normalized: %#v", logs)
		}
		m := c.Metrics(time.Now())
		if m.Summary.TotalRequests != 1 || m.Summary.RequestsPerMinute != 1 || m.StatusCodes["201"] != 1 {
			t.Fatalf("normalized zero Started missing from metrics: %#v", m)
		}
	})
}

func TestCollectorMetricsWindowExpiresWithoutResettingTotal(t *testing.T) {
	c := NewCollector(1)
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	c.Add(RequestLog{ID: "old", Started: now.Add(-2 * time.Minute), Status: 200, Provider: "p", DurationMs: 10})
	c.Add(RequestLog{ID: "recent", Started: now.Add(-5 * time.Second), Status: 500, Provider: "p", DurationMs: 20, Error: "failed"})

	m := c.Metrics(now.Add(time.Minute))
	if m.Summary.TotalRequests != 2 {
		t.Fatalf("expected cumulative total 2, got %d", m.Summary.TotalRequests)
	}
	if m.Summary.RequestsPerMinute != 0 || len(m.StatusCodes) != 0 {
		t.Fatalf("expired minute buckets must be empty: %#v", m)
	}
}

func TestCollectorLateCompletionDoesNotEraseNewerBucket(t *testing.T) {
	c := NewCollector(10)
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)

	// 两条请求的开始时间相差 61 秒，落入同一个环形槽；新请求先完成并上报。
	c.Add(RequestLog{ID: "new", Started: now, Status: 200, Provider: "p", DurationMs: 10})
	c.Add(RequestLog{ID: "slow", Started: now.Add(-61 * time.Second), Status: 200, Provider: "p", DurationMs: 61_000})

	m := c.Metrics(now)
	if m.Summary.TotalRequests != 2 {
		t.Fatalf("expected cumulative total 2, got %d", m.Summary.TotalRequests)
	}
	if m.Summary.RequestsPerMinute != 1 || m.StatusCodes["200"] != 1 {
		t.Fatalf("late completion must not erase the newer metric bucket: %#v", m)
	}
}

func TestCollectorFutureTimestampDoesNotPoisonCurrentBucket(t *testing.T) {
	c := NewCollector(10)
	now := time.Now().Truncate(time.Second)

	// 未来记录不属于当前窗口，也不能抢占 61 秒前后共用的环形槽。
	c.Add(RequestLog{ID: "future", Started: now.Add(61 * time.Second), Status: 503, Provider: "p", DurationMs: 10})
	c.Add(RequestLog{ID: "current", Started: now, Status: 200, Provider: "p", DurationMs: 10})

	m := c.Metrics(now)
	if m.Summary.TotalRequests != 2 {
		t.Fatalf("expected cumulative total 2, got %d", m.Summary.TotalRequests)
	}
	if m.Summary.RequestsPerMinute != 1 || m.StatusCodes["200"] != 1 || m.StatusCodes["503"] != 0 {
		t.Fatalf("future timestamp must not poison the current metric bucket: %#v", m)
	}
}
