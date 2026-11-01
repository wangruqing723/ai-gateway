package keypool

import (
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/config"
)

func TestFingerprintMatchesLegacyAlgorithm(t *testing.T) {
	if got := Fingerprint("fake-key-value"); got != "DEF7A3A7" {
		t.Fatalf("Fingerprint() = %q", got)
	}
	if got := Fingerprint(""); got != "" {
		t.Fatalf("空密钥指纹 = %q", got)
	}
}

func TestLateSuccessDoesNotClearCooldownOrRecover(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var events []Event
	pool := New(clock)
	pool.SetEventHandler(func(event Event) { events = append(events, event) })
	cfg := keyConfig("fake-primary")
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]

	older := selectKey(t, pool, provider)
	later := selectKey(t, pool, provider)
	pool.Report(later, 401, 0)
	pool.Report(older, 200, 0)

	snapshot := pool.Snapshot(cfg)["p"][0]
	if snapshot.State != StateCooling || snapshot.RemainingMs != 300_000 {
		t.Fatalf("冷却前发出的迟到成功改变了冷却: %+v", snapshot)
	}
	if len(events) != 1 || events[0].Kind != "key_cooldown" {
		t.Fatalf("迟到成功不应记恢复事件: %+v", events)
	}

	if !pool.Release("p", "k1") {
		t.Fatal("应能手动解除冷却")
	}
	newer := selectKey(t, pool, provider)
	pool.Report(newer, 401, 0)
	if len(events) != 1 {
		t.Fatalf("同类别异常恢复前应去重: %+v", events)
	}
	if !pool.Release("p", "k1") {
		t.Fatal("第二次应能手动解除冷却")
	}
	newest := selectKey(t, pool, provider)
	pool.Report(newest, 200, 0)
	if len(events) != 2 || events[1].Kind != "key_recovered" {
		t.Fatalf("更新序号的成功应记恢复事件: %+v", events)
	}
}

func TestZeroAuthCooldownDoesNotCreateRecoverySequence(t *testing.T) {
	pool := New(nil)
	var events []Event
	pool.SetEventHandler(func(event Event) { events = append(events, event) })
	cfg := keyConfig("fake-key")
	cfg.Providers["p"].KeyCooldown = &config.KeyCooldown{AuthMs: intPointer(0)}
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]

	failed := selectKey(t, pool, provider)
	pool.Report(failed, 401, 0)
	pool.Report(selectKey(t, pool, provider), 200, 0)

	if got := pool.Snapshot(cfg)["p"][0].State; got != StateSelectable {
		t.Fatalf("零时长 auth 不应进入冷却: state=%s", got)
	}
	if len(events) != 0 {
		t.Fatalf("未进入冷却的异常不应产生冷却或恢复事件: %+v", events)
	}
	pool.mu.Lock()
	seq := pool.states[keyID{provider: "p", id: "k1"}].abnormalSeq
	pool.mu.Unlock()
	if seq != 0 {
		t.Fatalf("未进入冷却的异常仍更新 abnormalSeq: %d", seq)
	}
}

func TestReconcileDiscardsOldCredentialTicket(t *testing.T) {
	pool := New(nil)
	oldConfig := keyConfig("fake-old")
	pool.Reconcile(oldConfig)
	ticket := selectKey(t, pool, oldConfig.Providers["p"])

	newConfig := keyConfig("fake-new")
	pool.Reconcile(newConfig)
	pool.Report(ticket, 401, 0)

	if got := pool.Snapshot(newConfig)["p"][0].State; got != StateSelectable {
		t.Fatalf("旧凭据 ticket 污染新状态: state=%s", got)
	}
}

func TestReconcileKeepsOrClearsStateByConnectionIdentity(t *testing.T) {
	now := time.Now()
	pool := New(func() time.Time { return now })
	cfg := keyConfig("fake-first")
	pool.Reconcile(cfg)
	pool.Report(selectKey(t, pool, cfg.Providers["p"]), 401, 0)
	initial := pool.Snapshot(cfg)["p"][0]

	// 改名、启用状态、顺序与冷却配置变化都不改变连接身份。
	changedPresentation := keyConfig("fake-first")
	changedPresentation.Providers["p"].APIKeys[0].Name = "备用"
	changedPresentation.Providers["p"].APIKeys[0].Enabled = boolPointer(false)
	changedPresentation.Providers["p"].KeyCooldown = &config.KeyCooldown{AuthMs: intPointer(0)}
	changedPresentation.Providers["p"].APIKeys = append(changedPresentation.Providers["p"].APIKeys, config.ProviderKey{ID: "k2", Value: "fake-second"})
	changedPresentation.Providers["p"].APIKeys[0], changedPresentation.Providers["p"].APIKeys[1] = changedPresentation.Providers["p"].APIKeys[1], changedPresentation.Providers["p"].APIKeys[0]
	pool.Reconcile(changedPresentation)
	kept := pool.Snapshot(changedPresentation)["p"]
	if len(kept) != 2 || kept[1].State != StateDisabled || kept[1].RemainingMs != initial.RemainingMs {
		t.Fatalf("展示或启用配置变化丢失冷却: initial=%+v current=%+v", initial, kept)
	}

	changedCredential := keyConfig("fake-replaced")
	pool.Reconcile(changedCredential)
	if got := pool.Snapshot(changedCredential)["p"][0].State; got != StateSelectable {
		t.Fatalf("凭据替换后状态未清除: %s", got)
	}
	pool.Report(selectKey(t, pool, changedCredential.Providers["p"]), 401, 0)
	changedConnection := keyConfig("fake-replaced")
	changedConnection.Providers["p"].BaseURL = "https://other.example.com"
	pool.Reconcile(changedConnection)
	if got := pool.Snapshot(changedConnection)["p"][0].State; got != StateSelectable {
		t.Fatalf("连接身份变化后状态未清除: %s", got)
	}
}

func TestRetryAfterAndCooldownConfiguration(t *testing.T) {
	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	var now = base
	pool := New(func() time.Time { return now })

	cfg := keyConfig("fake-key")
	cfg.Providers["p"].KeyCooldown = &config.KeyCooldown{QuotaMs: intPointer(0), RateLimitMs: intPointer(0)}
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]
	pool.Report(selectKey(t, pool, provider), 429, 45*time.Second)
	if got := pool.Snapshot(cfg)["p"][0].RemainingMs; got != 45_000 {
		t.Fatalf("429 Retry-After 未覆盖零配置: %dms", got)
	}

	// 冷却配置不在 connFP 中，热重载时保留已有截止时间。
	now = now.Add(2 * time.Second)
	changedDuration := keyConfig("fake-key")
	changedDuration.Providers["p"].KeyCooldown = &config.KeyCooldown{RateLimitMs: intPointer(1)}
	pool.Reconcile(changedDuration)
	if got := pool.Snapshot(changedDuration)["p"][0].RemainingMs; got != 43_000 {
		t.Fatalf("冷却时长变化回改已有截止: %dms", got)
	}

	quota := keyConfig("fake-quota")
	quota.Providers["p"].KeyCooldown = &config.KeyCooldown{QuotaMs: intPointer(0)}
	pool.Reconcile(quota)
	pool.Report(selectKey(t, pool, quota.Providers["p"]), 402, 70*time.Second)
	if got := pool.Snapshot(quota)["p"][0].RemainingMs; got != 70_000 {
		t.Fatalf("402 Retry-After 未按上游值冷却: %dms", got)
	}
}

func TestCooldownDefaultsAndEventsAreDeduplicatedByClass(t *testing.T) {
	now := time.Now()
	pool := New(func() time.Time { return now })
	var mu sync.Mutex
	var events []Event
	pool.SetEventHandler(func(event Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	cfg := keyConfig("fake-key")
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]
	pool.Report(selectKey(t, pool, provider), 401, time.Hour)
	if got := pool.Snapshot(cfg)["p"][0].RemainingMs; got != 300_000 {
		t.Fatalf("auth 不应采用 Retry-After: %dms", got)
	}
	if !pool.Release("p", "k1") {
		t.Fatal("解除冷却失败")
	}
	pool.Report(selectKey(t, pool, provider), 403, 0)
	if !pool.Release("p", "k1") {
		t.Fatal("解除第二次冷却失败")
	}
	pool.Report(selectKey(t, pool, provider), 402, 0)
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Kind != "key_cooldown" || events[1].Kind != "key_cooldown" {
		t.Fatalf("同类事件应去重、类别变化可再记: %+v", events)
	}
	if events[0].Provider != "p" || events[0].Detail == "" {
		t.Fatalf("事件缺少 Provider 或密钥详情: %+v", events[0])
	}
}

func TestSelectUsesConfigurationOrderAndEarliestCoolingTime(t *testing.T) {
	now := time.Now()
	pool := New(func() time.Time { return now })
	cfg := keyConfig("fake-first")
	cfg.Providers["p"].APIKeys = append(cfg.Providers["p"].APIKeys, config.ProviderKey{ID: "k2", Value: "fake-second"})
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]
	first := selectKey(t, pool, provider)
	if first.Key.ID != "k1" {
		t.Fatalf("首选密钥 = %q", first.Key.ID)
	}
	pool.Report(first, 401, 0)
	second := selectKey(t, pool, provider)
	if second.Key.ID != "k2" {
		t.Fatalf("冷却期间应选下一份密钥，得到 %q", second.Key.ID)
	}
	pool.Report(second, 429, 10*time.Second)
	if got := pool.Snapshot(cfg)["p"][0].State; got != StateCooling {
		t.Fatalf("401 密钥状态 = %s", got)
	}
	// 单独排除第二份密钥后，最早恢复时间应来自它的 10 秒 Retry-After。
	if _, earliest := pool.Select("p", provider, nil); !earliest.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("最早恢复时间 = %v", earliest)
	}
}

func TestConcurrentSelectReportAndSnapshot(t *testing.T) {
	pool := New(nil)
	cfg := keyConfig("fake-key")
	pool.Reconcile(cfg)
	provider := cfg.Providers["p"]
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ticket, _ := pool.Select("p", provider, nil)
				if ticket != nil {
					status := 200
					if (i+j)%7 == 0 {
						status = 429
					}
					pool.Report(ticket, status, time.Millisecond)
				}
				_ = pool.Snapshot(cfg)
			}
		}(i)
	}
	wg.Wait()
}

func selectKey(t *testing.T, pool *Pool, provider *config.Provider) *Ticket {
	t.Helper()
	ticket, _ := pool.Select("p", provider, nil)
	if ticket == nil {
		t.Fatal("没有选出可用密钥")
	}
	return ticket
}

func keyConfig(value string) *config.Config {
	return &config.Config{Providers: map[string]*config.Provider{
		"p": {
			Name:    "p",
			BaseURL: "https://api.example.com",
			Format:  "openai",
			APIKeys: []config.ProviderKey{{ID: "k1", Name: "主密钥", Value: value}},
		},
	}}
}

func boolPointer(value bool) *bool { return &value }
func intPointer(value int) *int    { return &value }
