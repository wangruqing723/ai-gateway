package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/backupcrypto"
)

// 测试统一使用的假备份密码；只用于假账户。
const testBackupPassword = "test-backup-password"

// fastBackups 让管理器用最低允许迭代次数加密，缩短测试耗时；生产固定 600,000 次。
func fastBackups(m *Manager) *Manager {
	m.backupIterations = backupcrypto.MinIterations
	return m
}

// newBackupEnv 创建使用快速迭代的测试环境。
func newBackupEnv(t *testing.T) *testEnv {
	env := newTestEnv(t)
	fastBackups(env.m)
	return env
}

// editAccount 在写锁内修改账户并落盘，同时推进账户版本。
func (env *testEnv) editAccount(id string, edit func(*account)) {
	env.t.Helper()
	unlock, err := env.m.lockForWrite(context.Background())
	if err != nil {
		env.t.Fatal(err)
	}
	defer unlock()
	item := env.m.accounts[id]
	if item == nil {
		env.t.Fatalf("账户 %s 不存在", id)
	}
	edit(item)
	if err := env.m.persistLocked(); err != nil {
		env.t.Fatal(err)
	}
	env.m.bumpVersionLocked()
}

// rekeyAccount 把账户换成指定本地 ID，用于构造源 ID 冲突。
func (env *testEnv) rekeyAccount(from, to string) {
	env.t.Helper()
	unlock, err := env.m.lockForWrite(context.Background())
	if err != nil {
		env.t.Fatal(err)
	}
	defer unlock()
	item := env.m.accounts[from]
	delete(env.m.accounts, from)
	item.ID = to
	env.m.accounts[to] = item
	if err := env.m.persistLocked(); err != nil {
		env.t.Fatal(err)
	}
	env.m.bumpVersionLocked()
}

// persisted 返回账户可持久化字段的副本；不存在时返回 nil。
func persisted(m *Manager, id string) *account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item == nil {
		return nil
	}
	saved := &account{}
	copyPersistentFields(saved, item)
	return saved
}

func (env *testEnv) exportBackup(ids ...string) []byte {
	env.t.Helper()
	container, err := env.m.ExportBackup(ids, testBackupPassword, env.m.Version())
	if err != nil {
		env.t.Fatalf("导出备份: %v", err)
	}
	return container
}

// sealPayload 直接加密任意明文，用于构造导出路径产生不了的备份内容。
func (env *testEnv) sealPayload(plaintext string) []byte {
	env.t.Helper()
	container, err := env.m.sealBackup([]byte(plaintext), testBackupPassword)
	if err != nil {
		env.t.Fatal(err)
	}
	return container
}

func (env *testEnv) planRestore(container []byte, options RestoreOptions, actions ...RestoreAction) *RestorePlan {
	env.t.Helper()
	plan, err := env.m.PlanRestore(container, testBackupPassword, options, actions, nil)
	if err != nil {
		env.t.Fatalf("恢复预检: %v", err)
	}
	return plan
}

func (env *testEnv) applyRestore(container []byte, options RestoreOptions, actions ...RestoreAction) RestoreResult {
	env.t.Helper()
	result, err := env.m.ApplyRestore(container, testBackupPassword, options, actions, nil, nil)
	if err != nil {
		env.t.Fatalf("应用恢复: %v", err)
	}
	return result
}

func TestExportBackupValidation(t *testing.T) {
	env := newBackupEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	version := env.m.Version()
	cases := []struct {
		name     string
		ids      []string
		password string
		version  string
		want     error
	}{
		{"密码过短", []string{id}, "short", version, backupcrypto.ErrInvalidPassword},
		{"空选择", nil, testBackupPassword, version, ErrBackupSelection},
		{"重复选择", []string{id, id}, testBackupPassword, version, ErrBackupSelection},
		{"账户不存在", []string{id, "acc_ffff"}, testBackupPassword, version, ErrBackupSelection},
		{"版本过期", []string{id}, testBackupPassword, "stale-version", ErrAccountVersionConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.m.ExportBackup(tc.ids, tc.password, tc.version); !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.want)
			}
		})
	}
	tooMany := make([]string, MaxBackupAccounts+1)
	for i := range tooMany {
		tooMany[i] = id + strings.Repeat("0", i)
	}
	if _, err := env.m.ExportBackup(tooMany, testBackupPassword, version); !errors.Is(err, ErrBackupSelection) {
		t.Fatalf("超过上限的错误 = %v", err)
	}
	before, _ := os.ReadFile(env.path)
	container := env.exportBackup(id)
	after, _ := os.ReadFile(env.path)
	if string(before) != string(after) || env.m.Version() != version || env.up.tokenCalls.Load() != 0 {
		t.Fatal("导出不能落盘、推进版本或刷新令牌")
	}
	for _, secret := range []string{"fake-refresh", "fake-access-remote-1", "one@example.invalid"} {
		if strings.Contains(string(container), secret) {
			t.Fatalf("加密容器包含明文 %q", secret)
		}
	}
	info, err := backupcrypto.Inspect(container)
	if err != nil || info.Iterations != backupcrypto.MinIterations {
		t.Fatalf("容器信息 = %#v err=%v", info, err)
	}
}

func TestRestoreRoundTripKeepsSourceIDsAndMetadata(t *testing.T) {
	source := newBackupEnv(t)
	first := source.addAccount("remote-1", "one@example.invalid", "refresh-1", source.clock.Now().Add(time.Hour))
	second := source.addAccount("remote-2", "two@example.invalid", "refresh-2", source.clock.Now().Add(time.Hour))
	pausedAt := source.clock.Now().Add(-time.Minute).UTC()
	source.editAccount(second, func(item *account) {
		item.Tags = []string{"团队", "备用"}
		item.Note = "备份备注"
		item.Paused = true
		item.PausedAt = &pausedAt
	})
	container := source.exportBackup(first, second)

	dest := newBackupEnv(t)
	recorder := recordEvents(dest.m)
	before, _ := os.ReadFile(dest.path)
	plan := dest.planRestore(container, RestoreOptions{})
	after, _ := os.ReadFile(dest.path)
	if string(before) != string(after) || dest.up.tokenCalls.Load() != 0 || dest.up.usageCalls.Load() != 0 {
		t.Fatal("恢复预检不能落盘或访问上游")
	}
	if !plan.CanApply || plan.Metadata != RestoreMetadataKeep || plan.RestorePaused ||
		plan.Summary != (ImportPlanSummary{Total: 2, Add: 2}) {
		t.Fatalf("计划 = %#v", plan)
	}
	entry := plan.Entries[1]
	if entry.Status != "new" || entry.Action != "add" || entry.TargetAccountID != second || entry.Remapped ||
		!entry.MetadataChanges || entry.PausedChange || !entry.HasNote || !entry.Paused ||
		!reflect.DeepEqual(entry.Tags, []string{"团队", "备用"}) {
		t.Fatalf("第 2 条计划 = %#v", entry)
	}
	encoded, _ := json.Marshal(plan)
	for _, secret := range []string{"refresh-1", "refresh-2", "fake-access-remote", "备份备注", "eyJ"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("恢复计划包含敏感内容 %q", secret)
		}
	}

	result := dest.applyRestore(container, RestoreOptions{})
	wantMappings := []RestoreMapping{
		{SourceAccountID: first, TargetAccountID: first, Action: "add"},
		{SourceAccountID: second, TargetAccountID: second, Action: "add"},
	}
	if result.Imported != 2 || result.Updated != 0 || !reflect.DeepEqual(result.Mappings, wantMappings) || len(result.Accounts) != 2 {
		t.Fatalf("恢复结果 = %#v", result)
	}
	assertKinds(t, recorder)
	reopened := dest.open()
	for _, pair := range [][2]string{{first, "refresh-1"}, {second, "refresh-2"}} {
		got := persisted(reopened, pair[0])
		want := persisted(source.m, pair[0])
		if got == nil || got.RefreshToken != pair[1] || got.AccessToken != want.AccessToken || got.IDToken != want.IDToken ||
			got.AccountID != want.AccountID || !got.ExpiresAt.Equal(want.ExpiresAt) || !got.AuthorizedAt.Equal(*want.AuthorizedAt) {
			t.Fatalf("恢复后的账户 %s = %#v", pair[0], got)
		}
	}
	restored := persisted(reopened, second)
	if !reflect.DeepEqual(restored.Tags, []string{"团队", "备用"}) || restored.Note != "备份备注" || restored.Paused || restored.PausedAt != nil {
		t.Fatalf("默认恢复应保留标签备注且不恢复暂停：%#v", restored)
	}
}

func TestRestorePausedAddKeepsPauseTime(t *testing.T) {
	source := newBackupEnv(t)
	id := source.addAccount("remote-1", "one@example.invalid", "refresh-1", source.clock.Now().Add(time.Hour))
	pausedAt := source.clock.Now().Add(-time.Minute).UTC()
	source.editAccount(id, func(item *account) { item.Paused, item.PausedAt = true, &pausedAt })
	container := source.exportBackup(id)

	dest := newBackupEnv(t)
	recorder := recordEvents(dest.m)
	options := RestoreOptions{RestorePaused: true}
	if plan := dest.planRestore(container, options); !plan.Entries[0].PausedChange || !plan.RestorePaused {
		t.Fatalf("计划 = %#v", plan.Entries[0])
	}
	dest.applyRestore(container, options)
	got := persisted(dest.m, id)
	if !got.Paused || got.PausedAt == nil || !got.PausedAt.Equal(pausedAt) || !dest.m.IsPaused(id) {
		t.Fatalf("恢复暂停状态 = %#v", got)
	}
	// 新增账户本来就不存在，恢复为暂停不是一次暂停操作，不产生事件。
	assertKinds(t, recorder)
}

func TestRestoreRemapsCollidingOrInvalidSourceID(t *testing.T) {
	source := newBackupEnv(t)
	id := source.addAccount("remote-1", "one@example.invalid", "refresh-1", source.clock.Now().Add(time.Hour))
	container := source.exportBackup(id)

	dest := newBackupEnv(t)
	other := dest.addAccount("remote-other", "other@example.invalid", "other-refresh", dest.clock.Now().Add(time.Hour))
	dest.rekeyAccount(other, id)
	plan := dest.planRestore(container, RestoreOptions{})
	entry := plan.Entries[0]
	if !plan.CanApply || entry.Status != "new" || !entry.Remapped || entry.TargetAccountID != "" ||
		!strings.Contains(strings.Join(entry.Warnings, "\n"), "已被其他账户使用") {
		t.Fatalf("冲突计划 = %#v", entry)
	}
	result := dest.applyRestore(container, RestoreOptions{})
	if len(result.Mappings) != 1 {
		t.Fatalf("映射 = %#v", result.Mappings)
	}
	mapping := result.Mappings[0]
	if mapping.SourceAccountID != id || mapping.TargetAccountID == id || !mapping.Remapped || mapping.Action != "add" ||
		!validBackupAccountID(mapping.TargetAccountID) {
		t.Fatalf("冲突映射 = %#v", mapping)
	}
	if kept := persisted(dest.m, id); kept == nil || kept.AccountID != "remote-other" || kept.RefreshToken != "other-refresh" {
		t.Fatalf("已有账户被覆盖：%#v", kept)
	}
	if added := persisted(dest.m, mapping.TargetAccountID); added == nil || added.AccountID != "remote-1" || added.RefreshToken != "refresh-1" {
		t.Fatalf("重映射后的账户 = %#v", added)
	}

	// 源本地 ID 格式不合法时同样生成新 ID。
	invalidID := newBackupEnv(t)
	payload := `{"version":1,"accounts":[{"id":"../evil","chatgptAccountId":"remote-9","accessToken":"a","refreshToken":"r"}]}`
	container = invalidID.sealPayload(payload)
	entry = invalidID.planRestore(container, RestoreOptions{}).Entries[0]
	if !entry.Remapped || !strings.Contains(strings.Join(entry.Warnings, "\n"), "格式不合法") {
		t.Fatalf("非法 ID 计划 = %#v", entry)
	}
	result = invalidID.applyRestore(container, RestoreOptions{})
	if len(result.Mappings) != 1 || result.Mappings[0].SourceAccountID != "../evil" || !result.Mappings[0].Remapped ||
		!validBackupAccountID(result.Mappings[0].TargetAccountID) {
		t.Fatalf("非法 ID 映射 = %#v", result.Mappings)
	}
}

// updateFixture 构造：备份里 remote-1 带新凭据和备份元数据，目标里 remote-1 有旧凭据和本地元数据。
func updateFixture(t *testing.T) (container []byte, sourceID string, dest *testEnv, targetID string) {
	source := newBackupEnv(t)
	sourceID = source.addAccount("remote-1", "one@example.invalid", "backup-refresh", source.clock.Now().Add(2*time.Hour))
	source.editAccount(sourceID, func(item *account) {
		item.AccessToken = "backup-access"
		item.Tags = []string{"备份标签"}
		item.Note = "备份备注"
		item.LastRefresh = source.clock.Now()
	})
	container = source.exportBackup(sourceID)
	dest = newBackupEnv(t)
	targetID = dest.addAccount("remote-1", "one@example.invalid", "local-refresh", dest.clock.Now().Add(time.Hour))
	dest.editAccount(targetID, func(item *account) {
		item.Tags = []string{"本地标签"}
		item.Note = "本地备注"
		item.LastRefresh = dest.clock.Now().Add(-2 * time.Hour)
	})
	return container, sourceID, dest, targetID
}

func TestRestoreExistingIdentityDefaultsToSkip(t *testing.T) {
	container, sourceID, dest, targetID := updateFixture(t)
	plan := dest.planRestore(container, RestoreOptions{})
	entry := plan.Entries[0]
	if entry.Status != "existing" || entry.MatchedAccountID != targetID || entry.Action != "skip" ||
		!reflect.DeepEqual(entry.AllowedActions, []string{"skip", "update"}) || plan.CanApply || !plan.NoChanges {
		t.Fatalf("默认计划 = %#v canApply=%v", entry, plan.CanApply)
	}
	if _, err := dest.m.ApplyRestore(container, testBackupPassword, RestoreOptions{}, nil, nil, nil); !errors.Is(err, ErrRestorePlanNotApplicable) {
		t.Fatalf("全部跳过的应用错误 = %v", err)
	}
	wrong := dest.planRestore(container, RestoreOptions{}, RestoreAction{Index: 1, Action: "update", TargetAccountID: sourceID + "0"})
	if wrong.CanApply || !strings.Contains(strings.Join(wrong.Blockers, "\n"), "上游身份一致") {
		t.Fatalf("错误目标计划 = %#v", wrong.Blockers)
	}
}

func TestRestoreUpdateKeepsMetadataByDefault(t *testing.T) {
	container, sourceID, dest, targetID := updateFixture(t)
	recorder := recordEvents(dest.m)
	before, _ := dest.m.CredentialVersion(targetID)
	update := RestoreAction{Index: 1, Action: "update", TargetAccountID: targetID}
	plan := dest.planRestore(container, RestoreOptions{}, update)
	if entry := plan.Entries[0]; !plan.CanApply || entry.TargetAccountID != targetID || entry.MetadataChanges || entry.PausedChange {
		t.Fatalf("更新计划 = %#v", entry)
	}
	result := dest.applyRestore(container, RestoreOptions{}, update)
	want := []RestoreMapping{{SourceAccountID: sourceID, TargetAccountID: targetID, Action: "update", Remapped: true}}
	if result.Updated != 1 || result.Imported != 0 || !reflect.DeepEqual(result.Mappings, want) {
		t.Fatalf("更新结果 = %#v", result)
	}
	got := persisted(dest.open(), targetID)
	if got.AccessToken != "backup-access" || got.RefreshToken != "backup-refresh" ||
		!reflect.DeepEqual(got.Tags, []string{"本地标签"}) || got.Note != "本地备注" {
		t.Fatalf("keep 策略更新后 = %#v", got)
	}
	if after, _ := dest.m.CredentialVersion(targetID); after != before+1 {
		t.Fatalf("凭据世代 %d -> %d，期望推进一次", before, after)
	}
	assertKinds(t, recorder, EventCredentialsUpdated)
	if detail := recorder.all()[0].Detail; !strings.Contains(detail, "备份恢复") {
		t.Fatalf("事件说明 = %q", detail)
	}
}

func TestRestoreUpdateReplaceMetadataAndPause(t *testing.T) {
	container, _, dest, targetID := updateFixture(t)
	recorder := recordEvents(dest.m)
	update := RestoreAction{Index: 1, Action: "update", TargetAccountID: targetID}
	options := RestoreOptions{Metadata: RestoreMetadataReplace}
	if entry := dest.planRestore(container, options, update).Entries[0]; !entry.MetadataChanges {
		t.Fatalf("replace 计划 = %#v", entry)
	}
	dest.applyRestore(container, options, update)
	got := persisted(dest.m, targetID)
	if !reflect.DeepEqual(got.Tags, []string{"备份标签"}) || got.Note != "备份备注" || got.Paused {
		t.Fatalf("replace 策略更新后 = %#v", got)
	}
	assertKinds(t, recorder, EventCredentialsUpdated)

	// 备份中账户已暂停：只有 restorePaused 才把已有账户改成暂停，并产生暂停事件。
	pausedSource := newBackupEnv(t)
	id := pausedSource.addAccount("remote-1", "one@example.invalid", "paused-refresh", pausedSource.clock.Now().Add(time.Hour))
	pausedSource.editAccount(id, func(item *account) { item.Paused = true; item.LastRefresh = pausedSource.clock.Now() })
	paused := pausedSource.exportBackup(id)
	if entry := dest.planRestore(paused, RestoreOptions{}, update).Entries[0]; entry.PausedChange {
		t.Fatalf("未选 restorePaused 不应改变暂停：%#v", entry)
	}
	pauseOptions := RestoreOptions{RestorePaused: true}
	if entry := dest.planRestore(paused, pauseOptions, update).Entries[0]; !entry.PausedChange {
		t.Fatalf("restorePaused 计划 = %#v", entry)
	}
	dest.applyRestore(paused, pauseOptions, update)
	if got := persisted(dest.m, targetID); !got.Paused || got.PausedAt == nil || !dest.m.IsPaused(targetID) {
		t.Fatalf("restorePaused 更新后 = %#v", got)
	}
	assertKinds(t, recorder, EventCredentialsUpdated, EventCredentialsUpdated, EventPaused)
}

func TestRestoreUnsafeUpdatesAreSkipOnly(t *testing.T) {
	// 本地多个账户使用同一上游身份：不能自动挑选更新目标。
	container, _, dest, targetID := updateFixture(t)
	dest.addAccount("remote-1", "one-copy@example.invalid", "copy-refresh", dest.clock.Now().Add(time.Hour))
	entry := dest.planRestore(container, RestoreOptions{}).Entries[0]
	if !reflect.DeepEqual(entry.AllowedActions, []string{"skip"}) || !strings.Contains(strings.Join(entry.Warnings, "\n"), "多个账户") {
		t.Fatalf("多匹配计划 = %#v", entry)
	}
	plan := dest.planRestore(container, RestoreOptions{}, RestoreAction{Index: 1, Action: "update", TargetAccountID: targetID})
	if plan.CanApply || len(plan.Entries[0].Errors) == 0 {
		t.Fatalf("多匹配不应允许更新：%#v", plan.Entries[0])
	}

	// 没有 refresh_token 的记录不能更新已有账户，但仍可新增未知身份。
	env := newBackupEnv(t)
	existing := env.addAccount("remote-1", "one@example.invalid", "local-refresh", env.clock.Now().Add(time.Hour))
	payload := `{"version":1,"accounts":[
	  {"id":"acc_01","chatgptAccountId":"remote-1","accessToken":"only-access"},
	  {"id":"acc_02","chatgptAccountId":"remote-2","accessToken":"only-access-2"}]}`
	container = env.sealPayload(payload)
	plan = env.planRestore(container, RestoreOptions{}, RestoreAction{Index: 1, Action: "update", TargetAccountID: existing})
	if plan.CanApply || !strings.Contains(strings.Join(plan.Entries[0].Warnings, "\n"), "不能用于更新") {
		t.Fatalf("无刷新凭据更新计划 = %#v", plan.Entries[0])
	}
	added := plan.Entries[1]
	if added.Status != "new" || added.Action != "add" || added.HasRefreshToken ||
		!strings.Contains(strings.Join(added.Warnings, "\n"), "没有 refresh_token") {
		t.Fatalf("无刷新凭据新增计划 = %#v", added)
	}
	// 默认跳过已有身份、新增未知身份；跳过的已有身份也给出映射，供绑定迁移预填。
	result := env.applyRestore(container, RestoreOptions{})
	want := []RestoreMapping{
		{SourceAccountID: "acc_01", TargetAccountID: existing, Action: "skip", Remapped: true},
		{SourceAccountID: "acc_02", TargetAccountID: "acc_02", Action: "add"},
	}
	if result.Imported != 1 || result.Skipped != 1 || !reflect.DeepEqual(result.Mappings, want) {
		t.Fatalf("混合恢复结果 = %#v", result)
	}
	if got := persisted(env.m, existing); got.RefreshToken != "local-refresh" {
		t.Fatalf("跳过的账户被改动：%#v", got)
	}
}

func TestRestoreInvalidAndDuplicateEntries(t *testing.T) {
	env := newBackupEnv(t)
	payload := `{"version":1,"createdAt":"2030-01-01T00:00:00Z","accounts":[
	  {"id":"acc_0a","chatgptAccountId":"remote-1","accessToken":"a1","refreshToken":"r1"},
	  {"id":"acc_0b","chatgptAccountId":"","accessToken":"a2","refreshToken":"r2"},
	  {"id":"acc_0c","chatgptAccountId":"remote-1","accessToken":"a3","refreshToken":"r3"},
	  {"id":"acc_0d","chatgptAccountId":"remote-4","accessToken":"a4","refreshToken":"r4","expiresAt":"不是时间"}]}`
	container := env.sealPayload(payload)
	plan := env.planRestore(container, RestoreOptions{})
	if plan.CanApply || plan.CreatedAt != "2030-01-01T00:00:00Z" ||
		plan.Summary != (ImportPlanSummary{Total: 4, Add: 1, Skip: 1, Invalid: 2, Blocked: 2}) {
		t.Fatalf("默认计划汇总 = %#v canApply=%v", plan.Summary, plan.CanApply)
	}
	if entry := plan.Entries[1]; entry.Status != "invalid" || entry.InvalidReason != "缺少上游账户 ID" || entry.Action != "" {
		t.Fatalf("无效条目 = %#v", entry)
	}
	if entry := plan.Entries[2]; entry.DuplicateOf != 1 || entry.Action != "skip" {
		t.Fatalf("重复条目 = %#v", entry)
	}
	if entry := plan.Entries[3]; entry.Status != "invalid" || !strings.Contains(entry.InvalidReason, "expiresAt") {
		t.Fatalf("时间无效条目 = %#v", entry)
	}
	skipInvalid := []RestoreAction{{Index: 2, Action: "skip"}, {Index: 4, Action: "skip"}}
	plan = env.planRestore(container, RestoreOptions{}, skipInvalid...)
	if !plan.CanApply || plan.Summary != (ImportPlanSummary{Total: 4, Add: 1, Skip: 3, Invalid: 2}) {
		t.Fatalf("显式跳过后的汇总 = %#v", plan.Summary)
	}
	duplicateAdd := append(append([]RestoreAction{}, skipInvalid...), RestoreAction{Index: 3, Action: "add"})
	plan = env.planRestore(container, RestoreOptions{}, duplicateAdd...)
	if plan.CanApply || !strings.Contains(strings.Join(plan.Entries[2].Errors, "\n"), "与第 1 条是同一上游身份") {
		t.Fatalf("同身份重复新增 = %#v", plan.Entries[2])
	}
	plan = env.planRestore(container, RestoreOptions{}, RestoreAction{Index: 2, Action: "add"}, RestoreAction{Index: 4, Action: "skip"})
	if plan.CanApply || !reflect.DeepEqual(plan.Entries[1].Errors, []string{"无效条目只能跳过"}) {
		t.Fatalf("无效条目选择新增 = %#v", plan.Entries[1])
	}
	result := env.applyRestore(container, RestoreOptions{}, skipInvalid...)
	if result.Imported != 1 || result.Skipped != 3 || len(result.Mappings) != 1 || result.Mappings[0].TargetAccountID != "acc_0a" {
		t.Fatalf("恢复结果 = %#v", result)
	}
}

func TestRestoreRejectsInvalidPayloadsAndRequests(t *testing.T) {
	env := newBackupEnv(t)
	for name, payload := range map[string]string{
		"版本":   `{"version":2,"accounts":[{"chatgptAccountId":"r","accessToken":"a"}]}`,
		"空账户":  `{"version":1,"accounts":[]}`,
		"未知字段": `{"version":1,"accounts":[{"chatgptAccountId":"r","accessToken":"a"}],"extra":1}`,
		"多余内容": `{"version":1,"accounts":[{"chatgptAccountId":"r","accessToken":"a"}]} {}`,
		"非对象":  `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := env.m.PlanRestore(env.sealPayload(payload), testBackupPassword, RestoreOptions{}, nil, nil); !errors.Is(err, ErrInvalidBackup) {
				t.Fatalf("错误 = %v", err)
			}
		})
	}
	tooMany := make([]string, MaxBackupAccounts+1)
	for i := range tooMany {
		tooMany[i] = `{"chatgptAccountId":"r","accessToken":"a"}`
	}
	payload := `{"version":1,"accounts":[` + strings.Join(tooMany, ",") + `]}`
	if _, err := env.m.PlanRestore(env.sealPayload(payload), testBackupPassword, RestoreOptions{}, nil, nil); !errors.Is(err, ErrInvalidBackup) {
		t.Fatalf("超过账户上限的错误 = %v", err)
	}

	id := env.addAccount("remote-1", "one@example.invalid", "refresh-1", env.clock.Now().Add(time.Hour))
	container := env.exportBackup(id)
	if _, err := env.m.PlanRestore(container, "wrong-password-value", RestoreOptions{}, nil, nil); !errors.Is(err, backupcrypto.ErrDecrypt) {
		t.Fatalf("错误密码 = %v", err)
	}
	// 选项先于密钥派生校验：错误密码加无效选项仍报选项错误。
	if _, err := env.m.PlanRestore(container, "wrong-password-value", RestoreOptions{Metadata: "merge"}, nil, nil); !errors.Is(err, ErrInvalidRestoreAction) {
		t.Fatalf("无效 metadata = %v", err)
	}
	for name, actions := range map[string][]RestoreAction{
		"序号越界": {{Index: 2, Action: "skip"}},
		"重复序号": {{Index: 1, Action: "skip"}, {Index: 1, Action: "add"}},
		"未知动作": {{Index: 1, Action: "delete"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := env.m.PlanRestore(container, testBackupPassword, RestoreOptions{}, actions, nil); !errors.Is(err, ErrInvalidRestoreAction) {
				t.Fatalf("错误 = %v", err)
			}
		})
	}
	if _, err := env.m.ApplyRestore(container, "wrong-password-value", RestoreOptions{}, nil, nil, nil); !errors.Is(err, backupcrypto.ErrDecrypt) {
		t.Fatalf("应用时错误密码 = %v", err)
	}
}

func TestRestoreWarnsAboutStaleBackup(t *testing.T) {
	source := newBackupEnv(t)
	id := source.addAccount("remote-1", "one@example.invalid", "old-refresh", source.clock.Now().Add(time.Hour))
	container := source.exportBackup(id)
	dest := newBackupEnv(t)
	target := dest.addAccount("remote-1", "one@example.invalid", "new-refresh", dest.clock.Now().Add(time.Hour))
	dest.editAccount(target, func(item *account) { item.LastRefresh = dest.clock.Now(); item.NeedsReauth = true })
	entry := dest.planRestore(container, RestoreOptions{}, RestoreAction{Index: 1, Action: "update", TargetAccountID: target}).Entries[0]
	warnings := strings.Join(entry.Warnings, "\n")
	if entry.Action != "update" || !strings.Contains(warnings, "可能是旧备份") || !strings.Contains(warnings, "更新凭据可恢复") {
		t.Fatalf("旧备份提醒 = %#v", entry.Warnings)
	}
}

func TestRestoreRollbackAndVerify(t *testing.T) {
	container, _, dest, target := updateFixture(t)
	actions := []RestoreAction{{Index: 1, Action: "update", TargetAccountID: target}}
	before, err := os.ReadFile(dest.path)
	if err != nil {
		t.Fatal(err)
	}
	version := dest.m.Version()
	credentials, _ := dest.m.CredentialVersion(target)
	recorder := recordEvents(dest.m)
	denied := errors.New("计划已失效")
	_, err = dest.m.ApplyRestore(container, testBackupPassword, RestoreOptions{}, actions, nil, func(p *RestorePlan) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatalf("校验错误 = %v", err)
	}
	restore := dest.failPersist()
	_, err = dest.m.ApplyRestore(container, testBackupPassword, RestoreOptions{Metadata: RestoreMetadataReplace}, actions, nil, nil)
	restore()
	if !errors.Is(err, ErrAccountRestorePersist) {
		t.Fatalf("落盘错误 = %v", err)
	}
	after, err := os.ReadFile(dest.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || dest.m.Version() != version {
		t.Fatal("失败改变了磁盘或版本")
	}
	got := persisted(dest.m, target)
	if got.RefreshToken != "local-refresh" || got.Note != "本地备注" {
		t.Fatal("失败未回滚内存")
	}
	if current, _ := dest.m.CredentialVersion(target); current != credentials {
		t.Fatal("失败推进了凭据世代")
	}
	assertKinds(t, recorder)

	// 新增也必须在落盘失败时完整撤销。
	empty := newBackupEnv(t)
	restore = empty.failPersist()
	_, err = empty.m.ApplyRestore(container, testBackupPassword, RestoreOptions{}, nil, nil, nil)
	restore()
	if !errors.Is(err, ErrAccountRestorePersist) || len(empty.m.List(nil)) != 0 {
		t.Fatalf("新增回滚失败: %v", err)
	}
}

func TestRestoreFingerprintTracksCredentialsAndOptions(t *testing.T) {
	container, _, dest, target := updateFixture(t)
	update := RestoreAction{Index: 1, Action: "update", TargetAccountID: target}
	plan := dest.planRestore(container, RestoreOptions{}, update)
	fingerprint := plan.Fingerprint()
	if fingerprint == dest.planRestore(container, RestoreOptions{Metadata: RestoreMetadataReplace}, update).Fingerprint() {
		t.Fatal("元数据策略未绑定")
	}
	if fingerprint == dest.planRestore(container, RestoreOptions{RestorePaused: true}, update).Fingerprint() {
		t.Fatal("暂停策略未绑定")
	}
	if fingerprint == dest.planRestore(container, RestoreOptions{}).Fingerprint() {
		t.Fatal("动作未绑定")
	}
	dest.m.mu.Lock()
	dest.m.accounts[target].credentialVersion++
	dest.m.mu.Unlock()
	if fingerprint == dest.planRestore(container, RestoreOptions{}, update).Fingerprint() {
		t.Fatal("凭据世代未绑定")
	}
	// 已生成计划的指纹不能随账户指针内容变化。
	if fingerprint != plan.Fingerprint() {
		t.Fatal("旧计划指纹被可变状态改变")
	}
}

func TestRestoreAcrossInstances(t *testing.T) {
	container, _, dest, target := updateFixture(t)
	peer := dest.openPeer()
	update := RestoreAction{Index: 1, Action: "update", TargetAccountID: target}
	plan := dest.planRestore(container, RestoreOptions{}, update)
	_, _, _, err := peer.SetPaused(target, true, peer.Version())
	if err != nil {
		t.Fatal(err)
	}
	_, err = dest.m.ApplyRestore(container, testBackupPassword, RestoreOptions{}, []RestoreAction{update}, nil, func(current *RestorePlan) error {
		if current.Fingerprint() != plan.Fingerprint() {
			return ErrAccountVersionConflict
		}
		return nil
	})
	if !errors.Is(err, ErrAccountVersionConflict) {
		t.Fatalf("跨实例过期计划 = %v", err)
	}
	if persisted(dest.m, target).RefreshToken != "local-refresh" {
		t.Fatal("过期计划替换了凭据")
	}
	dest.applyRestore(container, RestoreOptions{}, update)
	peer.List(nil)
	if got := persisted(peer, target); got.RefreshToken != "backup-refresh" || !got.Paused {
		t.Fatal("对端未同步恢复结果或暂停被重置")
	}
}
