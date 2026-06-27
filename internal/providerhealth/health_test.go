package providerhealth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/config"
)

func TestCheckAllMapsStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]*config.Provider{
		"ok":  {Name: "ok", BaseURL: server.URL + "/v1", APIKey: "good", Format: "openai"},
		"bad": {Name: "bad", BaseURL: server.URL, APIKey: "bad", Format: "openai"},
	}}

	statuses := NewChecker().CheckAll(context.Background(), cfg, server.Client())
	if statuses["ok"].Status != "ok" {
		t.Fatalf("expected ok status, got %#v", statuses["ok"])
	}
	if statuses["bad"].Status != "error" || statuses["bad"].HTTPCode != http.StatusUnauthorized {
		t.Fatalf("expected auth error, got %#v", statuses["bad"])
	}
}

func TestSnapshotIncludesUncheckedProviders(t *testing.T) {
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"pending": {Name: "pending", BaseURL: "example.com", Format: "openai"},
	}}

	statuses := NewChecker().Snapshot(cfg)
	if statuses["pending"].Status != "unchecked" {
		t.Fatalf("expected unchecked status, got %#v", statuses["pending"])
	}
}
