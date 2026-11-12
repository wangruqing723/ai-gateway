package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func TestDiagnosticKnownCredentialBeforeTruncation(t *testing.T) {
	secret := "private-credential-abcdef"
	raw := strings.Repeat("x", maxObservedErrorBodyBytes-5) + secret
	_, observed, truncated, _ := readErrorBody(strings.NewReader(raw), maxErrorBodyBytes, secret)
	if !truncated || strings.Contains(observed, "priva") {
		t.Fatalf("截断泄露: %q", observed)
	}
}

func TestObserverEOFError(t *testing.T) {
	var got ErrorDiagnostic
	o := &streamErrorObserver{opts: &Options{Provider: &config.Provider{Format: "openai-responses"}, OnDiagnostic: func(d ErrorDiagnostic) { got = d }}}
	o.feed([]byte("data: {\"type\":\"error\",\"code\":\"broken\",\"message\":\"failed\"}"))
	o.finish()
	if got.Code != "broken" {
		t.Fatalf("未捕获EOF: %+v", got)
	}
}

func TestPassthroughErrorKeepsClientBytes(t *testing.T) {
	payload := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"error\",\"code\":\"upstream_http2_stream_error\",\"message\":\"Upstream HTTP/2 stream failed\"}\n\n"
	upstream := newStreamServer(nil, func(w http.ResponseWriter) { io.WriteString(w, payload) })
	defer upstream.Close()
	recorder := httptest.NewRecorder()
	opts := forwardTestOptions(upstream, recorder, "openai-responses", "openai-responses", true, 500, 500)
	var got ErrorDiagnostic
	opts.OnDiagnostic = func(d ErrorDiagnostic) { got = d }
	retries := 0
	opts.ShouldRetry = func(int, time.Duration, error) bool { retries++; return false }
	if err := Forward(opts); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || recorder.Body.String() != payload || retries != 0 || got.Source != "upstream" {
		t.Fatalf("状态=%d 重试=%d 诊断=%+v 正文=%s", recorder.Code, retries, got, recorder.Body.String())
	}
}

func TestErrorObserverChunkBoundaries(t *testing.T) {
	payload := "event: error\r\ndata: {\"type\":\"error\",\r\ndata: \"code\":\"upstream_http2_stream_error\",\"message\":\"Upstream HTTP/2 stream failed\"}\r\n\r\n"
	for size := 1; size < 30; size++ {
		var got []ErrorDiagnostic
		o := &streamErrorObserver{opts: &Options{Provider: &config.Provider{Format: "openai-responses"}, OnDiagnostic: func(d ErrorDiagnostic) { got = append(got, d) }}}
		for i := 0; i < len(payload); i += size {
			end := i + size
			if end > len(payload) {
				end = len(payload)
			}
			o.feed([]byte(payload[i:end]))
		}
		if len(got) != 1 || got[0].Code != "upstream_http2_stream_error" || got[0].BodyState != "available" {
			t.Fatalf("分块=%d: %+v", size, got)
		}
	}
}

func TestErrorObserverHugeOutputExcluded(t *testing.T) {
	var got ErrorDiagnostic
	o := &streamErrorObserver{opts: &Options{Provider: &config.Provider{Format: "openai-responses"}, OnDiagnostic: func(d ErrorDiagnostic) { got = d }}}
	o.feed([]byte("data: {\"type\":\"response.failed\",\"response\":{\"output\":[\"" + strings.Repeat("private", 20000) + "\"],\"error\":{\"code\":\"token_limit\",\"message\":\"denied\"}}}\n\n"))
	if got.Code != "token_limit" || strings.Contains(got.Body, "private") || len(got.Body) > 8192 {
		t.Fatalf("不应记录成功内容: %+v", got)
	}
}

func TestDiagnosticProtocolMatrix(t *testing.T) {
	for _, format := range []string{"anthropic", "openai", "openai-responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(format+fmt.Sprint(stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(403)
					io.WriteString(w, `{"error":{"type":"permission_error","code":"denied","message":"Bearer private-token","details":{"password":"private"}}}`)
				}))
				defer server.Close()
				out := httptest.NewRecorder()
				client := format
				if format == "openai" {
					client = "openai-chat"
				}
				opts := forwardTestOptions(server, out, format, client, stream, 500, 500)
				var d ErrorDiagnostic
				opts.OnDiagnostic = func(v ErrorDiagnostic) { d = v }
				_ = Forward(opts)
				if d.Source != "upstream" || d.Code != "denied" || strings.Contains(d.Body, "private") || d.BodyState != "available" {
					t.Fatalf("诊断错误: %+v", d)
				}
			})
		}
	}
}

func TestDiagnosticCredentialEncodingAndCompleteMessage(t *testing.T) {
	if got := SanitizeDiagnostic("status", "secret-long"); got != "status" {
		t.Fatalf("误改完整消息: %s", got)
	}
	secret := "credential\"with\\escape"
	raw, _ := json.Marshal(map[string]any{"message": secret})
	opts := &Options{Provider: &config.Provider{APIKey: secret}}
	opts.OnDiagnostic = func(d ErrorDiagnostic) {
		if strings.Contains(d.Body, "credential") {
			t.Fatalf("转义凭据泄漏: %s", d.Body)
		}
	}
	reportDiagnostic(opts, ErrorDiagnostic{Body: string(raw)})
}

func TestDiagnosticTransportAndClient(t *testing.T) {
	req := httptest.NewRequest("POST", "http://localhost", nil)
	var d ErrorDiagnostic
	opts := &Options{Provider: &config.Provider{}, ClientReq: req, OnDiagnostic: func(v ErrorDiagnostic) { d = v }}
	classifyDiagnosticError(opts, context.DeadlineExceeded)
	if d.Source != "transport" {
		t.Fatalf("超时来源: %+v", d)
	}
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	opts.ClientReq = req.WithContext(ctx)
	classifyDiagnosticError(opts, nil)
	if d.Source != "client" {
		t.Fatalf("取消来源: %+v", d)
	}
}
