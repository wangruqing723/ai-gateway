package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
)

func decodeBackgroundStatus(t *testing.T, srv *server) chatgptauth.BackgroundStatus {
	t.Helper()
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/background", "")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /api/chatgpt/background = %d %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tokenRefresh", "usageRefresh", "running", "crossProcessLocking", "reason"} {
		if _, ok := body.Data[key]; !ok {
			t.Fatalf("后台状态缺少字段 %s: %s", key, recorder.Body.String())
		}
	}
	var status struct {
		Data chatgptauth.BackgroundStatus `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status.Data
}

func backgroundConfig(t *testing.T, first, block string) *config.Config {
	t.Helper()
	raw := chatGPTGatewayYAML(first, "", 5)
	if block != "" {
		raw = strings.Replace(raw, "routes:", block+"routes:", 1)
	}
	cfg, err := config.DecodeAndValidate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestChatGPTBackgroundEndpointAndHotReload(t *testing.T) {
	srv, first, _ := newChatGPTGatewayServer(t, "")
	t.Cleanup(srv.stopChatGPTBackground)
	if recorder := chatGPTAPI(t, srv, http.MethodPost, "/api/chatgpt/background", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应返回 405: %d", recorder.Code)
	}
	if status := decodeBackgroundStatus(t, srv); status.Running || status.TokenRefresh || status.UsageRefresh {
		t.Fatalf("默认关闭: %#v", status)
	}
	accountsHaveBackground := func() bool {
		recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "")
		return strings.Contains(recorder.Body.String(), `"background"`)
	}
	if accountsHaveBackground() {
		t.Fatal("后台关闭时账户视图不应带 background")
	}

	srv.applyRuntimeConfig(backgroundConfig(t, first, "chatgptBackground:\n  tokenRefresh: true\n  activeWindowMinutes: 5\n"), "rev-2")
	status := decodeBackgroundStatus(t, srv)
	if !status.CrossProcessLocking {
		t.Skip("当前平台没有跨进程文件锁")
	}
	if !status.Running || !status.TokenRefresh || status.UsageRefresh || status.Reason != "" {
		t.Fatalf("热重载开启后台: %#v", status)
	}
	if !accountsHaveBackground() {
		t.Fatal("后台开启时账户视图应带 background")
	}
	recorder := chatGPTAPI(t, srv, http.MethodGet, "/api/chatgpt/accounts", "")
	if !strings.Contains(recorder.Body.String(), `"skipped":"inactive"`) || strings.Contains(recorder.Body.String(), `"usage":{"lastRunAt"`) {
		t.Fatalf("未使用的账户应标记 inactive，且用量功能关闭时省略 usage: %s", recorder.Body.String())
	}

	srv.applyRuntimeConfig(backgroundConfig(t, first, ""), "rev-3")
	if status := decodeBackgroundStatus(t, srv); status.Running || status.TokenRefresh {
		t.Fatalf("热重载关闭后台: %#v", status)
	}
	if accountsHaveBackground() {
		t.Fatal("后台关闭后账户视图不应再带 background")
	}
}
