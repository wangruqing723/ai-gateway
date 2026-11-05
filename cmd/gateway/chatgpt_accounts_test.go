package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/router"
)

// 本文件只使用临时目录、假账户与假凭据；blockedTransport 保证测试不会意外访问真实 OAuth 或 ChatGPT。

type blockedTransport struct{}

func (blockedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("测试禁止出网: %s", r.URL.Host)
}

var blockedClient = &http.Client{Transport: blockedTransport{}}

// chatGPTGatewayYAML 生成一个普通 Provider 加一个 OAuth Provider 的配置；codexExtra 追加到 codex 节下。
func chatGPTGatewayYAML(accountID, codexExtra string, maxConcurrent int) string {
	return fmt.Sprintf(`host: "127.0.0.1"
port: 7789
providers:
  primary:
    baseUrl: "https://api.example.com"
    apiKey: "fake-primary-key"
    format: openai
    maxConcurrent: %d
    maxPerSecond: 0
    maxQueueWait: 30000
  codex:
    baseUrl: "https://chatgpt.com/backend-api/codex"
    format: openai-responses
    authType: chatgptOAuth
    accountId: %q
%s    maxConcurrent: 2
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "gpt-*"
    provider: codex
    model: gpt-6-luna
  - match: "*"
    provider: primary
    model: upstream
`, maxConcurrent, accountID, codexExtra)
}

// newFakeChatGPTManager 导入两个按邮箱排序的假账户，返回管理器与本地账户 ID。
func newFakeChatGPTManager(t *testing.T) (*chatgptauth.Manager, string, string) {
	t.Helper()
	mgr, err := chatgptauth.New(filepath.Join(t.TempDir(), "fake-accounts.json"), blockedClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Import([]byte(`[
  {"access_token":"fake-access-a","refresh_token":"fake-refresh-a","account_id":"remote-a","email":"a@example.invalid","expires_at":"2099-01-01T00:00:00Z"},
  {"access_token":"fake-access-b","refresh_token":"fake-refresh-b","account_id":"remote-b","email":"b@example.invalid","expires_at":"2099-01-01T00:00:00Z"}
]`), false); err != nil {
		t.Fatal(err)
	}
	views := mgr.List(nil)
	if len(views) != 2 {
		t.Fatalf("假账户数 = %d", len(views))
	}
	return mgr, views[0].ID, views[1].ID
}

func newChatGPTGatewayServer(t *testing.T, codexExtra string) (*server, string, string) {
	t.Helper()
	mgr, first, second := newFakeChatGPTManager(t)
	srv := newConfigTestServer(t, chatGPTGatewayYAML(first, codexExtra, 5))
	srv.chatgptAuth = mgr
	return srv, first, second
}

func chatGPTAPI(t *testing.T, srv *server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "http://127.0.0.1:7789"+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	srv.handle(recorder, request)
	return recorder
}

func codexProvider(srv *server) config.Provider {
	srv.cfgMu.RLock()
	defer srv.cfgMu.RUnlock()
	return *srv.cfg.Providers["codex"]
}

func errorTypeOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return body.Error.Type
}

// TestPutConfigChatGPTAccountReferenceValidation 锁定普通保存与配置层同一空引用语义（T01）。
func TestPutConfigChatGPTAccountReferenceValidation(t *testing.T) {
	cases := []struct {
		name      string
		accountID string // "@first" 替换成真实账户 ID
		extra     string
		status    int
		errType   string
		message   string
	}{
		{"停用且未绑定可保存", "", "    enabled: false\n", http.StatusOK, "", ""},
		{"启用但未绑定被拒绝", "", "", http.StatusBadRequest, "config_validation_error", "accountId 不能为空"},
		{"停用但引用不存在账户被拒绝", "acc_missing", "    enabled: false\n", http.StatusBadRequest, "config_validation_error", "引用了不存在的 ChatGPT OAuth 账户"},
		{"启用且引用不存在账户被拒绝", "acc_missing", "", http.StatusBadRequest, "config_validation_error", "引用了不存在的 ChatGPT OAuth 账户"},
		{"启用且引用现有账户可保存", "@first", "", http.StatusOK, "", ""},
		{"停用且引用现有账户可保存", "@first", "    enabled: false\n", http.StatusOK, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, first, _ := newChatGPTGatewayServer(t, "")
			accountID := strings.ReplaceAll(tc.accountID, "@first", first)
			before := mustReadFile(t, gotConfigPath(srv))
			recorder := putConfig(t, srv, chatGPTGatewayYAML(accountID, tc.extra, 6), "")
			if recorder.Code != tc.status {
				t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
			}
			if tc.status != http.StatusOK {
				if errorTypeOf(t, recorder) != tc.errType || !strings.Contains(recorder.Body.String(), tc.message) {
					t.Fatalf("错误响应 = %s", recorder.Body.String())
				}
				if string(mustReadFile(t, gotConfigPath(srv))) != string(before) || codexProvider(srv).AccountID != first {
					t.Fatal("被拒绝的保存不应改动磁盘或内存配置")
				}
				return
			}
			if got := codexProvider(srv); got.AccountID != accountID {
				t.Fatalf("保存后 accountId = %q", got.AccountID)
			}
		})
	}
	t.Run("账户管理器不可用", func(t *testing.T) {
		srv, first, _ := newChatGPTGatewayServer(t, "")
		srv.chatgptAuth = nil
		if recorder := putConfig(t, srv, chatGPTGatewayYAML("", "    enabled: false\n", 6), ""); recorder.Code != http.StatusOK {
			t.Fatalf("停用且未绑定不依赖账户管理器: %d %s", recorder.Code, recorder.Body.String())
		}
		recorder := putConfig(t, srv, chatGPTGatewayYAML(first, "", 6), "")
		if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "oauth_unavailable" {
			t.Fatalf("有引用时应报管理器不可用: %d %s", recorder.Code, recorder.Body.String())
		}
	})
}

// TestChatGPTBindUnbindSaveRebindFlow 覆盖解绑后普通保存、重绑不丢其他配置（T05）。
func TestChatGPTBindUnbindSaveRebindFlow(t *testing.T) {
	srv, first, second := newChatGPTGatewayServer(t, "")
	if recorder := chatGPTAPI(t, srv, http.MethodDelete, "/api/chatgpt/accounts/"+first+"/bindings/codex", ""); recorder.Code != http.StatusOK {
		t.Fatalf("解绑 = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := codexProvider(srv); got.AccountID != "" || got.IsEnabled() {
		t.Fatalf("解绑后 codex = %#v", got)
	}
	// 解绑后的普通保存：前端会原样回传停用且未绑定的 codex，同时修改无关设置。
	unbound := chatGPTGatewayYAML("", "    enabled: false\n", 9)
	unbound = strings.Replace(unbound, `apiKey: "fake-primary-key"`, `apiKey: "`+config.APIKeyKeepSentinel+`"`, 1)
	if recorder := putConfig(t, srv, unbound, ""); recorder.Code != http.StatusOK {
		t.Fatalf("解绑后保存无关设置 = %d %s", recorder.Code, recorder.Body.String())
	}
	srv.cfgMu.RLock()
	primary := *srv.cfg.Providers["primary"]
	routes := len(srv.cfg.Routes)
	srv.cfgMu.RUnlock()
	if primary.MaxConcurrent != 9 || primary.APIKey != "fake-primary-key" || routes != 2 {
		t.Fatalf("保存后的其他配置 = %#v routes=%d", primary, routes)
	}

	if recorder := chatGPTAPI(t, srv, http.MethodPut, "/api/chatgpt/accounts/"+second+"/bindings/codex", ""); recorder.Code != http.StatusOK {
		t.Fatalf("重绑 = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := codexProvider(srv); got.AccountID != second || !got.IsEnabled() {
		t.Fatalf("重绑后 codex = %#v", got)
	}
	srv.cfgMu.RLock()
	primary = *srv.cfg.Providers["primary"]
	routes = len(srv.cfg.Routes)
	srv.cfgMu.RUnlock()
	if primary.MaxConcurrent != 9 || primary.APIKey != "fake-primary-key" || routes != 2 {
		t.Fatalf("重绑丢失其他配置 = %#v routes=%d", primary, routes)
	}
	disk, err := config.DecodeAndValidate(mustReadFile(t, gotConfigPath(srv)))
	if err != nil || disk.Providers["codex"].AccountID != second || disk.Providers["primary"].MaxConcurrent != 9 {
		t.Fatalf("重绑后磁盘配置 = %v", err)
	}

	// 账户列表保留旧字段，绑定关系随重绑更新。
	var listed struct {
		Data []map[string]any `json:"data"`
	}
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil || len(listed.Data) != 2 {
		t.Fatalf("账户列表 = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, item := range listed.Data {
		for _, key := range []string{"id", "status", "expiresAt", "authorization", "usageState", "invocation"} {
			if _, ok := item[key]; !ok {
				t.Fatalf("账户视图缺少字段 %q: %#v", key, item)
			}
		}
		if item["status"] != "ready" {
			t.Fatalf("旧 status 字段 = %#v", item["status"])
		}
	}
	if providers, _ := listed.Data[1]["providers"].([]any); listed.Data[1]["id"] != second || len(providers) != 1 || providers[0] != "codex" {
		t.Fatalf("重绑后的引用关系 = %#v", listed.Data[1])
	}
	if _, ok := listed.Data[0]["providers"]; ok {
		t.Fatalf("原账户不应再显示绑定: %#v", listed.Data[0])
	}
	if recorder := chatGPTAPI(t, srv, http.MethodDelete, "/api/chatgpt/accounts/"+second, ""); recorder.Code != http.StatusConflict || errorTypeOf(t, recorder) != "account_in_use" {
		t.Fatalf("仍被绑定的账户应拒绝删除: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatGPTAPI(t, srv, http.MethodDelete, "/api/chatgpt/accounts/"+first, ""); recorder.Code != http.StatusOK {
		t.Fatalf("未绑定账户删除 = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestBindSameAccountKeepsProviderEnabledState 锁定同账户重复绑定不改变 Provider 启停（T05）。
func TestBindSameAccountKeepsProviderEnabledState(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "    enabled: false\n")
	before := mustReadFile(t, gotConfigPath(srv))
	recorder := chatGPTAPI(t, srv, http.MethodPut, "/api/chatgpt/accounts/"+first+"/bindings/codex", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"bound"`) {
		t.Fatalf("重复绑定 = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := codexProvider(srv); got.AccountID != first || got.IsEnabled() {
		t.Fatalf("同账户重复绑定改变了启停状态: %#v", got)
	}
	if string(mustReadFile(t, gotConfigPath(srv))) != string(before) {
		t.Fatal("同账户重复绑定不应重写配置文件")
	}
	// 解绑不存在的账户返回 404，不改动配置。
	if recorder := chatGPTAPI(t, srv, http.MethodDelete, "/api/chatgpt/accounts/acc_missing/bindings/codex", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("解绑不存在的账户 = %d", recorder.Code)
	}
}

// newFixedChatGPTManager 直接写入固定 ID 的假账户文件，便于构造需要重新授权和已过期账户。
func newFixedChatGPTManager(t *testing.T) *chatgptauth.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-accounts.json")
	store := `{"version":1,"accounts":[
  {"id":"acc_ready","chatgptAccountId":"remote-ready","accessToken":"fake-a","refreshToken":"fake-r","expiresAt":"2099-01-01T00:00:00Z"},
  {"id":"acc_reauth","chatgptAccountId":"remote-reauth","accessToken":"fake-a","refreshToken":"fake-r","expiresAt":"2099-01-01T00:00:00Z","needsReauth":true},
  {"id":"acc_expired","chatgptAccountId":"remote-expired","accessToken":"fake-a","refreshToken":"fake-r","expiresAt":"2000-01-01T00:00:00Z"}
]}`
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := chatgptauth.New(path, blockedClient)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func oauthProvider(name, accountID string) *config.Provider {
	return &config.Provider{Name: name, BaseURL: "https://chatgpt.com/backend-api/codex", Format: "openai-responses", AuthType: "chatgptOAuth", AccountID: accountID}
}

// TestChatGPTProviderStatusRequiresRealInvocation 锁定 OAuth Provider 健康只来自真实调用（T03/T04）。
func TestChatGPTProviderStatusRequiresRealInvocation(t *testing.T) {
	srv := &server{}
	if got := srv.chatGPTProviderStatus("codex", oauthProvider("codex", "acc_ready")); got.Status != "error" || !strings.Contains(got.Message, "管理器不可用") {
		t.Fatalf("无管理器 = %#v", got)
	}
	srv.chatgptAuth = newFixedChatGPTManager(t)
	basic := []struct {
		name, accountID, status, message string
	}{
		{"未绑定", "", "unchecked", "未绑定 ChatGPT OAuth 账户"},
		{"账户不存在", "acc_missing", "error", "绑定的 ChatGPT OAuth 账户不存在"},
		{"需要重新授权", "acc_reauth", "error", "ChatGPT OAuth 账户需要重新授权"},
		{"凭据可用但未实测", "acc_ready", "unverified", "访问令牌有效，到期前会在使用时自动刷新；尚未经网关实际调用验证"},
		{"可刷新但未实测", "acc_expired", "unverified", "访问令牌已过期，下次使用时会尝试用刷新凭据续期；尚未经网关实际调用验证"},
	}
	for _, tc := range basic {
		got := srv.chatGPTProviderStatus("codex", oauthProvider("codex", tc.accountID))
		if got.Status != tc.status || got.Message != tc.message || got.Name != "codex" || got.Endpoint != "https://chatgpt.com/backend-api/codex/responses" {
			t.Errorf("%s: %#v", tc.name, got)
		}
	}
	mgr := srv.chatgptAuth
	mgr.FinishInvocation(mgr.BeginInvocation("acc_ready", "codex", "gpt-6-luna"), chatgptauth.InvocationResult{Success: true, HTTPStatus: 200})
	got := srv.chatGPTProviderStatus("codex", oauthProvider("codex", "acc_ready"))
	if got.Status != "ok" || got.Message != "最近一次真实调用成功（模型 gpt-6-luna）" || got.CheckedAt == "" {
		t.Fatalf("真实调用成功 = %#v", got)
	}
	// 同一账户经另一个 Provider 的成功不能证明本 Provider 可用。
	if other := srv.chatGPTProviderStatus("codex-backup", oauthProvider("codex-backup", "acc_ready")); other.Status != "unverified" {
		t.Fatalf("其他 Provider 不应继承调用结果: %#v", other)
	}
	failures := []struct {
		status int
		reason string
		want   string
	}{
		{429, "429/ratelimit", "warn"},
		{400, "400/error", "warn"},
		{200, "conversion_error", "warn"},
		{401, "401/error", "error"},
		{403, "403/auth", "error"},
		{502, "502/server", "error"},
		{0, "transport_error", "error"},
		{200, "stream_interrupted", "error"},
	}
	for _, tc := range failures {
		mgr.FinishInvocation(mgr.BeginInvocation("acc_ready", "codex", "gpt-6-luna"), chatgptauth.InvocationResult{HTTPStatus: tc.status, Reason: tc.reason})
		got := srv.chatGPTProviderStatus("codex", oauthProvider("codex", "acc_ready"))
		if got.Status != tc.want || got.HTTPCode != tc.status || !strings.HasSuffix(got.Message, "（模型 gpt-6-luna）") {
			t.Errorf("%d %s: %#v", tc.status, tc.reason, got)
		}
	}
	// 自动刷新失败比最近一次调用更新时，健康显示刷新问题。
	if _, _, err := mgr.AccessToken(context.Background(), "acc_expired", blockedClient); err == nil {
		t.Fatal("被阻断的刷新应失败")
	}
	got = srv.chatGPTProviderStatus("codex", oauthProvider("codex", "acc_expired"))
	if got.Status != "warn" || !strings.Contains(got.Message, "无法连接 OAuth 服务") || got.CheckedAt == "" {
		t.Fatalf("刷新失败 = %#v", got)
	}
}

func TestChatGPTInvocationResultClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		err      error
		recorded bool
		success  bool
		reason   string
	}{
		{"成功", 200, nil, true, true, ""},
		{"没有上游响应也没有错误", 0, nil, false, false, ""},
		{"客户端取消", 200, context.Canceled, false, false, ""},
		{"包装后的客户端取消", 0, fmt.Errorf("wrapped: %w", context.Canceled), false, false, ""},
		{"限流", 429, nil, true, false, "429/ratelimit"},
		{"鉴权拒绝", 401, nil, true, false, "401/error"},
		{"上游 5xx", 502, nil, true, false, "502/server"},
		{"传输错误", 0, errors.New("dial failed"), true, false, "transport_error"},
		{"响应头超时", 0, proxy.ErrStreamHeaderTimeout, true, false, "header_timeout"},
		{"整体超时", 0, proxy.ErrRequestTimeout, true, false, "timeout"},
		{"成功响应头后流中断", 200, errors.New("unexpected EOF"), true, false, "stream_interrupted"},
		{"成功响应头后活跃超时", 200, context.DeadlineExceeded, true, false, "stream_interrupted"},
		{"响应转换失败", 200, proxy.ErrConversion, true, false, "conversion_error"},
		{"流内限流", 200, &proxy.UpstreamStreamError{Status: 429}, true, false, "429/ratelimit"},
		{"流内过载", 200, &proxy.UpstreamStreamError{Status: 529}, true, false, "529/server"},
	}
	for _, tc := range cases {
		result, ok := chatGPTInvocationResult(tc.status, tc.err)
		if ok != tc.recorded || (ok && (result.Success != tc.success || result.Reason != tc.reason)) {
			t.Errorf("%s: %#v %v", tc.name, result, ok)
		}
	}
	if result, _ := chatGPTInvocationResult(200, &proxy.UpstreamStreamError{Status: 429}); result.HTTPStatus != 429 {
		t.Errorf("流内失败应记录等效状态码: %#v", result)
	}
}

// TestForwardAttemptRecordsChatGPTInvocation 用假上游验证真实尝试结果会写回账户和 Provider 调用状态。
func TestForwardAttemptRecordsChatGPTInvocation(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		state    string
		provider string
	}{
		{"成功", http.StatusOK, "success", "ok"},
		{"限流", http.StatusTooManyRequests, "failure", "warn"},
		{"鉴权拒绝", http.StatusUnauthorized, "failure", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if tc.status != http.StatusOK {
					w.Header().Set("content-type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"fake upstream failure"}}`)
					return
				}
				w.Header().Set("content-type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			t.Cleanup(upstream.Close)
			mgr := newFixedChatGPTManager(t)
			provider := oauthProvider("codex", "acc_ready")
			provider.BaseURL = upstream.URL
			srv := &server{chatgptAuth: mgr, resolveHTTPClient: testClientResolver(http.DefaultClient)}
			raw := map[string]any{
				"model": "gpt-6-luna", "stream": false,
				"input": []any{map[string]any{"type": "message", "role": "user",
					"content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
			}
			outcome := srv.forwardAttempt(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://client.test/v1/responses", nil), forwardAttemptInput{
				cfg:   &config.Config{DirectMode: true, DirectTimeoutNoStream: 5000, DirectTimeoutStreamHeader: 5000, DirectTimeoutStreamActive: 5000},
				start: time.Now(), reqID: "r-test", clientFormat: "openai-responses", originalModel: "gpt-6-luna",
				internal: converter.FromOpenAIResponses(raw), rawBody: raw,
				candidate: router.Candidate{Provider: provider, TargetModel: "gpt-6-luna"}, attemptNo: 1, httpAttemptNo: 1,
				reqLog: &metrics.RequestLog{}, detail: &metrics.AttemptDetail{},
			})
			if !outcome.requestStarted || outcome.buildErr != "" {
				t.Fatalf("应发出真实上游请求: %#v", outcome)
			}
			view, _ := mgr.ProviderInvocation("acc_ready", "codex")
			if view.State != tc.state || view.Model != "gpt-6-luna" || view.Provider != "codex" {
				t.Fatalf("调用记录 = %#v", view)
			}
			if tc.status != http.StatusOK && view.HTTPStatus != tc.status {
				t.Fatalf("失败状态码 = %#v", view)
			}
			if got := srv.chatGPTProviderStatus("codex", provider); got.Status != tc.provider {
				t.Fatalf("Provider 健康 = %#v", got)
			}
		})
	}
}
