package chatgptauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// 后台调度测试直接调用 runBackgroundTick，用假时钟推进时间，不依赖真实的 30 秒周期。

func backgroundOn(token, usage bool) BackgroundSettings {
	return BackgroundSettings{TokenRefresh: token, UsageRefresh: usage, UsageInterval: 30 * time.Minute, ActiveWindow: time.Hour}
}

// enableBackground 只写入设置、不启动循环，由测试手动驱动每一轮。
func (env *testEnv) enableBackground(settings BackgroundSettings) {
	env.t.Helper()
	settings = backgroundSettingsOf(settings)
	env.m.bgSettings.Store(&settings)
	env.m.bgMu.Lock()
	env.m.bg.requested = settings
	env.m.bgMu.Unlock()
}

func (env *testEnv) tick() {
	env.t.Helper()
	env.m.runBackgroundTick(context.Background())
}

// markUsed 模拟一次真实上游调用，让账户成为活跃账户。
func (env *testEnv) markUsed(id string) {
	env.t.Helper()
	if ticket := env.m.BeginInvocation(id, "codex", "gpt-test"); ticket == nil {
		env.t.Fatal("账户不存在")
	}
}

func (env *testEnv) backgroundView(id string) *BackgroundView {
	env.t.Helper()
	view, ok := env.m.Account(id)
	if !ok {
		env.t.Fatal("账户不存在")
	}
	return view.Background
}

func TestBackgroundTokenRefreshOnlyForActiveAccounts(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(5*time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("bg-access", "bg-refresh", "", 3600))
	})
	env.enableBackground(backgroundOn(true, false))
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 0 {
		t.Fatalf("闲置账户不应预刷新，交换次数 = %d", calls)
	}
	if bg := env.backgroundView(id); bg == nil || bg.Active || bg.Skipped != BackgroundSkipInactive || bg.Usage != nil {
		t.Fatalf("闲置账户视图: %#v", bg)
	}
	// 诊断调用不让账户变成活跃账户。
	env.m.BeginInvocationWith(id, "codex", "gpt-test", InvocationSource{Diagnostic: true})
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 0 {
		t.Fatalf("诊断调用不应触发预刷新，交换次数 = %d", calls)
	}
	env.markUsed(id)
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("活跃账户到期前 10 分钟内应预刷新一次，交换次数 = %d", calls)
	}
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("刷新后令牌远未到期，不应再交换，交换次数 = %d", calls)
	}
	bg := env.backgroundView(id)
	if bg == nil || !bg.Active || bg.Skipped != "" || bg.Token == nil || bg.Token.LastResult != backgroundResultOK ||
		bg.Token.Failures != 0 || bg.Token.NextAt != env.clock.Now().Add(50*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("预刷新后的视图: %#v %#v", bg, bg.Token)
	}
	// 超过活跃窗口后不再调度。
	env.clock.Advance(2 * time.Hour)
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("超过活跃窗口不应再预刷新，交换次数 = %d", calls)
	}
}

func TestBackgroundTokenFailureBacksOff(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(9*time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fake-unavailable", http.StatusServiceUnavailable)
	})
	env.enableBackground(backgroundOn(true, false))
	env.markUsed(id)
	steps := []struct {
		advance  time.Duration
		calls    int32
		failures int
	}{
		{0, 1, 1},
		{30 * time.Second, 1, 1}, // 1 分钟退避内
		{31 * time.Second, 2, 2}, // 退避到期，第二次失败后退避 2 分钟
		{61 * time.Second, 2, 2}, // 仍在 2 分钟退避内
		{61 * time.Second, 3, 3}, // 到期
		{0, 3, 3},
	}
	for i, step := range steps {
		env.clock.Advance(step.advance)
		env.tick()
		bg := env.backgroundView(id)
		if calls := env.up.tokenCalls.Load(); calls != step.calls || bg.Token.Failures != step.failures {
			t.Fatalf("第 %d 步: 交换 %d 次、失败 %d 次，want %d、%d", i, calls, bg.Token.Failures, step.calls, step.failures)
		}
	}
	if bg := env.backgroundView(id); bg.Token.LastResult != backgroundResultError || bg.Skipped != "" {
		t.Fatalf("网络类失败不应标记重新授权: %#v", bg)
	}
	// 任何入口的成功都清零失败计数。
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("manual-access", "manual-refresh", "", 3600))
	})
	if _, err := env.m.RefreshCredentials(context.Background(), id, nil, true); err != nil {
		t.Fatal(err)
	}
	if bg := env.backgroundView(id); bg.Token.Failures != 0 {
		t.Fatalf("人工刷新成功后应清零失败计数: %#v", bg.Token)
	}
}

func TestBackgroundSkipReasons(t *testing.T) {
	env := newTestEnv(t)
	paused := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Minute))
	reauth := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Minute))
	noRefresh := env.addAccount("remote-3", "three@example.invalid", "", env.clock.Now().Add(time.Hour))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
	})
	env.enableBackground(backgroundOn(true, true))
	for _, id := range []string{paused, reauth, noRefresh} {
		env.markUsed(id)
	}
	if _, _, _, err := env.m.SetPaused(paused, true, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	env.tick()
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("只有可调度账户会交换，交换次数 = %d", calls)
	}
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected", http.StatusTeapot)
	})
	env.clock.Advance(2 * time.Minute)
	env.tick()
	want := map[string]string{paused: BackgroundSkipPaused, reauth: BackgroundSkipReauth, noRefresh: BackgroundSkipNoRefresh}
	for id, reason := range want {
		bg := env.backgroundView(id)
		if bg == nil || bg.Skipped != reason || !bg.Active || bg.Token == nil || bg.Usage == nil || bg.Token.NextAt != "" {
			t.Fatalf("%s 跳过原因: %#v", reason, bg)
		}
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("invalid_grant 之后不应再后台交换，交换次数 = %d", calls)
	}
}

func TestBackgroundUsageIntervalAndRetryAfter(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(30*24*time.Hour))
	env.up.setUsage(serveUsage(fakeUsagePayload))
	settings := backgroundOn(false, true)
	env.enableBackground(settings)
	env.markUsed(id)
	env.tick()
	if calls := env.up.usageCalls.Load(); calls != 1 {
		t.Fatalf("从未查询过的活跃账户应立即刷新用量，查询次数 = %d", calls)
	}
	jitter := usageJitter(id, settings.UsageInterval)
	if jitter < 0 || jitter >= settings.UsageInterval/10 {
		t.Fatalf("错开量应在十分之一周期内: %s", jitter)
	}
	env.clock.Advance(settings.UsageInterval + jitter - time.Second)
	env.markUsed(id)
	env.tick()
	if calls := env.up.usageCalls.Load(); calls != 1 {
		t.Fatalf("周期加错开量之内不应再查询，查询次数 = %d", calls)
	}
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7200")
		http.Error(w, "fake-limited", http.StatusTooManyRequests)
	})
	env.clock.Advance(2 * time.Second)
	env.tick()
	bg := env.backgroundView(id)
	if calls := env.up.usageCalls.Load(); calls != 2 || bg.Usage.Failures != 1 || bg.Usage.LastResult != backgroundResultError {
		t.Fatalf("到期后应查询一次并记录失败: %d %#v", calls, bg.Usage)
	}
	// Retry-After（2 小时）比一个周期的退避（30 分钟）更长，以它为准。
	if want := env.clock.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339); bg.Usage.NextAt != want {
		t.Fatalf("下次计划应按 Retry-After 推迟: %s want %s", bg.Usage.NextAt, want)
	}
	env.clock.Advance(time.Hour)
	env.markUsed(id)
	env.tick()
	if calls := env.up.usageCalls.Load(); calls != 2 {
		t.Fatalf("Retry-After 期间不应查询，查询次数 = %d", calls)
	}
	// 人工刷新不受后台退避限制，成功后清零失败计数并解除推迟。
	env.up.setUsage(serveUsage(fakeUsagePayload))
	if _, err := env.m.RefreshAccountUsage(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	bg = env.backgroundView(id)
	if bg.Usage.Failures != 0 || bg.Usage.NextAt != env.clock.Now().Add(settings.UsageInterval+jitter).UTC().Format(time.RFC3339) {
		t.Fatalf("人工刷新成功后的计划: %#v", bg.Usage)
	}
}

func TestBackgroundUsageAttemptSharedBetweenInstances(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(30*24*time.Hour))
	peer := env.openPeer()
	env.up.setUsage(serveUsage(fakeUsagePayload))
	env.enableBackground(backgroundOn(false, true))
	settings := backgroundSettingsOf(backgroundOn(false, true))
	peer.bgSettings.Store(&settings)
	env.markUsed(id)
	if ticket := peer.BeginInvocation(id, "codex", "gpt-test"); ticket == nil {
		t.Fatal("另一实例应看到账户")
	}
	env.tick()
	peer.runBackgroundTick(context.Background())
	if calls := env.up.usageCalls.Load(); calls != 1 {
		t.Fatalf("另一实例应从共享文件读到最近尝试时间并推迟，查询次数 = %d", calls)
	}
}

func TestBackgroundLifecycle(t *testing.T) {
	env := newTestEnv(t)
	supported := true
	env.m.lockingOverride = &supported
	env.m.tickOverride = 10 * time.Millisecond
	if status := env.m.BackgroundStatus(); status.Running || status.TokenRefresh || status.Reason != "" {
		t.Fatalf("默认关闭: %#v", status)
	}
	if err := env.m.SetBackground(BackgroundSettings{TokenRefresh: true}); err != nil {
		t.Fatal(err)
	}
	env.m.bgMu.Lock()
	done := env.m.bg.done
	env.m.bgMu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for env.m.BackgroundStatus().LastTickAt == "" {
		if time.Now().After(deadline) {
			t.Fatal("后台循环没有执行检查")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 热重载只更新设置，不出现第二个循环。
	if err := env.m.SetBackground(BackgroundSettings{TokenRefresh: true, UsageRefresh: true}); err != nil {
		t.Fatal(err)
	}
	env.m.bgMu.Lock()
	sameLoop := env.m.bg.done == done
	env.m.bgMu.Unlock()
	status := env.m.BackgroundStatus()
	if !sameLoop || !status.Running || !status.UsageRefresh || !status.CrossProcessLocking {
		t.Fatalf("热重载后应沿用同一个循环: %v %#v", sameLoop, status)
	}
	if got := env.m.bgSettings.Load(); got == nil || !got.UsageRefresh || got.UsageInterval != 30*time.Minute || got.ActiveWindow != time.Hour {
		t.Fatalf("运行设置应补齐默认周期: %#v", got)
	}
	if err := env.m.SetBackground(BackgroundSettings{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("全部关闭后 SetBackground 应等循环退出")
	}
	if status := env.m.BackgroundStatus(); status.Running || env.m.bgSettings.Load() != nil {
		t.Fatalf("关闭后不应运行: %#v", status)
	}
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	if bg := env.backgroundView(id); bg != nil {
		t.Fatalf("后台关闭时账户视图不应带 background: %#v", bg)
	}
}

func TestBackgroundRefusesWithoutCrossProcessLocking(t *testing.T) {
	env := newTestEnv(t)
	unsupported := false
	env.m.lockingOverride = &unsupported
	err := env.m.SetBackground(BackgroundSettings{UsageRefresh: true})
	if !errors.Is(err, ErrBackgroundUnsupported) {
		t.Fatalf("不支持跨进程锁时应拒绝启用: %v", err)
	}
	status := env.m.BackgroundStatus()
	if status.Running || status.Reason != backgroundReasonPlatform || !status.UsageRefresh || status.CrossProcessLocking {
		t.Fatalf("拒绝启用时的状态: %#v", status)
	}
	if err := env.m.SetBackground(BackgroundSettings{}); err != nil || env.m.BackgroundStatus().Reason != "" {
		t.Fatalf("全部关闭后不再报告平台原因: %v %#v", err, env.m.BackgroundStatus())
	}
}

func TestBackgroundStopCancelsInFlightWork(t *testing.T) {
	env := newTestEnv(t)
	supported := true
	env.m.lockingOverride = &supported
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Minute))
	entered := make(chan struct{})
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		// 读完请求体后服务端才会监测连接断开，客户端取消才能传到 r.Context()。
		_ = r.ParseForm()
		close(entered)
		<-r.Context().Done()
	})
	env.markUsed(id)
	if err := env.m.SetBackground(BackgroundSettings{TokenRefresh: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("后台预刷新没有开始")
	}
	stopped := make(chan struct{})
	go func() {
		env.m.StopBackground()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("StopBackground 应取消在途出网并等循环退出")
	}
	view, _ := env.m.Account(id)
	if view.Authorization.LastRefreshError != "" || view.Background != nil {
		t.Fatalf("关闭导致的取消不应记为刷新失败: %#v", view.Authorization)
	}
}
