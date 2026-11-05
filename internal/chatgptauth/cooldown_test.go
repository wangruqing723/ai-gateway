package chatgptauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 第五阶段：账户共享额度冷却。只使用临时目录与假账户。

func TestReportUsageLimitRequiresSharedCooldown(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ticket := env.m.BeginInvocation(id, "codex", "gpt-x")
	if got := env.m.ReportUsageLimit(ticket, env.clock.Now().Add(time.Hour)); !got.IsZero() {
		t.Fatalf("未开启共享冷却时不应生效：%v", got)
	}
	if !env.m.CooldownUntil(id).IsZero() {
		t.Fatal("未开启时不应冷却")
	}
	if got := env.m.ReportUsageLimit(nil, time.Time{}); !got.IsZero() {
		t.Fatal("nil ticket 应为空操作")
	}
}

func TestReportUsageLimitDurationRules(t *testing.T) {
	env := newTestEnv(t)
	env.m.SetSharedCooldown(true)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	now := env.clock.Now()

	// 没有上游时间：默认 60 秒。
	got := env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), time.Time{})
	if !got.Equal(now.Add(DefaultCooldown)) {
		t.Fatalf("默认冷却 = %v", got)
	}
	// 更早的截止时间不缩短已有冷却。
	got = env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), now.Add(10*time.Second))
	if !got.Equal(now.Add(DefaultCooldown)) {
		t.Fatalf("不应缩短冷却：%v", got)
	}
	// 更晚的时间延长。
	got = env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), now.Add(2*time.Hour))
	if !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("应延长冷却：%v", got)
	}
	// 超过 30 天夹到上限。
	got = env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), now.Add(365*24*time.Hour))
	if !got.Equal(now.Add(MaxCooldown)) {
		t.Fatalf("应夹到 30 天上限：%v", got)
	}
	view, _ := env.m.Account(id)
	if view.CooldownUntil != now.Add(MaxCooldown).UTC().Format(time.RFC3339) {
		t.Fatalf("视图 cooldownUntil = %q", view.CooldownUntil)
	}
	// 到期后视图不再显示，准入恢复。
	env.clock.Advance(MaxCooldown + time.Second)
	if !env.m.CooldownUntil(id).IsZero() {
		t.Fatal("到期后应结束冷却")
	}
	if view, _ := env.m.Account(id); view.CooldownUntil != "" {
		t.Fatalf("到期后视图仍显示冷却：%q", view.CooldownUntil)
	}
	watch, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatalf("到期后准入 = %v", err)
	}
	watch.Release()
}

func TestCooldownWakesWaitersAndRejectsAdmission(t *testing.T) {
	env := newTestEnv(t)
	env.m.SetSharedCooldown(true)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	other := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	watch, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	untouched, err := env.m.WatchAdmission(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	defer untouched.Release()

	env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), env.clock.Now().Add(time.Hour))
	select {
	case <-watch.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("冷却应唤醒等待者")
	}
	if !watch.CooledDown() || watch.Paused() {
		t.Fatalf("唤醒原因应为冷却：cooled=%v paused=%v", watch.CooledDown(), watch.Paused())
	}
	if untouched.Context().Err() != nil {
		t.Fatal("其他账户的等待者不应被唤醒")
	}
	if _, err := env.m.WatchAdmission(context.Background(), id); !errors.Is(err, ErrAccountCooldown) {
		t.Fatalf("冷却中准入 = %v", err)
	}
}

func TestClearCooldownFloorAndEvents(t *testing.T) {
	env := newTestEnv(t)
	recorder := recordEvents(env.m)
	env.m.SetSharedCooldown(true)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))

	if _, _, err := env.m.ClearCooldown("acc_missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在账户 = %v", err)
	}
	_, changed, err := env.m.ClearCooldown(id)
	if err != nil || changed {
		t.Fatalf("没有冷却时 changed=%v err=%v", changed, err)
	}

	stale := env.m.BeginInvocation(id, "codex", "m")
	env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), env.clock.Now().Add(time.Hour))
	env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), env.clock.Now().Add(2*time.Hour)) // 延长不重复记事件
	view, changed, err := env.m.ClearCooldown(id)
	if err != nil || !changed || view.CooldownUntil != "" {
		t.Fatalf("解除 = %#v changed=%v err=%v", view, changed, err)
	}
	// 解除前发出的请求迟到的额度错误不再生效。
	if got := env.m.ReportUsageLimit(stale, env.clock.Now().Add(time.Hour)); !got.IsZero() {
		t.Fatalf("旧请求不应重新冷却：%v", got)
	}
	// 解除后新发出的请求仍可触发冷却。
	if got := env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), time.Time{}); got.IsZero() {
		t.Fatal("解除后的新请求应能触发冷却")
	}
	want := []string{EventCooldown, EventCooldownCleared, EventCooldown}
	if got := recorder.kinds(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("事件 = %v，期望 %v", got, want)
	}
}

func TestCooldownSurvivesSuccessAndReauthButNotDisable(t *testing.T) {
	env := newTestEnv(t)
	recorder := recordEvents(env.m)
	env.m.SetSharedCooldown(true)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	inflight := env.m.BeginInvocation(id, "codex", "m")
	env.m.ReportUsageLimit(env.m.BeginInvocation(id, "codex", "m"), env.clock.Now().Add(time.Hour))

	// 成功请求与凭据替换都不解除冷却。
	env.m.FinishInvocation(inflight, InvocationResult{Success: true})
	env.m.mu.Lock()
	env.m.resetInvocationsLocked(env.m.accounts[id])
	env.m.mu.Unlock()
	if env.m.CooldownUntil(id).IsZero() {
		t.Fatal("成功或凭据替换不应解除冷却")
	}

	// 关闭共享冷却：清除且不记事件；关闭前发出的请求重新开启后不生效。
	stale := env.m.BeginInvocation(id, "codex", "m")
	env.m.SetSharedCooldown(false)
	if !env.m.CooldownUntil(id).IsZero() {
		t.Fatal("关闭共享冷却后应清除")
	}
	env.m.SetSharedCooldown(true)
	if !env.m.CooldownUntil(id).IsZero() {
		t.Fatal("重新开启不应恢复旧冷却")
	}
	if got := env.m.ReportUsageLimit(stale, env.clock.Now().Add(time.Hour)); !got.IsZero() {
		t.Fatal("关闭前发出的请求不应在重新开启后冷却")
	}
	if got := recorder.kinds(); len(got) != 1 || got[0] != EventCooldown {
		t.Fatalf("事件 = %v，关闭冷却不应记事件", got)
	}
}

func TestCooldownStaleTicketAfterDeleteIgnored(t *testing.T) {
	env := newTestEnv(t)
	env.m.SetSharedCooldown(true)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ticket := env.m.BeginInvocation(id, "codex", "m")
	if err := env.m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if got := env.m.ReportUsageLimit(ticket, env.clock.Now().Add(time.Hour)); !got.IsZero() {
		t.Fatal("已删除账户的迟到结果不应生效")
	}
}

func TestCooldownSkipStat(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.m.RecordAdmissionSkip(id, SkipCooldown)
	stats, ok := env.m.AccountStats(id)
	if !ok || stats.Window.Skips.Cooldown != 1 || stats.Total.Skips.Cooldown != 1 || stats.Window.Skips.Queue != 0 {
		t.Fatalf("冷却跳过统计 = %#v", stats.Window.Skips)
	}
}
