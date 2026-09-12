package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
)

func TestBuildUpstreamURLSelectsProtocolEndpoint(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{"anthropic", "/v1/messages"},
		{"openai", "/v1/chat/completions"},
		{"openai-responses", "/v1/responses"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			base, path := buildUpstreamURL(&config.Provider{BaseURL: "https://upstream.example/v1", Format: tt.format})
			if base != "https://upstream.example" || path != tt.want {
				t.Fatalf("base/path = %q/%q, want https://upstream.example/%q", base, path, tt.want)
			}
		})
	}
}

func TestResponsesProviderUsesBearerAuthentication(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{Format: "openai-responses", APIKey: "test-key"}, "")
	if got := req.Header.Get("authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("unexpected anthropic key header = %q", got)
	}
}

func TestSetUpstreamHeadersAppliesExtraHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{
		Format: "anthropic",
		APIKey: "real-key",
		ExtraHeaders: map[string]string{
			"anthropic-beta": "context-1m-2025-08-07",
			"x-trace":        "abc",
			// 覆盖协议头是允许的
			"anthropic-version": "2099-01-01",
		},
	}, "")

	if got := req.Header.Get("anthropic-beta"); got != "context-1m-2025-08-07" {
		t.Errorf("anthropic-beta = %q，期望自定义头被写入", got)
	}
	if got := req.Header.Get("x-trace"); got != "abc" {
		t.Errorf("x-trace = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2099-01-01" {
		t.Errorf("anthropic-version = %q，期望允许被 extraHeaders 覆盖", got)
	}
}

// TestSetUpstreamHeadersExtraHeadersCannotBreakAuth 锁住运行时兜底：
// 即使配置校验被绕过（热重载、未来新入口），鉴权与 UA 也绝不能被覆盖。
func TestSetUpstreamHeadersExtraHeadersCannotBreakAuth(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{
		Format:    "anthropic",
		APIKey:    "real-key",
		UserAgent: "provider-agent/1.0",
		ExtraHeaders: map[string]string{
			"x-api-key":     "hijacked",
			"Authorization": "Bearer hijacked",
			"content-type":  "text/plain",
			"User-Agent":    "hijacked-agent",
			"":              "empty-name",
			"   ":           "blank-name",
		},
	}, "")

	if got := req.Header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q，期望保持 real-key", got)
	}
	// 转发路径 anthropic 格式现在也发 Authorization，这条断言的分量因此更重：
	// 该头已成为部分中转唯一认的鉴权头，被 extraHeaders 劫持等于把请求打到别人的账上。
	if got := req.Header.Get("authorization"); got != "Bearer real-key" {
		t.Errorf("authorization = %q，期望保持 Bearer real-key（不可被 extraHeaders 劫持）", got)
	}
	if got := req.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q，期望保持 application/json", got)
	}
	if got := req.Header.Get("User-Agent"); got != "provider-agent/1.0" {
		t.Errorf("User-Agent = %q，期望保持 provider 配置值", got)
	}
}

func TestApplyExtraHeadersNilAndEmpty(t *testing.T) {
	// nil 与空 map 都必须是空操作：逐字节兼容未配置该字段的既有行为。
	for _, extra := range []map[string]string{nil, {}} {
		req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
		before := len(req.Header)
		ApplyExtraHeaders(req.Header, extra)
		if len(req.Header) != before {
			t.Fatalf("extra=%#v 改变了头数量：%d → %d", extra, before, len(req.Header))
		}
	}
}

// blankHeader 返回一个不带任何默认头的 http.Header，便于断言「某个头不存在」。
func blankHeader(t *testing.T) http.Header {
	t.Helper()
	header := httptest.NewRequest("GET", "http://gateway.invalid", nil).Header
	for k := range header {
		delete(header, k)
	}
	return header
}

// TestSetUpstreamAuthHeadersSendsBothForAnthropic 锁住核心契约：anthropic 格式下
// x-api-key 与 Authorization 两个头都要带，三条出网路径（转发、模型列表、健康检测）一致。
//
// 判据来自实测：new-api / one-api 系中转（anyrouter、agentrouter）主要按 Authorization
// 取令牌——Claude Code 对接它们的官方说明让用户设 ANTHROPIC_AUTH_TOKEN 而非
// ANTHROPIC_API_KEY，前者发的就是 Authorization: Bearer。只发 x-api-key 时
// anyrouter.top 的 /v1/models 返回 401、带 Authorization 返回 200。
func TestSetUpstreamAuthHeadersSendsBothForAnthropic(t *testing.T) {
	header := blankHeader(t)

	SetUpstreamAuthHeaders(header, "anthropic", "real-key")

	if got := header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q，期望 real-key（Anthropic 官方只读这个头）", got)
	}
	if got := header.Get("authorization"); got != "Bearer real-key" {
		t.Errorf("authorization = %q，期望 Bearer real-key（中转主要认这个头）", got)
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望 2023-06-01（官方缺了会 400）", got)
	}
}

// TestSetUpstreamAuthHeadersAnthropicEmptyKeyOmitsBearer 锁住空 key 的边界：
// 不补空 Authorization。空 key 是可达状态（provider.apiKey 留空且客户端未带鉴权头，
// ResolveAPIKeyWithSource 返回 ""/none，转发不阻断），典型是本地不校验鉴权的兼容服务；
// 给它发 "Bearer "（空令牌）可能被解析成非法 Authorization 而 400，比不发更糟。
func TestSetUpstreamAuthHeadersAnthropicEmptyKeyOmitsBearer(t *testing.T) {
	header := blankHeader(t)

	SetUpstreamAuthHeaders(header, "anthropic", "")

	if got := header.Get("authorization"); got != "" {
		t.Errorf("authorization = %q，空 key 时不应补该头", got)
	}
	if _, present := header["Authorization"]; present {
		t.Error("Authorization 键不应存在（设空字符串与不设是两种语义）")
	}
	// x-api-key 与 anthropic-version 仍按原行为写入，逐字节保持改造前语义。
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望仍写入", got)
	}
}

// TestSetUpstreamAuthHeadersOpenAIFormatsUnchanged 锁住非 anthropic 格式不受影响：
// 只发 Authorization，不得多出 x-api-key / anthropic-version；空 key 仍写空 Bearer，
// 与改造前逐字节一致（openai 系原本就无条件写这个头）。
func TestSetUpstreamAuthHeadersOpenAIFormatsUnchanged(t *testing.T) {
	for _, format := range []string{"openai", "openai-responses"} {
		t.Run(format, func(t *testing.T) {
			header := blankHeader(t)

			SetUpstreamAuthHeaders(header, format, "test-key")

			if got := header.Get("authorization"); got != "Bearer test-key" {
				t.Errorf("authorization = %q", got)
			}
			if got := header.Get("x-api-key"); got != "" {
				t.Errorf("x-api-key = %q，非 anthropic 格式不应出现该头", got)
			}
			if got := header.Get("anthropic-version"); got != "" {
				t.Errorf("anthropic-version = %q，非 anthropic 格式不应出现该头", got)
			}

			empty := blankHeader(t)
			SetUpstreamAuthHeaders(empty, format, "")
			if got := empty.Get("authorization"); got != "Bearer " {
				t.Errorf("空 key 时 authorization = %q，期望 %q（保持改造前行为）", got, "Bearer ")
			}
		})
	}
}

// TestForwardPathSendsBothAuthHeadersForAnthropic 把转发路径 /v1/messages 的新契约写死：
// 与探测路径共用 SetUpstreamAuthHeaders，anthropic 格式下两个鉴权头都带。
//
// 此前这里断言的是「转发路径不应带 authorization」，理由是「两个头都带可能被部分上游
// 判为冲突」。那是保守推断，已被三个真实端点实测证伪（均用假 key，只看头组合的反应）：
//
//	api.anthropic.com  只带 x-api-key → 401 authentication_error；两个都带 → 同一错误
//	anyrouter.top      只带 x-api-key / 只带 Bearer / 两个都带 → 均 401「无效的令牌」
//	agentrouter.org    三种组合 → 均 401 unauthorized_client_error
func TestForwardPathSendsBothAuthHeadersForAnthropic(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{Format: "anthropic", APIKey: "real-key"}, "")

	if got := req.Header.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := req.Header.Get("authorization"); got != "Bearer real-key" {
		t.Errorf("authorization = %q，转发路径也要带该头（中转主要按它取令牌）", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
}

// TestForwardPathAnthropicEmptyKeyOmitsBearer 转发路径的空 key 边界，理由同
// TestSetUpstreamAuthHeadersAnthropicEmptyKeyOmitsBearer。
func TestForwardPathAnthropicEmptyKeyOmitsBearer(t *testing.T) {
	req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
	setUpstreamHeaders(req, &config.Provider{Format: "anthropic"}, "")

	if _, present := req.Header["Authorization"]; present {
		t.Errorf("Authorization = %q，空 key 时不应补该头", req.Header.Get("authorization"))
	}
}

func TestSetUpstreamHeadersUserAgentPriority(t *testing.T) {
	tests := []struct {
		name            string
		providerUA      string
		clientUA        string
		want            string
		wantHeaderExist bool
	}{
		{
			name:            "provider user agent overrides client user agent",
			providerUA:      "provider-agent/1.0",
			clientUA:        "client-agent/2.0",
			want:            "provider-agent/1.0",
			wantHeaderExist: true,
		},
		{
			name:            "client user agent is forwarded when provider is not configured",
			clientUA:        "client-agent/2.0",
			want:            "client-agent/2.0",
			wantHeaderExist: true,
		},
		{
			name:            "user agent remains unset when neither source provides one",
			wantHeaderExist: false,
		},
		{
			name:            "provider user agent is used without a client user agent",
			providerUA:      "provider-agent/1.0",
			want:            "provider-agent/1.0",
			wantHeaderExist: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://gateway.invalid", nil)
			setUpstreamHeaders(req, &config.Provider{Format: "openai", UserAgent: tt.providerUA}, tt.clientUA)

			_, gotHeader := req.Header["User-Agent"]
			if gotHeader != tt.wantHeaderExist {
				t.Fatalf("User-Agent header present = %v, want %v", gotHeader, tt.wantHeaderExist)
			}
			if got := req.Header.Get("User-Agent"); got != tt.want {
				t.Fatalf("User-Agent = %q, want %q", got, tt.want)
			}
		})
	}
}
