package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/providerhealth"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/router"
	"ai-gateway/internal/vision"
	"ai-gateway/internal/webbuild"
)

const (
	testMaxConfigBodyBytes = 1 << 20
	testMaxProxyBodyBytes  = 32 << 20
)

func newBoundaryTestServer() *server {
	return &server{
		cfg: &config.Config{
			Host:      "127.0.0.1",
			Port:      7789,
			Providers: map[string]*config.Provider{},
			Routes:    []config.Route{},
		},
		qm:             queue.NewManager(),
		httpClient:     http.DefaultClient,
		metrics:        metrics.NewCollector(10),
		providerHealth: providerhealth.NewChecker(),
	}
}

func TestForwardAttemptCanceledQueueUsesClientDisconnectedReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "http://client.test/v1/messages", nil).WithContext(ctx)
	provider := &config.Provider{
		Name: "primary", BaseURL: "http://127.0.0.1:1", Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000,
	}
	detail := metrics.AttemptDetail{}
	reqLog := metrics.RequestLog{}
	srv := &server{qm: queue.NewManager(), httpClient: http.DefaultClient}
	outcome := srv.forwardAttempt(httptest.NewRecorder(), request, forwardAttemptInput{
		cfg:          &config.Config{DirectMode: false},
		start:        time.Now(),
		clientFormat: "anthropic",
		internal: converter.FromAnthropic(map[string]any{
			"model": "client-model", "messages": []any{map[string]any{"role": "user", "content": "你好"}},
		}),
		rawBody:   map[string]any{"model": "client-model", "messages": []any{}},
		candidate: router.Candidate{Provider: provider, TargetModel: "upstream-model"},
		reqLog:    &reqLog,
		detail:    &detail,
	})
	if detail.Reason == "queue_timeout" || detail.Reason != "client_disconnected" {
		t.Fatalf("队列取消原因 = %q，期望 client_disconnected", detail.Reason)
	}
	if detail.Outcome != "skipped" {
		t.Fatalf("队列取消结果 = %q，期望 skipped", detail.Outcome)
	}
	if outcome.abandoned || outcome.requestStarted {
		t.Fatalf("取消的队列结果 = %#v", outcome)
	}
}

func TestHandleMethodGuards(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantAllow  string
	}{
		{name: "proxy get", method: http.MethodGet, path: "/v1/messages", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		{name: "metrics post", method: http.MethodPost, path: "/api/metrics", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodGet},
		{name: "raw config post", method: http.MethodPost, path: "/api/config/raw", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodGet},
		{name: "unknown head", method: http.MethodHead, path: "/does-not-exist", wantStatus: http.StatusNotFound},
		{name: "health head", method: http.MethodHead, path: "/health", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, "http://127.0.0.1:7789"+tt.path, nil)
			srv.handle(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantAllow != "" && recorder.Header().Get("Allow") != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", recorder.Header().Get("Allow"), tt.wantAllow)
			}
		})
	}
}

func TestHealthHeadUsesGetRepresentationHeadersWithoutBody(t *testing.T) {
	srv := newBoundaryTestServer()
	getRecorder := httptest.NewRecorder()
	srv.handle(getRecorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	headRecorder := httptest.NewRecorder()
	srv.handle(headRecorder, httptest.NewRequest(http.MethodHead, "http://127.0.0.1:7789/health", nil))

	if headRecorder.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headRecorder.Code)
	}
	if headRecorder.Body.Len() != 0 {
		t.Fatalf("HEAD body must be empty, got %q", headRecorder.Body.String())
	}
	if got, want := headRecorder.Header().Get("Content-Type"), getRecorder.Header().Get("Content-Type"); got == "" || got != want {
		t.Errorf("HEAD Content-Type = %q, want GET value %q", got, want)
	}
	if length, err := strconv.Atoi(headRecorder.Header().Get("Content-Length")); err != nil || length <= 0 {
		t.Errorf("HEAD Content-Length = %q, want a positive representation length", headRecorder.Header().Get("Content-Length"))
	}
}

func TestObservationEndpointsUseHostAllowlist(t *testing.T) {
	endpoints := []string{"/health", "/api/metrics", "/api/logs"}
	tests := []struct {
		name        string
		requestHost string
		configHost  string
		wantStatus  int
	}{
		{name: "ipv4 loopback", requestHost: "127.0.0.42:7789", configHost: "127.0.0.1", wantStatus: http.StatusOK},
		{name: "localhost", requestHost: "localhost:7789", configHost: "127.0.0.1", wantStatus: http.StatusOK},
		{name: "ipv6 loopback", requestHost: "[::1]:7789", configHost: "127.0.0.1", wantStatus: http.StatusOK},
		{name: "configured host", requestHost: "Gateway.Example:7789", configHost: "gateway.example", wantStatus: http.StatusOK},
		{name: "wildcard listener accepts ipv4 literal", requestHost: "192.0.2.10:7789", configHost: "0.0.0.0", wantStatus: http.StatusOK},
		{name: "wildcard listener accepts ipv6 literal", requestHost: "[2001:db8::10]:7789", configHost: "::", wantStatus: http.StatusOK},
		{name: "external host rejected", requestHost: "evil.example.com", configHost: "127.0.0.1", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		for _, endpoint := range endpoints {
			t.Run(tt.name+" "+endpoint, func(t *testing.T) {
				srv := newBoundaryTestServer()
				srv.cfg.Host = tt.configHost
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "http://"+tt.requestHost+endpoint, nil)
				srv.handle(recorder, request)
				if recorder.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
				}
				if tt.wantStatus == http.StatusForbidden {
					var body struct {
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
						t.Fatalf("forbidden response is not JSON: %v", err)
					}
					if body.Error.Type != "forbidden_host" || body.Error.Message != "拒绝非本机 Host 的请求" {
						t.Fatalf("forbidden response = %#v, want forbidden_host error", body.Error)
					}
				}
			})
		}
	}
}

func TestHealthStatsAreCachedAndConcurrentRefreshCoalesces(t *testing.T) {
	cacheSpy := &healthStatsCacheSpy{stats: cache.Stats{Total: 7, ContentSize: 128, DBSize: 256}}
	srv := newBoundaryTestServer()
	srv.cache = cacheSpy

	const requests = 32
	start := make(chan struct{})
	statuses := make(chan int, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recorder := httptest.NewRecorder()
			srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
			statuses <- recorder.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent /health status = %d, want 200", status)
		}
	}
	if got := cacheSpy.StatsCalls(); got != 1 {
		t.Fatalf("concurrent /health GetStats calls = %d, want 1 within TTL", got)
	}

	srv.healthStatsMu.Lock()
	srv.healthStatsAt = time.Now().Add(-healthStatsCacheTTL)
	srv.healthStatsMu.Unlock()
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expired /health status = %d, want 200", recorder.Code)
	}
	if got := cacheSpy.StatsCalls(); got != 2 {
		t.Fatalf("expired /health GetStats calls = %d, want 2", got)
	}
}

func TestManagementMutationsRejectUntrustedBrowserOrigins(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		target       string
		origin       string
		secFetchSite string
		contentType  string
		body         string
	}{
		{name: "foreign config PUT", method: http.MethodPut, target: "http://127.0.0.1:7789/api/config", origin: "https://attacker.example", contentType: "application/yaml", body: "{}"},
		{name: "rebound config PUT", method: http.MethodPut, target: "http://attacker.example/api/config", origin: "http://attacker.example", secFetchSite: "same-origin", contentType: "application/yaml", body: "{}"},
		{name: "foreign reload", method: http.MethodPost, target: "http://127.0.0.1:7789/api/config/reload", origin: "https://attacker.example"},
		{name: "foreign provider health", method: http.MethodPost, target: "http://127.0.0.1:7789/api/providers/health", origin: "https://attacker.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			request.Header.Set("Origin", tt.origin)
			if tt.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", tt.secFetchSite)
			}
			if tt.contentType != "" {
				request.Header.Set("Content-Type", tt.contentType)
			}
			srv.handle(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandleRejectsCrossSiteInference(t *testing.T) {
	tests := []struct {
		name   string
		target string
		header http.Header
	}{
		{name: "sec fetch site", target: "http://127.0.0.1:7789/v1/messages", header: http.Header{"Sec-Fetch-Site": {"cross-site"}}},
		{name: "foreign origin", target: "http://127.0.0.1:7789/v1/messages", header: http.Header{"Origin": {"https://attacker.example"}}},
		{name: "dns rebinding same origin", target: "http://attacker.example/v1/messages", header: http.Header{"Origin": {"http://attacker.example"}, "Sec-Fetch-Site": {"same-origin"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(`{}`))
			request.Header = tt.header.Clone()
			request.Header.Set("Content-Type", "application/json")
			srv.handle(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandleAllowsSameOriginAndCLIInference(t *testing.T) {
	tests := []struct {
		name   string
		target string
		origin string
	}{
		{name: "same local origin", target: "http://127.0.0.1:7789/v1/messages", origin: "http://127.0.0.1:7789"},
		{name: "localhost origin", target: "http://localhost:7789/v1/messages", origin: "http://localhost:7789"},
		{name: "no browser origin", target: "http://127.0.0.1:7789/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				request.Header.Set("Origin", tt.origin)
			}
			srv.handle(recorder, request)
			if recorder.Code == http.StatusForbidden {
				t.Fatalf("same-origin/CLI request was rejected: %s", recorder.Body.String())
			}
		})
	}
}

func TestHandleRequiresJSONForInference(t *testing.T) {
	for _, contentType := range []string{"", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(`{}`))
			if contentType != "" {
				request.Header.Set("Content-Type", contentType)
			}
			srv.handle(recorder, request)
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandleBodyLimits(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		srv := newBoundaryTestServer()
		recorder := httptest.NewRecorder()
		body := io.LimitReader(zeroReader{}, testMaxProxyBodyBytes+1)
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", body)
		request.Header.Set("Content-Type", "application/json")
		srv.handle(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("config", func(t *testing.T) {
		srv := newBoundaryTestServer()
		recorder := httptest.NewRecorder()
		body := io.LimitReader(zeroReader{}, testMaxConfigBodyBytes+1)
		request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7789/api/config", body)
		request.Header.Set("Content-Type", "application/yaml")
		srv.handle(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("content length rejects before read", func(t *testing.T) {
		tests := []struct {
			name        string
			method      string
			path        string
			contentType string
			limit       int64
		}{
			{name: "proxy", method: http.MethodPost, path: "/v1/messages", contentType: "application/json", limit: testMaxProxyBodyBytes},
			{name: "config", method: http.MethodPut, path: "/api/config", contentType: "application/yaml", limit: testMaxConfigBodyBytes},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				body := &countingEOFReader{}
				request := httptest.NewRequest(tt.method, "http://127.0.0.1:7789"+tt.path, body)
				request.Header.Set("Content-Type", tt.contentType)
				request.ContentLength = tt.limit + 1
				recorder := httptest.NewRecorder()
				newBoundaryTestServer().handle(recorder, request)
				if recorder.Code != http.StatusRequestEntityTooLarge || body.reads != 0 {
					t.Fatalf("status/reads = %d/%d, want 413 without reading body", recorder.Code, body.reads)
				}
			})
		}
	})
}

func TestSlowConfigBodyDoesNotBlockOtherConfigOperations(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
	body := newBlockingBody()
	putDone := make(chan struct{})
	go func() {
		defer close(putDone)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7789/api/config", body)
		request.Header.Set("Content-Type", "application/yaml")
		srv.handle(recorder, request)
	}()

	select {
	case <-body.started:
	case <-time.After(time.Second):
		close(body.release)
		t.Fatal("slow PUT did not start reading its request body")
	}

	rawDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config/raw", nil))
		rawDone <- recorder
	}()

	blocked := false
	select {
	case recorder := <-rawDone:
		if recorder.Code != http.StatusOK {
			t.Fatalf("raw config status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		blocked = true
	}

	close(body.release)
	select {
	case <-putDone:
	case <-time.After(time.Second):
		t.Fatal("slow PUT did not exit after releasing its body")
	}
	if blocked {
		select {
		case <-rawDone:
		case <-time.After(time.Second):
			t.Fatal("raw config remained blocked after slow PUT exited")
		}
		t.Fatal("slow PUT body held the configuration transaction lock")
	}
}

func TestSlowRawConfigWriterDoesNotBlockReload(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
	writer := newBlockingResponseWriter()
	rawDone := make(chan struct{})
	go func() {
		defer close(rawDone)
		srv.handle(writer, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config/raw", nil))
	}()

	select {
	case <-writer.started:
	case <-time.After(time.Second):
		close(writer.release)
		t.Fatal("raw config did not start writing its response")
	}

	reloadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.handle(recorder, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config/reload", nil))
		reloadDone <- recorder
	}()

	blocked := false
	select {
	case recorder := <-reloadDone:
		if recorder.Code != http.StatusOK {
			t.Fatalf("reload status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		blocked = true
	}

	close(writer.release)
	select {
	case <-rawDone:
	case <-time.After(time.Second):
		t.Fatal("raw config writer did not exit after release")
	}
	if blocked {
		select {
		case <-reloadDone:
		case <-time.After(time.Second):
			t.Fatal("reload remained blocked after raw response completed")
		}
		t.Fatal("slow raw response held the configuration transaction lock")
	}
}

func TestStaticAssetsAndSecurityHeaders(t *testing.T) {
	tests := []struct {
		path        string
		contentType string
	}{
		{path: "/", contentType: "text/html"},
		{path: "/vendor/alpine.min.js", contentType: "javascript"},
		{path: "/vendor/tailwind.css", contentType: "text/css"},
		// 字体必须发出 font/woff2：Go 内置 MIME 表没有 .woff2，distroless 也没有
		// /etc/mime.types 兜底，退化成 application/octet-stream 后叠加 nosniff 会被浏览器拒绝。
		{path: "/vendor/material-symbols-outlined.woff2", contentType: "font/woff2"},
		{path: "/vendor/inter-latin.woff2", contentType: "font/woff2"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			srv := newBoundaryTestServer()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789"+tt.path, nil)
			srv.handle(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Fatalf("Content-Type = %q, want containing %q", got, tt.contentType)
			}
			for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
				if recorder.Header().Get(header) == "" {
					t.Errorf("missing security header %s", header)
				}
			}
			// 字体本地化后 CSP 不该再为 Google 域名开口子；留着等于本地化没做干净。
			csp := recorder.Header().Get("Content-Security-Policy")
			for _, forbidden := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
				if strings.Contains(csp, forbidden) {
					t.Errorf("CSP still allows %s: %s", forbidden, csp)
				}
			}
		})
	}
}

func TestEmbeddedAdminPageUsesLocalAssetsAndSafeConfigState(t *testing.T) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	for _, required := range []string{
		// 样式改成构建期产物后，页面只留一条 <link>；Play CDN 那个 407KB 运行时编译器已删除。
		`href="/vendor/tailwind.css"`,
		`src="/vendor/alpine.min.js"`,
		`rel="icon" href="data:,"`,
		`x-show="isConfigTab()"`,
		// 保存按钮按脏状态禁用。canSave() 是 isConfigTab() && isDirty() 的封装，
		// 保存按钮、未保存浮层与 beforeunload 三处共用它，避免各写一份判据后互相矛盾。
		// 两个都断言：只查 canSave() 的话，脏比对被整个删掉也能过测试。
		`!canSave()`,
		`isDirty()`,
		`apiKeyConfigured`,
		`If-Match`,
		`application/yaml`,
		`configPayload()`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("admin page missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"https://cdn.tailwindcss.com",
		"cdn.jsdelivr.net/npm/alpinejs",
		`x-text="provider.apiKey || '(未配置)'"`,
		// 字体外部依赖已去掉，CSP 也随之收紧，不能再漏回来。
		"fonts.googleapis.com",
		"fonts.gstatic.com",
		// Play CDN 的运行时编译器与喂给它的内联 tailwind.config 都不该再回来。
		// 只禁赋值形式：注释里提到源文件名 web/tailwind.config.js 是正常的。
		"/vendor/tailwindcss.js",
		"tailwind.config =",
		"tailwind.config=",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("admin page still contains unsafe/stale pattern %q", forbidden)
		}
	}
}

// 页面拆成 src/app/*.js.part 之后，开发模式必须实时拼装源码而不是回读产物 index.html，
// 否则改片段要先手工跑一遍 make web-html 才能在浏览器里看到效果，热加载等于半废。
func TestDevModeAssemblesFromSourceFragments(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 产物故意写成与源码不同的内容：拿到哪一份就能判断走了哪条路径。
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>STALE ARTIFACT</html></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl := "<html><body>\n" + webbuild.Marker + "\n</body></html>"
	if err := os.WriteFile(filepath.Join(dir, "src", "index.template.html"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "app", "00-x.js.part"), []byte("FRESH_FROM_FRAGMENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newBoundaryTestServer()
	srv.webDevDir = dir
	recorder := httptest.NewRecorder()
	srv.handleIndex(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "FRESH_FROM_FRAGMENT") {
		t.Error("开发模式没有实时拼装片段")
	}
	if strings.Contains(body, "STALE ARTIFACT") {
		t.Error("开发模式回读了产物 index.html，改片段将不会生效")
	}
	// 热加载脚本仍需注入，否则浏览器不会自动刷新。
	if !strings.Contains(body, "/__dev/reload") {
		t.Error("拼装路径漏了注入热加载脚本")
	}

	// 只有产物、没有 src/ 的目录应退回直接读 index.html，保持拆分前行为。
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "index.html"), []byte("<html>ONLY ARTIFACT</html></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.webDevDir = bare
	recorder = httptest.NewRecorder()
	srv.handleIndex(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/", nil))
	if !strings.Contains(recorder.Body.String(), "ONLY ARTIFACT") {
		t.Error("无 src/ 时应退回读 index.html")
	}
}

// SSE 监听必须覆盖片段文件。只盯 index.html 的话，改片段时它根本不变，
// mtime 永不前进、浏览器永不刷新——热加载看着还在，实际已经废了。
func TestDevReloadWatchesFragmentModTime(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "index.template.html"), []byte(webbuild.Marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fragment := filepath.Join(dir, "src", "app", "00-x.js.part")
	if err := os.WriteFile(fragment, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 产物存在但故意设成很早的 mtime，确保「最新时间」只可能来自片段。
	artifact := filepath.Join(dir, "index.html")
	if err := os.WriteFile(artifact, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(artifact, old, old); err != nil {
		t.Fatal(err)
	}

	srv := newBoundaryTestServer()
	srv.webDevDir = dir

	before := srv.webSourcesModTime()
	if before.IsZero() {
		t.Fatal("webSourcesModTime 返回零值")
	}

	// 把片段 mtime 推到未来，模拟一次编辑。
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(fragment, future, future); err != nil {
		t.Fatal(err)
	}
	after := srv.webSourcesModTime()
	if !after.After(before) {
		t.Error("改片段后 webSourcesModTime 没有前进，SSE 不会触发刷新")
	}
}

// 样式从 index.html 的内联 <style> 搬到构建产物 vendor/tailwind.css 之后，
// 原先钉在页面上的字体与图标断言必须跟着搬过来，否则 S3-3 那批保障会随迁移一起消失。
// 产物是 minify 过的，字符串形态与源文件不同：url 与 font-family 的引号被去掉、
// 'liga' 被规范成 "liga"，所以这里按产物的真实形态断言。
func TestBuiltStylesheetKeepsLocalFontsAndComponentLayers(t *testing.T) {
	data, err := webFS.ReadFile("web/vendor/tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)

	for _, required := range []string{
		// 字体本地化：三个字体族都走 /vendor/ 下的 woff2。
		"url(/vendor/material-symbols-outlined.woff2)",
		"url(/vendor/inter-latin.woff2)",
		"url(/vendor/inter-latin-ext.woff2)",
		"url(/vendor/jetbrains-mono-latin.woff2)",
		"url(/vendor/jetbrains-mono-latin-ext.woff2)",
		// 图标类必须自带 font-family 与 liga：这两条原先由 Google 的 css2 样式表提供，
		// 少任何一条，页面上的图标都会退化成 monitoring、dns 这样的字面英文单词。
		"font-family:Material Symbols Outlined",
		`font-feature-settings:"liga"`,
		// 图标字体必须 block 而不是 swap：fallback 阶段会把 ligature 名字当普通文本画出来，
		// swap 会先闪一屏英文单词。
		"font-display:block",
		// 组件类只从 @layer components 来，被 Tailwind 的按需裁剪丢掉时页面会大面积失样式。
		".btn{",
		".btn-primary{",
		".btn-secondary{",
		".btn-danger{",
		".field{",
		".chip{",
		".glass{",
		".material-symbols-outlined{",
		".icon-fill{",
		// 这三个类只从 Alpine :class 表达式引用，写成层外普通 CSS 以免参与裁剪。
		".gw-spin{",
		".gw-grab{",
		".gw-grabbing{",
		"@keyframes gw-spin",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("built stylesheet missing %q", required)
		}
	}

	if strings.Contains(css, "fonts.gstatic.com") || strings.Contains(css, "fonts.googleapis.com") {
		t.Error("built stylesheet still references Google Fonts")
	}

	// 层叠顺序是这次迁移最容易静默坏掉的地方：Play CDN 把生成的 <style> append 到 head 末尾，
	// 于是工具类天然排在页面自定义 CSS 之后。页面大量依赖这个顺序——
	// .material-symbols-outlined 定死 font-size: 20px，图标按钮靠 text-[18px] 覆盖它；
	// .btn 定死 padding: .625rem .875rem，紧凑按钮靠 px-2 py-1.5 覆盖它。
	// 换成静态表后顺序由 @layer 保证，这里直接按字节偏移验证 components 排在 utilities 之前。
	for _, pair := range []struct{ component, utility string }{
		{".btn{", ".px-2{"},
		{".material-symbols-outlined{", `.text-\[18px\]{`},
		{".chip{", ".px-2{"},
		{".field{", ".w-full{"},
		// .icon-fill 与 .material-symbols-outlined 同为单类选择器，只能靠源码顺序决胜。
		{".material-symbols-outlined{", ".icon-fill{"},
	} {
		first := strings.Index(css, pair.component)
		second := strings.Index(css, pair.utility)
		if first < 0 {
			t.Errorf("built stylesheet missing component selector %q", pair.component)
			continue
		}
		if second < 0 {
			t.Errorf("built stylesheet missing utility selector %q", pair.utility)
			continue
		}
		if first > second {
			t.Errorf("cascade order broken: %q (at %d) must precede %q (at %d)", pair.component, first, pair.utility, second)
		}
	}
}

func TestEmbeddedAdminPageUsesActualListenerAndKeepsRestartWarnings(t *testing.T) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	for _, required := range []string{
		`restartRequired: []`,
		`this.restartRequired = data.restartRequired || []`,
		`health?.listenAddress`,
		`restartSuffix(result)`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("admin page missing runtime-state contract %q", required)
		}
	}
	if strings.Count(page, `restartSuffix(result)`) < 2 {
		t.Error("save and reload must both preserve restartRequired warnings")
	}
	for _, forbidden := range []string{
		`x-text="config.port || '--'"`,
		`(config.host || '127.0.0.1') + ':' + (config.port || 7789)`,
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("admin page still reports configured listener as live state: %q", forbidden)
		}
	}
}

func TestGetConfigUsesExactSecretSentinelAndRevision(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "super-secret") {
		t.Fatal("GET /api/config leaked the configured API key")
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	providers := body["providers"].(map[string]any)
	primary := providers["primary"].(map[string]any)
	if primary["apiKey"] != config.APIKeyKeepSentinel || primary["apiKeyConfigured"] != true {
		t.Fatalf("provider secret view = %#v", primary)
	}
	revision, _ := body["revision"].(string)
	if revision == "" || recorder.Header().Get("ETag") == "" {
		t.Fatalf("revision/ETag missing: body=%#v headers=%#v", body, recorder.Header())
	}
}

func TestConfigRevisionIsOpaqueAndRotatesOnSecretChange(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "guessable-one", 5))
	first := httptest.NewRecorder()
	srv.handle(first, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	var firstBody map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	firstRevision, _ := firstBody["revision"].(string)
	if firstRevision == "" {
		t.Fatal("initial revision is empty")
	}
	if firstRevision == bareConfigDigest(srv) {
		t.Fatal("revision exposes the bare SHA-256 of the secret-bearing config")
	}

	updated := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "guessable-two", 5)
	put := putConfig(t, srv, updated, first.Header().Get("ETag"))
	if put.Code != http.StatusOK {
		t.Fatalf("secret-only update status = %d; body=%s", put.Code, put.Body.String())
	}
	var putBody map[string]any
	if err := json.Unmarshal(put.Body.Bytes(), &putBody); err != nil {
		t.Fatal(err)
	}
	secondRevision, _ := putBody["revision"].(string)
	if secondRevision == "" || secondRevision == firstRevision {
		t.Fatalf("secret-only update did not rotate revision: first=%q second=%q", firstRevision, secondRevision)
	}
	if secondRevision == bareConfigDigest(srv) {
		t.Fatal("rotated revision still exposes the bare SHA-256 of the secret-bearing config")
	}
}

func TestSameConfigGetsDifferentRevisionAcrossServers(t *testing.T) {
	raw := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "same-secret", 5)
	revisions := make([]string, 2)
	for i := range revisions {
		srv := newConfigTestServer(t, raw)
		recorder := httptest.NewRecorder()
		srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		revisions[i], _ = body["revision"].(string)
	}
	if revisions[0] == "" || revisions[1] == "" || revisions[0] == revisions[1] {
		t.Fatalf("same config revisions must be independent opaque values: %#v", revisions)
	}
}

func TestGetConfigKeepsEmptyAPIKeySemantics(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "", 5))
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	primary := body["providers"].(map[string]any)["primary"].(map[string]any)
	if primary["apiKey"] != "" || primary["apiKeyConfigured"] != false {
		t.Fatalf("empty API key view = %#v", primary)
	}
}

func TestConfigPutMediaTypes(t *testing.T) {
	raw := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5)
	jsonBody := `{"host":"127.0.0.1","port":7789,"providers":{"primary":{"baseUrl":"https://api.example.com","apiKey":"secret","format":"openai","maxConcurrent":5,"maxPerSecond":0,"maxQueueWait":30000}},"routes":[{"match":"*","provider":"primary","model":"upstream"}]}`
	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{name: "json", contentType: "application/json; charset=utf-8", body: jsonBody, want: http.StatusOK},
		{name: "yaml", contentType: "application/yaml", body: raw, want: http.StatusOK},
		{name: "x-yaml", contentType: "application/x-yaml", body: raw, want: http.StatusOK},
		{name: "text yaml", contentType: "text/yaml", body: raw, want: http.StatusOK},
		{name: "missing", body: raw, want: http.StatusUnsupportedMediaType},
		{name: "plain text", contentType: "text/plain", body: raw, want: http.StatusUnsupportedMediaType},
		{name: "malformed", contentType: `application/json; charset="`, body: raw, want: http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newConfigTestServer(t, raw)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7789/api/config", strings.NewReader(tt.body))
			if tt.contentType != "" {
				request.Header.Set("Content-Type", tt.contentType)
			}
			srv.handle(recorder, request)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.want, recorder.Body.String())
			}
		})
	}
}

func TestGetRawConfigRedactsStructuredYAML(t *testing.T) {
	raw := `providers:
  primary: {baseUrl: https://api.example.com, apiKey: "flow-secret", format: openai, maxConcurrent: 5, maxPerSecond: 0, maxQueueWait: 30000}
routes:
  - match: "*"
    provider: primary
    model: upstream
`
	srv := newConfigTestServer(t, raw)
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config/raw", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "flow-secret") || !strings.Contains(recorder.Body.String(), config.APIKeyKeepSentinel) {
		t.Fatalf("raw config was not safely redacted:\n%s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "yaml") {
		t.Fatalf("Content-Type = %q, want YAML", got)
	}
	if _, err := config.DecodeAndValidate(recorder.Body.Bytes()); err != nil {
		t.Fatalf("redacted YAML is invalid: %v", err)
	}
}

func TestPutConfigSecretRoundTripAndIdentityGuard(t *testing.T) {
	t.Run("same identity preserves secret", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
		body := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 7)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
		}
		srv.cfgMu.RLock()
		got := srv.cfg.Providers["primary"]
		srv.cfgMu.RUnlock()
		if got.APIKey != "super-secret" || got.MaxConcurrent != 7 {
			t.Fatalf("saved provider = %#v", got)
		}
		disk, err := config.DecodeAndValidate(mustReadFile(t, gotConfigPath(srv)))
		if err != nil {
			t.Fatal(err)
		}
		if disk.Providers["primary"].APIKey != "super-secret" {
			t.Fatalf("disk API key = %q, want preserved secret", disk.Providers["primary"].APIKey)
		}
	})

	t.Run("url change preserves secret", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
		body := testConfigYAML("127.0.0.1", 7789, "https://redirect.example.com", config.APIKeyKeepSentinel, 5)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
		}
		srv.cfgMu.RLock()
		got := srv.cfg.Providers["primary"]
		srv.cfgMu.RUnlock()
		// url 变了但 apiKey 留空（sentinel），旧密钥应仍被保留：
		// 不再强制用户在改 url 时重新填写 apiKey。
		if got.BaseURL != "https://redirect.example.com" || got.APIKey != "super-secret" {
			t.Fatalf("saved provider = %#v, want new url with preserved secret", got)
		}
	})

	t.Run("format change preserves secret", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
		body := strings.Replace(testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 5), "format: openai", "format: anthropic", 1)
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
		}
		srv.cfgMu.RLock()
		got := srv.cfg.Providers["primary"]
		srv.cfgMu.RUnlock()
		if got.Format != "anthropic" || got.APIKey != "super-secret" {
			t.Fatalf("saved provider = %#v, want new format with preserved secret", got)
		}
	})

	t.Run("sentinel without existing secret rejected", func(t *testing.T) {
		// 全新 provider 名带上 sentinel：磁盘上没有已存密钥可保留，必须拒绝，
		// 否则会被静默吞成空密钥。把 provider 名连同路由引用一并改成 brand-new，
		// 让校验能走到 sentinel 保留环节。
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "super-secret", 5))
		body := strings.ReplaceAll(testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 5), "primary", "brand-new")
		recorder := putConfig(t, srv, body, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestPutConfigRevisionConflict(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
	body := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 6)
	recorder := putConfig(t, srv, body, `"stale-revision"`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	srv.cfgMu.RLock()
	maxConcurrent := srv.cfg.Providers["primary"].MaxConcurrent
	srv.cfgMu.RUnlock()
	if maxConcurrent != 5 {
		t.Fatalf("revision conflict changed config: maxConcurrent=%d", maxConcurrent)
	}
}

func TestConcurrentPutWithSameRevisionOnlyOneWins(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
	getRecorder := httptest.NewRecorder()
	srv.handle(getRecorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	etag := getRecorder.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET config did not return ETag")
	}

	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, maxConcurrent := range []int{6, 7} {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			<-start
			body := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, value)
			statuses <- putConfig(t, srv, body, etag).Code
		}(maxConcurrent)
	}
	close(start)
	wg.Wait()
	close(statuses)

	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent PUT statuses = %#v, want one 200 and one 409", counts)
	}
	srv.cfgMu.RLock()
	value := srv.cfg.Providers["primary"].MaxConcurrent
	srv.cfgMu.RUnlock()
	if value != 6 && value != 7 {
		t.Fatalf("runtime config is not one complete winner: %d", value)
	}
}

func TestPutConfigReportsRestartRequired(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
	body := testConfigYAML("0.0.0.0", 7790, "https://api.example.com", config.APIKeyKeepSentinel, 5)
	recorder := putConfig(t, srv, body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		RestartRequired []string `json:"restartRequired"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(response.RestartRequired, ",")
	if !strings.Contains(joined, "host") || !strings.Contains(joined, "port") {
		t.Fatalf("restartRequired = %#v, want host and port", response.RestartRequired)
	}
	healthRecorder := httptest.NewRecorder()
	srv.handle(healthRecorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/health", nil))
	var health map[string]any
	if err := json.Unmarshal(healthRecorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["listenAddress"] != "127.0.0.1:7789" {
		t.Fatalf("actual listen address = %#v, want old listener", health["listenAddress"])
	}
}

func TestReloadUsesSamePathAndRollsBackInvalidConfig(t *testing.T) {
	initial := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5)
	srv := newConfigTestServer(t, initial)
	path := gotConfigPath(srv)
	updated := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 7)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewRecorder()
	srv.handle(reload, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config/reload", nil))
	if reload.Code != http.StatusOK {
		t.Fatalf("valid reload status = %d; body=%s", reload.Code, reload.Body.String())
	}
	srv.cfgMu.RLock()
	maxConcurrent := srv.cfg.Providers["primary"].MaxConcurrent
	loadedPath := srv.cfg.Path
	stableRevision := srv.revision
	srv.cfgMu.RUnlock()
	if maxConcurrent != 7 || loadedPath != path {
		t.Fatalf("reloaded config = maxConcurrent %d path %q, want 7 and %q", maxConcurrent, loadedPath, path)
	}

	if err := os.WriteFile(path, []byte("providers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := httptest.NewRecorder()
	srv.handle(invalid, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/api/config/reload", nil))
	if invalid.Code != http.StatusInternalServerError {
		t.Fatalf("invalid reload status = %d, want 500; body=%s", invalid.Code, invalid.Body.String())
	}
	srv.cfgMu.RLock()
	maxConcurrent = srv.cfg.Providers["primary"].MaxConcurrent
	gotRevision := srv.revision
	srv.cfgMu.RUnlock()
	if maxConcurrent != 7 || gotRevision != stableRevision {
		t.Fatalf("invalid reload mutated runtime config/revision: maxConcurrent=%d revision=%q want=%q", maxConcurrent, gotRevision, stableRevision)
	}
}

func TestPutConfigReconcilesDeletedProviderQueue(t *testing.T) {
	raw := strings.Replace(testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5), "providers:\n", `providers:
  removed:
    baseUrl: https://removed.example.com
    apiKey: old
    format: openai
    maxConcurrent: 9
    maxPerSecond: 4
    maxQueueWait: 30000
`, 1)
	srv := newConfigTestServer(t, raw)
	release, _, err := srv.qm.Acquire(context.Background(), "removed", 9, 4, 1000)
	if err != nil {
		t.Fatal(err)
	}
	release()

	body := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 5)
	recorder := putConfig(t, srv, body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if status := srv.qm.StatusOf("removed", 0, 0); status.MaxConcurrent != 0 || status.MaxPerSecond != 0 {
		t.Fatalf("deleted provider queue still exists: %#v", status)
	}
}

func TestApplyRuntimeConfigPropagatesEveryDynamicComponent(t *testing.T) {
	oldCfg, err := config.DecodeAndValidate([]byte(testConfigYAML("127.0.0.1", 7789, "https://old.example.com", "old", 5)))
	if err != nil {
		t.Fatal(err)
	}
	newCfg, err := config.DecodeAndValidate([]byte(testConfigYAML("127.0.0.1", 7789, "https://new.example.com", "new", 7)))
	if err != nil {
		t.Fatal(err)
	}
	newCfg.DirectMode = true
	newCfg.Cache.MaxAgeDays = 3
	newCfg.Cache.MaxRecords = 44
	qm := queue.NewManager()
	qm.Reconcile(map[string]queue.Limits{"removed": {MaxConcurrent: 9, MaxPerSecond: 4}})
	cacheSpy := &runtimeCacheSpy{}
	visionSpy := &runtimeVisionSpy{}
	healthSpy := &runtimeHealthSpy{}
	srv := &server{
		cfg:            oldCfg,
		revision:       "old-revision",
		listenHost:     oldCfg.Host,
		listenPort:     oldCfg.Port,
		qm:             qm,
		cache:          cacheSpy,
		translator:     visionSpy,
		providerHealth: healthSpy,
	}

	restartRequired := srv.applyRuntimeConfig(newCfg, "next-revision")
	if len(restartRequired) != 0 {
		t.Fatalf("unexpected restartRequired: %#v", restartRequired)
	}
	if len(visionSpy.modes) != 1 || !visionSpy.modes[0] {
		t.Fatalf("vision direct-mode updates = %#v", visionSpy.modes)
	}
	if cacheSpy.calls != 1 || cacheSpy.maxAgeDays != 3 || cacheSpy.maxRecords != 44 {
		t.Fatalf("cache cleanup propagation = %#v", cacheSpy)
	}
	if healthSpy.calls != 1 || healthSpy.oldCfg != oldCfg || healthSpy.newCfg != newCfg {
		t.Fatalf("provider health invalidation = %#v", healthSpy)
	}
	release, _, err := qm.Acquire(context.Background(), "primary", 99, 99, 1000)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if status := qm.StatusOf("primary", 0, 0); status.MaxConcurrent != 7 || status.MaxPerSecond != 0 {
		t.Fatalf("new provider authoritative queue limits = %#v", status)
	}
	if status := qm.StatusOf("removed", 0, 0); status.MaxConcurrent != 0 || status.MaxPerSecond != 0 {
		t.Fatalf("removed provider queue still active = %#v", status)
	}
	srv.cfgMu.RLock()
	gotCfg, gotRevision := srv.cfg, srv.revision
	srv.cfgMu.RUnlock()
	if gotCfg != newCfg || gotRevision != "next-revision" {
		t.Fatalf("runtime config/revision = %p/%q, want %p/next-revision", gotCfg, gotRevision, newCfg)
	}
}

func TestInferenceRejectsRequestConversionError(t *testing.T) {
	srv := newBoundaryTestServer()
	srv.cfg.Providers["primary"] = &config.Provider{
		Name: "primary", BaseURL: "http://127.0.0.1:1", Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000,
	}
	srv.cfg.Routes = []config.Route{{Match: "*", Provider: "primary", Model: "upstream"}}
	recorder := httptest.NewRecorder()
	body := `{"model":"client-model","messages":[],"tools":[{"type":"computer_20241022","name":"computer"}]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	srv.handle(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "conversion_error") {
		t.Fatalf("status/body = %d/%s, want explicit request conversion error", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceRejectsTargetSpecificToolOutput(t *testing.T) {
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "http://127.0.0.1:1", "secret", 5))
	recorder := httptest.NewRecorder()
	body := `{"model":"client-model","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"https://images.example/test.png"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	srv.handle(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "conversion_error") {
		t.Fatalf("status/body = %d/%s, want target-aware conversion error", recorder.Code, recorder.Body.String())
	}
}

func TestSameFormatRequestPreservesNativeExtensions(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		providerFormat string
		requestBody    string
		responseBody   string
		withVision     bool
		assert         func(*testing.T, map[string]any)
	}{
		{
			name: "anthropic cache control", path: "/v1/messages", providerFormat: "anthropic",
			requestBody:  `{"model":"client-model","max_tokens":32,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}],"system":[{"type":"text","text":"cached system","cache_control":{"type":"ephemeral"}}],"tools":[{"type":"computer_20241022","name":"computer","display_width_px":1024,"display_height_px":768,"cache_control":{"type":"ephemeral"}}]}`,
			responseBody: `{"id":"msg_test","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			withVision:   true,
			assert: func(t *testing.T, body map[string]any) {
				system, _ := body["system"].([]any)
				if len(system) != 1 {
					t.Fatalf("native Anthropic system blocks were dropped: %#v", body)
				}
				block, _ := system[0].(map[string]any)
				tools, _ := body["tools"].([]any)
				if len(tools) != 1 {
					t.Fatalf("native Anthropic tools were dropped: %#v", body)
				}
				tool, _ := tools[0].(map[string]any)
				if block["cache_control"] == nil || tool["cache_control"] == nil || tool["type"] != "computer_20241022" {
					t.Fatalf("native Anthropic extensions were dropped: %#v", body)
				}
			},
		},
		{
			name: "openai strict function", path: "/v1/chat/completions", providerFormat: "openai",
			requestBody:  `{"model":"client-model","messages":[{"role":"system","content":"keep me"},{"role":"user","name":"alice","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}],"tools":[{"type":"function","function":{"name":"lookup","description":"test","strict":true,"parameters":{"type":"object"}}}]}`,
			responseBody: `{"id":"chatcmpl-test","model":"upstream","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			withVision:   true,
			assert: func(t *testing.T, body map[string]any) {
				tools, _ := body["tools"].([]any)
				if len(tools) != 1 {
					t.Fatalf("native OpenAI tools were dropped: %#v", body)
				}
				tool, _ := tools[0].(map[string]any)
				function, _ := tool["function"].(map[string]any)
				if function["strict"] != true {
					t.Fatalf("native OpenAI strict flag was dropped: %#v", body)
				}
				messages, _ := body["messages"].([]any)
				if len(messages) != 2 {
					t.Fatalf("native OpenAI messages were rebuilt unexpectedly: %#v", body)
				}
				user, _ := messages[1].(map[string]any)
				if user["name"] != "alice" {
					t.Fatalf("native OpenAI message fields were dropped: %#v", body)
				}
			},
		},
		{
			name: "openai responses native fields", path: "/v1/responses", providerFormat: "openai-responses",
			requestBody:  `{"model":"client-model","input":[{"type":"message","role":"user","name":"alice","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://images.example/test.png"}]}],"store":true,"tools":[{"type":"function","name":"lookup","strict":true,"parameters":{"type":"object"}}]}`,
			responseBody: `{"id":"resp-test","object":"response","status":"completed","model":"upstream","output":[{"type":"message","id":"msg-test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			withVision:   true,
			assert: func(t *testing.T, body map[string]any) {
				if body["store"] != true {
					t.Fatalf("native Responses store was dropped: %#v", body)
				}
				tools, _ := body["tools"].([]any)
				tool, _ := tools[0].(map[string]any)
				if tool["strict"] != true || tool["name"] != "lookup" {
					t.Fatalf("native Responses tool was dropped: %#v", body)
				}
				input, _ := body["input"].([]any)
				message, _ := input[0].(map[string]any)
				if message["name"] != "alice" {
					t.Fatalf("native Responses message fields were dropped: %#v", body)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.responseBody)
			}))
			defer upstream.Close()

			provider := &config.Provider{Name: "primary", BaseURL: upstream.URL, Format: tt.providerFormat, MaxConcurrent: 1, MaxQueueWait: 1000}
			providers := map[string]*config.Provider{"primary": provider}
			route := config.Route{Match: "*", Provider: "primary", Model: "upstream"}
			if tt.withVision {
				providers["vision"] = &config.Provider{Name: "vision", BaseURL: upstream.URL, Format: "openai", MaxConcurrent: 1, MaxQueueWait: 1000}
				route.Vision = &config.Vision{Provider: "vision", Model: "vision-model"}
			}
			srv := &server{
				cfg: &config.Config{
					Host: "127.0.0.1", Port: 7789, Timeout: 500, StreamActivityTimeout: 500,
					DirectMode: true, DirectTimeoutNoStream: 500, DirectTimeoutStreamHeader: 500, DirectTimeoutStreamActive: 500,
					Providers: providers,
					Routes:    []config.Route{route},
				},
				qm: queue.NewManager(), httpClient: upstream.Client(), metrics: metrics.NewCollector(10),
				providerHealth: providerhealth.NewChecker(), translator: &runtimeVisionSpy{},
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7789"+tt.path, strings.NewReader(tt.requestBody))
			request.Header.Set("Content-Type", "application/json")
			srv.handle(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
			}
			upstreamBody := <-captured
			if upstreamBody["model"] != "upstream" {
				t.Fatalf("upstream model = %#v", upstreamBody["model"])
			}
			tt.assert(t, upstreamBody)
		})
	}
}

func TestNewGatewayHTTPServerTimeouts(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	httpServer := newGatewayHTTPServer("127.0.0.1:7789", handler)
	if httpServer.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s", httpServer.ReadHeaderTimeout)
	}
	if httpServer.ReadTimeout != 60*time.Second {
		t.Fatalf("ReadTimeout = %s", httpServer.ReadTimeout)
	}
	if httpServer.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout = %s", httpServer.IdleTimeout)
	}
	if httpServer.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes = %d", httpServer.MaxHeaderBytes)
	}
	if httpServer.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, long SSE must remain unlimited", httpServer.WriteTimeout)
	}
}

func TestStatusRecorderUnwrapsResponseController(t *testing.T) {
	underlying := &deadlineWriterSpy{ResponseWriter: httptest.NewRecorder()}
	recorder := &statusRecorder{ResponseWriter: underlying}
	deadline := time.Now().Add(time.Second)
	if err := http.NewResponseController(recorder).SetWriteDeadline(deadline); err != nil {
		t.Fatalf("SetWriteDeadline() error = %v", err)
	}
	if !underlying.deadline.Equal(deadline) {
		t.Fatalf("underlying deadline = %v, want %v", underlying.deadline, deadline)
	}
}

func TestShutdownThenCloseOrdersResources(t *testing.T) {
	events := make([]string, 0, 2)
	shutdown := shutdownFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || time.Until(deadline) <= 0 {
			t.Fatalf("shutdown context deadline = %v, want within one second", deadline)
		}
		events = append(events, "shutdown")
		return nil
	})
	closeResource := func() error {
		events = append(events, "close")
		return nil
	}
	if err := shutdownThenClose(shutdown, time.Second, closeResource); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "shutdown,close" {
		t.Fatalf("shutdown order = %s", got)
	}
}

type deadlineWriterSpy struct {
	http.ResponseWriter
	deadline time.Time
}

type countingEOFReader struct {
	reads int
}

func (r *countingEOFReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func (w *deadlineWriterSpy) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func newConfigTestServer(t *testing.T, raw string) *server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	cfg.Path = path
	return &server{
		cfg:            cfg,
		qm:             queue.NewManager(),
		httpClient:     http.DefaultClient,
		metrics:        metrics.NewCollector(10),
		providerHealth: providerhealth.NewChecker(),
	}
}

func testConfigYAML(host string, port int, baseURL, apiKey string, maxConcurrent int) string {
	return fmt.Sprintf(`host: %q
port: %d
providers:
  primary:
    baseUrl: %q
    apiKey: %q
    format: openai
    maxConcurrent: %d
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "*"
    provider: primary
    model: upstream
`, host, port, baseURL, apiKey, maxConcurrent)
}

// TestModelsListDescribesMultiTargetRoutes 锁定 /v1/models 不再直接读 route.Provider。
//
// 多候选写法下校验强制 route.Provider / route.Model 为空，直接读会让每条 targets 路由
// 输出 owned_by: ""、target_model: ""。
func TestModelsListDescribesMultiTargetRoutes(t *testing.T) {
	yaml := `host: "127.0.0.1"
port: 7789
providers:
  primary:
    baseUrl: "https://a.example.com"
    apiKey: "k1"
    format: openai
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
  backup:
    baseUrl: "https://b.example.com"
    apiKey: "k2"
    format: anthropic
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: "multi-*"
    targets:
      - provider: primary
        model: model-a
      - provider: backup
        model: model-b
    strategy: round-robin
  - match: "single-*"
    provider: primary
    model: model-c
`
	srv := newConfigTestServer(t, yaml)
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			OwnedBy     string `json:"owned_by"`
			TargetModel string `json:"target_model"`
			Strategy    string `json:"strategy"`
			Targets     []struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"targets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, entry := range body.Data {
		byID[entry.ID] = i
	}
	multi, ok := byID["multi-*"]
	if !ok {
		t.Fatalf("缺少 multi-* 条目: body=%s", recorder.Body.String())
	}
	m := body.Data[multi]
	if m.OwnedBy != "primary" || m.TargetModel != "model-a" {
		t.Errorf("multi-* owned_by/target_model = %q/%q, 期望 primary/model-a", m.OwnedBy, m.TargetModel)
	}
	if len(m.Targets) != 2 || m.Targets[1].Provider != "backup" || m.Targets[1].Model != "model-b" {
		t.Errorf("multi-* targets = %#v, 期望 2 个候选且第二个是 backup/model-b", m.Targets)
	}
	if m.Strategy != "round-robin" {
		t.Errorf("multi-* strategy = %q, 期望 round-robin", m.Strategy)
	}

	single, ok := byID["single-*"]
	if !ok {
		t.Fatalf("缺少 single-* 条目: body=%s", recorder.Body.String())
	}
	s := body.Data[single]
	if s.OwnedBy != "primary" || s.TargetModel != "model-c" {
		t.Errorf("single-* owned_by/target_model = %q/%q, 期望 primary/model-c", s.OwnedBy, s.TargetModel)
	}
	if len(s.Targets) != 0 || s.Strategy != "" {
		t.Errorf("单候选不应输出 targets/strategy，实际 targets=%#v strategy=%q", s.Targets, s.Strategy)
	}
}

// TestGetConfigExposesFailoverAndBreaker 锁定「GET 必须吐出 failover / breaker」这半边契约。
//
// PUT /api/config 是全量替换、不与旧配置合并，所以前端只能靠 GET 读到这两块再原样回传。
// 一旦 GET 不再返回它们，任何一次表单保存都会把用户手写的配置抹成零值
// （applyDefaults 填出 enabled: false），且界面仍提示保存成功。
func TestGetConfigExposesFailoverAndBreaker(t *testing.T) {
	yaml := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5) + `failover:
  enabled: true
  maxAttempts: 3
breaker:
  enabled: true
  consecutiveFailures: 4
`
	srv := newConfigTestServer(t, yaml)
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/config", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	failover, ok := body["failover"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 failover 块: body=%s", recorder.Body.String())
	}
	if failover["enabled"] != true {
		t.Errorf("failover.enabled = %#v, 期望 true", failover["enabled"])
	}
	if failover["maxAttempts"] != float64(3) {
		t.Errorf("failover.maxAttempts = %#v, 期望 3", failover["maxAttempts"])
	}
	brk, ok := body["breaker"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 breaker 块: body=%s", recorder.Body.String())
	}
	if brk["enabled"] != true {
		t.Errorf("breaker.enabled = %#v, 期望 true", brk["enabled"])
	}
	if brk["consecutiveFailures"] != float64(4) {
		t.Errorf("breaker.consecutiveFailures = %#v, 期望 4", brk["consecutiveFailures"])
	}
}

// TestPutConfigWithoutFailoverBlockDisablesIt 把「全量替换」的后果固化成测试。
//
// 这不是缺陷而是 PUT 的既定语义（严格解码 + 全量替换，便于用 API 关掉功能）：
// 省略 failover 就等于关掉它。前端因此必须原样回传，见 configPayload()。
func TestPutConfigWithoutFailoverBlockDisablesIt(t *testing.T) {
	yaml := testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5) + `failover:
  enabled: true
  maxAttempts: 3
`
	srv := newConfigTestServer(t, yaml)
	srv.cfgMu.RLock()
	before := srv.cfg.Failover.Enabled
	srv.cfgMu.RUnlock()
	if !before {
		t.Fatal("前置条件不成立：failover 应处于启用状态")
	}

	// 不带 failover 块的完整配置
	recorder := putConfig(t, srv, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", config.APIKeyKeepSentinel, 5), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	srv.cfgMu.RLock()
	after := srv.cfg.Failover.Enabled
	srv.cfgMu.RUnlock()
	if after {
		t.Error("failover.enabled 仍为 true；PUT 应是全量替换")
	}
}

func putConfig(t *testing.T, srv *server, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7789/api/config", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/yaml")
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	srv.handle(recorder, request)
	return recorder
}

func gotConfigPath(srv *server) string {
	srv.cfgMu.RLock()
	defer srv.cfgMu.RUnlock()
	return srv.cfg.Path
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type blockingBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingBody() *blockingBody {
	return &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingBody) Read(_ []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}

func (b *blockingBody) Close() error { return nil }

type blockingResponseWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type runtimeCacheSpy struct {
	calls      int
	maxAgeDays int
	maxRecords int
}

func (s *runtimeCacheSpy) GetStats() cache.Stats { return cache.Stats{} }
func (s *runtimeCacheSpy) Cleanup(maxAgeDays, maxRecords int) (cache.CleanupResult, error) {
	s.calls++
	s.maxAgeDays = maxAgeDays
	s.maxRecords = maxRecords
	return cache.CleanupResult{}, nil
}

type healthStatsCacheSpy struct {
	mu    sync.Mutex
	calls int
	stats cache.Stats
}

func (s *healthStatsCacheSpy) GetStats() cache.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.stats
}

func (s *healthStatsCacheSpy) StatsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *healthStatsCacheSpy) Cleanup(int, int) (cache.CleanupResult, error) {
	return cache.CleanupResult{}, nil
}

type runtimeVisionSpy struct {
	modes []bool
}

func (s *runtimeVisionSpy) Translate(_ context.Context, messages []any, _ *config.Provider, _ string, _ vision.LogFunc) []any {
	return messages
}
func (s *runtimeVisionSpy) SetDirectMode(enabled bool) { s.modes = append(s.modes, enabled) }

type runtimeHealthSpy struct {
	calls  int
	oldCfg *config.Config
	newCfg *config.Config
}

func (s *runtimeHealthSpy) Snapshot(*config.Config) map[string]providerhealth.Status {
	return map[string]providerhealth.Status{}
}
func (s *runtimeHealthSpy) CheckAll(context.Context, *config.Config, *http.Client) map[string]providerhealth.Status {
	return map[string]providerhealth.Status{}
}
func (s *runtimeHealthSpy) CheckProvider(_ context.Context, _ *config.Config, _ *http.Client, name string) (providerhealth.Status, bool) {
	return providerhealth.Status{Name: name}, true
}
func (s *runtimeHealthSpy) InvalidateChanged(oldCfg, newCfg *config.Config) {
	s.calls++
	s.oldCfg = oldCfg
	s.newCfg = newCfg
}

func newBlockingResponseWriter() *blockingResponseWriter {
	return &blockingResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }
func (w *blockingResponseWriter) WriteHeader(_ int)   {}
func (w *blockingResponseWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func bareConfigDigest(srv *server) string {
	srv.cfgMu.RLock()
	copyCfg := *srv.cfg
	copyCfg.Path = ""
	copyCfg.Providers = make(map[string]*config.Provider, len(srv.cfg.Providers))
	for name, provider := range srv.cfg.Providers {
		providerCopy := *provider
		providerCopy.Name = ""
		copyCfg.Providers[name] = &providerCopy
	}
	srv.cfgMu.RUnlock()
	data, _ := json.Marshal(copyCfg)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

type shutdownFunc func(context.Context) error

func (fn shutdownFunc) Shutdown(ctx context.Context) error { return fn(ctx) }

// TestHandleProviderModels 覆盖前端「查询远程模型列表」依赖的后端端点：
// 成功解析上游 /v1/models 的 data[].id；上游不支持该端点时返回 502 与可读消息；
// 按provider 格式带上正确的鉴权头（openai 走 Bearer，anthropic 走 x-api-key）。
func TestHandleProviderModels(t *testing.T) {
	t.Run("openai models returned", func(t *testing.T) {
		var gotAuth string
		var gotPath string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("authorization")
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
		}))
		defer up.Close()

		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, up.URL, "test-secret", 5))
		rec := callProviderModels(t, srv, "primary")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if gotPath != "/v1/models" {
			t.Fatalf("upstream path = %q, want /v1/models", gotPath)
		}
		if gotAuth != "Bearer test-secret" {
			t.Fatalf("auth header = %q, want Bearer test-secret", gotAuth)
		}
		var resp struct {
			Provider string   `json:"provider"`
			Models   []string `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Provider != "primary" || len(resp.Models) != 2 || resp.Models[0] != "gpt-4o" {
			t.Fatalf("unexpected response: %#v", resp)
		}
	})

	t.Run("anthropic uses x-api-key header", func(t *testing.T) {
		var gotAPIKey, gotVersion string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAPIKey = r.Header.Get("x-api-key")
			gotVersion = r.Header.Get("anthropic-version")
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-5-sonnet"}]}`))
		}))
		defer up.Close()

		raw := strings.Replace(testConfigYAML("127.0.0.1", 7789, up.URL, "anthropic-secret", 5), "format: openai", "format: anthropic", 1)
		srv := newConfigTestServer(t, raw)
		rec := callProviderModels(t, srv, "primary")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if gotAPIKey != "anthropic-secret" {
			t.Fatalf("x-api-key = %q, want anthropic-secret", gotAPIKey)
		}
		if gotVersion != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", gotVersion)
		}
	})

	t.Run("upstream unsupported endpoint surfaces 502", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer up.Close()

		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, up.URL, "secret", 5))
		rec := callProviderModels(t, srv, "primary")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "不支持") {
			t.Fatalf("body should mention unsupported endpoint: %s", rec.Body.String())
		}
	})

	t.Run("missing provider parameter rejected", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/models", nil)
		srv.handle(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", recorder.Code)
		}
	})

	t.Run("unknown provider rejected", func(t *testing.T) {
		srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, "https://api.example.com", "secret", 5))
		rec := callProviderModels(t, srv, "no-such-provider")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func callProviderModels(t *testing.T, srv *server, provider string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7789/api/providers/models?provider="+provider, nil)
	srv.handle(recorder, request)
	return recorder
}
