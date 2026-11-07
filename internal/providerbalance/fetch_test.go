package providerbalance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func value(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
func TestFetchProtocols(t *testing.T) {
	tests := []struct {
		name, typ, source, body, ratio, kind, remaining, used, limit string
		unlimited                                                    bool
		items, windows                                               int
		code                                                         string
	}{
		{name: "账户精确大整数", typ: "newapi", source: "account", body: `{"success":true,"data":{"id":123,"quota":9007199254740993,"used_quota":0,"email":"secret@example.invalid"}}`, ratio: "1", kind: "account_balance", remaining: "9007199254740993", used: "0", limit: "<nil>", items: 1},
		{name: "账户原始单位", typ: "newapi", source: "account", body: `{"success":true,"data":{"quota":0}}`, kind: "account_balance", remaining: "0", used: "<nil>", limit: "<nil>", items: 1},
		{name: "密钥与账户授权", typ: "newapi", source: "apiKey", body: `{"code":true,"data":{"total_available":150,"total_used":50,"total_granted":200,"unlimited_quota":false,"user_quota":{"total_available":700,"total_used":30}}}`, ratio: "100", kind: "key_quota", remaining: "1.5", used: "0.5", limit: "2", items: 2},
		{name: "新版success信封", typ: "newapi", source: "apiKey", body: `{"success":true,"data":{"total_available":0,"total_used":0,"unlimited_quota":false}}`, ratio: "1", kind: "key_quota", remaining: "0", used: "0", limit: "<nil>", items: 1},
		{name: "密钥不限额不扩散账户", typ: "newapi", source: "apiKey", body: `{"code":true,"data":{"total_available":-3,"total_used":0,"unlimited_quota":true,"user_quota":{"total_available":0}}}`, ratio: "1", kind: "key_quota", remaining: "<nil>", used: "0", limit: "<nil>", unlimited: true, items: 2},
		{name: "账户身份不符", typ: "newapi", source: "account", body: `{"success":true,"data":{"id":124,"quota":3}}`, code: "invalid_response"},
		{name: "200鉴权失败", typ: "newapi", source: "account", body: `{"success":false,"message":"fake-secret"}`, code: "credential_rejected"},
		{name: "缺信封", typ: "newapi", source: "account", body: `{"data":{"quota":3}}`, code: "invalid_response"},
		{name: "畸形信封", typ: "newapi", source: "account", body: `{"success":{},"data":{"quota":3}}`, code: "invalid_response"},
		{name: "冲突信封", typ: "newapi", source: "apiKey", body: `{"success":false,"code":true,"data":{}}`, code: "credential_rejected"},
		{name: "未授权账户字段不能假定存在", typ: "newapi", source: "apiKey", body: `{"code":true,"data":{"total_available":10,"unlimited_quota":false,"user_quota":{}}}`, code: "invalid_response"},
		{name: "账户钱包", typ: "sub2api", source: "account", body: `{"code":0,"data":{"balance":0.123456789012345678,"frozen_balance":999}}`, kind: "account_balance", remaining: "0.123456789012345678", used: "<nil>", limit: "<nil>", items: 1},
		{name: "账户失败信封", typ: "sub2api", source: "account", body: `{"code":403,"message":"fake-secret"}`, code: "permission_denied"},
		{name: "账户空余额", typ: "sub2api", source: "account", body: `{"code":0,"data":{"balance":null}}`, code: "invalid_response"},
		{name: "钱包负数非无限额", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"USD","balance":-1,"remaining":-1}`, kind: "account_balance", remaining: "-1", used: "<nil>", limit: "<nil>", items: 1},
		{name: "密钥总限额", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"quota_limited","quota":{"unit":"USD","limit":10,"used":1.2,"remaining":8.8}}`, kind: "key_quota", remaining: "8.8", used: "1.2", limit: "10", items: 1},
		{name: "仅周期限额不补零", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"quota_limited","rate_limits":[{"window":"5h","limit":10,"used":2,"remaining":8,"reset_at":"2030-01-01T00:00:00Z"}]}`, kind: "key_quota", remaining: "<nil>", used: "<nil>", limit: "<nil>", items: 1, windows: 1},
		{name: "订阅剩余", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"USD","remaining":8,"subscription":{"daily_limit_usd":10,"daily_usage_usd":2,"weekly_limit_usd":null,"expires_at":"2030-01-01T00:00:00Z"}}`, kind: "subscription_quota", remaining: "8", used: "<nil>", limit: "<nil>", items: 1, windows: 1},
		{name: "订阅不限额", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"USD","remaining":-1,"subscription":{"daily_limit_usd":null,"daily_usage_usd":0}}`, kind: "subscription_quota", remaining: "<nil>", used: "<nil>", limit: "<nil>", unlimited: true, items: 1, windows: 1},
		{name: "矛盾订阅不限额", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"USD","remaining":-1,"subscription":{"daily_limit_usd":10}}`, code: "invalid_response"},
		{name: "只有用量", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","usage":{"total":{"actual_cost":0}}}`, kind: "usage_only", remaining: "<nil>", used: "0", limit: "<nil>", items: 1},
		{name: "未知模式", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"wallet","remaining":1}`, code: "invalid_response"},
		{name: "余额缺失不可猜测", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","remaining":-1}`, code: "invalid_response"},
		{name: "非USD拒绝", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"CNY","balance":1}`, code: "invalid_response"},
		{name: "失效密钥", typ: "sub2api", source: "apiKey", body: `{"isValid":false,"mode":"unrestricted","unit":"USD","balance":1}`, code: "credential_rejected"},
		{name: "负密钥剩余不可作不限额", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"quota_limited","quota":{"unit":"USD","limit":10,"remaining":-1}}`, code: "invalid_response"},
		{name: "非法周期时间", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"quota_limited","rate_limits":[{"window":"5h","limit":10,"reset_at":"fake-secret"}]}`, code: "invalid_response"},
		{name: "重复周期", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"quota_limited","rate_limits":[{"window":"5h","limit":10},{"window":"5h","limit":20}]}`, code: "invalid_response"},
		{name: "订阅窗口用量超限归零", typ: "sub2api", source: "apiKey", body: `{"isValid":true,"mode":"unrestricted","unit":"USD","remaining":0,"subscription":{"daily_limit_usd":10,"daily_usage_usd":12}}`, kind: "subscription_quota", remaining: "0", used: "<nil>", limit: "<nil>", items: 1, windows: 1},
		{name: "账户授权负余额", typ: "newapi", source: "apiKey", body: `{"code":true,"data":{"total_available":10,"unlimited_quota":false,"user_quota":{"total_available":-1}}}`, ratio: "1", kind: "key_quota", remaining: "10", used: "<nil>", limit: "<nil>", items: 2},
		{name: "非法数值", typ: "sub2api", source: "account", body: `{"code":0,"data":{"balance":"NaN"}}`, code: "invalid_response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prefix/api/status" {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			q := Query{Type: tt.typ, Source: tt.source, BaseURL: server.URL + "/prefix/", Token: "fake-secret", KeyID: "key1", UserID: "123", QuotaPerUnit: tt.ratio}
			snapshot, err := Fetch(context.Background(), server.Client(), q)
			if tt.code != "" {
				if err == nil || err.Code != tt.code {
					t.Fatalf("错误=%v，预期 %s", err, tt.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Items) != tt.items {
				t.Fatalf("条目数=%d", len(snapshot.Items))
			}
			item := snapshot.Items[0]
			if item.Kind != tt.kind || value(item.Remaining) != tt.remaining || value(item.Used) != tt.used || value(item.Limit) != tt.limit || item.Unlimited != tt.unlimited || len(item.Windows) != tt.windows {
				t.Fatalf("额度项不符: %+v，remaining=%s used=%s limit=%s", item, value(item.Remaining), value(item.Used), value(item.Limit))
			}
			encoded, _ := json.Marshal(snapshot)
			for _, secret := range []string{"fake-secret", "secret@example.invalid", "frozen_balance"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("泄露 %s", secret)
				}
			}
			if tt.typ == "newapi" {
				if item.RawUnit == nil || *item.RawUnit != "quota" {
					t.Fatal("缺原始配额单位")
				}
				if tt.ratio == "" && len(snapshot.Warnings) != 1 {
					t.Fatal("缺单位警告")
				}
			}
			if tt.items == 2 && snapshot.Items[1].Unlimited {
				t.Fatal("密钥不限额扩散到了账户")
			}
		})
	}
}

func TestFetchUnitDiscoveryHeadersAndPrefix(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("User-Agent") != "fake-UA" {
			t.Error("缺少 UA")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Extra") != "" {
			t.Error("继承了额外凭据")
		}
		switch r.URL.Path {
		case "/proxy/api/user/self":
			if r.Header.Get("Authorization") != "Bearer fake-token" || r.Header.Get("New-Api-User") != "123" {
				t.Error("账户认证错误")
			}
			fmt.Fprint(w, `{"success":true,"data":{"id":123,"quota":100,"used_quota":0}}`)
		case "/proxy/api/status":
			if r.Header.Get("Authorization") != "" || r.Header.Get("New-Api-User") != "" {
				t.Error("公开单位发现泄露凭据")
			}
			fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":10}}`)
		default:
			t.Errorf("错误路径 %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := server.Client()
	jar, _ := cookiejar.New(nil)
	client.Jar = jar
	u, _ := url.Parse(server.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "fake-session", Value: "secret"}})
	snapshot, err := Fetch(context.Background(), client, Query{Type: "newapi", Source: "account", BaseURL: server.URL + "/proxy/", Token: "fake-token", UserID: "123", UserAgent: "fake-UA"})
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Items[0]
	if requests.Load() != 2 || value(item.Remaining) != "10" || value(item.RawRemaining) != "100" || value(item.QuotaPerUnit) != "10" || value(item.ConversionSource) != "upstream_status" {
		t.Fatalf("换算错误: %+v", item)
	}
	if client.Jar != jar || client.CheckRedirect != nil {
		t.Fatal("修改了原 client")
	}
}

func TestProtocolRequestPaths(t *testing.T) {
	for _, tt := range []struct{ typ, source, path, body string }{
		{"newapi", "account", "/nested/api/user/self", `{"success":true,"data":{"id":123,"quota":0}}`},
		{"newapi", "apiKey", "/nested/api/usage/token", `{"code":true,"data":{"total_available":0,"unlimited_quota":false}}`},
		{"sub2api", "account", "/nested/api/v1/auth/me", `{"code":0,"data":{"balance":0}}`},
		{"sub2api", "apiKey", "/nested/v1/usage", `{"isValid":true,"mode":"unrestricted","unit":"USD","balance":0}`},
	} {
		t.Run(tt.typ+tt.source, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tt.path || r.Header.Get("Authorization") != "Bearer fake-token" || r.Header.Get("User-Agent") != "Go-http-client/1.1" {
					t.Error("请求路径、认证或默认UA不符")
				}
				expected := ""
				if tt.typ == "newapi" && tt.source == "account" {
					expected = "000123"
				}
				if r.Header.Get("New-Api-User") != expected {
					t.Error("兼容用户身份头不符")
				}
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			ratio := ""
			if tt.typ == "newapi" {
				ratio = "1"
			}
			_, err := Fetch(context.Background(), server.Client(), Query{Type: tt.typ, Source: tt.source, BaseURL: server.URL + "/nested", Token: "fake-token", UserID: "000123", QuotaPerUnit: ratio})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJSONContract(t *testing.T) {
	raw, _ := json.Marshal(newItem("account_balance", "source", "USD", Query{}))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	expected := []string{"kind", "source", "remaining", "used", "limit", "unit", "unlimited", "keyId", "windows", "rawRemaining", "rawUsed", "rawUnit", "quotaPerUnit", "conversionSource"}
	if len(fields) != len(expected) {
		t.Fatalf("额度JSON字段数: %d", len(fields))
	}
	for _, name := range expected {
		if _, ok := fields[name]; !ok {
			t.Fatalf("缺字段 %s", name)
		}
	}
	for _, name := range []string{"remaining", "used", "limit", "keyId", "rawRemaining", "rawUsed", "rawUnit", "quotaPerUnit", "conversionSource"} {
		if string(fields[name]) != "null" {
			t.Fatalf("未知字段%s不为null", name)
		}
	}
	if string(fields["windows"]) != "[]" {
		t.Fatal("窗口不是空数组")
	}
}

func TestFetchHTTPFailures(t *testing.T) {
	for _, tt := range []struct {
		status int
		code   string
	}{{401, "credential_rejected"}, {403, "permission_denied"}, {404, "unsupported_endpoint"}, {405, "unsupported_endpoint"}, {429, "rate_limited"}, {500, "invalid_response"}} {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "42")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, "fake-secret")
			}))
			defer server.Close()
			_, err := Fetch(context.Background(), server.Client(), Query{Type: "sub2api", Source: "account", BaseURL: server.URL, Token: "fake-secret"})
			if err == nil || err.Code != tt.code || err.HTTPStatus != tt.status || err.RetryAfter != 42*time.Second {
				t.Fatalf("错误: %+v", err)
			}
			raw, _ := json.Marshal(err)
			if strings.Contains(string(raw), "fake-secret") || strings.Contains(string(raw), "RetryAfter") || strings.Contains(string(raw), "retryAfter") {
				t.Fatal("错误内容泄露")
			}
		})
	}
}

func TestFetchInvalidBodies(t *testing.T) {
	valid := `{"code":0,"data":{"balance":0}}`
	for _, tt := range []struct{ name, body, code string }{
		{"HTML", `<html>fake-secret</html>`, "invalid_response"},
		{"null", `null`, "invalid_response"},
		{"尾随JSON", valid + `{}`, "invalid_response"},
		{"超过限制", valid + strings.Repeat(" ", (1<<20)+1-len(valid)), "response_too_large"},
		{"恰好限制", valid + strings.Repeat(" ", (1<<20)-len(valid)), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) }))
			defer server.Close()
			_, err := Fetch(context.Background(), server.Client(), Query{Type: "sub2api", Source: "account", BaseURL: server.URL, Token: "fake-secret"})
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Code != tt.code {
				t.Fatalf("错误=%v", err)
			}
		})
	}
}

func TestFetchRedirectRejected(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		fmt.Fprint(w, `{"code":0,"data":{"balance":1}}`)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	var redirectCalled atomic.Int32
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { redirectCalled.Add(1); return nil }
	_, err := Fetch(context.Background(), client, Query{Type: "sub2api", Source: "account", BaseURL: server.URL, Token: "fake-secret"})
	if err == nil || err.Code != "invalid_response" || received.Load() != 0 || redirectCalled.Load() != 0 {
		t.Fatal("重定向未拒绝")
	}
}

func TestFetchCancellationAndSharedBudget(t *testing.T) {
	for _, unitDiscovery := range []bool{false, true} {
		t.Run(fmt.Sprint(unitDiscovery), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if unitDiscovery && r.URL.Path == "/api/user/self" {
					fmt.Fprint(w, `{"success":true,"data":{"quota":0}}`)
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			typ := "sub2api"
			if unitDiscovery {
				typ = "newapi"
			}
			snapshot, err := Fetch(ctx, server.Client(), Query{Type: typ, Source: "account", BaseURL: server.URL, Token: "fake-secret"})
			if unitDiscovery {
				if err != nil || len(snapshot.Warnings) != 1 || value(snapshot.Items[0].Remaining) != "0" {
					t.Fatalf("单位发现失败丢失已取得余额: %v", err)
				}
			} else if err == nil || err.Code != "upstream_timeout" {
				t.Fatalf("错误=%v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Fetch(ctx, http.DefaultClient, Query{Type: "sub2api", Source: "account", BaseURL: "http://127.0.0.1:1", Token: "fake-secret"})
	if err == nil || err.Code != "query_cancelled" {
		t.Fatalf("错误=%v", err)
	}
}

func TestDecimal(t *testing.T) {
	for _, tt := range []struct {
		input, want string
		ok          bool
	}{
		{"0", "0", true}, {"-0", "0", true}, {"9007199254740993", "9007199254740993", true},
		{"0.123456789012345678", "0.123456789012345678", true}, {"1e2", "100", true}, {"1e-18", "0.000000000000000001", true},
		{"1e-19", "", false}, {"1000000000000000001", "", false}, {"1e9999999999999999", "", false}, {"NaN", "", false}, {"+1", "", false}, {"1/2", "", false}, {"01", "", false}, {"0.1234567890123456789", "", false},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := decimal(json.Number(tt.input))
			if ok != tt.ok || got != tt.want {
				t.Fatalf("结果 %q,%v", got, ok)
			}
		})
	}
}

func TestQuotaPerUnitLeadingZeros(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/self" {
			t.Error("显式单位不应发起单位发现")
		}
		fmt.Fprint(w, `{"success":true,"data":{"quota":9}}`)
	}))
	defer server.Close()
	for _, tt := range []struct{ raw, normalized, remaining string }{{"0009", "9", "1"}, {"0001.50", "1.5", "6"}, {"000.5", "0.5", "18"}} {
		t.Run(tt.raw, func(t *testing.T) {
			snapshot, err := Fetch(context.Background(), server.Client(), Query{Type: "newapi", Source: "account", BaseURL: server.URL, Token: "fake-token", QuotaPerUnit: tt.raw})
			if err != nil {
				t.Fatal(err)
			}
			item := snapshot.Items[0]
			if value(item.QuotaPerUnit) != tt.normalized || value(item.Remaining) != tt.remaining || value(item.ConversionSource) != "configuration" {
				t.Fatalf("配置规范化失败: %+v", item)
			}
		})
	}
	if _, ok := decimal(json.Number("0009")); ok {
		t.Fatal("上游数字语法被放宽")
	}
}

func TestQueryValidationAndUserID(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"id":9223372036854775807,"quota":0}}`)
	}))
	defer server.Close()
	base := Query{Type: "newapi", Source: "account", BaseURL: server.URL, Token: "fake-secret", QuotaPerUnit: "1"}
	for _, tt := range []struct {
		name   string
		change func(*Query)
	}{
		{"配置指数", func(q *Query) { q.QuotaPerUnit = "1e2" }},
		{"配置零", func(q *Query) { q.QuotaPerUnit = "0" }},
		{"配置负数", func(q *Query) { q.QuotaPerUnit = "-1" }},
		{"配置精度", func(q *Query) { q.QuotaPerUnit = "0.0000000000000000001" }},
		{"配置超额", func(q *Query) { q.QuotaPerUnit = "1000000000000000001" }},
		{"凭据控制符", func(q *Query) { q.Token = "fake\nsecret" }},
		{"不返回令牌作为ID", func(q *Query) { q.KeyID = q.Token }},
		{"ID控制符", func(q *Query) { q.KeyID = "bad\nID" }},
		{"用户ID符号", func(q *Query) { q.UserID = "+123" }},
		{"用户ID溢出", func(q *Query) { q.UserID = "9223372036854775808" }},
		{"userinfo", func(q *Query) { q.BaseURL = "http://fake:secret@127.0.0.1" }},
		{"fragment", func(q *Query) { q.BaseURL += "#fragment" }},
		{"query", func(q *Query) { q.BaseURL += "?token=fake" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := base
			tt.change(&q)
			_, err := Fetch(context.Background(), server.Client(), q)
			if err == nil {
				t.Fatal("未拒绝不合法查询")
			}
		})
	}
	if called.Load() != 0 {
		t.Fatal("校验失败仍然出网")
	}
	base.UserID = "09223372036854775807"
	if _, err := Fetch(context.Background(), server.Client(), base); err != nil {
		t.Fatalf("前导零或最大int64身份失败: %v", err)
	}
}

func TestUnitDiscoveryFailuresRemainRaw(t *testing.T) {
	for _, body := range []string{`{"success":false}`, `{"success":true,"data":{"quota_per_unit":0}}`, `{"success":true,"data":{"quota_per_unit":"NaN"}}`, `<html>fake-secret</html>`, strings.Repeat("x", (1<<20)+1)} {
		t.Run(fmt.Sprint(len(body))+body[:1], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/user/self" {
					fmt.Fprint(w, `{"success":true,"data":{"quota":0}}`)
				} else {
					fmt.Fprint(w, body)
				}
			}))
			defer server.Close()
			snapshot, err := Fetch(context.Background(), server.Client(), Query{Type: "newapi", Source: "account", BaseURL: server.URL, Token: "fake-secret"})
			if err != nil || snapshot.Items[0].Unit != "quota" || value(snapshot.Items[0].Remaining) != "0" || len(snapshot.Warnings) != 1 {
				t.Fatalf("单位失败未保留原始值: %v", err)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		input string
		want  time.Duration
	}{{"0", 0}, {"-1", 0}, {"42", 42 * time.Second}, {"9223372036854775807", 0}, {"invalid", 0}, {now.Add(5 * time.Minute).Format(http.TimeFormat), 5 * time.Minute}, {now.Add(-time.Minute).Format(http.TimeFormat), 0}} {
		if got := retryAfter(tt.input, now); got != tt.want {
			t.Fatalf("Retry-After %q: %s", tt.input, got)
		}
	}
}

func TestSnapshotClone(t *testing.T) {
	s := &Snapshot{Items: []Item{{Remaining: ptr("1"), RawRemaining: ptr("2"), Windows: []Window{{Remaining: ptr("3"), ResetAt: ptr("time")}}}}, Warnings: []string{"warning"}}
	cloned := s.Clone()
	*cloned.Items[0].Remaining = "changed"
	*cloned.Items[0].RawRemaining = "changed"
	*cloned.Items[0].Windows[0].Remaining = "changed"
	*cloned.Items[0].Windows[0].ResetAt = "changed"
	cloned.Warnings[0] = "changed"
	if value(s.Items[0].Remaining) != "1" || value(s.Items[0].RawRemaining) != "2" || value(s.Items[0].Windows[0].Remaining) != "3" || value(s.Items[0].Windows[0].ResetAt) != "time" || s.Warnings[0] != "warning" {
		t.Fatal("快照副本共享内存")
	}
	var empty *Snapshot
	if empty.Clone() != nil {
		t.Fatal("nil副本不为nil")
	}
}
