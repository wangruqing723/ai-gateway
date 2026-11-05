package chatgptauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// 第四阶段：账户统计。只使用假时钟、假账户和本机假上游。

func statsOf(t *testing.T, m *Manager, id string) AccountStatsView {
	t.Helper()
	view, ok := m.AccountStats(id)
	if !ok {
		t.Fatalf("账户 %s 的统计不存在", id)
	}
	return view
}

func TestAccountStatsWindowAndTotal(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(24*time.Hour))
	env.m.SetStatsWindow(5)

	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"),
		InvocationResult{Success: true, HTTPStatus: 200, HeaderLatency: 400 * time.Millisecond, HeadersReceived: true})
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"),
		InvocationResult{HTTPStatus: 429, Reason: "429/ratelimit", HeaderLatency: 90 * time.Millisecond, HeadersReceived: true})
	// 本地重试的传输失败：计入真实调用与重试次数，但没有响应头，不进入延迟样本。
	env.m.FinishInvocation(env.m.BeginInvocationWith(id, "codex", "m", InvocationSource{LocalRetry: true}),
		InvocationResult{Reason: "transport_error"})

	view := statsOf(t, env.m, id)
	w := view.Window
	if w.Requests != 3 || w.Successes != 1 || w.Failures != 2 || w.LocalRetries != 1 || w.RateLimited != 1 || w.LatencySamples != 2 {
		t.Fatalf("窗口统计 = %#v", w)
	}
	if w.SuccessRate == nil || *w.SuccessRate < 0.333 || *w.SuccessRate > 0.334 || w.P50LatencyMs == nil || w.P95LatencyMs == nil {
		t.Fatalf("成功率或延迟 = %#v", w)
	}
	if *w.P50LatencyMs > *w.P95LatencyMs || *w.P95LatencyMs < 400 {
		t.Fatalf("延迟分位 p50=%d p95=%d", *w.P50LatencyMs, *w.P95LatencyMs)
	}
	if view.WindowMinutes != 5 || view.Total.Requests != 3 || view.LastSuccessAt == "" || view.LastFailureAt == "" || view.Since == "" {
		t.Fatalf("统计视图 = %#v", view)
	}

	// 窗口以分钟为边界：5 分钟窗口覆盖当前分钟及之前 4 分钟。
	env.clock.Advance(4 * time.Minute)
	if got := statsOf(t, env.m, id).Window.Requests; got != 3 {
		t.Fatalf("第 4 分钟后窗口请求数 = %d", got)
	}
	env.clock.Advance(time.Minute)
	view = statsOf(t, env.m, id)
	if view.Window.Requests != 0 || view.Window.SuccessRate != nil || view.Window.P50LatencyMs != nil || view.Total.Requests != 3 {
		t.Fatalf("移出窗口后 = window %#v total %#v", view.Window, view.Total)
	}
	// 热重载放大窗口后，仍保留在分桶中的历史重新进入窗口。
	env.m.SetStatsWindow(60)
	if got := statsOf(t, env.m, id).Window.Requests; got != 3 {
		t.Fatalf("窗口放大后请求数 = %d", got)
	}
	// 超过分桶容量的旧桶被复用后不再计入，累计保持不变。
	env.clock.Advance(61 * time.Minute)
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	view = statsOf(t, env.m, id)
	if view.Window.Requests != 1 || view.Total.Requests != 4 || view.Total.Successes != 2 {
		t.Fatalf("分桶复用后 = window %#v total %#v", view.Window, view.Total)
	}
}

func TestAccountStatsWindowClampAndDefault(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	for _, tc := range []struct{ set, want int }{{0, defaultStatsWindowMinutes}, {-3, defaultStatsWindowMinutes}, {1, 1}, {600, maxStatsWindowMinutes}} {
		env.m.SetStatsWindow(tc.set)
		if got := statsOf(t, env.m, id).WindowMinutes; got != tc.want {
			t.Errorf("SetStatsWindow(%d) 后窗口 = %d，期望 %d", tc.set, got, tc.want)
		}
	}
}

func TestAccountStatsSeparatesDiagnosticsSkipsAndManagement(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))

	env.m.FinishInvocation(env.m.BeginInvocationWith(id, "codex", "m", InvocationSource{Diagnostic: true}),
		InvocationResult{Success: true, HTTPStatus: 200, HeaderLatency: time.Second, HeadersReceived: true})
	env.m.FinishInvocation(env.m.BeginInvocationWith(id, "codex", "m", InvocationSource{Diagnostic: true}),
		InvocationResult{HTTPStatus: 500, Reason: "5xx"})
	env.m.RecordAdmissionSkip(id, SkipAccountPaused)
	env.m.RecordAdmissionSkip(id, SkipAuthorization)
	env.m.RecordAdmissionSkip(id, SkipQueue)
	env.m.RecordAdmissionSkip(id, SkipQueue)

	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
		t.Fatal("刷新应失败")
	}
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 3600))
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	env.up.setUsage(serveUsage(fakeUsagePayload))
	if _, err := env.m.RefreshAccountUsage(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusBadGateway) })
	if _, err := env.m.RefreshAccountUsage(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}

	view := statsOf(t, env.m, id)
	w := view.Window
	if w.Requests != 0 || w.SuccessRate != nil || w.LatencySamples != 0 || w.P95LatencyMs != nil {
		t.Fatalf("诊断与管理请求不应进入真实调用: %#v", w)
	}
	if w.Diagnostics != (AccountStatPair{Successes: 1, Failures: 1}) || w.Skips != (AccountStatSkips{AccountPaused: 1, Authorization: 1, Queue: 2}) {
		t.Fatalf("诊断或跳过 = %#v / %#v", w.Diagnostics, w.Skips)
	}
	if w.Refresh != (AccountStatPair{Successes: 1, Failures: 1}) || w.Usage != (AccountStatPair{Successes: 1, Failures: 1}) {
		t.Fatalf("管理请求 = refresh %#v usage %#v", w.Refresh, w.Usage)
	}
	if view.LastSuccessAt != "" || view.LastFailureAt != "" {
		t.Fatalf("诊断不应写最近成功或失败时间: %#v", view)
	}
}

// blockUntilCancelled 读完请求体后触发调用方取消，并等到客户端断开，模拟在途请求被取消。
func blockUntilCancelled(cancel context.CancelFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func TestAccountStatsCallerCancelIsNotCounted(t *testing.T) {
	env := newTestEnv(t)
	expired := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	env.up.setToken(blockUntilCancelled(cancel))
	if _, _, err := env.m.AccessToken(ctx, expired, nil); err == nil {
		t.Fatal("取消的刷新应返回错误")
	}
	valid := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ctx, cancel = context.WithCancel(context.Background())
	env.up.setUsage(blockUntilCancelled(cancel))
	if _, err := env.m.RefreshAccountUsage(ctx, valid, true); err != nil {
		t.Fatal(err)
	}
	if env.up.tokenCalls.Load() != 1 || env.up.usageCalls.Load() != 1 {
		t.Fatalf("应各发出一次在途请求: token %d usage %d", env.up.tokenCalls.Load(), env.up.usageCalls.Load())
	}
	for _, id := range []string{expired, valid} {
		if w := statsOf(t, env.m, id).Window; w.Refresh != (AccountStatPair{}) || w.Usage != (AccountStatPair{}) {
			t.Fatalf("调用方取消不应计入管理请求: %#v", w)
		}
	}
}

func TestAccountStatsUnknownAndDeletedAccounts(t *testing.T) {
	env := newTestEnv(t)
	if _, ok := env.m.AccountStats("acc_missing"); ok {
		t.Fatal("不存在的账户不应有统计")
	}
	env.m.RecordAdmissionSkip("acc_missing", SkipQueue)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ticket := env.m.BeginInvocation(id, "codex", "m")
	if err := env.m.Delete(id); err != nil {
		t.Fatal(err)
	}
	env.m.FinishInvocation(ticket, InvocationResult{Success: true, HTTPStatus: 200})
	if _, ok := env.m.AccountStats(id); ok {
		t.Fatal("删除后的账户不应再有统计")
	}
	// 删除后以同一上游身份重建：本地 ID 不同，不继承旧统计。
	recreated := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	if view := statsOf(t, env.m, recreated); view.Total.Requests != 0 {
		t.Fatalf("重建账户继承了旧统计: %#v", view.Total)
	}
}

func TestAccountStatsJSONUsesNullWithoutSamples(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	encoded, err := json.Marshal(statsOf(t, env.m, id))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Window map[string]any `json:"window"`
		Total  map[string]any `json:"total"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for scope, values := range map[string]map[string]any{"window": decoded.Window, "total": decoded.Total} {
		for _, field := range []string{"successRate", "p50LatencyMs", "p95LatencyMs"} {
			value, present := values[field]
			if !present || value != nil {
				t.Errorf("%s.%s 应为 null，实际 present=%v value=%v", scope, field, present, value)
			}
		}
	}
}
