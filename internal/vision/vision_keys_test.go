package vision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/queue"
)

func TestDoRecognizeKeyListSwitchesAfter429AndCoolsPrimary(t *testing.T) {
	var received []string
	var receivedMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		receivedMu.Lock()
		received = append(received, key)
		receivedMu.Unlock()
		if key == "vision-primary-test-secret" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeVisionResponse(t, w, "备用密钥识别结果")
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:          "vision",
		BaseURL:       server.URL,
		Format:        "openai",
		MaxConcurrent: 1,
		MaxQueueWait:  1000,
		APIKeys: []config.ProviderKey{
			{ID: "primary", Value: "vision-primary-test-secret"},
			{ID: "backup", Value: "vision-backup-test-secret"},
		},
	}
	now := time.Now()
	pool := keypool.New(func() time.Time { return now })
	pool.Reconcile(&config.Config{Providers: map[string]*config.Provider{"vision": provider}})
	translator := New(nil, queue.NewManager(), func(string) (*http.Client, error) {
		return server.Client(), nil
	}, false)
	translator.SetKeyPool(pool)

	got, err := translator.doRecognize(context.Background(), imageBlock("retry-after-429"), provider, "vision-model")
	if err != nil {
		t.Fatalf("doRecognize() error = %v", err)
	}
	if got != "备用密钥识别结果" {
		t.Fatalf("识别结果 = %q，期望备用密钥结果", got)
	}
	receivedMu.Lock()
	gotKeys := append([]string(nil), received...)
	receivedMu.Unlock()
	if len(gotKeys) != 2 || gotKeys[0] != "vision-primary-test-secret" || gotKeys[1] != "vision-backup-test-secret" {
		t.Fatalf("上游收到的密钥顺序 = %#v", gotKeys)
	}
	foundPrimary := false
	for _, snapshot := range pool.Snapshot(&config.Config{Providers: map[string]*config.Provider{"vision": provider}})["vision"] {
		if snapshot.ID == "primary" && (snapshot.Reason != "rateLimit" || snapshot.RemainingMs != 60_000) {
			t.Fatalf("主密钥冷却 = %#v，期望遵守 Retry-After: 60", snapshot)
		}
		if snapshot.ID == "primary" {
			foundPrimary = true
		}
	}
	if !foundPrimary {
		t.Fatal("冷却快照中没有主密钥")
	}
	if ticket, _ := pool.Select("vision", provider, map[string]struct{}{"backup": {}}); ticket != nil {
		t.Fatalf("主密钥 429 后仍可选择：%q", ticket.Key.ID)
	}
}

func TestDoRecognizeKeyListAllCoolingFailsWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:          "vision",
		BaseURL:       server.URL,
		Format:        "openai",
		MaxConcurrent: 1,
		MaxQueueWait:  1000,
		APIKeys: []config.ProviderKey{
			{ID: "primary", Value: "vision-primary-test-secret"},
			{ID: "backup", Value: "vision-backup-test-secret"},
		},
	}
	pool := keypool.New(nil)
	pool.Reconcile(&config.Config{Providers: map[string]*config.Provider{"vision": provider}})
	excluded := make(map[string]struct{})
	for range provider.APIKeys {
		ticket, _ := pool.Select("vision", provider, excluded)
		if ticket == nil {
			t.Fatal("预期取得用于设置冷却的密钥 ticket")
		}
		pool.Report(ticket, http.StatusTooManyRequests, time.Minute)
		excluded[ticket.Key.ID] = struct{}{}
	}
	translator := New(nil, queue.NewManager(), func(string) (*http.Client, error) {
		return server.Client(), nil
	}, false)
	translator.SetKeyPool(pool)

	_, err := translator.doRecognize(context.Background(), imageBlock("all-cooling"), provider, "vision-model")
	if err == nil || !strings.Contains(err.Error(), "没有可选择的 Provider 密钥") {
		t.Fatalf("doRecognize() error = %v，期望提示没有可选择的密钥", err)
	}
	if got := ClassifyFailure(err); got != "other" {
		t.Fatalf("ClassifyFailure() = %q，期望 other", got)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("所有密钥冷却时仍发送了 %d 个请求", got)
	}
}
