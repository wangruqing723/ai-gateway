package breaker

import (
	"sync"
	"testing"
	"time"
)

// newTestBreaker 构造带可控时钟的熔断器，避免测试依赖真实等待。
func newTestBreaker(s Settings) (*Breaker, func(time.Duration)) {
	b := New(s)
	var mu sync.Mutex
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	return b, advance
}

func defaultTestSettings() Settings {
	return Settings{Enabled: true, ConsecutiveFailures: 3, OpenMs: 30_000, HalfOpenProbes: 1}
}

func TestDisabledBreakerAlwaysAllows(t *testing.T) {
	b, _ := newTestBreaker(Settings{Enabled: false, ConsecutiveFailures: 1})
	for i := 0; i < 10; i++ {
		b.Report("p1", OutcomeFailure)
	}
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("禁用时 Allow 必须恒为 true")
	}
	if snap := b.Snapshot(); snap != nil {
		t.Fatalf("禁用时 Snapshot 应为 nil，实际 %#v", snap)
	}
}

func TestOpensOnlyAfterConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker(defaultTestSettings())

	// 阈值前一次仍应放行：错误率类判据会在首次失败就触发（LiteLLM #17418），这里用连续计数避免
	for i := 0; i < 2; i++ {
		b.Report("p1", OutcomeFailure)
		if allowed, _ := b.Allow("p1"); !allowed {
			t.Fatalf("第 %d 次失败后就熔断，早于阈值 3", i+1)
		}
	}

	b.Report("p1", OutcomeFailure)
	allowed, retryAfter := b.Allow("p1")
	if allowed {
		t.Fatal("连续 3 次失败后应开路")
	}
	if retryAfter <= 0 || retryAfter > 30*time.Second {
		t.Fatalf("retryAfter = %s，期望落在 0-30s 内", retryAfter)
	}
	if got := b.Snapshot()["p1"].State; got != StateOpen {
		t.Fatalf("state = %q，期望 %q", got, StateOpen)
	}
}

func TestSuccessResetsFailureStreak(t *testing.T) {
	b, _ := newTestBreaker(defaultTestSettings())
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeSuccess)
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeFailure)

	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("成功应清零连续失败计数，不该在此处开路")
	}
	if got := b.Snapshot()["p1"].ConsecutiveFailures; got != 2 {
		t.Fatalf("consecutiveFailures = %d，期望 2", got)
	}
}

func TestIgnoredOutcomesNeverOpen(t *testing.T) {
	// 429 与 401/403 在调用侧映射为 OutcomeIgnored：
	// 429 是上游正常限流，401/403 是配置问题，熔断都修不了。
	b, _ := newTestBreaker(defaultTestSettings())
	for i := 0; i < 20; i++ {
		b.Report("p1", OutcomeIgnored)
	}
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("OutcomeIgnored 不应触发熔断")
	}
	if got := b.Snapshot()["p1"].ConsecutiveFailures; got != 0 {
		t.Fatalf("consecutiveFailures = %d，期望 0", got)
	}
}

func TestOpenToHalfOpenAfterCooldown(t *testing.T) {
	b, advance := newTestBreaker(defaultTestSettings())
	for i := 0; i < 3; i++ {
		b.Report("p1", OutcomeFailure)
	}

	advance(29 * time.Second)
	if allowed, retryAfter := b.Allow("p1"); allowed {
		t.Fatal("冷却未结束就放行")
	} else if retryAfter <= 0 {
		t.Fatalf("retryAfter = %s，冷却中应为正值", retryAfter)
	}

	advance(2 * time.Second)
	allowed, _ := b.Allow("p1")
	if !allowed {
		t.Fatal("冷却结束后应放行一个半开探针")
	}
	if got := b.Snapshot()["p1"].State; got != StateHalfOpen {
		t.Fatalf("state = %q，期望 %q", got, StateHalfOpen)
	}

	// 探针额度用满后，后续请求应去别的候选而不是在此排队
	if allowed, retryAfter := b.Allow("p1"); allowed {
		t.Fatal("halfOpenProbes=1 时不应放行第二个探针")
	} else if retryAfter != 0 {
		t.Fatalf("探针额度满时 retryAfter = %s，期望 0", retryAfter)
	}
}

func TestHalfOpenSuccessClosesBreaker(t *testing.T) {
	b, advance := newTestBreaker(defaultTestSettings())
	for i := 0; i < 3; i++ {
		b.Report("p1", OutcomeFailure)
	}
	advance(31 * time.Second)
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("冷却结束应放行探针")
	}

	b.Report("p1", OutcomeSuccess)
	state := b.Snapshot()["p1"]
	if state.State != StateClosed {
		t.Fatalf("state = %q，期望 %q", state.State, StateClosed)
	}
	if state.ConsecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d，期望 0", state.ConsecutiveFailures)
	}
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("闭合后应恒放行")
	}
}

func TestHalfOpenFailureReopensWithFreshCooldown(t *testing.T) {
	b, advance := newTestBreaker(defaultTestSettings())
	for i := 0; i < 3; i++ {
		b.Report("p1", OutcomeFailure)
	}
	advance(31 * time.Second)
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("冷却结束应放行探针")
	}

	b.Report("p1", OutcomeFailure)
	state := b.Snapshot()["p1"]
	if state.State != StateOpen {
		t.Fatalf("state = %q，期望重新开路", state.State)
	}
	if state.TotalOpens != 2 {
		t.Fatalf("totalOpens = %d，期望 2（首次 + 探测失败）", state.TotalOpens)
	}
	// 冷却应从探测失败时刻重算，而不是沿用首次开路时间
	advance(29 * time.Second)
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatal("探测失败后冷却应重新计时")
	}
	advance(2 * time.Second)
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("新一轮冷却结束后应再放探针")
	}
}

func TestHalfOpenProbesRespectsLimit(t *testing.T) {
	b, advance := newTestBreaker(Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 1_000, HalfOpenProbes: 3})
	b.Report("p1", OutcomeFailure)
	advance(2 * time.Second)

	for i := 0; i < 3; i++ {
		if allowed, _ := b.Allow("p1"); !allowed {
			t.Fatalf("第 %d 个探针应被放行（上限 3）", i+1)
		}
	}
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatal("超出 halfOpenProbes 的请求不应放行")
	}
	if got := b.Snapshot()["p1"].ProbesInFlight; got != 3 {
		t.Fatalf("probesInFlight = %d，期望 3", got)
	}
}

func TestIgnoredOutcomeReleasesProbeSlot(t *testing.T) {
	// 构建失败等未真实发起请求的场景会汇报 OutcomeIgnored，
	// 必须归还探针额度，否则半开状态会被一个没结论的请求永久占住。
	b, advance := newTestBreaker(defaultTestSettings())
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeFailure)
	advance(31 * time.Second)
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("冷却结束应放行探针")
	}

	b.Report("p1", OutcomeIgnored)
	if got := b.Snapshot()["p1"].ProbesInFlight; got != 0 {
		t.Fatalf("probesInFlight = %d，期望归还为 0", got)
	}
	if got := b.Snapshot()["p1"].State; got != StateHalfOpen {
		t.Fatalf("state = %q，OutcomeIgnored 不应改变半开状态", got)
	}
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("额度归还后应能再放一个探针")
	}
}

func TestResetAndResetAll(t *testing.T) {
	b, _ := newTestBreaker(defaultTestSettings())
	for i := 0; i < 3; i++ {
		b.Report("p1", OutcomeFailure)
		b.Report("p2", OutcomeFailure)
	}
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatal("p1 应已开路")
	}

	if !b.Reset("p1") {
		t.Fatal("Reset 命中已有状态应返回 true")
	}
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("Reset 后应放行")
	}
	if b.Reset("不存在") {
		t.Fatal("Reset 未知 provider 应返回 false")
	}

	if n := b.ResetAll(); n != 1 {
		t.Fatalf("ResetAll = %d，期望 1（仅 p2 非闭合）", n)
	}
	if allowed, _ := b.Allow("p2"); !allowed {
		t.Fatal("ResetAll 后 p2 应放行")
	}
}

func TestSetSettingsDisableClearsState(t *testing.T) {
	b, _ := newTestBreaker(defaultTestSettings())
	for i := 0; i < 3; i++ {
		b.Report("p1", OutcomeFailure)
	}
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatal("p1 应已开路")
	}

	b.SetSettings(Settings{Enabled: false})
	// 再启用时不应沿用禁用前的过期判据
	b.SetSettings(defaultTestSettings())
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatal("禁用再启用后应从干净状态开始")
	}
	if got := b.Snapshot()["p1"].ConsecutiveFailures; got != 0 {
		t.Fatalf("consecutiveFailures = %d，期望 0", got)
	}
}

func TestSetSettingsLoweringThresholdKeepsCounters(t *testing.T) {
	// 仅调整参数（未禁用）时不清状态：正在积累的失败计数应继续有效
	b, _ := newTestBreaker(defaultTestSettings())
	b.Report("p1", OutcomeFailure)
	b.Report("p1", OutcomeFailure)

	b.SetSettings(Settings{Enabled: true, ConsecutiveFailures: 2, OpenMs: 30_000, HalfOpenProbes: 1})
	if got := b.Snapshot()["p1"].ConsecutiveFailures; got != 2 {
		t.Fatalf("consecutiveFailures = %d，期望保留为 2", got)
	}
	// 阈值降到 2，下一次失败即应开路
	b.Report("p1", OutcomeFailure)
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatal("阈值降低后应按新阈值开路")
	}
}

func TestReconcileDropsRemovedProviders(t *testing.T) {
	b, _ := newTestBreaker(defaultTestSettings())
	b.Report("keep", OutcomeFailure)
	b.Report("drop", OutcomeFailure)

	b.Reconcile(map[string]struct{}{"keep": {}})
	snap := b.Snapshot()
	if _, ok := snap["drop"]; ok {
		t.Fatal("已删除的 provider 状态应被丢弃，避免 map 无界增长")
	}
	if _, ok := snap["keep"]; !ok {
		t.Fatal("仍在配置中的 provider 状态应保留")
	}
}

func TestOpenProvidersSorted(t *testing.T) {
	b, _ := newTestBreaker(Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 30_000, HalfOpenProbes: 1})
	b.Report("zeta", OutcomeFailure)
	b.Report("alpha", OutcomeFailure)
	b.Report("healthy", OutcomeSuccess)

	got := b.OpenProviders()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("OpenProviders = %v，期望 [alpha zeta]", got)
	}
}

func TestZeroSettingsUseDefaults(t *testing.T) {
	b, advance := newTestBreaker(Settings{Enabled: true})
	for i := 0; i < defaultConsecutiveFailures; i++ {
		b.Report("p1", OutcomeFailure)
	}
	if allowed, _ := b.Allow("p1"); allowed {
		t.Fatalf("应按默认阈值 %d 开路", defaultConsecutiveFailures)
	}
	advance(time.Duration(defaultOpenMs)*time.Millisecond + time.Second)
	if allowed, _ := b.Allow("p1"); !allowed {
		t.Fatalf("应按默认冷却 %dms 转半开", defaultOpenMs)
	}
}

func TestConcurrentAllowReportIsRaceFree(t *testing.T) {
	b, _ := newTestBreaker(Settings{Enabled: true, ConsecutiveFailures: 5, OpenMs: 50, HalfOpenProbes: 2})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if allowed, _ := b.Allow("p1"); allowed {
					switch (seed + j) % 3 {
					case 0:
						b.Report("p1", OutcomeSuccess)
					case 1:
						b.Report("p1", OutcomeFailure)
					default:
						b.Report("p1", OutcomeIgnored)
					}
				}
				b.Snapshot()
				b.OpenProviders()
			}
		}(i)
	}
	wg.Wait()

	// 探针计数不应因并发出现负值或漂移到上限之上
	if got := b.Snapshot()["p1"].ProbesInFlight; got < 0 || got > 2 {
		t.Fatalf("probesInFlight = %d，期望落在 0-2", got)
	}
}
