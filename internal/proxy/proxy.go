// Package proxy 对齐 Node 版 lib/proxy.js：转发请求到上游，处理流式/非流式响应。
//
// 相比 Node 版手搓 setTimeout + proxyRes.destroy + streamCtx 标志，Go 版用 context 统一管理
// 请求超时与活跃超时，流式转发用单独 goroutine，取消/超时通过 ctx 一处收口，不会有 slot 泄漏。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/httputil"
)

// LogFunc 日志函数，绑定了 reqId
type LogFunc func(format string, args ...any)

const (
	maxResponseBodyBytes = 32 << 20
	maxErrorBodyBytes    = 1 << 20
	// maxObservedErrorBodyBytes 限制写入请求日志的上游错误正文。实际回传仍沿用
	// maxErrorBodyBytes 的既有保护上限，观测能力不能扩大内存或泄露风险。
	maxObservedErrorBodyBytes = 8 << 10
	// observedErrorBodyTimeout 限制即将放弃的候选采集错误正文和 drain 的时间，
	// 不能让诊断信息读取占用下一次故障转移的时间预算。
	observedErrorBodyTimeout = 100 * time.Millisecond
	maxSSEEventBytes         = 8 << 20
	// maxDrainBytes 放弃尝试时最多 drain 的响应体字节数，用于保住连接复用。
	maxDrainBytes = 64 << 10
)

var (
	errResponseBodyTooLarge  = errors.New("上游响应体超过大小限制")
	errSSEEventTooLarge      = errors.New("上游 SSE 事件超过大小限制")
	errStreamActivityTimeout = errors.New("流式传输活跃超时")
	// ErrConversion 表示上游已响应，但无法安全转换为客户端协议。
	ErrConversion = errors.New("上游响应协议转换失败")

	// ErrAttemptAbandoned 表示 ShouldRetry 判定放弃本次尝试，未向客户端写入任何字节。
	// 调用方应据此换下一个候选重试，或在候选耗尽后自行写终态响应。
	ErrAttemptAbandoned = errors.New("本次尝试已放弃，交由调用方重试")
	// ErrStreamHeaderTimeout 流式阶段等待上游响应头超时，此时客户端尚未收到任何字节。
	ErrStreamHeaderTimeout = errors.New("上游响应头超时")
	// ErrRequestTimeout 非流式整体超时。
	ErrRequestTimeout = errors.New("上游请求超时")
)

// ShouldRetryFunc 判定一次失败的尝试能否放弃并交给调用方重试。
//
// 只在「尚未向客户端写入任何字节」时调用：
//   - 返回 true：Forward 立即放弃本次尝试并返回 ErrAttemptAbandoned，不写客户端响应
//   - 返回 false 或钩子为 nil：按既有逻辑把错误 / 上游响应写回客户端
//
// upstreamCode 为上游状态码（传输层失败时为 0），retryAfter 为解析后的
// Retry-After（缺失或无法解析时为 0），err 为传输层错误（有响应时为 nil）。
type ShouldRetryFunc func(upstreamCode int, retryAfter time.Duration, err error) bool

// UpstreamStatusFunc 上报本次尝试真实拿到的上游状态码。
//
// 只在确实收到上游响应头时调用；传输错误、超时等没有响应的情况不调用，
// 让调用方能用「未上报」区分「上游没给状态码」和「上游给了 5xx」。
type UpstreamStatusFunc func(upstreamCode int)

// UpstreamHeadersFunc 在收到上游响应头后报告可观测性字段。request ID 与
// Retry-After 都来自响应头，传输失败时不会调用。
type UpstreamHeadersFunc func(upstreamCode int, requestID string, retryAfter time.Duration)

// ErrorBodyFunc 报告已限量且脱敏的上游错误正文。正文只在状态码 >= 400 时采集。
type ErrorBodyFunc func(body string, truncated bool)

// ResponseStartedFunc 表示网关已经开始向客户端写入响应；此后不能再安全 failover。
type ResponseStartedFunc func()

// StreamConversionErrorFunc 报告跨格式流式转换失败时已收到的上游原始 SSE。
// raw 是原始字节（含 SSE 换行），truncated 表示超出缓冲上限后被截断。
type StreamConversionErrorFunc func(raw []byte, truncated bool, convErr error)

// Options 转发选项
type Options struct {
	ClientReq             *http.Request
	ClientRes             http.ResponseWriter
	UpstreamBody          []byte // 已转换好的上游请求体
	Provider              *config.Provider
	ClientFormat          string // anthropic | openai-chat | openai-responses
	OriginalModel         string
	IsStreaming           bool
	Log                   LogFunc
	StartTime             time.Time
	TimeoutMs             int
	HeaderTimeoutMs       int // 流式：等上游响应头的超时（毫秒）。为 0 时回退用 TimeoutMs，保证非直通调用方行为不变
	StreamActivityTimeout int
	HTTPClient            *http.Client
	// ShouldRetry 为 nil 时行为与不支持转移的旧版本完全一致。
	// 流式响应一旦开始写入（handleStream）就不再询问：已发出的 SSE 字节不可回收。
	ShouldRetry ShouldRetryFunc
	// OnUpstreamStatus 收到上游响应头后上报状态码，可为 nil。
	// 与 ShouldRetry 分开：后者只在「可放弃」的时机询问，拿不到最后一个候选
	// （不允许转移）的状态码，而熔断判据恰恰需要它。
	OnUpstreamStatus UpstreamStatusFunc
	// OnUpstreamHeaders 提供状态码以外的上游诊断元数据，可与旧回调并存。
	OnUpstreamHeaders UpstreamHeadersFunc
	// OnErrorBody 提供脱敏、最多 8 KiB 的上游错误正文。
	OnErrorBody ErrorBodyFunc
	// OnResponseStarted 在向客户端写响应头或正文前调用。
	OnResponseStarted ResponseStartedFunc
	// OnStreamConversionError 在跨格式流式转换失败时提供已收到的上游原始 SSE 字节。
	//
	// 为 nil 时（默认）完全不缓冲原始流，不产生额外内存开销——排查开关关闭是常态，
	// 不能让每条流都多付一份拷贝。缓冲上限与 maxSSEEventBytes 一致，超限后停止追加
	// 并置 truncated，而不是中断转发：抓包是旁路，不该影响客户端。
	//
	// 只在 ErrConversion 那条路径回调。上游提前 EOF、活跃超时走 finishTransformedStream，
	// 那类失败原始流本身就是完整的，落盘没有增量信息。
	OnStreamConversionError StreamConversionErrorFunc
	// ToolNamespaces 是请求侧展平 Codex namespace 工具时得到的「扁平名 → 原始身份」
	// 映射，为空表示无需还原。响应侧据它把上游返回的扁平 function_call 名改回
	// {name, namespace}，否则 Codex 报 unsupported call。透传路径同样需要。
	ToolNamespaces map[string]converter.NamespacedName
}

// recordUpstreamStatus 上报上游状态码，供调用方做熔断判据与请求日志。
func recordUpstreamStatus(opts *Options, code int) {
	if opts.OnUpstreamStatus != nil {
		opts.OnUpstreamStatus(code)
	}
}

func recordUpstreamHeaders(opts *Options, resp *http.Response) {
	if resp == nil {
		return
	}
	recordUpstreamStatus(opts, resp.StatusCode)
	if opts.OnUpstreamHeaders != nil {
		opts.OnUpstreamHeaders(resp.StatusCode, upstreamRequestID(resp.Header), parseRetryAfter(resp.Header, time.Now()))
	}
}

func recordErrorBody(opts *Options, body string, truncated bool) {
	if opts.OnErrorBody != nil {
		opts.OnErrorBody(body, truncated)
	}
}

func markResponseStarted(opts *Options) {
	if opts.OnResponseStarted != nil {
		opts.OnResponseStarted()
	}
}

func upstreamRequestID(h http.Header) string {
	for _, key := range []string{"x-request-id", "request-id", "x-openai-request-id"} {
		if value := strings.TrimSpace(h.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// abandonAttempt 在尚未写入客户端时询问是否放弃本次尝试。
func abandonAttempt(opts *Options, upstreamCode int, retryAfter time.Duration, err error) bool {
	if opts.ShouldRetry == nil {
		return false
	}
	return opts.ShouldRetry(upstreamCode, retryAfter, err)
}

// drainBody 限量 drain 响应体以保住连接复用；Close 由调用方的 defer 负责。
func drainBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
}

var sensitiveErrorField = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|token|authorization|cookie|secret|password)["']?\s*[:=]\s*["']?)([^\s,"'}]+)`)

// readErrorBody 一次性消费最多 limit 字节的上游错误正文，同时产出用于日志的
// 8 KiB 脱敏片段。回传分支用 maxErrorBodyBytes，放弃分支只用观测上限。
func readErrorBody(r io.Reader, limit int) (raw []byte, observed string, truncated bool, err error) {
	if limit <= 0 {
		return nil, "", false, fmt.Errorf("错误正文读取上限必须大于零")
	}
	raw, readErr := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if len(raw) > maxObservedErrorBodyBytes {
		truncated = true
	}
	observedRaw := raw
	if len(observedRaw) > maxObservedErrorBodyBytes {
		observedRaw = []byte(truncateUTF8(string(observedRaw), maxObservedErrorBodyBytes))
	}
	observed = sanitizeErrorBody(observedRaw)
	if len(observed) > maxObservedErrorBodyBytes {
		// 对无效 JSON 做正则脱敏时，替换后的字节数理论上可能略有变化；日志契约
		// 仍必须严格保持 8 KiB 上限。
		observed = truncateUTF8(observed, maxObservedErrorBodyBytes)
		truncated = true
	}
	if readErr != nil {
		return raw, observed, truncated, readErr
	}
	if len(raw) > limit {
		return raw, observed, true, fmt.Errorf("%w (%d bytes)", errResponseBodyTooLarge, limit)
	}
	return raw, observed, truncated, nil
}

// truncateUTF8 按字节上限截断字符串，并移除截断处不完整的 UTF-8 序列。
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}

// readObservedErrorBody 读取即将放弃的候选的短错误正文，并在同一短计时器内 drain。
// 计时器取消请求上下文以打断卡住的读取；正常完成时仍会 drain 以保持连接复用。
func readObservedErrorBody(cancel context.CancelFunc, resp *http.Response) (string, bool) {
	timerDone := make(chan struct{})
	timer := time.AfterFunc(observedErrorBodyTimeout, func() {
		cancel()
		close(timerDone)
	})
	_, observed, truncated, _ := readErrorBody(resp.Body, maxObservedErrorBodyBytes)
	drainBody(resp)
	if !timer.Stop() {
		<-timerDone
	}
	return observed, truncated
}

// readStreamErrorBody 为“已拿到流响应头、且必须回传错误正文”的阶段保留活跃超时。
// 流式请求已在拿到响应头后取消 header timer，错误正文本身仍可能卡住。
func readStreamErrorBody(ctx context.Context, cancel context.CancelFunc, r io.Reader, opts *Options, limit int) ([]byte, string, bool, error) {
	timeoutMs := opts.StreamActivityTimeout
	if timeoutMs <= 0 {
		timeoutMs = opts.TimeoutMs
	}
	var timedOut atomic.Bool
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		timedOut.Store(true)
		cancel()
		close(timerDone)
	})
	raw, observed, truncated, err := readErrorBody(r, limit)
	if !timer.Stop() {
		<-timerDone
	}
	if timedOut.Load() {
		return raw, observed, truncated, fmt.Errorf("%w: 上游错误响应体超时", context.DeadlineExceeded)
	}
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return raw, observed, truncated, ctx.Err()
	}
	return raw, observed, truncated, err
}

func sanitizeErrorBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// JSON 错误体按字段名递归脱敏，避免正则跨引号导致结构损坏。
	var value any
	if json.Unmarshal(raw, &value) == nil {
		redactSensitiveJSON(value)
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return sensitiveErrorField.ReplaceAllString(string(raw), "${1}[REDACTED]")
}

func redactSensitiveJSON(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if isSensitiveErrorField(key) {
				current[key] = "[REDACTED]"
				continue
			}
			redactSensitiveJSON(child)
		}
	case []any:
		for _, child := range current {
			redactSensitiveJSON(child)
		}
	}
}

func isSensitiveErrorField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
	return normalized == "apikey" || normalized == "authorization" || normalized == "cookie" ||
		strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "password")
}

// parseRetryAfter 解析 Retry-After：整数秒或 HTTP-date。缺失或无法解析返回 0。
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if delay := deadline.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

// Forward 执行转发。返回时表示响应已完成（或已失败），调用方可释放队列 slot。
func Forward(opts *Options) error {
	upstreamURL, upstreamPath := buildUpstreamURL(opts.Provider)

	// 请求超时用 context 控制；流式响应的活跃超时单独处理（见 handleStream）
	ctx, cancel := context.WithCancel(opts.ClientReq.Context())
	defer cancel()

	timeout := time.Duration(opts.TimeoutMs) * time.Millisecond

	if opts.IsStreaming {
		// 流式路径：对 Do 阶段加 header 响应超时，避免上游接受连接但不发字节时永久阻塞
		// 拿到响应头后切换到活跃超时控制（见 handleStream）
		// header 超时优先用 HeaderTimeoutMs（直通模式），为 0 时回退到整体 TimeoutMs
		headerTimeoutMs := opts.TimeoutMs
		if opts.HeaderTimeoutMs > 0 {
			headerTimeoutMs = opts.HeaderTimeoutMs
		}
		headerTimeout := time.Duration(headerTimeoutMs) * time.Millisecond
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL+upstreamPath, bytes.NewReader(opts.UpstreamBody))
		if err != nil {
			markResponseStarted(opts)
			writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
			return err
		}
		clientUserAgent := ""
		if opts.ClientReq != nil {
			clientUserAgent = opts.ClientReq.Header.Get("User-Agent")
		}
		setUpstreamHeaders(req, opts.Provider, clientUserAgent)

		var headerTimedOut atomic.Bool
		headerTimerDone := make(chan struct{})
		headerTimer := time.AfterFunc(headerTimeout, func() {
			headerTimedOut.Store(true)
			cancel()
			close(headerTimerDone)
		})
		resp, err := opts.HTTPClient.Do(req)
		if !headerTimer.Stop() {
			<-headerTimerDone
		}
		if headerTimedOut.Load() {
			if resp != nil {
				_ = resp.Body.Close()
			}
			err = context.DeadlineExceeded
		}
		if err != nil {
			if clientRequestCanceled(opts) {
				opts.Log("客户端断开连接")
				return nil
			}
			opts.Log("转发失败: %s", err.Error())
			isHeaderTimeout := headerTimedOut.Load() || errors.Is(err, context.DeadlineExceeded)
			// 此处尚未向客户端写入任何字节，可安全放弃本次尝试交给调用方换候选。
			retryErr := err
			if isHeaderTimeout {
				retryErr = fmt.Errorf("%w: %v", ErrStreamHeaderTimeout, err)
			}
			if abandonAttempt(opts, 0, 0, retryErr) {
				return ErrAttemptAbandoned
			}
			if isHeaderTimeout {
				markResponseStarted(opts)
				writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", fmt.Sprintf("上游响应头超时 (%d秒)", headerTimeoutMs/1000))
				return err
			}
			markResponseStarted(opts)
			writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
			return err
		}
		defer resp.Body.Close()

		elapsed := time.Since(opts.StartTime).Milliseconds()
		recordUpstreamHeaders(opts, resp)
		if resp.StatusCode >= 400 {
			opts.Log("← HTTP %d [%dms]", resp.StatusCode, elapsed)
			if abandonAttempt(opts, resp.StatusCode, parseRetryAfter(resp.Header, time.Now()), nil) {
				observed, truncated := readObservedErrorBody(cancel, resp)
				recordErrorBody(opts, observed, truncated)
				return ErrAttemptAbandoned
			}
			raw, observed, truncated, readErr := readStreamErrorBody(ctx, cancel, resp.Body, opts, maxErrorBodyBytes)
			recordErrorBody(opts, observed, truncated)
			drainBody(resp)
			return handleStreamErrorPayload(ctx, resp.StatusCode, raw, readErr, opts)
		}
		opts.Log("← %d [%dms]", resp.StatusCode, elapsed)
		return handleStream(ctx, cancel, resp, opts)
	}

	// 非流式：整体超时
	ctx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL+upstreamPath, bytes.NewReader(opts.UpstreamBody))
	if err != nil {
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	clientUserAgent := ""
	if opts.ClientReq != nil {
		clientUserAgent = opts.ClientReq.Header.Get("User-Agent")
	}
	setUpstreamHeaders(req, opts.Provider, clientUserAgent)

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		if clientRequestCanceled(opts) {
			opts.Log("客户端断开连接")
			return nil
		}
		opts.Log("转发失败: %s", err.Error())
		isTimeout := errors.Is(err, context.DeadlineExceeded)
		retryErr := err
		if isTimeout {
			retryErr = fmt.Errorf("%w: %v", ErrRequestTimeout, err)
		}
		if abandonAttempt(opts, 0, 0, retryErr) {
			return ErrAttemptAbandoned
		}
		if isTimeout {
			markResponseStarted(opts)
			writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", fmt.Sprintf("上游请求超时 (%d秒)", opts.TimeoutMs/1000))
			return err
		}
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	defer resp.Body.Close()

	elapsed := time.Since(opts.StartTime).Milliseconds()
	recordUpstreamHeaders(opts, resp)
	if resp.StatusCode >= 400 {
		opts.Log("← HTTP %d [%dms]", resp.StatusCode, elapsed)
		if abandonAttempt(opts, resp.StatusCode, parseRetryAfter(resp.Header, time.Now()), nil) {
			observed, truncated := readObservedErrorBody(cancel2, resp)
			recordErrorBody(opts, observed, truncated)
			return ErrAttemptAbandoned
		}
		raw, observed, truncated, readErr := readErrorBody(resp.Body, maxErrorBodyBytes)
		recordErrorBody(opts, observed, truncated)
		drainBody(resp)
		return handleErrorPayload(ctx, resp.StatusCode, raw, readErr, opts)
	}
	opts.Log("← %d [%dms]", resp.StatusCode, elapsed)
	return handleResponse(ctx, resp, opts)
}

// handleResponse 非流式响应：读取整体，按格式转换后回写。
func handleResponse(ctx context.Context, resp *http.Response, opts *Options) error {
	raw, err := readAllLimited(resp.Body, maxResponseBodyBytes)
	if err != nil {
		opts.Log("上游响应流错误: %s", err.Error())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("上游响应体超时 (%d秒)", opts.TimeoutMs/1000)
			markResponseStarted(opts)
			writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", msg)
			return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
		}
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "parse_error", "上游响应解析失败: "+err.Error())
		return err
	}

	var result map[string]any
	var conversionErr error
	if opts.Provider.Format == "anthropic" {
		result, conversionErr = converter.ConvertAnthropicResponseChecked(data, opts.ClientFormat, opts.OriginalModel)
	} else if opts.Provider.Format == "openai" {
		result, conversionErr = converter.ConvertOpenAIChatResponseChecked(data, opts.ClientFormat, opts.OriginalModel)
	} else {
		result, conversionErr = converter.ConvertOpenAIResponsesResponseChecked(data, opts.ClientFormat, opts.OriginalModel)
	}
	if conversionErr != nil {
		opts.Log("上游响应转换失败: %s", conversionErr.Error())
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "conversion_error", "上游响应转换失败: "+conversionErr.Error())
		return fmt.Errorf("%w: %v", ErrConversion, conversionErr)
	}

	// namespace 还原：上游按展平名返回 function_call，Codex 只认
	// {name, namespace} 那一对。透传（Provider.Format == ClientFormat）时也要做，
	// 那条路径不经过任何转换器。
	if converter.RestoreNamespacedCalls(result, opts.ToolNamespaces) {
		opts.Log("已还原 %d 个 namespace 工具名", len(opts.ToolNamespaces))
	}

	out, _ := json.Marshal(result)
	markResponseStarted(opts)
	return writeResponseWithDeadline(ctx, opts.ClientRes, http.StatusOK, out)
}

// handleStream 流式响应：核心改善点。
// 用活跃超时计时器：每收到一块数据就重置；超时则 cancel ctx 断开上游，并向客户端补发合规 SSE 收尾事件。
func handleStream(ctx context.Context, cancel context.CancelFunc, resp *http.Response, opts *Options) error {
	flusher, ok := opts.ClientRes.(http.Flusher)
	if !ok {
		return fmt.Errorf("ResponseWriter 不支持 Flush，无法流式转发")
	}

	h := opts.ClientRes.Header()
	h.Set("content-type", "text/event-stream")
	h.Set("cache-control", "no-cache")
	h.Set("connection", "keep-alive")
	if opts.ClientFormat == "anthropic" {
		h.Set("anthropic-version", "2023-06-01")
	}
	markResponseStarted(opts)
	opts.ClientRes.WriteHeader(http.StatusOK)

	activityTimeout := time.Duration(opts.StreamActivityTimeout) * time.Millisecond
	if err := writeAndFlushStream(opts.ClientRes, flusher, nil, activityTimeout); err != nil {
		return finishStreamWrite(err, cancel, opts)
	}

	// watcher 独占 timer，读取循环仅发送活跃信号，避免跨 goroutine Reset timer。
	activity := make(chan struct{}, 1)
	watcherStop := make(chan struct{})
	watcherDone := make(chan struct{})
	var timedOut atomic.Bool
	go func() {
		defer close(watcherDone)
		timer := time.NewTimer(activityTimeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				timedOut.Store(true)
				cancel() // 中断上游读取
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(activityTimeout)
			case <-ctx.Done():
				return
			case <-watcherStop:
				return
			}
		}
	}()
	defer func() {
		close(watcherStop)
		<-watcherDone
	}()
	markActivity := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	// 相同格式直接透传字节流，保留完整 SSE 格式与打字机效果；不同格式逐行解析转换。
	// 但带 namespace 映射时不能走字节透传：上游返回的是展平名，必须逐行改回
	// {name, namespace}，否则 Codex 认不出自己的工具（实测 unsupported call）。
	isPassthrough := converter.IsPassthrough(opts.Provider.Format, opts.ClientFormat) &&
		len(opts.ToolNamespaces) == 0

	if isPassthrough {
		buf := make([]byte, 16*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				markActivity()
				if werr := writeAndFlushStream(opts.ClientRes, flusher, buf[:n], activityTimeout); werr != nil {
					return finishStreamWrite(werr, cancel, opts)
				}
			}
			if err != nil {
				return finishStream(err, timedOut.Load(), opts, flusher)
			}
		}
	}

	// 不同格式：逐行解析并经 transformer 转换（对齐 Node 的按 \n 切分逻辑）。
	// 手动按 \n 分行读取，并将跨格式单事件限制在 8 MiB。
	transform := converter.NewStreamTransformer(opts.Provider.Format, opts.ClientFormat)
	// 套一层 namespace 还原。放在最外层，使转换器与透传器（同格式时 IsPassthrough
	// 已被上面的条件排除，这里拿到的是 passthrough{}）都被覆盖。
	transform = converter.WithNamespaceRestore(transform, opts.ToolNamespaces)
	readBuf := make([]byte, 64*1024)
	var lineBuf []byte
	// rawCapture 仅在排查开关（OnStreamConversionError 非 nil）开启时累积原始上游 SSE。
	capture := newRawCapture(opts.OnStreamConversionError)
	for {
		n, readErr := resp.Body.Read(readBuf)
		if n > 0 {
			markActivity()
			chunk := readBuf[:n]
			capture.append(chunk)
			for {
				idx := bytes.IndexByte(chunk, '\n')
				if idx < 0 {
					// 无完整行，剩余数据暂存到 lineBuf
					if len(lineBuf)+len(chunk) > maxSSEEventBytes {
						cancel()
						return finishStream(newSSEEventTooLargeError(), false, opts, flusher)
					}
					lineBuf = append(lineBuf, chunk...)
					break
				}
				if len(lineBuf)+idx > maxSSEEventBytes {
					cancel()
					return finishStream(newSSEEventTooLargeError(), false, opts, flusher)
				}
				line := string(append(lineBuf, chunk[:idx]...))
				lineBuf = lineBuf[:0]
				chunk = chunk[idx+1:]
				line = strings.TrimRight(line, "\r")
				if line == "" {
					if werr := writeAndFlushStream(opts.ClientRes, flusher, []byte("\n"), activityTimeout); werr != nil {
						return finishStreamWrite(werr, cancel, opts)
					}
					continue
				}
				if stop, err := writeTransformedLine(transform, line, cancel, opts, flusher, activityTimeout); stop {
					capture.reportIfConversionError(transform)
					return err
				}
				if converter.StreamCompleted(transform) {
					return nil
				}
			}
		}
		if readErr != nil {
			// 流结束：lineBuf 中可能有不以 \n 结尾的最后一行
			if len(lineBuf) > 0 {
				line := strings.TrimRight(string(lineBuf), "\r")
				if line != "" {
					if stop, err := writeTransformedLine(transform, line, cancel, opts, flusher, activityTimeout); stop {
						capture.reportIfConversionError(transform)
						return err
					}
					if converter.StreamCompleted(transform) {
						return nil
					}
				}
			}
			return finishTransformedStream(transform, readErr, timedOut.Load(), opts, flusher)
		}
	}
}

// rawCapture 是流式转换失败时的原始上游 SSE 旁路缓冲。
//
// hook 为 nil 时所有方法都是空操作且不分配，让排查开关关闭（常态）零成本。
type rawCapture struct {
	hook      StreamConversionErrorFunc
	buf       []byte
	truncated bool
}

func newRawCapture(hook StreamConversionErrorFunc) *rawCapture {
	return &rawCapture{hook: hook}
}

func (c *rawCapture) append(chunk []byte) {
	if c.hook == nil || c.truncated {
		return
	}
	if len(c.buf)+len(chunk) > maxSSEEventBytes {
		// 只取填得下的部分，剩下的丢弃并标记；抓包是旁路，不能反过来打断转发。
		if room := maxSSEEventBytes - len(c.buf); room > 0 {
			c.buf = append(c.buf, chunk[:room]...)
		}
		c.truncated = true
		return
	}
	c.buf = append(c.buf, chunk...)
}

// reportIfConversionError 仅在 transformer 自身报了协议转换错误时回调。
func (c *rawCapture) reportIfConversionError(transform converter.StreamTransformer) {
	if c.hook == nil {
		return
	}
	convErr := converter.StreamError(transform)
	if convErr == nil {
		return
	}
	c.hook(c.buf, c.truncated, convErr)
}

func finishTransformedStream(transform converter.StreamTransformer, err error, timedOut bool, opts *Options, flusher http.Flusher) error {
	if converter.StreamCompleted(transform) {
		return nil
	}
	if clientRequestCanceled(opts) {
		opts.Log("客户端断开连接")
		return nil
	}

	errType := "upstream_error"
	message := "上游流式响应在协议终态前结束"
	resultErr := err
	if timedOut {
		errType = "timeout_error"
		message = fmt.Sprintf("上游流式响应超时 (%d秒无数据)", opts.StreamActivityTimeout/1000)
		resultErr = fmt.Errorf("%w: %s", errStreamActivityTimeout, message)
	} else if err != nil && err != io.EOF {
		message = "上游响应流错误: " + err.Error()
	} else {
		resultErr = fmt.Errorf("%w: %s", io.ErrUnexpectedEOF, message)
	}
	opts.Log("%s", message)
	failure := converter.AbortStream(transform, errType, message)
	if len(failure) == 0 {
		if writeErr := writeStreamFailure(opts.ClientRes, flusher, time.Duration(opts.StreamActivityTimeout)*time.Millisecond, opts.ClientFormat, errType, message); writeErr != nil {
			return writeErr
		}
		return resultErr
	}
	if writeErr := writeAndFlushStream(opts.ClientRes, flusher, []byte(strings.Join(failure, "")), time.Duration(opts.StreamActivityTimeout)*time.Millisecond); writeErr != nil {
		return writeErr
	}
	return resultErr
}

func writeTransformedLine(transform converter.StreamTransformer, line string, cancel context.CancelFunc, opts *Options, flusher http.Flusher, writeTimeout time.Duration) (bool, error) {
	out := transform.Transform(line)
	if err := converter.StreamError(transform); err != nil {
		cancel()
		opts.Log("上游流式响应转换失败: %s", err.Error())
		failure := converter.StreamFailure(transform)
		if len(failure) == 0 {
			if writeErr := writeStreamFailure(opts.ClientRes, flusher, writeTimeout, opts.ClientFormat, "conversion_error", "上游流式响应转换失败: "+err.Error()); writeErr != nil {
				return true, finishStreamWrite(writeErr, cancel, opts)
			}
			return true, fmt.Errorf("%w: %v", ErrConversion, err)
		}
		if writeErr := writeAndFlushStream(opts.ClientRes, flusher, []byte(strings.Join(failure, "")), writeTimeout); writeErr != nil {
			return true, finishStreamWrite(writeErr, cancel, opts)
		}
		return true, fmt.Errorf("%w: %v", ErrConversion, err)
	}
	for _, event := range out {
		if event == "" {
			continue
		}
		if err := writeAndFlushStream(opts.ClientRes, flusher, []byte(event), writeTimeout); err != nil {
			return true, finishStreamWrite(err, cancel, opts)
		}
	}
	return false, nil
}

// finishStream 统一处理流结束：正常 EOF / 活跃超时 / 客户端断开。
func finishStream(err error, timedOut bool, opts *Options, flusher http.Flusher) error {
	if err == nil || err == io.EOF {
		return nil // 正常结束
	}
	if timedOut {
		msg := fmt.Sprintf("上游流式响应超时 (%d秒无数据)", opts.StreamActivityTimeout/1000)
		opts.Log("%s，已主动断开", msg)
		if writeErr := writeStreamFailure(opts.ClientRes, flusher, time.Duration(opts.StreamActivityTimeout)*time.Millisecond, opts.ClientFormat, "timeout_error", msg); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("%w: %s", errStreamActivityTimeout, msg)
	}
	// context.Canceled：客户端主动断开连接，属正常结束而非异常
	if errors.Is(err, context.Canceled) {
		opts.Log("客户端断开连接")
		return nil
	}
	msg := "上游响应流错误: " + err.Error()
	opts.Log("%s", msg)
	if writeErr := writeStreamFailure(opts.ClientRes, flusher, time.Duration(opts.StreamActivityTimeout)*time.Millisecond, opts.ClientFormat, "upstream_error", msg); writeErr != nil {
		return writeErr
	}
	return err
}

// writeStreamFailure 向已进入 200 的客户端流写入对应协议的失败终态。
func writeStreamFailure(w http.ResponseWriter, f http.Flusher, timeout time.Duration, clientFormat, errType, message string) error {
	var event []byte
	switch clientFormat {
	case "anthropic":
		payload, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errType, "message": message},
		})
		event = []byte(fmt.Sprintf("event: error\ndata: %s\n\n", payload))
	case "openai-responses":
		event = responsesStreamFailure(errType, message)
	default:
		payload, _ := json.Marshal(map[string]any{
			"error": map[string]any{"type": errType, "message": message},
		})
		event = []byte(fmt.Sprintf("data: %s\n\n", payload))
	}
	return writeAndFlushStream(w, f, event, timeout)
}

func writeAndFlushStream(w http.ResponseWriter, flusher http.Flusher, payload []byte, timeout time.Duration) error {
	controller := http.NewResponseController(w)
	deadlineSet := false
	if timeout > 0 {
		err := controller.SetWriteDeadline(time.Now().Add(timeout))
		switch {
		case err == nil:
			deadlineSet = true
		case !errors.Is(err, http.ErrNotSupported):
			return err
		}
	}
	if deadlineSet {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	if err := controller.Flush(); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		flusher.Flush()
	}
	return nil
}

func finishStreamWrite(err error, cancel context.CancelFunc, opts *Options) error {
	cancel()
	if clientRequestCanceled(opts) {
		opts.Log("客户端断开连接")
		return nil
	}
	return err
}

func clientRequestCanceled(opts *Options) bool {
	return opts.ClientReq != nil && errors.Is(opts.ClientReq.Context().Err(), context.Canceled)
}

func newSSEEventTooLargeError() error {
	return fmt.Errorf("%w (%d bytes)", errSSEEventTooLarge, maxSSEEventBytes)
}

// responsesStreamFailure 生成 OpenAI Responses 协议的失败终止事件。
func responsesStreamFailure(errType, message string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed",
			"error":  map[string]any{"code": errType, "message": message},
		},
	})
	return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\n", payload))
}

// handleStreamErrorPayload 将已经读取过一次的上游错误正文回写给客户端。
func handleStreamErrorPayload(ctx context.Context, statusCode int, raw []byte, readErr error, opts *Options) error {
	timeoutMs := opts.StreamActivityTimeout
	if timeoutMs <= 0 {
		timeoutMs = opts.TimeoutMs
	}
	if clientRequestCanceled(opts) {
		opts.Log("客户端断开连接")
		return nil
	}
	if readErr != nil {
		if errors.Is(readErr, errResponseBodyTooLarge) {
			msg := readErr.Error()
			opts.Log("   错误响应读取失败: %s", msg)
			markResponseStarted(opts)
			if writeErr := writeJSONErrorWithTimeout(opts.ClientRes, http.StatusBadGateway, "upstream_error", msg, time.Duration(timeoutMs)*time.Millisecond); writeErr != nil {
				return writeErr
			}
			return readErr
		}
		if errors.Is(readErr, context.DeadlineExceeded) {
			msg := fmt.Sprintf("上游错误响应体超时 (%d秒)", timeoutMs/1000)
			opts.Log("%s", msg)
			markResponseStarted(opts)
			if writeErr := writeJSONErrorWithTimeout(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", msg, time.Duration(timeoutMs)*time.Millisecond); writeErr != nil {
				return writeErr
			}
			return readErr
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		opts.Log("   错误响应读取失败: %s", readErr.Error())
		markResponseStarted(opts)
		if writeErr := writeJSONErrorWithTimeout(opts.ClientRes, http.StatusBadGateway, "upstream_error", readErr.Error(), time.Duration(timeoutMs)*time.Millisecond); writeErr != nil {
			return writeErr
		}
		return readErr
	}
	markResponseStarted(opts)
	return writeErrorResponseWithTimeout(statusCode, raw, opts.ClientRes, opts.Log, time.Duration(timeoutMs)*time.Millisecond)
}

// handleErrorPayload 回写已经读取过的错误正文，确保最终失败时能原样回传。
func handleErrorPayload(ctx context.Context, statusCode int, raw []byte, readErr error, opts *Options) error {
	if readErr != nil {
		opts.Log("   错误响应读取失败: %s", readErr.Error())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg := "上游错误响应体超时"
			markResponseStarted(opts)
			writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", msg)
			return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
		}
		markResponseStarted(opts)
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "upstream_error", readErr.Error())
		return readErr
	}
	markResponseStarted(opts)
	return writeErrorResponse(ctx, statusCode, raw, opts.ClientRes, opts.Log)
}

func writeErrorResponse(ctx context.Context, statusCode int, raw []byte, w http.ResponseWriter, log LogFunc) error {
	if len(raw) > 300 {
		log("   错误响应: %s", raw[:300])
	} else {
		log("   错误响应: %s", raw)
	}
	return writeResponseWithDeadline(ctx, w, statusCode, raw)
}

func writeErrorResponseWithTimeout(statusCode int, raw []byte, w http.ResponseWriter, log LogFunc, timeout time.Duration) error {
	if len(raw) > 300 {
		log("   错误响应: %s", raw[:300])
	} else {
		log("   错误响应: %s", raw)
	}
	return writeResponseAtDeadline(w, statusCode, raw, time.Now().Add(timeout))
}

func writeResponseWithDeadline(ctx context.Context, w http.ResponseWriter, statusCode int, payload []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		return writeResponseAtDeadline(w, statusCode, payload, deadline)
	}
	return writeResponseAtDeadline(w, statusCode, payload, time.Time{})
}

func writeJSONErrorWithTimeout(w http.ResponseWriter, status int, errType, message string, timeout time.Duration) error {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
	return writeResponseAtDeadline(w, status, payload, time.Now().Add(timeout))
}

func writeResponseAtDeadline(w http.ResponseWriter, statusCode int, payload []byte, deadline time.Time) error {
	controller := http.NewResponseController(w)
	deadlineSet := false
	if !deadline.IsZero() {
		err := controller.SetWriteDeadline(deadline)
		switch {
		case err == nil:
			deadlineSet = true
		case !errors.Is(err, http.ErrNotSupported):
			return err
		}
	}
	if deadlineSet {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(statusCode)
	_, err := w.Write(payload)
	return err
}

// readAllLimited 最多读取 limit+1 字节，用额外一字节区分刚好达到上限和超限。
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", errResponseBodyTooLarge, limit)
	}
	return raw, nil
}

// writeJSONError 代理内部使用 httputil 共享实现。
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	httputil.WriteJSONError(w, status, errType, msg)
}
