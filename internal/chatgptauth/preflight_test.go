package chatgptauth

import (
	"testing"
	"time"
)

func TestAccountPreflightInvocationSourceAndIsolation(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "a@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	ticket := env.m.BeginInvocationWith(id, "", "model", InvocationSource{Diagnostic: true, AccountPreflight: true})
	env.m.FinishInvocation(ticket, InvocationResult{Success: true, HTTPStatus: 200})
	view, _ := env.m.Account(id)
	if view.Invocation.Mode != "account" || view.Invocation.Provider != "" || view.Invocation.State != "success" {
		t.Fatalf("来源=%+v", view.Invocation)
	}
	provider, _ := env.m.ProviderInvocation(id, "")
	if provider.State != "unknown" {
		t.Fatal("独立预检记录进入了 Provider 索引")
	}
	env.m.mu.Lock()
	env.m.resetInvocationsLocked(env.m.accounts[id])
	env.m.mu.Unlock()
	env.m.FinishInvocation(ticket, InvocationResult{Success: true, HTTPStatus: 200})
	view, _ = env.m.Account(id)
	if view.Invocation.State != "unknown" {
		t.Fatal("旧票据覆盖新凭据")
	}
}

func TestDiagnosticIdentityAllowsRotationRejectsReplacement(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "a@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	identity := env.m.DiagnosticIdentity(id)
	ticket := env.m.BeginInvocationWith(id, "", "model", InvocationSource{Diagnostic: true, AccountPreflight: true})
	env.m.mu.Lock()
	env.m.accounts[id].credentialVersion++
	env.m.mu.Unlock()
	if !env.m.DiagnosticIdentityCurrent(id, identity) || !env.m.InvocationCurrent(id, ticket) {
		t.Fatal("正常轮换误作外部凭据替换")
	}
	env.m.mu.Lock()
	env.m.resetInvocationsLocked(env.m.accounts[id])
	env.m.mu.Unlock()
	if env.m.DiagnosticIdentityCurrent(id, identity) || env.m.InvocationCurrent(id, ticket) {
		t.Fatal("外部替换未隔离诊断")
	}
	if err := env.m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if env.m.InvocationCurrent(id, ticket) {
		t.Fatal("删除后仍可用旧ticket")
	}
}
