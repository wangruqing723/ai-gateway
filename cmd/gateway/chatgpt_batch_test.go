package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-gateway/internal/chatgptauth"
)

// batchRequest 只访问本机假账户管理接口，不访问真实凭据或上游。
func batchRequest(t *testing.T, srv *server, path string, body any, etag string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/chatgpt/accounts/"+path, bytes.NewReader(data))
	req.Header.Set("Origin", "http://127.0.0.1:7789")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	srv.handle(rec, req)
	return rec
}

func batchPreview(t *testing.T, srv *server, request chatGPTBatchRequest) (chatGPTBatchRequest, string) {
	t.Helper()
	rec := batchRequest(t, srv, "batch/preview", request, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("预检失败: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data chatGPTBatchPreview `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.PreviewToken == nil {
		t.Fatalf("缺少预检凭证: %s", rec.Body.String())
	}
	request.PreviewToken = *response.Data.PreviewToken
	return request, rec.Header().Get("ETag")
}

func TestChatGPTBatchApplyAndReplay(t *testing.T) {
	srv, first, second := newChatGPTGatewayServer(t, "")
	request, etag := batchPreview(t, srv, chatGPTBatchRequest{Operation: "pause", AccountIDs: []string{first, second}})
	rec := batchRequest(t, srv, "batch/apply", request, etag)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag || !srv.chatgptAuth.IsPaused(first) || !srv.chatgptAuth.IsPaused(second) {
		t.Fatalf("应用失败: %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String())
	replay := batchRequest(t, srv, "batch/apply", request, etag)
	if replay.Code != http.StatusConflict || errorTypeOf(t, replay) != "account_conflict" {
		t.Fatalf("重复应用: %d %s", replay.Code, replay.Body.String())
	}
}

func TestChatGPTBatchRejectsChangedPreview(t *testing.T) {
	for _, scenario := range []string{"input", "account", "config", "expired", "missing_etag", "import_key"} {
		t.Run(scenario, func(t *testing.T) {
			srv, first, _ := newChatGPTGatewayServer(t, "")
			now := time.Now()
			srv.transferNow = func() time.Time { return now }
			request, etag := batchPreview(t, srv, chatGPTBatchRequest{Operation: "pause", AccountIDs: []string{first}})
			status, category := http.StatusConflict, "preview_mismatch"
			switch scenario {
			case "input":
				request.Operation = "resume"
			case "account":
				note := "版本变化"
				if _, err := srv.chatgptAuth.UpdateMetadata(first, chatgptauth.AccountMetadataPatch{Note: &note}); err != nil {
					t.Fatal(err)
				}
				category = "account_conflict"
			case "config":
				srv.revision = "different-config"
				category = "config_conflict"
			case "expired":
				now = now.Add(11 * time.Minute)
				status, category = http.StatusGone, "preview_expired"
			case "missing_etag":
				etag = ""
				status, category = http.StatusPreconditionRequired, "precondition_required"
			case "import_key":
				key, err := srv.chatGPTBatchKey()
				if err != nil {
					t.Fatal(err)
				}
				token, err := verifyChatGPTImportToken(request.PreviewToken, key)
				if err != nil {
					t.Fatal(err)
				}
				importKey, err := srv.chatGPTImportKey()
				if err != nil {
					t.Fatal(err)
				}
				request.PreviewToken, _, err = srv.signChatGPTImportToken(importKey, token)
				if err != nil {
					t.Fatal(err)
				}
				status, category = http.StatusBadRequest, "invalid_preview_token"
			}
			rec := batchRequest(t, srv, "batch/apply", request, etag)
			if rec.Code != status || errorTypeOf(t, rec) != category {
				t.Fatalf("响应: %d %s", rec.Code, rec.Body.String())
			}
			if srv.chatgptAuth.IsPaused(first) {
				t.Fatal("拒绝的请求改变了暂停状态")
			}
		})
	}
}

func TestChatGPTBatchDeleteBoundAccountBlocked(t *testing.T) {
	srv, first, second := newChatGPTGatewayServer(t, "    enabled: false\n")
	rec := batchRequest(t, srv, "batch/preview", chatGPTBatchRequest{Operation: "delete", AccountIDs: []string{first, second}}, "")
	var response struct {
		Data chatGPTBatchPreview `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.Data.CanApply || response.Data.PreviewToken != nil || response.Data.Summary.Blocked != 1 {
		t.Fatalf("引用删除应阻断: %d %s", rec.Code, rec.Body.String())
	}
	if len(srv.chatgptAuth.List(nil)) != 2 {
		t.Fatal("预检不能删除账户")
	}
}

func TestChatGPTUsageBatchBudgetAndGuard(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	srv, first, _ := newDiagnoseServer(t, up, "")
	previous := chatGPTUsageBatchBudget
	chatGPTUsageBatchBudget = 100 * time.Millisecond
	t.Cleanup(func() { chatGPTUsageBatchBudget = previous })
	request := map[string]any{"accountIds": []string{first}, "refreshMode": "force"}
	srv.chatGPTBatchRefreshing.Store(true)
	rec := batchRequest(t, srv, "usage/batch-refresh", request, "")
	if rec.Code != http.StatusConflict || errorTypeOf(t, rec) != "batch_refresh_in_progress" {
		t.Fatalf("并发守卫: %d %s", rec.Code, rec.Body.String())
	}
	srv.chatGPTBatchRefreshing.Store(false)
	rec = batchRequest(t, srv, "usage/batch-refresh", request, "")
	var response struct {
		Data struct {
			Results []chatgptauth.UsageBatchResult `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(response.Data.Results) != 1 || response.Data.Results[0].Status != "cancelled" || srv.chatGPTBatchRefreshing.Load() {
		t.Fatalf("预算取消: %d %s", rec.Code, rec.Body.String())
	}
	view, _ := srv.chatgptAuth.Account(first)
	if view.Usage != nil && view.Usage.Error != "" {
		t.Fatalf("取消污染快照: %#v", view.Usage)
	}
}
