package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hookCall 记录一次 ShouldRetry 调用的入参，便于断言分类结果。
type hookCall struct {
	upstreamCode int
	retryAfter   time.Duration
	err          error
}

// recordingHook 返回一个记录入参并按 decide 决策的钩子。
func recordingHook(decide bool, calls *[]hookCall) ShouldRetryFunc {
	return func(upstreamCode int, retryAfter time.Duration, err error) bool {
		*calls = append(*calls, hookCall{upstreamCode: upstreamCode, retryAfter: retryAfter, err: err})
		return decide
	}
}

// deadUpstream 造一个监听后立即关闭的服务，保证连接必然失败（传输层错误）。
func deadUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	// Close 后 URL 仍可用于构造请求，但连接会被拒绝。
	srv.URL = url
	return srv
}

func TestShouldRetryStreamTransportError(t *testing.T) {
	upstream := deadUpstream(t)

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if calls[0].upstreamCode != 0 {
		t.Fatalf("upstreamCode = %d, want 0（传输层失败无状态码）", calls[0].upstreamCode)
	}
	if calls[0].err == nil {
		t.Fatal("err = nil, want 传输层错误")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃尝试时写入了 %d 字节，want 0", recorder.Body.Len())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("放弃尝试时写了状态码 %d，want 未写入（recorder 默认 200）", recorder.Code)
	}
}

func TestShouldRetryStreamHeaderTimeoutWrapsSentinel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 40, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if !errors.Is(calls[0].err, ErrStreamHeaderTimeout) {
		t.Fatalf("err = %v, want 包裹 ErrStreamHeaderTimeout", calls[0].err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃尝试时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

func TestShouldRetryStreamUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if calls[0].upstreamCode != http.StatusTooManyRequests {
		t.Fatalf("upstreamCode = %d, want 429", calls[0].upstreamCode)
	}
	if calls[0].retryAfter != 3*time.Second {
		t.Fatalf("retryAfter = %s, want 3s", calls[0].retryAfter)
	}
	if calls[0].err != nil {
		t.Fatalf("err = %v, want nil（有响应时不带传输错误）", calls[0].err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃尝试时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

func TestShouldRetryNonStreamTransportError(t *testing.T) {
	upstream := deadUpstream(t)

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if calls[0].upstreamCode != 0 || calls[0].err == nil {
		t.Fatalf("calls[0] = %+v, want code 0 且带错误", calls[0])
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃尝试时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

func TestShouldRetryNonStreamUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
	}))
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if !errors.Is(err, ErrAttemptAbandoned) {
		t.Fatalf("Forward() error = %v, want ErrAttemptAbandoned", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if calls[0].upstreamCode != http.StatusBadGateway {
		t.Fatalf("upstreamCode = %d, want 502", calls[0].upstreamCode)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("放弃尝试时写入了 %d 字节，want 0", recorder.Body.Len())
	}
}

// 钩子返回 false 时必须退回既有写响应路径，保证配置未开启转移的行为不变。
func TestShouldRetryFalseKeepsLegacyResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
	}))
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 500, 500)
	opts.ShouldRetry = recordingHook(false, &calls)

	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 1", len(calls))
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 原样透传", recorder.Code)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("body 为空，want 上游错误体透传")
	}
}

// ShouldRetry 为 nil 时行为与旧版本完全一致。
func TestNilShouldRetryUnchangedBehaviour(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", false, 500, 500)

	if err := Forward(opts); err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "unavailable") {
		t.Fatalf("body = %q, want 上游错误体", recorder.Body.String())
	}
}

// 流式一旦开始写出字节就不再询问钩子，这是硬边界。
func TestShouldRetryNotCalledAfterStreamWritesBegin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		// 写出部分数据后直接断开，模拟流中途失败。
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer upstream.Close()

	var calls []hookCall
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai", "openai-chat", true, 500, 500)
	opts.ShouldRetry = recordingHook(true, &calls)

	err := Forward(opts)
	if errors.Is(err, ErrAttemptAbandoned) {
		t.Fatal("Forward() 返回 ErrAttemptAbandoned，但字节已写出，不应放弃")
	}
	if len(calls) != 0 {
		t.Fatalf("ShouldRetry 调用次数 = %d, want 0（已开写不得询问）", len(calls))
	}
	if !strings.Contains(recorder.Body.String(), "data: first") {
		t.Fatalf("body = %q, want 已写出的 SSE 数据", recorder.Body.String())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺失", value: "", want: 0},
		{name: "整数秒", value: "7", want: 7 * time.Second},
		{name: "零秒", value: "0", want: 0},
		{name: "负数按 0", value: "-5", want: 0},
		{name: "无法解析", value: "soon", want: 0},
		{name: "HTTP-date 未来", value: now.Add(12 * time.Second).Format(http.TimeFormat), want: 12 * time.Second},
		{name: "HTTP-date 过去按 0", value: now.Add(-30 * time.Second).Format(http.TimeFormat), want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			if got := parseRetryAfter(h, now); got != tc.want {
				t.Fatalf("parseRetryAfter() = %s, want %s", got, tc.want)
			}
		})
	}
}
