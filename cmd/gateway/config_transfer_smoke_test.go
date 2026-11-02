// Package main 的隔离 smoke 覆盖真实 HTTP 路由、保存和热重载，不使用页面 API mock。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/config"
)

// 复跑命令：docker exec ai-gateway-dev-verify go test ./cmd/gateway -run TestConfigTransferSmokeLocalHTTP -count=1 -v
func TestConfigTransferSmokeLocalHTTP(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	srv := newConfigTestServer(t, testConfigYAML("127.0.0.1", 7789, upstream.URL, "fake-smoke-old-secret", 5))
	srv.revision = "smoke-original"
	gateway := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer gateway.Close()
	client := gateway.Client()

	call := func(path, method string, value any, revision string, withOrigin bool) (int, http.Header, []byte) {
		t.Helper()
		var data []byte
		if value != nil {
			var err error
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(context.Background(), method, gateway.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		if withOrigin {
			req.Header.Set("Origin", gateway.URL)
		}
		if revision != "" {
			req.Header.Set("If-Match", fmt.Sprintf("%q", revision))
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, body
	}

	status, _, body := call("/api/config", http.MethodGet, nil, "", false)
	if status != 200 || bytes.Contains(body, []byte("fake-smoke-old-secret")) {
		t.Fatal("普通查询脱敏失败")
	}
	status, headers, exported := call("/api/config/export", http.MethodPost, config.TransferExportRequest{Scope: "all"}, srv.revision, true)
	if status != 200 || headers.Get("Content-Disposition") == "" || !bytes.Contains(exported, []byte("fake-smoke-old-secret")) {
		t.Fatal("真实附件导出失败")
	}
	status, _, _ = call("/api/config/export", http.MethodPost, config.TransferExportRequest{Scope: "all"}, srv.revision, false)
	if status != 403 {
		t.Fatal("真实 HTTP 无 Origin 未拒绝")
	}

	content := strings.Replace(transferHTTPSource, "https://example.invalid/v1", upstream.URL, 1)
	before, err := os.ReadFile(srv.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	status, _, previewBody := call("/api/config/import/preview", http.MethodPost, configTransferImportRequest{Content: content}, srv.revision, true)
	if status != 200 || bytes.Contains(previewBody, []byte("fake-import-api-secret")) {
		t.Fatalf("真实预览失败：%d %s", status, previewBody)
	}
	var preview struct {
		Data struct {
			Token    string `json:"previewToken"`
			CanApply bool   `json:"canApply"`
		} `json:"data"`
	}
	if err := json.Unmarshal(previewBody, &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Data.CanApply || preview.Data.Token == "" {
		t.Fatal("真实预览未签发凭证")
	}
	afterPreview, err := os.ReadFile(srv.cfg.Path)
	if err != nil || !bytes.Equal(before, afterPreview) {
		t.Fatal("预览写入了配置")
	}
	oldRevision := srv.revision
	input := configTransferImportRequest{Content: content, PreviewToken: preview.Data.Token}
	status, _, body = call("/api/config/import/apply", http.MethodPost, input, oldRevision, true)
	if status != 200 || !bytes.Contains(body, []byte("success")) {
		t.Fatalf("真实应用失败：%d %s", status, body)
	}
	status, _, _ = call("/api/config/import/apply", http.MethodPost, input, oldRevision, true)
	if status != 409 {
		t.Fatal("真实 HTTP 旧版本重放未拒绝")
	}
	status, _, body = call("/api/config", http.MethodGet, nil, "", false)
	if status != 200 || !bytes.Contains(body, []byte("imported")) || bytes.Contains(body, []byte("fake-import-api-secret")) {
		t.Fatal("导入后查询未更新或泄密")
	}
	stored, err := os.ReadFile(srv.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DecodeAndValidate(stored)
	if err != nil || cfg.Providers["imported"] == nil || cfg.Providers["imported"].APIKey != "fake-import-api-secret" {
		t.Fatal("真实磁盘保存未成功")
	}
	if upstreamRequests.Load() != 0 {
		t.Fatalf("导入导出产生上游请求：%d", upstreamRequests.Load())
	}
	t.Log("真实 HTTP smoke 通过：脱敏查询、含凭据附件、Origin、只读预览、一次应用、旧版本拒绝、落盘及零上游请求")
}
