// Command ai-gateway 是当前主版本的 Go 网关实现。
//
// 已实现：配置加载、路由匹配、per-provider 并发/限速队列、流式/非流式转发、context 超时、
// 三格式互转 converter、vision 图片识别、SQLite 缓存、健康检查。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
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

//go:embed web/index.html web/vendor/*
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
	revision, err := newConfigRevision()
	if err != nil {
		_ = imgCache.Close()
		fmt.Fprintf(os.Stderr, "[gateway] 生成配置 revision 失败: %s\n", err)
		os.Exit(1)
	}

	srv := &server{
		cfg:            cfg,
		revision:       revision,
		listenHost:     cfg.Host,
		listenPort:     cfg.Port,
		qm:             qm,
		httpClient:     httpClient,
		cache:          imgCache,
		translator:     translator,
		metrics:        metrics.NewCollector(1000),
		providerHealth: providerhealth.NewChecker(),
		webDevDir:      os.Getenv("AI_GATEWAY_WEB_DIR"),
	}
	initialLimits := make(map[string]queue.Limits, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		initialLimits[name] = queue.Limits{MaxConcurrent: provider.MaxConcurrent, MaxPerSecond: provider.MaxPerSecond}
	}
	qm.Reconcile(initialLimits)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handle)

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	printBanner(cfg, imgCache)

	httpServer := newGatewayHTTPServer(addr, mux)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		<-stop
		logSystem("收到退出信号，正在关闭...")
		if err := shutdownThenClose(httpServer, 30*time.Second, imgCache.Close); err != nil {
			logSystem("优雅关闭异常: %s", err)
		}
		close(shutdownDone)
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		signal.Stop(stop)
		_ = imgCache.Close()
		logSystem("服务器错误: %s", err)
		os.Exit(1)
	}
	<-shutdownDone
}

type httpShutdowner interface {
	Shutdown(context.Context) error
}

type cacheRuntime interface {
	GetStats() cache.Stats
	Cleanup(maxAgeDays, maxRecords int) (cache.CleanupResult, error)
}

type visionRuntime interface {
	Translate(context.Context, []any, *config.Provider, string, vision.LogFunc) []any
	SetDirectMode(bool)
}

type providerHealthRuntime interface {
	Snapshot(*config.Config) map[string]providerhealth.Status
	CheckAll(context.Context, *config.Config, *http.Client) map[string]providerhealth.Status
	InvalidateChanged(*config.Config, *config.Config)
}

func newGatewayHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func shutdownThenClose(server httpShutdowner, timeout time.Duration, closeResource func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := server.Shutdown(ctx)
	closeErr := closeResource()
	return errors.Join(shutdownErr, closeErr)
}

type server struct {
	cfg            *config.Config
	cfgMu          sync.RWMutex // 保护 cfg 的并发读写
	configOpMu     sync.Mutex   // 串行化保存、重载与运行时应用
	revision       string
	revisionSource func() (string, error)
	listenHost     string
	listenPort     int
	qm             *queue.Manager
	httpClient     *http.Client
	cache          cacheRuntime
	translator     visionRuntime
	metrics        *metrics.Collector
	providerHealth providerHealthRuntime
	webDevDir      string
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.Split(r.URL.Path, "?")[0]
	setSecurityHeaders(w)

	// 前端页面：GET/HEAD / 返回 index.html
	if urlPath == "/" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		s.handleIndex(w, r)
		return
	}

	if strings.HasPrefix(urlPath, "/vendor/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		s.handleStatic(w, r, urlPath)
		return
	}

	// 开发模式前端热加载：仅 AI_GATEWAY_WEB_DIR 启用时可用
	if urlPath == "/__dev/reload" && s.webDevDir != "" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleDevReload(w, r)
		return
	}

	// 配置管理 API：/api/config/*
	if strings.HasPrefix(urlPath, "/api/config") {
		s.handleConfigAPI(w, r)
		return
	}

	if urlPath == "/api/metrics" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleMetrics(w, r)
		return
	}

	if urlPath == "/api/logs" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleLogs(w, r)
		return
	}

	if urlPath == "/api/providers/health" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		if isCrossSiteRequest(r) {
			writeForbiddenOrigin(w)
			return
		}
		s.handleProviderHealthCheck(w, r)
		return
	}

	// /health 端点
	if urlPath == "/health" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		s.handleHealth(w, r.Method == http.MethodHead)
		return
	}

	// /v1/models 端点：返回已配置的可用模型列表
	if urlPath == "/v1/models" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
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
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if isCrossSiteRequest(r) {
		writeForbiddenOrigin(w)
		return
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "推理请求须使用 application/json")
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
		ClientIP:     clientIP(r),
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

	raw, err := readBodyLimited(r, maxProxyBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			reqLog.Error = err.Error()
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
			return
		}
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
	apiKey, keySource := router.ResolveAPIKeyWithSource(p, r.Header)
	p.APIKey = apiKey
	reqLog.KeySource = keySource
	reqLog.KeyFingerprint = keyFingerprint(apiKey)

	// 是否需要视觉识别：路由配了 vision 且消息含图片
	needVision := matched.VisionProvider != nil && vision.HasImages(internal.Messages)
	isPassthrough := converter.IsPassthrough(p.Format, clientFormat)
	if internal.Err != nil && !isPassthrough {
		reqLog.Error = "请求协议转换失败: " + internal.Err.Error()
		writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
		return
	}
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
	if isPassthrough {
		// 同格式请求无需 canonical 重建；保留 provider 原生扩展字段，只替换路由后的 model。
		upstreamMap = body
		upstreamMap["model"] = matched.TargetModel
		if needVision {
			// 图片翻译只覆盖 messages，system/tools 等 provider 原生扩展仍保留原值。
			var translated map[string]any
			if p.Format == "anthropic" {
				translated = converter.ToAnthropicBody(internal, matched.TargetModel)
			} else {
				translated = converter.ToOpenAIChatBody(internal, matched.TargetModel)
			}
			upstreamMap["messages"] = mergeTranslatedMessageContent(body["messages"], translated["messages"])
		}
	} else if p.Format == "anthropic" {
		upstreamMap, err = converter.ToAnthropicBodyChecked(internal, matched.TargetModel)
	} else {
		upstreamMap, err = converter.ToOpenAIChatBodyChecked(internal, matched.TargetModel)
	}
	if err != nil {
		reqLog.Error = "上游请求协议转换失败: " + err.Error()
		writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
		return
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

func mergeTranslatedMessageContent(original, translated any) any {
	originalMessages, originalOK := original.([]any)
	translatedMessages, translatedOK := translated.([]any)
	if !originalOK || !translatedOK || len(originalMessages) != len(translatedMessages) {
		return translated
	}
	merged := make([]any, len(originalMessages))
	for i := range originalMessages {
		originalMessage, originalOK := originalMessages[i].(map[string]any)
		translatedMessage, translatedOK := translatedMessages[i].(map[string]any)
		if !originalOK || !translatedOK || originalMessage["role"] != translatedMessage["role"] {
			return translated
		}
		message := make(map[string]any, len(originalMessage))
		for key, value := range originalMessage {
			message[key] = value
		}
		if content, exists := translatedMessage["content"]; exists {
			message["content"] = content
		}
		merged[i] = message
	}
	return merged
}

func (s *server) handleHealth(w http.ResponseWriter, head bool) {
	s.cfgMu.RLock()
	cfg := s.cfg
	listenHost := s.listenHost
	listenPort := s.listenPort
	s.cfgMu.RUnlock()
	if listenHost == "" {
		listenHost = cfg.Host
	}
	if listenPort == 0 {
		listenPort = cfg.Port
	}

	queues := make(map[string]queue.Status)
	for name, p := range cfg.Providers {
		queues[name] = s.qm.StatusOf(name, p.MaxConcurrent, p.MaxPerSecond)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	var cs cache.Stats
	if s.cache != nil {
		cs = s.cache.GetStats()
	}
	health := map[string]any{
		"status":         "ok",
		"timeout":        cfg.Timeout,
		"listenAddress":  net.JoinHostPort(listenHost, strconv.Itoa(listenPort)),
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
	out, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "健康状态序列化失败: "+err.Error())
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(out)
	}
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
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request, urlPath string) {
	relative := strings.TrimPrefix(urlPath, "/")
	if relative == "" || strings.Contains(relative, "..") || strings.ContainsRune(relative, '\\') {
		writeJSONError(w, http.StatusNotFound, "gateway_error", "静态资源不存在")
		return
	}
	var data []byte
	var err error
	if s.webDevDir != "" {
		data, err = os.ReadFile(filepath.Join(s.webDevDir, filepath.FromSlash(relative)))
	} else {
		data, err = webFS.ReadFile("web/" + relative)
	}
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "gateway_error", "静态资源不存在")
		return
	}
	w.Header().Set("Content-Type", staticContentType(relative))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
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

	switch path {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.handleGetConfig(w, r)
		case http.MethodPut:
			if isCrossSiteRequest(r) {
				writeForbiddenOrigin(w)
				return
			}
			if !isAllowedConfigMediaType(requestMediaType(r)) {
				writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "配置须使用 JSON 或 YAML Content-Type")
				return
			}
			s.handlePutConfig(w, r)
		default:
			writeMethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
	case "/raw":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleGetConfigRaw(w, r)
	case "/reload":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		if isCrossSiteRequest(r) {
			writeForbiddenOrigin(w)
			return
		}
		s.handleReloadConfig(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "gateway_error", "未知的配置 API 端点")
	}
}

// handleGetConfig 返回配置 JSON（apiKey 脱敏）
func (s *server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, revision, restartRequired, err := s.configViewSnapshot()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置 revision 失败: "+err.Error())
		return
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "配置序列化失败: "+err.Error())
		return
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "配置序列化失败: "+err.Error())
		return
	}
	providers, _ := response["providers"].(map[string]any)
	for name, provider := range cfg.Providers {
		view, _ := providers[name].(map[string]any)
		view["apiKeyConfigured"] = provider.APIKey == config.APIKeyKeepSentinel
	}
	response["revision"] = revision
	response["restartRequired"] = restartRequired

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", strconv.Quote(revision))
	_ = json.NewEncoder(w).Encode(response)
}

func (s *server) configViewSnapshot() (*config.Config, string, []string, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.revision == "" {
		revision, err := s.issueConfigRevision()
		if err != nil {
			return nil, "", nil, err
		}
		s.revision = revision
	}
	if s.listenHost == "" {
		s.listenHost = s.cfg.Host
	}
	if s.listenPort == 0 {
		s.listenPort = s.cfg.Port
	}
	cfgCopy := *s.cfg
	cfgCopy.Providers = make(map[string]*config.Provider, len(s.cfg.Providers))
	for name, provider := range s.cfg.Providers {
		providerCopy := *provider
		providerCopy.Name = ""
		if provider.APIKey != "" {
			providerCopy.APIKey = config.APIKeyKeepSentinel
		}
		cfgCopy.Providers[name] = &providerCopy
	}
	return &cfgCopy, s.revision, restartRequiredFields(s.cfg, s.listenHost, s.listenPort), nil
}

// handleGetConfigRaw 返回原始 YAML 文本（apiKey 脱敏）
func (s *server) handleGetConfigRaw(w http.ResponseWriter, r *http.Request) {
	s.configOpMu.Lock()
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	data, err := config.ReadRaw(cfg)
	if err != nil {
		s.configOpMu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "读取配置失败: "+err.Error())
		return
	}

	redacted, err := config.RedactYAML(data, config.APIKeyKeepSentinel)
	s.configOpMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "配置脱敏失败: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(redacted)
}

// handlePutConfig 保存配置
func (s *server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBodyLimited(r, maxConfigBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
			return
		}
		writeJSONError(w, http.StatusBadRequest, "gateway_error", "读取请求体失败: "+err.Error())
		return
	}

	newCfg, err := config.DecodeAndValidate(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "config_validation_error", err.Error())
		return
	}

	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()

	s.cfgMu.Lock()
	oldCfg := s.cfg
	var revisionErr error
	if s.revision == "" {
		s.revision, revisionErr = s.issueConfigRevision()
	}
	currentRevision := s.revision
	s.cfgMu.Unlock()
	if revisionErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置 revision 失败: "+revisionErr.Error())
		return
	}
	if ifMatch := normalizeETag(r.Header.Get("If-Match")); ifMatch != "" && ifMatch != "*" && ifMatch != currentRevision {
		writeJSONError(w, http.StatusConflict, "revision_conflict", "配置已被其他操作更新，请重新加载")
		return
	}

	for name, p := range newCfg.Providers {
		if p.APIKey != config.APIKeyKeepSentinel {
			continue
		}
		oldProvider, exists := oldCfg.Providers[name]
		if !exists || oldProvider.APIKey == "" || !config.SameProviderIdentity(oldProvider, p) {
			writeJSONError(w, http.StatusBadRequest, "config_validation_error", fmt.Sprintf("providers.%s 无法保留已有 apiKey：provider 身份已变化或密钥不存在", name))
			return
		}
		p.APIKey = oldProvider.APIKey
	}

	// 设置路径并保存
	newCfg.Path = oldCfg.Path
	revision, err := s.issueConfigRevision()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置 revision 失败: "+err.Error())
		return
	}
	if err := config.Save(newCfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "保存配置失败: "+err.Error())
		return
	}
	restartRequired := s.applyRuntimeConfig(newCfg, revision)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", strconv.Quote(revision))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "success",
		"message":         "配置已保存，服务已热重载",
		"revision":        revision,
		"restartRequired": restartRequired,
	})
}

// handleReloadConfig 从磁盘重新加载配置
func (s *server) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()

	s.cfgMu.RLock()
	oldCfg := s.cfg
	s.cfgMu.RUnlock()
	raw, err := config.ReadRaw(oldCfg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "重新加载配置失败: "+err.Error())
		return
	}
	newCfg, err := config.DecodeAndValidate(raw)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "重新加载配置失败: "+err.Error())
		return
	}
	newCfg.Path = oldCfg.Path
	revision, err := s.issueConfigRevision()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置 revision 失败: "+err.Error())
		return
	}

	restartRequired := s.applyRuntimeConfig(newCfg, revision)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", strconv.Quote(revision))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "success",
		"message":         "配置已从磁盘重新加载",
		"revision":        revision,
		"restartRequired": restartRequired,
	})
}

func (s *server) applyRuntimeConfig(newCfg *config.Config, revision string) []string {
	limits := make(map[string]queue.Limits, len(newCfg.Providers))
	for name, provider := range newCfg.Providers {
		limits[name] = queue.Limits{MaxConcurrent: provider.MaxConcurrent, MaxPerSecond: provider.MaxPerSecond}
	}

	s.cfgMu.Lock()
	oldCfg := s.cfg
	if s.listenHost == "" {
		s.listenHost = oldCfg.Host
	}
	if s.listenPort == 0 {
		s.listenPort = oldCfg.Port
	}
	restartRequired := restartRequiredFields(newCfg, s.listenHost, s.listenPort)
	if s.qm != nil {
		s.qm.Reconcile(limits)
	}
	if s.providerHealth != nil {
		s.providerHealth.InvalidateChanged(oldCfg, newCfg)
	}
	if s.translator != nil {
		s.translator.SetDirectMode(newCfg.DirectMode)
	}
	s.cfg = newCfg
	s.revision = revision
	s.cfgMu.Unlock()

	if s.cache != nil {
		if _, err := s.cache.Cleanup(newCfg.Cache.MaxAgeDays, newCfg.Cache.MaxRecords); err != nil {
			logSystem("按新配置清理缓存失败: %s", err)
		}
	}
	return restartRequired
}

func restartRequiredFields(cfg *config.Config, listenHost string, listenPort int) []string {
	fields := make([]string, 0, 2)
	if cfg.Host != listenHost {
		fields = append(fields, "host")
	}
	if cfg.Port != listenPort {
		fields = append(fields, "port")
	}
	return fields
}

func (s *server) issueConfigRevision() (string, error) {
	if s.revisionSource != nil {
		revision, err := s.revisionSource()
		if err != nil {
			return "", err
		}
		if revision == "" {
			return "", errors.New("revision 为空")
		}
		return revision, nil
	}
	return newConfigRevision()
}

func newConfigRevision() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func normalizeETag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "W/"))
	}
	return strings.Trim(value, `"`)
}
