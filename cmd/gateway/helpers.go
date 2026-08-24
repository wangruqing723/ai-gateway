package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/httputil"
)

const (
	maxConfigBodyBytes = 1 << 20
	maxProxyBodyBytes  = 32 << 20
)

var errRequestBodyTooLarge = errors.New("请求体超过大小限制")

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

// readBodyLimited 读取有明确上限的请求体。
func readBodyLimited(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	if r.ContentLength > limit {
		return nil, errRequestBodyTooLarge
	}
	reader := &io.LimitedReader{R: r.Body, N: limit + 1}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errRequestBodyTooLarge
	}
	return data, nil
}

func requestMediaType(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Content-Type"))
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func isAllowedConfigMediaType(value string) bool {
	switch value {
	case "application/json", "application/yaml", "application/x-yaml", "text/yaml":
		return true
	default:
		return false
	}
}

func isCrossSiteRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !strings.EqualFold(parsed.Scheme, scheme) || !strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	return !isLoopbackHostname(parsed.Hostname())
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
}

func writeMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP method")
}

func writeForbiddenOrigin(w http.ResponseWriter) {
	writeJSONError(w, http.StatusForbidden, "forbidden_origin", "拒绝跨站请求")
}

func staticContentType(path string) string {
	if value := mime.TypeByExtension(filepath.Ext(path)); value != "" {
		return value
	}
	return "application/octet-stream"
}

// writeJSONError 统一 JSON 错误响应（委托 httputil 共享实现）
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	httputil.WriteJSONError(w, status, errType, msg)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能把 deadline/flush 传给真实连接 writer。
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *statusRecorder) Bytes() int64 {
	return r.bytes
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
		// 走 TargetList()：多候选写法下 r.Provider / r.Model 恒为空，直接打会输出「→ /」，
		// 而这里是运维在启动时唯一能看到解析后路由表的地方。
		targets := r.TargetList()
		parts := make([]string, 0, len(targets))
		for _, t := range targets {
			parts = append(parts, t.Provider+"/"+t.Model)
		}
		strategy := ""
		if len(targets) > 1 {
			s := r.Strategy
			if s == "" {
				s = "failover"
			}
			strategy = fmt.Sprintf(" [%s]", s)
		}
		logSystem("    %-30s → %s%s%s", r.Match, strings.Join(parts, ", "), strategy, vis)
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

func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("x-forwarded-for")); xff != "" {
		if idx := strings.Index(xff, ","); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return xff
	}
	if xr := strings.TrimSpace(r.Header.Get("x-real-ip")); xr != "" {
		return xr
	}
	if ra := strings.TrimSpace(r.RemoteAddr); ra != "" {
		if host, _, err := net.SplitHostPort(ra); err == nil {
			return host
		}
		return ra
	}
	return "unknown"
}

func keyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha1.Sum([]byte(apiKey))
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	if len(encoded) >= 8 {
		return encoded[:8]
	}
	return encoded
}
