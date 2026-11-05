package chatgptauth

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 第四阶段：账户事件只在状态转移时产生。只使用假账户、假凭据和本机假上游。

type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func recordEvents(m *Manager) *eventRecorder {
	recorder := &eventRecorder{}
	m.SetEventHandler(func(event Event) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.events = append(recorder.events, event)
	})
	return recorder
}

func (r *eventRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, 0, len(r.events))
	for _, event := range r.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func (r *eventRecorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func assertKinds(t *testing.T, recorder *eventRecorder, want ...string) {
	t.Helper()
	got := recorder.kinds()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("事件序列 = %v，期望 %v", got, want)
	}
}

func failRefresh(env *testEnv, status int, code string) {
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, status, map[string]any{"error": code})
	})
}

func TestRefreshEventsDedupAndRecover(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	recorder := recordEvents(env.m)

	failRefresh(env, http.StatusServiceUnavailable, "temporarily_unavailable")
	for i := 0; i < 3; i++ {
		if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
			t.Fatal("刷新应失败")
		}
	}
	assertKinds(t, recorder, EventRefreshFailed)

	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 60))
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	// 再次正常轮换不产生事件：只有从失败恢复才记一次。
	env.clock.Advance(time.Minute)
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	assertKinds(t, recorder, EventRefreshFailed, EventRefreshRecovered)

	// 恢复后再次失败是新的异常，重新记一次。
	env.clock.Advance(2 * time.Minute)
	failRefresh(env, http.StatusBadGateway, "bad_gateway")
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
		t.Fatal("刷新应失败")
	}
	assertKinds(t, recorder, EventRefreshFailed, EventRefreshRecovered, EventRefreshFailed)

	events := recorder.all()
	if events[0].Account != "one@example.invalid" || !reflect.DeepEqual(events[0].AccountIDs, []string{id}) || events[0].Detail == "" {
		t.Fatalf("事件身份 = %#v", events[0])
	}
	if !strings.Contains(events[1].Detail, "以之后的真实调用为准") {
		t.Fatalf("刷新恢复不应宣称调用恢复: %q", events[1].Detail)
	}
}

func TestReauthEventEscalatesOnceAndSurvivesRestart(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	recorder := recordEvents(env.m)

	failRefresh(env, http.StatusServiceUnavailable, "temporarily_unavailable")
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	failRefresh(env, http.StatusBadRequest, "invalid_grant")
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	// 已需要重新授权时不再交换，也不重复产生事件。
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	assertKinds(t, recorder, EventRefreshFailed, EventReauthRequired)
	if calls := env.up.tokenCalls.Load(); calls != 2 {
		t.Fatalf("令牌交换次数 = %d", calls)
	}

	// 重启后恢复告警状态但不重复产生事件；重新授权并成功调用后才产生恢复事件。
	env.m = env.open()
	restarted := recordEvents(env.m)
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 3600},
		accountMetadata{AccountID: "remote-1"}); err != nil {
		t.Fatal(err)
	}
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	assertKinds(t, restarted, EventCredentialsUpdated, EventRecovered)
	if event := restarted.all()[1]; event.Provider != "codex" || !reflect.DeepEqual(event.AccountIDs, []string{id}) {
		t.Fatalf("恢复事件 = %#v", event)
	}
}

func TestRecoveryRequiresNewSuccessAfterReplacement(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	recorder := recordEvents(env.m)
	failRefresh(env, http.StatusBadRequest, "invalid_grant")
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)

	before := env.m.BeginInvocation(id, "codex", "m")
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 3600},
		accountMetadata{AccountID: "remote-1"}); err != nil {
		t.Fatal(err)
	}
	// 替换前发出的尝试迟到成功、时间流逝都不构成恢复证据。
	env.m.FinishInvocation(before, InvocationResult{Success: true, HTTPStatus: 200})
	env.clock.Advance(time.Hour)
	// 替换后的失败也不是恢复；之后第一次成功才是。诊断调用同样可以作为证据。
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{HTTPStatus: 500, Reason: "5xx"})
	assertKinds(t, recorder, EventReauthRequired, EventCredentialsUpdated)
	env.m.FinishInvocation(env.m.BeginInvocationWith(id, "codex", "m", InvocationSource{Diagnostic: true}), InvocationResult{Success: true, HTTPStatus: 200})
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	assertKinds(t, recorder, EventReauthRequired, EventCredentialsUpdated, EventRecovered)
}

func TestNoRecoveryEventWithoutPriorAlert(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	recorder := recordEvents(env.m)
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 3600},
		accountMetadata{AccountID: "remote-1"}); err != nil {
		t.Fatal(err)
	}
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	assertKinds(t, recorder, EventCredentialsUpdated)
	// 新授权的账户不产生凭据更新事件。
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "other-access", RefreshToken: "other-refresh", ExpiresIn: 3600},
		accountMetadata{AccountID: "remote-new"}); err != nil {
		t.Fatal(err)
	}
	assertKinds(t, recorder, EventCredentialsUpdated)
}

func TestNewFailureAfterReplacementResetsRecoveryEvidence(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	recorder := recordEvents(env.m)
	failRefresh(env, http.StatusServiceUnavailable, "temporarily_unavailable")
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	if _, err := env.m.saveTokens(tokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 1},
		accountMetadata{AccountID: "remote-1"}); err != nil {
		t.Fatal(err)
	}
	// 替换后再次刷新失败：之前等待的恢复作废，成功调用不再产生恢复事件。
	_, _, _ = env.m.AccessToken(context.Background(), id, nil)
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{Success: true, HTTPStatus: 200})
	assertKinds(t, recorder, EventRefreshFailed, EventCredentialsUpdated, EventRefreshFailed)
}

func TestPauseEventsMergeBatchesAndSkipFailures(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	recorder := recordEvents(env.m)

	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "pause", AccountIDs: []string{first, second}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := env.m.SetPaused(first, false, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	// 目标状态未变不产生事件；写盘失败回滚，也不产生事件。
	if _, _, _, err := env.m.SetPaused(first, false, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	restore := env.failPersist()
	if _, _, _, err := env.m.SetPaused(second, false, env.m.Version()); err == nil {
		t.Fatal("注入写盘失败应返回错误")
	}
	restore()
	assertKinds(t, recorder, EventPaused, EventResumed)
	events := recorder.all()
	if !reflect.DeepEqual(events[0].AccountIDs, []string{first, second}) && !reflect.DeepEqual(events[0].AccountIDs, []string{second, first}) {
		t.Fatalf("批量暂停事件账户 = %#v", events[0].AccountIDs)
	}
	if events[0].Account != "2 个账户" || !strings.Contains(events[0].Detail, "one@example.invalid") || !strings.Contains(events[0].Detail, "two@example.invalid") {
		t.Fatalf("批量暂停事件 = %#v", events[0])
	}
	if events[1].Account != "one@example.invalid" || !reflect.DeepEqual(events[1].AccountIDs, []string{first}) || !strings.Contains(events[1].Detail, "保持停用") {
		t.Fatalf("单账户恢复事件 = %#v", events[1])
	}
}

func TestImportUpdateEmitsOneMergedEventWithoutSecrets(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	recorder := recordEvents(env.m)
	payload := `[
  {"access_token":"fresh-access-1","refresh_token":"fresh-refresh-1","account_id":"remote-1"},
  {"access_token":"fresh-access-2","refresh_token":"fresh-refresh-2","account_id":"remote-2"},
  {"access_token":"fresh-access-3","refresh_token":"fresh-refresh-3","account_id":"remote-3"}
]`
	actions := []ImportAction{{Index: 1, Action: "update", TargetAccountID: first}, {Index: 2, Action: "update", TargetAccountID: second}}
	if _, err := env.m.ApplyImport([]byte(payload), false, actions, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertKinds(t, recorder, EventCredentialsUpdated)
	event := recorder.all()[0]
	if len(event.AccountIDs) != 2 || event.Account != "2 个账户" || !strings.Contains(event.Detail, "导入更新") {
		t.Fatalf("导入更新事件 = %#v", event)
	}
	// 只新增账户的导入不产生凭据更新事件。
	if _, err := env.m.ApplyImport([]byte(`[{"access_token":"only-new-access","refresh_token":"only-new-refresh","account_id":"remote-4"}]`), false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertKinds(t, recorder, EventCredentialsUpdated)

	encoded, err := json.Marshal(recorder.all())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fresh-access", "fresh-refresh", "fake-access", "fake-refresh", "only-new-access", "only-new-refresh"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("事件包含凭据 %q: %s", secret, encoded)
		}
	}
}

func TestEventHandlerCanBeCleared(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	recorder := recordEvents(env.m)
	env.m.SetEventHandler(nil)
	if _, _, _, err := env.m.SetPaused(id, true, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	assertKinds(t, recorder)
}
