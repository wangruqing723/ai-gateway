package chatgptauth

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestAuthorizationViewTransitions(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	failure := &refreshFailure{At: now.Add(-time.Minute), Category: "transport"}
	cases := []struct {
		name       string
		item       *account
		state      string
		canRefresh bool
		message    string
	}{
		{"访问令牌有效且可刷新", &account{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)}, "valid", true, "访问令牌有效，到期前会在使用时自动刷新"},
		{"访问令牌过期但可刷新", &account{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(-time.Hour)}, "refreshable", true, "访问令牌已过期，下次使用时会尝试用刷新凭据续期"},
		{"到期时间缺失但可刷新", &account{AccessToken: "a", RefreshToken: "r"}, "refreshable", true, "访问令牌已过期，下次使用时会尝试用刷新凭据续期"},
		{"恰好到期按过期处理", &account{AccessToken: "a", RefreshToken: "r", ExpiresAt: now}, "refreshable", true, "访问令牌已过期，下次使用时会尝试用刷新凭据续期"},
		{"无刷新凭据且未过期", &account{AccessToken: "a", ExpiresAt: now.Add(time.Hour)}, "valid", false, "访问令牌有效；没有刷新凭据，到期后需要重新授权"},
		{"无刷新凭据且已过期", &account{AccessToken: "a", ExpiresAt: now.Add(-time.Hour)}, "reauthorization_required", false, "访问令牌已过期且没有刷新凭据，需要重新授权"},
		{"刷新凭据已被拒绝", &account{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour), NeedsReauth: true}, "reauthorization_required", false, "刷新凭据已失效，需要重新授权"},
		{"最近刷新失败覆盖文案", &account{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(-time.Hour), lastRefreshFailure: failure}, "refreshable", true, refreshFailureMessage(failure)},
	}
	for _, tc := range cases {
		view := authorizationViewOf(tc.item, now)
		if view.State != tc.state || view.CanRefresh != tc.canRefresh || view.Message != tc.message {
			t.Errorf("%s: %#v", tc.name, view)
		}
	}
	withFailure := authorizationViewOf(&account{RefreshToken: "r", NeedsReauth: true, lastRefreshFailure: failure}, now)
	if withFailure.Message != "刷新凭据已失效，需要重新授权" || withFailure.LastRefreshFailedAt != "2029-12-31T23:59:00Z" {
		t.Errorf("需要重新授权时不应被刷新失败文案覆盖: %#v", withFailure)
	}
}

func TestRefreshFailureMessages(t *testing.T) {
	cases := map[string]string{
		"invalid_grant":  "刷新凭据已被 ChatGPT 拒绝，需要重新授权",
		"transport":      "最近一次自动刷新无法连接 OAuth 服务，可能是网络或代理问题；下次使用时会重试",
		"timeout":        "最近一次自动刷新超时；下次使用时会重试",
		"missing_tokens": "最近一次自动刷新收到无法识别的 OAuth 响应；下次使用时会重试",
	}
	for category, want := range cases {
		if got := refreshFailureMessage(&refreshFailure{Category: category}); got != want {
			t.Errorf("%s = %q", category, got)
		}
	}
	if got := refreshFailureMessage(&refreshFailure{Category: "http_error", HTTPStatus: 503}); got != "最近一次自动刷新失败（HTTP 503）；下次使用时会重试" {
		t.Errorf("带状态码 = %q", got)
	}
	if got := refreshFailureMessage(&refreshFailure{Category: "unknown"}); got != "最近一次自动刷新失败；下次使用时会重试" {
		t.Errorf("未知类别 = %q", got)
	}
}

func TestUsageStateViewOf(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	cases := []struct {
		name             string
		usage            *UsageSnapshot
		state, freshness string
		message          string
		lastSuccess      string
		resetPending     []string
	}{
		{"从未查询", nil, "unknown", "none", "尚未查询用量", "", nil},
		{"空快照", &UsageSnapshot{}, "unknown", "none", "尚未查询用量", "", nil},
		{"缓存内成功", &UsageSnapshot{LastAttemptAt: at(-10 * time.Minute), LastSuccessAt: at(-10 * time.Minute)}, "ok", "fresh", "用量数据来自最近一次成功查询", at(-10 * time.Minute), nil},
		{"成功已过期", &UsageSnapshot{LastAttemptAt: at(-31 * time.Minute), LastSuccessAt: at(-31 * time.Minute)}, "ok", "stale", "用量数据已超过缓存周期，可能已过时", at(-31 * time.Minute), nil},
		{"失败且无成功", &UsageSnapshot{LastAttemptAt: at(-time.Minute), Error: "x"}, "error", "none", "用量查询失败，尚无成功数据", "", nil},
		{"失败但保留成功", &UsageSnapshot{LastAttemptAt: at(-time.Minute), LastSuccessAt: at(-10 * time.Minute), Error: "x"}, "error", "fresh", "最近一次用量查询失败，显示的是上次成功查询的数据", at(-10 * time.Minute), nil},
		{"旧快照成功", &UsageSnapshot{LastCheckedAt: at(-10 * time.Minute)}, "ok", "fresh", "用量数据来自最近一次成功查询", at(-10 * time.Minute), nil},
		{"旧快照失败", &UsageSnapshot{LastCheckedAt: at(-10 * time.Minute), Error: "x"}, "error", "none", "用量查询失败，尚无成功数据", "", nil},
		{"重置待复核", &UsageSnapshot{LastAttemptAt: at(-10 * time.Minute), LastSuccessAt: at(-10 * time.Minute), Windows: []UsageWindow{
			{Name: "已到重置", ResetAt: at(-5 * time.Minute)},
			{Name: "成功前已重置", ResetAt: at(-20 * time.Minute)},
			{Name: "未到重置", ResetAt: at(time.Hour)},
			{Name: "无重置时间"},
		}}, "ok", "fresh", "用量数据来自最近一次成功查询", at(-10 * time.Minute), []string{"已到重置"}},
		{"过期数据也提示重置", &UsageSnapshot{LastAttemptAt: at(-2 * time.Hour), LastSuccessAt: at(-2 * time.Hour), Windows: []UsageWindow{
			{Name: "primary", ResetAt: at(-time.Hour)},
		}}, "ok", "stale", "用量数据已超过缓存周期，可能已过时", at(-2 * time.Hour), []string{"primary"}},
	}
	for _, tc := range cases {
		view := usageStateViewOf(tc.usage, now)
		if view.State != tc.state || view.Freshness != tc.freshness || view.Message != tc.message ||
			view.LastSuccessAt != tc.lastSuccess || !reflect.DeepEqual(view.ResetPendingWindows, tc.resetPending) {
			t.Errorf("%s: %#v", tc.name, view)
		}
	}
}

func TestInvocationRecordsLatestAttemptPerAccountAndProvider(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	if view, _ := env.m.Account(id); view.Invocation.State != "unknown" || view.Invocation.Message != "尚无经网关发出的真实调用记录" {
		t.Fatalf("初始调用状态 = %#v", view.Invocation)
	}
	if ticket := env.m.BeginInvocation("acc_missing", "p1", "m"); ticket != nil {
		t.Fatal("不存在的账户不应发放调用凭据")
	}
	env.m.FinishInvocation(nil, InvocationResult{Success: true}) // 空凭据为空操作

	older := env.m.BeginInvocation(id, "p1", "model-old")
	newer := env.m.BeginInvocation(id, "p2", "model-new")
	env.clock.Advance(time.Second)
	env.m.FinishInvocation(newer, InvocationResult{HTTPStatus: 429, Reason: "429/ratelimit"})
	env.m.FinishInvocation(older, InvocationResult{Success: true, HTTPStatus: 200})
	view, _ := env.m.Account(id)
	// 最近调用仍是更新的失败；统计口径的最后成功时间按尝试计数，迟到成功也真实发生过。
	want := InvocationView{State: "failure", Message: "最近一次真实调用被上游限流（HTTP 429）", Provider: "p2", Model: "model-new",
		ObservedAt: env.clock.Now().Format(time.RFC3339), HTTPStatus: 429, Reason: "429/ratelimit",
		LastSuccessAt: env.clock.Now().UTC().Format(time.RFC3339)}
	if !reflect.DeepEqual(view.Invocation, want) {
		t.Fatalf("旧尝试迟到成功不应盖掉更新的失败: %#v", view.Invocation)
	}
	p1, ok := env.m.ProviderInvocation(id, "p1")
	if !ok || p1.State != "success" || p1.Model != "model-old" || p1.Message != "最近一次真实调用成功" {
		t.Fatalf("p1 按 Provider 的记录 = %#v %v", p1, ok)
	}
	if p3, ok := env.m.ProviderInvocation(id, "p3"); !ok || p3.State != "unknown" {
		t.Fatalf("没有调用的 Provider = %#v %v", p3, ok)
	}
	if _, ok := env.m.ProviderInvocation("acc_missing", "p1"); ok {
		t.Fatal("不存在的账户应返回 false")
	}
	// 调用记录只属于当前进程，重启后回到未知。
	if reopened, _ := env.open().Account(id); reopened.Invocation.State != "unknown" {
		t.Fatalf("重启后调用状态 = %#v", reopened.Invocation)
	}
}

func TestInvocationFloorIsolatesReauthorizedCredentials(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	before := env.m.BeginInvocation(id, "p1", "m")
	env.m.FinishInvocation(env.m.BeginInvocation(id, "p1", "m"), InvocationResult{HTTPStatus: 401, Reason: "401/error"})
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600},
		accountMetadata{AccountID: "remote-1"}); err != nil {
		t.Fatal(err)
	}
	if view, _ := env.m.Account(id); view.Invocation.State != "unknown" {
		t.Fatalf("重新授权后应清空旧凭据的调用状态: %#v", view.Invocation)
	}
	env.m.FinishInvocation(before, InvocationResult{Success: true, HTTPStatus: 200})
	if view, _ := env.m.Account(id); view.Invocation.State != "unknown" || view.Invocation.LastSuccessAt == "" {
		// 最近调用隔离旧凭据，但统计按尝试计数并跨重新授权保留，最后成功时间仍可用于排序。
		t.Fatalf("重新授权前开始的尝试不应写入新凭据: %#v", view.Invocation)
	}
	if p1, _ := env.m.ProviderInvocation(id, "p1"); p1.State != "unknown" {
		t.Fatalf("重新授权后 Provider 记录 = %#v", p1)
	}
	env.m.FinishInvocation(env.m.BeginInvocation(id, "p1", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	if view, _ := env.m.Account(id); view.Invocation.State != "success" {
		t.Fatalf("重新授权后的新尝试应正常记录: %#v", view.Invocation)
	}
}

func TestInvocationIgnoresDeletedAccount(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ticket := env.m.BeginInvocation(id, "p1", "m")
	if err := env.m.Delete(id); err != nil {
		t.Fatal(err)
	}
	env.m.FinishInvocation(ticket, InvocationResult{Success: true})
	if env.m.BeginInvocation(id, "p1", "m") != nil {
		t.Fatal("删除后不应再发放调用凭据")
	}
	result, err := env.m.Import([]byte(`{"access_token":"a","refresh_token":"r","account_id":"remote-1"}`), false)
	if err != nil || result.Imported != 1 || result.Accounts[0].Invocation.State != "unknown" || result.Accounts[0].Invocation.LastSuccessAt != "" {
		t.Fatalf("删除后重新导入的账户不应继承旧调用结果: %#v %v", result, err)
	}
}

func TestInvocationConcurrentFinishKeepsHighestSequence(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tickets := make([]*InvocationTicket, 64)
	for i := range tickets {
		tickets[i] = env.m.BeginInvocation(id, "p1", fmt.Sprintf("model-%d", i))
	}
	var wait sync.WaitGroup
	for i := len(tickets) - 1; i >= 0; i-- {
		wait.Add(1)
		go func(ticket *InvocationTicket) {
			defer wait.Done()
			env.m.FinishInvocation(ticket, InvocationResult{Success: true, HTTPStatus: 200})
			_, _ = env.m.Account(id)
		}(tickets[i])
	}
	wait.Wait()
	view, _ := env.m.Account(id)
	p1, _ := env.m.ProviderInvocation(id, "p1")
	if view.Invocation.Model != "model-63" || p1.Model != "model-63" {
		t.Fatalf("并发写回应保留序号最大的尝试: %#v %#v", view.Invocation, p1)
	}
}

func TestInvocationFailureMessages(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   string
	}{
		{200, "conversion_error", "最近一次真实调用中网关无法转换上游响应"},
		{200, "stream_interrupted", "最近一次真实调用在上游开始响应后中断，可能是网络、代理或超时问题"},
		{401, "401/error", "最近一次真实调用被上游拒绝（HTTP 401），请检查账户授权或权限"},
		{403, "403/auth", "最近一次真实调用被上游拒绝（HTTP 403），请检查账户授权或权限"},
		{429, "429/ratelimit", "最近一次真实调用被上游限流（HTTP 429）"},
		{502, "502/server", "最近一次真实调用上游服务异常（HTTP 502）"},
		{400, "400/error", "最近一次真实调用失败（HTTP 400），可能与模型或请求参数有关"},
		{0, "transport_error", "最近一次真实调用没有得到上游响应，可能是网络、代理或超时问题"},
	}
	for _, tc := range cases {
		if got := invocationFailureMessage(tc.status, tc.reason); got != tc.want {
			t.Errorf("%d %s = %q", tc.status, tc.reason, got)
		}
	}
}
