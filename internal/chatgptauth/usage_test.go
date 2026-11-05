package chatgptauth

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeUsagePayload 覆盖上游用量响应的各种 reset_at 写法、可选窗口与 credits。
// 假时钟起点为 2030-01-01T00:00:00Z，下列重置时间都在其之后。
const fakeUsagePayload = `{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window": {"used_percent": 25, "limit_window_seconds": 18000, "reset_at": 1893600000},
    "secondary_window": {"used_percent": 120, "limit_window_seconds": 604800, "reset_at": "2030-01-08T00:00:00Z"}
  },
  "code_review_rate_limit": {
    "primary_window": {"used_percent": 10, "reset_after_seconds": 3600},
    "secondary_window": {"limit_window_seconds": 604800}
  },
  "additional_rate_limits": [
    {"limit_name": " gpt-6 ", "rate_limit": {"primary_window": {"used_percent": 50, "reset_at": "1893628800"}}},
    {"rate_limit": {"primary_window": {"used_percent": 0, "reset_at": 1893628800000}}}
  ],
  "credits": {"balance": 12.5}
}`

func serveUsage(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestRefreshAccountUsageParsesWindowsAndPersists(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access-remote-1" || r.Header.Get("Chatgpt-Account-Id") != "remote-1" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "codex-cli" {
			http.Error(w, "unexpected headers", http.StatusBadRequest)
			return
		}
		serveUsage(fakeUsagePayload)(w, r)
	})
	view, err := env.m.RefreshAccountUsage(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []UsageWindow{
		{Name: "primary", LimitWindowSeconds: 18000, UsedPercent: 25, RemainingPercent: 75, ResetAt: "2030-01-02T16:00:00Z"},
		{Name: "secondary", LimitWindowSeconds: 604800, UsedPercent: 120, RemainingPercent: 0, ResetAt: "2030-01-08T00:00:00Z"},
		{Name: "code review primary", UsedPercent: 10, RemainingPercent: 90, ResetAt: "2030-01-01T01:00:00Z"},
		{Name: "gpt-6 primary", UsedPercent: 50, RemainingPercent: 50, ResetAt: "2030-01-03T00:00:00Z"},
		{Name: "additional 2 primary", UsedPercent: 0, RemainingPercent: 100, ResetAt: "2030-01-03T00:00:00Z"},
	}
	if view.Usage == nil || !reflect.DeepEqual(view.Usage.Windows, want) {
		t.Fatalf("用量窗口 = %#v", view.Usage)
	}
	now := env.clock.Now().Format(time.RFC3339)
	if view.Usage.Credits == nil || view.Usage.Credits.Balance != "12.5" || view.Plan != "pro" || view.Usage.Error != "" ||
		view.Usage.LastAttemptAt != now || view.Usage.LastSuccessAt != now || view.Usage.LastCheckedAt != now {
		t.Fatalf("用量元信息 = %#v plan=%q", view.Usage, view.Plan)
	}
	if view.UsageState.State != "ok" || view.UsageState.Freshness != "fresh" || len(view.UsageState.ResetPendingWindows) != 0 {
		t.Fatalf("用量状态 = %#v", view.UsageState)
	}
	reopened, ok := env.open().Account(id)
	if !ok || !reflect.DeepEqual(reopened.Usage, view.Usage) || reopened.Plan != "pro" {
		t.Fatalf("重启后用量快照 = %#v", reopened.Usage)
	}
}

func TestRefreshAccountUsageFailureKeepsLastSuccess(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(24*time.Hour))
	env.up.setUsage(serveUsage(fakeUsagePayload))
	first, err := env.m.RefreshAccountUsage(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(31 * time.Minute)
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fake-upstream-secret-body", http.StatusServiceUnavailable)
	})
	second, err := env.m.RefreshAccountUsage(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Usage.Error != "ChatGPT 用量服务返回 HTTP 503" || !reflect.DeepEqual(second.Usage.Windows, first.Usage.Windows) {
		t.Fatalf("失败后应保留旧窗口并记录错误: %#v", second.Usage)
	}
	if second.Usage.LastSuccessAt != first.Usage.LastSuccessAt || second.Usage.LastAttemptAt != env.clock.Now().Format(time.RFC3339) {
		t.Fatalf("失败后时间字段 = %#v", second.Usage)
	}
	if second.UsageState.State != "error" || second.UsageState.Freshness != "stale" ||
		second.UsageState.Message != "最近一次用量查询失败，显示的是上次成功查询的数据" {
		t.Fatalf("失败后用量状态 = %#v", second.UsageState)
	}
	env.clock.Advance(31 * time.Minute)
	env.up.setUsage(serveUsage(fakeUsagePayload))
	third, _ := env.m.RefreshAccountUsage(context.Background(), id, false)
	if third.Usage.Error != "" || third.UsageState.State != "ok" || third.UsageState.Freshness != "fresh" {
		t.Fatalf("恢复后用量 = %#v %#v", third.Usage, third.UsageState)
	}
}

func TestRefreshAccountUsageErrorsAreSanitized(t *testing.T) {
	oversized := `{"plan_type":"pro"` + strings.Repeat(" ", maxUsageBody) + `}`
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"401", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fake-secret", http.StatusUnauthorized) }, "ChatGPT 拒绝了用量查询，请确认账户状态或稍后重试"},
		{"403", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fake-secret", http.StatusForbidden) }, "ChatGPT 拒绝了用量查询，请确认账户状态或稍后重试"},
		{"500", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "fake-secret", http.StatusInternalServerError)
		}, "ChatGPT 用量服务返回 HTTP 500"},
		{"非 JSON", serveUsage(`fake-secret <html>`), "无法读取 ChatGPT 用量，请稍后重试"},
		{"超过大小上限", serveUsage(oversized), "无法读取 ChatGPT 用量，请稍后重试"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
			env.up.setUsage(tc.handler)
			view, err := env.m.RefreshAccountUsage(context.Background(), id, true)
			if err != nil {
				t.Fatal(err)
			}
			if view.Usage == nil || view.Usage.Error != tc.want || strings.Contains(view.Usage.Error, "fake-secret") {
				t.Fatalf("错误文案 = %#v", view.Usage)
			}
			if len(view.Usage.Windows) != 0 || view.Usage.LastSuccessAt != "" {
				t.Fatalf("没有成功数据时不应出现窗口或成功时间: %#v", view.Usage)
			}
			if view.UsageState.State != "error" || view.UsageState.Freshness != "none" || view.UsageState.Message != "用量查询失败，尚无成功数据" {
				t.Fatalf("用量状态 = %#v", view.UsageState)
			}
		})
	}
}

func TestRefreshAccountUsageTimeout(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	view, err := env.m.RefreshAccountUsage(ctx, id, true)
	if err != nil || view.Usage == nil || view.Usage.Error != "ChatGPT 用量查询超时，请稍后重试" {
		t.Fatalf("超时文案 = %#v, %v", view.Usage, err)
	}
}

func TestRefreshAccountUsageCacheTTLAndForce(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(24*time.Hour))
	env.up.setUsage(serveUsage(fakeUsagePayload))
	refresh := func(force bool, wantCalls int32) {
		t.Helper()
		if _, err := env.m.RefreshAccountUsage(context.Background(), id, force); err != nil {
			t.Fatal(err)
		}
		if calls := env.up.usageCalls.Load(); calls != wantCalls {
			t.Fatalf("force=%v 后用量请求 %d 次，期望 %d", force, calls, wantCalls)
		}
	}
	refresh(false, 1) // 没有快照，必须查询
	refresh(false, 1) // 缓存周期内复用
	refresh(true, 2)  // 强制刷新绕过缓存
	env.clock.Advance(29 * time.Minute)
	refresh(false, 2)
	env.clock.Advance(time.Minute)
	refresh(false, 3) // 恰好 30 分钟已不在缓存周期内

	// 现有语义：缓存按最近一次尝试计时，失败也会在缓存周期内抑制非强制刷新。
	env.clock.Advance(31 * time.Minute)
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fail", http.StatusInternalServerError) })
	refresh(false, 4)
	refresh(false, 4)
	refresh(true, 5)
}

func TestUsageCacheRestoresLegacySnapshotAfterRestart(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(24*time.Hour))
	// 早期版本只写 lastCheckedAt，没有 lastAttemptAt / lastSuccessAt。
	checked := env.clock.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	unlock, err := env.m.lockForWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.m.accounts[id].Usage = &UsageSnapshot{PlanType: "plus", LastCheckedAt: checked,
		Windows: []UsageWindow{{Name: "primary", UsedPercent: 40, RemainingPercent: 60, ResetAt: "2030-01-01T02:00:00Z"}}}
	if err := env.m.persistLocked(); err != nil {
		t.Fatal(err)
	}
	unlock()

	reopened := env.open()
	env.up.setUsage(serveUsage(fakeUsagePayload))
	view, err := reopened.RefreshAccountUsage(context.Background(), id, false)
	if err != nil || env.up.usageCalls.Load() != 0 {
		t.Fatalf("旧快照仍在缓存周期内，不应查询上游: calls=%d err=%v", env.up.usageCalls.Load(), err)
	}
	if view.UsageState.State != "ok" || view.UsageState.Freshness != "fresh" || view.UsageState.LastSuccessAt != checked {
		t.Fatalf("旧快照状态 = %#v", view.UsageState)
	}
	env.clock.Advance(21 * time.Minute)
	if _, err := reopened.RefreshAccountUsage(context.Background(), id, false); err != nil || env.up.usageCalls.Load() != 1 {
		t.Fatalf("旧快照过期后应重新查询: calls=%d err=%v", env.up.usageCalls.Load(), err)
	}
}

func TestRefreshAccountUsageTokenFailureMessages(t *testing.T) {
	cases := []struct {
		name    string
		refresh string
		token   http.HandlerFunc
		want    string
	}{
		{"过期且无刷新凭据", "", nil, "OAuth 刷新凭据已失效，请重新授权"},
		{"刷新凭据被拒", "fake-refresh", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		}, "OAuth 刷新凭据已失效，请重新授权"},
		{"刷新服务暂时失败", "fake-refresh", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
		}, "无法获取访问令牌，请检查 OAuth 代理或网络设置后重试"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.addAccount("remote-1", "one@example.invalid", tc.refresh, env.clock.Now().Add(-time.Minute))
			if tc.token != nil {
				env.up.setToken(tc.token)
			}
			view, err := env.m.RefreshAccountUsage(context.Background(), id, true)
			if err != nil || view.Usage == nil || view.Usage.Error != tc.want {
				t.Fatalf("令牌失败文案 = %#v, %v", view.Usage, err)
			}
			if env.up.usageCalls.Load() != 0 {
				t.Fatal("拿不到令牌时不应请求用量端点")
			}
		})
	}
}

func TestRefreshUsageAllAccountsAndMissingAccount(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "b@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "a@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.up.setUsage(serveUsage(fakeUsagePayload))
	views := env.m.RefreshUsage(context.Background(), map[string][]string{first: {"p2", "p1"}}, false)
	if len(views) != 2 || views[0].ID != second || views[1].ID != first || env.up.usageCalls.Load() != 2 {
		t.Fatalf("全部刷新 = %#v calls=%d", views, env.up.usageCalls.Load())
	}
	if !reflect.DeepEqual(views[1].Providers, []string{"p1", "p2"}) || views[0].Usage == nil || views[1].Usage == nil {
		t.Fatalf("全部刷新视图 = %#v", views)
	}
	if _, err := env.m.RefreshAccountUsage(context.Background(), "acc_missing", true); err != ErrAccountNotFound {
		t.Fatalf("不存在的账户 = %v", err)
	}
}
