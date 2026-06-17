// Package proxy 对齐 Node 版 lib/proxy.js：转发请求到上游，处理流式/非流式响应。
//
// 相比 Node 版手搓 setTimeout + proxyRes.destroy + streamCtx 标志，Go 版用 context 统一管理
// 请求超时与活跃超时，流式转发用单独 goroutine，取消/超时通过 ctx 一处收口，不会有 slot 泄漏。
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
)

// LogFunc 日志函数，绑定了 reqId
type LogFunc func(format string, args ...any)

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
	StreamActivityTimeout int
	HTTPClient            *http.Client
}

// Forward 执行转发。返回时表示响应已完成（或已失败），调用方可释放队列 slot。
func Forward(opts *Options) error {
	upstreamURL, upstreamPath := buildUpstreamURL(opts.Provider)

	// 请求超时用 context 控制；流式响应的活跃超时单独处理（见 handleStream）
	ctx, cancel := context.WithCancel(opts.ClientReq.Context())
	defer cancel()

	// 非流式才设整体超时；流式靠活跃超时，避免长响应被整体超时误杀
	if !opts.IsStreaming {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
		defer c2()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL+upstreamPath, bytes.NewReader(opts.UpstreamBody))
	if err != nil {
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	setUpstreamHeaders(req, opts.Provider)

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		opts.Log("转发失败: %s", err.Error())
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "proxy_error", err.Error())
		return err
	}
	defer resp.Body.Close()

	elapsed := time.Since(opts.StartTime).Milliseconds()
	if resp.StatusCode >= 400 {
		opts.Log("← HTTP %d [%dms]", resp.StatusCode, elapsed)
		return handleError(resp, opts.ClientRes, opts.Log)
	}
	opts.Log("← %d [%dms]", resp.StatusCode, elapsed)

	if !opts.IsStreaming {
		return handleResponse(resp, opts)
	}
	return handleStream(ctx, cancel, resp, opts)
}

// handleResponse 非流式响应：读取整体，按格式转换后回写。
func handleResponse(resp *http.Response, opts *Options) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		opts.Log("上游响应流错误: %s", err.Error())
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "upstream_error", err.Error())
		return err
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		writeJSONError(opts.ClientRes, http.StatusBadGateway, "parse_error", "上游响应解析失败: "+err.Error())
		return err
	}

	var result map[string]any
	if opts.Provider.Format == "anthropic" {
		result = converter.ConvertAnthropicResponse(data, opts.ClientFormat, opts.OriginalModel)
	} else {
		result = converter.ConvertOpenAIChatResponse(data, opts.ClientFormat, opts.OriginalModel)
	}

	out, _ := json.Marshal(result)
	opts.ClientRes.Header().Set("content-type", "application/json")
	opts.ClientRes.WriteHeader(http.StatusOK)
	_, err = opts.ClientRes.Write(out)
	return err
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
	flusher.Flush()

	activityTimeout := time.Duration(opts.StreamActivityTimeout) * time.Millisecond

	// 活跃超时计时器：到期 cancel ctx，从而中断下面的 Read
	timer := time.NewTimer(activityTimeout)
	defer timer.Stop()
	timedOut := false
	go func() {
		select {
		case <-timer.C:
			timedOut = true
			cancel() // 中断上游读取
		case <-ctx.Done():
		}
	}()

	// 相同格式直接透传字节流，保留完整 SSE 格式与打字机效果；不同格式逐行解析转换。
	isPassthrough := (opts.Provider.Format == "anthropic" && opts.ClientFormat == "anthropic") ||
		(opts.Provider.Format == "openai" && opts.ClientFormat == "openai-chat")

	if isPassthrough {
		buf := make([]byte, 16*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				timer.Reset(activityTimeout)
				if _, werr := opts.ClientRes.Write(buf[:n]); werr != nil {
					cancel()
					return werr
				}
				flusher.Flush()
			}
			if err != nil {
				return finishStream(err, timedOut, opts, flusher)
			}
		}
	}

	// 不同格式：逐行解析并经 transformer 转换（对齐 Node 的按 \n 切分逻辑）。
	transform := converter.NewStreamTransformer(opts.Provider.Format, opts.ClientFormat)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 放大单行上限，应对大块 SSE
	for scanner.Scan() {
		timer.Reset(activityTimeout)
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			// 保留空行分隔（对齐 Node：clientRes.write('\n')）
			if _, werr := opts.ClientRes.Write([]byte("\n")); werr != nil {
				cancel()
				return werr
			}
			flusher.Flush()
			continue
		}
		for _, out := range transform.Transform(line) {
			if out == "" {
				continue
			}
			if _, werr := opts.ClientRes.Write([]byte(out)); werr != nil {
				cancel()
				return werr
			}
		}
		flusher.Flush()
	}
	return finishStream(scanner.Err(), timedOut, opts, flusher)
}

// finishStream 统一处理流结束：正常 EOF / 活跃超时 / 客户端断开。
func finishStream(err error, timedOut bool, opts *Options, flusher http.Flusher) error {
	if err == nil || err == io.EOF {
		return nil // 正常结束
	}
	if timedOut {
		opts.Log("上游流式响应超时，已主动断开 (%d秒无数据)", opts.StreamActivityTimeout/1000)
		gracefulSSEClose(opts.ClientRes, flusher, opts.ClientFormat, opts.StreamActivityTimeout)
		return nil
	}
	return err // 客户端断开或其它读取错误
}

// gracefulSSEClose 向客户端补发合规 SSE 收尾事件，避免截断流（对齐 Node 版改动）。
func gracefulSSEClose(w http.ResponseWriter, f http.Flusher, clientFormat string, timeoutMs int) {
	if clientFormat == "anthropic" {
		msg := fmt.Sprintf("上游流式响应超时 (%d秒无数据)", timeoutMs/1000)
		payload, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "timeout_error", "message": msg},
		})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	} else {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	f.Flush()
}

// handleError 上游错误响应：原样回传状态码与响应体。
func handleError(resp *http.Response, w http.ResponseWriter, log LogFunc) error {
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 300 {
		log("   错误响应: %s", raw[:300])
	} else {
		log("   错误响应: %s", raw)
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err := w.Write(raw)
	return err
}

func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errType, "message": msg},
	})
	w.Write(payload)
}
