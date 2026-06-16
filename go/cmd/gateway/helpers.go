package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"ai-gateway/internal/config"
)

// 北京时间日志，对齐 Node 版格式：[时间] [reqId] msg
func beijingTime() string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc).Format("2006-01-02 15:04:05.000")
}

func logf(reqID, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] [%s] %s\n", beijingTime(), reqID, fmt.Sprintf(format, args...))
}

func logSystem(format string, args ...any) {
	logf("system", format, args...)
}

// readBody 读取请求体
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// writeJSONError 统一 JSON 错误响应
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errType, "message": msg},
	})
	w.Write(payload)
}

// mask 脱敏密钥显示
func mask(v string) string {
	if v == "" {
		return "(未配置)"
	}
	if len(v) <= 8 {
		return "****"
	}
	return v[:4] + "****" + v[len(v)-4:]
}

// printBanner 启动横幅，对齐 Node 版样式
func printBanner(cfg *config.Config) {
	logSystem("═══════════════════════════════════════════")
	logSystem("  ai-gateway (Go PoC) 启动成功")
	logSystem("───────────────────────────────────────────")
	logSystem("  监听地址 : http://%s:%d", cfg.Host, cfg.Port)
	logSystem("  配置文件 : %s", cfg.Path)
	logSystem("  Providers:")
	for name, p := range cfg.Providers {
		logSystem("    %s: %s [%s] key=%s 并发=%d", name, p.BaseURL, p.Format, mask(p.APIKey), p.MaxConcurrent)
	}
	logSystem("  Routes  : %d 条", len(cfg.Routes))
	for _, r := range cfg.Routes {
		vis := ""
		if r.Vision != nil {
			vis = fmt.Sprintf(" + vision(%s)", r.Vision.Model)
		}
		logSystem("    %-30s → %s/%s%s", r.Match, r.Provider, r.Model, vis)
	}
	logSystem("───────────────────────────────────────────")
	logSystem("  请求超时 : %d 秒", cfg.Timeout/1000)
	logSystem("  流式活跃超时 : %d 秒", cfg.StreamActivityTimeout/1000)
	logSystem("───────────────────────────────────────────")
	logSystem("  接受格式: Anthropic · OpenAI Chat · OpenAI Responses")
	logSystem("  健康检查: GET /health")
	logSystem("═══════════════════════════════════════════")
}
