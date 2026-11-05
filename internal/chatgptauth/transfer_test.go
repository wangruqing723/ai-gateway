package chatgptauth

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// zipJSONEntries 按文件名顺序读出 ZIP 中各条目内容。
func zipJSONEntries(t *testing.T, data []byte) []string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := append([]*zip.File(nil), reader.File...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	out := make([]string, 0, len(files))
	for _, file := range files {
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(content))
	}
	return out
}

type zipEntry struct {
	name    string
	mode    os.FileMode
	content string
}

// buildZip 构造测试 ZIP；mode 为 0 时按普通文件写入。
func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0o600
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// credentialsOf 读取账户凭据快照，避免测试复制含锁的 account。
func credentialsOf(t *testing.T, m *Manager, remoteID string) transferAccountSnapshot {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.accounts {
		if item.AccountID == remoteID {
			return transferAccountSnapshot{
				ID: item.ID, Email: item.Email, Plan: item.Plan, AccountID: item.AccountID,
				IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
				ExpiresAt: item.ExpiresAt, LastRefresh: item.LastRefresh,
			}
		}
	}
	t.Fatalf("找不到远端账户 %s", remoteID)
	return transferAccountSnapshot{}
}

func TestExportImportRoundTripAllFormats(t *testing.T) {
	source := newTestEnv(t)
	expires := source.clock.Now().Add(6 * time.Hour)
	first := source.addAccount("remote-1", "one@example.invalid", "fake-refresh-1", expires)
	second := source.addAccount("remote-2", "two@example.invalid", "fake-refresh-2", expires)
	cases := []struct {
		format, detected, contentType string
		ids                           []string
	}{
		{"cockpit_tools", "cockpit_tools_or_cliproxyapi", "application/json", []string{first, second}},
		{"cliproxyapi", "cliproxyapi", "application/json", []string{first}},
		{"cliproxyapi", "cliproxyapi", "application/zip", []string{first, second}},
		{"sub2api", "sub2api", "application/json", []string{first, second}},
		{"auth_json", "auth_json", "application/json", []string{first}},
		{"auth_json", "auth_json", "application/zip", []string{first, second}},
	}
	for _, tc := range cases {
		t.Run(tc.format+"/"+tc.contentType, func(t *testing.T) {
			file, err := source.m.Export(tc.format, tc.ids)
			if err != nil || file.ContentType != tc.contentType {
				t.Fatalf("导出 = %q, %v", file.ContentType, err)
			}
			target := newTestEnv(t)
			result, err := target.m.Import(file.Data, file.ContentType == "application/zip")
			if err != nil || result.DetectedFormat != tc.detected || result.Imported != len(tc.ids) || result.SkippedDuplicates != 0 {
				t.Fatalf("导入 = %#v, %v", result, err)
			}
			for _, remoteID := range []string{"remote-1", "remote-2"}[:len(tc.ids)] {
				want := credentialsOf(t, source.m, remoteID)
				got := credentialsOf(t, target.m, remoteID)
				if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
					got.Email != want.Email || got.Plan != want.Plan || !got.LastRefresh.Equal(want.LastRefresh) {
					t.Fatalf("往返后凭据 = %#v，期望 %#v", got, want)
				}
				// 官方 auth.json 不带 expires_at，只能从 access token 的 exp 声明恢复；
				// 假令牌不是 JWT，所以到期时间丢失，导入后按“可刷新”处理。
				if tc.format == "auth_json" {
					view, _ := target.m.Account(got.ID)
					if !got.ExpiresAt.IsZero() || view.Authorization.State != "refreshable" {
						t.Fatalf("auth.json 往返到期时间 = %v 状态 = %q", got.ExpiresAt, view.Authorization.State)
					}
				} else if !got.ExpiresAt.Equal(want.ExpiresAt) {
					t.Fatalf("往返后到期时间 = %v，期望 %v", got.ExpiresAt, want.ExpiresAt)
				}
			}
		})
	}
}

func TestImportSkipsDuplicatesWithoutOverwriting(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	payload := `[
  {"access_token": "replacement-access", "refresh_token": "replacement-refresh", "account_id": "remote-1"},
  {"access_token": "new-access", "refresh_token": "new-refresh", "account_id": "remote-2"},
  {"access_token": "second-copy", "refresh_token": "second-refresh", "account_id": "remote-2"}
]`
	result, err := env.m.Import([]byte(payload), false)
	if err != nil || result.Imported != 1 || result.SkippedDuplicates != 2 || len(result.Accounts) != 1 {
		t.Fatalf("导入结果 = %#v, %v", result, err)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "第 1 条") || !strings.Contains(strings.Join(result.Warnings, "\n"), "第 3 条") {
		t.Fatalf("重复警告 = %#v", result.Warnings)
	}
	if existing := credentialsOf(t, env.m, "remote-1"); existing.ID != id || existing.AccessToken != "fake-access-remote-1" {
		t.Fatalf("重复导入覆盖了已有凭据: %#v", existing)
	}
	if added := credentialsOf(t, env.m, "remote-2"); added.AccessToken != "new-access" {
		t.Fatalf("同批次重复应保留第一条: %#v", added)
	}
	again, err := env.m.Import([]byte(payload), false)
	if err != nil || again.Imported != 0 || again.SkippedDuplicates != 3 || len(env.m.List(nil)) != 2 {
		t.Fatalf("全部重复时 = %#v, %v", again, err)
	}
}

func TestImportRejectsInvalidBatchAtomically(t *testing.T) {
	future := "2030-06-01T00:00:00Z"
	past := "2029-06-01T00:00:00Z"
	cases := []struct {
		name, payload, want string
	}{
		{"第三条缺 access_token", `[{"access_token":"a1","refresh_token":"r1","account_id":"remote-1"},` +
			`{"access_token":"a2","refresh_token":"r2","account_id":"remote-2"},{"refresh_token":"r3","account_id":"remote-3"}]`, "第 3 条账户缺少 access_token"},
		{"非 ChatGPT 认证", `{"auth_mode":"apikey","tokens":{"access_token":"a","account_id":"remote-1"}}`, "auth_mode 不属于 ChatGPT OAuth"},
		{"缺 account_id", `{"access_token":"a","refresh_token":"r"}`, "无法解析 ChatGPT account_id"},
		{"无刷新凭据且已过期", `{"access_token":"a","account_id":"remote-1","expires_at":"` + past + `"}`, "缺少 refresh_token"},
		{"无刷新凭据且无到期时间", `{"access_token":"a","account_id":"remote-1"}`, "缺少 refresh_token"},
		{"数组元素不是对象", `["fake"]`, "每条账户必须是 JSON 对象"},
		{"空数组", `[]`, "账户数组为空"},
		{"sub2api 空账户", `{"type":"sub2api-data","accounts":[]}`, "sub2api accounts 必须是非空数组"},
		{"多个 JSON 值", `{"access_token":"a","refresh_token":"r","account_id":"remote-1"} {}`, "JSON 文件只能包含一个值"},
		{"不是 JSON", `fake-secret-not-json`, "JSON 无法解析"},
		{"空文件", ``, "文件为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			_, err := env.m.Import([]byte(tc.payload), false)
			if !errors.Is(err, ErrInvalidImport) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "fake-secret") {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
			if len(env.m.List(nil)) != 0 {
				t.Fatal("无效批次不应导入任何账户")
			}
			if _, statErr := os.Stat(env.path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("无效批次不应写账户文件: %v", statErr)
			}
		})
	}
	// 没有 refresh_token 但 access token 未过期时允许导入，并给出到期后需重新授权的警告。
	env := newTestEnv(t)
	result, err := env.m.Import([]byte(`{"access_token":"a","account_id":"remote-1","expires_at":"`+future+`"}`), false)
	if err != nil || result.Imported != 1 || !strings.Contains(strings.Join(result.Warnings, ""), "没有 refresh_token") {
		t.Fatalf("短期令牌导入 = %#v, %v", result, err)
	}
	if view := result.Accounts[0]; view.Status != "ready" || view.Authorization.State != "valid" || view.Authorization.CanRefresh {
		t.Fatalf("短期令牌状态 = %#v", view)
	}
}

func TestImportPersistFailureRollsBack(t *testing.T) {
	env := newTestEnv(t)
	env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	before, _ := os.ReadFile(env.path)
	restore := env.failPersist()
	_, err := env.m.Import([]byte(`[{"access_token":"a2","refresh_token":"r2","account_id":"remote-2"},`+
		`{"access_token":"a3","refresh_token":"r3","account_id":"remote-3"}]`), false)
	restore()
	if !errors.Is(err, ErrAccountImportPersist) {
		t.Fatalf("落盘失败 = %v", err)
	}
	after, _ := os.ReadFile(env.path)
	if len(env.m.List(nil)) != 1 || !bytes.Equal(before, after) {
		t.Fatal("落盘失败后不应留下部分导入")
	}
}

func TestImportCountLimit(t *testing.T) {
	env := newTestEnv(t)
	items := make([]string, maxAccountTransferCount+1)
	for i := range items {
		items[i] = `{"access_token":"a","refresh_token":"r","account_id":"remote-` + strings.Repeat("x", i+1) + `"}`
	}
	if _, err := env.m.Import([]byte("["+strings.Join(items, ",")+"]"), false); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("超过数量上限 = %v", err)
	}
	if _, err := env.m.Import(make([]byte, maxAccountTransferBytes+1), false); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("超过字节上限 = %v", err)
	}
	if len(env.m.List(nil)) != 0 {
		t.Fatal("超限批次不应导入账户")
	}
}

func TestImportArchiveLimitsAndPaths(t *testing.T) {
	valid := `{"access_token":"a","refresh_token":"r","account_id":"remote-1"}`
	tooManyEntries := make([]zipEntry, maxArchiveEntries+1)
	for i := range tooManyEntries {
		tooManyEntries[i] = zipEntry{name: "dir" + strings.Repeat("x", i) + "/", mode: os.ModeDir | 0o700}
	}
	tooManyFiles := make([]zipEntry, maxAccountTransferCount+1)
	for i := range tooManyFiles {
		tooManyFiles[i] = zipEntry{name: "f" + strings.Repeat("x", i) + ".json", content: valid}
	}
	cases := []struct {
		name    string
		data    []byte
		wantErr error
		want    string
	}{
		{"上级目录", buildZip(t, zipEntry{name: "../a.json", content: valid}), ErrInvalidImport, "不安全路径"},
		{"中间上级目录", buildZip(t, zipEntry{name: "a/../b.json", content: valid}), ErrInvalidImport, "不安全路径"},
		{"绝对路径", buildZip(t, zipEntry{name: "/a.json", content: valid}), ErrInvalidImport, "不安全路径"},
		{"反斜杠", buildZip(t, zipEntry{name: `a\b.json`, content: valid}), ErrInvalidImport, "不安全路径"},
		{"盘符", buildZip(t, zipEntry{name: "C:a.json", content: valid}), ErrInvalidImport, "不安全路径"},
		{"非 JSON 文件", buildZip(t, zipEntry{name: "a.txt", content: valid}), ErrInvalidImport, "只能包含 JSON 文件"},
		{"符号链接", buildZip(t, zipEntry{name: "a.json", mode: os.ModeSymlink | 0o777, content: "target.json"}), ErrInvalidImport, "只能包含 JSON 文件"},
		{"只有目录", buildZip(t, zipEntry{name: "dir/", mode: os.ModeDir | 0o700}), ErrInvalidImport, "没有 JSON 文件"},
		{"不是 ZIP", []byte("fake-not-a-zip"), ErrInvalidImport, "ZIP 文件无法读取"},
		{"条目内 JSON 无效", buildZip(t, zipEntry{name: "a.json", content: "{"}), ErrInvalidImport, "JSON 无法解析"},
		{"单个条目过大", buildZip(t, zipEntry{name: "a.json", content: strings.Repeat(" ", maxAccountTransferItem+1)}), ErrImportTooLarge, ""},
		{"条目数过多", buildZip(t, tooManyEntries...), ErrImportTooLarge, ""},
		{"JSON 文件过多", buildZip(t, tooManyFiles...), ErrImportTooLarge, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			_, err := env.m.Import(tc.data, true)
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望 %v / %q", err, tc.wantErr, tc.want)
			}
			if len(env.m.List(nil)) != 0 {
				t.Fatal("被拒绝的 ZIP 不应导入账户")
			}
		})
	}
	env := newTestEnv(t)
	nested := buildZip(t,
		zipEntry{name: "accounts/", mode: os.ModeDir | 0o700},
		zipEntry{name: "accounts/one/AUTH.JSON", content: valid},
		zipEntry{name: "two.json", content: `[{"access_token":"b","refresh_token":"r","account_id":"remote-2"}]`},
	)
	result, err := env.m.Import(nested, true)
	if err != nil || result.Imported != 2 {
		t.Fatalf("嵌套目录 ZIP = %#v, %v", result, err)
	}
}

func TestExportRejectsInvalidRequests(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	env.m.mu.Lock()
	env.m.accounts[id].IDToken = ""
	env.m.mu.Unlock()
	tooMany := make([]string, maxAccountTransferCount+1)
	cases := []struct {
		name, format string
		ids          []string
		want         string
	}{
		{"未选择账户", "cockpit_tools", nil, "必须选择"},
		{"选择过多", "cockpit_tools", tooMany, "必须选择"},
		{"未知格式", "fake-format", []string{id}, "不支持的导出格式"},
		{"重复 ID", "cockpit_tools", []string{id, id}, "重复 ID"},
		{"不存在的账户", "cockpit_tools", []string{"acc_missing"}, "所选账户不存在"},
		{"官方格式缺 id_token", "auth_json", []string{id}, "缺少 id_token"},
	}
	for _, tc := range cases {
		if _, err := env.m.Export(tc.format, tc.ids); !errors.Is(err, ErrInvalidExport) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 错误 = %v", tc.name, err)
		}
	}
}

func TestLegacyStoreLoadsAndDerivesStates(t *testing.T) {
	env := newTestEnv(t)
	legacy := `{"version":1,"accounts":[
  {"id":"acc_ready","email":"ready@example.invalid","chatgptAccountId":"remote-1","accessToken":"a1","refreshToken":"r1",
   "expiresAt":"2030-01-01T05:00:00Z","lastRefresh":"2029-12-31T00:00:00Z","futureField":"ignored",
   "usage":{"planType":"pro","lastCheckedAt":"2029-12-31T23:00:00Z","windows":[{"name":"primary","usedPercent":30,"remainingPercent":70,"resetAt":"2029-12-31T23:30:00Z"}]}},
  {"id":"acc_reauth","chatgptAccountId":"remote-2","accessToken":"a2","refreshToken":"r2","expiresAt":"2030-01-01T05:00:00Z","needsReauth":true},
  {"id":"acc_expired","chatgptAccountId":"remote-3","accessToken":"a3","refreshToken":"r3","expiresAt":"2029-12-31T00:00:00Z"},
  {"id":"","chatgptAccountId":"remote-4","accessToken":"a4"},
  {"id":"acc_no_remote","accessToken":"a5"},
  {"id":"acc_no_tokens","chatgptAccountId":"remote-6"},
  null
]}`
	if err := os.MkdirAll(filepath.Dir(env.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m := env.open()
	if info, err := os.Stat(env.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("加载后文件权限 = %v, %v", info, err)
	}
	views := map[string]AccountView{}
	for _, view := range m.List(nil) {
		views[view.ID] = view
	}
	if len(views) != 3 {
		t.Fatalf("应只加载 3 个有效旧账户，实际 %#v", views)
	}
	ready := views["acc_ready"]
	if ready.Status != "ready" || ready.Authorization.State != "valid" || ready.Plan != "pro" ||
		ready.UsageState.State != "ok" || ready.UsageState.Freshness != "stale" ||
		ready.UsageState.LastSuccessAt != "2029-12-31T23:00:00Z" || len(ready.UsageState.ResetPendingWindows) != 1 {
		t.Fatalf("旧有效账户视图 = %#v", ready)
	}
	if reauth := views["acc_reauth"]; reauth.Status != "reauthorization_required" ||
		reauth.Authorization.State != "reauthorization_required" || reauth.Authorization.CanRefresh {
		t.Fatalf("旧重新授权账户视图 = %#v", reauth)
	}
	// 旧 status 仍是 ready（有刷新凭据），新的授权维度说明访问令牌已过期、使用时会刷新。
	if expired := views["acc_expired"]; expired.Status != "ready" || expired.Authorization.State != "refreshable" ||
		expired.UsageState.State != "unknown" || expired.Invocation.State != "unknown" {
		t.Fatalf("旧过期账户视图 = %#v", expired)
	}
	if err := os.WriteFile(env.path, []byte(`{"version":1,"accounts":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(env.path, nil); err == nil {
		t.Fatal("损坏的账户文件应拒绝加载，不能静默清空")
	}
}
