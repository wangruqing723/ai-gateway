package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/metrics"
)

// 第四阶段：账户统计与事件经网关的归属、HTTP 边界。只使用假账户与 diagnoseUpstream 本机假上游。

// withAccountEvents 给测试网关接上与生产相同的事件流和账户事件转发。
func withAccountEvents(srv *server) *server {
	srv.eventLog = metrics.NewEventLog()
	srv.chatgptAuth.SetEventHandler(srv.onChatGPTAccountEvent)
	return srv
}

type accountMetricsResponse struct {
	Data struct {
		chatgptauth.AccountStatsView
		Events []metrics.Event `json:"events"`
	} `json:"data"`
}

func accountMetrics(t *testing.T, srv *server, id string) accountMetricsResponse {
	t.Helper()
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts/"+id+"/metrics", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("账户统计 = %d %s", recorder.Code, recorder.Body.String())
	}
	var out accountMetricsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func accountEventKinds(t *testing.T, srv *server) []string {
	t.Helper()
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts/events", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("账户事件 = %d %s", recorder.Code, recorder.Body.String())
	}
	var out struct {
		Data []metrics.Event `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(out.Data))
	for _, event := range out.Data {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestChatGPTAccountMetricsEndpointBoundaries(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	withAccountEvents(srv)
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts/"+first+"/metrics", "")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("账户统计 = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"successRate":null`, `"p95LatencyMs":null`, `"events":[]`, `"windowMinutes":15`} {
		if !strings.Contains(body, want) {
			t.Fatalf("空样本响应缺少 %s: %s", want, body)
		}
	}
	cases := []struct {
		method, path string
		status       int
		errType      string
	}{
		{http.MethodGet, "/api/chatgpt/accounts/acc_missing/metrics", http.StatusNotFound, "account_not_found"},
		{http.MethodPost, "/api/chatgpt/accounts/" + first + "/metrics", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/api/chatgpt/accounts/%2F/metrics", http.StatusBadRequest, "invalid_account_id"},
		{http.MethodPost, "/api/chatgpt/accounts/events", http.StatusMethodNotAllowed, ""},
	}
	for _, tc := range cases {
		recorder := chatGPTAPI(t, srv, tc.method, tc.path, "")
		if recorder.Code != tc.status || (tc.errType != "" && errorTypeOf(t, recorder) != tc.errType) {
			t.Errorf("%s %s = %d %s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
	}
	if kinds := accountEventKinds(t, srv); len(kinds) != 0 {
		t.Fatalf("初始账户事件 = %v", kinds)
	}
	// 统计窗口随 metrics.windowMinutes 热重载。
	raw := strings.Replace(chatGPTGatewayYAML(first, "", 5), "routes:", "metrics:\n  windowMinutes: 30\nroutes:", 1)
	if recorder := putConfig(t, srv, raw, ""); recorder.Code != http.StatusOK {
		t.Fatalf("保存配置 = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := accountMetrics(t, srv, first).Data.WindowMinutes; got != 30 {
		t.Fatalf("热重载后窗口 = %d", got)
	}
}

// 故障转移：第一个账户的 503 与第二个账户的成功分别计入各自账户，尝试明细带账户快照。
func TestChatGPTAccountMetricsAttributeFailover(t *testing.T) {
	up := newDiagnoseUpstream(t)
	succeeded := up.responses
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fake-access-a" {
			writeTestJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "fake overloaded"}})
			return
		}
		succeeded(w, r)
	})
	srv, first, second := newPauseServer(t, up, func(a, b string) string { return pausePairYAML(a, b, 2, "") })
	withAccountEvents(srv)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusOK || recorder.Header().Get("x-ai-gateway-provider") != "codex2" {
		t.Fatalf("故障转移 = %d %s", recorder.Code, recorder.Body.String())
	}
	record := lastRequestLog(t, srv)
	if len(record.AttemptDetails) != 2 || record.AttemptDetails[0].AccountID != first || record.AttemptDetails[1].AccountID != second {
		t.Fatalf("尝试明细账户 = %#v", record.AttemptDetails)
	}
	a, b := accountMetrics(t, srv, first).Data, accountMetrics(t, srv, second).Data
	if a.Window.Requests != 1 || a.Window.Failures != 1 || a.Window.LatencySamples != 1 || a.LastFailureAt == "" || a.LastSuccessAt != "" {
		t.Fatalf("失败账户统计 = %#v", a)
	}
	if b.Window.Requests != 1 || b.Window.Successes != 1 || b.Window.SuccessRate == nil || *b.Window.SuccessRate != 1 || b.LastSuccessAt == "" {
		t.Fatalf("成功账户统计 = %#v", b)
	}
	if a.Window.LocalRetries != 0 || a.Window.RateLimited != 0 {
		t.Fatalf("不应有本地重试或限流 = %#v", a.Window)
	}
	// 列表视图的最后成功时间来自账户统计，供「最近调用成功时间」排序使用。
	views := map[string]chatgptauth.AccountView{}
	for _, view := range srv.chatgptAuth.List(nil) {
		views[view.ID] = view
	}
	if views[first].Invocation.LastSuccessAt != "" || views[second].Invocation.LastSuccessAt != b.LastSuccessAt {
		t.Fatalf("列表视图最后成功时间 = %q / %q", views[first].Invocation.LastSuccessAt, views[second].Invocation.LastSuccessAt)
	}
}

// 本地重试：同一账户的两次真实调用都计入，重试单列；顶层请求日志仍只有一条。
func TestChatGPTAccountMetricsCountLocalRetries(t *testing.T) {
	up := newDiagnoseUpstream(t)
	succeeded := up.responses
	var calls atomic.Int32
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeTestJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "fake limited"}})
			return
		}
		succeeded(w, r)
	})
	srv, first, _ := newPauseServer(t, up, func(a, b string) string {
		return chatGPTGatewayYAML(a, "    localRetry:\n      maxRetries: 1\n      intervalMs: 0\n", 5)
	})
	withAccountEvents(srv)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusOK {
		t.Fatalf("本地重试 = %d %s", recorder.Code, recorder.Body.String())
	}
	if logs := srv.metrics.Logs(metrics.LogFilter{}); len(logs) != 1 {
		t.Fatalf("顶层请求日志条数 = %d", len(logs))
	}
	w := accountMetrics(t, srv, first).Data.Window
	if w.Requests != 2 || w.Successes != 1 || w.Failures != 1 || w.LocalRetries != 1 || w.RateLimited != 1 {
		t.Fatalf("本地重试统计 = %#v", w)
	}
}

// 准入跳过与授权事件：暂停、取令牌失败只计跳过，不计真实调用；事件经网关事件流可见。
func TestChatGPTAccountMetricsSkipsAndEvents(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.token, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
	})
	expired := `[
  {"access_token":"fake-access-a","refresh_token":"fake-refresh-a","account_id":"remote-a","email":"a@example.invalid","expires_at":"2000-01-01T00:00:00Z"},
  {"access_token":"fake-access-b","refresh_token":"fake-refresh-b","account_id":"remote-b","email":"b@example.invalid","expires_at":"2099-01-01T00:00:00Z"}
]`
	srv, first, _ := newPauseServerWithAccounts(t, up, expired, nil)
	withAccountEvents(srv)
	if recorder := responsesRequest(t, srv, nil); recorder.Code == http.StatusOK {
		t.Fatalf("取令牌失败不应成功: %s", recorder.Body.String())
	}
	if up.responsesCalls.Load() != 0 {
		t.Fatal("取令牌失败不应发出模型请求")
	}
	stats := accountMetrics(t, srv, first).Data
	if stats.Window.Requests != 0 || stats.Window.Skips.Authorization != 1 || stats.Window.Refresh.Failures != 1 {
		t.Fatalf("授权跳过统计 = %#v", stats.Window)
	}
	if len(stats.Events) != 1 || stats.Events[0].Kind != chatgptauth.EventReauthRequired || stats.Events[0].Account != "a@example.invalid" {
		t.Fatalf("账户事件 = %#v", stats.Events)
	}

	pauseAccount(t, srv, first, true)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("暂停 = %d %s", recorder.Code, recorder.Body.String())
	}
	if record := lastRequestLog(t, srv); len(record.AttemptDetails) != 1 || record.AttemptDetails[0].AccountID != first {
		t.Fatalf("暂停跳过明细 = %#v", record.AttemptDetails)
	}
	stats = accountMetrics(t, srv, first).Data
	if stats.Window.Requests != 0 || stats.Window.Skips.AccountPaused != 1 || stats.Window.Skips.Authorization != 1 {
		t.Fatalf("暂停跳过统计 = %#v", stats.Window.Skips)
	}
	if kinds := accountEventKinds(t, srv); strings.Join(kinds, ",") != "account_paused,account_reauth_required" {
		t.Fatalf("账户事件流 = %v", kinds)
	}
	recorder := httptestGet(t, srv, "/api/metrics")
	if !strings.Contains(recorder, `"accountIds":["`+first+`"]`) || strings.Contains(recorder, "fake-refresh") || strings.Contains(recorder, "fake-access") {
		t.Fatalf("运行事件应带账户维度且不含凭据: %s", recorder)
	}
}

// 诊断调用只计入诊断计数，不进入真实调用、成功率与延迟。
func TestChatGPTAccountMetricsKeepDiagnosticsSeparate(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newDiagnoseServer(t, up, "")
	withAccountEvents(srv)
	decodeDiagnose(t, diagnoseRequest(t, srv, first, inferenceBody("fake-diagnose-model"), nil))
	w := accountMetrics(t, srv, first).Data
	if w.Window.Requests != 0 || w.Window.SuccessRate != nil || w.Window.LatencySamples != 0 || w.Window.Diagnostics.Successes != 1 || w.LastSuccessAt != "" {
		t.Fatalf("诊断统计 = %#v", w)
	}
	// 诊断调用写回「最近实际调用」，但不提供统计口径的最后成功时间。
	for _, view := range srv.chatgptAuth.List(nil) {
		if view.ID == first && (view.Invocation.State != "success" || view.Invocation.LastSuccessAt != "") {
			t.Fatalf("诊断后列表视图 = %#v", view.Invocation)
		}
	}
}

// 绑定变化：配置生效后每个 Provider 一条事件；未变化的保存不产生事件。
func TestChatGPTBindingEventsPerProvider(t *testing.T) {
	srv, first, second := newChatGPTGatewayServer(t, "")
	withAccountEvents(srv)
	if recorder := putConfig(t, srv, chatGPTGatewayYAML(second, "", 5), ""); recorder.Code != http.StatusOK {
		t.Fatalf("改绑 = %d %s", recorder.Code, recorder.Body.String())
	}
	events := srv.eventLog.AccountEvents("", 0)
	if len(events) != 1 || events[0].Kind != "account_binding_changed" || events[0].Provider != "codex" ||
		strings.Join(events[0].AccountIDs, ",") != first+","+second || events[0].Account != "b@example.invalid" ||
		!strings.Contains(events[0].Detail, "a@example.invalid") {
		t.Fatalf("改绑事件 = %#v", events)
	}
	// 只改普通 Provider 的并发：没有绑定变化，不产生事件。
	if recorder := putConfig(t, srv, chatGPTGatewayYAML(second, "", 7), ""); recorder.Code != http.StatusOK {
		t.Fatalf("普通保存 = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatGPTAPI(t, srv, http.MethodDelete, "/api/chatgpt/accounts/"+second+"/bindings/codex", ""); recorder.Code != http.StatusOK {
		t.Fatalf("解绑 = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatGPTAPI(t, srv, http.MethodPut, "/api/chatgpt/accounts/"+first+"/bindings/codex", ""); recorder.Code != http.StatusOK {
		t.Fatalf("绑定 = %d %s", recorder.Code, recorder.Body.String())
	}
	events = srv.eventLog.AccountEvents("", 0)
	if len(events) != 3 || !strings.Contains(events[1].Detail, "解除绑定") || !strings.Contains(events[0].Detail, "已绑定") {
		t.Fatalf("绑定事件序列 = %#v", events)
	}
	if got := accountMetrics(t, srv, first).Data.Events; len(got) != 2 {
		t.Fatalf("账户 %s 的事件 = %#v", first, got)
	}
}

// 请求途中换绑：在途尝试仍归属请求开始时的账户，换绑只影响之后的请求。
func TestChatGPTAccountMetricsKeepAttributionAcrossRebind(t *testing.T) {
	up := newDiagnoseUpstream(t)
	succeeded := up.responses
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		succeeded(w, r)
	})
	srv, first, second := newPauseServer(t, up, nil)
	withAccountEvents(srv)
	done := make(chan int, 1)
	go func() { done <- responsesRequest(t, srv, nil).Code }()
	<-entered
	if recorder := putConfig(t, srv, chatGPTGatewayYAML(second, "", 5), ""); recorder.Code != http.StatusOK {
		close(release)
		t.Fatalf("在途换绑 = %d %s", recorder.Code, recorder.Body.String())
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("在途请求 = %d", code)
	}
	if record := lastRequestLog(t, srv); len(record.AttemptDetails) != 1 || record.AttemptDetails[0].AccountID != first {
		t.Fatalf("在途请求明细 = %#v", record.AttemptDetails)
	}
	if a := accountMetrics(t, srv, first).Data.Window; a.Requests != 1 || a.Successes != 1 {
		t.Fatalf("原账户统计 = %#v", a)
	}
	if b := accountMetrics(t, srv, second).Data.Window; b.Requests != 0 {
		t.Fatalf("新账户不应继承在途请求 = %#v", b)
	}
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusOK {
		t.Fatalf("换绑后请求 = %d", recorder.Code)
	}
	if b := accountMetrics(t, srv, second).Data.Window; b.Requests != 1 {
		t.Fatalf("换绑后新请求应计入新账户 = %#v", b)
	}
}

func httptestGet(t *testing.T, srv *server, path string) string {
	t.Helper()
	recorder := chatGPTAPI(t, srv, http.MethodGet, path, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}
