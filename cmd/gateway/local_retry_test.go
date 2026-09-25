package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/breaker"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/queue"
)

func localRetryPolicy(maxRetries, intervalMs int) *config.LocalRetry {
	return &config.LocalRetry{MaxRetries: &maxRetries, IntervalMs: &intervalMs}
}

func localRetryProvider(name, baseURL string) *config.Provider {
	return &config.Provider{Name: name, BaseURL: baseURL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
}

func TestLocalRetrySucceedsOnNthAttemptWithoutConsumingFailoverAttempt(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if primaryHits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer backup.Close()

	maxAttempts := 1
	failover := defaultTestFailover()
	failover.MaxAttempts = &maxAttempts
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "primary", Model: "primary-model", LocalRetry: localRetryPolicy(2, 0)},
		{Provider: "backup", Model: "backup-model"},
	}}
	srv := newFailoverTestServer(map[string]*config.Provider{
		"primary": localRetryProvider("primary", primary.URL),
		"backup":  localRetryProvider("backup", backup.URL),
	}, route, primary.Client(), failover)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望第三次本地尝试成功", response.Code, response.Body.String())
	}
	if primaryHits.Load() != 3 || backupHits.Load() != 0 {
		t.Fatalf("上游命中 primary=%d backup=%d, 期望 3/0", primaryHits.Load(), backupHits.Load())
	}
	if got := response.Header().Get("x-ai-gateway-attempts"); got != "1" {
		t.Errorf("x-ai-gateway-attempts = %q, 本地重试不应增加 failover 尝试号", got)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 3 {
		t.Fatalf("Attempts/details = %d/%d, 期望 1/3", entry.Attempts, len(entry.AttemptDetails))
	}
	if entry.AttemptDetails[0].LocalRetry || !entry.AttemptDetails[1].LocalRetry || !entry.AttemptDetails[2].LocalRetry {
		t.Errorf("local retry 标记不正确: %#v", entry.AttemptDetails)
	}
	for i, detail := range entry.AttemptDetails {
		if detail.AttemptNumber != i+1 {
			t.Errorf("第 %d 条 detail.AttemptNumber = %d, 期望 HTTP 尝试号 %d", i+1, detail.AttemptNumber, i+1)
		}
	}
}

func TestLocalRetryAttemptsDoNotConsumeMaxAttemptsBeforeFailover(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := statusServer(http.StatusServiceUnavailable, &primaryHits, nil)
	defer primary.Close()
	backup := statusServer(http.StatusOK, &backupHits, nil)
	defer backup.Close()

	maxAttempts := 2
	failover := defaultTestFailover()
	failover.MaxAttempts = &maxAttempts
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "primary", Model: "primary-model", LocalRetry: localRetryPolicy(2, 0)},
		{Provider: "backup", Model: "backup-model"},
	}}
	srv := newFailoverTestServer(map[string]*config.Provider{
		"primary": localRetryProvider("primary", primary.URL),
		"backup":  localRetryProvider("backup", backup.URL),
	}, route, primary.Client(), failover)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望 failover 候选成功", response.Code, response.Body.String())
	}
	if primaryHits.Load() != 3 || backupHits.Load() != 1 {
		t.Fatalf("上游命中 primary=%d backup=%d, 期望 3/1", primaryHits.Load(), backupHits.Load())
	}
	if got := response.Header().Get("x-ai-gateway-attempts"); got != "2" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望本地重试后的第二候选编号 2", got)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 2 || len(entry.AttemptDetails) != 4 {
		t.Fatalf("Attempts/details = %d/%d, 期望 2/4", entry.Attempts, len(entry.AttemptDetails))
	}
	if entry.AttemptDetails[0].AttemptNumber != 1 || entry.AttemptDetails[1].AttemptNumber != 2 || entry.AttemptDetails[2].AttemptNumber != 3 || entry.AttemptDetails[3].AttemptNumber != 4 {
		t.Errorf("HTTP 尝试号应逐次递增: %#v", entry.AttemptDetails)
	}
	if entry.AttemptDetails[3].Provider != "backup" || entry.AttemptDetails[3].LocalRetry {
		t.Errorf("failover detail 应属于非本地重试的 backup: %#v", entry.AttemptDetails[3])
	}
}

func TestLocalRetryReportsBreakerOnceWithFinalResult(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = localRetryPolicy(1, 0)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{})
	transitions := make(chan [2]string, 4)
	srv.breaker = breaker.New(breaker.Settings{
		Enabled: true, ConsecutiveFailures: 1, OpenMs: 1, HalfOpenProbes: 1,
		OnStateChange: func(_, from, to string) { transitions <- [2]string{from, to} },
	})
	srv.breaker.Report("solo", breaker.OutcomeFailure)
	<-transitions // 清除初始 closed -> open 状态变化。
	time.Sleep(10 * time.Millisecond)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s, 期望半开探针本地重试成功", response.Code, response.Body.String())
	}
	select {
	case transition := <-transitions:
		if transition != [2]string{breaker.StateHalfOpen, breaker.StateClosed} {
			t.Fatalf("breaker transition = %v, 期望单次最终成功 half_open -> closed", transition)
		}
	case <-time.After(time.Second):
		t.Fatal("本地重试成功后没有收到 breaker 恢复状态变化")
	}
	select {
	case transition := <-transitions:
		t.Fatalf("单个候选重试期间 breaker 被重复上报，额外状态变化 %v", transition)
	default:
	}
	if state := srv.breaker.Snapshot()["solo"].State; state != breaker.StateClosed {
		t.Errorf("breaker state = %q, 期望 closed", state)
	}
}

func TestLocalRetryDoesNotRetryFreeAttemptAndFailovers(t *testing.T) {
	var limitedHits, backupHits atomic.Int32
	limited := statusServer(http.StatusTooManyRequests, &limitedHits, map[string]string{"Retry-After": "60"})
	defer limited.Close()
	backup := statusServer(http.StatusOK, &backupHits, nil)
	defer backup.Close()
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "limited", Model: "limited-model", LocalRetry: localRetryPolicy(3, 0)},
		{Provider: "backup", Model: "backup-model"},
	}}
	srv := newFailoverTestServer(map[string]*config.Provider{
		"limited": localRetryProvider("limited", limited.URL),
		"backup":  localRetryProvider("backup", backup.URL),
	}, route, limited.Client(), defaultTestFailover())

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || limitedHits.Load() != 1 || backupHits.Load() != 1 {
		t.Fatalf("response=%d hits limited/backup=%d/%d, 期望 freeAttempt 不本地重试并转移", response.Code, limitedHits.Load(), backupHits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 || !entry.AttemptDetails[0].FreeAttempt || entry.AttemptDetails[0].LocalRetry {
		t.Errorf("429 freeAttempt 日志不符合预期: %#v", entry)
	}
}

func TestLocalRetryWorksWhenFailoverDisabled(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = localRetryPolicy(1, 0)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{Enabled: false})

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d, 期望 failover 关闭时仍本地重试成功", response.Code, hits.Load())
	}
	if got := response.Header().Get("x-ai-gateway-attempts"); got != "1" {
		t.Errorf("x-ai-gateway-attempts = %q, 期望 1", got)
	}
}

func TestClassifyFailureQueueTimeoutIgnoresFailoverEnabledGate(t *testing.T) {
	on := true
	failover := config.Failover{Enabled: false, OnQueueTimeout: &on}
	decision := classifyFailure(&failover, 0, 0, queue.ErrQueueTimeout)
	if !decision.transfer || decision.reason != "queue_timeout" {
		t.Fatalf("classifyFailure() = %#v, 期望独立于 failover.enabled 分类队列超时", decision)
	}
	if failoverReason(&failover, 0, 0, queue.ErrQueueTimeout).transfer {
		t.Fatal("failoverReason() 在 failover.enabled=false 时仍应阻止候选转移")
	}
}

func TestLocalRetryWaitStopsWhenClientDisconnects(t *testing.T) {
	var hits atomic.Int32
	upstream := statusServer(http.StatusServiceUnavailable, &hits, nil)
	defer upstream.Close()
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = localRetryPolicy(3, 60_000)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handle(response, request)
		close(done)
	}()
	deadline := time.After(time.Second)
	for hits.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("客户端请求尚未触发第一次上游调用")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// 留出时间让 503 被识别并进入较长的本地重试等待。
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消客户端请求后，本地重试等待没有及时退出")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("客户端断开后上游命中 %d 次，期望停止在第一次", got)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if len(entry.AttemptDetails) != 1 || !strings.Contains(entry.AttemptTrail, "local_retry") {
		t.Errorf("请求没有在重试等待期间退出: %#v", entry)
	}
}
