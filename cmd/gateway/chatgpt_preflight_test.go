package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/metrics"
	"ai-gateway/internal/queue"
)

func accountPreflightBody(prompt string) map[string]any {
	return map[string]any{"checks": []string{"inference"}, "mode": "account", "model": "fake-model", "prompt": prompt, "confirm": "consume_quota"}
}

func TestChatGPTPreflightUnboundPromptAndPrivacy(t *testing.T) {
	for _, prompt := range []string{"", " \n\t", " 自定义内容\n原样保留 "} {
		t.Run(fmt.Sprintf("内容长度%d", len(prompt)), func(t *testing.T) {
			up := newDiagnoseUpstream(t)
			srv, _, id := newDiagnoseServer(t, up, "")
			// 禁止普通 Provider 客户端出网，确保使用账户管理客户端。
			beforeConfig, _ := json.Marshal(srv.cfg)
			srv.resolveHTTPClient = testClientResolver(blockedClient)
			recorder := diagnoseRequest(t, srv, id, accountPreflightBody(prompt), nil)
			out := decodeDiagnose(t, recorder)
			if step := stepOf(out, "inference"); step.Status != "success" || step.Mode != "account" || step.Provider != "" {
				t.Fatalf("独立预检=%+v", step)
			}
			up.mu.Lock()
			body := up.lastResponses
			auth := up.lastAuth
			up.mu.Unlock()
			want := prompt
			if strings.TrimSpace(want) == "" {
				want = chatGPTDiagnosePrompt
			}
			input := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			if input["text"] != want || body["max_output_tokens"] != float64(256) || body["store"] != false || body["stream"] != true {
				t.Fatalf("请求不符合预算/内容: %+v", body)
			}
			if auth != "Bearer fake-access-b" {
				t.Fatal("账户凭据选择错误")
			}
			if out.Data.Account.Invocation.Mode != "account" || out.Data.Account.Invocation.Provider != "" {
				t.Fatalf("调用来源=%+v", out.Data.Account.Invocation)
			}
			if len(srv.metrics.Logs(metrics.LogFilter{})) != 0 {
				t.Fatal("诊断污染日志")
			}
			assertNoSecrets(t, recorder.Body.String(), "secret-model-output", want)
			afterConfig, _ := json.Marshal(srv.cfg)
			if string(beforeConfig) != string(afterConfig) {
				t.Fatal("预检改变了配置")
			}
		})
	}
}

func TestChatGPTPreflightInputBoundaries(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, _, id := newDiagnoseServer(t, up, "")
	for _, body := range []string{
		`{"checks":["inference"],"mode":"account","model":"m","confirm":"consume_quota","prompt":null}`,
		`{"checks":["inference"],"mode":"account","model":"m","confirm":"consume_quota","prompt":42}`,
		`{"checks":["inference"],"mode":"account","provider":"codex","model":"m","confirm":"consume_quota"}`,
		`{"checks":["inference"],"mode":"account","model":"m"}`,
		`{"prompt":""}`, `{"mode":"account"}`, `{"checks":["inference"],"mode":"account","model":"m","confirm":"consume_quota","prompt":"\u0000"}`,
		`{"checks":["inference"],"mode":"account","model":"m","confirm":"consume_quota","prompt":"` + strings.Repeat("中", 1366) + `"}`,
		"{\"prompt\":\"\xff\"}",
	} {
		if r := diagnoseRequest(t, srv, id, []byte(body), nil); r.Code != 400 {
			t.Fatalf("非法输入状态=%d %s", r.Code, r.Body.String())
		}
	}
	if up.responsesCalls.Load() != 0 || up.tokenCalls.Load() != 0 {
		t.Fatal("非法参数发生出网")
	}
	body := accountPreflightBody(strings.Repeat("a", 4096))
	if r := diagnoseRequest(t, srv, id, body, nil); r.Code != 200 {
		t.Fatalf("合法最大文本=%d %s", r.Code, r.Body.String())
	}
}

func TestChatGPTPreflightHonorsSharedQueueAndCancellation(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, _, id := newDiagnoseServer(t, up, "")
	srv.qm.SetGroupLimits(queue.GroupLimits{MaxConcurrent: 1})
	release, _, err := srv.qm.AcquireGroupOnly(context.Background(), id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- diagnoseRequest(t, srv, id, accountPreflightBody(""), func(r *http.Request) { *r = *r.WithContext(ctx) })
	}()
	deadline := time.After(time.Second)
	for srv.qm.GroupStatusOf(id).Queued == 0 {
		select {
		case <-deadline:
			t.Fatal("预检未等待共享账户槽")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消未退出")
	}
	if up.responsesCalls.Load() != 0 {
		t.Fatal("预检绕过共享槽")
	}
	if srv.qm.GroupStatusOf(id).Queued != 0 {
		t.Fatal("取消后残留排队")
	}
}

func TestChatGPTAccountCatalogCacheAndRefresh(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/backend-api/codex/models" || r.Header.Get("Authorization") != "Bearer fake-access-b" || r.Header.Get("ChatGPT-Account-Id") != "remote-b" {
			t.Errorf("账户目录请求错误: %s", r.URL)
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"fake-catalog-model","visibility":"list"}]}`)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, _, id := newDiagnoseServerWithClients(t, "", &http.Client{Transport: rewriteTransport{target: target}}, testClientResolver(blockedClient))
	t.Cleanup(srv.stopProviderQueries)
	path := "/api/chatgpt/accounts/" + id + "/models"
	for i := 0; i < 2; i++ {
		r := providerQueryRequest(srv, "GET", path, "")
		if r.Code != 200 || !strings.Contains(r.Body.String(), "fake-catalog-model") {
			t.Fatalf("目录=%d %s", r.Code, r.Body.String())
		}
		assertNoSecrets(t, r.Body.String(), "fake-access-b", "remote-b")
	}
	if calls.Load() != 1 {
		t.Fatal("目录没有命中缓存")
	}
	// 真实同源浏览器 GET 不发送 Origin；不能按管理写入接口拒绝它。
	get := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789"+path, nil)
	get.Header.Set("Sec-Fetch-Site", "same-origin")
	getRec := httptest.NewRecorder()
	srv.handle(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("同源 GET 被拒绝: %d %s", getRec.Code, getRec.Body.String())
	}
	get.Header.Set("Sec-Fetch-Site", "cross-site")
	getRec = httptest.NewRecorder()
	srv.handle(getRec, get)
	if getRec.Code != http.StatusForbidden {
		t.Fatal("跨站 GET 未拒绝")
	}
	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789"+path+"/refresh", strings.NewReader("{}"))
	post.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	srv.handle(postRec, post)
	if postRec.Code != http.StatusForbidden {
		t.Fatal("缺 Origin 的刷新写入未拒绝")
	}

	r := providerQueryRequest(srv, "POST", path+"/refresh", "{}")
	if r.Code != 200 || calls.Load() != 2 {
		t.Fatalf("刷新=%d %s", r.Code, r.Body.String())
	}
	if view, _ := srv.chatgptAuth.Account(id); view.Invocation.State != "unknown" {
		t.Fatal("目录查询污染调用状态")
	}
	if err := srv.chatgptAuth.Delete(id); err != nil {
		t.Fatal(err)
	}
	srv.reconcileAccountCatalogs()
	r = providerQueryRequest(srv, "GET", path, "")
	if r.Code != 404 {
		t.Fatalf("删除后返回目录=%d", r.Code)
	}
}

func parsePreflightResults(t *testing.T, body string) []chatGPTDiagnoseBatchResult {
	t.Helper()
	var results []chatGPTDiagnoseBatchResult
	for _, event := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(event, "event: result\n") {
			continue
		}
		var result chatGPTDiagnoseBatchResult
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "event: result\ndata: ")), &result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	return results
}

func TestChatGPTPreflightBatchMixedResultsAndDefaultPrompt(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newDiagnoseServer(t, up, "")
	if _, _, _, err := srv.chatgptAuth.SetPaused(first, true, srv.chatgptAuth.Version()); err != nil {
		t.Fatal(err)
	}
	body := accountPreflightBody("")
	body["accountIds"] = []string{first, second, "missing"}
	raw, _ := json.Marshal(body)
	r := providerQueryRequest(srv, "POST", "/api/chatgpt/accounts/diagnose/batch", string(raw))
	if r.Code != 200 || r.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("批量=%d %s", r.Code, r.Body.String())
	}
	results := parsePreflightResults(t, r.Body.String())
	if len(results) != 3 {
		t.Fatalf("结果数=%d %s", len(results), r.Body.String())
	}
	expected := map[string]string{first: "partial", second: "success", "missing": "not_found"}
	for _, result := range results {
		if result.Status != expected[result.AccountID] {
			t.Fatalf("结果=%+v", result)
		}
	}
	if up.responsesCalls.Load() != 1 || !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal("批量调用次数或结束事件错误")
	}
	assertNoSecrets(t, r.Body.String(), "secret-model-output", "Reply with OK.")
	if srv.chatGPTBatchDiagnosing.Load() {
		t.Fatal("批量门未释放")
	}
}

func TestChatGPTPreflightBatchValidationAndBusy(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, _, id := newDiagnoseServer(t, up, "")
	for _, body := range []string{`{"accountIds":[]}`, fmt.Sprintf(`{"accountIds":[%q,%q]}`, id, id), `{"accountIds":["a"],"checks":["inference"],"model":"m","mode":"account"}`, `{"accountIds":["a"],"checks":["inference"],"model":"m","provider":"codex","confirm":"consume_quota"}`} {
		r := providerQueryRequest(srv, "POST", "/api/chatgpt/accounts/diagnose/batch", body)
		if r.Code != 400 {
			t.Fatalf("非法批量=%d %s", r.Code, r.Body.String())
		}
	}
	srv.chatGPTDiagnosing.Store(id, struct{}{})
	defer srv.chatGPTDiagnosing.Delete(id)
	body := fmt.Sprintf(`{"accountIds":[%q]}`, id)
	r := providerQueryRequest(srv, "POST", "/api/chatgpt/accounts/diagnose/batch", body)
	results := parsePreflightResults(t, r.Body.String())
	if len(results) != 1 || results[0].Status != "busy" {
		t.Fatalf("忙碌=%s", r.Body.String())
	}
	srv.chatGPTBatchDiagnosing.Store(true)
	r = providerQueryRequest(srv, "POST", "/api/chatgpt/accounts/diagnose/batch", body)
	srv.chatGPTBatchDiagnosing.Store(false)
	if r.Code != 409 {
		t.Fatalf("批次重复=%d", r.Code)
	}
	if up.responsesCalls.Load() != 0 || up.tokenCalls.Load() != 0 {
		t.Fatal("非法/忙碌请求出网")
	}
}

func TestChatGPTPreflightBatchConcurrencyCancelAndBudget(t *testing.T) {
	for _, budgetExpired := range []bool{false, true} {
		t.Run(fmt.Sprintf("总预算%t", budgetExpired), func(t *testing.T) {
			up := newDiagnoseUpstream(t)
			srv, _, _ := newDiagnoseServer(t, up, "")
			var accounts []map[string]any
			for i := 0; i < 5; i++ {
				accounts = append(accounts, map[string]any{"access_token": "fake-access", "account_id": fmt.Sprintf("batch-%d", i), "expires_at": "2099-01-01T00:00:00Z"})
			}
			data, _ := json.Marshal(accounts)
			if _, err := srv.chatgptAuth.Import(data, false); err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, view := range srv.chatgptAuth.List(nil) {
				ids = append(ids, view.ID)
			}
			entered := make(chan struct{}, len(ids))
			var running, maxRunning atomic.Int32
			up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
				n := running.Add(1)
				defer running.Add(-1)
				for {
					old := maxRunning.Load()
					if n <= old || maxRunning.CompareAndSwap(old, n) {
						break
					}
				}
				entered <- struct{}{}
				<-r.Context().Done()
			})
			old := chatGPTDiagnoseBatchBudget
			if budgetExpired {
				chatGPTDiagnoseBatchBudget = 150 * time.Millisecond
			}
			defer func() { chatGPTDiagnoseBatchBudget = old }()
			body := accountPreflightBody("private-test-content")
			body["accountIds"] = ids
			raw, _ := json.Marshal(body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "http://127.0.0.1:7789/api/chatgpt/accounts/diagnose/batch", strings.NewReader(string(raw))).WithContext(ctx)
			req.Header.Set("Origin", "http://127.0.0.1:7789")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); srv.handle(rec, req) }()
			for i := 0; i < 3; i++ {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("批量未启动并发任务")
				}
			}
			if !budgetExpired {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("取消/预算未结束批次")
			}
			if maxRunning.Load() != 3 || up.responsesCalls.Load() != 3 {
				t.Fatalf("并发或取消后仍派发: max=%d calls=%d", maxRunning.Load(), up.responsesCalls.Load())
			}
			if srv.chatGPTBatchDiagnosing.Load() {
				t.Fatal("批次 gate 未释放")
			}
			for _, id := range ids {
				if _, busy := srv.chatGPTDiagnosing.Load(id); busy {
					t.Fatal("账户互斥未释放")
				}
				view, _ := srv.chatgptAuth.Account(id)
				if view.Invocation.State == "failure" {
					t.Fatal("取消误报账户调用失败")
				}
			}
			if budgetExpired {
				results := parsePreflightResults(t, rec.Body.String())
				if len(results) != len(ids) {
					t.Fatal("预算耗尽未补全结果")
				}
				for _, result := range results {
					if result.Status != "cancelled" {
						t.Fatalf("预算结果=%+v", result)
					}
				}
				if !strings.Contains(rec.Body.String(), "event: done") {
					t.Fatal("缺结束事件")
				}
			}
			assertNoSecrets(t, rec.Body.String(), "private-test-content")
		})
	}
}

func TestChatGPTAccountCatalogLateDeletionAndErrors(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"models":[{"slug":"obsolete-model","visibility":"list"}]}`)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	srv, _, id := newDiagnoseServerWithClients(t, "", &http.Client{Transport: rewriteTransport{target: target}}, testClientResolver(blockedClient))
	t.Cleanup(srv.stopProviderQueries)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- providerQueryRequest(srv, "GET", "/api/chatgpt/accounts/"+id+"/models", "") }()
	<-started
	if err := srv.chatgptAuth.Delete(id); err != nil {
		t.Fatal(err)
	}
	srv.reconcileAccountCatalogs()
	close(release)
	rec := <-done
	if rec.Code != 409 || strings.Contains(rec.Body.String(), "obsolete-model") {
		t.Fatalf("迟到目录=%d %s", rec.Code, rec.Body.String())
	}
}

func TestChatGPTPreflightProviderPromptAndStreamFailure(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, id, _ := newDiagnoseServer(t, up, "")
	body := inferenceBody("fake-model")
	body["prompt"] = "provider-private-input"
	out := decodeDiagnose(t, diagnoseRequest(t, srv, id, body, nil))
	if stepOf(out, "inference").Status != "success" {
		t.Fatal("旧Provider模式失效")
	}
	up.mu.Lock()
	encoded, _ := json.Marshal(up.lastResponses)
	up.mu.Unlock()
	if !strings.Contains(string(encoded), "provider-private-input") {
		t.Fatal("Provider 未使用自定义内容")
	}
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"private-failure-message\"}}}\n\n")
	})
	rec := diagnoseRequest(t, srv, id, accountPreflightBody(""), nil)
	out = decodeDiagnose(t, rec)
	if stepOf(out, "inference").Status != "failure" {
		t.Fatalf("流内错误未识别: %+v", stepOf(out, "inference"))
	}
	assertNoSecrets(t, rec.Body.String(), "private-failure-message")
}
