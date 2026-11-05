package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/balancer"
	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/router"
)

// 第五阶段：账户共享约束、共享冷却与额度感知排序。只使用假账户与 diagnoseUpstream 本机假上游。

const accountLimitsSharedCooldown = "chatgptAccountLimits:\n  sharedCooldown: true\n"

// usageLimitBody 是 ChatGPT 额度耗尽的错误正文形状（HTTP 429 + error.type=usage_limit_reached）。
func usageLimitBody(resetsInSeconds int) string {
	return `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":` + strconv.Itoa(resetsInSeconds) + `}}`
}

// limitedFor 让指定访问令牌的账户返回额度耗尽，其余账户沿用原来的成功响应。
func limitedFor(up *diagnoseUpstream, tokens map[string]bool, body string) {
	up.mu.Lock()
	success := up.responses
	up.mu.Unlock()
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		if tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body))
			return
		}
		success(w, r)
	})
}

func newCooldownServer(t *testing.T, up *diagnoseUpstream, extra string) (*server, string, string) {
	t.Helper()
	srv, first, second := newPauseServer(t, up, func(a, b string) string { return pausePairYAML(a, b, 2, "") + extra })
	withAccountEvents(srv)
	srv.applyChatGPTAccountLimits(srv.cfg)
	return srv, first, second
}

func TestUsageLimitResetAtParsing(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter time.Duration
		ok         bool
		want       time.Time
	}{
		{name: "非 429", status: 403, body: `{"error":{"type":"usage_limit_reached"}}`},
		{name: "普通限流", status: 429, body: `{"error":{"type":"rate_limit_exceeded","resets_in_seconds":60}}`},
		{name: "非 JSON", status: 429, body: `too many requests`},
		{name: "空正文", status: 429},
		{name: "无时间", status: 429, body: `{"error":{"type":"usage_limit_reached"}}`, ok: true},
		{name: "resets_in_seconds", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`, ok: true, want: now.Add(120 * time.Second)},
		{name: "resets_at 秒", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_at":1900000600}}`, ok: true, want: now.Add(600 * time.Second)},
		{name: "resets_at 毫秒", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_at":1900000600000}}`, ok: true, want: now.Add(600 * time.Second)},
		{name: "数字字符串", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":"90"}}`, ok: true, want: now.Add(90 * time.Second)},
		{name: "取最晚", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_at":1900000100,"resets_in_seconds":300}}`, retryAfter: 200 * time.Second, ok: true, want: now.Add(300 * time.Second)},
		{name: "Retry-After 更晚", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`, retryAfter: time.Hour, ok: true, want: now.Add(time.Hour)},
		{name: "已过去的时间", status: 429, body: `{"error":{"type":"usage_limit_reached","resets_at":1800000000}}`, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usageLimitResetAt(tc.status, tc.body, tc.retryAfter, now)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("usageLimitResetAt = %v,%v，期望 %v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSharedCooldownSkipsAccountAndFailsOver(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newCooldownServer(t, up, accountLimitsSharedCooldown)
	limitedFor(up, map[string]bool{"fake-access-a": true}, usageLimitBody(3600))

	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusOK || recorder.Header().Get("x-ai-gateway-provider") != "codex2" {
		t.Fatalf("首个请求应转移到 codex2: %d provider=%q %s", recorder.Code, recorder.Header().Get("x-ai-gateway-provider"), recorder.Body.String())
	}
	// 冷却 1 小时超过 maxRetryAfterMs（默认 5s），触发冷却的那次按免费尝试处理。
	if recorder.Header().Get("x-ai-gateway-attempts") != "1" {
		t.Fatalf("触发冷却的尝试不应占用额度：attempts=%q", recorder.Header().Get("x-ai-gateway-attempts"))
	}
	until := srv.chatgptAuth.CooldownUntil(first)
	if remaining := time.Until(until); remaining < 59*time.Minute || remaining > time.Hour+time.Minute {
		t.Fatalf("冷却截止 = %v（剩余 %s）", until, remaining)
	}
	if view, _ := srv.chatgptAuth.Account(first); view.CooldownUntil == "" {
		t.Fatal("账户视图应带 cooldownUntil")
	}

	calls := up.responsesCalls.Load()
	recorder = responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusOK || recorder.Header().Get("x-ai-gateway-provider") != "codex2" {
		t.Fatalf("冷却中的账户应被跳过: %d %s", recorder.Code, recorder.Body.String())
	}
	if up.responsesCalls.Load() != calls+1 {
		t.Fatalf("冷却跳过不应出网：调用 %d → %d", calls, up.responsesCalls.Load())
	}
	record := lastRequestLog(t, srv)
	if len(record.AttemptDetails) < 1 || record.AttemptDetails[0].Kind != "account_cooldown" || record.AttemptDetails[0].RetryAfterMs <= 0 {
		t.Fatalf("尝试明细 = %v", attemptKinds(record))
	}
	if !strings.Contains(record.AttemptTrail, "codex:account_cooldown") {
		t.Fatalf("尝试轨迹 = %q", record.AttemptTrail)
	}
	stats := accountMetrics(t, srv, first)
	if stats.Data.Window.Skips.Cooldown != 1 || stats.Data.Window.RateLimited != 1 {
		t.Fatalf("账户统计 skips=%#v rateLimited=%d", stats.Data.Window.Skips, stats.Data.Window.RateLimited)
	}
	if kinds := strings.Join(accountEventKinds(t, srv), ","); !strings.Contains(kinds, chatgptauth.EventCooldown) {
		t.Fatalf("事件 = %s", kinds)
	}
}

func TestAllAccountsCooledDownReturnsAccountUsageLimited(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, second := newCooldownServer(t, up, accountLimitsSharedCooldown)
	limitedFor(up, map[string]bool{"fake-access-a": true, "fake-access-b": true}, usageLimitBody(3600))

	// 最后一个候选的额度耗尽原样透传，但两个账户都进入冷却。
	recorder := responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("首个请求 = %d %s", recorder.Code, recorder.Body.String())
	}
	if srv.chatgptAuth.CooldownUntil(first).IsZero() || srv.chatgptAuth.CooldownUntil(second).IsZero() {
		t.Fatal("两个账户都应进入冷却")
	}
	calls := up.responsesCalls.Load()
	recorder = responsesRequest(t, srv, nil)
	if recorder.Code != http.StatusTooManyRequests || errorTypeOf(t, recorder) != "account_usage_limited" {
		t.Fatalf("全部冷却 = %d %s", recorder.Code, recorder.Body.String())
	}
	seconds, err := strconv.Atoi(recorder.Header().Get("retry-after"))
	if err != nil || seconds < 3500 || seconds > 3601 {
		t.Fatalf("retry-after = %q", recorder.Header().Get("retry-after"))
	}
	if up.responsesCalls.Load() != calls {
		t.Fatal("全部冷却时不应出网")
	}
	if record := lastRequestLog(t, srv); record.Attempts != 0 {
		t.Fatalf("冷却跳过不应计入尝试：%d", record.Attempts)
	}
}

func TestSharedCooldownOffKeepsProviderSemantics(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newCooldownServer(t, up, "")
	limitedFor(up, map[string]bool{"fake-access-a": true}, usageLimitBody(3600))
	for i := 0; i < 2; i++ {
		calls := up.responsesCalls.Load()
		recorder := responsesRequest(t, srv, nil)
		if recorder.Code != http.StatusOK || up.responsesCalls.Load() != calls+2 {
			t.Fatalf("第 %d 次：未开启共享冷却时每次都应先试 codex：%d calls %d→%d", i+1, recorder.Code, calls, up.responsesCalls.Load())
		}
	}
	if !srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("未开启时不应冷却")
	}
}

func TestNonUsageLimit429DoesNotCooldown(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newCooldownServer(t, up, accountLimitsSharedCooldown)
	limitedFor(up, map[string]bool{"fake-access-a": true}, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
	if recorder := responsesRequest(t, srv, nil); recorder.Code != http.StatusOK {
		t.Fatalf("请求 = %d", recorder.Code)
	}
	if !srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("普通 429 不应升级为账户冷却")
	}
}

func TestClearCooldownEndpoint(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newCooldownServer(t, up, accountLimitsSharedCooldown)
	limitedFor(up, map[string]bool{"fake-access-a": true}, usageLimitBody(3600))
	responsesRequest(t, srv, nil)
	if srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("前置：应已冷却")
	}
	path := "/api/chatgpt/accounts/" + first + "/cooldown"
	if recorder := chatGPTAPI(t, srv, http.MethodGet, path, ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", recorder.Code)
	}
	deleteWithOrigin := func(target, origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:7789"+target, nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		srv.handle(recorder, request)
		return recorder
	}
	const local = "http://127.0.0.1:7789"
	if recorder := deleteWithOrigin("/api/chatgpt/accounts/bad%2Fid/cooldown", local); recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 ID = %d", recorder.Code)
	}
	if recorder := deleteWithOrigin("/api/chatgpt/accounts/acc_0000000000000000000000000000000f/cooldown", local); recorder.Code != http.StatusNotFound {
		t.Fatalf("不存在 = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, origin := range []string{"https://evil.example", ""} {
		if recorder := deleteWithOrigin(path, origin); recorder.Code != http.StatusForbidden || srv.chatgptAuth.CooldownUntil(first).IsZero() {
			t.Fatalf("Origin %q = %d，应拒绝且保留冷却", origin, recorder.Code)
		}
	}

	clearOnce := func() (bool, chatgptauth.AccountView) {
		recorder := deleteWithOrigin(path, local)
		if recorder.Code != http.StatusOK {
			t.Fatalf("解除 = %d %s", recorder.Code, recorder.Body.String())
		}
		var out struct {
			Data struct {
				Account chatgptauth.AccountView `json:"account"`
				Changed bool                    `json:"changed"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data.Changed, out.Data.Account
	}
	changed, view := clearOnce()
	if !changed || view.CooldownUntil != "" || view.ID != first {
		t.Fatalf("首次解除 changed=%v view=%#v", changed, view)
	}
	if changed, _ := clearOnce(); changed {
		t.Fatal("没有冷却时 changed 应为 false")
	}
	if kinds := strings.Join(accountEventKinds(t, srv), ","); !strings.Contains(kinds, chatgptauth.EventCooldownCleared) {
		t.Fatalf("事件 = %s", kinds)
	}
	// 解除后新的请求可以再次触发冷却。
	responsesRequest(t, srv, nil)
	if srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("解除后上游仍耗尽，应再次冷却")
	}
	// 关闭共享冷却的热重载清除全部冷却。
	off := *srv.cfg
	off.ChatGPTAccountLimits = config.ChatGPTAccountLimits{}
	srv.applyChatGPTAccountLimits(&off)
	if !srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("关闭 sharedCooldown 应清除冷却")
	}
}

// sharedAccountYAML 让 codex、codex2 绑定同一账户，各自走一条单候选路由。
func sharedAccountYAML(id, limits string, directMode bool) string {
	direct := ""
	if directMode {
		direct = "directMode: true\n"
	}
	return `host: "127.0.0.1"
port: 7789
` + direct + limits + `providers:
  codex:
    baseUrl: "https://chatgpt.com/backend-api/codex"
    format: openai-responses
    authType: chatgptOAuth
    accountId: "` + id + `"
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
  codex2:
    baseUrl: "https://chatgpt.com/backend-api/codex"
    format: openai-responses
    authType: chatgptOAuth
    accountId: "` + id + `"
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "gpt-a"
    provider: codex
    model: gpt-6-luna
  - match: "gpt-b"
    provider: codex2
    model: gpt-6-luna
`
}

func modelRequest(srv *server, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.handle(recorder, request)
	return recorder
}

// blockFirstResponse 让第一个上游请求阻塞到 release 关闭，返回「已进入上游」信号。
func blockFirstResponse(up *diagnoseUpstream) (entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	up.mu.Lock()
	success := up.responses
	up.mu.Unlock()
	var once sync.Once
	up.set(&up.responses, func(w http.ResponseWriter, r *http.Request) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
		}
		success(w, r)
	})
	return entered, release
}

func waitAccountGroup(t *testing.T, srv *server, id string, running, queued int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := srv.qm.GroupStatusOf(id)
		if status.Running == running && status.Queued == queued {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("账户准入状态 = %+v，期望 running=%d queued=%d", srv.qm.GroupStatusOf(id), running, queued)
}

type admissionResponse struct {
	Data struct {
		Admission chatGPTAdmissionView `json:"admission"`
	} `json:"data"`
}

func accountAdmission(t *testing.T, srv *server, id string) chatGPTAdmissionView {
	t.Helper()
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts/"+id+"/metrics", "")
	var out admissionResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &out) != nil {
		t.Fatalf("账户统计 = %d %s", recorder.Code, recorder.Body.String())
	}
	return out.Data.Admission
}

func newSharedAccountServer(t *testing.T, up *diagnoseUpstream, limits string, directMode bool) (*server, string) {
	t.Helper()
	srv, first, _ := newPauseServer(t, up, func(a, _ string) string { return sharedAccountYAML(a, limits, directMode) })
	withAccountEvents(srv)
	srv.applyChatGPTAccountLimits(srv.cfg)
	return srv, first
}

func TestAccountConcurrencySharedAcrossProviders(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, id := newSharedAccountServer(t, up, "chatgptAccountLimits:\n  maxConcurrent: 1\n", false)
	entered, release := blockFirstResponse(up)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- modelRequest(srv, "gpt-a") }()
	<-entered
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- modelRequest(srv, "gpt-b") }()
	waitAccountGroup(t, srv, id, 1, 1)
	if status := srv.qm.StatusOf("codex2", 5, 0); status.Running != 0 || status.Queued != 1 {
		t.Fatalf("账户排队不应占用 codex2 槽位：%+v", status)
	}
	admission := accountAdmission(t, srv, id)
	if !admission.Limited || admission.Running != 1 || admission.Queued != 1 || admission.MaxConcurrent != 1 || admission.SharedCooldown {
		t.Fatalf("admission = %#v", admission)
	}
	close(release)
	for _, done := range []chan *httptest.ResponseRecorder{firstDone, secondDone} {
		select {
		case recorder := <-done:
			if recorder.Code != http.StatusOK {
				t.Fatalf("请求 = %d %s", recorder.Code, recorder.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("请求未完成")
		}
	}
	waitAccountGroup(t, srv, id, 0, 0)
}

func TestCooldownWakesQueuedAccountWaiter(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, id := newSharedAccountServer(t, up, "chatgptAccountLimits:\n  maxConcurrent: 1\n  sharedCooldown: true\n", false)
	entered, release := blockFirstResponse(up)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- modelRequest(srv, "gpt-a") }()
	<-entered
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- modelRequest(srv, "gpt-b") }()
	waitAccountGroup(t, srv, id, 1, 1)

	ticket := srv.chatgptAuth.BeginInvocation(id, "codex", "gpt-6-luna")
	srv.chatgptAuth.ReportUsageLimit(ticket, time.Now().Add(time.Hour))
	select {
	case recorder := <-secondDone:
		if recorder.Code != http.StatusTooManyRequests || errorTypeOf(t, recorder) != "account_usage_limited" || recorder.Header().Get("retry-after") == "" {
			t.Fatalf("排队中的请求应被冷却唤醒：%d %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("冷却没有唤醒排队中的请求")
	}
	if status := srv.qm.StatusOf("codex2", 5, 0); status.Running != 0 || status.Queued != 0 {
		t.Fatalf("放弃后 codex2 状态 = %+v", status)
	}
	close(release)
	if recorder := <-firstDone; recorder.Code != http.StatusOK {
		t.Fatalf("已准入的请求不受之后的冷却影响：%d", recorder.Code)
	}
	if stats := accountMetrics(t, srv, id); stats.Data.Window.Skips.Cooldown != 1 {
		t.Fatalf("冷却跳过 = %#v", stats.Data.Window.Skips)
	}
}

func TestDirectModeSkipsAccountLimits(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, id := newSharedAccountServer(t, up, "chatgptAccountLimits:\n  maxConcurrent: 1\n", true)
	entered, release := blockFirstResponse(up)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- modelRequest(srv, "gpt-a") }()
	<-entered
	if recorder := modelRequest(srv, "gpt-b"); recorder.Code != http.StatusOK {
		t.Fatalf("直通模式不应受账户并发约束：%d", recorder.Code)
	}
	if admission := accountAdmission(t, srv, id); admission.Limited || admission.MaxConcurrent != 0 {
		t.Fatalf("直通模式 admission = %#v", admission)
	}
	close(release)
	<-firstDone
}

// usageFor 让指定访问令牌的账户返回已耗尽的用量，其余账户剩余 60%。
func usageFor(up *diagnoseUpstream, exhausted map[string]bool) {
	reset := time.Now().Add(3 * time.Hour).Unix()
	up.set(&up.usage, func(w http.ResponseWriter, r *http.Request) {
		used := 40
		if exhausted[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			used = 100
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"plan_type": "plus", "rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": used, "limit_window_seconds": 18000, "reset_at": reset},
		}})
	})
}

func newQuotaServer(t *testing.T, up *diagnoseUpstream, quotaAware bool) (*server, string, string) {
	t.Helper()
	srv, first, second := newPauseServer(t, up, func(a, b string) string {
		route := "routes:\n  - match: \"gpt-*\"\n    strategy: round-robin\n"
		if quotaAware {
			route += "    quotaAware: true\n"
		}
		return strings.Replace(pausePairYAML(a, b, 2, ""), "routes:\n  - match: \"gpt-*\"\n", route, 1)
	})
	srv.selector = balancer.New()
	usageFor(up, map[string]bool{"fake-access-a": true})
	for _, id := range []string{first, second} {
		if _, err := srv.chatgptAuth.RefreshAccountUsage(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	if tier := srv.chatgptAuth.QuotaTier(first); tier != chatgptauth.QuotaExhausted {
		t.Fatalf("前置：账户 a 应为已耗尽，实际 %d", tier)
	}
	return srv, first, second
}

func TestQuotaAwareOrderPrefersAccountsWithQuota(t *testing.T) {
	for _, quotaAware := range []bool{false, true} {
		up := newDiagnoseUpstream(t)
		srv, _, _ := newQuotaServer(t, up, quotaAware)
		served := map[string]int{}
		for i := 0; i < 4; i++ {
			recorder := responsesRequest(t, srv, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("请求 = %d %s", recorder.Code, recorder.Body.String())
			}
			served[recorder.Header().Get("x-ai-gateway-provider")]++
		}
		if quotaAware && served["codex2"] != 4 {
			t.Fatalf("额度感知应总是先试有额度的账户：%v", served)
		}
		if !quotaAware && (served["codex"] != 2 || served["codex2"] != 2) {
			t.Fatalf("未开启时保持轮询：%v", served)
		}
	}
}

func TestQuotaOrderKeepsStickyTargetFirst(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, _, _ := newQuotaServer(t, up, true)
	matched := router.MatchRoute("gpt-6-luna", srv.cfg)
	if matched == nil || !matched.QuotaAware || len(matched.Candidates) != 2 {
		t.Fatalf("路由 = %#v", matched)
	}
	if got := srv.applyQuotaOrder(matched, []int{0, 1}, -1); got[0] != 1 || got[1] != 0 {
		t.Fatalf("无粘性时已耗尽账户应排后：%v", got)
	}
	if got := srv.applyQuotaOrder(matched, []int{0, 1}, 0); got[0] != 0 || got[1] != 1 {
		t.Fatalf("粘性命中的目标应保持队首：%v", got)
	}
	// 粘性下标不在队首（未命中提到队首）时不享受豁免。
	if got := srv.applyQuotaOrder(matched, []int{0, 1}, 1); got[0] != 1 {
		t.Fatalf("粘性下标与队首不一致时按分层：%v", got)
	}
	plain := *matched
	plain.QuotaAware = false
	if got := srv.applyQuotaOrder(&plain, []int{0, 1}, -1); got[0] != 0 || got[1] != 1 {
		t.Fatalf("未开启时不改变顺序：%v", got)
	}
}

func TestCandidateKeyIncludesBoundAccount(t *testing.T) {
	oauth := router.Candidate{Provider: &config.Provider{Name: "codex", AuthType: "chatgptOAuth", AccountID: "acc_a"}, TargetModel: "m"}
	rebound := router.Candidate{Provider: &config.Provider{Name: "codex", AuthType: "chatgptOAuth", AccountID: "acc_b"}, TargetModel: "m"}
	plain := router.Candidate{Provider: &config.Provider{Name: "primary"}, TargetModel: "m"}
	if candidateKey(oauth) == candidateKey(rebound) {
		t.Fatal("换绑账户后粘性身份必须变化")
	}
	if candidateKey(plain) != "primary/m" {
		t.Fatalf("非 OAuth 候选身份保持不变：%q", candidateKey(plain))
	}
}

func TestApplyRuntimeConfigPropagatesAccountLimits(t *testing.T) {
	up := newDiagnoseUpstream(t)
	srv, first, _ := newCooldownServer(t, up, accountLimitsSharedCooldown)
	limitedFor(up, map[string]bool{"fake-access-a": true}, usageLimitBody(3600))
	responsesRequest(t, srv, nil)
	if srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("前置：应已冷却")
	}
	raw := strings.Replace(pausePairYAML(first, "acc_unused", 2, ""), "routes:", "chatgptAccountLimits:\n  maxConcurrent: 3\n  maxPerSecond: 4\nroutes:", 1)
	next, err := config.DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	srv.applyRuntimeConfig(next, "rev-2")
	if got := srv.qm.GroupLimitsSnapshot(); got.MaxConcurrent != 3 || got.MaxPerSecond != 4 {
		t.Fatalf("热重载后组上限 = %+v", got)
	}
	if srv.chatgptAuth.SharedCooldownEnabled() || !srv.chatgptAuth.CooldownUntil(first).IsZero() {
		t.Fatal("热重载关闭 sharedCooldown 应清除冷却")
	}
}
