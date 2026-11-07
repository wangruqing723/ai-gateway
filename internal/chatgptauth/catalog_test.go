package chatgptauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestCatalogIdentityAndCredentials(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "fake@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	identity, err := env.m.CatalogIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	before := env.m.Version()
	_, _, _, err = env.m.SetPaused(id, true, before)
	if err != nil {
		t.Fatal(err)
	}
	token, remote, captured, err := env.m.CatalogCredentials(context.Background(), id, nil)
	if err != nil || token != "fake-access-remote-1" || remote != "remote-1" || captured != identity || !env.m.CatalogIdentityCurrent(id, captured) {
		t.Fatalf("目录身份或暂停账户查询不正确：token=%t remote=%q err=%v", token != "", remote, err)
	}
	if env.up.tokenCalls.Load() != 0 || env.up.usageCalls.Load() != 0 {
		t.Fatal("有效凭据的目录快照不能刷新或查询用量")
	}
	env.m.mu.Lock()
	env.m.accounts[id].credentialVersion++
	env.m.mu.Unlock()
	if env.m.CatalogIdentityCurrent(id, captured) {
		t.Fatal("凭据替换后目录身份没有失效")
	}
	if _, err := env.m.CatalogIdentity("missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在身份错误=%v", err)
	}
}

func TestCatalogCredentialsRefreshAndGeneration(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "fake@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	before, err := env.m.CatalogIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		writeJSONBody(w, http.StatusOK, tokenGrant("new-access", "new-refresh", fakeIDToken(t, "remote-1", "fake@example.invalid", "plus"), 3600))
	})
	token, remote, after, err := env.m.CatalogCredentials(context.Background(), id, env.up.server.Client())
	if err != nil || token != "new-access" || remote != "remote-1" || before == after || !env.m.CatalogIdentityCurrent(id, after) {
		t.Fatalf("刷新后的目录快照错误：token=%t remote=%q err=%v", token != "", remote, err)
	}
	if env.up.tokenCalls.Load() != 1 {
		t.Fatal("目录取令牌应复用一次刷新交换")
	}
	if view, ok := env.m.Account(id); !ok || view.Invocation.State != "unknown" {
		t.Fatal("取目录凭据不能计为模型调用")
	}
}

func TestCatalogIdentityDeleteRecreateAndReauth(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "fake@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	identity, _ := env.m.CatalogIdentity(id)
	env.m.mu.Lock()
	old := env.m.accounts[id]
	replacement := &account{}
	copyPersistentFields(replacement, old)
	env.m.accounts[id] = replacement
	env.m.mu.Unlock()
	if env.m.CatalogIdentityCurrent(id, identity) {
		t.Fatal("同 ID 重建的对象不能沿用目录身份")
	}
	env.m.mu.Lock()
	replacement.NeedsReauth = true
	env.m.mu.Unlock()
	if _, err := env.m.CatalogIdentity(id); !errors.Is(err, ErrReauthorizationRequired) {
		t.Fatalf("需重新授权身份错误=%v", err)
	}
	if _, _, _, err := env.m.CatalogCredentials(context.Background(), id, nil); !errors.Is(err, ErrReauthorizationRequired) {
		t.Fatalf("需重新授权凭据错误=%v", err)
	}
}

func TestCatalogCredentialsCanceledAfterExchangeKeepsRotatedTokens(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "fake@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated-access", "rotated-refresh", "", 3600))
	})
	env.m.beforeReplace = func() error { cancel(); return nil }
	_, _, _, err := env.m.CatalogCredentials(ctx, id, env.up.server.Client())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("交换后取消应阻止目录请求：%v", err)
	}
	reopened := env.open()
	reopened.mu.RLock()
	got := reopened.accounts[id]
	ok := got.AccessToken == "rotated-access" && got.RefreshToken == "rotated-refresh"
	reopened.mu.RUnlock()
	if !ok {
		t.Fatal("目录取消不能丢弃已轮换且已保存的凭据")
	}
}
