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
	maxSSEEventBytes     = 8 << 20
)

var (
	errResponseBodyTooLarge  = errors.New("上游响应体超过大小限制")
	errSSEEventTooLarge      = errors.New("上游 SSE 事件超过大小限制")
	errStreamActivityTimeout = errors.New("流式传输活跃超时")
)

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
			writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
			return err
		}
		setUpstreamHeaders(req, opts.Provider)

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
			if headerTimedOut.Load() || errors.Is(err, context.DeadlineExceeded) {
				writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", fmt.Sprintf("上游响应头超时 (%d秒)", headerTimeoutMs/1000))
				return err
			}
			writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
			return err
		}
		defer resp.Body.Close()

		elapsed := time.Since(opts.StartTime).Milliseconds()
		if resp.StatusCode >= 400 {
			opts.Log("← HTTP %d [%dms]", resp.StatusCode, elapsed)
			return handleStreamError(ctx, cancel, resp, opts)
		}
		opts.Log("← %d [%dms]", resp.StatusCode, elapsed)
		return handleStream(ctx, cancel, resp, opts)
	}

	// 非流式：整体超时
	ctx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL+upstreamPath, bytes.NewReader(opts.UpstreamBody))
	if err != nil {
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	setUpstreamHeaders(req, opts.Provider)

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		if clientRequestCanceled(opts) {
			opts.Log("客户端断开连接")
			return nil
		}
		opts.Log("转发失败: %s", err.Error())
		if errors.Is(err, context.DeadlineExceeded) {
			writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", fmt.Sprintf("上游请求超时 (%d秒)", opts.TimeoutMs/1000))
			return err
		}
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	defer resp.Body.Close()

	elapsed := time.Since(opts.StartTime).Milliseconds()
	if resp.StatusCode >= 400 {
		opts.Log("← HTTP %d [%dms]", resp.StatusCode, elapsed)
		return handleError(ctx, resp, opts.ClientRes, opts.Log)
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
			writeJSONError(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", msg)
			return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
		}
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "parse_error", "上游响应解析失败: "+err.Error())
		return err
	}

	var result map[string]any
	var conversionErr error
	if opts.Provider.Format == "anthropic" {
		result, conversionErr = converter.ConvertAnthropicResponseChecked(data, opts.ClientFormat, opts.OriginalModel)
	} else {
		result, conversionErr = converter.ConvertOpenAIChatResponseChecked(data, opts.ClientFormat, opts.OriginalModel)
	}
	if conversionErr != nil {
		opts.Log("上游响应转换失败: %s", conversionErr.Error())
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "conversion_error", "上游响应转换失败: "+conversionErr.Error())
		return conversionErr
	}

	out, _ := json.Marshal(result)
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
	isPassthrough := converter.IsPassthrough(opts.Provider.Format, opts.ClientFormat)

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
	readBuf := make([]byte, 64*1024)
	var lineBuf []byte
	for {
		n, readErr := resp.Body.Read(readBuf)
		if n > 0 {
			markActivity()
			chunk := readBuf[:n]
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
			return true, err
		}
		if writeErr := writeAndFlushStream(opts.ClientRes, flusher, []byte(strings.Join(failure, "")), writeTimeout); writeErr != nil {
			return true, finishStreamWrite(writeErr, cancel, opts)
		}
		return true, err
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

func handleStreamError(ctx context.Context, cancel context.CancelFunc, resp *http.Response, opts *Options) error {
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
	raw, err := readAllLimited(resp.Body, maxErrorBodyBytes)
	if !timer.Stop() {
		<-timerDone
	}
	if clientRequestCanceled(opts) {
		opts.Log("客户端断开连接")
		return nil
	}
	if timedOut.Load() {
		msg := fmt.Sprintf("上游错误响应体超时 (%d秒)", timeoutMs/1000)
		opts.Log("%s", msg)
		if writeErr := writeJSONErrorWithTimeout(opts.ClientRes, http.StatusGatewayTimeout, "timeout_error", msg, time.Duration(timeoutMs)*time.Millisecond); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		opts.Log("   错误响应读取失败: %s", err.Error())
		if writeErr := writeJSONErrorWithTimeout(opts.ClientRes, http.StatusBadGateway, "upstream_error", err.Error(), time.Duration(timeoutMs)*time.Millisecond); writeErr != nil {
			return writeErr
		}
		return err
	}
	return writeErrorResponseWithTimeout(resp.StatusCode, raw, opts.ClientRes, opts.Log, time.Duration(timeoutMs)*time.Millisecond)
}

// handleError 上游错误响应：原样回传状态码与响应体。
func handleError(ctx context.Context, resp *http.Response, w http.ResponseWriter, log LogFunc) error {
	raw, err := readAllLimited(resp.Body, maxErrorBodyBytes)
	if err != nil {
		log("   错误响应读取失败: %s", err.Error())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg := "上游错误响应体超时"
			writeJSONError(w, http.StatusGatewayTimeout, "timeout_error", msg)
			return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
		}
		writeJSONError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}
	return writeErrorResponse(ctx, resp.StatusCode, raw, w, log)
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
