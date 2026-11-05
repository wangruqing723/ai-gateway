package chatgptauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本包测试只使用临时目录、假令牌和 httptest 假上游，不读取真实账户文件，也不访问 OpenAI。

// fakeClock 是可手动推进的测试时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeJWT 生成只有 payload 有意义的假 JWT；chatgptauth 只解码声明，不校验签名。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".fake-signature"
}

func fakeIDToken(t *testing.T, accountID, email, plan string) string {
	t.Helper()
	return fakeJWT(t, map[string]any{
		"email": email,
		chatGPTIssuer: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	})
}

// fakeUpstream 同时扮演 OAuth 令牌端点和用量端点；处理函数可在测试中替换。
type fakeUpstream struct {
	t          *testing.T
	server     *httptest.Server
	mu         sync.Mutex
	token      http.HandlerFunc
	usage      http.HandlerFunc
	tokenCalls atomic.Int32
	usageCalls atomic.Int32
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{t: t}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		token, usage := up.token, up.usage
		up.mu.Unlock()
		switch r.URL.Path {
		case "/oauth/token":
			up.tokenCalls.Add(1)
			if token == nil {
				http.Error(w, "unexpected token request", http.StatusTeapot)
				return
			}
			token(w, r)
		case "/usage":
			up.usageCalls.Add(1)
			if usage == nil {
				http.Error(w, "unexpected usage request", http.StatusTeapot)
				return
			}
			usage(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.server.Close)
	return up
}

func (up *fakeUpstream) setToken(handler http.HandlerFunc) {
	up.mu.Lock()
	defer up.mu.Unlock()
	up.token = handler
}

func (up *fakeUpstream) setUsage(handler http.HandlerFunc) {
	up.mu.Lock()
	defer up.mu.Unlock()
	up.usage = handler
}

func writeJSONBody(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// tokenGrant 是假令牌端点的成功响应。
func tokenGrant(access, refresh, idToken string, expiresIn int64) map[string]any {
	return map[string]any{"access_token": access, "refresh_token": refresh, "id_token": idToken, "expires_in": expiresIn}
}

// testEnv 组合一个指向假上游、使用假时钟的账户管理器。
type testEnv struct {
	t     *testing.T
	path  string
	clock *fakeClock
	up    *fakeUpstream
	m     *Manager
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{t: t, path: filepath.Join(t.TempDir(), "data", "chatgpt-accounts.json"), clock: newFakeClock(), up: newFakeUpstream(t)}
	env.m = env.open()
	return env
}

// open 从同一路径重新加载管理器，用于验证落盘与重启恢复。
func (env *testEnv) open() *Manager {
	env.t.Helper()
	m, err := New(env.path, env.up.server.Client())
	if err != nil {
		env.t.Fatalf("新建账户管理器: %v", err)
	}
	m.clock = env.clock.Now
	m.oauthBase = env.up.server.URL
	m.usageEndpoint = env.up.server.URL + "/usage"
	m.callback = "http://localhost:1455/auth/callback"
	return m
}

// addAccount 直接写入一个假账户，返回本地 ID。用于不关心授权流程的测试。
func (env *testEnv) addAccount(remoteID, email, refresh string, expiresAt time.Time) string {
	env.t.Helper()
	unlock, err := env.m.lockForWrite(context.Background())
	if err != nil {
		env.t.Fatal(err)
	}
	defer unlock()
	id, err := randomHex(16)
	if err != nil {
		env.t.Fatal(err)
	}
	authorized := env.clock.Now().Add(-time.Hour)
	item := &account{
		ID: "acc_" + id, Email: email, Plan: "plus", AccountID: remoteID,
		IDToken: fakeIDToken(env.t, remoteID, email, "plus"), AccessToken: "fake-access-" + remoteID,
		RefreshToken: refresh, ExpiresAt: expiresAt, LastRefresh: authorized, AuthorizedAt: &authorized,
	}
	env.m.accounts[item.ID] = item
	if err := env.m.persistLocked(); err != nil {
		env.t.Fatal(err)
	}
	return item.ID
}

// failPersist 让之后的落盘在替换文件前失败，返回恢复函数。
func (env *testEnv) failPersist() func() {
	env.m.mu.Lock()
	env.m.beforeReplace = func() error { return errInjectedPersist }
	env.m.mu.Unlock()
	return func() {
		env.m.mu.Lock()
		env.m.beforeReplace = nil
		env.m.mu.Unlock()
	}
}

var errInjectedPersist = &injectedError{"注入的落盘失败"}

type injectedError struct{ message string }

func (e *injectedError) Error() string { return e.message }
