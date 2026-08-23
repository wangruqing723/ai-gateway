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

	"ai-gateway/internal/breaker"
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
		breaker:        breaker.New(breakerSettings(cfg)),
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
	breaker        *breaker.Breaker
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

	// 手动重置熔断器。不与 /api/providers/health 合并：两者判据不同，
	// 耦合起来「为什么这个上游被放行了」会变得难以解释。
	if urlPath == "/api/providers/breaker/reset" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		if isCrossSiteRequest(r) {
			writeForbiddenOrigin(w)
			return
		}
		s.handleBreakerReset(w, r)
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

	// 路由匹配：得到按配置顺序排列的候选列表，作为故障转移的尝试顺序
	matched := router.MatchRoute(model, cfg)
	if matched == nil {
		reqLog.Error = fmt.Sprintf("没有匹配 model %q 的路由规则", model)
		writeJSONError(w, http.StatusBadRequest, "gateway_error", reqLog.Error)
		return
	}
	reqLog.Route = matched.RouteMatch

	// 是否需要视觉识别：路由配了 vision 且消息含图片
	needVision := matched.VisionProvider != nil && vision.HasImages(internal.Messages)
	reqLog.Vision = needVision

	// 首个候选决定日志与提示里的展示信息（真实使用的候选在循环里逐次覆盖）
	first := matched.Candidates[0]
	reqLog.Provider = first.Provider.Name
	reqLog.TargetModel = first.TargetModel
	displayModel := first.TargetModel
	if needVision {
		displayModel = matched.VisionModel
	}
	logf(reqID, "→ %s → %s [%d msgs, stream=%v]", model, displayModel, len(internal.Messages), internal.Stream)

	// 图片翻译放在候选循环之外：产出的是文字描述，与具体目标无关，重复调用纯浪费
	if needVision {
		vp := matched.VisionProvider
		vp.APIKey = router.ResolveAPIKey(vp, r.Header)
		internal.Messages = s.translator.Translate(r.Context(), internal.Messages, vp, matched.VisionModel,
			func(f string, a ...any) { logf(reqID, f, a...) })
	}

	// 候选尝试范围：failover 关闭时只试首个候选，行为与单目标时代一致
	attemptLimit := 1
	if cfg.Failover.Enabled && len(matched.Candidates) > 1 {
		attemptLimit = cfg.Failover.MaxAttempts
		if attemptLimit > len(matched.Candidates) {
			attemptLimit = len(matched.Candidates)
		}
		if attemptLimit < 1 {
			attemptLimit = 1
		}
	}

	var (
		trail         []string
		buildErr      string
		attempts      int
		lastAbandoned bool
		breakerSkips  int
		soonestRetry  time.Duration
	)
	// 遍历全部候选，但真实尝试次数受 attemptLimit 约束：
	// 被熔断跳过的候选不消耗尝试额度，否则熔断反而会削弱可用性。
	for i := 0; i < len(matched.Candidates) && attempts < attemptLimit; i++ {
		candidate := matched.Candidates[i]
		name := candidate.Provider.Name

		// 熔断过滤：开路的 provider 直接跳过，不占用尝试额度
		if s.breaker != nil {
			allowed, retryAfter := s.breaker.Allow(name)
			if !allowed {
				breakerSkips++
				trail = append(trail, name+":breaker_open")
				if retryAfter > 0 && (soonestRetry == 0 || retryAfter < soonestRetry) {
					soonestRetry = retryAfter
				}
				logf(reqID, "  候选 %s 已熔断，跳过（剩余冷却 %dms）", name, retryAfter.Milliseconds())
				continue
			}
		}

		// 是否还有后续候选可试：额度未用尽且后面还有候选
		hasNext := attempts+1 < attemptLimit && i+1 < len(matched.Candidates)

		outcome := s.forwardAttempt(w, r, forwardAttemptInput{
			cfg:           cfg,
			reqID:         reqID,
			start:         start,
			clientFormat:  clientFormat,
			originalModel: model,
			internal:      internal,
			rawBody:       body,
			needVision:    needVision,
			candidate:     candidate,
			allowRetry:    hasNext,
			attemptNo:     attempts + 1,
			reqLog:        &reqLog,
		})

		if outcome.buildErr != "" {
			// 该候选构建不出上游请求（协议不兼容等）：跳过，换下一个。
			// 未发起网络请求，须归还熔断的探针额度。
			if s.breaker != nil {
				s.breaker.Report(name, breaker.OutcomeIgnored)
			}
			buildErr = outcome.buildErr
			trail = append(trail, fmt.Sprintf("%s:build_error", name))
			logf(reqID, "  候选 %s 构建失败，跳过: %s", name, outcome.buildErr)
			continue
		}

		if s.breaker != nil {
			s.breaker.Report(name, outcome.breakerOutcome)
		}
		attempts++
		trail = append(trail, outcome.trail)
		lastAbandoned = outcome.abandoned
		if !outcome.abandoned {
			break
		}
		logf(reqID, "  候选 %s 放弃（%s），尝试下一个", name, outcome.trail)
	}

	reqLog.Attempts = attempts
	if len(trail) > 1 {
		reqLog.AttemptTrail = strings.Join(trail, " → ")
	}

	switch {
	case attempts == 0 && breakerSkips > 0:
		// 全部候选被熔断：给出最早恢复者的剩余冷却，让客户端知道何时重试
		reqLog.AttemptTrail = strings.Join(trail, " → ")
		reqLog.Error = "全部候选上游已熔断"
		if soonestRetry > 0 {
			seconds := int(soonestRetry.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("retry-after", strconv.Itoa(seconds))
		}
		writeJSONError(w, http.StatusServiceUnavailable, "breaker_open", reqLog.Error)
	case attempts == 0:
		// 全部候选都构建失败：此时没有任何一次真实转发，需自行写终态
		reqLog.Error = "上游请求协议转换失败: " + buildErr
		reqLog.AttemptTrail = strings.Join(trail, " → ")
		writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
	case lastAbandoned:
		// 兜底：最后一次尝试被放弃却没有后续候选可试（例如剩余候选全被熔断）。
		// 放弃时未向客户端写入任何字节，必须在这里补一个终态响应。
		reqLog.Error = "全部候选上游均不可用"
		writeJSONError(w, http.StatusBadGateway, "all_candidates_failed", reqLog.Error)
	}
}

// forwardAttemptInput 单次候选尝试的输入，字段在循环内不被修改。
type forwardAttemptInput struct {
	cfg           *config.Config
	reqID         string
	start         time.Time
	clientFormat  string
	originalModel string
	internal      *converter.Internal
	rawBody       map[string]any
	needVision    bool
	candidate     router.Candidate
	attemptNo     int // 从 1 开始，用于 x-ai-gateway-attempts
	allowRetry    bool
	reqLog        *metrics.RequestLog
}

// forwardAttemptOutcome 单次尝试结果。
// buildErr 非空表示该候选无法构建上游请求，未发起任何网络请求；
// abandoned 表示已发起请求但按 failover 策略放弃，且未向客户端写入任何字节。
type forwardAttemptOutcome struct {
	buildErr  string
	abandoned bool
	trail     string // "provider:429/ratelimit"
	// breakerOutcome 本次尝试对熔断计数的影响，由调用方汇报给 breaker。
	breakerOutcome breaker.Outcome
}

// forwardAttempt 执行单个候选的一次转发尝试。
// 队列 slot 的 release 收在本函数的 defer 里，循环多次尝试也不会累积占用。
func (s *server) forwardAttempt(w http.ResponseWriter, r *http.Request, in forwardAttemptInput) forwardAttemptOutcome {
	cfg := in.cfg
	reqID := in.reqID
	p := in.candidate.Provider
	targetModel := in.candidate.TargetModel

	apiKey, keySource := router.ResolveAPIKeyWithSource(p, r.Header)
	p.APIKey = apiKey

	isPassthrough := converter.IsPassthrough(p.Format, in.clientFormat)
	if in.internal.Err != nil && !isPassthrough {
		return forwardAttemptOutcome{buildErr: "请求协议转换失败: " + in.internal.Err.Error()}
	}

	// 内部格式 → 上游 provider 请求体
	var (
		upstreamMap map[string]any
		err         error
	)
	if isPassthrough {
		// 同格式请求无需 canonical 重建；保留 provider 原生扩展字段，只替换路由后的 model。
		// 必须浅拷贝：直接改 in.rawBody 会污染后续候选的输入。
		upstreamMap = make(map[string]any, len(in.rawBody)+1)
		for key, value := range in.rawBody {
			upstreamMap[key] = value
		}
		upstreamMap["model"] = targetModel
		if in.needVision {
			// 图片翻译只覆盖 messages，system/tools 等 provider 原生扩展仍保留原值。
			var translated map[string]any
			if p.Format == "anthropic" {
				translated = converter.ToAnthropicBody(in.internal, targetModel)
			} else {
				translated = converter.ToOpenAIChatBody(in.internal, targetModel)
			}
			upstreamMap["messages"] = mergeTranslatedMessageContent(in.rawBody["messages"], translated["messages"])
		}
	} else if p.Format == "anthropic" {
		upstreamMap, err = converter.ToAnthropicBodyChecked(in.internal, targetModel)
	} else {
		upstreamMap, err = converter.ToOpenAIChatBodyChecked(in.internal, targetModel)
	}
	if err != nil {
		return forwardAttemptOutcome{buildErr: err.Error()}
	}
	upstreamBody, err := json.Marshal(upstreamMap)
	if err != nil {
		in.reqLog.Error = "上游请求体序列化失败: " + err.Error()
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", in.reqLog.Error)
		// 网关侧序列化失败，与上游健康无关
		return forwardAttemptOutcome{trail: p.Name + ":serialize_error", breakerOutcome: breaker.OutcomeIgnored}
	}

	// 本次尝试真正会用到该候选，更新日志归属
	in.reqLog.Provider = p.Name
	in.reqLog.TargetModel = targetModel
	in.reqLog.KeySource = keySource
	in.reqLog.KeyFingerprint = keyFingerprint(apiKey)

	// 直通模式：跳过队列（并发/限速/排队），请求直接转发；超时用 direct* 配置。
	// 非直通模式：先经队列获取执行槽位（并发 + 限速 + 等待超时）。
	if !cfg.DirectMode {
		release, waitMs, err := s.qm.Acquire(r.Context(), p.Name, p.MaxConcurrent, p.MaxPerSecond, p.MaxQueueWait)
		in.reqLog.QueueWaitMs += waitMs
		if err != nil {
			logf(reqID, "  队列处理异常: %s", err.Error())
			if err == queue.ErrQueueTimeout {
				// 排队超时且允许转移：换下一个候选，别把客户端 503 掉
				if in.allowRetry && config.BoolOr(cfg.Failover.OnQueueTimeout, true) && cfg.Failover.Enabled {
					// 本地队列背压，不是上游故障
					return forwardAttemptOutcome{abandoned: true, trail: p.Name + ":queue_timeout", breakerOutcome: breaker.OutcomeIgnored}
				}
				in.reqLog.Error = err.Error()
				writeJSONError(w, http.StatusServiceUnavailable, "queue_timeout", err.Error())
			} else if r.Context().Err() != nil {
				// 客户端已断开，net/http 会忽略写入，无需也不应写响应
				in.reqLog.Error = err.Error()
				logf(reqID, "  客户端已断开，跳过响应")
			} else {
				in.reqLog.Error = err.Error()
				writeJSONError(w, http.StatusBadGateway, "gateway_error", "队列错误: "+err.Error())
			}
			return forwardAttemptOutcome{trail: p.Name + ":queue_error", breakerOutcome: breaker.OutcomeIgnored}
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

	// 告知客户端实际服务的候选与尝试次数（对齐 Cloudflare 的 cf-aig-step 思路）。
	// 所有放弃都发生在 WriteHeader 之前，因此这里覆盖写入是安全的，流式响应也能带上。
	h := w.Header()
	h.Set("x-ai-gateway-provider", p.Name)
	h.Set("x-ai-gateway-attempts", strconv.Itoa(in.attemptNo))

	var (
		abandonReason  atomic.Value // string
		abandonBreaker atomic.Value // breaker.Outcome
		// attemptStatus 本次尝试拿到的上游状态码，0 表示上游没给出响应头。
		// 必须用本次尝试的局部变量，不能直接读 in.reqLog.UpstreamStatus：后者跨候选
		// 共享，候选 1 的 502 会在候选 2 传输失败（不上报）时被当成候选 2 的状态码。
		attemptStatus int
	)
	opts := &proxy.Options{
		ClientReq:             r,
		ClientRes:             w,
		UpstreamBody:          upstreamBody,
		Provider:              p,
		ClientFormat:          in.clientFormat,
		OriginalModel:         in.originalModel,
		IsStreaming:           in.internal.Stream,
		Log:                   func(f string, a ...any) { logf(reqID, f, a...) },
		StartTime:             in.start,
		TimeoutMs:             timeoutMs,
		HeaderTimeoutMs:       headerTimeoutMs,
		StreamActivityTimeout: activityTimeoutMs,
		HTTPClient:            s.httpClient,
		// 记录真实上游状态码：熔断判据和请求日志都要用。proxy 只在确实拿到
		// 响应头时回调，因此 0 恒表示「上游没给状态码」而非「上游返回 0」。
		// 回调与 Forward 同 goroutine 同步执行，无需额外同步。
		OnUpstreamStatus: func(code int) {
			attemptStatus = code
			in.reqLog.UpstreamStatus = code
		},
	}
	if in.allowRetry {
		opts.ShouldRetry = func(upstreamCode int, retryAfter time.Duration, err error) bool {
			reason, ok := failoverReason(&cfg.Failover, upstreamCode, retryAfter, err)
			if !ok {
				return false
			}
			abandonReason.Store(reason)
			abandonBreaker.Store(breakerOutcomeFor(upstreamCode, err))
			return true
		}
	}

	forwardErr := proxy.Forward(opts)
	if errors.Is(forwardErr, proxy.ErrAttemptAbandoned) {
		reason, _ := abandonReason.Load().(string)
		if reason == "" {
			reason = "abandoned"
		}
		outcome, ok := abandonBreaker.Load().(breaker.Outcome)
		if !ok {
			outcome = breaker.OutcomeIgnored
		}
		return forwardAttemptOutcome{abandoned: true, trail: p.Name + ":" + reason, breakerOutcome: outcome}
	}

	upstreamStatus := attemptStatus
	if forwardErr != nil {
		in.reqLog.Error = forwardErr.Error()
		// 区分客户端断开、超时、其他错误，避免把正常断开误报为异常
		if errors.Is(forwardErr, context.Canceled) {
			logf(reqID, "  客户端断开连接")
		} else if errors.Is(forwardErr, context.DeadlineExceeded) {
			logf(reqID, "  转发超时（%d秒）: %s", timeoutMs/1000, forwardErr.Error())
		} else {
			logf(reqID, "  转发结束（异常）: %s", forwardErr.Error())
		}
	}
	status := upstreamStatus
	if status == 0 {
		status = statusFromRecorder(w)
	}
	// 熔断判据用 upstreamStatus 而非 status：后者在上游无响应时会回落到网关自己写的
	// 状态码，据此判断会把网关侧错误算到上游头上。upstreamStatus 为 0 时按 forwardErr
	// 分类，正好覆盖传输错误与超时。
	//
	// 这里必须显式赋值：OutcomeSuccess 是零值，漏赋会让最后一个候选（allowRetry=false，
	// 不触发放弃）的 5xx 被当成成功上报，既开不了路还会清零此前累积的失败streak。
	return forwardAttemptOutcome{
		trail:          fmt.Sprintf("%s:%d", p.Name, status),
		breakerOutcome: breakerOutcomeFor(upstreamStatus, forwardErr),
	}
}

// failoverReason 按 failover 配置判定一次失败能否转移，返回 trail 用的分类名。
func failoverReason(f *config.Failover, upstreamCode int, retryAfter time.Duration, err error) (string, bool) {
	if !f.Enabled {
		return "", false
	}
	if upstreamCode == 0 {
		// 传输层失败：连接错误 / 超时
		switch {
		case errors.Is(err, proxy.ErrStreamHeaderTimeout):
			return "header_timeout", f.TransferOnStreamHeaderTimeout()
		case errors.Is(err, proxy.ErrRequestTimeout), errors.Is(err, context.DeadlineExceeded):
			return "timeout", f.TransferOnTransportError()
		default:
			return "transport_error", f.TransferOnTransportError()
		}
	}
	switch {
	case upstreamCode == http.StatusTooManyRequests:
		if !f.TransferOnRateLimit() {
			return "", false
		}
		// Retry-After 超过阈值说明该 provider 短期没戏，同样转移
		return "429/ratelimit", true
	case upstreamCode == http.StatusUnauthorized || upstreamCode == http.StatusForbidden:
		// 默认关闭：key 配错时转移只会连锁失败，且掩盖真实原因
		return fmt.Sprintf("%d/auth", upstreamCode), f.TransferOnAuthError()
	case upstreamCode >= 500:
		return fmt.Sprintf("%d/server", upstreamCode), f.TransferOnServerError()
	default:
		// 4xx 业务错误（400 参数错、404 模型不存在等）换 provider 也是同样结果
		return "", false
	}
}

// breakerOutcomeFor 把一次尝试结果归类成熔断判据。
//
// 计入失败：传输错误、超时、5xx。
// 不计入：429（上游正常限流，判成故障比不熔断更糟）、401/403（配置问题，熔断修不了
// 还会掩盖密钥过期）、客户端主动断开、普通 4xx（请求本身的问题）。
func breakerOutcomeFor(upstreamCode int, err error) breaker.Outcome {
	if upstreamCode == 0 {
		switch {
		case errors.Is(err, context.Canceled):
			// 客户端断开，与上游健康无关
			return breaker.OutcomeIgnored
		case err != nil:
			return breaker.OutcomeFailure
		default:
			return breaker.OutcomeIgnored
		}
	}
	switch {
	case upstreamCode == http.StatusTooManyRequests:
		return breaker.OutcomeIgnored
	case upstreamCode == http.StatusUnauthorized || upstreamCode == http.StatusForbidden:
		return breaker.OutcomeIgnored
	case upstreamCode >= 500:
		return breaker.OutcomeFailure
	case upstreamCode >= 200 && upstreamCode < 400:
		return breaker.OutcomeSuccess
	default:
		// 普通 4xx：上游是健康的，只是这个请求它不接受
		return breaker.OutcomeIgnored
	}
}

// statusFromRecorder 读取已写回客户端的状态码，用于 trail 记录。
func statusFromRecorder(w http.ResponseWriter) int {
	if rec, ok := w.(*statusRecorder); ok {
		return rec.Status()
	}
	return 0
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
		// breakers 为 null 表示熔断未启用，与「全部健康」区分开
		"breakers": s.breakerSnapshot(),
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

// breakerSnapshot 返回熔断状态快照，未启用或未构造时返回 nil。
func (s *server) breakerSnapshot() map[string]breaker.State {
	if s.breaker == nil {
		return nil
	}
	return s.breaker.Snapshot()
}

// handleBreakerReset 手动闭合熔断器。带 provider 参数时只重置该 provider，否则全部重置。
//
// 不与 POST /api/providers/health 合并：两者判据不同（一个探 /v1/models，
// 一个看真实请求结果），耦合起来行为难解释。
func (s *server) handleBreakerReset(w http.ResponseWriter, r *http.Request) {
	if s.breaker == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "gateway_error", "熔断器未启用")
		return
	}
	provider := r.URL.Query().Get("provider")
	payload := map[string]any{}
	if provider != "" {
		payload["provider"] = provider
		payload["reset"] = s.breaker.Reset(provider)
	} else {
		payload["reset"] = s.breaker.ResetAll()
	}
	payload["breakers"] = s.breaker.Snapshot()
	out, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
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
	if s.breaker != nil {
		s.breaker.SetSettings(breakerSettings(newCfg))
		active := make(map[string]struct{}, len(newCfg.Providers))
		for name := range newCfg.Providers {
			active[name] = struct{}{}
		}
		s.breaker.Reconcile(active)
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

// breakerSettings 把配置里的 breaker 块映射成熔断器参数。
func breakerSettings(cfg *config.Config) breaker.Settings {
	return breaker.Settings{
		Enabled:             cfg.Breaker.Enabled,
		ConsecutiveFailures: cfg.Breaker.ConsecutiveFailures,
		OpenMs:              cfg.Breaker.OpenMs,
		HalfOpenProbes:      cfg.Breaker.HalfOpenProbes,
	}
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
