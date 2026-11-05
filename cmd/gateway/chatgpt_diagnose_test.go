package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/metrics"
)

// 诊断测试只使用假账户与本机假上游：rewriteTransport 把 auth.openai.com 与 chatgpt.com
// 改写到 httptest 服务器，其他主机一律拒绝，保证不会访问真实 OAuth 或 ChatGPT。

type diagnoseUpstream struct {
	server         *httptest.Server
	mu             sync.Mutex
	token          http.HandlerFunc
	usage          http.HandlerFunc
	responses      http.HandlerFunc
	tokenCalls     atomic.Int32
	usageCalls     atomic.Int32
	responsesCalls atomic.Int32
	lastResponses  map[string]any
	lastAuth       string
}

func newDiagnoseUpstream(t *testing.T) *diagnoseUpstream {
	t.Helper()
	up := &diagnoseUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		token, usage, responses := up.token, up.usage, up.responses
		up.mu.Unlock()
		switch r.URL.Path {
		case "/oauth/token":
			up.tokenCalls.Add(1)
			token(w, r)
		case "/backend-api/wham/usage":
			up.usageCalls.Add(1)
			usage(w, r)
		case "/backend-api/codex/responses":
			up.responsesCalls.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			up.mu.Lock()
			up.lastResponses, up.lastAuth = body, r.Header.Get("Authorization")
			up.mu.Unlock()
			responses(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.server.Close)
	up.token = func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{"access_token": "rotated-access-a", "refresh_token": "rotated-refresh-a", "expires_in": 3600})
	}
	up.usage = func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{"plan_type": "plus", "rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": 100, "limit_window_seconds": 18000, "reset_at": 1893600000},
		}})
	}
	up.responses = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"secret-model-output\"}]}]}}\n\n")
	}
	return up
}

func (up *diagnoseUpstream) set(target *http.HandlerFunc, handler http.HandlerFunc) {
	up.mu.Lock()
	defer up.mu.Unlock()
	*target = handler
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "auth.openai.com" && r.URL.Host != "chatgpt.com" {
		return blockedTransport{}.RoundTrip(r)
	}
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// newDiagnoseServer 返回绑定 remote-a 账户的 codex Provider 网关；管理客户端与 Provider 客户端都指向假上游。
func newDiagnoseServer(t *testing.T, up *diagnoseUpstream, codexExtra string) (*server, string, string) {
	t.Helper()
	target, err := url.Parse(up.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rewriteTransport{target: target}}
	return newDiagnoseServerWithClients(t, codexExtra, client, testClientResolver(client))
}

// newDiagnoseServerWithClients 分别注入管理客户端与 Provider 客户端解析器，用于验证两类出网路径互不混用。
func newDiagnoseServerWithClients(t *testing.T, codexExtra string, client *http.Client, resolve func(string) (*http.Client, error)) (*server, string, string) {
	t.Helper()
	mgr, err := chatgptauth.New(filepath.Join(t.TempDir(), "fake-accounts.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Import([]byte(`[
  {"access_token":"fake-access-a","refresh_token":"fake-refresh-a","account_id":"remote-a","email":"a@example.invalid","expires_at":"2099-01-01T00:00:00Z"},
  {"access_token":"fake-access-b","account_id":"remote-b","email":"b@example.invalid","expires_at":"2099-01-01T00:00:00Z"}
]`), false); err != nil {
		t.Fatal(err)
	}
	views := mgr.List(nil)
	srv := newConfigTestServer(t, chatGPTGatewayYAML(views[0].ID, codexExtra, 5))
	srv.chatgptAuth = mgr
	srv.resolveHTTPClient = resolve
	return srv, views[0].ID, views[1].ID
}

func diagnoseRequest(t *testing.T, srv *server, id string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	data, raw := body.([]byte)
	if !raw {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/chatgpt/accounts/"+id+"/diagnose", bytes.NewReader(data))
	request.Header.Set("Origin", "http://127.0.0.1:7789")
	request.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	srv.handle(recorder, request)
	return recorder
}

type diagnoseResponse struct {
	Data struct {
		AccountID      string                   `json:"accountId"`
		TokenExchanged bool                     `json:"tokenExchanged"`
		ActualCall     bool                     `json:"actualCall"`
		BudgetMs       int64                    `json:"budgetMs"`
		Steps          []chatGPTDiagnoseStep    `json:"steps"`
		Account        *chatgptauth.AccountView `json:"account"`
	} `json:"data"`
}

func decodeDiagnose(t *testing.T, recorder *httptest.ResponseRecorder) diagnoseResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("诊断 = %d %s", recorder.Code, recorder.Body.String())
	}
	var out diagnoseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Steps) != 4 {
		t.Fatalf("步骤数 = %d", len(out.Data.Steps))
	}
	for index, name := range chatGPTDiagnoseStepOrder {
		if out.Data.Steps[index].Name != name {
			t.Fatalf("步骤顺序 = %#v", out.Data.Steps)
		}
	}
	return out
}

func stepOf(out diagnoseResponse, name string) chatGPTDiagnoseStep {
	for _, step := range out.Data.Steps {
		if step.Name == name {
			return step
		}
	}
	return chatGPTDiagnoseStep{}
}

func inferenceBody(model string) map[string]any {
	return map[string]any{"checks": []string{"inference"}, "provider": "codex", "model": model, "confirm": chatGPTDiagnoseConfirm}
}

// assertNoSecrets 确认响应不含令牌、刷新凭据、代理口令或模型输出。
func assertNoSecrets(t *testing.T, body string, extra ...string) {
	t.Helper()
	for _, secret := range append([]string{"fake-access", "fake-refresh", "rotated-access", "rotated-refresh", "secret-model-output"}, extra...) {
		if strings.Contains(body, secret) {
			t.Fatalf("诊断响应泄露 %q: %s", secret, body)
		}
	}
}
func TestChatGPTDiagnoseRejectsInvalidRequestsWithoutNetwork(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newDiagnoseServer(t, up, "")
	srv.cfgMu.Lock()
	srv.cfg.Providers["other"] = oauthProvider("other", second)
	srv.cfgMu.Unlock()
	withMethod := func(method string) func(*http.Request) { return func(r *http.Request) { r.Method = method } }
	cases := []struct {
		name     string
		id       string
		body     any
		mutate   func(*http.Request)
		status   int
		errorTyp string
	}{
		{"GET 方法", first, map[string]any{}, withMethod(http.MethodGet), http.StatusMethodNotAllowed, ""},
		{"缺少 Origin", first, map[string]any{}, func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden, ""},
		{"跨站 Origin", first, map[string]any{}, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden, ""},
		{"非 JSON", first, map[string]any{}, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, ""},
		{"账户不存在", "acc_missing", map[string]any{}, nil, http.StatusNotFound, "account_not_found"},
		{"未知步骤", first, map[string]any{"checks": []string{"probe"}}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"重复步骤", first, map[string]any{"checks": []string{"usage", "usage"}}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"checks 为 null", first, []byte(`{"checks":null}`), nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"未知字段", first, []byte(`{"forceRefresh":true}`), nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"多个 JSON 值", first, []byte(`{}{}`), nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"refreshMode 非法", first, map[string]any{"checks": []string{"refresh"}, "refreshMode": "always"}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"未选刷新却带 refreshMode", first, map[string]any{"refreshMode": "force"}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"未选实际调用却带模型", first, map[string]any{"checks": []string{"usage"}, "model": "gpt-6-luna"}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"实际调用缺少确认", first, map[string]any{"checks": []string{"inference"}, "provider": "codex", "model": "gpt-6-luna"}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"实际调用确认值错误", first, map[string]any{"checks": []string{"inference"}, "provider": "codex", "model": "gpt-6-luna", "confirm": "yes"}, nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"实际调用缺少模型", first, inferenceBody(" "), nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"模型名含空白", first, inferenceBody("gpt 6"), nil, http.StatusBadRequest, "invalid_diagnose_request"},
		{"Provider 不存在", first, map[string]any{"checks": []string{"inference"}, "provider": "ghost", "model": "m", "confirm": chatGPTDiagnoseConfirm}, nil, http.StatusBadRequest, "invalid_diagnose_target"},
		{"普通 Provider", first, map[string]any{"checks": []string{"inference"}, "provider": "primary", "model": "m", "confirm": chatGPTDiagnoseConfirm}, nil, http.StatusBadRequest, "invalid_diagnose_target"},
		{"Provider 绑定其他账户", first, map[string]any{"checks": []string{"inference"}, "provider": "other", "model": "m", "confirm": chatGPTDiagnoseConfirm}, nil, http.StatusBadRequest, "invalid_diagnose_target"},
		{"请求体超限", first, []byte(`{"model":"` + strings.Repeat("m", maxChatGPTDiagnoseRequestBytes) + `"}`), nil, http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := diagnoseRequest(t, srv, tc.id, tc.body, tc.mutate)
			if recorder.Code != tc.status {
				t.Fatalf("状态 = %d，期望 %d: %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if tc.errorTyp != "" && errorTypeOf(t, recorder) != tc.errorTyp {
				t.Fatalf("错误类型 = %q，期望 %q", errorTypeOf(t, recorder), tc.errorTyp)
			}
		})
	}
	if calls := up.tokenCalls.Load() + up.usageCalls.Load() + up.responsesCalls.Load(); calls != 0 {
		t.Fatalf("无效请求不应出网，实际 %d 次", calls)
	}
}

func TestChatGPTDiagnoseDefaultsToLocalOnly(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newDiagnoseServer(t, up, "")
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{}, nil))
	local := stepOf(out, "local")
	if local.Status != "success" || !strings.Contains(strings.Join(local.Details, "\n"), "绑定 Provider codex：已启用") {
		t.Fatalf("本地检查 = %#v", local)
	}
	for _, name := range []string{"refresh", "usage", "inference"} {
		if step := stepOf(out, name); step.Status != "skipped" || step.Message != "未选择此步骤" {
			t.Fatalf("%s = %#v", name, step)
		}
	}
	if out.Data.TokenExchanged || out.Data.ActualCall || out.Data.Account == nil || out.Data.Account.ID != first {
		t.Fatalf("汇总 = %#v", out.Data)
	}
	if calls := up.tokenCalls.Load() + up.usageCalls.Load() + up.responsesCalls.Load(); calls != 0 {
		t.Fatalf("默认诊断不应出网，实际 %d 次", calls)
	}
	// 没有刷新凭据的账户：本地检查成功但提示到期后需要重新授权；未绑定 Provider 时说明不会接到流量。
	other := stepOf(decodeDiagnose(t, diagnoseRequest(t, srv, second, map[string]any{"checks": []string{"local"}}, nil)), "local")
	if other.Status != "success" || !strings.Contains(other.Message, "没有刷新凭据") || !strings.Contains(strings.Join(other.Details, "\n"), "未绑定任何 Provider") {
		t.Fatalf("无刷新凭据账户 = %#v", other)
	}
}
func TestChatGPTDiagnoseFullFlowUsesRotatedCredentialsWithoutMetrics(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newDiagnoseServer(t, up, "    maxTokens: 8192\n")
	body := inferenceBody("fake-diagnose-model")
	body["checks"], body["refreshMode"] = []string{"inference", "usage", "refresh"}, "force"
	recorder := diagnoseRequest(t, srv, first, body, nil)
	out := decodeDiagnose(t, recorder)
	for _, name := range chatGPTDiagnoseStepOrder {
		if step := stepOf(out, name); step.Status != "success" {
			t.Fatalf("%s = %#v", name, step)
		}
	}
	if !out.Data.TokenExchanged || !out.Data.ActualCall || !stepOf(out, "refresh").TokenExchanged {
		t.Fatalf("交换和调用标记 = %#v", out.Data)
	}
	if up.tokenCalls.Load() != 1 || up.usageCalls.Load() != 1 || up.responsesCalls.Load() != 1 {
		t.Fatalf("上游次数 = %d/%d/%d", up.tokenCalls.Load(), up.usageCalls.Load(), up.responsesCalls.Load())
	}
	up.mu.Lock()
	requestBody, auth := up.lastResponses, up.lastAuth
	up.mu.Unlock()
	if auth != "Bearer rotated-access-a" || requestBody["stream"] != true || requestBody["store"] != false || requestBody["max_output_tokens"] != float64(chatGPTDiagnoseMaxOutputTokens) {
		t.Fatalf("实际调用未使用新凭据或预算 = %#v", requestBody)
	}
	if requestBody["model"] != "fake-diagnose-model" || !strings.Contains(stepOf(out, "usage").Message, "额度已用尽") {
		t.Fatalf("模型或用量 = %#v / %#v", requestBody, stepOf(out, "usage"))
	}
	if logs := srv.metrics.Logs(metrics.LogFilter{}); len(logs) != 0 {
		t.Fatalf("诊断混入请求日志 = %#v", logs)
	}
	if account := out.Data.Account; account == nil || account.Invocation.State != "success" || account.Invocation.Provider != "codex" || account.Invocation.Model != "fake-diagnose-model" {
		t.Fatalf("实际调用未记到账户 = %#v", account)
	}
	assertNoSecrets(t, recorder.Body.String())
}

func TestChatGPTDiagnoseAutoRefreshDoesNotExchangeValidToken(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newDiagnoseServer(t, up, "")
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{"checks": []string{"refresh"}, "refreshMode": "auto"}, nil))
	if step := stepOf(out, "refresh"); step.Status != "success" || step.TokenExchanged || !strings.Contains(step.Message, "未交换令牌") {
		t.Fatalf("自动刷新 = %#v", step)
	}
	without := decodeDiagnose(t, diagnoseRequest(t, srv, second, map[string]any{"checks": []string{"refresh"}, "refreshMode": "force"}, nil))
	if step := stepOf(without, "refresh"); step.Status != "failure" || step.Category != "no_refresh_token" {
		t.Fatalf("无刷新凭据 = %#v", step)
	}
	if up.tokenCalls.Load() != 0 {
		t.Fatal("有效令牌或缺少刷新凭据不应交换")
	}
}

func TestChatGPTDiagnoseInvalidGrantBlocksDependentSteps(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.token, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "secret-upstream-description"})
	})
	srv, first, _ := newDiagnoseServer(t, up, "")
	body := inferenceBody("fake-model")
	body["checks"], body["refreshMode"] = []string{"refresh", "usage", "inference"}, "force"
	recorder := diagnoseRequest(t, srv, first, body, nil)
	out := decodeDiagnose(t, recorder)
	if step := stepOf(out, "refresh"); step.Status != "failure" || step.Category != "authorization" || step.HTTPStatus != 400 {
		t.Fatalf("刷新错误 = %#v", step)
	}
	for _, name := range []string{"usage", "inference"} {
		if stepOf(out, name).Status != "skipped" {
			t.Fatalf("%s 未跳过", name)
		}
	}
	if up.tokenCalls.Load() != 1 || up.usageCalls.Load() != 0 || up.responsesCalls.Load() != 0 {
		t.Fatal("刷新失败后继续出网")
	}
	if out.Data.Account.Authorization.State != "reauthorization_required" {
		t.Fatal("未记录重新授权要求")
	}
	assertNoSecrets(t, recorder.Body.String(), "secret-upstream-description")
	// 下一次只做本地检查即可发现失效，不重复向 OAuth 服务交换。
	second := decodeDiagnose(t, diagnoseRequest(t, srv, first, body, nil))
	if stepOf(second, "local").Category != "authorization" || stepOf(second, "refresh").Status != "skipped" || up.tokenCalls.Load() != 1 {
		t.Fatalf("本地授权阻断 = %#v", second.Data)
	}
}

func TestChatGPTDiagnoseUsageFailureDoesNotBlockInference(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, 401, map[string]any{"error": "secret-usage-error"})
	})
	srv, first, _ := newDiagnoseServer(t, up, "")
	body := inferenceBody("fake-model")
	body["checks"] = []string{"usage", "inference"}
	recorder := diagnoseRequest(t, srv, first, body, nil)
	out := decodeDiagnose(t, recorder)
	if step := stepOf(out, "usage"); step.Status != "failure" || step.Category != "query_failed" || step.HTTPStatus != 401 || !strings.Contains(step.Message, "不单独证明") {
		t.Fatalf("用量错误 = %#v", step)
	}
	if stepOf(out, "inference").Status != "success" || out.Data.Account.Authorization.State != "valid" {
		t.Fatal("用量错误错误地阻断了实际调用或授权")
	}
	assertNoSecrets(t, recorder.Body.String(), "secret-usage-error")
}

func TestChatGPTDiagnoseInferenceFailureScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		category string
	}{
		{"限流", 429, "quota"}, {"模型参数", 400, "model"}, {"上游故障", 503, "upstream"}, {"权限", 401, "authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newDiagnoseUpstream(t)
			up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
				writeTestJSON(w, tc.status, map[string]any{"error": map[string]any{"message": "secret-response-error"}})
			})
			srv, first, _ := newDiagnoseServer(t, up, "")
			recorder := diagnoseRequest(t, srv, first, inferenceBody("fake-model"), nil)
			out := decodeDiagnose(t, recorder)
			if step := stepOf(out, "inference"); step.Status != "failure" || step.Category != tc.category || step.HTTPStatus != tc.status {
				t.Fatalf("调用结论 = %#v", step)
			}
			if !out.Data.ActualCall || up.responsesCalls.Load() != 1 {
				t.Fatal("实际调用次数错误")
			}
			if out.Data.Account.Authorization.State != "valid" {
				t.Fatal("推理权限错误不能证明刷新凭据失效")
			}
			assertNoSecrets(t, recorder.Body.String(), "secret-response-error")
		})
	}
}

func TestChatGPTDiagnoseSkipsDisabledProvider(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newDiagnoseServer(t, up, "    enabled: false\n")
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, inferenceBody("fake-model"), nil))
	if step := stepOf(out, "inference"); step.Status != "skipped" || !strings.Contains(step.Message, "已停用") {
		t.Fatalf("停用结论 = %#v", step)
	}
	if out.Data.ActualCall || up.responsesCalls.Load() != 0 {
		t.Fatal("诊断绕过提供商停用设置")
	}
}

func TestChatGPTDiagnoseConcurrentGuardIsReleased(t *testing.T) {
	up := newDiagnoseUpstream(t)
	started, release := make(chan struct{}), make(chan struct{})
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			writeTestJSON(w, 200, map[string]any{})
		case <-r.Context().Done():
		}
	})
	srv, first, _ := newDiagnoseServer(t, up, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- diagnoseRequest(t, srv, first, map[string]any{"checks": []string{"usage"}}, func(r *http.Request) { *r = *r.WithContext(ctx) })
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("用量请求没有开始")
	}
	conflict := diagnoseRequest(t, srv, first, map[string]any{}, nil)
	if conflict.Code != 409 || errorTypeOf(t, conflict) != "diagnose_in_progress" {
		t.Fatalf("并发诊断 = %d %s", conflict.Code, conflict.Body.String())
	}
	close(release)
	select {
	case recorder := <-done:
		decodeDiagnose(t, recorder)
	case <-time.After(3 * time.Second):
		t.Fatal("首个诊断未完成")
	}
	if _, running := srv.chatGPTDiagnosing.Load(first); running {
		t.Fatal("诊断守卫未释放")
	}
	decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{}, nil))
}

func TestChatGPTDiagnoseCancellationPreservesUsageSnapshot(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newDiagnoseServer(t, up, "")
	decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{"checks": []string{"usage"}}, nil))
	before, _ := srv.chatgptAuth.Account(first)
	beforeJSON, _ := json.Marshal(before.Usage)
	started := make(chan struct{})
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- diagnoseRequest(t, srv, first, map[string]any{"checks": []string{"usage"}}, func(r *http.Request) { *r = *r.WithContext(ctx) })
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("用量请求没有开始")
	}
	cancel()
	select {
	case recorder := <-done:
		if recorder.Body.Len() != 0 {
			t.Fatal("取消后仍写响应")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消未中止请求")
	}
	after, _ := srv.chatgptAuth.Account(first)
	afterJSON, _ := json.Marshal(after.Usage)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("取消改写快照: %s / %s", beforeJSON, afterJSON)
	}
	if _, running := srv.chatGPTDiagnosing.Load(first); running {
		t.Fatal("取消后守卫未释放")
	}
}

func TestChatGPTDiagnoseBudgetCancelsRemainingSteps(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	srv, first, _ := newDiagnoseServer(t, up, "")
	previous := chatGPTDiagnoseBudget
	chatGPTDiagnoseBudget = 100 * time.Millisecond
	t.Cleanup(func() { chatGPTDiagnoseBudget = previous })
	body := inferenceBody("fake-model")
	body["checks"] = []string{"usage", "inference"}
	started := time.Now()
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, body, nil))
	if time.Since(started) > 3*time.Second || out.Data.BudgetMs != 100 {
		t.Fatal("总预算未生效")
	}
	for _, name := range []string{"usage", "inference"} {
		if step := stepOf(out, name); step.Status != "cancelled" || !strings.Contains(step.Message, "超过总时长") {
			t.Fatalf("%s = %#v", name, step)
		}
	}
	if up.usageCalls.Load() != 1 || up.responsesCalls.Load() != 0 || out.Data.Account.Usage != nil {
		t.Fatal("预算取消继续出网或改写了账户状态")
	}
}

func TestChatGPTDiagnoseProxySourcesDoNotExposeCredentials(t *testing.T) {
	t.Setenv("AI_GATEWAY_OAUTH_PROXY", "http://oauth-user:secret-oauth-proxy@proxy.invalid:8080")
	up := newDiagnoseUpstream(t)
	srv, first, _ := newDiagnoseServer(t, up, "    proxy: \"http://provider-user:secret-provider-proxy@proxy.invalid:8080\"\n")
	body := inferenceBody("fake-model")
	body["checks"] = []string{"refresh", "usage", "inference"}
	recorder := diagnoseRequest(t, srv, first, body, nil)
	out := decodeDiagnose(t, recorder)
	for _, name := range []string{"refresh", "usage"} {
		if stepOf(out, name).ProxySource != "oauth_proxy" {
			t.Fatalf("%s 代理来源错误", name)
		}
	}
	if stepOf(out, "inference").ProxySource != "provider_proxy" {
		t.Fatal("实际调用未显示提供商代理来源")
	}
	assertNoSecrets(t, recorder.Body.String(), "secret-oauth-proxy", "secret-provider-proxy", "proxy.invalid")
}

func TestChatGPTDiagnoseNeverLogsOrCapturesUpstreamPayload(t *testing.T) {
	dir := t.TempDir()
	bodyFile, streamFile := filepath.Join(dir, "body.jsonl"), filepath.Join(dir, "stream.jsonl")
	t.Setenv("AI_GATEWAY_DEBUG_UPSTREAM_BODY", bodyFile)
	t.Setenv("AI_GATEWAY_DEBUG_UPSTREAM_STREAM", streamFile)
	up := newDiagnoseUpstream(t)
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, 400, map[string]any{"error": map[string]any{"message": "secret-diagnostic-upstream-body"}})
	})
	srv, first, _ := newDiagnoseServer(t, up, "")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = original; _ = writer.Close(); _ = reader.Close() })
	recorder := diagnoseRequest(t, srv, first, inferenceBody("fake-model"), nil)
	os.Stderr = original
	_ = writer.Close()
	logged, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	decodeDiagnose(t, recorder)
	assertNoSecrets(t, string(logged), "secret-diagnostic-upstream-body")
	if !strings.Contains(string(logged), "账户诊断结束") {
		t.Fatal("缺少安全的诊断汇总日志")
	}
	for _, path := range []string{bodyFile, streamFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("诊断原文被抓包或检查失败: %s %v", path, err)
		}
	}
}

// pathRecorder 记录经过某个客户端的请求路径，用来区分管理请求与 Provider 请求。
type pathRecorder struct {
	inner http.RoundTripper
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	return p.inner.RoundTrip(r)
}

func (p *pathRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func TestChatGPTDiagnoseRoutesManagementAndProviderTraffic(t *testing.T) {
	up := newDiagnoseUpstream(t)
	// 强制刷新得到 60 秒有效期，低于请求期刷新阈值，使后续实际调用必须再刷新一次。
	up.set(&up.token, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{"access_token": "rotated-access-a", "refresh_token": "rotated-refresh-a", "expires_in": 60})
	})
	target, err := url.Parse(up.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	management := &pathRecorder{inner: rewriteTransport{target: target}}
	provider := &pathRecorder{inner: rewriteTransport{target: target}}
	var proxiesMu sync.Mutex
	var proxies []string
	resolve := func(proxyURL string) (*http.Client, error) {
		proxiesMu.Lock()
		proxies = append(proxies, proxyURL)
		proxiesMu.Unlock()
		return &http.Client{Transport: provider}, nil
	}
	srv, first, _ := newDiagnoseServerWithClients(t, "    proxy: \"http://provider.invalid:8080\"\n", &http.Client{Transport: management}, resolve)
	decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{"checks": []string{"refresh"}, "refreshMode": "force"}, nil))
	if got := management.seen(); len(got) != 1 || got[0] != "/oauth/token" || len(provider.seen()) != 0 {
		t.Fatalf("刷新链路应只走管理客户端: 管理 %v，Provider %v", got, provider.seen())
	}
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, inferenceBody("fake-model"), nil))
	if stepOf(out, "inference").Status != "success" {
		t.Fatalf("实际调用 = %#v", stepOf(out, "inference"))
	}
	if got := provider.seen(); len(got) != 2 || got[0] != "/oauth/token" || got[1] != "/backend-api/codex/responses" {
		t.Fatalf("实际调用的请求期刷新和模型请求都应走 Provider 客户端: %v", got)
	}
	if len(management.seen()) != 1 {
		t.Fatalf("实际调用不应经过管理客户端: %v", management.seen())
	}
	proxiesMu.Lock()
	defer proxiesMu.Unlock()
	if len(proxies) == 0 {
		t.Fatal("实际调用没有按 Provider 代理解析客户端")
	}
	for _, proxyURL := range proxies {
		if proxyURL != "http://provider.invalid:8080" {
			t.Fatalf("实际调用使用了错误的代理设置: %q", proxyURL)
		}
	}
}
