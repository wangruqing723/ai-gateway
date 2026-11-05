package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// 第三阶段：批量计划、应用与批量用量刷新。只使用临时目录、假账户与 httptest 假上游。

func TestNormalizeBatchRequestValidation(t *testing.T) {
	many := make([]string, maxBatchAccounts+1)
	for i := range many {
		many[i] = fmt.Sprintf("acc_%d", i)
	}
	cases := []struct {
		name    string
		request BatchRequest
	}{
		{"未知操作", BatchRequest{Operation: "enable", AccountIDs: []string{"acc_a"}}},
		{"没有账户", BatchRequest{Operation: "pause"}},
		{"超过上限", BatchRequest{Operation: "pause", AccountIDs: many}},
		{"重复账户", BatchRequest{Operation: "pause", AccountIDs: []string{"acc_a", "acc_a"}}},
		{"非法 ID", BatchRequest{Operation: "pause", AccountIDs: []string{"acc/a"}}},
		{"非标签操作带标签", BatchRequest{Operation: "pause", AccountIDs: []string{"acc_a"}, AddTags: []string{"x"}}},
		{"标签操作无标签", BatchRequest{Operation: "tags", AccountIDs: []string{"acc_a"}, AddTags: []string{"  "}}},
		{"同时添加与移除", BatchRequest{Operation: "tags", AccountIDs: []string{"acc_a"}, AddTags: []string{"工作"}, RemoveTags: []string{"工作"}}},
		{"标签过长", BatchRequest{Operation: "tags", AccountIDs: []string{"acc_a"}, AddTags: []string{"abcdefghijklmnopqrstuvwxyz0123456789"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeBatchRequest(tc.request); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("应拒绝: %v", err)
			}
		})
	}
	got, err := NormalizeBatchRequest(BatchRequest{Operation: "tags", AccountIDs: []string{"acc_a"}, AddTags: []string{" 工作 ", "工作", "Team"}, RemoveTags: []string{"旧"}})
	if err != nil || !reflect.DeepEqual(got.AddTags, []string{"工作", "Team"}) || !reflect.DeepEqual(got.RemoveTags, []string{"旧"}) {
		t.Fatalf("规范化 = %#v %v", got, err)
	}
}

func TestPlanBatchTagsAndBlockers(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags := []string{"旧", "Team"}
	if _, err := env.m.UpdateMetadata(first, AccountMetadataPatch{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	version := env.m.Version()
	request, err := NormalizeBatchRequest(BatchRequest{Operation: "tags", AccountIDs: []string{first, second}, AddTags: []string{"team", "新"}, RemoveTags: []string{"旧"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := env.m.PlanBatch(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 大小写不敏感：已有 Team 不重复添加 team，并保留原始大小写与顺序。
	if !reflect.DeepEqual(plan.Entries[0].After.Tags, []string{"Team", "新"}) || !reflect.DeepEqual(plan.Entries[1].After.Tags, []string{"team", "新"}) {
		t.Fatalf("标签计划 = %#v", plan.Entries)
	}
	if !plan.CanApply || plan.Summary != (BatchSummary{Total: 2, Change: 2}) || plan.BaseVersion != version || env.m.Version() != version {
		t.Fatalf("计划 = %#v，预检不能改版本", plan)
	}
	if _, err := env.m.PlanBatch(BatchRequest{Operation: "pause", AccountIDs: []string{first, "acc_missing"}}, nil); err == nil {
		t.Fatal("缺失账户应报错")
	} else {
		var missing *BatchMissingError
		if !errors.As(err, &missing) || !reflect.DeepEqual(missing.IDs, []string{"acc_missing"}) {
			t.Fatalf("缺失列表 = %v", err)
		}
	}
	// 被 Provider 引用（含停用）的账户阻断删除，整批不可应用。
	deletePlan, err := env.m.PlanBatch(BatchRequest{Operation: "delete", AccountIDs: []string{first, second}}, map[string][]string{first: {"codex"}})
	if err != nil || deletePlan.CanApply || deletePlan.Summary != (BatchSummary{Total: 2, Change: 1, Blocked: 1}) || len(deletePlan.Blockers) != 1 {
		t.Fatalf("删除计划 = %#v %v", deletePlan, err)
	}
	// 已全部启用时恢复是无变更。
	resume, err := env.m.PlanBatch(BatchRequest{Operation: "resume", AccountIDs: []string{first}}, nil)
	if err != nil || resume.CanApply || !resume.NoChanges {
		t.Fatalf("恢复计划 = %#v %v", resume, err)
	}
}

func TestApplyBatchPauseResumeTagsDelete(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	watch, err := env.m.WatchAdmission(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	ids := []string{first, second}

	version := env.m.Version()
	plan, next, err := env.m.ApplyBatch(BatchRequest{Operation: "pause", AccountIDs: ids}, nil, nil)
	if err != nil || plan.Summary.Change != 2 || next == version || !env.m.IsPaused(first) || !env.m.IsPaused(second) {
		t.Fatalf("批量暂停 = %#v %q %v", plan, next, err)
	}
	if !watch.Paused() {
		t.Fatal("批量暂停应唤醒等待者")
	}
	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "pause", AccountIDs: ids}, nil, nil); !errors.Is(err, ErrBatchNotApplicable) {
		t.Fatalf("无变更的应用应拒绝: %v", err)
	}
	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "resume", AccountIDs: ids}, nil, nil); err != nil || env.m.IsPaused(first) || env.m.IsPaused(second) {
		t.Fatalf("批量恢复 = %v", err)
	}

	request, _ := NormalizeBatchRequest(BatchRequest{Operation: "tags", AccountIDs: ids, AddTags: []string{"批量"}})
	if _, _, err := env.m.ApplyBatch(request, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, view := range env.m.Views(ids, nil) {
		if !reflect.DeepEqual(view.Tags, []string{"批量"}) {
			t.Fatalf("批量标签 = %#v", view.Tags)
		}
	}

	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "delete", AccountIDs: ids}, map[string][]string{first: {"codex"}}, nil); !errors.Is(err, ErrBatchNotApplicable) {
		t.Fatalf("存在引用时整批删除应拒绝: %v", err)
	}
	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "delete", AccountIDs: ids}, nil, nil); err != nil || len(env.m.List(nil)) != 0 {
		t.Fatalf("批量删除 = %v，剩余 %d", err, len(env.m.List(nil)))
	}
	if len(env.open().List(nil)) != 0 {
		t.Fatal("批量删除应落盘")
	}
}

func TestApplyBatchVerifyAndPersistFailureLeaveNoChanges(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ids := []string{first, second}
	version := env.m.Version()
	errStale := errors.New("计划已变化")
	if _, _, err := env.m.ApplyBatch(BatchRequest{Operation: "pause", AccountIDs: ids}, nil, func(*BatchPlan) error { return errStale }); !errors.Is(err, errStale) {
		t.Fatalf("verify 失败应原样返回: %v", err)
	}
	for _, operation := range []string{"pause", "tags", "delete"} {
		request := BatchRequest{Operation: operation, AccountIDs: ids}
		if operation == "tags" {
			request.AddTags = []string{"x"}
		}
		restore := env.failPersist()
		_, _, err := env.m.ApplyBatch(request, nil, nil)
		restore()
		if !errors.Is(err, ErrAccountBatchPersist) {
			t.Fatalf("%s 写盘失败 = %v", operation, err)
		}
		views := env.m.Views(ids, nil)
		if len(views) != 2 || env.m.Version() != version {
			t.Fatalf("%s 写盘失败后应整批回滚: %d 个账户", operation, len(views))
		}
		for _, view := range views {
			if !view.Enabled || len(view.Tags) != 0 {
				t.Fatalf("%s 回滚不完整: %#v", operation, view)
			}
		}
	}
}

func TestBatchFingerprintTracksStateChanges(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	request := BatchRequest{Operation: "pause", AccountIDs: []string{first}}
	before, _ := env.m.PlanBatch(request, nil)
	again, _ := env.m.PlanBatch(request, nil)
	if before.Fingerprint() != again.Fingerprint() {
		t.Fatal("相同状态的指纹应稳定")
	}
	withRef, _ := env.m.PlanBatch(request, map[string][]string{first: {"codex"}})
	if withRef.Fingerprint() == before.Fingerprint() {
		t.Fatal("引用关系变化应改变指纹")
	}
	note := "只改备注"
	if _, err := env.m.UpdateMetadata(first, AccountMetadataPatch{Note: &note}); err != nil {
		t.Fatal(err)
	}
	after, _ := env.m.PlanBatch(request, nil)
	if after.Fingerprint() == before.Fingerprint() {
		t.Fatal("账户控制版本变化应改变指纹")
	}
}

func TestRefreshUsageBatchResults(t *testing.T) {
	env := newTestEnv(t)
	fresh := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	failing := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Chatgpt-Account-Id") == "remote-2" {
			http.Error(w, "fake-failure", http.StatusServiceUnavailable)
			return
		}
		serveUsage(fakeUsagePayload)(w, r)
	})
	ids := []string{fresh, failing, "acc_missing"}
	results := env.m.RefreshUsageBatch(context.Background(), ids, false, 5*time.Second)
	if len(results) != 3 || results[0].Status != "success" || results[1].Status != "failure" || results[1].Category == "" || results[2].Status != "not_found" {
		t.Fatalf("首次刷新 = %#v", results)
	}
	calls := env.up.usageCalls.Load()
	results = env.m.RefreshUsageBatch(context.Background(), ids[:1], false, 5*time.Second)
	if results[0].Status != "cached" || env.up.usageCalls.Load() != calls {
		t.Fatalf("缓存周期内应复用快照且不出网: %#v", results)
	}
	results = env.m.RefreshUsageBatch(context.Background(), ids[:1], true, 5*time.Second)
	if results[0].Status != "success" || env.up.usageCalls.Load() != calls+1 {
		t.Fatalf("force 应重新查询: %#v", results)
	}
	// 暂停账户仍可手动刷新，且刷新不推进账户控制版本。
	if _, _, _, err := env.m.SetPaused(fresh, true, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	version := env.m.Version()
	results = env.m.RefreshUsageBatch(context.Background(), ids[:1], true, 5*time.Second)
	if results[0].Status != "success" || env.m.Version() != version {
		t.Fatalf("暂停账户刷新 = %#v，版本 %q→%q", results, version, env.m.Version())
	}
}

func TestRefreshUsageBatchBudgetCancelsWithoutFailureSnapshot(t *testing.T) {
	env := newTestEnv(t)
	ids := make([]string, 0, 4)
	for i := 1; i <= 4; i++ {
		ids = append(ids, env.addAccount(fmt.Sprintf("remote-%d", i), fmt.Sprintf("u%d@example.invalid", i), "fake-refresh", env.clock.Now().Add(time.Hour)))
	}
	started := make(chan struct{}, 4)
	env.up.setUsage(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	results := env.m.RefreshUsageBatch(context.Background(), ids, true, 150*time.Millisecond)
	if got := env.up.usageCalls.Load(); got != 3 {
		t.Fatalf("并发上限应为 3，实际出网 %d 次", got)
	}
	for _, result := range results {
		if result.Status != "cancelled" || result.Category != "" {
			t.Fatalf("预算耗尽应全部记为取消: %#v", results)
		}
	}
	for _, view := range env.m.Views(ids, nil) {
		if view.Usage != nil && view.Usage.Error != "" {
			t.Fatalf("取消不能写入失败快照: %#v", view.Usage)
		}
	}
}
