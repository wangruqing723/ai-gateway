package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/balancer"
	"ai-gateway/internal/breaker"
	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
)

const transferHTTPSource = `kind: ai-gateway.provider-route-transfer
version: 1
providers:
  imported:
    baseUrl: https://example.invalid/v1
    format: openai
    apiKey: fake-import-api-secret
    proxy: http://u:fake-import-proxy-secret@proxy.invalid:8080
    maxConcurrent: 7
    extraHeaders:
      X-Fake-Private: fake-import-header-secret
    extraBody:
      private: fake-import-body-secret
routes:
  - match: imported-*
    provider: imported
    model: fake-import-model
`

func newTransferHTTPServer(t *testing.T) *server {
	t.Helper()
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://example.invalid", "fake-original-secret", 5))
	srv.revision = "original-revision"
	srv.keyPool = keypool.New(nil)
	srv.keyPool.Reconcile(srv.cfg)
	srv.breaker = breaker.New(breakerSettings(srv.cfg))
	srv.selector = balancer.New()
	return srv
}

func transferHTTPRequest(srv *server, path string, value any) *httptest.ResponseRecorder {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config"+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:7789")
	srv.cfgMu.RLock()
	revision := srv.revision
	srv.cfgMu.RUnlock()
	req.Header.Set("If-Match", fmt.Sprintf("%q", revision))
	recorder := httptest.NewRecorder()
	srv.handle(recorder, req)
	return recorder
}

func transferHTTPPreview(t *testing.T, srv *server, options config.TransferOptions) string {
	t.Helper()
	return transferHTTPPreviewContent(t, srv, transferHTTPSource, options)
}

func transferHTTPPreviewContent(t *testing.T, srv *server, content string, options config.TransferOptions) string {
	t.Helper()
	recorder := transferHTTPRequest(srv, "/import/preview", configTransferImportRequest{Content: content, Options: options})
	if recorder.Code != http.StatusOK {
		t.Fatalf("预览状态=%d，响应=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data struct {
			Token    string `json:"previewToken"`
			CanApply bool   `json:"canApply"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Data.CanApply || body.Data.Token == "" {
		t.Fatalf("没有可应用预览：%s", recorder.Body.String())
	}
	return body.Data.Token
}

func TestConfigTransferExportProtectsOriginAndUsesSavedSnapshot(t *testing.T) {
	srv := newTransferHTTPServer(t)
	// 外部磁盘修改尚未重载：导出必须仍对应已加载配置和当前 revision。
	if err := os.WriteFile(srv.cfg.Path, []byte(testConfigYAML("127.0.0.1", 7789, "https://different.invalid", "fake-disk-secret", 5)), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := transferHTTPRequest(srv, "/export", config.TransferExportRequest{Scope: "all"})
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "fake-original-secret") || strings.Contains(recorder.Body.String(), "fake-disk-secret") {
		t.Fatalf("导出未使用已加载快照：%d %s", recorder.Code, recorder.Body.String())
	}
	for _, header := range []string{"Content-Disposition", "Content-Length", "ETag"} {
		if recorder.Header().Get(header) == "" {
			t.Errorf("导出缺头 %s", header)
		}
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("附件安全头缺失")
	}
}

func TestConfigTransferHTTPBoundary(t *testing.T) {
	tests := []struct {
		name, method, origin, contentType, etag, body, fetchSite string
		status                                                   int
	}{
		{"no-origin", "POST", "", "application/json", `"original-revision"`, `{"scope":"all"}`, "", 403},
		{"foreign-origin", "POST", "https://evil.invalid", "application/json", `"original-revision"`, `{"scope":"all"}`, "", 403},
		{"other-port", "POST", "http://127.0.0.1:7788", "application/json", `"original-revision"`, `{"scope":"all"}`, "", 403},
		{"origin-path", "POST", "http://127.0.0.1:7789/path", "application/json", `"original-revision"`, `{"scope":"all"}`, "", 403},
		{"cross-site", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"all"}`, "cross-site", 403},
		{"get", "GET", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"all"}`, "", 405},
		{"wrong-media", "POST", "http://127.0.0.1:7789", "text/plain", `"original-revision"`, `{"scope":"all"}`, "", 415},
		{"missing-etag", "POST", "http://127.0.0.1:7789", "application/json", "", `{"scope":"all"}`, "", 428},
		{"weak-etag", "POST", "http://127.0.0.1:7789", "application/json", `W/"original-revision"`, `{"scope":"all"}`, "", 428},
		{"wildcard", "POST", "http://127.0.0.1:7789", "application/json", `*`, `{"scope":"all"}`, "", 428},
		{"stale-etag", "POST", "http://127.0.0.1:7789", "application/json", `"stale"`, `{"scope":"all"}`, "", 409},
		{"duplicate-json", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"selected","scope":"all"}`, "", 400},
		{"case-variant-json", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"selected","Scope":"all"}`, "", 400},
		{"unknown-json", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"all","fake-private-field":"fake-private-secret"}`, "", 400},
		{"null-json", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"all","selection":null}`, "", 400},
		{"trailing-json", "POST", "http://127.0.0.1:7789", "application/json", `"original-revision"`, `{"scope":"all"} {}`, "", 400},
		{"ok-charset", "POST", "http://127.0.0.1:7789", "application/json; charset=utf-8", `"original-revision"`, `{"scope":"all"}`, "", 200},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTransferHTTPServer(t)
			req := httptest.NewRequest(test.method, "http://127.0.0.1:7789/api/config/export", strings.NewReader(test.body))
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("If-Match", test.etag)
			req.Header.Set("Sec-Fetch-Site", test.fetchSite)
			recorder := httptest.NewRecorder()
			srv.handle(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("状态=%d，期望=%d，响应=%s", recorder.Code, test.status, recorder.Body.String())
			}
			if test.status != 200 && (strings.Contains(recorder.Body.String(), "fake-original-secret") || strings.Contains(recorder.Body.String(), "fake-private-secret")) {
				t.Fatal("错误响应泄露凭据")
			}
		})
	}
}

func TestConfigTransferPreviewIsReadOnlyAndRedacted(t *testing.T) {
	srv := newTransferHTTPServer(t)
	before, err := os.ReadFile(srv.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	var network atomic.Int32
	srv.resolveHTTPClient = func(string) (*http.Client, error) { network.Add(1); return nil, errors.New("测试禁止出网") }
	recorder := transferHTTPRequest(srv, "/import/preview", configTransferImportRequest{Content: transferHTTPSource})
	if recorder.Code != 200 {
		t.Fatalf("预览失败：%s", recorder.Body.String())
	}
	for _, private := range []string{"fake-import-api-secret", "fake-import-proxy-secret", "fake-import-header-secret", "fake-import-body-secret", "fake-import-model", "X-Fake-Private"} {
		if strings.Contains(recorder.Body.String(), private) {
			t.Fatalf("预览泄露原值 %s", private)
		}
	}
	after, err := os.ReadFile(srv.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || srv.revision != "original-revision" || len(srv.cfg.Providers) != 1 || network.Load() != 0 {
		t.Fatal("预览修改了配置或访问上游")
	}
	for _, path := range []string{"", "/raw"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config"+path, nil)
		response := httptest.NewRecorder()
		srv.handle(response, req)
		if response.Code != 200 || strings.Contains(response.Body.String(), "fake-original-secret") {
			t.Fatal("普通配置脱敏回归")
		}
	}
}

func TestConfigTransferAppliesOncePreservesGlobalsAndRuntime(t *testing.T) {
	srv := newTransferHTTPServer(t)
	srv.cfg.Timeout = 54321
	content := strings.Replace(transferHTTPSource, "    apiKey: fake-import-api-secret", "    apiKeys:\n      - id: imported\n        value: fake-import-api-secret", 1)
	token := transferHTTPPreviewContent(t, srv, content, config.TransferOptions{InsertNewRoutes: "before"})
	input := configTransferImportRequest{Content: content, Options: config.TransferOptions{InsertNewRoutes: "before"}, PreviewToken: token}
	recorder := transferHTTPRequest(srv, "/import/apply", input)
	if recorder.Code != 200 {
		t.Fatalf("应用失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if srv.revision == "original-revision" || srv.cfg.Timeout != 54321 || len(srv.cfg.Providers) != 2 || srv.cfg.Routes[0].Match != "imported-*" || srv.cfg.Routes[1].Match != "*" {
		t.Fatal("完整应用或全局保留失败")
	}
	if !srv.keyPool.HasSelectable("imported", srv.cfg.Providers["imported"], nil) {
		t.Fatal("运行时密钥池未 Reconcile")
	}
	persisted, err := os.ReadFile(srv.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.DecodeAndValidate(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers["imported"].APIKeys) != 1 || loaded.Providers["imported"].APIKeys[0].Value != "fake-import-api-secret" || loaded.Timeout != 54321 {
		t.Fatal("磁盘状态与运行时不一致")
	}
	revision := srv.revision
	replay := transferHTTPRequest(srv, "/import/apply", input)
	if replay.Code != 409 || srv.revision != revision {
		t.Fatal("凭证重放重复保存")
	}
}

func TestConfigTransferTokenFailures(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*server, *configTransferImportRequest)
		status int
		code   string
	}{
		{"missing", func(_ *server, input *configTransferImportRequest) { input.PreviewToken = "" }, 400, "invalid_preview_token"},
		{"signature", func(_ *server, input *configTransferImportRequest) { input.PreviewToken += "changed" }, 400, "invalid_preview_token"},
		{"document", func(_ *server, input *configTransferImportRequest) { input.Content += "\n# changed\n" }, 409, "preview_changed"},
		{"options", func(_ *server, input *configTransferImportRequest) { input.Options.InsertNewRoutes = "before" }, 409, "preview_changed"},
		{"expired", func(srv *server, _ *configTransferImportRequest) {
			srv.transferNow = func() time.Time { return time.Now().Add(configTransferPreviewTTL) }
		}, 409, "preview_expired"},
		{"revision", func(srv *server, _ *configTransferImportRequest) { srv.revision = "other-revision" }, 409, "config_conflict"},
		{"process-key", func(srv *server, _ *configTransferImportRequest) { srv.transferKey[0] ^= 0xff }, 400, "invalid_preview_token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTransferHTTPServer(t)
			token := transferHTTPPreview(t, srv, config.TransferOptions{})
			input := configTransferImportRequest{Content: transferHTTPSource, PreviewToken: token}
			test.modify(srv, &input)
			before, _ := os.ReadFile(srv.cfg.Path)
			recorder := transferHTTPRequest(srv, "/import/apply", input)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("凭证失败分类错误：%d %s", recorder.Code, recorder.Body.String())
			}
			after, _ := os.ReadFile(srv.cfg.Path)
			if !bytes.Equal(before, after) || len(srv.cfg.Providers) != 1 {
				t.Fatal("无效凭证写入了部分配置")
			}
		})
	}
}

func TestConfigTransferSaveAndRevisionFailureDoNotPublish(t *testing.T) {
	for _, name := range []string{"save", "revision"} {
		t.Run(name, func(t *testing.T) {
			srv := newTransferHTTPServer(t)
			token := transferHTTPPreview(t, srv, config.TransferOptions{})
			path := srv.cfg.Path
			before, _ := os.ReadFile(path)
			if name == "save" {
				srv.cfg.Path = filepath.Join(t.TempDir(), "nonexistent", "config.yaml")
			} else {
				srv.revisionSource = func() (string, error) { return "", errors.New("fake-private-revision-secret") }
			}
			recorder := transferHTTPRequest(srv, "/import/apply", configTransferImportRequest{Content: transferHTTPSource, PreviewToken: token})
			if recorder.Code != 500 || len(srv.cfg.Providers) != 1 || srv.revision != "original-revision" {
				t.Fatalf("失败发布了配置：%d %s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "fake-private-revision-secret") {
				t.Fatal("内部异常原文泄露")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("失败改写了原文件")
			}
		})
	}
}

func TestConfigTransferConcurrentApplyOnlyOneWins(t *testing.T) {
	srv := newTransferHTTPServer(t)
	token := transferHTTPPreview(t, srv, config.TransferOptions{})
	input := configTransferImportRequest{Content: transferHTTPSource, PreviewToken: token}
	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() { defer wait.Done(); statuses <- transferHTTPRequest(srv, "/import/apply", input).Code }()
	}
	wait.Wait()
	close(statuses)
	ok, conflict := 0, 0
	for status := range statuses {
		if status == 200 {
			ok++
		} else if status == 409 {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("并发应用状态 ok=%d conflict=%d", ok, conflict)
	}
}

func TestConfigTransferRevalidatesDeletedOAuthAccount(t *testing.T) {
	srv := newTransferHTTPServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-accounts.json")
	store := `{"version":1,"accounts":[{"id":"acc_target","chatgptAccountId":"fake-remote-account","accessToken":"fake-oauth-token","refreshToken":"fake-oauth-refresh"}]}`
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := chatgptauth.New(path, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	srv.chatgptAuth = manager
	content := `providers:
  oauth:
    baseUrl: https://chatgpt.com/backend-api/codex
    format: openai-responses
    authType: chatgptOAuth
    accountId: acc_source
routes: []
`
	options := config.TransferOptions{AccountMappings: map[string]string{"acc_source": "acc_target"}}
	token := transferHTTPPreviewContent(t, srv, content, options)
	if err := manager.Delete("acc_target"); err != nil {
		t.Fatal(err)
	}
	recorder := transferHTTPRequest(srv, "/import/apply", configTransferImportRequest{Content: content, Options: options, PreviewToken: token})
	if recorder.Code != 409 || !strings.Contains(recorder.Body.String(), "import_dependency_changed") || srv.revision != "original-revision" || srv.cfg.Providers["oauth"] != nil {
		t.Fatalf("账户删除后凭证仍可应用：%d %s", recorder.Code, recorder.Body.String())
	}
}

func TestConfigTransferCancelledApplyDoesNotSave(t *testing.T) {
	srv := newTransferHTTPServer(t)
	token := transferHTTPPreview(t, srv, config.TransferOptions{})
	data, err := json.Marshal(configTransferImportRequest{Content: transferHTTPSource, PreviewToken: token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config/import/apply", bytes.NewReader(data)).WithContext(ctx)
	req.Header.Set("Origin", "http://127.0.0.1:7789")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"original-revision"`)
	before, _ := os.ReadFile(srv.cfg.Path)
	srv.handle(httptest.NewRecorder(), req)
	after, _ := os.ReadFile(srv.cfg.Path)
	if !bytes.Equal(before, after) || srv.revision != "original-revision" || len(srv.cfg.Providers) != 1 {
		t.Fatal("已取消的应用请求仍写配置")
	}
}

func TestConfigTransferLimitsAndSafeParseErrors(t *testing.T) {
	srv := newTransferHTTPServer(t)
	oversized := strings.Repeat("x", config.MaxTransferBytes+1)
	recorder := transferHTTPRequest(srv, "/import/preview", configTransferImportRequest{Content: oversized})
	if recorder.Code != 413 {
		t.Fatal("源大小未限制")
	}
	bad := strings.Replace(transferHTTPSource, "format: openai", "format: [fake-private-parse-secret]", 1)
	recorder = transferHTTPRequest(srv, "/import/preview", configTransferImportRequest{Content: bad})
	if recorder.Code != 400 || strings.Contains(recorder.Body.String(), "fake-private-parse-secret") {
		t.Fatal("解析错误泄露原文")
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config/import/preview", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://127.0.0.1:7789")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"original-revision"`)
	req.ContentLength = maxConfigTransferRequestBytes + 1
	response := httptest.NewRecorder()
	srv.handle(response, req)
	if response.Code != 413 {
		t.Fatal("外层请求体上限未限制")
	}
}
