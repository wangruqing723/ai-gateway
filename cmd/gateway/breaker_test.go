package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/breaker"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
)

// newBreakerTestServer 在 failover 测试网关上挂一个熔断器，参数由调用方给定。
func newBreakerTestServer(providers map[string]*config.Provider, route config.Route, client *http.Client,
	failover config.Failover, settings breaker.Settings) *server {
	srv := newFailoverTestServer(providers, route, client, failover)
	srv.breaker = breaker.New(settings)
	return srv
}

func testBreakerSettings() breaker.Settings {
	return breaker.Settings{Enabled: true, ConsecutiveFailures: 3, OpenMs: 30_000, HalfOpenProbes: 1}
}

// singleCandidateRoute 只有一个候选，用于观察「最后一个候选」的熔断上报。
func singleCandidateRoute(provider, model string) config.Route {
	return config.Route{Match: "*", Targets: []config.Target{{Provider: provider, Model: model}}}
}

const anthropicReqBody = `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// statusServer 返回固定状态码，并统计命中次数。
func statusServer(code int, hits *atomic.Int32, headers map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		if code == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, anthropicOKBody)
			return
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
}

// TestBreakerOpensAfterConsecutiveFailures 覆盖回归点：最后一个候选（allowRetry=false，
// 不触发放弃）的 5xx 必须计入失败，否则单候选路由永远开不了路。
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	var hits atomic.Int32
	up := statusServer(http.StatusBadGateway, &hits, nil)
	defer up.Close()

	providers := map[string]*config.Provider{
		"solo": {Name: "solo", BaseURL: up.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newBreakerTestServer(providers, singleCandidateRoute("solo", "m"), up.Client(),
		defaultTestFailover(), testBreakerSettings())

	// 前 3 次真实打到上游，502 透传给客户端
	for i := 1; i <= 3; i++ {
		recorder := postInference(srv, "/v1/messages", anthropicReqBody)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("第 %d 次 status = %d, 期望 502 透传", i, recorder.Code)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("上游命中 = %d, 期望 3", hits.Load())
	}
	snap := srv.breaker.Snapshot()
	if got := snap["solo"].State; got != breaker.StateOpen {
		t.Fatalf("state = %q, 期望 %s（连续 3 次 502 应开路）", got, breaker.StateOpen)
	}

	// 第 4 次应被熔断拦住，不再打上游
	recorder := postInference(srv, "/v1/messages", anthropicReqBody)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("熔断后 status = %d, 期望 503", recorder.Code)
	}
	if hits.Load() != 3 {
		t.Errorf("熔断后上游命中 = %d, 期望仍为 3（不应再发起请求）", hits.Load())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败: %v, body=%s", err, recorder.Body.String())
	}
	if payload.Error.Type != "breaker_open" {
		t.Errorf("error.type = %q, 期望 breaker_open", payload.Error.Type)
	}
	retryAfter := recorder.Header().Get("retry-after")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Errorf("retry-after = %q, 期望 >=1 的整数秒", retryAfter)
	}
}

// TestBreakerSkipDoesNotConsumeAttemptBudget 是本阶段最关键的断言：
// maxAttempts=2 且首个候选已熔断时，剩下两个候选仍应各拿到一次尝试额度。
// 若熔断跳过消耗额度，第三个候选永远轮不到，熔断反而削弱了可用性。
func TestBreakerSkipDoesNotConsumeAttemptBudget(t *testing.T) {
	var deadHits, badHits, goodHits atomic.Int32
	dead := statusServer(http.StatusBadGateway, &deadHits, nil)
	defer dead.Close()
	bad := statusServer(http.StatusBadGateway, &badHits, nil)
	defer bad.Close()
	good := statusServer(http.StatusOK, &goodHits, nil)
	defer good.Close()

	providers := map[string]*config.Provider{
		"dead": {Name: "dead", BaseURL: dead.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"bad":  {Name: "bad", BaseURL: bad.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"good": {Name: "good", BaseURL: good.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "dead", Model: "m1"},
		{Provider: "bad", Model: "m2"},
		{Provider: "good", Model: "m3"},
	}}
	failover := defaultTestFailover()
	attempts := 2
	failover.MaxAttempts = &attempts
	srv := newBreakerTestServer(providers, route, dead.Client(), failover, testBreakerSettings())

	// 直接用公开 API 把 dead 打到开路，避免依赖额外请求造状态
	for i := 0; i < 3; i++ {
		srv.breaker.Report("dead", breaker.OutcomeFailure)
	}
	if got := srv.breaker.Snapshot()["dead"].State; got != breaker.StateOpen {
		t.Fatalf("前置条件不成立: dead state = %q", got)
	}

	recorder := postInference(srv, "/v1/messages", anthropicReqBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望跳过 dead 后转移到 good 得 200", recorder.Code, recorder.Body.String())
	}
	if deadHits.Load() != 0 {
		t.Errorf("dead 命中 = %d, 期望 0（已熔断）", deadHits.Load())
	}
	if badHits.Load() != 1 || goodHits.Load() != 1 {
		t.Errorf("命中 bad=%d good=%d, 期望各 1 次", badHits.Load(), goodHits.Load())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "good" {
		t.Errorf("x-ai-gateway-provider = %q, 期望 good", got)
	}
	// 熔断跳过不计入尝试次数，最终成功是第 2 次真实尝试
	if got := recorder.Header().Get("x-ai-gateway-attempts"); got != "2" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望 2", got)
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条, 期望 1 条", len(logs))
	}
	if !strings.Contains(logs[0].AttemptTrail, "dead:breaker_open") {
		t.Errorf("AttemptTrail = %q, 期望含 dead:breaker_open", logs[0].AttemptTrail)
	}
}

// TestBreakerAllCandidatesOpenReturns503 全部候选熔断时给 503 + retry-after，
// 并且一次上游请求都不发。
func TestBreakerAllCandidatesOpenReturns503(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := statusServer(http.StatusOK, &aHits, nil)
	defer a.Close()
	b := statusServer(http.StatusOK, &bHits, nil)
	defer b.Close()

	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: a.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"b": {Name: "b", BaseURL: b.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	srv := newBreakerTestServer(providers, route, a.Client(), defaultTestFailover(), testBreakerSettings())
	for i := 0; i < 3; i++ {
		srv.breaker.Report("a", breaker.OutcomeFailure)
		srv.breaker.Report("b", breaker.OutcomeFailure)
	}

	recorder := postInference(srv, "/v1/messages", anthropicReqBody)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status/body = %d/%s, 期望 503", recorder.Code, recorder.Body.String())
	}
	if aHits.Load() != 0 || bHits.Load() != 0 {
		t.Errorf("命中 a=%d b=%d, 期望全为 0", aHits.Load(), bHits.Load())
	}
	if recorder.Header().Get("retry-after") == "" {
		t.Error("缺少 retry-after 头，客户端无法得知何时重试")
	}
	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 1 {
		t.Fatalf("请求日志 = %d 条, 期望 1 条", len(logs))
	}
	trail := logs[0].AttemptTrail
	if !strings.Contains(trail, "a:breaker_open") || !strings.Contains(trail, "b:breaker_open") {
		t.Errorf("AttemptTrail = %q, 期望两个候选都记为 breaker_open", trail)
	}
	if logs[0].Attempts != 0 {
		t.Errorf("Attempts = %d, 期望 0（没有真实尝试）", logs[0].Attempts)
	}
}

// TestBreakerIgnoresRateLimitAndAuthErrors 429/401/403 不进熔断判据：
// 429 是上游正常限流，401/403 是配置问题，摘掉上游都解决不了还会掩盖真实原因。
func TestBreakerIgnoresRateLimitAndAuthErrors(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"429 限流不开路", http.StatusTooManyRequests},
		{"401 认证失败不开路", http.StatusUnauthorized},
		{"403 无权限不开路", http.StatusForbidden},
		{"400 请求错误不开路", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			up := statusServer(tc.code, &hits, nil)
			defer up.Close()
			providers := map[string]*config.Provider{
				"solo": {Name: "solo", BaseURL: up.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
			}
			srv := newBreakerTestServer(providers, singleCandidateRoute("solo", "m"), up.Client(),
				defaultTestFailover(), testBreakerSettings())

			// 打足够多次，若判据错了必然开路
			for i := 0; i < 5; i++ {
				postInference(srv, "/v1/messages", anthropicReqBody)
			}
			if hits.Load() != 5 {
				t.Fatalf("上游命中 = %d, 期望 5（不应被熔断拦截）", hits.Load())
			}
			state := srv.breaker.Snapshot()["solo"]
			if state.State == breaker.StateOpen {
				t.Errorf("state = %s, 期望保持闭合", state.State)
			}
			if state.ConsecutiveFailures != 0 {
				t.Errorf("ConsecutiveFailures = %d, 期望 0", state.ConsecutiveFailures)
			}
		})
	}
}

// TestBreakerSuccessResetsFailureStreak 失败后成功应清零，避免零散失败累积成误熔断。
func TestBreakerSuccessResetsFailureStreak(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(http.StatusBadGateway))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		code := int(mode.Load())
		if code == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, anthropicOKBody)
			return
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer up.Close()

	providers := map[string]*config.Provider{
		"solo": {Name: "solo", BaseURL: up.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newBreakerTestServer(providers, singleCandidateRoute("solo", "m"), up.Client(),
		defaultTestFailover(), testBreakerSettings())

	postInference(srv, "/v1/messages", anthropicReqBody)
	postInference(srv, "/v1/messages", anthropicReqBody)
	if got := srv.breaker.Snapshot()["solo"].ConsecutiveFailures; got != 2 {
		t.Fatalf("失败计数 = %d, 期望 2", got)
	}
	mode.Store(int32(http.StatusOK))
	if recorder := postInference(srv, "/v1/messages", anthropicReqBody); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200", recorder.Code)
	}
	state := srv.breaker.Snapshot()["solo"]
	if state.ConsecutiveFailures != 0 || state.State != breaker.StateClosed {
		t.Errorf("state = %s failures = %d, 期望 closed/0", state.State, state.ConsecutiveFailures)
	}
}

// TestBreakerResetEndpoint 手动重置端点：带 provider 只重置该项，不带则全量重置。
func TestBreakerResetEndpoint(t *testing.T) {
	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"b": {Name: "b", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newBreakerTestServer(providers, singleCandidateRoute("a", "m"), http.DefaultClient,
		defaultTestFailover(), testBreakerSettings())
	for i := 0; i < 3; i++ {
		srv.breaker.Report("a", breaker.OutcomeFailure)
		srv.breaker.Report("b", breaker.OutcomeFailure)
	}

	// 只重置 a
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/breaker/reset?provider=a", nil)
	srv.handle(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 200", recorder.Code, recorder.Body.String())
	}
	var single struct {
		Provider string                   `json:"provider"`
		Reset    bool                     `json:"reset"`
		Breakers map[string]breaker.State `json:"breakers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &single); err != nil {
		t.Fatalf("解析响应失败: %v, body=%s", err, recorder.Body.String())
	}
	if single.Provider != "a" || !single.Reset {
		t.Errorf("provider=%q reset=%v, 期望 a/true", single.Provider, single.Reset)
	}
	if single.Breakers["a"].State != breaker.StateClosed {
		t.Errorf("a state = %q, 期望 closed", single.Breakers["a"].State)
	}
	if single.Breakers["b"].State != breaker.StateOpen {
		t.Errorf("b state = %q, 期望仍为 open（未指定不应被重置）", single.Breakers["b"].State)
	}

	// 全量重置
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/breaker/reset", nil)
	srv.handle(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("全量重置 status = %d", recorder.Code)
	}
	for name, state := range srv.breaker.Snapshot() {
		if state.State != breaker.StateClosed {
			t.Errorf("%s state = %q, 期望全部 closed", name, state.State)
		}
	}
}

// TestBreakerResetRejectsWrongMethodAndCrossSite 管理端点的边界与其他 /api 一致。
func TestBreakerResetRejectsWrongMethodAndCrossSite(t *testing.T) {
	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newBreakerTestServer(providers, singleCandidateRoute("a", "m"), http.DefaultClient,
		defaultTestFailover(), testBreakerSettings())

	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/breaker/reset", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, 期望 405", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/breaker/reset", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	srv.handle(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("跨站 status = %d, 期望 403", recorder.Code)
	}
}

// TestBreakerResetWithoutBreakerReturns503 未启用熔断时端点明确报错，而不是静默成功。
func TestBreakerResetWithoutBreakerReturns503(t *testing.T) {
	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	srv := newFailoverTestServer(providers, singleCandidateRoute("a", "m"), http.DefaultClient, defaultTestFailover())
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/providers/breaker/reset", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status/body = %d/%s, 期望 503", recorder.Code, recorder.Body.String())
	}
}

// TestHealthExposesBreakerSnapshot /health 带出熔断状态；未启用时为 null，
// 让调用方能区分「没开熔断」和「全部健康」。
func TestHealthExposesBreakerSnapshot(t *testing.T) {
	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := singleCandidateRoute("a", "m")

	srv := newBreakerTestServer(providers, route, http.DefaultClient, defaultTestFailover(), testBreakerSettings())
	for i := 0; i < 3; i++ {
		srv.breaker.Report("a", breaker.OutcomeFailure)
	}
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var enabled struct {
		Breakers map[string]breaker.State `json:"breakers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &enabled); err != nil {
		t.Fatalf("解析 /health 失败: %v, body=%s", err, recorder.Body.String())
	}
	if enabled.Breakers["a"].State != breaker.StateOpen {
		t.Errorf("breakers.a.state = %q, 期望 open", enabled.Breakers["a"].State)
	}

	// 未启用熔断：字段应为 null
	off := newFailoverTestServer(providers, route, http.DefaultClient, defaultTestFailover())
	recorder = httptest.NewRecorder()
	off.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析 /health 失败: %v", err)
	}
	if got := strings.TrimSpace(string(raw["breakers"])); got != "null" {
		t.Errorf("breakers = %s, 期望 null", got)
	}
}

// TestBreakerOutcomeForCountsPost2xxFailures 锁定「拿到成功响应头之后才失败」必须计入熔断。
//
// 回归点：早先的实现只在 upstreamCode == 0 时看 err，2xx 一律判成功。这会让流中途
// 断掉的上游永远熔断不了（OutcomeSuccess 还会清零已累积的失败计数），并且因为粘性
// 复用同一判据，会把会话钉在坏上游上。
func TestBreakerOutcomeForCountsPost2xxFailures(t *testing.T) {
	cases := []struct {
		name string
		code int
		err  error
		want breaker.Outcome
	}{
		{"2xx 干净结束", 200, nil, breaker.OutcomeSuccess},
		{"2xx 后流被掐断", 200, io.ErrUnexpectedEOF, breaker.OutcomeFailure},
		{"2xx 后活跃超时", 200, context.DeadlineExceeded, breaker.OutcomeFailure},
		{"2xx 后响应转换失败", 200, errors.New("响应转换失败"), breaker.OutcomeFailure},
		{"2xx 后客户端自己断开", 200, context.Canceled, breaker.OutcomeIgnored},
		{"3xx 后失败", 302, io.ErrUnexpectedEOF, breaker.OutcomeFailure},
		{"5xx", 502, nil, breaker.OutcomeFailure},
		{"429 不计入", 429, nil, breaker.OutcomeIgnored},
		{"401 不计入", 401, nil, breaker.OutcomeIgnored},
		{"普通 4xx 不计入", 400, nil, breaker.OutcomeIgnored},
		{"无响应头 + 传输错误", 0, io.ErrUnexpectedEOF, breaker.OutcomeFailure},
		{"无响应头 + 客户端断开", 0, context.Canceled, breaker.OutcomeIgnored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := breakerOutcomeFor(tc.code, tc.err); got != tc.want {
				t.Errorf("breakerOutcomeFor(%d, %v) = %v, 期望 %v", tc.code, tc.err, got, tc.want)
			}
		})
	}
}
