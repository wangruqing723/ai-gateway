package chatgptauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartLoginBuildsPKCEAuthorizationURL(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authURL.Query()
	if authURL.Path != "/oauth/authorize" || query.Get("client_id") != oauthClientID || query.Get("response_type") != "code" {
		t.Fatalf("授权链接参数错误: %s", start.AuthURL)
	}
	if query.Get("redirect_uri") != "http://localhost:1455/auth/callback" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("回调地址或 PKCE 方法错误: %s", start.AuthURL)
	}
	if query.Get("state") == "" || query.Get("code_challenge") == "" || start.SessionID == "" {
		t.Fatalf("缺少 state、challenge 或 session: %#v", start)
	}
	if !start.ExpiresAt.Equal(env.clock.Now().Add(loginTTL)) {
		t.Fatalf("会话有效期 = %s", start.ExpiresAt)
	}
	if status, ok := env.m.Status(start.SessionID); !ok || status.Status != "pending" {
		t.Fatalf("新会话状态 = %#v %v", status, ok)
	}
}

func TestStartLoginWithoutCallbackServer(t *testing.T) {
	env := newTestEnv(t)
	env.m.callback = ""
	if _, err := env.m.StartLogin(); err == nil {
		t.Fatal("回调服务未启动时不应创建授权会话")
	}
}

// callbackFor 构造与会话匹配的回调链接。
func callbackFor(t *testing.T, env *testEnv, start LoginStart, code string) string {
	t.Helper()
	authURL, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	return "http://localhost:1455/auth/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(authURL.Query().Get("state"))
}

func TestCompleteLoginExchangesCodeWithPKCE(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	authURL, _ := url.Parse(start.AuthURL)
	challenge := authURL.Query().Get("code_challenge")
	idToken := fakeIDToken(t, "remote-1", "one@example.invalid", "plus")
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fake-code" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("redirect_uri") != "http://localhost:1455/auth/callback" {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
			return
		}
		writeJSONBody(w, http.StatusOK, tokenGrant("fake-access", "fake-refresh", idToken, 3600))
	})
	status, err := env.m.CompleteLoginFromCallback(context.Background(), callbackFor(t, env, start, "fake-code"))
	if err != nil || status.Status != "completed" || status.Account == nil {
		t.Fatalf("回调结果 = %#v, %v", status, err)
	}
	if status.Account.Email != "one@example.invalid" || status.Account.Plan != "plus" || status.Account.Status != "ready" {
		t.Fatalf("账户元信息 = %#v", status.Account)
	}
	if status.Account.AuthorizedAt != env.clock.Now().Format(time.RFC3339) || status.Account.Authorization.State != "valid" {
		t.Fatalf("授权时间或状态 = %#v", status.Account)
	}
	info, err := os.Stat(env.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("账户文件权限 = %v, %v", info, err)
	}
	reopened := env.open()
	views := reopened.List(nil)
	if len(views) != 1 || views[0].ID != status.Account.ID {
		t.Fatalf("重启后账户 = %#v", views)
	}
	if _, ok := env.m.Status(start.SessionID); !ok {
		t.Fatal("完成后的会话应保留供页面轮询")
	}
}
func TestCompleteLoginRejectsMismatchedCallbacks(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	valid := callbackFor(t, env, start, "fake-code")
	state := url.QueryEscape(func() string { u, _ := url.Parse(start.AuthURL); return u.Query().Get("state") }())
	cases := map[string]string{
		"错误 state":   "http://localhost:1455/auth/callback?code=fake-code&state=wrong-state",
		"重复 state":   valid + "&state=" + state,
		"错误端口":       strings.Replace(valid, ":1455", ":1457", 1),
		"错误路径":       strings.Replace(valid, "/auth/callback", "/auth/other", 1),
		"https 协议":   strings.Replace(valid, "http://", "https://", 1),
		"带 fragment": valid + "#frag",
		"带用户信息":      strings.Replace(valid, "http://", "http://user@", 1),
		"code 与错误并存": valid + "&error=access_denied",
		"缺少 code":    "http://localhost:1455/auth/callback?state=" + state,
		"相对链接":       "/auth/callback?code=fake-code&state=" + state,
	}
	for name, callback := range cases {
		if _, err := env.m.CompleteLoginFromCallback(context.Background(), callback); !errors.Is(err, ErrInvalidOAuthCallback) {
			t.Errorf("%s: err = %v，期望 ErrInvalidOAuthCallback", name, err)
		}
	}
	if calls := env.up.tokenCalls.Load(); calls != 0 {
		t.Fatalf("无效回调不应交换令牌，实际 %d 次", calls)
	}
	if status, _ := env.m.Status(start.SessionID); status.Status != "pending" {
		t.Fatalf("无效回调不应改变会话状态: %#v", status)
	}
}

func TestCompleteLoginRejectsExpiredSession(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(loginTTL + time.Second)
	if _, err := env.m.CompleteLoginFromCallback(context.Background(), callbackFor(t, env, start, "fake-code")); !errors.Is(err, ErrInvalidOAuthCallback) {
		t.Fatalf("过期会话应拒绝回调，实际 %v", err)
	}
	if _, ok := env.m.Status(start.SessionID); ok {
		t.Fatal("过期会话应被清理")
	}
	if env.up.tokenCalls.Load() != 0 {
		t.Fatal("过期会话不应交换令牌")
	}
}
func TestCompleteLoginUserCancelled(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	callback := strings.Replace(callbackFor(t, env, start, "x"), "code=x", "error=access_denied", 1)
	status, err := env.m.CompleteLoginFromCallback(context.Background(), callback)
	if err != nil || status.Status != "error" || !strings.Contains(status.Message, "取消") {
		t.Fatalf("取消授权结果 = %#v, %v", status, err)
	}
	if env.up.tokenCalls.Load() != 0 || len(env.m.List(nil)) != 0 {
		t.Fatal("取消授权不应交换令牌或创建账户")
	}
}

func TestCompleteLoginConcurrentAndRepeatedCallbacksExchangeOnce(t *testing.T) {
	env := newTestEnv(t)
	start, err := env.m.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	idToken := fakeIDToken(t, "remote-1", "one@example.invalid", "plus")
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		writeJSONBody(w, http.StatusOK, tokenGrant("fake-access", "fake-refresh", idToken, 3600))
	})
	callback := callbackFor(t, env, start, "fake-code")
	first := make(chan LoginStatus, 1)
	go func() {
		status, _ := env.m.CompleteLoginFromCallback(context.Background(), callback)
		first <- status
	}()
	<-entered
	var wait sync.WaitGroup
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			status, err := env.m.CompleteLoginFromCallback(context.Background(), callback)
			if err != nil || status.Status != "processing" {
				t.Errorf("并发回调应返回 processing，实际 %#v, %v", status, err)
			}
		}()
	}
	wait.Wait()
	close(release)
	if status := <-first; status.Status != "completed" {
		t.Fatalf("首个回调 = %#v", status)
	}
	again, err := env.m.CompleteLoginFromCallback(context.Background(), callback)
	if err != nil || again.Status != "completed" {
		t.Fatalf("重复回调应返回已完成状态，实际 %#v, %v", again, err)
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 || len(env.m.List(nil)) != 1 {
		t.Fatalf("令牌交换次数 = %d，账户数 = %d，期望各 1", calls, len(env.m.List(nil)))
	}
}
func TestCompleteLoginExchangeFailuresAreSanitized(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"invalid_grant", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "fake-upstream-secret"})
		}, "invalid_grant"},
		{"缺少账户 ID", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusOK, tokenGrant("fake-access", "fake-refresh", fakeJWT(t, map[string]any{"email": "x@example.invalid"}), 3600))
		}, "账户信息不完整"},
		{"缺少刷新凭据", func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusOK, tokenGrant("fake-access", "", "", 3600))
		}, "missing_tokens"},
		{"非 JSON 响应", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>fake-upstream-secret</html>"))
		}, "invalid_response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.up.setToken(tc.handler)
			start, err := env.m.StartLogin()
			if err != nil {
				t.Fatal(err)
			}
			status, err := env.m.CompleteLoginFromCallback(context.Background(), callbackFor(t, env, start, "fake-code"))
			if err != nil || status.Status != "error" || !strings.Contains(status.Message, tc.want) {
				t.Fatalf("结果 = %#v, %v；期望消息包含 %q", status, err, tc.want)
			}
			if strings.Contains(status.Message, "fake-upstream-secret") || strings.Contains(status.Message, "fake-access") {
				t.Fatalf("错误消息泄露上游正文或令牌: %q", status.Message)
			}
			if len(env.m.List(nil)) != 0 {
				t.Fatal("失败的授权不应创建账户")
			}
		})
	}
}
func TestReloginKeepsLocalIdentityAndMetadata(t *testing.T) {
	env := newTestEnv(t)
	login := func(access string) LoginStatus {
		t.Helper()
		start, err := env.m.StartLogin()
		if err != nil {
			t.Fatal(err)
		}
		idToken := fakeIDToken(t, "remote-1", "one@example.invalid", "plus")
		env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
			writeJSONBody(w, http.StatusOK, tokenGrant(access, "refresh-"+access, idToken, 3600))
		})
		status, err := env.m.CompleteLoginFromCallback(context.Background(), callbackFor(t, env, start, "code-"+access))
		if err != nil || status.Status != "completed" {
			t.Fatalf("授权 = %#v, %v", status, err)
		}
		return status
	}
	first := login("first-access")
	tags := []string{"工作"}
	note := "保留的备注"
	if _, err := env.m.UpdateMetadata(first.Account.ID, AccountMetadataPatch{Tags: &tags, Note: &note}); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(24 * time.Hour)
	second := login("second-access")
	if second.Account.ID != first.Account.ID || second.Account.AuthorizedAt != first.Account.AuthorizedAt {
		t.Fatalf("重新授权改变了本地 ID 或首次授权时间: %#v vs %#v", first.Account, second.Account)
	}
	if len(second.Account.Tags) != 1 || second.Account.Tags[0] != "工作" || second.Account.Note != note {
		t.Fatalf("重新授权丢失标签备注: %#v", second.Account)
	}
	token, _, err := env.m.AccessToken(context.Background(), first.Account.ID, nil)
	if err != nil || token != "second-access" {
		t.Fatalf("重新授权后令牌 = %q, %v", token, err)
	}
}
