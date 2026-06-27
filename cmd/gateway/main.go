// Command ai-gateway 是当前主版本的 Go 网关实现。
//
// 已实现：配置加载、路由匹配、per-provider 并发/限速队列、流式/非流式转发、context 超时、
// 三格式互转 converter、vision 图片识别、SQLite 缓存、健康检查。
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // 嵌入时区数据库，使 distroless 等无 tzdata 的镜像也能解析 Asia/Shanghai

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/providerhealth"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/router"
	"ai-gateway/internal/vision"
)

//go:embed web/index.html
var webFS embed.FS

var reqCounter uint64

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] %s\n", err)
		os.Exit(1)
	}

	qm := queue.NewManager()

	// 复用连接池的 HTTP 客户端；不设 http.Client.Timeout（非流式靠 ctx 超时，流式靠 header 超时 + 活跃超时）
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// 初始化 SQLite 图片缓存
	imgCache, err := cache.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] 缓存初始化失败: %s\n", err)
		os.Exit(1)
	}
	// 启动时清理过期缓存
	maxAgeDays := cfg.Cache.MaxAgeDays
	if maxAgeDays == 0 {
		maxAgeDays = 7
	}
	maxRecords := cfg.Cache.MaxRecords
	if maxRecords == 0 {
		maxRecords = 1000
	}
	if res, err := imgCache.Cleanup(maxAgeDays, maxRecords); err == nil && res.Deleted > 0 {
		fmt.Fprintf(os.Stderr, "[ai-gateway] 缓存清理: 删除 %d 条过期记录\n", res.Deleted)
	}

	translator := vision.New(imgCache, qm, httpClient, cfg.DirectMode)

	srv := &server{cfg: cfg, qm: qm, httpClient: httpClient, cache: imgCache, translator: translator, metrics: metrics.NewCollector(1000), providerHealth: providerhealth.NewChecker(), webDevDir: os.Getenv("AI_GATEWAY_WEB_DIR")}

	// 优雅退出：关闭缓存
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logSystem("收到退出信号，正在关闭...")
		imgCache.Close()
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handle)

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	printBanner(cfg, imgCache)

	httpServer := &http.Server{Addr: addr, Handler: mux}
	if err := httpServer.ListenAndServe(); err != nil {
		logSystem("服务器错误: %s", err)
		imgCache.Close()
		os.Exit(1)
	}
}

type server struct {
	cfg            *config.Config
	cfgMu          sync.RWMutex // 保护 cfg 的并发读写
	qm             *queue.Manager
	httpClient     *http.Client
	cache          *cache.Cache
	translator     *vision.Translator
	metrics        *metrics.Collector
	providerHealth *providerhealth.Checker
	webDevDir      string
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.Split(r.URL.Path, "?")[0]

	// HEAD 健康探测
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 前端页面：GET / 返回 index.html
	if urlPath == "/" && r.Method == http.MethodGet {
		s.handleIndex(w, r)
		return
	}

	// 开发模式前端热加载：仅 AI_GATEWAY_WEB_DIR 启用时可用
	if urlPath == "/__dev/reload" && r.Method == http.MethodGet && s.webDevDir != "" {
		s.handleDevReload(w, r)
		return
	}

	// 配置管理 API：/api/config/*
	if strings.HasPrefix(urlPath, "/api/config") {
		s.handleConfigAPI(w, r)
		return
	}

	if urlPath == "/api/metrics" && r.Method == http.MethodGet {
		s.handleMetrics(w, r)
		return
	}

	if urlPath == "/api/logs" && r.Method == http.MethodGet {
		s.handleLogs(w, r)
		return
	}

	if urlPath == "/api/providers/health" && r.Method == http.MethodPost {
		s.handleProviderHealthCheck(w, r)
		return
	}

	// /health 端点
	if urlPath == "/health" && r.Method == http.MethodGet {
		s.handleHealth(w)
		return
	}

	// /v1/models 端点：返回已配置的可用模型列表
	if urlPath == "/v1/models" && r.Method == http.MethodGet {
		s.handleModels(w)
		return
	}

	// 获取配置快照（避免长时间持有读锁）
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	clientFormat := converter.DetectClientFormat(urlPath)
	if clientFormat == "" {
		writeJSONError(w, http.StatusNotFound, "gateway_error", "未知端点: "+urlPath)
		return
	}

	reqID := nextReqID()
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	reqLog := metrics.RequestLog{
		ID:           reqID,
		Started:      start,
		Time:         start.In(beijingLoc).Format("2006-01-02 15:04:05"),
		Method:       r.Method,
		Path:         urlPath,
		ClientFormat: clientFormat,
		Format:       clientFormat,
	}
	defer func() {
		reqLog.Status = rec.Status()
		reqLog.ResponseBytes = rec.Bytes()
		reqLog.DurationMs = time.Since(start).Milliseconds()
		if reqLog.Error == "" && reqLog.Status >= 400 {
			reqLog.Error = http.StatusText(reqLog.Status)
		}
		s.metrics.Add(reqLog)
	}()

	logf(reqID, "→ %s %s [%s]", r.Method, urlPath, clientFormat)

	raw, err := readBody(r)
	if err != nil {
		reqLog.Error = "读取请求失败: " + err.Error()
		writeJSONError(w, http.StatusBadRequest, "gateway_error", reqLog.Error)
		return
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		reqLog.Error = "请求体 JSON 解析失败: " + err.Error()
		writeJSONError(w, http.StatusBadRequest, "gateway_error", reqLog.Error)
		return
	}

	model, _ := body["model"].(string)
	reqLog.Model = model

	// 规范化为内部格式
	var internal *converter.Internal
	switch clientFormat {
	case "anthropic":
		internal = converter.FromAnthropic(body)
	case "openai-chat":
		internal = converter.FromOpenAIChat(body)
	default:
		internal = converter.FromOpenAIResponses(body)
	}
	model = internal.Model
	reqLog.Model = model
	reqLog.Stream = internal.Stream

	// 路由匹配
	matched := router.MatchRoute(model, cfg)
	if matched == nil {
		reqLog.Error = fmt.Sprintf("没有匹配 model %q 的路由规则", model)
		writeJSONError(w, http.StatusBadRequest, "gateway_error", reqLog.Error)
		return
	}
	reqLog.Route = matched.RouteMatch
	reqLog.Provider = matched.Provider.Name
	reqLog.TargetModel = matched.TargetModel
	p := matched.Provider
	p.APIKey = router.ResolveAPIKey(p, r.Header)

	// 是否需要视觉识别：路由配了 vision 且消息含图片
	needVision := matched.VisionProvider != nil && vision.HasImages(internal.Messages)
	reqLog.Vision = needVision
	displayModel := matched.TargetModel
	if needVision {
		displayModel = matched.VisionModel
	}
	logf(reqID, "→ %s → %s [%d msgs, stream=%v]", model, displayModel, len(internal.Messages), internal.Stream)

	// 图片翻译：把图片块替换为视觉模型生成的文字描述
	if needVision {
		vp := matched.VisionProvider
		vp.APIKey = router.ResolveAPIKey(vp, r.Header)
		internal.Messages = s.translator.Translate(r.Context(), internal.Messages, vp, matched.VisionModel,
			func(f string, a ...any) { logf(reqID, f, a...) })
	}

	// 内部格式 → 上游 provider 请求体
	var upstreamMap map[string]any
	if p.Format == "anthropic" {
		upstreamMap = converter.ToAnthropicBody(internal, matched.TargetModel)
	} else {
		upstreamMap = converter.ToOpenAIChatBody(internal, matched.TargetModel)
	}
	upstreamBody, err := json.Marshal(upstreamMap)
	if err != nil {
		reqLog.Error = "上游请求体序列化失败: " + err.Error()
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", reqLog.Error)
		return
	}

	// 直通模式：跳过队列（并发/限速/排队），请求直接转发；超时用 direct* 配置。
	// 非直通模式：先经队列获取执行槽位（并发 + 限速 + 等待超时）。
	if !cfg.DirectMode {
		release, waitMs, err := s.qm.Acquire(r.Context(), p.Name, p.MaxConcurrent, p.MaxPerSecond, p.MaxQueueWait)
		reqLog.QueueWaitMs = waitMs
		if err != nil {
			reqLog.Error = err.Error()
			logf(reqID, "  队列处理异常: %s", err.Error())
			if err == queue.ErrQueueTimeout {
				writeJSONError(w, http.StatusServiceUnavailable, "queue_timeout", err.Error())
			} else if r.Context().Err() != nil {
				// 客户端已断开，net/http 会忽略写入，无需也不应写响应
				logf(reqID, "  客户端已断开，跳过响应")
			} else {
				writeJSONError(w, http.StatusBadGateway, "gateway_error", "队列错误: "+err.Error())
			}
			return
		}
		defer release()
	}

	// 超时参数：直通模式用 direct* 三档；否则沿用全局 timeout / streamActivityTimeout
	timeoutMs := cfg.Timeout
	headerTimeoutMs := 0 // 0 表示 proxy 回退用 timeoutMs
	activityTimeoutMs := cfg.StreamActivityTimeout
	if cfg.DirectMode {
		timeoutMs = cfg.DirectTimeoutNoStream
		headerTimeoutMs = cfg.DirectTimeoutStreamHeader
		activityTimeoutMs = cfg.DirectTimeoutStreamActive
	}

	opts := &proxy.Options{
		ClientReq:             r,
		ClientRes:             w,
		UpstreamBody:          upstreamBody,
		Provider:              p,
		ClientFormat:          clientFormat,
		OriginalModel:         model,
		IsStreaming:           internal.Stream,
		Log:                   func(f string, a ...any) { logf(reqID, f, a...) },
		StartTime:             start,
		TimeoutMs:             timeoutMs,
		HeaderTimeoutMs:       headerTimeoutMs,
		StreamActivityTimeout: activityTimeoutMs,
		HTTPClient:            s.httpClient,
	}
	if err := proxy.Forward(opts); err != nil {
		reqLog.Error = err.Error()
		// 区分客户端断开、超时、其他错误，避免把正常断开误报为异常
		if errors.Is(err, context.Canceled) {
			logf(reqID, "  客户端断开连接")
		} else if errors.Is(err, context.DeadlineExceeded) {
			logf(reqID, "  转发超时（%d秒）: %s", timeoutMs/1000, err.Error())
		} else {
			logf(reqID, "  转发结束（异常）: %s", err.Error())
		}
	}
}

func (s *server) handleHealth(w http.ResponseWriter) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	queues := make(map[string]queue.Status)
	for name, p := range cfg.Providers {
		queues[name] = s.qm.StatusOf(name, p.MaxConcurrent, p.MaxPerSecond)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	cs := s.cache.GetStats()
	health := map[string]any{
		"status":         "ok",
		"timeout":        cfg.Timeout,
		"queues":         queues,
		"providerHealth": s.providerHealth.Snapshot(cfg),
		"cache": map[string]any{
			"total":       cs.Total,
			"contentSize": cs.ContentSize,
		},
		"memory": map[string]any{
			"heapAllocMB": m.HeapAlloc / 1024 / 1024,
			"sysMB":       m.Sys / 1024 / 1024,
		},
	}
	out, _ := json.MarshalIndent(health, "", "  ")
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func (s *server) handleProviderHealthCheck(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	statuses := s.providerHealth.CheckAll(r.Context(), cfg, s.httpClient)
	out, _ := json.MarshalIndent(map[string]any{
		"providers": statuses,
	}, "", "  ")
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	out, _ := json.MarshalIndent(s.metrics.Metrics(time.Now()), "", "  ")
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := metrics.LogFilter{
		Provider: q.Get("provider"),
		Model:    q.Get("model"),
		Status:   q.Get("status"),
		Stream:   q.Get("stream"),
		Query:    q.Get("q"),
	}
	if limit, err := strconv.Atoi(q.Get("limit")); err == nil {
		filter.Limit = limit
	}
	if offset, err := strconv.Atoi(q.Get("offset")); err == nil {
		filter.Offset = offset
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}

	logs := s.metrics.Logs(filter)
	out, _ := json.MarshalIndent(map[string]any{
		"data":  logs,
		"limit": filter.Limit,
	}, "", "  ")
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// handleModels 返回已配置的可用模型列表（OpenAI 兼容格式）
// 注意：返回的 id 是路由匹配模式（支持通配符 * 和 ?），而非具体模型名称。
// 客户端请求时，网关会按顺序匹配这些模式，首条命中生效。
func (s *server) handleModels(w http.ResponseWriter) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	// 从路由规则中提取唯一的 match 模型模式
	seen := make(map[string]bool)
	models := make([]map[string]any, 0, len(cfg.Routes))
	for _, route := range cfg.Routes {
		if !seen[route.Match] {
			seen[route.Match] = true
			entry := map[string]any{
				"id":           route.Match,
				"object":       "model",
				"owned_by":     route.Provider,
				"target_model": route.Model,
			}
			// 如果配置了 vision，添加视觉模型信息
			if route.Vision != nil {
				entry["vision"] = map[string]any{
					"provider": route.Vision.Provider,
					"model":    route.Vision.Model,
				}
			}
			models = append(models, entry)
		}
	}

	result := map[string]any{
		"object":      "list",
		"data":        models,
		"description": "返回网关已配置的路由匹配模式（支持通配符 * 和 ?）。客户端请求时按顺序匹配，首条命中生效。",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	return fmt.Sprintf("r%05d", (n-1)%100000+1)
}

// handleIndex 返回前端页面。默认使用嵌入资源；开发模式下从 AI_GATEWAY_WEB_DIR 实时读取。
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	if s.webDevDir != "" {
		data, err = os.ReadFile(filepath.Join(s.webDevDir, "index.html"))
		if err == nil {
			data = injectDevReload(data)
		}
		w.Header().Set("Cache-Control", "no-store")
	} else {
		data, err = webFS.ReadFile("web/index.html")
	}
	if err != nil {
		http.Error(w, "前端页面加载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func injectDevReload(data []byte) []byte {
	script := []byte(`<script>
(() => {
  const events = new EventSource('/__dev/reload');
  events.onmessage = (event) => {
    if (event.data === 'reload') window.location.reload();
  };
})();
</script>`)
	if bytes.Contains(data, script) {
		return data
	}
	if idx := bytes.LastIndex(data, []byte("</body>")); idx >= 0 {
		out := make([]byte, 0, len(data)+len(script))
		out = append(out, data[:idx]...)
		out = append(out, script...)
		out = append(out, data[idx:]...)
		return out
	}
	return append(data, script...)
}

func (s *server) handleDevReload(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前响应不支持流式刷新", http.StatusInternalServerError)
		return
	}
	indexPath := filepath.Join(s.webDevDir, "index.html")
	lastMod := fileModTime(indexPath)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			current := fileModTime(indexPath)
			if !current.IsZero() && current.After(lastMod) {
				lastMod = current
				fmt.Fprint(w, "data: reload\n\n")
				flusher.Flush()
			}
		}
	}
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// handleConfigAPI 处理配置管理相关的 API 请求
func (s *server) handleConfigAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/config")

	switch {
	case path == "" && r.Method == http.MethodGet:
		// GET /api/config - 返回配置 JSON（apiKey 脱敏）
		s.handleGetConfig(w, r)
	case path == "/raw" && r.Method == http.MethodGet:
		// GET /api/config/raw - 返回原始 YAML
		s.handleGetConfigRaw(w, r)
	case path == "" && r.Method == http.MethodPut:
		// PUT /api/config - 保存配置
		s.handlePutConfig(w, r)
	case path == "/reload" && r.Method == http.MethodPost:
		// POST /api/config/reload - 重新加载配置
		s.handleReloadConfig(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "gateway_error", "未知的配置 API 端点")
	}
}

// handleGetConfig 返回配置 JSON（apiKey 脱敏）
func (s *server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	// 创建副本并脱敏 apiKey
	cfgCopy := *cfg
	providersCopy := make(map[string]*config.Provider)
	for name, p := range cfg.Providers {
		pCopy := *p
		pCopy.APIKey = mask(p.APIKey)
		providersCopy[name] = &pCopy
	}
	cfgCopy.Providers = providersCopy

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfgCopy)
}

// handleGetConfigRaw 返回原始 YAML 文本（apiKey 脱敏）
func (s *server) handleGetConfigRaw(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	data, err := config.ReadRaw(cfg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "读取配置失败: "+err.Error())
		return
	}

	// 简单脱敏：替换所有 apiKey 值
	content := string(data)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "apiKey:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				value := strings.TrimSpace(parts[1])
				if value != "" && value != `""` && value != `''` {
					lines[i] = parts[0] + ": " + mask(value)
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(strings.Join(lines, "\n")))
}

// handlePutConfig 保存配置
func (s *server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "gateway_error", "读取请求体失败: "+err.Error())
		return
	}

	// 验证配置
	newCfg, err := config.LoadAndValidate(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "config_validation_error", err.Error())
		return
	}

	s.cfgMu.RLock()
	oldCfg := s.cfg
	s.cfgMu.RUnlock()

	// 处理 apiKey：如果前端传回的是脱敏值，保留原值
	for name, p := range newCfg.Providers {
		if oldP, exists := oldCfg.Providers[name]; exists {
			if strings.Contains(p.APIKey, "****") {
				p.APIKey = oldP.APIKey
			}
		}
	}

	// 设置路径并保存
	newCfg.Path = oldCfg.Path
	if err := config.Save(newCfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "保存配置失败: "+err.Error())
		return
	}

	// 热重载：更新配置和队列管理器
	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()

	// 更新队列管理器的 provider 限速器
	for name, p := range newCfg.Providers {
		s.qm.UpdateProvider(name, p.MaxConcurrent, p.MaxPerSecond)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "配置已保存，服务已热重载",
	})
}

// handleReloadConfig 从磁盘重新加载配置
func (s *server) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	newCfg, err := config.Load()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "重新加载配置失败: "+err.Error())
		return
	}

	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()

	// 更新队列管理器的 provider 限速器
	for name, p := range newCfg.Providers {
		s.qm.UpdateProvider(name, p.MaxConcurrent, p.MaxPerSecond)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "配置已从磁盘重新加载",
	})
}
