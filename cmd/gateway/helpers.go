package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/httputil"
)

// 包级缓存时区，避免每条日志重复加载
var beijingLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// 北京时间日志，对齐 Node 版格式：[时间] [reqId] msg
func beijingTime() string {
	return time.Now().In(beijingLoc).Format("2006-01-02 15:04:05.000")
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

// writeJSONError 统一 JSON 错误响应（委托 httputil 共享实现）
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	httputil.WriteJSONError(w, status, errType, msg)
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
func printBanner(cfg *config.Config, c *cache.Cache) {
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
	cs := c.GetStats()
	if cs.Total > 0 {
		logSystem("  图片缓存 : %d 条 (%s)", cs.Total, formatSize(cs.ContentSize))
	} else {
		logSystem("  图片缓存 : 无")
	}
	logSystem("  请求超时 : %d 秒", cfg.Timeout/1000)
	logSystem("  流式活跃超时 : %d 秒", cfg.StreamActivityTimeout/1000)
	// 直通模式状态：开启时跳过队列，关闭时走队列（并发/限速/排队）
	if cfg.DirectMode {
		logSystem("  直通模式 : 开启（跳过队列，限流交由上游 429）")
		logSystem("    直通超时 : 非流式 %d秒 / 流式头 %d秒 / 流式活跃 %d秒", cfg.DirectTimeoutNoStream/1000, cfg.DirectTimeoutStreamHeader/1000, cfg.DirectTimeoutStreamActive/1000)
	} else {
		logSystem("  直通模式 : 关闭（请求经队列：并发/限速/排队）")
	}
	logSystem("───────────────────────────────────────────")
	logSystem("  接受格式: Anthropic · OpenAI Chat · OpenAI Responses")
	logSystem("  健康检查: GET /health")
	logSystem("═══════════════════════════════════════════")
}

// formatSize 人类可读体积
func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}
