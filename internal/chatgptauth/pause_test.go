package chatgptauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 第三阶段：账户暂停、准入监听。只使用临时目录与假账户。

func TestSetPausedLifecycleAndVersion(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	version := env.m.Version()
	if _, _, _, err := env.m.SetPaused(id, true, "stale"); !errors.Is(err, ErrAccountVersionConflict) {
		t.Fatalf("过期版本 = %v", err)
	}
	if _, _, _, err := env.m.SetPaused("acc_missing", true, version); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在账户 = %v", err)
	}
	view, changed, next, err := env.m.SetPaused(id, true, version)
	if err != nil || !changed || next == version || view.Enabled || view.PausedAt == "" {
		t.Fatalf("暂停 = %#v changed=%v version=%q err=%v", view, changed, next, err)
	}
	if !env.m.IsPaused(id) {
		t.Fatal("IsPaused 应为 true")
	}
	// 目标状态与当前相同：不写盘、不推进版本。
	_, changed, same, err := env.m.SetPaused(id, true, next)
	if err != nil || changed || same != next {
		t.Fatalf("重复暂停 changed=%v version=%q err=%v", changed, same, err)
	}
	reopened, ok := env.open().Account(id)
	if !ok || reopened.Enabled || reopened.PausedAt != view.PausedAt {
		t.Fatalf("重启后暂停状态 = %#v", reopened)
	}
	view, changed, _, err = env.m.SetPaused(id, false, next)
	if err != nil || !changed || !view.Enabled || view.PausedAt != "" {
		t.Fatalf("恢复 = %#v changed=%v err=%v", view, changed, err)
	}
}

func TestSetPausedPersistFailureRollsBackAndDoesNotWake(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	watch, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	version := env.m.Version()
	restore := env.failPersist()
	_, _, _, err = env.m.SetPaused(id, true, version)
	restore()
	if !errors.Is(err, ErrAccountStatePersist) {
		t.Fatalf("写盘失败 = %v", err)
	}
	if env.m.IsPaused(id) || env.m.Version() != version {
		t.Fatal("写盘失败后应回滚内存状态且不推进版本")
	}
	if watch.Context().Err() != nil || watch.Paused() {
		t.Fatal("写盘失败时账户仍启用，不能唤醒等待者")
	}
}

func TestWatchAdmissionWakesOnPauseOnly(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	other := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	if _, err := env.m.WatchAdmission(context.Background(), "acc_missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在账户 = %v", err)
	}
	watch, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	released, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	released.Release()
	released.Release() // 可重复调用
	untouched, err := env.m.WatchAdmission(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	defer untouched.Release()

	if _, _, _, err := env.m.SetPaused(id, true, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watch.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("暂停后排队中的等待者未被唤醒")
	}
	if !watch.Paused() || released.Paused() || untouched.Context().Err() != nil {
		t.Fatalf("唤醒范围错误: watch=%v released=%v untouched=%v", watch.Paused(), released.Paused(), untouched.Context().Err())
	}
	watch.Release()
	if _, err := env.m.WatchAdmission(context.Background(), id); !errors.Is(err, ErrAccountPaused) {
		t.Fatalf("已暂停账户应拒绝新的准入监听: %v", err)
	}
	// 恢复不会唤醒任何人，之后可以重新登记。
	if _, _, _, err := env.m.SetPaused(id, false, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	again, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	again.Release()
	var nilWatch *AdmissionWatch
	nilWatch.Release()
	if nilWatch.Paused() {
		t.Fatal("nil 监听不应报告暂停")
	}
}

func TestWatchAdmissionParentCancelIsNotPause(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	parent, cancel := context.WithCancel(context.Background())
	watch, err := env.m.WatchAdmission(parent, id)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	cancel()
	<-watch.Context().Done()
	if watch.Paused() {
		t.Fatal("客户端取消不能被当成账户暂停")
	}
}

func TestPauseSurvivesCredentialUpdateAndIsNotExported(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	if _, _, _, err := env.m.SetPaused(id, true, env.m.Version()); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"cockpit_tools", "cliproxyapi", "sub2api", "auth_json"} {
		file, err := env.m.Export(format, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(file.Data), "paused") {
			t.Fatalf("%s 导出包含暂停状态", format)
		}
	}
	actions := []ImportAction{{Index: 1, Action: "update", TargetAccountID: id}, {Index: 3, Action: "skip"}}
	if _, err := env.m.ApplyImport([]byte(planPayload), false, actions, nil, nil); err != nil {
		t.Fatal(err)
	}
	view, _ := env.m.Account(id)
	if view.Enabled || view.PausedAt == "" {
		t.Fatalf("显式凭据更新应保留暂停状态: %#v", view)
	}
	for _, account := range env.m.List(nil) {
		if account.ID != id && !account.Enabled {
			t.Fatalf("导入新增账户应默认启用: %#v", account)
		}
	}
}
