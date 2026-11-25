package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"ai-gateway/internal/balancer"
	"ai-gateway/internal/breaker"
	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/router"
)

const debugConsoleStreamBody = "data: {\"id\":\"chunk-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"

type debugConsoleSSEEvent struct {
	Name string
	Data json.RawMessage
}

func debugConsoleEncodeValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("编码调试请求失败: %v", err))
	}
	return string(encoded)
}

func debugConsoleEncodeRequest(t *testing.T, value any) string {
	t.Helper()
	return debugConsoleEncodeValue(value)
}

func debugConsoleCall(srv *server, method, path, body, contentType, origin string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "http://127.0.0.1:7789"+path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	srv.handle(recorder, request)
	return recorder
}

func debugConsoleRun(srv *server, fields map[string]any) *httptest.ResponseRecorder {
	return debugConsoleCall(srv, http.MethodPost, "/api/debug/run", debugConsoleEncodeValue(fields), "application/json", "")
}

func debugConsoleReadEvents(t *testing.T, body string) []debugConsoleSSEEvent {
	t.Helper()
	events := make([]debugConsoleSSEEvent, 0)
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if block == "" {
			continue
		}
		var event debugConsoleSSEEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
		if event.Name == "" || !json.Valid(event.Data) {
			t.Fatalf("SSE 事件无效: %q", block)
		}
		events = append(events, event)
	}
	return events
}

func debugConsoleEvent(events []debugConsoleSSEEvent, name string, occurrence int) (debugConsoleSSEEvent, bool) {
	for _, event := range events {
		if event.Name != name {
			continue
		}
		if occurrence == 0 {
			return event, true
		}
		occurrence--
	}
	return debugConsoleSSEEvent{}, false
}

func debugConsoleChunks(t *testing.T, events []debugConsoleSSEEvent) string {
	t.Helper()
	var joined strings.Builder
	for _, event := range events {
		if event.Name != debugChunkEvent {
			continue
		}
		var chunk debugResponseChunk
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			t.Fatalf("解析 response_chunk 失败: %v", err)
		}
		if !utf8.ValidString(chunk.Data) {
			t.Fatalf("response_chunk 不是合法 UTF-8: %q", chunk.Data)
		}
		joined.WriteString(chunk.Data)
	}
	return joined.String()
}

func debugConsoleDoneLog(t *testing.T, events []debugConsoleSSEEvent) debugDone {
	t.Helper()
	event, ok := debugConsoleEvent(events, debugDoneEvent, 0)
	if !ok {
		t.Fatal("缺少 done 事件")
	}
	var done debugDone
	if err := json.Unmarshal(event.Data, &done); err != nil {
		t.Fatalf("解析 done 事件失败: %v", err)
	}
	return done
}

func debugConsoleGetMetrics(t *testing.T, srv *server) metrics.Response {
	t.Helper()
	recorder := debugConsoleCall(srv, http.MethodGet, "/api/metrics", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/metrics status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response metrics.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析 /api/metrics 失败: %v", err)
	}
	return response
}

func debugConsoleGetLogs(t *testing.T, srv *server, query string) []metrics.RequestLog {
	t.Helper()
	recorder := debugConsoleCall(srv, http.MethodGet, "/api/logs"+query, "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/logs status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []metrics.RequestLog `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析 /api/logs 失败: %v", err)
	}
	return response.Data
}

func TestDebugConsoleProviderModeStreamsAndRedactsCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, debugConsoleStreamBody)
	}))
	defer upstream.Close()
	provider := &config.Provider{Name: "direct", BaseURL: upstream.URL, APIKey: "debug-secret-key", Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"direct": provider}, config.Route{Match: "*", Provider: "direct"}, upstream.Client(), config.Failover{})
	requestBody := map[string]any{"model": "direct-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "你好"}}}
	response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "direct", "body": requestBody})
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("调试响应 status/content-type = %d/%q", response.Code, response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", response.Header().Get("Cache-Control"))
	}
	if strings.Contains(response.Body.String(), provider.APIKey) {
		t.Fatalf("调试事件流泄漏了密钥原文: %s", response.Body.String())
	}
	events := debugConsoleReadEvents(t, response.Body.String())
	wantPrefix := []string{debugMetaEvent, debugUpstreamEvent, debugResponseEvent, debugChunkEvent}
	if len(events) < len(wantPrefix)+1 {
		t.Fatalf("事件不足: %v", events)
	}
	for i, want := range wantPrefix {
		if events[i].Name != want {
			t.Fatalf("事件[%d] = %q，期望 %q", i, events[i].Name, want)
		}
	}
	if events[len(events)-1].Name != debugDoneEvent {
		t.Fatalf("最后事件 = %q，期望 done", events[len(events)-1].Name)
	}
	var upstreamRequest debugUpstreamRequest
	if err := json.Unmarshal(events[1].Data, &upstreamRequest); err != nil {
		t.Fatalf("解析 upstream_request 失败: %v", err)
	}
	if upstreamRequest.Sequence != 1 || upstreamRequest.Provider != "direct" || upstreamRequest.TargetModel != "direct-model" {
		t.Errorf("upstream_request = %#v", upstreamRequest)
	}
	if upstreamRequest.Headers["Authorization"] != debugRedactedValue {
		t.Errorf("Authorization 没有脱敏: %#v", upstreamRequest.Headers)
	}
	var start debugResponseStart
	if err := json.Unmarshal(events[2].Data, &start); err != nil {
		t.Fatalf("解析 response_start 失败: %v", err)
	}
	if start.Status != http.StatusOK {
		t.Errorf("response_start.status = %d", start.Status)
	}
	if !strings.Contains(debugConsoleChunks(t, events), `"content":"你好"`) {
		t.Errorf("流式响应分片缺少上游正文: %q", debugConsoleChunks(t, events))
	}
	done := debugConsoleDoneLog(t, events)
	if !done.Log.Debug || done.Log.DebugMode != "provider" || done.Log.Path != "/api/debug/run" || done.Log.StartedAt == "" || done.Aborted {
		t.Errorf("done.log = %#v, aborted=%t", done.Log, done.Aborted)
	}
}

func TestDebugConsoleChunksMatchRealRouteResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()
	provider := &config.Provider{Name: "primary", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"primary": provider}, config.Route{Match: "client-*", Targets: []config.Target{{Provider: "primary", Model: "upstream-model"}}}, upstream.Client(), config.Failover{})
	body := `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	real := postInference(srv, "/v1/messages", body)
	if real.Code != http.StatusOK {
		t.Fatalf("真实链路 status/body = %d/%s", real.Code, real.Body.String())
	}
	var requestBody map[string]any
	if err := json.Unmarshal([]byte(body), &requestBody); err != nil {
		t.Fatal(err)
	}
	debug := debugConsoleRun(srv, map[string]any{
		"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "client-*", "body": requestBody,
	})
	events := debugConsoleReadEvents(t, debug.Body.String())
	got := debugConsoleChunks(t, events)
	if got != real.Body.String() {
		t.Fatalf("拼接 response_chunk 与真实响应正文不同:\n调试: %q\n真实: %q", got, real.Body.String())
	}
	var meta debugMeta
	metaEvent, ok := debugConsoleEvent(events, debugMetaEvent, 0)
	if !ok {
		t.Fatal("缺少 meta 事件")
	}
	if err := json.Unmarshal(metaEvent.Data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Mode != "route" || meta.Route != "client-*" || len(meta.Candidates) != 1 || meta.Candidates[0].TargetModel != "upstream-model" {
		t.Errorf("route meta = %#v", meta)
	}
	if done := debugConsoleDoneLog(t, events); !done.Log.Debug || done.Log.DebugMode != "route" {
		t.Errorf("route done.log 未带调试标记: %#v", done.Log)
	}
}

func TestDebugConsoleRouteFailoverAndLocalRetry(t *testing.T) {
	t.Run("故障转移照常并上报熔断", func(t *testing.T) {
		var firstHits, secondHits atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			firstHits.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			secondHits.Add(1)
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, anthropicOKBody)
		}))
		defer second.Close()
		providers := map[string]*config.Provider{
			"first":  {Name: "first", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
			"second": {Name: "second", BaseURL: second.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		}
		route := config.Route{Match: "*", Targets: []config.Target{{Provider: "first", Model: "m1"}, {Provider: "second", Model: "m2"}}}
		srv := newFailoverTestServer(providers, route, first.Client(), defaultTestFailover())
		srv.breaker = breaker.New(breaker.Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 60_000, HalfOpenProbes: 1})
		response := debugConsoleRun(srv, map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "*", "body": map[string]any{"model": "client", "messages": []any{}}})
		if response.Code != http.StatusOK || firstHits.Load() != 1 || secondHits.Load() != 1 {
			t.Fatalf("status/hits = %d/%d/%d, 期望转移后成功", response.Code, firstHits.Load(), secondHits.Load())
		}
		entry := debugConsoleDoneLog(t, debugConsoleReadEvents(t, response.Body.String())).Log
		if entry.Attempts != 2 || len(entry.AttemptDetails) != 2 || entry.AttemptDetails[0].Outcome != "transferred" {
			t.Fatalf("route 故障转移日志 = %#v", entry)
		}
		if state := srv.breaker.Snapshot()["first"]; state.State != breaker.StateOpen || state.ConsecutiveFailures != 1 {
			t.Errorf("失败 Provider 熔断状态 = %+v", state)
		}
	})

	t.Run("本地重试照常", func(t *testing.T) {
		var hits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"temporary"}`)
				return
			}
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, anthropicOKBody)
		}))
		defer upstream.Close()
		route := config.Route{Match: "*", Targets: []config.Target{{Provider: "primary", Model: "upstream", LocalRetry: localRetryPolicy(1, 0)}}}
		provider := &config.Provider{Name: "primary", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
		failover := defaultTestFailover()
		failover.Enabled = false
		srv := newFailoverTestServer(map[string]*config.Provider{"primary": provider}, route, upstream.Client(), failover)
		response := debugConsoleRun(srv, map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "*", "body": map[string]any{"model": "client", "messages": []any{}}})
		if response.Code != http.StatusOK || hits.Load() != 2 {
			t.Fatalf("status/hits = %d/%d, 期望本地重试后成功", response.Code, hits.Load())
		}
		entry := debugConsoleDoneLog(t, debugConsoleReadEvents(t, response.Body.String())).Log
		if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 || !entry.AttemptDetails[1].LocalRetry {
			t.Errorf("本地重试日志 = %#v", entry)
		}
	})
}

func TestDebugConsoleTargetNarrowsWithoutRotationAndAppliesOverrides(t *testing.T) {
	var firstHits, selectedHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer first.Close()
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := selectedHits.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析上游请求体失败: %v", err)
		}
		if body["model"] != "selected-model" || body["max_tokens"] != float64(55) || body["providerExtra"] != "provider" || body["routeExtra"] != "route" || body["choice"] != "target" {
			t.Errorf("候选覆盖未生效: %#v", body)
		}
		if hit == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer selected.Close()

	providerWindow, routeWindow, targetWindow := 1024, 2048, 32_000
	providerTokens, routeTokens, targetTokens := 88, 77, 55
	provider := &config.Provider{
		Name:          "selected",
		BaseURL:       selected.URL,
		Format:        "anthropic",
		MaxConcurrent: 1,
		MaxQueueWait:  1000,
		ContextWindow: &providerWindow,
		MaxTokens:     &providerTokens,
		ExtraBody:     map[string]any{"providerExtra": "provider", "choice": "provider"},
	}
	other := &config.Provider{Name: "first", BaseURL: first.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	route := config.Route{
		Match:         "*",
		Strategy:      balancer.StrategyRoundRobin,
		ContextWindow: &routeWindow,
		MaxTokens:     &routeTokens,
		ExtraBody:     map[string]any{"routeExtra": "route", "choice": "route"},
		Targets: []config.Target{
			{Provider: "first", Model: "first-model"},
			{
				Provider:      "selected",
				Model:         "selected-model",
				ContextWindow: &targetWindow,
				MaxTokens:     &targetTokens,
				LocalRetry:    localRetryPolicy(1, 0),
				ExtraBody:     map[string]any{"choice": "target"},
			},
		},
	}
	srv := newStrategyTestServer(map[string]*config.Provider{"first": other, "selected": provider}, route, selected.Client(), defaultTestFailover())
	seedSelector(t, srv.selector, "*")
	countersBefore, stickyBefore := srv.selector.RouteCounters(), srv.selector.StickyMappings()
	largeText := strings.Repeat("hello ", 5_000)
	body := map[string]any{"model": "client-model", "max_tokens": 3, "messages": []any{map[string]any{"role": "user", "content": largeText}}}
	response := debugConsoleRun(srv, map[string]any{
		"mode": "target", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "*", "targetIndex": 1, "targetProvider": "selected", "body": body,
	})
	if response.Code != http.StatusOK || firstHits.Load() != 0 || selectedHits.Load() != 2 {
		t.Fatalf("status/hits = %d/%d/%d, 只应重试所选候选", response.Code, firstHits.Load(), selectedHits.Load())
	}
	if got := srv.selector.RouteCounters(); got != countersBefore {
		t.Errorf("target 模式推进了轮转计数: %d -> %d", countersBefore, got)
	}
	if got := srv.selector.StickyMappings(); got != stickyBefore {
		t.Errorf("target 模式修改了会话粘性数: %d -> %d", stickyBefore, got)
	}
	events := debugConsoleReadEvents(t, response.Body.String())
	var meta debugMeta
	metaEvent, _ := debugConsoleEvent(events, debugMetaEvent, 0)
	if err := json.Unmarshal(metaEvent.Data, &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Candidates) != 1 || meta.Candidates[0].Provider != "selected" || meta.Strategy != "failover" {
		t.Errorf("target meta = %#v", meta)
	}
	entry := debugConsoleDoneLog(t, events).Log
	if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 || !entry.AttemptDetails[1].LocalRetry || entry.EstimatedInputTokens <= 1024 {
		t.Errorf("target 覆盖/重试日志 = %#v", entry)
	}
}

func TestDebugConsoleRouteMismatchReportsActualRoute(t *testing.T) {
	first := &config.Provider{Name: "first", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	second := &config.Provider{Name: "second", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"first": first, "second": second}, config.Route{Match: "claude-*", Provider: "first"}, http.DefaultClient, config.Failover{})
	srv.cfg.Routes = append(srv.cfg.Routes, config.Route{Match: "claude-opus-*", Provider: "second"})
	request := map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 1, "routeMatch": "claude-opus-*", "body": map[string]any{"model": "claude-opus-4", "messages": []any{}}}
	response := debugConsoleRun(srv, request)
	events := debugConsoleReadEvents(t, response.Body.String())
	startEvent, ok := debugConsoleEvent(events, debugResponseEvent, 0)
	if !ok {
		t.Fatal("路由不符缺少 response_start")
	}
	var start debugResponseStart
	if err := json.Unmarshal(startEvent.Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Status != http.StatusUnprocessableEntity {
		t.Fatalf("response_start.status = %d，期望 422", start.Status)
	}
	var chunk debugResponseChunk
	chunkEvent, ok := debugConsoleEvent(events, debugChunkEvent, 0)
	if !ok {
		t.Fatal("路由不符缺少 response_chunk")
	}
	if err := json.Unmarshal(chunkEvent.Data, &chunk); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chunk.Data, "debug_route_mismatch") || !strings.Contains(chunk.Data, "实际命中路由 match") || !strings.Contains(chunk.Data, "claude-*") {
		t.Fatalf("路由不符正文未说明实际 match: %s", chunk.Data)
	}
	metaEvent, _ := debugConsoleEvent(events, debugMetaEvent, 0)
	var meta debugMeta
	if err := json.Unmarshal(metaEvent.Data, &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Candidates) != 0 || meta.Route != "claude-*" {
		t.Errorf("路由不符 meta = %#v", meta)
	}
}

func TestDebugConsoleRouteNotFoundUsesContractErrorType(t *testing.T) {
	provider := &config.Provider{Name: "primary", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"primary": provider}, config.Route{Match: "known-*", Provider: "primary"}, http.DefaultClient, config.Failover{})
	response := debugConsoleRun(srv, map[string]any{
		"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "known-*",
		"body": map[string]any{"model": "unknown-model", "messages": []any{}},
	})
	events := debugConsoleReadEvents(t, response.Body.String())
	startEvent, ok := debugConsoleEvent(events, debugResponseEvent, 0)
	if !ok {
		t.Fatal("未命中路由缺少 response_start")
	}
	var start debugResponseStart
	if err := json.Unmarshal(startEvent.Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Status != http.StatusBadRequest || !strings.Contains(debugConsoleChunks(t, events), "route_not_found") {
		t.Fatalf("未命中路由错误 status/body = %d/%s", start.Status, debugConsoleChunks(t, events))
	}
	metaEvent, ok := debugConsoleEvent(events, debugMetaEvent, 0)
	if !ok {
		t.Fatal("未命中路由缺少 meta")
	}
	var meta debugMeta
	if err := json.Unmarshal(metaEvent.Data, &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Candidates) != 0 {
		t.Errorf("未命中路由 meta 含候选: %#v", meta)
	}
}

func TestDebugConsoleProviderModeIsolatedRuntimeState(t *testing.T) {
	t.Run("可测试已停用 Provider", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, openaiOKBody)
		}))
		defer upstream.Close()
		disabled := false
		provider := &config.Provider{Name: "disabled", BaseURL: upstream.URL, APIKey: "fake-disabled-key", Format: "openai", Enabled: &disabled, MaxConcurrent: 1, MaxQueueWait: 1000}
		srv := newFailoverTestServer(map[string]*config.Provider{"disabled": provider}, config.Route{Match: "*", Provider: "disabled"}, upstream.Client(), config.Failover{})
		response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "disabled", "body": map[string]any{"model": "direct", "messages": []any{}}})
		if response.Code != http.StatusOK {
			t.Fatalf("停用 Provider 调试 status/body = %d/%s", response.Code, response.Body.String())
		}
		events := debugConsoleReadEvents(t, response.Body.String())
		var meta debugMeta
		metaEvent, _ := debugConsoleEvent(events, debugMetaEvent, 0)
		if err := json.Unmarshal(metaEvent.Data, &meta); err != nil {
			t.Fatal(err)
		}
		if len(meta.Candidates) != 1 || meta.Candidates[0].Enabled {
			t.Errorf("停用 Provider meta = %#v", meta)
		}
	})

	t.Run("熔断开路仍能测且状态不变", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, openaiOKBody)
		}))
		defer upstream.Close()
		provider := &config.Provider{Name: "open", BaseURL: upstream.URL, APIKey: "fake-key", Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000}
		srv := newFailoverTestServer(map[string]*config.Provider{"open": provider}, config.Route{Match: "*", Provider: "open"}, upstream.Client(), config.Failover{})
		srv.breaker = breaker.New(breaker.Settings{Enabled: true, ConsecutiveFailures: 1, OpenMs: 60_000, HalfOpenProbes: 1})
		srv.breaker.Report("open", breaker.OutcomeFailure)
		before := srv.breaker.Snapshot()["open"]
		response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "open", "body": map[string]any{"model": "direct", "messages": []any{}}})
		if response.Code != http.StatusOK {
			t.Fatalf("熔断开路时 Provider 调试 status/body = %d/%s", response.Code, response.Body.String())
		}
		after := srv.breaker.Snapshot()["open"]
		if before.State != after.State || before.ConsecutiveFailures != after.ConsecutiveFailures || before.TotalOpens != after.TotalOpens || before.ProbesInFlight != after.ProbesInFlight {
			t.Errorf("Provider 调试改变熔断状态: before=%+v after=%+v", before, after)
		}
	})

	t.Run("上游 5xx 不改变熔断计数", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		}))
		defer upstream.Close()
		provider := &config.Provider{Name: "closed", BaseURL: upstream.URL, APIKey: "fake-key", Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000}
		srv := newFailoverTestServer(map[string]*config.Provider{"closed": provider}, config.Route{Match: "*", Provider: "closed"}, upstream.Client(), config.Failover{})
		srv.breaker = breaker.New(breaker.Settings{Enabled: true, ConsecutiveFailures: 3, OpenMs: 60_000, HalfOpenProbes: 1})
		srv.breaker.Report("closed", breaker.OutcomeFailure)
		before := srv.breaker.Snapshot()["closed"]
		response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "closed", "body": map[string]any{"model": "direct", "messages": []any{}}})
		if response.Code != http.StatusOK {
			t.Fatalf("5xx 仍应通过 SSE 200 返回，status=%d", response.Code)
		}
		events := debugConsoleReadEvents(t, response.Body.String())
		var start debugResponseStart
		startEvent, _ := debugConsoleEvent(events, debugResponseEvent, 0)
		if err := json.Unmarshal(startEvent.Data, &start); err != nil {
			t.Fatal(err)
		}
		if start.Status != http.StatusInternalServerError {
			t.Errorf("response_start.status = %d", start.Status)
		}
		after := srv.breaker.Snapshot()["closed"]
		if before.State != after.State || before.ConsecutiveFailures != after.ConsecutiveFailures || before.TotalOpens != after.TotalOpens {
			t.Errorf("Provider 5xx 调试改变熔断计数: before=%+v after=%+v", before, after)
		}
	})
}

func TestDebugConsoleProviderKeySwitchDoesNotCoolKeys(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			var hits atomic.Int32
			var authValuesMu sync.Mutex
			var authValues []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit := hits.Add(1)
				authValuesMu.Lock()
				authValues = append(authValues, r.Header.Get("Authorization"))
				authValuesMu.Unlock()
				if hit == 1 {
					if status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":"key rejected"}`)
					return
				}
				w.Header().Set("content-type", "application/json")
				_, _ = io.WriteString(w, openaiOKBody)
			}))
			defer upstream.Close()
			provider := providerWithKeys("keyed", upstream.URL,
				config.ProviderKey{ID: "first", Value: "fake-first-key"},
				config.ProviderKey{ID: "second", Value: "fake-second-key"})
			srv := newKeySwitchTestServer(map[string]*config.Provider{"keyed": provider}, singleCandidateRoute("keyed", "ignored"), upstream.Client(), config.Failover{})
			response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "keyed", "body": map[string]any{"model": "direct", "messages": []any{}}})
			if response.Code != http.StatusOK || hits.Load() != 2 {
				t.Fatalf("status/hits = %d/%d，期望换密钥后成功", response.Code, hits.Load())
			}
			authValuesMu.Lock()
			gotAuth := append([]string(nil), authValues...)
			authValuesMu.Unlock()
			if !reflect.DeepEqual(gotAuth, []string{"Bearer fake-first-key", "Bearer fake-second-key"}) {
				t.Fatalf("Authorization 顺序 = %#v", gotAuth)
			}
			for _, key := range srv.keyPool.Snapshot(srv.cfg)["keyed"] {
				if key.State != keypool.StateSelectable {
					t.Errorf("Provider 调试更新了密钥冷却: %#v", key)
				}
			}
			events := debugConsoleReadEvents(t, response.Body.String())
			var requests []debugUpstreamRequest
			for _, event := range events {
				if event.Name != debugUpstreamEvent {
					continue
				}
				var req debugUpstreamRequest
				if err := json.Unmarshal(event.Data, &req); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, req)
			}
			if len(requests) != 2 || requests[0].Sequence != 1 || requests[1].Sequence != 2 {
				t.Errorf("换密钥 upstream_request 序号 = %#v", requests)
			}
			entry := debugConsoleDoneLog(t, events).Log
			if entry.Attempts != 1 || len(entry.AttemptDetails) != 2 || entry.AttemptDetails[0].Outcome != "key_switched" || entry.AttemptDetails[0].Sequence != requests[0].Sequence {
				t.Errorf("换密钥尝试明细 = %#v", entry.AttemptDetails)
			}
		})
	}
}

func TestDebugConsoleProviderDoesNotReselectAttemptedKeys(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"key rejected"}`)
	}))
	defer upstream.Close()
	provider := providerWithKeys("keyed", upstream.URL,
		config.ProviderKey{ID: "first", Value: "fake-first-key"},
		config.ProviderKey{ID: "second", Value: "fake-second-key"})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"keyed": provider}, singleCandidateRoute("keyed", "ignored"), upstream.Client(), config.Failover{})
	response := debugConsoleRun(srv, map[string]any{
		"mode": "provider", "clientFormat": "openai-chat", "provider": "keyed",
		"body": map[string]any{"model": "direct", "messages": []any{}},
	})
	events := debugConsoleReadEvents(t, response.Body.String())
	var start debugResponseStart
	startEvent, ok := debugConsoleEvent(events, debugResponseEvent, 0)
	if !ok {
		t.Fatal("最终上游 401 缺少 response_start")
	}
	if err := json.Unmarshal(startEvent.Data, &start); err != nil {
		t.Fatal(err)
	}
	requestCount := 0
	for _, event := range events {
		if event.Name == debugUpstreamEvent {
			requestCount++
		}
	}
	if hits.Load() != 2 || requestCount != 2 || start.Status != http.StatusUnauthorized {
		t.Fatalf("已试密钥被重复尝试或终态状态码错误: hits=%d requests=%d status=%d", hits.Load(), requestCount, start.Status)
	}
	for _, key := range srv.keyPool.Snapshot(srv.cfg)["keyed"] {
		if key.State != keypool.StateSelectable {
			t.Errorf("Provider 调试更新了密钥冷却: %#v", key)
		}
	}
}

func TestDebugConsoleProviderWithoutSelectableKeyReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("没有可选密钥时不应调用上游")
	}))
	defer upstream.Close()
	disabled := false
	provider := providerWithKeys("keyed", upstream.URL, config.ProviderKey{ID: "disabled", Value: "fake-key", Enabled: &disabled})
	srv := newKeySwitchTestServer(map[string]*config.Provider{"keyed": provider}, singleCandidateRoute("keyed", "ignored"), upstream.Client(), config.Failover{})
	response := debugConsoleRun(srv, map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "keyed", "body": map[string]any{"model": "direct", "messages": []any{}}})
	if response.Code != http.StatusOK {
		t.Fatalf("SSE 外层状态码 = %d，校验通过后应进入 200 事件流", response.Code)
	}
	events := debugConsoleReadEvents(t, response.Body.String())
	var start debugResponseStart
	startEvent, _ := debugConsoleEvent(events, debugResponseEvent, 0)
	if err := json.Unmarshal(startEvent.Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Status != http.StatusServiceUnavailable || !strings.Contains(debugConsoleChunks(t, events), "no_selectable_provider_key") {
		t.Fatalf("无可选密钥错误 status/body = %d/%s", start.Status, debugConsoleChunks(t, events))
	}
}

func TestDebugConsoleDisabledProviderStillUsesQueueLimits(t *testing.T) {
	var active, maxActive, hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			observed := maxActive.Load()
			if current <= observed || maxActive.CompareAndSwap(observed, current) {
				break
			}
		}
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, openaiOKBody)
	}))
	defer upstream.Close()
	disabled := false
	provider := &config.Provider{Name: "disabled", BaseURL: upstream.URL, APIKey: "fake-key", Format: "openai", Enabled: &disabled, MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"disabled": provider}, config.Route{Match: "*", Provider: "disabled"}, upstream.Client(), config.Failover{})
	srv.cfg.DirectMode = false
	srv.cfg.Timeout = 1000
	srv.qm.Reconcile(map[string]queue.Limits{})
	fields := map[string]any{"mode": "provider", "clientFormat": "openai-chat", "provider": "disabled", "body": map[string]any{"model": "direct", "messages": []any{}}}
	var wait sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- debugConsoleRun(srv, fields)
		}()
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Errorf("禁用 Provider 调试状态 = %d，期望通过队列后成功", response.Code)
		}
	}
	if hits.Load() != 2 || maxActive.Load() != 1 {
		t.Errorf("调试队列并发控制错误: hits=%d maxActive=%d", hits.Load(), maxActive.Load())
	}
}

func TestDebugConsoleLogFilteringDoesNotChangeMetrics(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"debug failure"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()
	provider := &config.Provider{Name: "primary", BaseURL: upstream.URL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"primary": provider}, config.Route{Match: "*", Provider: "primary"}, upstream.Client(), config.Failover{})
	if response := postInference(srv, "/v1/messages", anthropicReqBody); response.Code != http.StatusOK {
		t.Fatalf("真实请求 status/body = %d/%s", response.Code, response.Body.String())
	}
	before := debugConsoleGetMetrics(t, srv)
	debugResponse := debugConsoleRun(srv, map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "*", "body": map[string]any{"model": "client", "messages": []any{}}})
	debugEvents := debugConsoleReadEvents(t, debugResponse.Body.String())
	if done := debugConsoleDoneLog(t, debugEvents); !done.Log.Debug || done.Log.DebugMode != "route" || done.Log.Status != http.StatusInternalServerError {
		t.Errorf("调试日志没有调试标记或状态错误: %#v", done.Log)
	}
	after := debugConsoleGetMetrics(t, srv)
	if after.Summary.TotalRequests != before.Summary.TotalRequests || after.Summary.WindowRequests != before.Summary.WindowRequests ||
		!reflect.DeepEqual(after.StatusCodes, before.StatusCodes) || !reflect.DeepEqual(after.Providers, before.Providers) || len(after.RecentErrors) != 0 {
		t.Errorf("调试记录影响了聚合指标: before=%#v after=%#v", before, after)
	}
	all := debugConsoleGetLogs(t, srv, "")
	emptyFilter := debugConsoleGetLogs(t, srv, "?debug=")
	only := debugConsoleGetLogs(t, srv, "?debug=only")
	exclude := debugConsoleGetLogs(t, srv, "?debug=exclude")
	if len(all) != 2 || len(emptyFilter) != len(all) || len(only) != 1 || !only[0].Debug || only[0].DebugMode != "route" || len(exclude) != 1 || exclude[0].Debug {
		t.Errorf("debug 日志筛选错误: all=%#v empty=%#v only=%#v exclude=%#v", all, emptyFilter, only, exclude)
	}
	badFilter := debugConsoleCall(srv, http.MethodGet, "/api/logs?debug=invalid", "", "", "")
	if badFilter.Code != http.StatusBadRequest {
		t.Errorf("非法 debug 筛选状态 = %d，期望 400", badFilter.Code)
	}
	duplicateBadFilter := debugConsoleCall(srv, http.MethodGet, "/api/logs?debug=&debug=invalid", "", "", "")
	if duplicateBadFilter.Code != http.StatusBadRequest {
		t.Errorf("重复参数中的非法 debug 筛选状态 = %d，期望 400", duplicateBadFilter.Code)
	}
}

func TestDebugConsoleStrictRequestBoundaries(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer upstream.Close()
	provider := &config.Provider{Name: "primary", BaseURL: upstream.URL, APIKey: "fake-key", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	srv := newFailoverTestServer(map[string]*config.Provider{"primary": provider}, config.Route{Match: "*", Provider: "primary"}, upstream.Client(), config.Failover{})
	valid := debugConsoleEncodeRequest(t, map[string]any{
		"mode": "provider", "clientFormat": "anthropic", "provider": "primary", "body": map[string]any{"model": "direct", "messages": []any{}},
	})
	for _, test := range []struct {
		name        string
		method      string
		body        string
		contentType string
		origin      string
		wantStatus  int
	}{
		{name: "非 POST", method: http.MethodGet, body: valid, contentType: "application/json", wantStatus: http.StatusMethodNotAllowed},
		{name: "跨站 Origin", method: http.MethodPost, body: valid, contentType: "application/json", origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "错误 Content-Type", method: http.MethodPost, body: valid, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "未知字段", method: http.MethodPost, body: strings.TrimSuffix(valid, "}") + `,"unexpected":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "尾随第二个 JSON 值", method: http.MethodPost, body: valid + ` {}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "缺少 model", method: http.MethodPost, body: `{"mode":"provider","clientFormat":"anthropic","provider":"primary","body":{"messages":[]}}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "Provider 模式包含路由字段", method: http.MethodPost, body: `{"mode":"provider","clientFormat":"anthropic","provider":"primary","routeIndex":0,"body":{"model":"direct"}}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "body 不是对象", method: http.MethodPost, body: `{"mode":"provider","clientFormat":"anthropic","provider":"primary","body":[]}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := debugConsoleCall(srv, test.method, "/api/debug/run", test.body, test.contentType, test.origin)
			if response.Code != test.wantStatus {
				t.Fatalf("status/body = %d/%s，期望 %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if response.Code != http.StatusOK && strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("进入 SSE 前错误却返回事件流: %#v", response.Header())
			}
		})
	}
}

func TestDebugConsoleConfigSelectionValidation(t *testing.T) {
	provider := &config.Provider{Name: "actual", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000}
	route := config.Route{Match: "current-*", Targets: []config.Target{{Provider: "actual", Model: "m"}}}
	srv := newFailoverTestServer(map[string]*config.Provider{"actual": provider}, route, http.DefaultClient, config.Failover{})
	for _, test := range []struct {
		name   string
		fields map[string]any
		status int
	}{
		{name: "Provider 不存在", fields: map[string]any{"mode": "provider", "clientFormat": "anthropic", "provider": "missing", "body": map[string]any{"model": "m"}}, status: http.StatusNotFound},
		{name: "路由配置已变化", fields: map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "old-*", "body": map[string]any{"model": "current-x"}}, status: http.StatusConflict},
		{name: "候选 Provider 已变化", fields: map[string]any{"mode": "target", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "current-*", "targetIndex": 0, "targetProvider": "old", "body": map[string]any{"model": "current-x"}}, status: http.StatusConflict},
		{name: "路由下标越界", fields: map[string]any{"mode": "route", "clientFormat": "anthropic", "routeIndex": 1, "routeMatch": "other", "body": map[string]any{"model": "x"}}, status: http.StatusNotFound},
		{name: "候选下标越界", fields: map[string]any{"mode": "target", "clientFormat": "anthropic", "routeIndex": 0, "routeMatch": "current-*", "targetIndex": 2, "targetProvider": "actual", "body": map[string]any{"model": "current-x"}}, status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := debugConsoleRun(srv, test.fields)
			if response.Code != test.status {
				t.Fatalf("status/body = %d/%s，期望 %d", response.Code, response.Body.String(), test.status)
			}
			if strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("校验错误提前返回 SSE: %#v", response.Header())
			}
		})
	}
}

func TestDebugConsoleBodyLimitMatchesInferenceLimit(t *testing.T) {
	srv := newFailoverTestServer(nil, config.Route{Match: "*"}, http.DefaultClient, config.Failover{})
	body := strings.Repeat(" ", maxProxyBodyBytes+1)
	response := debugConsoleCall(srv, http.MethodPost, "/api/debug/run", body, "application/json", "")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "request_too_large") {
		t.Fatalf("超限请求 status/body = %d/%s，期望 413 request_too_large", response.Code, response.Body.String())
	}
}

func TestDebugConsoleSanitizeUpstreamRequestBodyAndURL(t *testing.T) {
	candidate := router.Candidate{Provider: &config.Provider{Name: "provider", Format: "openai"}, TargetModel: "model"}
	headers := http.Header{
		"Authorization":         {"Bearer secret-token"},
		"X-Api-Key":             {"secret-api-key"},
		"Cookie":                {"session=secret-cookie"},
		"Chatgpt-Account-Id":    {"secret-account"},
		"X-Custom-Token":        {"secret-custom"},
		"User-Agent":            {"safe-agent"},
		"X-Custom-NonSensitive": {"safe-value"},
	}
	body := strings.Repeat("x", debugBodyLimit-1) + "你remaining"
	request := sanitizeDebugUpstreamRequest(3, candidate, http.MethodPost,
		"https://user:pass@example.test/v1?api_key=secret-query&safe=visible&signature=secret-signature", headers, []byte(body))
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"secret-token", "secret-api-key", "secret-cookie", "secret-account", "secret-custom", "user:pass", "secret-query", "secret-signature"} {
		if strings.Contains(text, secret) {
			t.Errorf("脱敏结果包含凭据 %q", secret)
		}
	}
	if !request.BodyTruncated || len(request.Body) >= debugBodyLimit || !utf8.ValidString(request.Body) {
		t.Errorf("请求体截断边界错误: truncated=%t len=%d valid=%t", request.BodyTruncated, len(request.Body), utf8.ValidString(request.Body))
	}
	if request.Headers["Authorization"] != debugRedactedValue || request.Headers["X-Custom-Token"] != debugRedactedValue || request.Headers["User-Agent"] != "safe-agent" {
		t.Errorf("请求头脱敏结构字段错误: %#v", request.Headers)
	}
	parsedURL, err := url.Parse(request.URL)
	if err != nil {
		t.Fatalf("脱敏后的 URL 无法解析: %v", err)
	}
	query := parsedURL.Query()
	if query.Get("api_key") != debugRedactedValue || query.Get("signature") != debugRedactedValue || query.Get("safe") != "visible" {
		t.Errorf("URL 查询参数脱敏结果不符: api_key=%q signature=%q safe=%q", query.Get("api_key"), query.Get("signature"), query.Get("safe"))
	}
}

func TestDebugEnvelopeWriterKeepsUTF8AndSerializesConcurrentEvents(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newDebugEnvelopeWriter(recorder)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	for _, part := range [][]byte{{0xe4}, {0xbd}, {0xa0}} {
		if _, err := writer.Write(part); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	events := debugConsoleReadEvents(t, recorder.Body.String())
	if got := debugConsoleChunks(t, events); got != "你" {
		t.Fatalf("跨 Write 的 UTF-8 字符 = %q", got)
	}
	var start debugResponseStart
	startEvent, _ := debugConsoleEvent(events, debugResponseEvent, 0)
	if err := json.Unmarshal(startEvent.Data, &start); err != nil {
		t.Fatal(err)
	}
	if start.Status != http.StatusCreated || !recorder.Flushed {
		t.Errorf("response_start/flush = %+v/%t", start, recorder.Flushed)
	}

	concurrentRecorder := httptest.NewRecorder()
	concurrentWriter := newDebugEnvelopeWriter(concurrentRecorder)
	const workers, perWorker = 8, 40
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			for item := 0; item < perWorker; item++ {
				if err := concurrentWriter.WriteEvent("concurrent", map[string]int{"worker": id, "item": item}); err != nil {
					t.Errorf("并发写事件失败: %v", err)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	if err := concurrentWriter.Finish(); err != nil {
		t.Fatal(err)
	}
	concurrentEvents := debugConsoleReadEvents(t, concurrentRecorder.Body.String())
	if len(concurrentEvents) != workers*perWorker {
		t.Fatalf("并发事件数 = %d，期望 %d", len(concurrentEvents), workers*perWorker)
	}
}
