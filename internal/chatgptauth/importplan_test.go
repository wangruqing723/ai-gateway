package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// planPayload 包含：已有账户的新凭据、新账户、无效条目、同批次重复。
const planPayload = `[
  {"access_token":"fresh-access-1","refresh_token":"fresh-refresh-1","account_id":"remote-1","email":"one-new@example.invalid","last_refresh":"2030-01-01T00:00:00Z"},
  {"access_token":"fresh-access-2","refresh_token":"fresh-refresh-2","account_id":"remote-2","email":"two@example.invalid"},
  {"refresh_token":"broken","account_id":"remote-3"},
  {"access_token":"dup-access-2","refresh_token":"dup-refresh-2","account_id":"remote-2"}
]`

func TestPlanImportDefaultsAndIsReadOnly(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	before, _ := os.ReadFile(env.path)
	version := env.m.Version()
	plan, err := env.m.PlanImport([]byte(planPayload), false, nil, map[string][]string{id: {"codex-b", "codex-a"}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(env.path)
	if string(before) != string(after) || env.m.Version() != version || env.up.tokenCalls.Load() != 0 || env.up.usageCalls.Load() != 0 {
		t.Fatal("预检不能落盘、推进版本或访问上游")
	}
	if plan.BaseVersion != version || plan.CanApply || plan.NoChanges || plan.Summary != (ImportPlanSummary{Total: 4, Add: 1, Skip: 2, Invalid: 1, Blocked: 1}) {
		t.Fatalf("计划汇总 = %#v canApply=%v", plan.Summary, plan.CanApply)
	}
	existing, added, invalid, duplicate := plan.Entries[0], plan.Entries[1], plan.Entries[2], plan.Entries[3]
	if existing.Status != "existing" || existing.MatchedAccountID != id || existing.SuggestedAction != "skip" ||
		!reflect.DeepEqual(existing.AllowedActions, []string{"skip", "update"}) || !reflect.DeepEqual(existing.AffectedProviders, []string{"codex-a", "codex-b"}) {
		t.Fatalf("已有账户条目 = %#v", existing)
	}
	if added.Status != "new" || added.Action != "add" || !added.HasRefreshToken {
		t.Fatalf("新账户条目 = %#v", added)
	}
	if invalid.Status != "invalid" || invalid.InvalidReason != "第 3 条账户缺少 access_token" || len(invalid.Errors) != 1 || invalid.Action != "" {
		t.Fatalf("无效条目 = %#v", invalid)
	}
	if duplicate.DuplicateOf != 2 || duplicate.Action != "skip" || duplicate.SuggestedAction != "skip" {
		t.Fatalf("同批次重复条目 = %#v", duplicate)
	}
	encoded, _ := json.Marshal(plan)
	for _, secret := range []string{"fresh-access", "fresh-refresh", "dup-access", "broken", "fake-access", "fake-refresh"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("计划响应包含凭据 %q", secret)
		}
	}
	if !strings.Contains(strings.Join(plan.Blockers, "\n"), "第 3 条") {
		t.Fatalf("阻断说明 = %#v", plan.Blockers)
	}
}

func TestPlanImportExplicitActions(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	plan, err := env.m.PlanImport([]byte(planPayload), false, []ImportAction{
		{Index: 1, Action: "update", TargetAccountID: id},
		{Index: 3, Action: "skip"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CanApply || plan.Summary != (ImportPlanSummary{Total: 4, Add: 1, Update: 1, Skip: 2, Invalid: 1}) {
		t.Fatalf("显式选择后计划 = %#v %#v", plan.Summary, plan.Blockers)
	}
	allSkip, err := env.m.PlanImport([]byte(planPayload), false, []ImportAction{
		{Index: 2, Action: "skip"}, {Index: 3, Action: "skip"},
	}, nil)
	if err != nil || allSkip.CanApply || !allSkip.NoChanges {
		t.Fatalf("全部跳过 = %#v, %v", allSkip, err)
	}
	blocked := []struct {
		name    string
		actions []ImportAction
		want    string
	}{
		{"更新目标不一致", []ImportAction{{Index: 1, Action: "update", TargetAccountID: "acc_other"}, {Index: 3, Action: "skip"}}, "上游身份一致"},
		{"新账户不能更新", []ImportAction{{Index: 2, Action: "update", TargetAccountID: id}, {Index: 3, Action: "skip"}}, "不能选择 更新"},
		{"已有账户不能新增", []ImportAction{{Index: 1, Action: "add"}, {Index: 3, Action: "skip"}}, "不能选择 新增"},
		{"无效条目不能新增", []ImportAction{{Index: 3, Action: "add"}}, "无效条目只能跳过"},
		{"同一身份两条写入", []ImportAction{{Index: 3, Action: "skip"}, {Index: 4, Action: "add"}}, "与第 2 条是同一上游身份"},
	}
	for _, tc := range blocked {
		plan, err := env.m.PlanImport([]byte(planPayload), false, tc.actions, nil)
		if err != nil || plan.CanApply || !strings.Contains(strings.Join(plan.Blockers, "\n"), tc.want) {
			t.Errorf("%s: %#v %v", tc.name, plan.Blockers, err)
		}
	}
	malformed := [][]ImportAction{
		{{Index: 0, Action: "skip"}},
		{{Index: 5, Action: "skip"}},
		{{Index: 1, Action: "skip"}, {Index: 1, Action: "skip"}},
		{{Index: 1, Action: "replace"}},
		{{Index: 2, Action: "add", TargetAccountID: id}},
		{{Index: 1, Action: "update"}},
	}
	for _, actions := range malformed {
		if _, err := env.m.PlanImport([]byte(planPayload), false, actions, nil); !errors.Is(err, ErrInvalidImportAction) {
			t.Errorf("动作 %#v 应返回 ErrInvalidImportAction，实际 %v", actions, err)
		}
	}
}

func TestPlanImportBlocksUnsafeUpdates(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	conflict := `{"access_token":"a","refresh_token":"r","account_id":"remote-1","id_token":"` + fakeIDToken(t, "remote-9", "x@example.invalid", "plus") + `"}`
	noRefresh := `{"access_token":"a","account_id":"remote-1","expires_at":"2030-06-01T00:00:00Z"}`
	stale := `{"access_token":"a","refresh_token":"r","account_id":"remote-1","last_refresh":"2029-01-01T00:00:00Z"}`
	for name, payload := range map[string]string{"身份声明不一致": conflict, "缺少 refresh_token": noRefresh} {
		plan, err := env.m.PlanImport([]byte(payload), false, []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}}, nil)
		if err != nil || plan.CanApply || containsString(plan.Entries[0].AllowedActions, "update") || len(plan.Entries[0].Warnings) == 0 {
			t.Errorf("%s 不应允许更新: %#v %v", name, plan.Entries[0], err)
		}
	}
	plan, err := env.m.PlanImport([]byte(stale), false, []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}}, nil)
	if err != nil || !plan.CanApply || !strings.Contains(strings.Join(plan.Entries[0].Warnings, ""), "可能是旧文件") {
		t.Fatalf("旧文件只提醒不阻断: %#v %v", plan.Entries[0], err)
	}
}

func TestApplyImportUpdatesCredentialsAndKeepsLocalData(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags, note := []string{"工作"}, "保留备注"
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags, Note: &note}); err != nil {
		t.Fatal(err)
	}
	env.m.mu.Lock()
	item := env.m.accounts[id]
	item.NeedsReauth = true
	item.Usage = &UsageSnapshot{LastAttemptAt: "2029-12-31T23:50:00Z", LastSuccessAt: "2029-12-31T23:50:00Z"}
	authorizedAt := *item.AuthorizedAt
	credentialVersion := item.credentialVersion
	env.m.mu.Unlock()
	env.m.FinishInvocation(env.m.BeginInvocation(id, "codex", "m"), InvocationResult{HTTPStatus: 401, Reason: "401/error"})
	actions := []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}, {Index: 3, Action: "skip"}}
	preview, err := env.m.PlanImport([]byte(planPayload), false, actions, nil)
	if err != nil || !preview.CanApply {
		t.Fatalf("预检 = %#v %v", preview, err)
	}
	result, err := env.m.ApplyImport([]byte(planPayload), false, actions, nil, func(plan *ImportPlan) error {
		if plan.Fingerprint() != preview.Fingerprint() {
			return errors.New("计划已变化")
		}
		return nil
	})
	if err != nil || result.Imported != 1 || result.Updated != 1 || result.SkippedDuplicates != 1 || result.SkippedInvalid != 1 {
		t.Fatalf("应用结果 = %#v, %v", result, err)
	}
	creds := credentialsOf(t, env.m, "remote-1")
	if creds.ID != id || creds.AccessToken != "fresh-access-1" || creds.RefreshToken != "fresh-refresh-1" || creds.IDToken != "" ||
		creds.Email != "one-new@example.invalid" || !creds.ExpiresAt.IsZero() {
		t.Fatalf("更新后的凭据应整组替换: %#v", creds)
	}
	view, _ := env.m.Account(id)
	if view.AuthorizedAt != authorizedAt.UTC().Format(time.RFC3339) || !reflect.DeepEqual(view.Tags, tags) || view.Note != note ||
		view.Usage == nil || view.Status != "ready" || view.Authorization.State != "refreshable" || view.Invocation.State != "unknown" {
		t.Fatalf("更新后应保留本地信息并清除旧状态: %#v", view)
	}
	env.m.mu.RLock()
	bumped := env.m.accounts[id].credentialVersion
	env.m.mu.RUnlock()
	if bumped != credentialVersion+1 || env.m.Version() == preview.BaseVersion {
		t.Fatal("显式更新应推进凭据世代和账户控制版本")
	}
	reopened, ok := env.open().Account(id)
	if !ok || reopened.Note != note || reopened.Email != "one-new@example.invalid" {
		t.Fatalf("重启后 = %#v", reopened)
	}
}

func TestApplyImportVerifyFailureAndPersistFailureLeaveNoChanges(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	actions := []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}, {Index: 3, Action: "skip"}}
	before, _ := os.ReadFile(env.path)
	version := env.m.Version()
	sentinel := errors.New("校验失败")
	if _, err := env.m.ApplyImport([]byte(planPayload), false, actions, nil, func(*ImportPlan) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("校验失败应原样返回: %v", err)
	}
	restore := env.failPersist()
	_, err := env.m.ApplyImport([]byte(planPayload), false, actions, nil, nil)
	restore()
	if !errors.Is(err, ErrAccountImportPersist) {
		t.Fatalf("落盘失败 = %v", err)
	}
	after, _ := os.ReadFile(env.path)
	creds := credentialsOf(t, env.m, "remote-1")
	if string(before) != string(after) || env.m.Version() != version || len(env.m.List(nil)) != 1 ||
		creds.AccessToken != "fake-access-remote-1" || creds.RefreshToken != "fake-refresh" || creds.Email != "one@example.invalid" {
		t.Fatalf("失败后不应留下半批次变更: %#v", creds)
	}
	if _, err := env.m.ApplyImport([]byte(planPayload), false, nil, nil, nil); !errors.Is(err, ErrImportPlanNotApplicable) {
		t.Fatalf("存在阻断项时应返回 ErrImportPlanNotApplicable，实际 %v", err)
	}
}

func TestAccountVersionOnlyAdvancesOnExplicitChanges(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Minute))
	version := env.m.Version()
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated", "rotated-refresh", "", 3600))
	})
	env.up.setUsage(serveUsage(fakeUsagePayload))
	if _, err := env.m.RefreshAccountUsage(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if env.up.tokenCalls.Load() != 1 || env.m.Version() != version {
		t.Fatalf("令牌轮换和用量刷新不应推进账户控制版本: calls=%d", env.up.tokenCalls.Load())
	}
	note := "x"
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Note: &note}); err != nil || env.m.Version() == version {
		t.Fatal("元数据修改应推进账户控制版本")
	}
	version = env.m.Version()
	if _, err := env.m.Import([]byte(`{"access_token":"a","refresh_token":"r","account_id":"remote-2"}`), false); err != nil || env.m.Version() == version {
		t.Fatal("导入应推进账户控制版本")
	}
	version = env.m.Version()
	if err := env.m.Delete(id); err != nil || env.m.Version() == version {
		t.Fatal("删除应推进账户控制版本")
	}
	if strings.Contains(env.m.Version(), "rotated") || env.open().Version() == env.m.Version() {
		t.Fatal("版本不应含凭据，且进程重启后必须变化")
	}
}

func TestInFlightRefreshDoesNotOverwriteImportedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response func(w http.ResponseWriter)
	}{
		{"刷新成功迟到", func(w http.ResponseWriter) {
			writeJSONBody(w, http.StatusOK, tokenGrant("stale-rotated", "stale-rotated-refresh", "", 3600))
		}},
		{"刷新凭据被拒迟到", func(w http.ResponseWriter) {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
			entered, release := make(chan struct{}), make(chan struct{})
			env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				close(entered)
				<-release
				tc.response(w)
			})
			type tokenResult struct {
				token string
				err   error
			}
			done := make(chan tokenResult, 1)
			go func() {
				token, _, err := env.m.AccessToken(context.Background(), id, nil)
				done <- tokenResult{token, err}
			}()
			<-entered
			// 交换期间只持有账户刷新锁、不占用全局存储锁：导入照常写入，迟到的交换结果按凭据世代放弃。
			payload := `{"access_token":"imported-access","refresh_token":"imported-refresh","account_id":"remote-1","expires_at":"2030-06-01T00:00:00Z"}`
			if _, err := env.m.ApplyImport([]byte(payload), false, []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			close(release)
			got := <-done
			if got.err != nil || got.token != "imported-access" {
				t.Fatalf("刷新迟到后应返回新导入的令牌: %#v", got)
			}
			creds := credentialsOf(t, env.m, "remote-1")
			view, _ := env.m.Account(id)
			if creds.AccessToken != "imported-access" || creds.RefreshToken != "imported-refresh" ||
				view.Status != "ready" || view.Authorization.LastRefreshError != "" {
				t.Fatalf("旧刷新结果覆盖了新导入的凭据: %#v %#v", creds, view.Authorization)
			}
		})
	}
}

func TestRotationBetweenPreviewAndApplyChangesFingerprint(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Minute))
	actions := []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}, {Index: 3, Action: "skip"}}
	preview, err := env.m.PlanImport([]byte(planPayload), false, actions, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated", "rotated-refresh", "", 3600))
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	again, err := env.m.PlanImport([]byte(planPayload), false, actions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.BaseVersion != preview.BaseVersion || again.Fingerprint() == preview.Fingerprint() {
		t.Fatal("令牌轮换不改变账户控制版本，但必须改变计划摘要，迫使重新预检")
	}
	if same, _ := env.m.PlanImport([]byte(planPayload), false, actions, nil); same.Fingerprint() != again.Fingerprint() {
		t.Fatal("状态未变时计划摘要应稳定")
	}
}

func TestRefreshCredentialsForceExchanges(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("forced", "forced-refresh", "", 3600))
	})
	if exchanged, err := env.m.RefreshCredentials(context.Background(), id, nil, false); err != nil || exchanged {
		t.Fatalf("非强制刷新在令牌有效时不应交换: %v %v", exchanged, err)
	}
	if exchanged, err := env.m.RefreshCredentials(context.Background(), id, nil, true); err != nil || !exchanged || env.up.tokenCalls.Load() != 1 {
		t.Fatalf("强制刷新应交换一次: %v %v", exchanged, err)
	}
	if creds := credentialsOf(t, env.m, "remote-1"); creds.AccessToken != "forced" {
		t.Fatalf("强制刷新后令牌 = %q", creds.AccessToken)
	}
}

func TestPlanImportArchiveCollectsAcrossFiles(t *testing.T) {
	env := newTestEnv(t)
	data := buildZip(t,
		zipEntry{name: "a.json", content: `[{"access_token":"a","refresh_token":"r","account_id":"remote-1"},{"account_id":"remote-x"}]`},
		zipEntry{name: "b/auth.json", content: `{"access_token":"b","refresh_token":"r","account_id":"remote-2"}`},
	)
	plan, err := env.m.PlanImport(data, true, []ImportAction{{Index: 2, Action: "skip"}}, nil)
	if err != nil || plan.Summary != (ImportPlanSummary{Total: 3, Add: 2, Skip: 1, Invalid: 1}) || !plan.CanApply {
		t.Fatalf("ZIP 计划 = %#v %v", plan, err)
	}
	if _, err := env.m.PlanImport(buildZip(t, zipEntry{name: "../a.json", content: "{}"}), true, nil, nil); !errors.Is(err, ErrInvalidImport) {
		t.Fatalf("不安全路径仍应整体拒绝: %v", err)
	}
}
