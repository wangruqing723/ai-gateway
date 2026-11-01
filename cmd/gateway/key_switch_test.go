package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/breaker"
	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/metrics"
)

func newKeySwitchTestServer(providers map[string]*config.Provider, route config.Route, client *http.Client, failover config.Failover) *server {
	srv := newFailoverTestServer(providers, route, client, failover)
	srv.cfg.Timeout = 3000
	srv.cfg.DirectTimeoutNoStream = 3000
	srv.cfg.DirectTimeoutStreamHeader = 3000
	srv.keyPool = keypool.New(nil)
	srv.eventLog = metrics.NewEventLog()
	srv.keyPool.SetEventHandler(srv.onKeyPoolEvent)
	srv.keyPool.Reconcile(srv.cfg)
	return srv
}

func TestKeySwitchIgnoresFailoverAndClientAPIKey(t *testing.T) {
	var hits atomic.Int32
	var firstHeader, secondHeader atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := hits.Add(1)
		authorization := r.Header.Get("Authorization")
		if hit == 1 {
			firstHeader.Store(authorization)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"credential rejected"}`)
			return
		}
		secondHeader.Store(authorization)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()

	provider := &config.Provider{
		Name: "primary", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000,
		APIKeys: []config.ProviderKey{{ID: "main", Name: "主密钥", Value: "fake-main-key"}, {ID: "backup", Name: "备用密钥", Value: "fake-backup-key"}},
	}
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(anthropicReqBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	response := httptest.NewRecorder()
	srv.handle(response, request)

	if response.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d, 期望 failover 关闭时仍切换密钥并成功", response.Code, hits.Load())
	}
	if firstHeader.Load() != "Bearer fake-main-key" || secondHeader.Load() != "Bearer fake-backup-key" {
		t.Fatalf("上游鉴权头 = %v/%v，不能读取客户端 x-api-key", firstHeader.Load(), secondHeader.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 {
		t.Fatalf("Attempts/details = %d/%d，密钥切换不占候选额度", entry.Attempts, len(entry.AttemptDetails))
	}
	first := entry.AttemptDetails[0]
	if first.Outcome != "key_switched" || first.Reason != "401/key_auth" || first.KeyID != "main" || first.KeyName != "主密钥" || first.KeyFingerprint != keyFingerprint("fake-main-key") {
		t.Fatalf("首个密钥明细 = %+v", first)
	}
	if entry.KeySource != "provider" || entry.KeyFingerprint != keyFingerprint("fake-backup-key") {
		t.Fatalf("顶层密钥来源/指纹 = %q/%q", entry.KeySource, entry.KeyFingerprint)
	}
	if !strings.Contains(entry.AttemptTrail, "primary#main:401/key_auth") {
		t.Fatalf("AttemptTrail 未记录密钥身份: %q", entry.AttemptTrail)
	}
	if got := srv.keyPool.Snapshot(srv.cfg)["primary"][0].State; got != keypool.StateCooling {
		t.Fatalf("主密钥状态 = %s，期望 cooling", got)
	}
}

func TestKeySwitchDoesNotSwitchOnServerError(t *testing.T) {
	var hits atomic.Int32
	upstream := statusServer(http.StatusInternalServerError, &hits, nil)
	defer upstream.Close()
	provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusInternalServerError || hits.Load() != 1 {
		t.Fatalf("status/hits = %d/%d，5xx 不应切换密钥", response.Code, hits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if len(entry.AttemptDetails) != 1 || entry.AttemptDetails[0].Outcome != "final_error" {
		t.Fatalf("尝试明细 = %+v", entry.AttemptDetails)
	}
	if got := srv.keyPool.Snapshot(srv.cfg)["primary"][0].State; got != keypool.StateSelectable {
		t.Fatalf("5xx 不应冷却密钥，state=%s", got)
	}
}

func TestAllKeysFailThenProviderFailover(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"credential rejected"}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer backup.Close()
	failover := defaultTestFailover()
	failover.OnAuthError = testBoolPtr(true)
	providers := map[string]*config.Provider{
		"primary": providerWithKeys("primary", primary.URL, config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"}),
		"backup":  {Name: "backup", BaseURL: backup.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000, APIKey: "fake-backup"},
	}
	route := config.Route{Match: "*", Targets: []config.Target{{Provider: "primary", Model: "first"}, {Provider: "backup", Model: "second"}}}
	srv := newKeySwitchTestServer(providers, route, primary.Client(), failover)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || primaryHits.Load() != 2 || backupHits.Load() != 1 {
		t.Fatalf("status/hits = %d/%d/%d，主候选两份密钥失败后应故障转移", response.Code, primaryHits.Load(), backupHits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 2 || len(entry.AttemptDetails) != 3 {
		t.Fatalf("Attempts/details = %d/%d", entry.Attempts, len(entry.AttemptDetails))
	}
}

func TestLastKeyFailureKeepsUpstreamResponse(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"failed-`+r.Header.Get("Authorization")+`"}`)
	}))
	defer upstream.Close()
	provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusUnauthorized || hits.Load() != 2 || !strings.Contains(response.Body.String(), "Bearer fake-b") {
		t.Fatalf("status/hits/body = %d/%d/%s，终态应透传最后一个密钥的上游响应", response.Code, hits.Load(), response.Body.String())
	}
	if strings.Contains(response.Body.String(), "fake-a") {
		t.Fatalf("响应不应包含前一密钥请求内容: %s", response.Body.String())
	}
}

func TestSingleKeyShortRateLimitUsesLocalRetryWithoutCooldown(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"short rate limit"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()
	provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "only", Value: "fake-only"})
	route := singleCandidateRoute("primary", "upstream")
	route.Targets[0].LocalRetry = localRetryPolicy(1, 0)
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, route, upstream.Client(), config.Failover{})

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d，短暂 429 应在同一密钥本地重试", response.Code, hits.Load())
	}
	if got := srv.keyPool.Snapshot(srv.cfg)["primary"][0].State; got != keypool.StateSelectable {
		t.Fatalf("本地重试成功后密钥不应冷却，state=%s", got)
	}
}

func TestLongRateLimitSwitchesKeysAndAggregatesFreeAttempt(t *testing.T) {
	var limitedHits, backupHits atomic.Int32
	limited := statusServer(http.StatusTooManyRequests, &limitedHits, map[string]string{"Retry-After": "60"})
	defer limited.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer backup.Close()
	providers := map[string]*config.Provider{
		"limited": providerWithKeys("limited", limited.URL, config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"}),
		"backup":  {Name: "backup", BaseURL: backup.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000, APIKey: "fake-backup"},
	}
	route := config.Route{Match: "*", Targets: []config.Target{{Provider: "limited", Model: "limited"}, {Provider: "backup", Model: "backup"}}}
	srv := newKeySwitchTestServer(providers, route, limited.Client(), defaultTestFailover())

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || limitedHits.Load() != 2 || backupHits.Load() != 1 {
		t.Fatalf("status/hits = %d/%d/%d", response.Code, limitedHits.Load(), backupHits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 3 || !entry.AttemptDetails[0].FreeAttempt || !entry.AttemptDetails[1].FreeAttempt {
		t.Fatalf("候选长限流额度统计错误: Attempts=%d details=%+v", entry.Attempts, entry.AttemptDetails)
	}
}

func TestCandidateFreeAttemptRequiresEveryKeyFailureToBeLongRateLimit(t *testing.T) {
	var limitedHits, backupHits atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := limitedHits.Add(1)
		if hit == 1 {
			w.Header().Set("Retry-After", "60")
		} else {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer limited.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer backup.Close()
	providers := map[string]*config.Provider{
		"limited": providerWithKeys("limited", limited.URL, config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"}),
		"backup":  {Name: "backup", BaseURL: backup.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000, APIKey: "fake-backup"},
	}
	route := config.Route{Match: "*", Targets: []config.Target{{Provider: "limited", Model: "limited"}, {Provider: "backup", Model: "backup"}}}
	srv := newKeySwitchTestServer(providers, route, limited.Client(), defaultTestFailover())

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusOK || limitedHits.Load() != 2 || backupHits.Load() != 1 {
		t.Fatalf("status/hits = %d/%d/%d", response.Code, limitedHits.Load(), backupHits.Load())
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 2 || len(entry.AttemptDetails) != 3 || !entry.AttemptDetails[0].FreeAttempt || entry.AttemptDetails[1].FreeAttempt {
		t.Fatalf("混合长短限流错误占用额度: Attempts=%d details=%+v", entry.Attempts, entry.AttemptDetails)
	}
}

func TestNoSelectableKeySkipsBreakerAndReturnsRetryAfter(t *testing.T) {
	provider := providerWithKeys("primary", "http://127.0.0.1:1", config.ProviderKey{ID: "a", Value: "fake-a"}, config.ProviderKey{ID: "b", Value: "fake-b"})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), http.DefaultClient, config.Failover{})
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired} {
		ticket, _ := srv.keyPool.Select("primary", provider, nil)
		if ticket == nil {
			t.Fatal("预热冷却状态时没有选择到下一份密钥")
		}
		srv.keyPool.Report(ticket, status, 0)
	}
	srv.breaker = breaker.New(breaker.Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 1, HalfOpenProbes: 1})
	srv.breaker.Report("primary", breaker.OutcomeFailure)
	time.Sleep(5 * time.Millisecond)
	allowed, _ := srv.breaker.Allow("primary")
	if !allowed {
		t.Fatal("熔断器没有进入半开状态")
	}
	srv.breaker.Report("primary", breaker.OutcomeIgnored)

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "no_selectable_provider_key") {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	if retryAfter, err := strconv.Atoi(response.Header().Get("retry-after")); err != nil || retryAfter < 1 {
		t.Fatalf("retry-after = %q，期望至少 1 秒", response.Header().Get("retry-after"))
	}
	state := srv.breaker.Snapshot()["primary"]
	if state.State != breaker.StateHalfOpen || state.ProbesInFlight != 0 {
		t.Fatalf("无可选密钥仍借出了熔断探针: %+v", state)
	}
	entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
	if entry.Attempts != 0 || len(entry.AttemptDetails) != 1 || entry.AttemptDetails[0].Kind != "key_skip" {
		t.Fatalf("无可选密钥日志错误: %+v", entry)
	}
}

func TestStreamPrereadRateLimitSwitchesKeyAndCommittedStreamDoesNot(t *testing.T) {
	t.Run("预读阶段切换", func(t *testing.T) {
		var hits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			if r.Header.Get("x-api-key") == "fake-main" {
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"upstream\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"limited\"}}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			for _, line := range []string{
				`data: {"type":"message_start","message":{"id":"m2","model":"upstream","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`data: {"type":"message_stop"}`,
			} {
				_, _ = io.WriteString(w, line+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		}))
		defer upstream.Close()
		provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "main", Value: "fake-main"}, config.ProviderKey{ID: "spare", Value: "fake-spare"})
		provider.Format = "anthropic"
		srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})
		response := postInference(srv, "/v1/messages", `{"model":"client","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
		if response.Code != http.StatusOK || hits.Load() != 2 || len(entry.AttemptDetails) != 2 || entry.AttemptDetails[0].Outcome != "key_switched" || entry.AttemptDetails[0].Reason != "429/key_rate_limit" {
			t.Fatalf("预读切换 status/hits/log = %d/%d/%+v", response.Code, hits.Load(), entry.AttemptDetails)
		}
	})

	t.Run("响应提交后不切换", func(t *testing.T) {
		var hits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for _, event := range []string{
				`data: {"type":"message_start","message":{"id":"m1","model":"upstream","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
				`event: error`,
				`data: {"type":"error","error":{"type":"rate_limit_error","message":"late limit"}}`,
			} {
				_, _ = io.WriteString(w, event+"\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		}))
		defer upstream.Close()
		provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "main", Value: "fake-main"}, config.ProviderKey{ID: "spare", Value: "fake-spare"})
		provider.Format = "anthropic"
		srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})
		response := postInference(srv, "/v1/messages", `{"model":"client","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
		if hits.Load() != 1 || len(entry.AttemptDetails) != 1 || entry.AttemptDetails[0].Outcome == "key_switched" {
			t.Fatalf("已提交响应后发生密钥切换: status=%d hits=%d detail=%+v", response.Code, hits.Load(), entry.AttemptDetails)
		}
	})
}

func TestKeySwitchChainSharesUpstreamTimeout(t *testing.T) {
	var hits atomic.Int32
	spareDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			time.Sleep(500 * time.Millisecond)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		close(spareDone)
	}))
	defer upstream.Close()
	provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "main", Value: "fake-main"}, config.ProviderKey{ID: "spare", Value: "fake-spare"})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})
	srv.cfg.Timeout = 2500
	srv.cfg.DirectTimeoutNoStream = 2500

	started := time.Now()
	_ = postInference(srv, "/v1/messages", anthropicReqBody)
	elapsed := time.Since(started)
	if hits.Load() != 2 {
		t.Fatalf("密钥切换未请求备用密钥: hits=%d", hits.Load())
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("密钥切换链重置了超时预算: 总耗时 %s", elapsed)
	}
	select {
	case <-spareDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("备用请求未在共享预算耗尽后退出")
	}
}

func TestForwardAttemptTimeoutBudgetPreservesDirectModes(t *testing.T) {
	cfg := &config.Config{
		Timeout:                   5000,
		StreamActivityTimeout:     4000,
		DirectMode:                true,
		DirectTimeoutNoStream:     700,
		DirectTimeoutStreamHeader: 900,
		DirectTimeoutStreamActive: 1100,
	}
	tests := []struct {
		name      string
		streaming bool
		keyList   bool
		chainMs   int
		want      [3]int
	}{
		{name: "旧单值流式不覆盖直通超时", streaming: true, chainMs: 250, want: [3]int{700, 900, 1100}},
		{name: "列表流式只覆盖响应头超时", streaming: true, keyList: true, chainMs: 250, want: [3]int{700, 250, 1100}},
		{name: "列表非流式覆盖整体超时", keyList: true, chainMs: 250, want: [3]int{250, 900, 1100}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout, headerTimeout, activityTimeout := forwardAttemptTimeouts(cfg, tt.streaming, tt.keyList, tt.chainMs)
			got := [3]int{timeout, headerTimeout, activityTimeout}
			if got != tt.want {
				t.Fatalf("超时参数 = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestKeySwitchRetryAfterOnlyFreesLong429(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "401 不免额", status: http.StatusUnauthorized},
		{name: "402 不免额", status: http.StatusPaymentRequired},
		{name: "403 不免额", status: http.StatusForbidden},
		{name: "长 429 免额", status: http.StatusTooManyRequests, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, `{"error":"credential response"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, openaiOKBody)
			}))
			defer upstream.Close()
			provider := providerWithKeys("primary", upstream.URL,
				config.ProviderKey{ID: "main", Value: "fake-main"},
				config.ProviderKey{ID: "spare", Value: "fake-spare"},
			)
			srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, singleCandidateRoute("primary", "upstream"), upstream.Client(), config.Failover{})

			response := postInference(srv, "/v1/messages", anthropicReqBody)
			entry := srv.metrics.Logs(metrics.LogFilter{Limit: 1})[0]
			if response.Code != http.StatusOK || hits.Load() != 2 || len(entry.AttemptDetails) != 2 {
				t.Fatalf("响应/命中/明细 = %d/%d/%d，期望 200/2/2", response.Code, hits.Load(), len(entry.AttemptDetails))
			}
			if got := entry.AttemptDetails[0].FreeAttempt; got != tt.want {
				t.Fatalf("首个密钥的 FreeAttempt = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestKeyListAuthFailureDoesNotUseLocalRetry(t *testing.T) {
	var hits atomic.Int32
	upstream := statusServer(http.StatusUnauthorized, &hits, nil)
	defer upstream.Close()
	provider := providerWithKeys("primary", upstream.URL, config.ProviderKey{ID: "only", Value: "fake-only"})
	route := singleCandidateRoute("primary", "upstream")
	route.Targets[0].LocalRetry = localRetryPolicy(3, 0)
	srv := newKeySwitchTestServer(map[string]*config.Provider{"primary": provider}, route, upstream.Client(), defaultTestFailover())

	response := postInference(srv, "/v1/messages", anthropicReqBody)
	if response.Code != http.StatusUnauthorized || hits.Load() != 1 {
		t.Fatalf("status/hits = %d/%d，密钥鉴权失败不应本地重试", response.Code, hits.Load())
	}
}

func providerWithKeys(name, baseURL string, keys ...config.ProviderKey) *config.Provider {
	return &config.Provider{Name: name, BaseURL: baseURL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000, APIKeys: keys}
}
