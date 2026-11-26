package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/router"
	"ai-gateway/internal/tokenest"
)

const (
	debugBodyLimit     = 256 << 10
	debugRedactedValue = "<已脱敏>"
	debugResponseEvent = "response_start"
	debugChunkEvent    = "response_chunk"
	debugDoneEvent     = "done"
	debugMetaEvent     = "meta"
	debugUpstreamEvent = "upstream_request"
)

type debugRunRequest struct {
	Mode           string         `json:"mode"`
	ClientFormat   string         `json:"clientFormat"`
	Provider       *string        `json:"provider,omitempty"`
	RouteIndex     *int           `json:"routeIndex,omitempty"`
	RouteMatch     *string        `json:"routeMatch,omitempty"`
	TargetIndex    *int           `json:"targetIndex,omitempty"`
	TargetProvider *string        `json:"targetProvider,omitempty"`
	Body           map[string]any `json:"body"`
}

type inferenceHooks struct {
	RequestID string
	// RouteResolver 为空时直接调用 router.MatchRoute。
	RouteResolver func(model string, cfg *config.Config) (*router.Match, error)
	DebugMode     string
	// UpstreamRequest 在每次真实上游请求发出前同步调用。
	UpstreamRequest func(sequence int, candidate router.Candidate, method, requestURL string, headers http.Header, body []byte)
}

type routeResolutionError struct {
	Status  int
	Type    string
	Message string
}

func (e *routeResolutionError) Error() string { return e.Message }

type debugMeta struct {
	RequestID    string               `json:"requestId"`
	Mode         string               `json:"mode"`
	ClientFormat string               `json:"clientFormat"`
	Model        string               `json:"model"`
	Route        string               `json:"route,omitempty"`
	Strategy     string               `json:"strategy,omitempty"`
	Candidates   []debugMetaCandidate `json:"candidates"`
}

type debugMetaCandidate struct {
	Provider       string `json:"provider"`
	TargetModel    string `json:"targetModel"`
	ProviderFormat string `json:"providerFormat"`
	Enabled        bool   `json:"enabled"`
}

type debugUpstreamRequest struct {
	Sequence       int            `json:"sequence"`
	Provider       string         `json:"provider"`
	TargetModel    string         `json:"targetModel"`
	ProviderFormat string         `json:"providerFormat"`
	Method         string         `json:"method"`
	URL            string         `json:"url"`
	Headers        map[string]any `json:"headers"`
	Body           string         `json:"body"`
	BodyTruncated  bool           `json:"bodyTruncated"`
}

type debugResponseStart struct {
	Status  int            `json:"status"`
	Headers map[string]any `json:"headers"`
}

type debugResponseChunk struct {
	Data string `json:"data"`
}

type debugDone struct {
	Log     metrics.RequestLog `json:"log"`
	Aborted bool               `json:"aborted"`
}

// debugEnvelopeWriter 把链路的普通 HTTP 响应封装为调试 SSE 事件。
// ResponseController 可经 Unwrap 设置底层写入 deadline；代理的流式类型断言由 Flush 满足。
type debugEnvelopeWriter struct {
	underlying http.ResponseWriter
	header     http.Header
	mu         sync.Mutex
	status     int
	started    bool
	bytes      int64
	pending    []byte
	writeErr   error
}

func newDebugEnvelopeWriter(underlying http.ResponseWriter) *debugEnvelopeWriter {
	return &debugEnvelopeWriter{underlying: underlying, header: make(http.Header)}
}

func (w *debugEnvelopeWriter) Header() http.Header { return w.header }

func (w *debugEnvelopeWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.writeErr != nil {
		return
	}
	w.status = status
	w.startLocked()
}

func (w *debugEnvelopeWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !w.started {
		w.status = http.StatusOK
		w.startLocked()
		if w.writeErr != nil {
			return 0, w.writeErr
		}
	}
	if len(data) == 0 {
		return 0, nil
	}
	combined := make([]byte, 0, len(w.pending)+len(data))
	combined = append(combined, w.pending...)
	combined = append(combined, data...)
	safeEnd := completeUTF8End(combined)
	w.pending = append(w.pending[:0], combined[safeEnd:]...)
	if safeEnd > 0 {
		if err := w.writeEventLocked(debugChunkEvent, debugResponseChunk{Data: string(combined[:safeEnd])}); err != nil {
			return 0, err
		}
	}
	w.bytes += int64(len(data))
	return len(data), nil
}

func (w *debugEnvelopeWriter) Flush() { _ = w.FlushError() }

func (w *debugEnvelopeWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	if !w.started {
		w.status = http.StatusOK
		w.startLocked()
	}
	if flusher, ok := w.underlying.(http.Flusher); ok {
		flusher.Flush()
	}
	return w.writeErr
}

func (w *debugEnvelopeWriter) Unwrap() http.ResponseWriter { return w.underlying }

func (w *debugEnvelopeWriter) WriteEvent(event string, data any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeEventLocked(event, data)
}

func (w *debugEnvelopeWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	if len(w.pending) > 0 {
		if !w.started {
			w.status = http.StatusOK
			w.startLocked()
		}
		if err := w.writeEventLocked(debugChunkEvent, debugResponseChunk{Data: string(w.pending)}); err != nil {
			return err
		}
		w.pending = nil
	}
	return nil
}

func (w *debugEnvelopeWriter) DebugResponseStatus() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *debugEnvelopeWriter) DebugResponseBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

func (w *debugEnvelopeWriter) startLocked() {
	if w.started || w.writeErr != nil {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.started = true
	w.writeErr = w.writeEventLocked(debugResponseEvent, debugResponseStart{
		Status:  w.status,
		Headers: debugHeaderValues(w.header),
	})
}

func (w *debugEnvelopeWriter) writeEventLocked(event string, data any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		w.writeErr = err
		return err
	}
	_, err = fmt.Fprintf(w.underlying, "event: %s\ndata: %s\n\n", event, encoded)
	if err != nil {
		w.writeErr = err
		return err
	}
	if flusher, ok := w.underlying.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func debugHeaderValues(header http.Header) map[string]any {
	result := make(map[string]any, len(header))
	for name, values := range header {
		if len(values) == 1 {
			result[name] = values[0]
		} else {
			result[name] = append([]string(nil), values...)
		}
	}
	return result
}

func completeUTF8End(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	start := len(data) - 1
	for start >= 0 && data[start]&0xc0 == 0x80 {
		start--
	}
	if start >= 0 {
		lead := data[start]
		expected := 0
		switch {
		case lead >= 0xc2 && lead <= 0xdf:
			expected = 2
		case lead >= 0xe0 && lead <= 0xef:
			expected = 3
		case lead >= 0xf0 && lead <= 0xf4:
			expected = 4
		}
		if expected > len(data)-start {
			return start
		}
	}
	return len(data)
}

func (s *server) handleDebugRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if isCrossSiteRequest(r) {
		writeForbiddenOrigin(w)
		return
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "调试请求须使用 application/json")
		return
	}
	raw, err := readBodyLimited(r, maxProxyBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
			return
		}
		writeJSONError(w, http.StatusBadRequest, "debug_invalid_request", "读取调试请求失败")
		return
	}
	request, err := decodeDebugRunRequest(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "debug_invalid_request", err.Error())
		return
	}

	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	var (
		provider       *config.Provider
		selectedRoute  config.Route
		selectedTarget config.Target
	)
	switch request.Mode {
	case "provider":
		provider = cfg.Providers[*request.Provider]
		if provider == nil {
			writeJSONError(w, http.StatusNotFound, "debug_target_not_found", "找不到所选 Provider")
			return
		}
	case "route", "target":
		if *request.RouteIndex < 0 || *request.RouteIndex >= len(cfg.Routes) {
			writeJSONError(w, http.StatusNotFound, "debug_target_not_found", "找不到所选路由")
			return
		}
		selectedRoute = cfg.Routes[*request.RouteIndex]
		if selectedRoute.Match != *request.RouteMatch {
			writeJSONError(w, http.StatusConflict, "debug_config_changed", "所选路由配置已变化，请重新选择")
			return
		}
		if request.Mode == "target" {
			targets := selectedRoute.TargetList()
			if *request.TargetIndex < 0 || *request.TargetIndex >= len(targets) {
				writeJSONError(w, http.StatusNotFound, "debug_target_not_found", "找不到所选候选")
				return
			}
			selectedTarget = targets[*request.TargetIndex]
			if selectedTarget.Provider != *request.TargetProvider {
				writeJSONError(w, http.StatusConflict, "debug_config_changed", "所选候选 Provider 已变化，请重新选择")
				return
			}
			if cfg.Providers[selectedTarget.Provider] == nil {
				writeJSONError(w, http.StatusNotFound, "debug_target_not_found", "所选候选 Provider 不存在")
				return
			}
		}
	}

	var inferenceBody []byte
	if request.Mode != "provider" {
		inferenceBody, err = json.Marshal(request.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "debug_invalid_request", "调试请求体编码失败")
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	stream := newDebugEnvelopeWriter(w)
	reqID := nextReqID()
	var record *metrics.RequestLog
	if request.Mode == "provider" {
		candidate := router.CandidateForProvider(provider, requestModel(request.Body))
		if candidate.Provider.Name == "" {
			candidate.Provider.Name = *request.Provider
		}
		record = s.runProviderDebug(stream, r, cfg, request, candidate, reqID)
	} else {
		r.Body = io.NopCloser(bytes.NewReader(inferenceBody))
		hooks := inferenceHooks{
			RequestID: reqID,
			DebugMode: request.Mode,
			UpstreamRequest: func(sequence int, candidate router.Candidate, method, requestURL string, headers http.Header, body []byte) {
				s.handleDebugUpstreamRequest(stream, sequence, candidate, method, requestURL, headers, body)
			},
		}
		hooks.RouteResolver = func(model string, current *config.Config) (*router.Match, error) {
			matched, actualRouteIndex := router.MatchRouteIndex(model, current)
			if matched == nil {
				if err := stream.WriteEvent(debugMetaEvent, makeDebugMeta(reqID, request, model, nil, false)); err != nil {
					return nil, &routeResolutionError{Type: "client_disconnected", Message: "客户端已断开连接"}
				}
				return nil, &routeResolutionError{
					Status:  http.StatusBadRequest,
					Type:    "route_not_found",
					Message: fmt.Sprintf("没有匹配 model %q 的路由规则", model),
				}
			}
			if matched.RouteMatch != *request.RouteMatch || actualRouteIndex != *request.RouteIndex {
				if err := stream.WriteEvent(debugMetaEvent, makeDebugMeta(reqID, request, model, matched, true)); err != nil {
					return matched, &routeResolutionError{Type: "client_disconnected", Message: "客户端已断开连接"}
				}
				return matched, &routeResolutionError{
					Status:  http.StatusUnprocessableEntity,
					Type:    "debug_route_mismatch",
					Message: fmt.Sprintf("请求模型 %q 实际命中路由 match=%q，与所选路由 match=%q 不一致", model, matched.RouteMatch, *request.RouteMatch),
				}
			}
			if request.Mode == "target" {
				if *request.TargetIndex >= len(matched.Candidates) {
					if err := stream.WriteEvent(debugMetaEvent, makeDebugMeta(reqID, request, model, nil, false)); err != nil {
						return nil, &routeResolutionError{Type: "client_disconnected", Message: "客户端已断开连接"}
					}
					return nil, nil
				}
				narrowed := *matched
				narrowed.Candidates = []router.Candidate{matched.Candidates[*request.TargetIndex]}
				narrowed.Strategy = "failover"
				matched = &narrowed
			}
			if err := stream.WriteEvent(debugMetaEvent, makeDebugMeta(reqID, request, model, matched, false)); err != nil {
				return matched, &routeResolutionError{Type: "client_disconnected", Message: "客户端已断开连接"}
			}
			return matched, nil
		}
		record = s.runInference(stream, r, "/api/debug/run", cfg, request.ClientFormat, hooks)
	}
	_ = stream.Finish()
	if record != nil {
		_ = stream.WriteEvent(debugDoneEvent, debugDone{Log: *record, Aborted: r.Context().Err() != nil})
	}
}

func decodeDebugRunRequest(raw []byte) (debugRunRequest, error) {
	var request debugRunRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("调试请求 JSON 无效或包含未知字段")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return request, errors.New("调试请求只能包含一个 JSON 值")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return request, errors.New("调试请求必须是 JSON 对象")
	}
	if request.Mode != "provider" && request.Mode != "route" && request.Mode != "target" {
		return request, errors.New("mode 必须是 provider、route 或 target")
	}
	switch request.ClientFormat {
	case "anthropic", "openai-chat", "openai-responses":
	default:
		return request, errors.New("clientFormat 必须是 anthropic、openai-chat 或 openai-responses")
	}
	if request.Body == nil {
		return request, errors.New("body 必须是 JSON 对象")
	}
	model, ok := request.Body["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return request, errors.New("body.model 必须是非空字符串")
	}

	present := func(name string) bool { _, ok := fields[name]; return ok }
	providerPresent := present("provider")
	routeIndexPresent := present("routeIndex")
	routeMatchPresent := present("routeMatch")
	targetIndexPresent := present("targetIndex")
	targetProviderPresent := present("targetProvider")
	switch request.Mode {
	case "provider":
		if !providerPresent || request.Provider == nil || strings.TrimSpace(*request.Provider) == "" {
			return request, errors.New("provider 模式必须指定 provider")
		}
		if routeIndexPresent || routeMatchPresent || targetIndexPresent || targetProviderPresent {
			return request, errors.New("provider 模式不能包含路由或候选字段")
		}
	case "route":
		if !routeIndexPresent || request.RouteIndex == nil || !routeMatchPresent || request.RouteMatch == nil || strings.TrimSpace(*request.RouteMatch) == "" {
			return request, errors.New("route 模式必须指定 routeIndex 和 routeMatch")
		}
		if providerPresent || targetIndexPresent || targetProviderPresent {
			return request, errors.New("route 模式不能包含 Provider 或候选字段")
		}
	case "target":
		if !routeIndexPresent || request.RouteIndex == nil || !routeMatchPresent || request.RouteMatch == nil || strings.TrimSpace(*request.RouteMatch) == "" ||
			!targetIndexPresent || request.TargetIndex == nil || !targetProviderPresent || request.TargetProvider == nil || strings.TrimSpace(*request.TargetProvider) == "" {
			return request, errors.New("target 模式必须指定 routeIndex、routeMatch、targetIndex 和 targetProvider")
		}
		if providerPresent {
			return request, errors.New("target 模式不能包含 provider 字段")
		}
	}
	return request, nil
}

func requestModel(body map[string]any) string {
	model, _ := body["model"].(string)
	return model
}

func makeDebugMeta(requestID string, request debugRunRequest, model string, matched *router.Match, emptyCandidates bool) debugMeta {
	meta := debugMeta{
		RequestID:    requestID,
		Mode:         request.Mode,
		ClientFormat: request.ClientFormat,
		Model:        model,
		Candidates:   make([]debugMetaCandidate, 0),
	}
	if matched == nil {
		return meta
	}
	meta.Route = matched.RouteMatch
	meta.Strategy = matched.Strategy
	if meta.Strategy == "" {
		meta.Strategy = "failover"
	}
	if emptyCandidates {
		return meta
	}
	for _, candidate := range matched.Candidates {
		if candidate.Provider == nil {
			continue
		}
		meta.Candidates = append(meta.Candidates, debugMetaCandidate{
			Provider:       candidate.Provider.Name,
			TargetModel:    candidate.TargetModel,
			ProviderFormat: candidate.Provider.Format,
			Enabled:        candidate.Provider.IsEnabled(),
		})
	}
	return meta
}

func sanitizeDebugUpstreamRequest(sequence int, candidate router.Candidate, method, requestURL string, headers http.Header, body []byte) debugUpstreamRequest {
	values := make(map[string]any, len(headers))
	for name, entries := range headers {
		clean := make([]string, len(entries))
		for i, value := range entries {
			if sensitiveDebugHeader(name) {
				clean[i] = debugRedactedValue
			} else {
				clean[i] = proxy.SanitizeDiagnostic(value)
			}
		}
		if len(clean) == 1 {
			values[name] = clean[0]
		} else {
			values[name] = clean
		}
	}
	truncated := len(body) > debugBodyLimit
	if truncated {
		end := debugBodyLimit
		for end > 0 && !utf8.Valid(body[:end]) {
			end--
		}
		body = body[:end]
	}
	providerName, targetModel, providerFormat := "", candidate.TargetModel, ""
	if candidate.Provider != nil {
		providerName = candidate.Provider.Name
		providerFormat = candidate.Provider.Format
	}
	return debugUpstreamRequest{
		Sequence:       sequence,
		Provider:       providerName,
		TargetModel:    targetModel,
		ProviderFormat: providerFormat,
		Method:         method,
		URL:            sanitizeDebugURL(requestURL),
		Headers:        values,
		Body:           string(body),
		BodyTruncated:  truncated,
	}
}

func sensitiveDebugHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "authorization", "x-api-key", "api-key", "cookie", "proxy-authorization":
		return true
	}
	if strings.Contains(lower, "chatgpt") && strings.Contains(lower, "account") {
		return true
	}
	for _, marker := range []string{"key", "token", "secret", "auth"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func sanitizeDebugURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[无效 URL]"
	}
	parsed.User = nil
	query := parsed.Query()
	for name, values := range query {
		lower := strings.ToLower(name)
		for _, marker := range []string{"key", "token", "secret", "sig", "password"} {
			if strings.Contains(lower, marker) {
				for i := range values {
					values[i] = debugRedactedValue
				}
				query[name] = values
				break
			}
		}
	}
	parsed.RawQuery = query.Encode()
	return proxy.SanitizeDiagnostic(parsed.String())
}

func (s *server) runProviderDebug(stream *debugEnvelopeWriter, r *http.Request, cfg *config.Config, request debugRunRequest, candidate router.Candidate, reqID string) (result *metrics.RequestLog) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: stream}
	w := http.ResponseWriter(rec)
	internal := normalizeDebugBody(request.ClientFormat, request.Body)
	model := internal.Model
	reqLog := metrics.RequestLog{
		ID:             reqID,
		Started:        start,
		StartedAt:      start.Format(time.RFC3339Nano),
		Time:           start.In(beijingLoc).Format("2006-01-02 15:04:05"),
		Method:         r.Method,
		Path:           "/api/debug/run",
		ClientIP:       clientIP(r),
		ClientFormat:   request.ClientFormat,
		Format:         request.ClientFormat,
		Model:          model,
		Stream:         internal.Stream,
		Provider:       candidate.Provider.Name,
		TargetModel:    candidate.TargetModel,
		UpstreamFormat: candidate.Provider.Format,
		Debug:          true,
		DebugMode:      "provider",
	}
	defer func() {
		reqLog.Status = rec.Status()
		reqLog.ResponseBytes = rec.Bytes()
		if reqLog.Error == "" && reqLog.Status >= 400 {
			reqLog.Error = http.StatusText(reqLog.Status)
		}
		reqLog.DurationMs = time.Since(start).Milliseconds()
		reqLog.Status = stream.DebugResponseStatus()
		reqLog.ResponseBytes = stream.DebugResponseBytes()
		finalizeErrorProvenance(&reqLog, r)
		if s.metrics != nil {
			s.metrics.Add(reqLog)
		}
		result = &reqLog
	}()

	if err := stream.WriteEvent(debugMetaEvent, debugMeta{
		RequestID:    reqID,
		Mode:         "provider",
		ClientFormat: request.ClientFormat,
		Model:        model,
		Candidates: []debugMetaCandidate{{
			Provider:       candidate.Provider.Name,
			TargetModel:    candidate.TargetModel,
			ProviderFormat: candidate.Provider.Format,
			Enabled:        candidate.Provider.IsEnabled(),
		}},
	}); err != nil {
		reqLog.Error = "客户端已断开连接"
		reqLog.ErrorSource = "client"
		reqLog.ErrorCategory = "client_disconnected"
		return
	}

	var trail trailBuffer
	var sequence int
	var requestCount int
	var lastOutcome forwardAttemptOutcome
	var excluded map[string]struct{}
	if candidate.Provider.UsesKeyList() {
		excluded = make(map[string]struct{}, len(candidate.Provider.APIKeys))
	}
	disableGenerationTimeout := candidate.TimeoutMode == config.TimeoutModeClient
	chainRemaining := time.Duration(requestChainBudgetMsForMode(cfg, internal.Stream, disableGenerationTimeout)) * time.Millisecond
	var contextBudget int
	if candidate.ContextWindow != nil {
		estimated := tokenest.Estimate(internal.System, internal.Messages, internal.Tools)
		reqLog.EstimatedInputTokens = estimated
		budget, fits := resolveContextBudget(candidate.ContextWindow, estimated, cfg.SafetyMargin())
		if !fits {
			message := contextWindowExceededMessage([]router.Candidate{candidate}, estimated, cfg.SafetyMargin())
			reqLog.ErrorSource = "gateway"
			reqLog.ErrorCategory = "context_window_exceeded"
			reqLog.Error = message
			writeJSONError(w, http.StatusRequestEntityTooLarge, "context_window_exceeded", message)
			return
		}
		contextBudget = budget
	}

	for {
		var ticket *keypool.Ticket
		var retryAt time.Time
		if candidate.Provider.UsesKeyList() {
			if s.keyPool != nil {
				ticket, retryAt = s.keyPool.Select(candidate.Provider.Name, candidate.Provider, excluded)
			}
			if ticket == nil {
				if requestCount == 0 || lastOutcome.keySwitch {
					sequence++
					started := time.Now()
					detail := metrics.AttemptDetail{
						Sequence:       sequence,
						Kind:           "key_skip",
						Provider:       candidate.Provider.Name,
						TargetModel:    candidate.TargetModel,
						ProviderFormat: candidate.Provider.Format,
						StartedAt:      started.In(beijingLoc).Format(time.RFC3339Nano),
						Outcome:        "skipped",
						Reason:         "no_selectable_key",
					}
					if s.keyPool != nil && retryAt.IsZero() {
						retryAt = s.keyPool.EarliestRecovery(candidate.Provider.Name, candidate.Provider)
					}
					if !retryAt.IsZero() {
						detail.RetryAfterMs = time.Until(retryAt).Milliseconds()
					}
					detail.DurationMs = time.Since(started).Milliseconds()
					appendAttemptDetail(&reqLog, detail)
					trail.Add(candidate.Provider.Name + ":no_selectable_key")
					reqLog.AttemptTrail = trail.String()
					reqLog.ErrorSource = "gateway"
					reqLog.ErrorCategory = "no_selectable_provider_key"
					reqLog.Error = "没有可选择的 Provider 密钥"
					if !retryAt.IsZero() {
						remaining := time.Until(retryAt)
						if remaining > 0 {
							seconds := int((remaining + time.Second - 1) / time.Second)
							if seconds < 1 {
								seconds = 1
							}
							w.Header().Set("retry-after", strconv.Itoa(seconds))
						}
					}
					writeJSONError(w, http.StatusServiceUnavailable, "no_selectable_provider_key", reqLog.Error)
				} else {
					trail.Add(candidate.Provider.Name + ":key_switch_exhausted")
					reqLog.AttemptTrail = trail.String()
					reqLog.ErrorSource = "gateway"
					reqLog.ErrorCategory = "no_selectable_provider_key"
					reqLog.Error = "没有可继续切换的 Provider 密钥"
					writeJSONError(w, http.StatusServiceUnavailable, "no_selectable_provider_key", reqLog.Error)
				}
				break
			}
		}

		sequence++
		attemptStarted := time.Now()
		detail := metrics.AttemptDetail{
			Sequence:       sequence,
			Provider:       candidate.Provider.Name,
			TargetModel:    candidate.TargetModel,
			ProviderFormat: candidate.Provider.Format,
			StartedAt:      attemptStarted.In(beijingLoc).Format(time.RFC3339Nano),
		}
		allowSwitch := false
		if ticket != nil && s.keyPool != nil {
			excluded[ticket.Key.ID] = struct{}{}
			allowSwitch = s.keyPool.HasSelectable(candidate.Provider.Name, candidate.Provider, excluded)
		}
		beforeRequest := func(method, requestURL string, headers http.Header, body []byte) {
			s.handleDebugUpstreamRequest(stream, sequence, candidate, method, requestURL, headers, body)
		}
		lastOutcome = s.forwardAttempt(w, r, forwardAttemptInput{
			cfg:                      cfg,
			reqID:                    reqID,
			start:                    start,
			clientFormat:             request.ClientFormat,
			originalModel:            model,
			internal:                 internal,
			rawBody:                  request.Body,
			candidate:                candidate,
			keyTicket:                ticket,
			allowKeySwitch:           allowSwitch,
			chainBudgetMs:            int(chainRemaining / time.Millisecond),
			disableGenerationTimeout: disableGenerationTimeout,
			contextBudget:            contextBudget,
			attemptNo:                1,
			httpAttemptNo:            requestCount + 1,
			allowFailoverTransfer:    false,
			allowLocalRetry:          false,
			allowInactiveQueue:       true,
			onBeforeRequest:          beforeRequest,
			reqLog:                   &reqLog,
			detail:                   &detail,
			diagnostic:               true,
		})
		detail.DurationMs = time.Since(attemptStarted).Milliseconds()
		appendAttemptDetail(&reqLog, detail)
		if lastOutcome.requestStarted {
			requestCount++
			chainRemaining -= lastOutcome.upstreamDuration
		}
		trail.Add(lastOutcome.trail)
		if lastOutcome.keySwitch {
			if ticket != nil {
				excluded[ticket.Key.ID] = struct{}{}
			}
			continue
		}
		break
	}

	// Provider 模式只有一个候选；换密钥会留下多条尝试明细，但不增加候选尝试数。
	if requestCount > 0 {
		reqLog.Attempts = 1
	}
	if trail.Len() > 0 {
		reqLog.AttemptTrail = trail.String()
	}
	if lastOutcome.accountPaused {
		reqLog.ErrorSource = "gateway"
		reqLog.ErrorCategory = "account_paused"
		reqLog.Error = "ChatGPT OAuth 账户已暂停，未发起上游请求"
		writeJSONError(w, http.StatusServiceUnavailable, "account_paused", reqLog.Error)
		return
	}
	if lastOutcome.accountCooldown {
		reqLog.ErrorSource = "gateway"
		reqLog.ErrorCategory = "account_cooldown"
		reqLog.Error = "ChatGPT OAuth 账户处于共享额度冷却，未发起上游请求"
		writeJSONError(w, http.StatusServiceUnavailable, "account_cooldown", reqLog.Error)
		return
	}
	if lastOutcome.buildErr != "" {
		reqLog.ErrorSource = "gateway"
		reqLog.ErrorCategory = "conversion_error"
		reqLog.Error = "上游请求构建失败: " + lastOutcome.buildErr
		writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
		return
	}
	if r.Context().Err() != nil {
		reqLog.ErrorSource = "client"
		reqLog.ErrorCategory = "client_disconnected"
		reqLog.Error = "客户端已断开连接"
		return
	}
	if stream.DebugResponseStatus() == 0 {
		reqLog.ErrorSource = "gateway"
		reqLog.ErrorCategory = "debug_request_abandoned"
		reqLog.Error = "Provider 调试请求未写出响应，轨迹: " + reqLog.AttemptTrail
		writeJSONError(w, http.StatusBadGateway, "gateway_error", reqLog.Error)
	}
	return
}

func normalizeDebugBody(clientFormat string, body map[string]any) *converter.Internal {
	switch clientFormat {
	case "anthropic":
		return converter.FromAnthropic(body)
	case "openai-chat":
		return converter.FromOpenAIChat(body)
	default:
		return converter.FromOpenAIResponses(body)
	}
}

func (s *server) handleDebugUpstreamRequest(stream *debugEnvelopeWriter, sequence int, candidate router.Candidate, method, requestURL string, headers http.Header, body []byte) {
	_ = stream.WriteEvent(debugUpstreamEvent, sanitizeDebugUpstreamRequest(sequence, candidate, method, requestURL, headers, body))
}
