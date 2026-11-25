package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestForwardBeforeRequestCallbackSeesFinalRequest(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "非流式"
		if streaming {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			var callbackSeen atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !callbackSeen.Load() {
					t.Error("HTTPClient.Do 前没有调用采集回调")
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
				}
				if r.Header.Get("User-Agent") != "provider-agent" {
					t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
				}
				if r.Header.Get("X-Global-Extra") != "global" || r.Header.Get("X-Provider-Extra") != "provider" {
					t.Errorf("扩展头未到达上游: %#v", r.Header)
				}
				if streaming {
					w.Header().Set("content-type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("content-type", "application/json")
				_, _ = io.WriteString(w, `{"id":"ok","choices":[]}`)
			}))
			defer upstream.Close()

			opts := forwardTestOptions(upstream, httptest.NewRecorder(), "openai", "openai-chat", streaming, 500, 500)
			opts.ClientReq.Header.Set("User-Agent", "client-agent")
			opts.Provider.UserAgent = "provider-agent"
			opts.Provider.ExtraHeaders = map[string]string{"X-Provider-Extra": "provider"}
			opts.GlobalUserAgent = "global-agent"
			opts.GlobalHeaders = map[string]string{"X-Global-Extra": "global"}
			opts.OnBeforeRequest = func(method, requestURL string, headers http.Header, body []byte) {
				callbackSeen.Store(true)
				if method != http.MethodPost {
					t.Errorf("method = %q, 期望 POST", method)
				}
				if requestURL != upstream.URL+"/v1/chat/completions" {
					t.Errorf("URL = %q", requestURL)
				}
				if headers.Get("Authorization") != "Bearer test-key" || headers.Get("User-Agent") != "provider-agent" ||
					headers.Get("X-Global-Extra") != "global" || headers.Get("X-Provider-Extra") != "provider" {
					t.Errorf("采集头不是最终请求头: %#v", headers)
				}
				if string(body) != `{}` {
					t.Errorf("采集请求体 = %q", body)
				}
			}
			if err := Forward(opts); err != nil {
				t.Fatalf("Forward() error = %v", err)
			}
			if !callbackSeen.Load() {
				t.Fatal("没有调用采集回调")
			}
		})
	}
}

func TestBeforeRequestCallbackIsOptional(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","choices":[]}`)
	}))
	defer upstream.Close()

	opts := forwardTestOptions(upstream, httptest.NewRecorder(), "openai", "openai-chat", false, 500, 500)
	opts.OnBeforeRequest = nil
	if err := Forward(opts); err != nil {
		t.Fatalf("回调为空时 Forward() error = %v", err)
	}
}
