package chatgptauth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccessTokenReusesValidTokenWithoutRefresh(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	token, accountID, err := env.m.AccessToken(context.Background(), id, nil)
	if err != nil || token != "fake-access-remote-1" || accountID != "remote-1" {
		t.Fatalf("AccessToken = %q %q %v", token, accountID, err)
	}
	if env.up.tokenCalls.Load() != 0 {
		t.Fatal("有效令牌不应触发刷新")
	}
}

func TestAccessTokenRefreshesNearExpiryAndRotates(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(refreshBefore-time.Second))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != oauthClientID {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
			return
		}
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", fakeIDToken(t, "remote-1", "renamed@example.invalid", "pro"), 7200))
	})
	token, _, err := env.m.AccessToken(context.Background(), id, nil)
	if err != nil || token != "new-access" {
		t.Fatalf("刷新结果 = %q, %v", token, err)
	}
	reopened := env.open()
	reopened.mu.RLock()
	item := reopened.accounts[id]
	reopened.mu.RUnlock()
	if item.RefreshToken != "new-refresh" || item.AccessToken != "new-access" || !item.ExpiresAt.Equal(env.clock.Now().Add(2*time.Hour)) {
		t.Fatalf("轮换后的凭据未落盘: %#v", item)
	}
	if item.Email != "renamed@example.invalid" || item.Plan != "pro" || !item.LastRefresh.Equal(env.clock.Now()) {
		t.Fatalf("刷新未更新账户元信息: email=%q plan=%q", item.Email, item.Plan)
	}
}
func TestAccessTokenInvalidGrantRequiresReauthorization(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "revoked-refresh", env.clock.Now().Add(-time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
		t.Fatal("invalid_grant 应返回错误")
	}
	view, _ := env.m.Account(id)
	if view.Status != "reauthorization_required" || view.Authorization.State != "reauthorization_required" || view.Authorization.CanRefresh {
		t.Fatalf("invalid_grant 后视图 = %#v", view)
	}
	// 需要重新授权后不再向上游重复交换。
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil || !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("再次取令牌应直接要求重新授权，实际 %v", err)
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("令牌交换次数 = %d，期望 1", calls)
	}
	if reopened, _ := env.open().Account(id); reopened.Status != "reauthorization_required" {
		t.Fatalf("重新授权要求未落盘: %#v", reopened)
	}
}

func TestAccessTokenTransientFailureStaysRefreshable(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
		t.Fatal("上游 503 应返回错误")
	}
	view, _ := env.m.Account(id)
	if view.Status != "ready" || view.Authorization.State != "refreshable" || !view.Authorization.CanRefresh {
		t.Fatalf("临时失败不应要求重新授权: %#v", view.Authorization)
	}
	if view.Authorization.LastRefreshFailedAt == "" || !strings.Contains(view.Authorization.LastRefreshError, "HTTP 503") {
		t.Fatalf("应记录最近刷新失败: %#v", view.Authorization)
	}
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 3600))
	})
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	view, _ = env.m.Account(id)
	if view.Authorization.State != "valid" || view.Authorization.LastRefreshError != "" {
		t.Fatalf("刷新成功后应清除失败记录: %#v", view.Authorization)
	}
}
func TestAccessTokenCallerCancelDoesNotRecordFailure(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		// 必须先读完请求体：服务端只有在请求体读到 EOF 后才启动后台读，
		// 不读就感知不到客户端断开，httptest.Server.Close 会一直等这个处理函数。
		_, _ = io.ReadAll(r.Body)
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	if _, _, err := env.m.AccessToken(ctx, id, nil); err == nil {
		t.Fatal("取消的刷新应返回错误")
	}
	view, _ := env.m.Account(id)
	if view.Authorization.LastRefreshError != "" || view.Authorization.State != "refreshable" {
		t.Fatalf("调用方取消不应记成刷新失败: %#v", view.Authorization)
	}
}

func TestAccessTokenConcurrentCallersShareOneRefresh(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 3600))
	})
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, _, err := env.m.AccessToken(context.Background(), id, nil)
			if err != nil || token != "new-access" {
				t.Errorf("并发取令牌 = %q, %v", token, err)
			}
		}()
	}
	wait.Wait()
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("并发调用应只刷新一次，实际 %d 次", calls)
	}
}
func TestAccessTokenPersistFailureKeepsRotatedTokenInMemory(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 3600))
	})
	restore := env.failPersist()
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil || !strings.Contains(err.Error(), "保存刷新后") {
		t.Fatalf("落盘失败应返回错误，实际 %v", err)
	}
	view, _ := env.m.Account(id)
	if !view.Authorization.CredentialsUnsaved || view.Authorization.State != "valid" {
		t.Fatalf("应标记新凭据未保存: %#v", view.Authorization)
	}
	// 上游已轮换 refresh token，内存不能回滚到旧凭据：下次直接复用新令牌，不再交换。
	token, _, err := env.m.AccessToken(context.Background(), id, nil)
	if err != nil || token != "new-access" || env.up.tokenCalls.Load() != 1 {
		t.Fatalf("应复用内存中的新令牌: %q %v calls=%d", token, err, env.up.tokenCalls.Load())
	}
	restore()
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &[]string{"x"}}); err != nil {
		t.Fatal(err)
	}
	view, _ = env.m.Account(id)
	if view.Authorization.CredentialsUnsaved {
		t.Fatal("之后的成功落盘应清除未保存标记")
	}
	reopened := env.open()
	reopened.mu.RLock()
	saved := reopened.accounts[id].RefreshToken
	reopened.mu.RUnlock()
	if saved != "new-refresh" {
		t.Fatalf("恢复落盘后应保存新凭据，实际 %q", saved)
	}
}

type countingTransport struct {
	calls atomic.Int32
	base  http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(r)
}

func TestAccessTokenUsesCallerClientForRefresh(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(-time.Minute))
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", "", 3600))
	})
	providerTransport := &countingTransport{base: env.up.server.Client().Transport}
	if _, _, err := env.m.AccessToken(context.Background(), id, &http.Client{Transport: providerTransport}); err != nil {
		t.Fatal(err)
	}
	if providerTransport.calls.Load() != 1 {
		t.Fatalf("转发路径的刷新应使用 Provider 自己的 client，实际调用 %d 次", providerTransport.calls.Load())
	}
}
