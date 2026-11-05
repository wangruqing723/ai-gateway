package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/chatgptauth"
)

// 第三阶段单账户暂停/恢复接口测试：只使用假账户与 blockedClient，不出网。

func stateRequest(t *testing.T, srv *server, id, body, ifMatch string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/chatgpt/accounts/"+id+"/state", strings.NewReader(body))
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

type stateResponse struct {
	Data struct {
		Account   chatgptauth.AccountView `json:"account"`
		Changed   bool                    `json:"changed"`
		Providers []struct {
			Name          string `json:"name"`
			ConfigEnabled bool   `json:"configEnabled"`
			Effective     string `json:"effective"`
		} `json:"providers"`
	} `json:"data"`
}

func decodeState(t *testing.T, recorder *httptest.ResponseRecorder) stateResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态接口 = %d %s", recorder.Code, recorder.Body.String())
	}
	var out stateResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChatGPTAccountStateRejectsInvalidRequests(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	etag := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag")
	cases := []struct {
		name    string
		id      string
		body    string
		ifMatch string
		mutate  func(*http.Request)
		status  int
		errType string
	}{
		{"缺少 If-Match", first, `{"state":"paused"}`, "", nil, http.StatusPreconditionRequired, "precondition_required"},
		{"弱 ETag", first, `{"state":"paused"}`, "W/" + etag, nil, http.StatusPreconditionRequired, "precondition_required"},
		{"版本过期", first, `{"state":"paused"}`, `"stale"`, nil, http.StatusConflict, "account_conflict"},
		{"非法状态", first, `{"state":"disabled"}`, etag, nil, http.StatusBadRequest, "invalid_account_state"},
		{"布尔状态", first, `{"state":true}`, etag, nil, http.StatusBadRequest, "invalid_account_state"},
		{"未知字段", first, `{"state":"paused","enabled":false}`, etag, nil, http.StatusBadRequest, "invalid_account_state"},
		{"账户不存在", "acc_missing", `{"state":"paused"}`, etag, nil, http.StatusNotFound, "account_not_found"},
		{"非 JSON", first, `{"state":"paused"}`, etag, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"缺少 Origin", first, `{"state":"paused"}`, etag, func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden, ""},
		{"错误方法", first, ``, etag, func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := stateRequest(t, srv, tc.id, tc.body, tc.ifMatch, tc.mutate)
			if recorder.Code != tc.status || (tc.errType != "" && errorTypeOf(t, recorder) != tc.errType) {
				t.Fatalf("响应 = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if srv.chatgptAuth.IsPaused(first) {
		t.Fatal("被拒绝的请求不能改变账户状态")
	}
}

func TestChatGPTAccountPauseResumeFlow(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	etag := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag")
	recorder := stateRequest(t, srv, first, `{"state":"paused"}`, etag, nil)
	paused := decodeState(t, recorder)
	pausedETag := recorder.Header().Get("ETag")
	if !paused.Data.Changed || paused.Data.Account.Enabled || paused.Data.Account.PausedAt == "" || pausedETag == etag || !strictETag(pausedETag) {
		t.Fatalf("暂停 = %s ETag=%q", recorder.Body.String(), pausedETag)
	}
	if len(paused.Data.Providers) != 1 || paused.Data.Providers[0].Name != "codex" || !paused.Data.Providers[0].ConfigEnabled || paused.Data.Providers[0].Effective != "account_paused" {
		t.Fatalf("Provider 有效状态 = %#v", paused.Data.Providers)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("状态接口不应缓存")
	}
	if provider := codexProvider(srv); !provider.IsEnabled() || provider.AccountID != first {
		t.Fatalf("暂停不能修改 Provider 配置: %#v", provider)
	}
	if list := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", ""); list.Header().Get("ETag") != pausedETag {
		t.Fatalf("列表 ETag = %q，暂停响应 ETag = %q", list.Header().Get("ETag"), pausedETag)
	}

	// 重复暂停：无变更、不推进版本。
	again := stateRequest(t, srv, first, `{"state":"paused"}`, pausedETag, nil)
	if decodeState(t, again).Data.Changed || again.Header().Get("ETag") != pausedETag {
		t.Fatalf("重复暂停 = %s ETag=%q", again.Body.String(), again.Header().Get("ETag"))
	}
	// 旧 ETag 已过期。
	if stale := stateRequest(t, srv, first, `{"state":"active"}`, etag, nil); stale.Code != http.StatusConflict {
		t.Fatalf("旧版本恢复 = %d", stale.Code)
	}
	resumed := decodeState(t, stateRequest(t, srv, first, `{"state":"active"}`, pausedETag, nil))
	if !resumed.Data.Changed || !resumed.Data.Account.Enabled || resumed.Data.Account.PausedAt != "" || resumed.Data.Providers[0].Effective != "active" {
		t.Fatalf("恢复 = %#v", resumed.Data)
	}
}

func TestChatGPTAccountStateReportsDisabledProvider(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "    enabled: false\n")
	etag := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag")
	paused := decodeState(t, stateRequest(t, srv, first, `{"state":"paused"}`, etag, nil))
	if len(paused.Data.Providers) != 1 || paused.Data.Providers[0].ConfigEnabled || paused.Data.Providers[0].Effective != "provider_disabled" {
		t.Fatalf("停用 Provider 应报告 provider_disabled: %#v", paused.Data.Providers)
	}
	// 恢复账户不会启用原本停用的 Provider。
	resumed := stateRequest(t, srv, first, `{"state":"active"}`, chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag"), nil)
	provider := codexProvider(srv)
	if decodeState(t, resumed).Data.Providers[0].Effective != "provider_disabled" || provider.IsEnabled() {
		t.Fatalf("恢复后 = %s", resumed.Body.String())
	}
}

func TestChatGPTMetadataPatchReturnsAccountVersion(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	before := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag")
	recorder := chatGPTAPI(t, srv, http.MethodPatch, "/api/chatgpt/accounts/"+first, `{"note":"新备注"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", recorder.Code, recorder.Body.String())
	}
	etag := recorder.Header().Get("ETag")
	if etag == before || etag != chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "").Header().Get("ETag") {
		t.Fatalf("PATCH ETag = %q，修改前 %q", etag, before)
	}
	// 新 ETag 可直接用于后续暂停。
	decodeState(t, stateRequest(t, srv, first, `{"state":"paused"}`, etag, nil))
}
