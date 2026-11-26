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

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/metrics"
)

func TestUntilDisconnectRetriesPastMaxRetriesWithoutFailover(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if primaryHits.Add(1) < 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer backup.Close()

	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "primary", Model: "primary-model", LocalRetry: continuousRetryPolicy(1, 2)},
		{Provider: "backup", Model: "backup-model"},
	}}
	srv := newFailoverTestServer(map[string]*config.Provider{
		"primary": localRetryProvider("primary", primary.URL),
		"backup":  localRetryProvider("backup", backup.URL),
	}, route, primary.Client(), defaultTestFailover())

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || primaryHits.Load() != 4 || backupHits.Load() != 0 {
		t.Fatalf("status/primary/backup = %d/%d/%d, 期望同候选第四次成功且不转移", response.Code, primaryHits.Load(), backupHits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 4 {
		t.Fatalf("Attempts/details = %d/%d, 期望候选尝试 1、HTTP 明细 4", entry.Attempts, len(entry.AttemptDetails))
	}
	for i, detail := range entry.AttemptDetails {
		if detail.LocalRetry != (i > 0) {
			t.Errorf("第 %d 次 HTTP 请求 LocalRetry=%t", i+1, detail.LocalRetry)
		}
	}
}

func TestUntilDisconnectDoesNotLoopOrTransferPermanentClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var primaryHits, backupHits atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryHits.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"permanent"}`)
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backupHits.Add(1)
				w.Header().Set("content-type", "application/json")
				_, _ = io.WriteString(w, anthropicOKBody)
			}))
			defer backup.Close()

			route := config.Route{Match: "*", Targets: []config.Target{
				{Provider: "primary", Model: "primary-model", LocalRetry: continuousRetryPolicy(20, 1)},
				{Provider: "backup", Model: "backup-model"},
			}}
			failover := defaultTestFailover()
			on := true
			failover.OnClientError = &on
			failover.OnAuthError = &on
			srv := newFailoverTestServer(map[string]*config.Provider{
				"primary": localRetryProvider("primary", primary.URL),
				"backup":  localRetryProvider("backup", backup.URL),
			}, route, primary.Client(), failover)

			response := postInference(srv, "/v1/messages", anthropicReqBody)
			if response.Code != status || primaryHits.Load() != 1 || backupHits.Load() != 0 {
				t.Fatalf("status/primary/backup = %d/%d/%d, permanent error must finish at selected candidate", response.Code, primaryHits.Load(), backupHits.Load())
			}
		})
	}
}

func TestUntilDisconnectWaitsForKeyCooldownAndRetriesRecoveredKey(t *testing.T) {
	var hits atomic.Int32
	var firstAt, secondAt atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			firstAt.Store(time.Now().UnixNano())
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		secondAt.Store(time.Now().UnixNano())
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()

	cooldownMs := 35
	provider := localRetryProvider("solo", upstream.URL)
	provider.APIKeys = []config.ProviderKey{{ID: "key-a", Value: "test-key"}}
	provider.KeyCooldown = &config.KeyCooldown{RateLimitMs: &cooldownMs}
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = continuousRetryPolicy(0, 2)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": provider}, route, upstream.Client(), defaultTestFailover())
	srv.keyPool = keypool.New(nil)
	srv.keyPool.Reconcile(srv.cfg)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d, 429 冷却恢复后应重试同一密钥", response.Code, hits.Load())
	}
	if waited := time.Duration(secondAt.Load() - firstAt.Load()); waited < time.Duration(cooldownMs)*time.Millisecond {
		t.Errorf("重试间隔 %s 短于密钥冷却 %dms", waited, cooldownMs)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if len(entry.AttemptDetails) != 2 || !entry.AttemptDetails[1].LocalRetry {
		t.Errorf("冷却恢复后的本地重试日志 = %#v", entry.AttemptDetails)
	}
}

func TestUntilDisconnectWaitsForLongRetryAfterWithoutFailover(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if primaryHits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer backup.Close()

	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "primary", Model: "primary-model", LocalRetry: continuousRetryPolicy(0, 2)},
		{Provider: "backup", Model: "backup-model"},
	}}
	failover := defaultTestFailover()
	capMs := 10
	failover.MaxRetryAfterMs = &capMs
	srv := newFailoverTestServer(map[string]*config.Provider{
		"primary": localRetryProvider("primary", primary.URL),
		"backup":  localRetryProvider("backup", backup.URL),
	}, route, primary.Client(), failover)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	started := time.Now()
	srv.handle(response, request)
	if response.Code != http.StatusOK || primaryHits.Load() != 2 || backupHits.Load() != 0 {
		t.Fatalf("status/primary/backup = %d/%d/%d, 长 Retry-After 应在原候选等待后重试", response.Code, primaryHits.Load(), backupHits.Load())
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Errorf("长 Retry-After 只等待 %s，要求至少等待 1 秒恢复时间", elapsed)
	}
}

func TestClientTimeoutModeDisablesGenerationTimeoutsInDirectAndQueueModes(t *testing.T) {
	for _, direct := range []bool{true, false} {
		for _, streaming := range []bool{false, true} {
			name := "queue"
			if direct {
				name = "direct"
			}
			if streaming {
				name += "-stream"
			} else {
				name += "-nonstream"
			}
			t.Run(name, func(t *testing.T) {
				var hits atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hits.Add(1)
					time.Sleep(60 * time.Millisecond) // 超过全局、直通、头部和活跃生成预算。
					if !streaming {
						w.Header().Set("content-type", "application/json")
						_, _ = io.WriteString(w, openaiOKBody)
						return
					}
					w.Header().Set("content-type", "text/event-stream")
					flusher, ok := w.(http.Flusher)
					if !ok {
						t.Error("测试上游不支持 Flush")
						return
					}
					_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`+"\n\n")
					flusher.Flush()
					time.Sleep(60 * time.Millisecond)
					_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
				}))
				defer upstream.Close()

				route := singleCandidateRoute("solo", "upstream-model")
				route.TimeoutMode = config.TimeoutModeClient
				srv := newFailoverTestServer(map[string]*config.Provider{
					"solo": {Name: "solo", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
				}, route, upstream.Client(), config.Failover{})
				srv.cfg.DirectMode = direct
				srv.cfg.Timeout = 25
				srv.cfg.StreamActivityTimeout = 25
				srv.cfg.DirectTimeoutNoStream = 25
				srv.cfg.DirectTimeoutStreamHeader = 25
				srv.cfg.DirectTimeoutStreamActive = 25

				body := `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
				path := "/v1/messages"
				if streaming {
					body = `{"model":"client-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
				}
				response := postInference(srv, path, body)
				if response.Code != http.StatusOK || hits.Load() != 1 {
					t.Fatalf("status/hits = %d/%d, body=%s", response.Code, hits.Load(), response.Body.String())
				}
				if streaming && (strings.Contains(response.Body.String(), "timeout_error") || !strings.Contains(response.Body.String(), "hello")) {
					t.Fatalf("流式 client 模式未等待并完成跨格式聚合: %s", response.Body.String())
				}
				if !streaming && !strings.Contains(response.Body.String(), `"ok"`) {
					t.Fatalf("非流式 client 模式转换正文异常: %s", response.Body.String())
				}
				if !direct {
					status := srv.qm.StatusOf("solo", 1, 0)
					if status.Running != 0 || status.Queued != 0 {
						t.Errorf("队列模式转发结束后仍占槽: %+v", status)
					}
				}
			})
		}
	}
}

func TestClientTimeoutModeAppliesToSlowUpstreamErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":`)
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, `"invalid"}`)
	}))
	defer upstream.Close()
	route := singleCandidateRoute("solo", "upstream-model")
	route.TimeoutMode = config.TimeoutModeClient
	srv := newFailoverTestServer(map[string]*config.Provider{
		"solo": {Name: "solo", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
	}, route, upstream.Client(), config.Failover{})
	srv.cfg.Timeout = 20
	srv.cfg.DirectTimeoutNoStream = 20
	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"invalid"`) {
		t.Fatalf("status/body = %d/%s, 错误正文读取不应回退到生成超时", response.Code, response.Body.String())
	}
}

func TestUntilDisconnectRetriesUnsubmittedNonStreamBodyReadFailure(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.Header().Set("content-length", "1000")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()
	route := singleCandidateRoute("solo", "upstream-model")
	route.LocalRetry = continuousRetryPolicy(0, 1)
	srv := newFailoverTestServer(map[string]*config.Provider{
		"solo": {Name: "solo", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
	}, route, upstream.Client(), config.Failover{})
	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || hits.Load() != 2 || !strings.Contains(response.Body.String(), `"ok"`) {
		t.Fatalf("status/hits/body = %d/%d/%s, 未提交的响应正文读失败应在原候选重试", response.Code, hits.Load(), response.Body.String())
	}
}

func TestTargetGlobalTimeoutOverridesClientRouteAndLimitedModeOverridesContinuousRoute(t *testing.T) {
	t.Run("target global timeout", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(80 * time.Millisecond)
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, openaiOKBody)
		}))
		defer upstream.Close()
		route := config.Route{Match: "*", TimeoutMode: config.TimeoutModeClient, Targets: []config.Target{{
			Provider: "solo", Model: "model", TimeoutMode: config.TimeoutModeGlobal,
		}}}
		srv := newFailoverTestServer(map[string]*config.Provider{
			"solo": {Name: "solo", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
		}, route, upstream.Client(), config.Failover{})
		srv.cfg.Timeout = 20
		srv.cfg.DirectTimeoutNoStream = 20
		response := postInference(srv, "/v1/messages", anthropicReqBody)
		if response.Code != http.StatusGatewayTimeout {
			t.Fatalf("target global timeout status/body = %d/%s, 期望全局超时", response.Code, response.Body.String())
		}
	})

	t.Run("target limited retry overrides continuous route", func(t *testing.T) {
		var firstHits, backupHits atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			firstHits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		}))
		defer first.Close()
		backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			backupHits.Add(1)
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, anthropicOKBody)
		}))
		defer backup.Close()
		zero := 0
		route := config.Route{
			Match:      "*",
			LocalRetry: continuousRetryPolicy(100, 1),
			Targets: []config.Target{
				{Provider: "first", Model: "first-model", LocalRetry: &config.LocalRetry{Mode: config.LocalRetryModeLimited, MaxRetries: &zero}},
				{Provider: "backup", Model: "backup-model"},
			},
		}
		srv := newFailoverTestServer(map[string]*config.Provider{
			"first":  localRetryProvider("first", first.URL),
			"backup": localRetryProvider("backup", backup.URL),
		}, route, first.Client(), defaultTestFailover())
		response := postInference(srv, "/v1/messages", anthropicReqBody)
		if response.Code != http.StatusOK || firstHits.Load() != 1 || backupHits.Load() != 1 {
			t.Fatalf("status/first/backup = %d/%d/%d, 显式 limited 应沿有限故障转移", response.Code, firstHits.Load(), backupHits.Load())
		}
	})
}

func TestClientTimeoutModeDoesNotApplyGlobalBudgetToKeySwitch(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer first-key" {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()
	provider := &config.Provider{
		Name: "solo", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000,
		APIKeys: []config.ProviderKey{{ID: "first", Value: "first-key"}, {ID: "second", Value: "second-key"}},
	}
	route := singleCandidateRoute("solo", "upstream-model")
	route.TimeoutMode = config.TimeoutModeClient
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": provider}, route, upstream.Client(), defaultTestFailover())
	srv.cfg.Timeout = 20
	srv.cfg.DirectTimeoutNoStream = 20
	srv.keyPool = keypool.New(nil)
	srv.keyPool.Reconcile(srv.cfg)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d, client 模式下密钥切换不能受全局预算限制: %s", response.Code, hits.Load(), response.Body.String())
	}
}

func TestUntilDisconnectWaitCancellationStopsAtIntervalAndReleasesQueue(t *testing.T) {
	var hits atomic.Int32
	upstream := statusServer(http.StatusServiceUnavailable, &hits, nil)
	defer upstream.Close()
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = continuousRetryPolicy(0, 60_000)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{})
	srv.cfg.DirectMode = false

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
	for hits.Load() == 0 || srv.qm.StatusOf("solo", 1, 0).Running != 0 {
		select {
		case <-deadline:
			t.Fatal("第一次上游请求后队列执行槽没有归还")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("客户端取消后重试间隔未及时结束")
	}
	if hits.Load() != 1 {
		t.Fatalf("取消后上游命中 %d 次，期望 1", hits.Load())
	}
	if status := srv.qm.StatusOf("solo", 1, 0); status.Running != 0 || status.Queued != 0 {
		t.Errorf("取消后仍持有队列槽: %+v", status)
	}
}

func TestUntilDisconnectCooldownCancellationStopsWithoutNewRequest(t *testing.T) {
	var hits atomic.Int32
	upstream := statusServer(http.StatusTooManyRequests, &hits, nil)
	defer upstream.Close()
	cooldownMs := 60_000
	provider := localRetryProvider("solo", upstream.URL)
	provider.APIKeys = []config.ProviderKey{{ID: "key-a", Value: "test-key"}}
	provider.KeyCooldown = &config.KeyCooldown{RateLimitMs: &cooldownMs}
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = continuousRetryPolicy(0, 1)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": provider}, route, upstream.Client(), defaultTestFailover())
	srv.keyPool = keypool.New(nil)
	srv.keyPool.Reconcile(srv.cfg)

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handle(response, request)
		close(done)
	}()
	deadline := time.After(time.Second)
	for srv.keyPool.EarliestRecovery("solo", provider).IsZero() {
		select {
		case <-deadline:
			cancel()
			t.Fatal("429 后没有设置密钥冷却")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(10 * time.Millisecond) // 等待进入可取消的密钥冷却等待。
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("客户端取消后密钥冷却等待未及时结束")
	}
	if hits.Load() != 1 {
		t.Fatalf("冷却等待取消后上游命中 %d 次，期望 1", hits.Load())
	}
}

func TestUntilDisconnectClientCancellationStopsPendingNetworkRequest(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{}, 1)
	serverDone := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
		serverDone <- struct{}{}
	}))
	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = continuousRetryPolicy(0, 1)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{})
	srv.cfg.DirectMode = false

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		close(releaseHandler)
		upstream.Close()
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handle(response, request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("上游未收到第一次请求")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("挂起网络请求取消后 handler 未结束")
	}
	if hits.Load() != 1 {
		t.Fatalf("取消网络请求后又发出请求，hits=%d", hits.Load())
	}
	if status := srv.qm.StatusOf("solo", 1, 0); status.Running != 0 || status.Queued != 0 {
		t.Errorf("取消网络请求后仍持有队列槽: %+v", status)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Error("客户端取消没有传到上游请求上下文")
	}
}

func TestUntilDisconnectDoesNotRetryResponseBodyLimitFailure(t *testing.T) {
	var hits atomic.Int32
	oversizedBody := make([]byte, (32<<20)+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oversizedBody)
			return
		}
		// 旧逻辑若把本地大小保护误判为可重试，只让第二次请求返回永久错误，
		// 使回归确定失败而不重复分配 32 MiB 响应。
		http.Error(w, `{"error":"permanent"}`, http.StatusBadRequest)
	}))
	t.Cleanup(upstream.Close)

	route := singleCandidateRoute("solo", "model")
	route.Targets[0].LocalRetry = continuousRetryPolicy(0, 1)
	srv := newFailoverTestServer(map[string]*config.Provider{"solo": localRetryProvider("solo", upstream.URL)}, route, upstream.Client(), config.Failover{})
	srv.cfg.DirectMode = false
	srv.cfg.Timeout = 10_000 // 给本机 32 MiB fixture 留出充足传输时间，避免被默认 1 秒预算抢先截断。

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("终态 status = %d，期望本地响应大小保护返回 502；hits=%d body=%s", response.Code, hits.Load(), response.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("超限响应后上游请求数 = %d，期望不自动重试且仅发送 1 次", got)
	}
	if status := srv.qm.StatusOf("solo", 1, 0); status.Running != 0 || status.Queued != 0 {
		t.Errorf("超限终态后仍持有队列槽: %+v", status)
	}
}

func TestUntilDisconnectDoesNotReplayAfterStreamResponseCommit(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("content-length", "1000") // 模拟已经提交内容后上游截断流。
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"once"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, `data: [DONE]`+"\n\n")
	}))
	defer backup.Close()
	route := config.Route{Match: "*", Targets: []config.Target{
		{Provider: "primary", Model: "primary-model", LocalRetry: continuousRetryPolicy(100, 1)},
		{Provider: "backup", Model: "backup-model"},
	}}
	srv := newFailoverTestServer(map[string]*config.Provider{
		"primary": {Name: "primary", BaseURL: primary.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
		"backup":  {Name: "backup", BaseURL: backup.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000},
	}, route, primary.Client(), defaultTestFailover())
	body := `{"model":"client-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	response := postInference(srv, "/v1/messages", body)
	if response.Code != http.StatusOK || primaryHits.Load() != 1 || backupHits.Load() != 0 {
		t.Fatalf("status/primary/backup = %d/%d/%d, 提交后的流不得重放", response.Code, primaryHits.Load(), backupHits.Load())
	}
	if got := strings.Count(response.Body.String(), "once"); got != 1 {
		t.Fatalf("已提交流中的内容出现 %d 次，期望一次: %s", got, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "upstream_error") {
		t.Fatalf("截断流应向客户端报告提交后的上游失败: %s", response.Body.String())
	}
}
