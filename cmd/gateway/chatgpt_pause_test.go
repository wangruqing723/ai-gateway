package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/breaker"
	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
)

// 第三阶段请求准入测试（D01）：只使用假账户与 diagnoseUpstream 本机假上游。

// pausePairYAML 生成两个 OAuth Provider（codex、codex2）组成的 failover 路由。
func pausePairYAML(firstID, secondID string, maxAttempts int, codexExtra string) string {
	return fmt.Sprintf(`host: "127.0.0.1"
port: 7789
failover:
  enabled: true
  maxAttempts: %d
providers:
  codex:
    baseUrl: "https://chatgpt.com/backend-api/codex"
    format: openai-responses
    authType: chatgptOAuth
    accountId: %q
%s    maxConcurrent: 1
    maxPerSecond: 0
    maxQueueWait: 30000
  codex2:
    baseUrl: "https://chatgpt.com/backend-api/codex"
    format: openai-responses
    authType: chatgptOAuth
    accountId: %q
    maxConcurrent: 1
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "gpt-*"
    targets:
      - provider: codex
        model: gpt-6-luna
      - provider: codex2
        model: gpt-6-luna
`, maxAttempts, firstID, codexExtra, secondID)
}

// pauseAccountsJSON 是两个长期有效、可刷新的假账户。
const pauseAccountsJSON = `[
  {"access_token":"fake-access-a","refresh_token":"fake-refresh-a","account_id":"remote-a","email":"a@example.invalid","expires_at":"2099-01-01T00:00:00Z"},
  {"access_token":"fake-access-b","refresh_token":"fake-refresh-b","account_id":"remote-b","email":"b@example.invalid","expires_at":"2099-01-01T00:00:00Z"}
]`

// newPauseServer 用两个可刷新的假账户和改写到假上游的客户端创建网关；yaml 为 nil 时使用单 Provider 配置。
func newPauseServer(t *testing.T, up *diagnoseUpstream, yaml func(first, second string) string) (*server, string, string) {
	t.Helper()
	return newPauseServerWithAccounts(t, up, pauseAccountsJSON, yaml)
}

func newPauseServerWithAccounts(t *testing.T, up *diagnoseUpstream, accounts string, yaml func(first, second string) string) (*server, string, string) {
	t.Helper()
	target, err := url.Parse(up.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rewriteTransport{target: target}}
	mgr, err := chatgptauth.New(filepath.Join(t.TempDir(), "fake-accounts.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Import([]byte(accounts), false); err != nil {
		t.Fatal(err)
	}
	views := mgr.List(nil)
	first, second := views[0].ID, views[1].ID
	raw := chatGPTGatewayYAML(first, "", 5)
	if yaml != nil {
		raw = yaml(first, second)
	}
	srv := newConfigTestServer(t, raw)
	srv.chatgptAuth = mgr
	srv.resolveHTTPClient = testClientResolver(client)
	srv.breaker = breaker.New(breaker.Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 60000})
	return srv, first, second
}

func pauseAccount(t *testing.T, srv *server, id string, paused bool) {
	t.Helper()
	if _, _, _, err := srv.chatgptAuth.SetPaused(id, paused, srv.chatgptAuth.Version()); err != nil {
		t.Fatal(err)
	}
}

// responsesRequest 发起一次经网关的非流式 Responses 请求。
func responsesRequest(t *testing.T, srv *server, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"gpt-6-luna","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		request = request.WithContext(ctx)
	}
	recorder := httptest.NewRecorder()
	srv.handle(recorder, request)
	return recorder
}

func lastRequestLog(t *testing.T, srv *server) metrics.RequestLog {
	t.Helper()
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 1})
	if len(logs) == 0 {
		t.Fatal("没有请求日志")
	}
	return logs[0]
}

func attemptKinds(record metrics.RequestLog) []string {
	kinds := make([]string, 0, len(record.AttemptDetails))
	for _, detail := range record.AttemptDetails {
		kinds = append(kinds, detail.Provider+":"+detail.Kind)
	}
	return kinds
}

func TestPausedAccountSkipsCandidateWithoutNetwork(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newPauseServer(t, up, nil)
	pauseAccount(t, srv, first, true)
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "all_candidates_paused" || recorder.Header().Get("retry-after") != "" {
		t.Fatalf("全部暂停 = %d %s retry-after=%q", recorder.Code, recorder.Body.String(), recorder.Header().Get("retry-after"))
	}
	if up.tokenCalls.Load() != 0 || up.responsesCalls.Load() != 0 {
		t.Fatal("暂停账户不应出网")
	}
	if _, touched := srv.breaker.Snapshot()["codex"]; touched {
		t.Fatal("前置暂停跳过不能调用 breaker.Allow 或 Report")
	}
	record := lastRequestLog(t, srv)
	if record.Attempts != 0 || len(record.AttemptDetails) != 1 || record.AttemptDetails[0].Kind != "account_paused" ||
		record.AttemptDetails[0].Reason != "account_paused" || record.AttemptDetails[0].Outcome != "skipped" {
		t.Fatalf("请求日志 = attempts %d %v", record.Attempts, attemptKinds(record))
	}
	if status := srv.chatGPTProviderStatus("codex", ptrProvider(codexProvider(srv))); status.Status != "unchecked" || !strings.Contains(status.Message, "暂停") {
		t.Fatalf("Provider 健康 = %#v", status)
	}
	out := decodeDiagnose(t, diagnoseRequest(t, srv, first, inferenceBody("fake-model"), nil))
	if step := stepOf(out, "inference"); step.Status != "skipped" || !strings.Contains(step.Message, "暂停") || out.Data.ActualCall {
		t.Fatalf("诊断实际调用应跳过: %#v", step)
	}
	if local := stepOf(decodeDiagnose(t, diagnoseRequest(t, srv, first, map[string]any{}, nil)), "local"); !strings.Contains(strings.Join(local.Details, "\n"), "账户已暂停") {
		t.Fatalf("本地检查应说明暂停: %#v", local.Details)
	}

	pauseAccount(t, srv, first, false)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusOK || up.responsesCalls.Load() != 1 {
		t.Fatalf("恢复后 = %d %s", recorder.Code, recorder.Body.String())
	}
}

func ptrProvider(p config.Provider) *config.Provider { return &p }

func TestPausedCandidateDoesNotConsumeAttempts(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newPauseServer(t, up, func(a, b string) string { return pausePairYAML(a, b, 1, "") })
	pauseAccount(t, srv, first, true)
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusOK || recorder.Header().Get("x-ai-gateway-provider") != "codex2" || recorder.Header().Get("x-ai-gateway-attempts") != "1" {
		t.Fatalf("暂停候选不应占用 maxAttempts: %d provider=%q attempts=%q", recorder.Code, recorder.Header().Get("x-ai-gateway-provider"), recorder.Header().Get("x-ai-gateway-attempts"))
	}
	up.mu.Lock()
	auth := up.lastAuth
	up.mu.Unlock()
	if auth != "Bearer fake-access-b" {
		t.Fatalf("应使用第二个账户的凭据: %q", auth)
	}
	if record := lastRequestLog(t, srv); record.Attempts != 1 || !strings.Contains(record.AttemptTrail, "codex:account_paused") {
		t.Fatalf("请求日志 = attempts %d trail %q", record.Attempts, record.AttemptTrail)
	}

	pauseAccount(t, srv, second, true)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "all_candidates_paused" {
		t.Fatalf("两个都暂停 = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPausedAndDisabledCandidatesReportDisabled(t *testing.T) {
	up := newDiagnoseUpstream(t)
	// codex 停用、codex2 的账户暂停：两类人工状态同档，返回 all_candidates_disabled 并说明两种原因。
	srv, _, second := newPauseServer(t, up, func(a, b string) string { return pausePairYAML(a, b, 2, "    enabled: false\n") })
	pauseAccount(t, srv, second, true)
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "all_candidates_disabled" ||
		!strings.Contains(recorder.Body.String(), "暂停") || recorder.Header().Get("retry-after") != "" {
		t.Fatalf("停用加暂停 = %d %s", recorder.Code, recorder.Body.String())
	}
	if up.responsesCalls.Load() != 0 {
		t.Fatal("不应出网")
	}
}

// 后续候选全部暂停时，最后一个真实候选的上游错误应原样返回，而不是合成 502。
func TestPausedTrailingCandidateKeepsRealUpstreamError(t *testing.T) {
	up := newDiagnoseUpstream(t)
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "fake overloaded"}})
	})
	srv, _, second := newPauseServer(t, up, func(a, b string) string { return pausePairYAML(a, b, 2, "") })
	pauseAccount(t, srv, second, true)
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) == "all_candidates_failed" || up.responsesCalls.Load() != 1 {
		t.Fatalf("应透传真实上游错误: %d %s，出网 %d 次", recorder.Code, recorder.Body.String(), up.responsesCalls.Load())
	}
}

// 排队中的等待者在账户暂停时被唤醒并跳过；已开始发送的请求继续完成。
func TestPauseWakesQueuedWaiterAndKeepsInFlightRequest(t *testing.T) {
	up := newDiagnoseUpstream(t)
	started, release := make(chan struct{}, 1), make(chan struct{})
	completed := up.responses
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		completed(w, r)
	})
	srv, first, _ := newPauseServer(t, up, func(a, b string) string {
		return strings.Replace(chatGPTGatewayYAML(a, "", 5), "    maxConcurrent: 2\n", "    maxConcurrent: 1\n", 1)
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- responsesRequest(t, srv, nil) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一个请求没有到达上游")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- responsesRequest(t, srv, nil) }()
	deadline := time.Now().Add(3 * time.Second)
	for srv.qm.StatusOf("codex", 1, 0).Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("第二个请求没有进入排队")
		}
		time.Sleep(5 * time.Millisecond)
	}

	pauseAccount(t, srv, first, true)
	select {
	case recorder := <-secondDone:
		if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "all_candidates_paused" {
			t.Fatalf("被唤醒的等待者 = %d %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("暂停没有唤醒排队中的请求")
	}
	if status := srv.qm.StatusOf("codex", 1, 0); status.Queued != 0 || status.Running != 1 {
		t.Fatalf("被唤醒的请求不应占用队列槽: %#v", status)
	}
	select {
	case <-firstDone:
		t.Fatal("暂停不应截断已开始发送的请求")
	default:
	}
	close(release)
	select {
	case recorder := <-firstDone:
		if recorder.Code != http.StatusOK {
			t.Fatalf("在途请求 = %d %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("在途请求没有完成")
	}
	if up.responsesCalls.Load() != 1 {
		t.Fatalf("上游调用次数 = %d", up.responsesCalls.Load())
	}
	if state := srv.breaker.Snapshot()["codex"]; state.State != breaker.StateClosed || state.ConsecutiveFailures != 0 || state.ProbesInFlight != 0 {
		t.Fatalf("暂停跳过不能改变熔断判据: %#v", state)
	}
}

// 取令牌期间被暂停：令牌交换照常完成并落盘，但不发出模型请求。
func TestPauseDuringTokenExchangeSkipsWithoutRequest(t *testing.T) {
	up := newDiagnoseUpstream(t)
	soon := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	accounts := `[
  {"access_token":"fake-access-a","refresh_token":"fake-refresh-a","account_id":"remote-a","email":"a@example.invalid","expires_at":"` + soon + `"},
  {"access_token":"fake-access-b","refresh_token":"fake-refresh-b","account_id":"remote-b","email":"b@example.invalid","expires_at":"2099-01-01T00:00:00Z"}
]`
	var srvRef atomic.Pointer[server]
	var firstID atomic.Value
	up.set(&up.token, func(w http.ResponseWriter, r *http.Request) {
		if srv := srvRef.Load(); srv != nil {
			pauseAccount(t, srv, firstID.Load().(string), true)
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"access_token": "rotated-access-a", "refresh_token": "rotated-refresh-a", "expires_in": 3600})
	})
	srv, first, _ := newPauseServerWithAccounts(t, up, accounts, nil)
	firstID.Store(first)
	srvRef.Store(srv)
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusServiceUnavailable || errorTypeOf(t, recorder) != "all_candidates_paused" {
		t.Fatalf("取令牌后被暂停 = %d %s", recorder.Code, recorder.Body.String())
	}
	if up.tokenCalls.Load() != 1 || up.responsesCalls.Load() != 0 {
		t.Fatalf("令牌交换 %d 次、模型请求 %d 次", up.tokenCalls.Load(), up.responsesCalls.Load())
	}
	view, _ := srv.chatgptAuth.Account(first)
	if expires, err := time.Parse(time.RFC3339, view.ExpiresAt); err != nil || time.Until(expires) < 30*time.Minute {
		t.Fatalf("交换得到的新凭据应已落盘: %q", view.ExpiresAt)
	}
	if record := lastRequestLog(t, srv); record.Attempts != 0 || !strings.Contains(strings.Join(attemptKinds(record), ","), "codex:account_paused") {
		t.Fatalf("请求日志 = attempts %d %v", record.Attempts, attemptKinds(record))
	}
	if state := srv.breaker.Snapshot()["codex"]; state.State != breaker.StateClosed || state.ConsecutiveFailures != 0 {
		t.Fatalf("熔断判据 = %#v", state)
	}
}
