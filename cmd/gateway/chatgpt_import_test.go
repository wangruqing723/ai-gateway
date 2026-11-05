package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/chatgptauth"
)

// importRequest 以本机浏览器同源方式调用导入预检/应用接口。
func importRequest(t *testing.T, srv *server, path string, body any, ifMatch string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	data, raw := body.([]byte)
	if !raw {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/chatgpt/accounts/import/"+path, bytes.NewReader(data))
	request.Header.Set("Origin", "http://127.0.0.1:7789")
	request.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	srv.handle(recorder, request)
	return recorder
}

func jsonSource(content string) chatGPTImportSource {
	return chatGPTImportSource{MediaType: "application/json", Encoding: "utf8", Content: content}
}

type previewResponse struct {
	Data struct {
		CanApply     bool                          `json:"canApply"`
		NoChanges    bool                          `json:"noChanges"`
		BaseVersion  string                        `json:"baseVersion"`
		Summary      chatgptauth.ImportPlanSummary `json:"summary"`
		Entries      []chatgptauth.ImportPlanEntry `json:"entries"`
		PreviewToken *string                       `json:"previewToken"`
		ExpiresAt    *string                       `json:"expiresAt"`
	} `json:"data"`
}

func decodePreview(t *testing.T, recorder *httptest.ResponseRecorder) previewResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("预检 = %d %s", recorder.Code, recorder.Body.String())
	}
	var out previewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// updatePayload 让第一个假账户（remote-a）获得新凭据，并新增 remote-c。
const updatePayload = `[
  {"access_token":"new-access-a","refresh_token":"new-refresh-a","account_id":"remote-a","expires_at":"2099-02-01T00:00:00Z"},
  {"access_token":"new-access-c","refresh_token":"new-refresh-c","account_id":"remote-c","email":"c@example.invalid"}
]`

func TestChatGPTImportPreviewApplyFlow(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	tags := []string{"保留"}
	if _, err := srv.chatgptAuth.UpdateMetadata(first, chatgptauth.AccountMetadataPatch{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	list := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "")
	actions := []chatgptauth.ImportAction{{Index: 1, Action: "update", TargetAccountID: first}}
	request := chatGPTImportRequest{Source: jsonSource(updatePayload), Actions: actions}
	recorder := importRequest(t, srv, "preview", request, "", nil)
	preview := decodePreview(t, recorder)
	if !preview.Data.CanApply || preview.Data.PreviewToken == nil || preview.Data.ExpiresAt == nil ||
		preview.Data.Summary != (chatgptauth.ImportPlanSummary{Total: 2, Add: 1, Update: 1}) {
		t.Fatalf("预检 = %s", recorder.Body.String())
	}
	if etag := recorder.Header().Get("ETag"); etag != list.Header().Get("ETag") || etag != strconv.Quote(preview.Data.BaseVersion) {
		t.Fatalf("预检 ETag = %q，列表 ETag = %q", etag, list.Header().Get("ETag"))
	}
	if got := preview.Data.Entries[0].AffectedProviders; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("受影响 Provider = %#v", got)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), "new-access") || strings.Contains(recorder.Body.String(), "new-refresh") {
		t.Fatalf("预检响应不应缓存或包含凭据: %s", recorder.Body.String())
	}
	request.PreviewToken = *preview.Data.PreviewToken
	applied := importRequest(t, srv, "apply", request, strconv.Quote(preview.Data.BaseVersion), nil)
	if applied.Code != http.StatusOK || !strings.Contains(applied.Body.String(), `"updated":1`) || !strings.Contains(applied.Body.String(), `"imported":1`) {
		t.Fatalf("应用 = %d %s", applied.Code, applied.Body.String())
	}
	if strings.Contains(applied.Body.String(), "new-access") || applied.Header().Get("ETag") == strconv.Quote(preview.Data.BaseVersion) {
		t.Fatalf("应用响应应推进 ETag 且不含凭据: %s", applied.Body.String())
	}
	token, _, err := srv.chatgptAuth.AccessToken(context.Background(), first, blockedClient)
	view, _ := srv.chatgptAuth.Account(first)
	if err != nil || token != "new-access-a" || codexProvider(srv).AccountID != first || len(view.Tags) != 1 {
		t.Fatalf("更新后应保留本地 ID、绑定和标签: token=%q err=%v view=%#v", token, err, view)
	}
	if replay := importRequest(t, srv, "apply", request, strconv.Quote(preview.Data.BaseVersion), nil); replay.Code != http.StatusConflict || errorTypeOf(t, replay) != "account_conflict" {
		t.Fatalf("同一预检凭证不能重放: %d %s", replay.Code, replay.Body.String())
	}
}

func TestChatGPTImportPreviewWithoutApplicablePlanHasNoToken(t *testing.T) {
	srv, _, _ := newChatGPTGatewayServer(t, "")
	for name, payload := range map[string]string{
		"存在无效条目": `[{"access_token":"a","refresh_token":"r","account_id":"remote-z"},{"account_id":"remote-y"}]`,
		"全部重复跳过": `{"access_token":"a","refresh_token":"r","account_id":"remote-a"}`,
	} {
		preview := decodePreview(t, importRequest(t, srv, "preview", chatGPTImportRequest{Source: jsonSource(payload)}, "", nil))
		if preview.Data.CanApply || preview.Data.PreviewToken != nil {
			t.Errorf("%s 不应签发预检凭证: %#v", name, preview.Data)
		}
	}
	zipped := base64.StdEncoding.EncodeToString(zipBytes(t, map[string]string{"a.json": `{"access_token":"a","refresh_token":"r","account_id":"remote-z"}`}))
	preview := decodePreview(t, importRequest(t, srv, "preview", chatGPTImportRequest{Source: chatGPTImportSource{MediaType: "application/zip", Encoding: "base64", Content: zipped}}, "", nil))
	if !preview.Data.CanApply || preview.Data.Summary.Add != 1 {
		t.Fatalf("ZIP 预检 = %#v", preview.Data)
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestChatGPTImportPlanBoundaryChecks(t *testing.T) {
	srv, _, _ := newChatGPTGatewayServer(t, "")
	valid := chatGPTImportRequest{Source: jsonSource(updatePayload)}
	cases := []struct {
		name    string
		path    string
		body    any
		ifMatch string
		mutate  func(*http.Request)
		status  int
		errType string
	}{
		{"错误方法", "preview", valid, "", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed, ""},
		{"缺少 Origin", "preview", valid, "", func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden, ""},
		{"跨站 Origin", "preview", valid, "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden, ""},
		{"错误媒体类型", "preview", valid, "", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"未知字段", "preview", []byte(`{"source":{"mediaType":"application/json","encoding":"utf8","content":"[]"},"extra":1}`), "", nil, http.StatusBadRequest, "invalid_import"},
		{"重复键", "preview", []byte(`{"source":{"mediaType":"application/json","encoding":"utf8","content":"[]","content":"[]"}}`), "", nil, http.StatusBadRequest, "invalid_import"},
		{"尾随值", "preview", []byte(`{"source":{"mediaType":"application/json","encoding":"utf8","content":"[]"}} {}`), "", nil, http.StatusBadRequest, "invalid_import"},
		{"预检携带凭证", "preview", chatGPTImportRequest{Source: valid.Source, PreviewToken: "x"}, "", nil, http.StatusBadRequest, "invalid_import"},
		{"不支持的源类型", "preview", chatGPTImportRequest{Source: chatGPTImportSource{MediaType: "text/plain", Encoding: "utf8", Content: "x"}}, "", nil, http.StatusBadRequest, "invalid_import"},
		{"base64 无效", "preview", chatGPTImportRequest{Source: chatGPTImportSource{MediaType: "application/zip", Encoding: "base64", Content: "@@@"}}, "", nil, http.StatusBadRequest, "invalid_import"},
		{"动作序号越界", "preview", chatGPTImportRequest{Source: valid.Source, Actions: []chatgptauth.ImportAction{{Index: 9, Action: "skip"}}}, "", nil, http.StatusBadRequest, "invalid_import"},
		{"源内容超限", "preview", chatGPTImportRequest{Source: jsonSource(strings.Repeat(" ", maxChatGPTImportSourceBytes+1))}, "", nil, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"应用缺少 If-Match", "apply", valid, "", nil, http.StatusPreconditionRequired, "precondition_required"},
		{"应用使用弱 ETag", "apply", valid, `W/"x"`, nil, http.StatusPreconditionRequired, "precondition_required"},
		{"应用使用通配符", "apply", valid, `*`, nil, http.StatusPreconditionRequired, "precondition_required"},
		{"应用凭证无效", "apply", chatGPTImportRequest{Source: valid.Source, PreviewToken: "forged.token"}, `"x"`, nil, http.StatusBadRequest, "invalid_preview_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := importRequest(t, srv, tc.path, tc.body, tc.ifMatch, tc.mutate)
			if recorder.Code != tc.status || (tc.errType != "" && errorTypeOf(t, recorder) != tc.errType) {
				t.Fatalf("= %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if len(srv.chatgptAuth.List(nil)) != 2 {
		t.Fatal("被拒绝的请求不应改动账户")
	}
}

func TestChatGPTImportApplyRejectsStalePreview(t *testing.T) {
	setup := func(t *testing.T) (*server, string, chatGPTImportRequest, string) {
		t.Helper()
		srv, first, _ := newChatGPTGatewayServer(t, "")
		request := chatGPTImportRequest{Source: jsonSource(updatePayload), Actions: []chatgptauth.ImportAction{{Index: 1, Action: "update", TargetAccountID: first}}}
		preview := decodePreview(t, importRequest(t, srv, "preview", request, "", nil))
		request.PreviewToken = *preview.Data.PreviewToken
		return srv, first, request, strconv.Quote(preview.Data.BaseVersion)
	}
	expectRejected := func(t *testing.T, srv *server, recorder *httptest.ResponseRecorder, status int, errType string) {
		t.Helper()
		if recorder.Code != status || errorTypeOf(t, recorder) != errType {
			t.Fatalf("= %d %s，期望 %d %s", recorder.Code, recorder.Body.String(), status, errType)
		}
		if creds, _, _ := srv.chatgptAuth.AccessToken(context.Background(), srv.chatgptAuth.List(nil)[0].ID, blockedClient); creds != "fake-access-a" || len(srv.chatgptAuth.List(nil)) != 2 {
			t.Fatal("被拒绝的应用不应改动账户")
		}
	}
	t.Run("篡改凭证", func(t *testing.T) {
		srv, _, request, etag := setup(t)
		last := request.PreviewToken[len(request.PreviewToken)-1]
		flipped := byte('0')
		if last == '0' {
			flipped = '1'
		}
		request.PreviewToken = request.PreviewToken[:len(request.PreviewToken)-1] + string(flipped)
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusBadRequest, "invalid_preview_token")
	})
	t.Run("过期", func(t *testing.T) {
		srv, _, request, etag := setup(t)
		srv.transferNow = func() time.Time { return time.Now().Add(chatGPTImportPreviewTTL + time.Second) }
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusGone, "preview_expired")
	})
	t.Run("选择改变", func(t *testing.T) {
		srv, _, request, etag := setup(t)
		request.Actions = append(request.Actions, chatgptauth.ImportAction{Index: 2, Action: "skip"})
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "preview_mismatch")
	})
	t.Run("内容改变", func(t *testing.T) {
		srv, _, request, etag := setup(t)
		request.Source.Content = strings.Replace(updatePayload, "new-access-c", "other-access-c", 1)
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "preview_mismatch")
	})
	t.Run("If-Match 不一致", func(t *testing.T) {
		srv, _, request, _ := setup(t)
		expectRejected(t, srv, importRequest(t, srv, "apply", request, `"other"`, nil), http.StatusConflict, "account_conflict")
	})
	t.Run("账户在预检后变化", func(t *testing.T) {
		srv, first, request, etag := setup(t)
		note := "并发修改"
		if _, err := srv.chatgptAuth.UpdateMetadata(first, chatgptauth.AccountMetadataPatch{Note: &note}); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "account_conflict")
	})
	t.Run("绑定配置在预检后变化", func(t *testing.T) {
		srv, _, request, etag := setup(t)
		srv.cfgMu.Lock()
		srv.revision = "changed-revision"
		srv.cfgMu.Unlock()
		expectRejected(t, srv, importRequest(t, srv, "apply", request, etag, nil), http.StatusConflict, "config_conflict")
	})
}
