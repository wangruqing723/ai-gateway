package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/backupcrypto"
	"ai-gateway/internal/chatgptauth"
)

const fakeBackupPassword = "fake-backup-password-123"

// backupRequest 模拟本机同源浏览器，全部夹具仅含假账户与假密码。
func backupRequest(t *testing.T, srv *server, path string, body any, etag string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	data, raw := body.([]byte)
	if !raw {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/chatgpt/accounts/backup/"+path, bytes.NewReader(data))
	r.Header.Set("Origin", "http://127.0.0.1:7789")
	r.Header.Set("Content-Type", "application/json")
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	srv.handle(w, r)
	return w
}

func assertBackupStatus(t *testing.T, w *httptest.ResponseRecorder, status int, kind string) {
	t.Helper()
	if w.Code != status || (kind != "" && errorTypeOf(t, w) != kind) {
		t.Fatalf("响应 = %d %s，期望 %d %s", w.Code, w.Body.String(), status, kind)
	}
}

func assertBackupHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("安全响应头 = %v", w.Header())
	}
}

func assertBackupNoSecrets(t *testing.T, w *httptest.ResponseRecorder, extras ...string) {
	t.Helper()
	for _, secret := range append([]string{fakeBackupPassword, "fake-access-a", "fake-refresh-a", "fake-access-b", "fake-refresh-b"}, extras...) {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("响应泄露假密码或假凭据")
		}
	}
}

func decodeBackupPreview(t *testing.T, w *httptest.ResponseRecorder) chatGPTBackupPreview {
	t.Helper()
	assertBackupStatus(t, w, http.StatusOK, "")
	var out struct {
		Data chatGPTBackupPreview `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	assertBackupHeaders(t, w)
	assertBackupNoSecrets(t, w)
	return out.Data
}

func exportBackupFixture(t *testing.T, srv *server, ids ...string) string {
	t.Helper()
	w := backupRequest(t, srv, "export", chatGPTBackupExportRequest{AccountIDs: ids, Password: fakeBackupPassword}, strconv.Quote(srv.chatgptAuth.Version()), nil)
	assertBackupStatus(t, w, http.StatusOK, "")
	assertBackupHeaders(t, w)
	assertBackupNoSecrets(t, w)
	if w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal("备份须作为二进制附件返回")
	}
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || !strings.HasPrefix(params["filename"], "chatgpt-accounts-") || !strings.HasSuffix(params["filename"], ".aigw-backup") {
		t.Fatal("附件文件名无效")
	}
	if strings.Contains(w.Body.String(), `"accounts"`) {
		t.Fatal("备份不应是可读账户 JSON")
	}
	return w.Body.String()
}

func TestChatGPTBackupBoundaryChecks(t *testing.T) {
	srv, _, _ := newChatGPTGatewayServer(t, "")
	for _, path := range []string{"export", "preview", "apply"} {
		t.Run(path, func(t *testing.T) {
			limit := maxChatGPTBackupRequestBytes
			if path == "export" {
				limit = maxChatGPTBackupExportRequestBytes
			}
			cases := []struct {
				name   string
				body   []byte
				mutate func(*http.Request)
				status int
				kind   string
			}{
				{"方法", []byte(`{}`), func(r *http.Request) { r.Method = http.MethodPut }, 405, ""},
				{"无 Origin", []byte(`{}`), func(r *http.Request) { r.Header.Del("Origin") }, 403, ""},
				{"跨站 Origin", []byte(`{}`), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403, ""},
				{"跨端口 Origin", []byte(`{}`), func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:7790") }, 403, ""},
				{"Sec-Fetch-Site 跨站", []byte(`{}`), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, ""},
				{"非 JSON", []byte(`{}`), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, "unsupported_media_type"},
				{"超限", bytes.Repeat([]byte(" "), limit+1), nil, 413, "request_too_large"},
				{"未知字段", []byte(`{"unknown":1}`), nil, 400, "invalid_backup_request"},
				{"重复键", []byte(`{"password":"a","password":"b"}`), nil, 400, "invalid_backup_request"},
				{"尾随内容", []byte(`{} {}`), nil, 400, "invalid_backup_request"},
				{"null", []byte(`{"password":null}`), nil, 400, "invalid_backup_request"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					w := backupRequest(t, srv, path, tc.body, `"x"`, tc.mutate)
					assertBackupStatus(t, w, tc.status, tc.kind)
					// 含被 handleChatGPTAuthAPI 提前拒绝的跨站 403，所有响应都不缓存。
					assertBackupHeaders(t, w)
				})
			}
			if path != "preview" {
				for _, etag := range []string{"", `W/"x"`, "*"} {
					w := backupRequest(t, srv, path, []byte(`{}`), etag, nil)
					assertBackupStatus(t, w, 428, "precondition_required")
					assertBackupHeaders(t, w)
				}
			}
		})
	}
	for name, body := range map[string][]byte{
		"预检携带凭证":      []byte(`{"previewToken":"x"}`),
		"预检携带空凭证":     []byte(`{"previewToken":""}`),
		"metadata 错误": []byte(`{"metadata":"other"}`),
		"布尔类型错误":      []byte(`{"restorePaused":"false"}`),
		"布尔重复键":       []byte(`{"restorePaused":false,"restorePaused":true}`),
		"大小写错误":       []byte(`{"RestorePaused":false}`),
		"嵌套重复键":       []byte(`{"actions":[{"index":1,"index":2}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			assertBackupStatus(t, backupRequest(t, srv, "preview", body, "", nil), 400, "invalid_backup_request")
		})
	}
	for _, path := range []string{"preview", "apply"} {
		w := backupRequest(t, srv, path, chatGPTBackupRestoreRequest{Backup: strings.Repeat(" ", backupcrypto.MaxContainerBytes+1)}, `"x"`, nil)
		assertBackupStatus(t, w, 413, "request_too_large")
	}
	assertBackupStatus(t, backupRequest(t, srv, "apply", chatGPTBackupRestoreRequest{PreviewToken: "forged.token"}, `"x"`, nil), 400, "invalid_preview_token")
}

func TestChatGPTBackupExport(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	etag := strconv.Quote(srv.chatgptAuth.Version())
	cases := []struct {
		name           string
		ids            []string
		password, etag string
		status         int
		kind           string
	}{
		{"错版本", []string{first}, fakeBackupPassword, `"wrong"`, 409, "account_conflict"},
		{"空选择", []string{}, fakeBackupPassword, etag, 400, "invalid_backup_request"},
		{"重复选择", []string{first, first}, fakeBackupPassword, etag, 400, "invalid_backup_request"},
		{"不存在", []string{"acc_missing"}, fakeBackupPassword, etag, 400, "invalid_backup_request"},
		{"空 ID", []string{""}, fakeBackupPassword, etag, 400, "invalid_backup_request"},
		{"弱密码", []string{first}, "weak", etag, 400, "invalid_backup_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := backupRequest(t, srv, "export", chatGPTBackupExportRequest{AccountIDs: tc.ids, Password: tc.password}, tc.etag, nil)
			assertBackupStatus(t, w, tc.status, tc.kind)
			assertBackupNoSecrets(t, w)
		})
	}
	srv.transferNow = func() time.Time { return time.Date(2026, 11, 5, 10, 11, 12, 0, time.FixedZone("假时区", 8*3600)) }
	w := backupRequest(t, srv, "export", chatGPTBackupExportRequest{AccountIDs: []string{first}, Password: fakeBackupPassword}, etag, nil)
	assertBackupStatus(t, w, http.StatusOK, "")
	assertBackupHeaders(t, w)
	assertBackupNoSecrets(t, w)
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="chatgpt-accounts-20261105-021112.aigw-backup"` {
		t.Fatalf("附件头 = %q", got)
	}
	if w.Header().Get("ETag") != etag || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("导出头 = %v", w.Header())
	}
	if strings.Contains(w.Body.String(), `"accounts"`) || strings.Contains(w.Body.String(), "example.invalid") {
		t.Fatal("备份正文不应含可读账户 JSON")
	}
	if _, err := backupcrypto.Inspect(w.Body.Bytes()); err != nil {
		t.Fatalf("导出正文应是合法容器: %v", err)
	}
}

func TestChatGPTBackupUnavailableManagerHeaders(t *testing.T) {
	srv, _, _ := newChatGPTGatewayServer(t, "")
	srv.chatgptAuth = nil
	w := backupRequest(t, srv, "preview", []byte(`{}`), "", nil)
	assertBackupStatus(t, w, http.StatusServiceUnavailable, "oauth_unavailable")
	assertBackupHeaders(t, w)
}

// backupFixture 只导出一次（600,000 次 PBKDF2），容器在各子测试间复用。
type backupFixture struct {
	container     string
	first, second string // 导出端的本地 ID：remote-a、remote-b
}

func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	srv, first, second := newChatGPTGatewayServer(t, "")
	return backupFixture{container: exportBackupFixture(t, srv, first, second), first: first, second: second}
}

// newRestoreTarget 构造只有 remote-a 的恢复目标：remote-b 待新增，源 ID 在目标端空闲。
func newRestoreTarget(t *testing.T) (*server, string) {
	t.Helper()
	srv, first, second := newChatGPTGatewayServer(t, "")
	if err := srv.chatgptAuth.Delete(second); err != nil {
		t.Fatal(err)
	}
	return srv, first
}

func previewRestore(t *testing.T, srv *server, request chatGPTBackupRestoreRequest) (chatGPTBackupPreview, string) {
	t.Helper()
	w := backupRequest(t, srv, "preview", request, "", nil)
	preview := decodeBackupPreview(t, w)
	if w.Header().Get("ETag") != strconv.Quote(preview.BaseVersion) {
		t.Fatalf("预检 ETag = %q", w.Header().Get("ETag"))
	}
	return preview, w.Header().Get("ETag")
}

func TestChatGPTBackupRestore(t *testing.T) {
	fixture := newBackupFixture(t)
	valid := chatGPTBackupRestoreRequest{Backup: fixture.container, Password: fakeBackupPassword}

	t.Run("预检错误", func(t *testing.T) {
		srv, _ := newRestoreTarget(t)
		tampered := []byte(fixture.container)
		var container map[string]any
		if err := json.Unmarshal(tampered, &container); err != nil {
			t.Fatal(err)
		}
		cipher := container["ciphertext"].(string)
		flip := byte('A')
		if cipher[10] == 'A' {
			flip = 'B'
		}
		container["ciphertext"] = cipher[:10] + string(flip) + cipher[11:]
		tamperedText, _ := json.Marshal(container)
		container["ciphertext"] = cipher
		container["version"] = 2
		unsupported, _ := json.Marshal(container)
		cases := []struct {
			name    string
			request chatGPTBackupRestoreRequest
			kind    string
		}{
			{"错误密码", chatGPTBackupRestoreRequest{Backup: fixture.container, Password: "wrong-fake-password-456"}, "backup_decrypt_failed"},
			{"篡改密文", chatGPTBackupRestoreRequest{Backup: string(tamperedText), Password: fakeBackupPassword}, "backup_decrypt_failed"},
			{"非法容器", chatGPTBackupRestoreRequest{Backup: `{"format":"x"}`, Password: fakeBackupPassword}, "invalid_backup"},
			{"非 JSON 容器", chatGPTBackupRestoreRequest{Backup: "not a backup", Password: fakeBackupPassword}, "invalid_backup"},
			{"不支持的版本", chatGPTBackupRestoreRequest{Backup: string(unsupported), Password: fakeBackupPassword}, "unsupported_backup"},
			{"空密码", chatGPTBackupRestoreRequest{Backup: fixture.container}, "invalid_backup_request"},
			{"动作越界", chatGPTBackupRestoreRequest{Backup: fixture.container, Password: fakeBackupPassword, Actions: []chatgptauth.RestoreAction{{Index: 9, Action: "skip"}}}, "invalid_backup_request"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w := backupRequest(t, srv, "preview", tc.request, "", nil)
				assertBackupStatus(t, w, http.StatusBadRequest, tc.kind)
				assertBackupHeaders(t, w)
				assertBackupNoSecrets(t, w, "wrong-fake-password-456")
				if tc.kind == "backup_decrypt_failed" && !strings.Contains(w.Body.String(), "密码错误或备份已损坏") {
					t.Fatalf("解密失败文案 = %s", w.Body.String())
				}
			})
		}
	})

	t.Run("无变更不签发凭证", func(t *testing.T) {
		srv, _, _ := newChatGPTGatewayServer(t, "")
		preview, _ := previewRestore(t, srv, valid)
		if preview.CanApply || !preview.NoChanges || preview.PreviewToken != nil || preview.ExpiresAt != nil {
			t.Fatalf("全部已存在时不应签发凭证: %#v", preview.RestorePlan)
		}
	})

	t.Run("往返", func(t *testing.T) {
		srv, local := newRestoreTarget(t)
		preview, etag := previewRestore(t, srv, valid)
		if !preview.CanApply || preview.PreviewToken == nil || preview.ExpiresAt == nil ||
			preview.Summary != (chatgptauth.ImportPlanSummary{Total: 2, Add: 1, Skip: 1}) {
			t.Fatalf("预检 = %#v", preview.RestorePlan)
		}
		if got := preview.Entries[0]; got.Status != "existing" || got.MatchedAccountID != local || len(got.AffectedProviders) != 1 || got.AffectedProviders[0] != "codex" {
			t.Fatalf("已有条目 = %#v", got)
		}
		if got := preview.Entries[1]; got.Status != "new" || got.TargetAccountID != fixture.second || got.Remapped {
			t.Fatalf("新增条目 = %#v", got)
		}
		request := valid
		request.PreviewToken = *preview.PreviewToken
		w := backupRequest(t, srv, "apply", request, etag, nil)
		assertBackupStatus(t, w, http.StatusOK, "")
		assertBackupHeaders(t, w)
		assertBackupNoSecrets(t, w)
		var out struct {
			Data chatgptauth.RestoreResult `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		want := []chatgptauth.RestoreMapping{
			{SourceAccountID: fixture.first, TargetAccountID: local, Action: "skip", Remapped: fixture.first != local},
			{SourceAccountID: fixture.second, TargetAccountID: fixture.second, Action: "add"},
		}
		if out.Data.Imported != 1 || out.Data.Skipped != 1 || fmt.Sprint(out.Data.Mappings) != fmt.Sprint(want) {
			t.Fatalf("应用结果 = %s", w.Body.String())
		}
		if w.Header().Get("ETag") != strconv.Quote(srv.chatgptAuth.Version()) || w.Header().Get("ETag") == etag {
			t.Fatalf("应用后 ETag = %q", w.Header().Get("ETag"))
		}
		token, _, err := srv.chatgptAuth.AccessToken(context.Background(), fixture.second, blockedClient)
		if err != nil || token != "fake-access-b" {
			t.Fatalf("恢复的账户凭据 = %q %v", token, err)
		}
		replay := backupRequest(t, srv, "apply", request, w.Header().Get("ETag"), nil)
		assertBackupStatus(t, replay, http.StatusConflict, "")
	})

	t.Run("拒绝陈旧预检", func(t *testing.T) {
		setup := func(t *testing.T) (*server, string, chatGPTBackupRestoreRequest, string, chatGPTBackupPreview) {
			t.Helper()
			srv, local := newRestoreTarget(t)
			preview, etag := previewRestore(t, srv, valid)
			if preview.PreviewToken == nil {
				t.Fatalf("应签发凭证: %#v", preview.RestorePlan)
			}
			request := valid
			request.PreviewToken = *preview.PreviewToken
			return srv, local, request, etag, preview
		}
		expectRejected := func(t *testing.T, srv *server, w *httptest.ResponseRecorder, status int, kind string) {
			t.Helper()
			assertBackupStatus(t, w, status, kind)
			assertBackupHeaders(t, w)
			assertBackupNoSecrets(t, w)
			if len(srv.chatgptAuth.List(nil)) != 1 {
				t.Fatal("被拒绝的应用不应改动账户")
			}
		}
		t.Run("篡改凭证", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			last := request.PreviewToken[len(request.PreviewToken)-1]
			flipped := "0"
			if last == '0' {
				flipped = "1"
			}
			request.PreviewToken = request.PreviewToken[:len(request.PreviewToken)-1] + flipped
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusBadRequest, "invalid_preview_token")
		})
		t.Run("导入域凭证不能冒用", func(t *testing.T) {
			srv, _, request, etag, preview := setup(t)
			backupKey, _ := srv.chatGPTBackupKey()
			importKey, _ := srv.chatGPTImportKey()
			digest, _ := chatGPTBackupInputDigest(backupKey, []byte(request.Backup), "keep", false, nil)
			srv.cfgMu.RLock()
			revision := srv.revision
			srv.cfgMu.RUnlock()
			forged, _, err := srv.signChatGPTImportToken(importKey, chatGPTImportToken{
				Accounts: preview.BaseVersion, Revision: revision, InputDigest: digest, PlanDigest: strings.Repeat("0", 64),
			})
			if err != nil {
				t.Fatal(err)
			}
			request.PreviewToken = forged
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusBadRequest, "invalid_preview_token")
		})
		t.Run("过期", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			srv.transferNow = func() time.Time { return time.Now().Add(chatGPTBackupPreviewTTL + time.Second) }
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusGone, "preview_expired")
		})
		t.Run("动作改变", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			request.Actions = []chatgptauth.RestoreAction{{Index: 2, Action: "skip"}}
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "preview_mismatch")
		})
		t.Run("metadata 改变", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			request.Metadata = chatgptauth.RestoreMetadataReplace
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "preview_mismatch")
		})
		t.Run("restorePaused 改变", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			request.RestorePaused = true
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "preview_mismatch")
		})
		t.Run("If-Match 不一致", func(t *testing.T) {
			srv, _, request, _, _ := setup(t)
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, `"other"`, nil), http.StatusConflict, "account_conflict")
		})
		t.Run("账户在预检后变化", func(t *testing.T) {
			srv, local, request, etag, _ := setup(t)
			note := "并发修改"
			if _, err := srv.chatgptAuth.UpdateMetadata(local, chatgptauth.AccountMetadataPatch{Note: &note}); err != nil {
				t.Fatal(err)
			}
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "account_conflict")
		})
		t.Run("配置在预检后变化", func(t *testing.T) {
			srv, _, request, etag, _ := setup(t)
			srv.cfgMu.Lock()
			srv.revision = "changed-revision"
			srv.cfgMu.Unlock()
			expectRejected(t, srv, backupRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "config_conflict")
		})
	})
}

func TestWriteChatGPTBackupErrorUsesFixedMessages(t *testing.T) {
	secret := "fake-secret-in-error"
	cases := []struct {
		err    error
		status int
		kind   string
	}{
		{fmt.Errorf("%w: %s", backupcrypto.ErrInvalidContainer, secret), 400, "invalid_backup"},
		{fmt.Errorf("%w: %s", chatgptauth.ErrInvalidBackup, secret), 400, "invalid_backup"},
		{fmt.Errorf("%w: %s", backupcrypto.ErrUnsupported, secret), 400, "unsupported_backup"},
		{fmt.Errorf("%w: %s", backupcrypto.ErrDecrypt, secret), 400, "backup_decrypt_failed"},
		{fmt.Errorf("%w: %s", backupcrypto.ErrInvalidPassword, secret), 400, "invalid_backup_request"},
		{fmt.Errorf("%w: %s", backupcrypto.ErrTooLarge, secret), 413, "request_too_large"},
		{fmt.Errorf("%w: %s", chatgptauth.ErrRestorePlanNotApplicable, secret), 409, "account_conflict"},
		{fmt.Errorf("%w: %s", chatgptauth.ErrAccountRestorePersist, secret), 500, "account_restore_failed"},
		{errors.New(secret), 500, "account_restore_failed"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		writeChatGPTBackupError(w, tc.err)
		if w.Code != tc.status || errorTypeOf(t, w) != tc.kind || strings.Contains(w.Body.String(), secret) {
			t.Errorf("%v => %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}
