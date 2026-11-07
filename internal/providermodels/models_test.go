package providermodels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseCatalog(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		bad  bool
	}{
		{"可见去重且不按API认证过滤", `{"models":[{"slug":"alpha","visibility":"list","supported_in_api":false},{"slug":"hidden","visibility":"hide"},{"slug":"alpha","visibility":"list"},{"slug":"beta","visibility":"list","extra":1}]}`, []string{"alpha", "beta"}, false},
		{"空目录", `{"models":[]}`, []string{}, false},
		{"缺字段", `{}`, nil, true},
		{"null", `{"models":null}`, nil, true},
		{"标准API清单不是Codex目录", `{"data":[{"id":"alpha"}]}`, nil, true},
		{"字段类型", `{"models":"private-secret"}`, nil, true},
		{"空模型", `{"models":[{"slug":"","visibility":"list"}]}`, nil, true},
		{"控制字符", `{"models":[{"slug":"alpha\u000a","visibility":"list"}]}`, nil, true},
		{"额外内容", `{"models":[]} {}`, nil, true},
		{"未知可见性不列出", `{"models":[{"slug":"alpha","visibility":"future"}]}`, []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, failure := Parse([]byte(tc.body))
			if tc.bad {
				if failure == nil || failure.Code != "invalid_response" || strings.Contains(failure.Error(), "private-secret") {
					t.Fatalf("未正确拒绝或泄露错误：%v", failure)
				}
				return
			}
			if failure != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("目录=%v err=%v", got, failure)
			}
		})
	}
	tooMany := make([]map[string]string, MaxModels+1)
	for i := range tooMany {
		tooMany[i] = map[string]string{"slug": "alpha", "visibility": "list"}
	}
	body, _ := json.Marshal(map[string]any{"models": tooMany})
	if _, err := Parse(body); err == nil {
		t.Fatal("目录数量必须受限")
	}
	body = []byte(`{"models":[{"slug":"` + strings.Repeat("a", MaxModelNameBytes+1) + `","visibility":"list"}]}`)
	if _, err := Parse(body); err == nil {
		t.Fatal("模型名称必须受限")
	}
}

func TestFetchCatalogRequestAndHeaders(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/prefix/codex/models" || r.URL.Query().Get("client_version") != "1.2.3" {
			t.Errorf("目录请求不正确：%s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fake-catalog-token" || r.Header.Get("ChatGPT-Account-Id") != "fake-account" || r.Header.Get("User-Agent") != "test-catalog-agent" {
			t.Error("目录凭据或UA错误")
		}
		if r.Header.Get("X-Test-Header") != "preserved" || r.Header.Get("Cookie") != "" {
			t.Error("附加头或cookie边界错误")
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"fake-model","visibility":"list","supported_in_api":false}]}`)
	}))
	defer upstream.Close()
	query := Query{BaseURL: upstream.URL + "/prefix/codex/", ClientVersion: "1.2.3", Token: "fake-catalog-token", AccountID: "fake-account", UserAgent: "test-catalog-agent", ExtraHeaders: map[string]string{"Authorization": "bad", "ChatGPT-Account-Id": "bad", "Cookie": "bad", "X-Test-Header": "preserved"}}
	result, err := Fetch(context.Background(), upstream.Client(), query)
	if err != nil || result == nil || !reflect.DeepEqual(result.Models, []string{"fake-model"}) || result.QueriedAt == "" || calls.Load() != 1 {
		t.Fatalf("目录查询结果不正确：%#v %v", result, err)
	}
}

func TestFetchCatalogFailuresAreSafe(t *testing.T) {
	for _, status := range []int{301, 401, 403, 404, 405, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "70")
				w.Header().Set("Location", "/secret-redirect")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-credential-secret")
			}))
			defer upstream.Close()
			_, err := Fetch(context.Background(), upstream.Client(), Query{BaseURL: upstream.URL, Token: "fake-token", AccountID: "fake-account"})
			if err == nil || err.HTTPStatus != status || strings.Contains(err.Error(), "private-credential-secret") {
				t.Fatalf("目录错误边界=%#v", err)
			}
			if status == 429 && err.RetryAfter != 70*time.Second {
				t.Fatalf("Retry-After=%v", err.RetryAfter)
			}
		})
	}
}

func TestFetchCatalogTooLargeCanceledAndRedirect(t *testing.T) {
	var redirected atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirected.Add(1)
			return
		}
		if r.URL.Query().Get("client_version") == "redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, strings.Repeat(" ", MaxResponseBytes+1))
	}))
	defer upstream.Close()
	query := Query{BaseURL: upstream.URL, Token: "fake", AccountID: "fake"}
	if _, err := Fetch(context.Background(), upstream.Client(), query); err == nil || err.Code != "response_too_large" {
		t.Fatalf("超大目录响应=%v", err)
	}
	query.ClientVersion = "redirect"
	if _, err := Fetch(context.Background(), upstream.Client(), query); err == nil || redirected.Load() != 0 {
		t.Fatal("目录查询不能跟随重定向")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fetch(ctx, upstream.Client(), query); err == nil || err.Code != "query_cancelled" {
		t.Fatalf("取消结果=%v", err)
	}
}
