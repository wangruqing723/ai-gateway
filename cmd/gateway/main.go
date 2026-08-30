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
	"io"
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

	"ai-gateway/internal/balancer"
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
	"ai-gateway/internal/webbuild"
)

//go:embed web/index.html web/vendor/*
var webFS embed.FS

var reqCounter uint64

const healthStatsCacheTTL = 10 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] %s\n", err)
		os.Exit(1)
	}

	qm := queue.NewManager()

	// 复用连接池的 HTTP 客户端；不设 http.Client.Timeout（非流式靠 ctx 超时，流式靠 header 超时 + 活跃超时）
	//
	// Proxy 必须显式设置：自建 Transport 的零值 Proxy 是「完全不走代理」，
	// 只有 http.DefaultTransport 才默认带 ProxyFromEnvironment。不写这一行，
	// HTTPS_PROXY / HTTP_PROXY 会被静默忽略，需要代理才能出网的部署（公司网络、
	// 被 DNS 污染的上游域名）只会看到 dial 失败，完全看不出是代理没生效。
	// 该 client 是所有出网路径的唯一出口：转发、vision 翻译、健康检测、模型查询。
	// 不想走代理时用 NO_PROXY 排除，语义与 curl、docker 一致。
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
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
		metrics:        metrics.NewCollectorWithWindow(1000, cfg.Metrics.WindowMinutes),
		providerHealth: providerhealth.NewChecker(),
		breaker:        breaker.New(breakerSettings(cfg)),
		selector:       balancer.New(),
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
	CheckProvider(context.Context, *config.Config, *http.Client, string) (providerhealth.Status, bool)
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
	healthStatsMu  sync.Mutex   // 串行化 /health 的缓存刷新，避免并发请求击穿 TTL
	healthStatsAt  time.Time
	healthCache    cache.Stats
	healthHeapMB   uint64
	healthSysMB    uint64
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
	// selector 持有跨请求的候选选择状态（per-route 轮转计数器 + prompt cache 粘性映射）。
	// router 是无状态纯函数，这些状态只能由 server 持有并显式传入。
	selector  *balancer.Selector
	webDevDir string
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

	// 非静态请求只接受本机 loopback、配置中的 host，或 0.0.0.0/:: 监听时的 IP 字面量。
	// 这一层独立于 Origin 校验，用来阻断 DNS rebinding 下同源页面读取网关响应。
	s.cfgMu.RLock()
	requestCfg := s.cfg
	s.cfgMu.RUnlock()
	if !allowRequestHost(r, requestCfg) {
		writeForbiddenHost(w)
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

	// 拉取某 provider 上游的 /v1/models 真实模型列表（非网关自己的 /v1/models）。
	// 用已落盘配置里的 apiKey 探测；供前端配置路由时选择目标模型。
	if urlPath == "/api/providers/models" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		if isCrossSiteRequest(r) {
			writeForbiddenOrigin(w)
			return
		}
		s.handleProviderModels(w, r)
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

	// 协议规范化失败且没有任何候选是同格式（同格式走原样透传，不需要 canonical 重建）
	// —— 此时无论试哪个候选都会失败，必须在 vision 之前返回：视觉翻译是一次真实的
	// 上游调用，放在后面等于先花钱再拒绝。单候选时代这条检查就在 vision 之前，
	// 多候选下的等价条件是「所有候选都不是透传」。
	if internal.Err != nil {
		anyPassthrough := false
		for _, candidate := range matched.Candidates {
			if converter.IsPassthrough(candidate.Provider.Format, clientFormat) {
				anyPassthrough = true
				break
			}
		}
		if !anyPassthrough {
			reqLog.Error = "请求协议转换失败: " + internal.Err.Error()
			writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
			return
		}
	}

	// 粘性键必须在 vision 翻译之前算：翻译会把图片块换成文字描述，
	// 而描述随缓存命中情况变化，会让同一会话前后算出两个不同的键。
	//
	// 只在真的会用到时才算：单候选路由和 failover 策略下 Select 根本不看这个键，
	// 而默认配置正是这一档，没必要为每个请求把 10-25 KB 的 system prompt 过一遍 SHA-256。
	stickyKey := ""
	if len(matched.Candidates) > 1 && balancer.UsesSticky(matched.Strategy) {
		stickyKey = balancer.StickyKey(internal.System, internal.Messages)
	}
	// 候选尝试顺序：strategy 决定「先试谁」，failover 决定「失败了还能试谁」，两者正交。
	order := s.candidateOrder(matched, stickyKey)

	// 首个候选决定日志与提示里的展示信息（真实使用的候选在循环里逐次覆盖）
	first := matched.Candidates[order[0]]
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

	// 候选尝试范围：failover 关闭时只试首个候选，行为与单目标时代一致。
	// 注意这里不看 strategy：round-robin 关掉 failover 是合法组合，
	// 表示「分流但不转移」——选中的那个失败就直接返回。
	attemptLimit := 1
	if cfg.Failover.Enabled && len(matched.Candidates) > 1 {
		attemptLimit = cfg.Failover.AttemptLimit()
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
		// freeSkips 因上游自报限流而放弃、且未消耗额度的次数
		freeSkips          int
		soonestRetry       time.Duration
		nextHTTPAttemptNo  int
		nextDetailSequence int
	)
	// 按 order 遍历全部候选，但真实尝试次数受 attemptLimit 约束：
	// 被熔断跳过的候选不消耗尝试额度，否则熔断反而会削弱可用性。
	for pos := 0; pos < len(order) && attempts < attemptLimit; pos++ {
		candidate := matched.Candidates[order[pos]]
		name := candidate.Provider.Name

		// 熔断过滤：开路的 provider 直接跳过，不占用尝试额度
		if s.breaker != nil {
			allowed, retryAfter := s.breaker.Allow(name)
			if !allowed {
				breakerSkips++
				trail = append(trail, name+":breaker_open")
				nextDetailSequence++
				reqLog.AttemptDetails = append(reqLog.AttemptDetails, metrics.AttemptDetail{
					Sequence:       nextDetailSequence,
					Kind:           "breaker_skip",
					Provider:       name,
					TargetModel:    candidate.TargetModel,
					ProviderFormat: candidate.Provider.Format,
					StartedAt:      time.Now().In(beijingLoc).Format(time.RFC3339Nano),
					Outcome:        "skipped",
					Reason:         "breaker_open",
				})
				if retryAfter > 0 && (soonestRetry == 0 || retryAfter < soonestRetry) {
					soonestRetry = retryAfter
				}
				logf(reqID, "  候选 %s 已熔断，跳过（剩余冷却 %dms）", name, retryAfter.Milliseconds())
				continue
			}
		}

		// 是否还有后续候选可试：额度未用尽且 order 里后面还有候选
		hasNext := attempts+1 < attemptLimit && pos+1 < len(order)
		nextDetailSequence++
		attemptStarted := time.Now()
		detail := metrics.AttemptDetail{
			Sequence:       nextDetailSequence,
			Provider:       name,
			TargetModel:    candidate.TargetModel,
			ProviderFormat: candidate.Provider.Format,
			StartedAt:      attemptStarted.In(beijingLoc).Format(time.RFC3339Nano),
		}

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
			httpAttemptNo: nextHTTPAttemptNo + 1,
			reqLog:        &reqLog,
			detail:        &detail,
		})
		detail.DurationMs = time.Since(attemptStarted).Milliseconds()
		reqLog.AttemptDetails = append(reqLog.AttemptDetails, detail)
		if outcome.requestStarted {
			nextHTTPAttemptNo++
		}

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
		if outcome.freeAttempt {
			// 上游自报「这段时间不可用」（429 + 超阈值的 Retry-After）：与熔断跳过同等
			// 待遇，不消耗额度，否则一个自曝限流的上游会挤掉本来还能试的健康候选。
			freeSkips++
			trail = append(trail, outcome.trail)
			lastAbandoned = true
			logf(reqID, "  候选 %s 放弃（%s），不计入尝试额度", name, outcome.trail)
			continue
		}
		attempts++
		trail = append(trail, outcome.trail)
		lastAbandoned = outcome.abandoned
		if !outcome.abandoned {
			// 只在确实成功后绑定粘性：绑定失败过的目标会把整条会话钉在坏上游上。
			// 复用 breakerOutcome 判定成功，避免再写一份状态码分类。
			if outcome.breakerOutcome == breaker.OutcomeSuccess {
				s.rememberSticky(matched, stickyKey, candidate)
			}
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
	case attempts == 0 && freeSkips > 0:
		// 全部候选都自报限流（429 + 超阈值 Retry-After）且都没消耗额度：
		// 这不是协议错误，必须透传 429 语义，否则客户端会收到误导性的 400。
		reqLog.AttemptTrail = strings.Join(trail, " → ")
		reqLog.Error = "全部候选上游均在限流中"
		writeJSONError(w, http.StatusTooManyRequests, "all_candidates_rate_limited", reqLog.Error)
	case attempts == 0 && buildErr != "":
		// 全部候选都构建失败：此时没有任何一次真实转发，需自行写终态
		reqLog.Error = "上游请求协议转换失败: " + buildErr
		reqLog.AttemptTrail = strings.Join(trail, " → ")
		writeJSONError(w, http.StatusBadRequest, "conversion_error", reqLog.Error)
	case attempts == 0:
		// 兜底：一次都没真实尝试，但既没熔断跳过也没构建失败（理论上不可达）
		reqLog.AttemptTrail = strings.Join(trail, " → ")
		reqLog.Error = "没有可用的候选上游"
		writeJSONError(w, http.StatusBadGateway, "all_candidates_failed", reqLog.Error)
	case lastAbandoned:
		// 兜底：最后一次尝试被放弃却没有后续候选可试（例如剩余候选全被熔断）。
		// 放弃时未向客户端写入任何字节，必须在这里补一个终态响应。
		reqLog.Error = "全部候选上游均不可用"
		writeJSONError(w, http.StatusBadGateway, "all_candidates_failed", reqLog.Error)
	}
}

// candidateOrder 计算候选的尝试顺序（返回下标序列），长度恒等于候选数，不丢候选。
//
// 策略只改「先试谁」；剩余候选仍按该顺序留在后面，供 failover 继续转移。
func (s *server) candidateOrder(matched *router.Match, stickyKey string) []int {
	n := len(matched.Candidates)
	if s.selector == nil || n <= 1 {
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		return order
	}
	keys := make([]string, n)
	for i, c := range matched.Candidates {
		keys[i] = candidateKey(c)
	}
	// load 取队列的真实在途量（running + queued），比轮询计数更准。
	// directMode 下队列未被使用，StatusOf 恒返回 0，least-queue 因此自然退化成
	// 轮询而不是静默退回配置顺序 —— 后者等于用户选的策略被忽略。
	load := func(i int) int {
		p := matched.Candidates[i].Provider
		st := s.qm.StatusOf(p.Name, p.MaxConcurrent, p.MaxPerSecond)
		return st.Running + st.Queued
	}
	return s.selector.Select(matched.RouteMatch, matched.Strategy, keys, stickyKey, load)
}

// candidateKey 候选的稳定身份，用作粘性映射的值。
// 用 provider/model 而不是下标：热重载改了 targets 顺序后，下标会指向另一个上游。
func candidateKey(c router.Candidate) string {
	return c.Provider.Name + "/" + c.TargetModel
}

// rememberSticky 记下「该前缀下次仍走这个候选」，保住上游侧的 prompt cache 前缀。
//
// failover 策略跳过：它本来就按配置顺序，粘性没有作用对象，
// 记了反而白占 LRU 容量、挤掉真正需要粘性的路由。
func (s *server) rememberSticky(matched *router.Match, stickyKey string, c router.Candidate) {
	if s.selector == nil || stickyKey == "" {
		return
	}
	if !balancer.UsesSticky(matched.Strategy) {
		return
	}
	s.selector.Remember(matched.RouteMatch, stickyKey, candidateKey(c))
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
	// httpAttemptNo 只对实际调用 proxy 的请求递增；与保留兼容语义的 attemptNo 分开。
	httpAttemptNo int
	allowRetry    bool
	reqLog        *metrics.RequestLog
	detail        *metrics.AttemptDetail
}

// forwardAttemptOutcome 单次尝试结果。
// buildErr 非空表示该候选无法构建上游请求，未发起任何网络请求；
// abandoned 表示已发起请求但按 failover 策略放弃，且未向客户端写入任何字节。
type forwardAttemptOutcome struct {
	buildErr  string
	abandoned bool
	// requestStarted 表示已实际调用上游 HTTP；构建/队列跳过不计入此编号。
	requestStarted bool
	trail          string // "provider:429/ratelimit"
	// breakerOutcome 本次尝试对熔断计数的影响，由调用方汇报给 breaker。
	breakerOutcome breaker.Outcome
	// freeAttempt 本次放弃不消耗 maxAttempts 额度，见 failoverDecision.freeAttempt。
	freeAttempt bool
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
	// 这里只兜住「部分候选是透传、部分不是」的情况：全部候选都不透传时 handle()
	// 已在 vision 之前返回了。buildErr 不自带前缀，终态由调用方统一加。
	if in.internal.Err != nil && !isPassthrough {
		setAttemptBuildError(in.detail, in.internal.Err)
		return forwardAttemptOutcome{buildErr: in.internal.Err.Error()}
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
			// 图片翻译只覆盖消息输入，system/tools 等 provider 原生扩展仍保留原值。
			var translated map[string]any
			if p.Format == "anthropic" {
				translated = converter.ToAnthropicBody(in.internal, targetModel)
				upstreamMap["messages"] = mergeTranslatedMessageContent(in.rawBody["messages"], translated["messages"])
			} else if p.Format == "openai" {
				translated = converter.ToOpenAIChatBody(in.internal, targetModel)
				upstreamMap["messages"] = mergeTranslatedMessageContent(in.rawBody["messages"], translated["messages"])
			} else {
				translated = converter.ToOpenAIResponsesBody(in.internal, targetModel)
				upstreamMap["input"] = mergeTranslatedResponsesInput(in.rawBody["input"], translated["input"])
			}
		}
	} else if p.Format == "anthropic" {
		upstreamMap, err = converter.ToAnthropicBodyChecked(in.internal, targetModel)
	} else if p.Format == "openai" {
		upstreamMap, err = converter.ToOpenAIChatBodyChecked(in.internal, targetModel)
	} else {
		upstreamMap, err = converter.ToOpenAIResponsesBodyChecked(in.internal, targetModel)
	}
	if err != nil {
		setAttemptBuildError(in.detail, err)
		return forwardAttemptOutcome{buildErr: err.Error()}
	}
	upstreamBody, err := json.Marshal(upstreamMap)
	if err != nil {
		buildErr := "上游请求体序列化失败: " + err.Error()
		if in.detail != nil {
			in.detail.Kind = "build_skip"
			in.detail.Outcome = "build_error"
			in.detail.Reason = "conversion_error"
			in.detail.Error = buildErr
		}
		// 这是构建阶段错误，尚未向客户端写入字节；允许后续候选继续尝试，
		// 并由外层在全部候选均不可构建时生成统一终态。
		return forwardAttemptOutcome{buildErr: buildErr}
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
			if in.detail != nil {
				in.detail.Kind = "queue_skip"
				in.detail.Error = err.Error()
			}
			logf(reqID, "  队列处理异常: %s", err.Error())
			if errors.Is(err, queue.ErrQueueTimeout) {
				if in.detail != nil {
					in.detail.Reason = "queue_timeout"
					in.detail.Outcome = "skipped"
				}
				// 排队超时且允许转移：换下一个候选，别把客户端 503 掉
				// 走访问器而不是直接 BoolOr：默认值只应写在 TransferOnXxx 一处，
				// 否则改默认值时这里会静默不同步
				if in.allowRetry && cfg.Failover.TransferOnQueueTimeout() && cfg.Failover.Enabled {
					if in.detail != nil {
						in.detail.Outcome = "transferred"
					}
					// 本地队列背压，不是上游故障
					return forwardAttemptOutcome{abandoned: true, trail: p.Name + ":queue_timeout", breakerOutcome: breaker.OutcomeIgnored}
				}
				in.reqLog.Error = err.Error()
				writeJSONError(w, http.StatusServiceUnavailable, "queue_timeout", err.Error())
			} else if r.Context().Err() != nil {
				if in.detail != nil {
					in.detail.Reason = "client_disconnected"
					in.detail.Outcome = "skipped"
				}
				// 客户端已断开，net/http 会忽略写入，无需也不应写响应
				in.reqLog.Error = err.Error()
				logf(reqID, "  客户端已断开，跳过响应")
			} else {
				if in.detail != nil {
					in.detail.Reason = "queue_error"
					in.detail.Outcome = "skipped"
				}
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
	if in.detail != nil {
		in.detail.Kind = "request"
		in.detail.AttemptNumber = in.httpAttemptNo
	}

	var (
		// abandonReason / abandonBreaker / abandonFree 由 ShouldRetry 回调写、
		// Forward 返回后读。与 OnUpstreamStatus 一样，回调与 Forward 同 goroutine
		// 同步执行，用普通变量即可；atomic.Value 只会多一层装箱和类型断言的失败路径。
		abandonReason string
		abandonFree   bool
		// 必须显式初始化成 OutcomeIgnored：Outcome 的零值是 OutcomeSuccess，
		// 万一回调没写入就用零值，会把一次放弃当成成功上报给熔断器。
		abandonBreaker = breaker.OutcomeIgnored
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
		OnUpstreamHeaders: func(code int, requestID string, retryAfter time.Duration) {
			if in.detail == nil {
				return
			}
			in.detail.UpstreamStatus = code
			in.detail.UpstreamRequestID = requestID
			in.detail.RetryAfterMs = retryAfter.Milliseconds()
		},
		OnErrorBody: func(body string, truncated bool) {
			if in.detail != nil {
				in.detail.ErrorBody = body
				in.detail.ErrorBodyTruncated = truncated
			}
		},
		OnResponseStarted: func() {
			if in.detail != nil {
				in.detail.ResponseStarted = true
			}
		},
	}
	if in.allowRetry {
		opts.ShouldRetry = func(upstreamCode int, retryAfter time.Duration, err error) bool {
			decision := failoverReason(&cfg.Failover, upstreamCode, retryAfter, err)
			if !decision.transfer {
				return false
			}
			abandonReason = decision.reason
			abandonBreaker = breakerOutcomeFor(upstreamCode, err)
			abandonFree = decision.freeAttempt
			return true
		}
	}

	forwardErr := proxy.Forward(opts)
	if errors.Is(forwardErr, proxy.ErrAttemptAbandoned) {
		// ErrAttemptAbandoned 只可能由上面的回调返回 true 触发，此时三个变量必已写入；
		// 兜底值仅防御 proxy 侧未来改动，不代表正常路径。
		reason := abandonReason
		if reason == "" {
			reason = "abandoned"
		}
		if in.detail != nil {
			in.detail.Outcome = "transferred"
			in.detail.Reason = reason
			in.detail.FreeAttempt = abandonFree
		}
		return forwardAttemptOutcome{
			abandoned:      true,
			requestStarted: true,
			trail:          p.Name + ":" + reason,
			breakerOutcome: abandonBreaker,
			freeAttempt:    abandonFree,
		}
	}

	upstreamStatus := attemptStatus
	if forwardErr != nil {
		in.reqLog.Error = forwardErr.Error()
		if in.detail != nil {
			in.detail.Error = forwardErr.Error()
		}
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
	breakerOutcome := breakerOutcomeFor(upstreamStatus, forwardErr)
	if in.detail != nil {
		if breakerOutcome == breaker.OutcomeSuccess {
			in.detail.Outcome = "success"
		} else {
			in.detail.Outcome = "final_error"
			in.detail.Reason = attemptFailureReason(upstreamStatus, forwardErr)
		}
	}
	return forwardAttemptOutcome{
		trail:          fmt.Sprintf("%s:%d", p.Name, status),
		requestStarted: true,
		breakerOutcome: breakerOutcome,
	}
}

func setAttemptBuildError(detail *metrics.AttemptDetail, err error) {
	if detail == nil {
		return
	}
	detail.Kind = "build_skip"
	detail.Outcome = "build_error"
	detail.Reason = "conversion_error"
	detail.Error = err.Error()
}

func attemptFailureReason(status int, err error) string {
	if errors.Is(err, proxy.ErrConversion) {
		return "conversion_error"
	}
	if status == http.StatusTooManyRequests {
		return "429/ratelimit"
	}
	if status >= 500 {
		return fmt.Sprintf("%d/server", status)
	}
	if status > 0 {
		return fmt.Sprintf("%d/error", status)
	}
	switch {
	case errors.Is(err, proxy.ErrStreamHeaderTimeout):
		return "header_timeout"
	case errors.Is(err, proxy.ErrRequestTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case err != nil:
		return "transport_error"
	default:
		return ""
	}
}

// failoverDecision 一次失败的转移判定结果。
type failoverDecision struct {
	// reason trail 里用的分类名，transfer 为 false 时无意义。
	reason string
	// transfer 是否放弃本候选、交给下一个。
	transfer bool
	// freeAttempt 本次失败不消耗 maxAttempts 额度。
	//
	// 仅用于「上游明确告知自己这段时间不可用」（429 且 Retry-After 超过
	// failover.maxRetryAfterMs），语义与熔断跳过一致：否则一个自曝限流的上游
	// 会挤掉本来还能试的健康候选。
	freeAttempt bool
}

// failoverReason 按 failover 配置判定一次失败能否转移。
func failoverReason(f *config.Failover, upstreamCode int, retryAfter time.Duration, err error) failoverDecision {
	if !f.Enabled {
		return failoverDecision{}
	}
	if upstreamCode == 0 {
		// 传输层失败：连接错误 / 超时
		switch {
		case errors.Is(err, proxy.ErrStreamHeaderTimeout):
			return failoverDecision{reason: "header_timeout", transfer: f.TransferOnStreamHeaderTimeout()}
		case errors.Is(err, proxy.ErrRequestTimeout), errors.Is(err, context.DeadlineExceeded):
			// 非流式整体超时单独一个开关，默认 false：已经等满一整个 timeout 预算，
			// 转移会让总耗时接近翻倍。不能并入 onTransportError —— 真正的连接失败
			// 几乎不耗时，是 failover 最该覆盖的场景，不该被一起关掉。
			return failoverDecision{reason: "timeout", transfer: f.TransferOnRequestTimeout()}
		default:
			return failoverDecision{reason: "transport_error", transfer: f.TransferOnTransportError()}
		}
	}
	switch {
	case upstreamCode == http.StatusTooManyRequests:
		if !f.TransferOnRateLimit() {
			return failoverDecision{}
		}
		// Retry-After 超过阈值：上游自己说了这段时间没戏，转移且不消耗额度。
		// cap 为 0 表示不设上限，此时所有 429 照常消耗额度。
		cap := f.RetryAfterCapMs()
		if cap > 0 && retryAfter > time.Duration(cap)*time.Millisecond {
			return failoverDecision{reason: "429/retry_after_too_long", transfer: true, freeAttempt: true}
		}
		return failoverDecision{reason: "429/ratelimit", transfer: true}
	case upstreamCode == http.StatusUnauthorized || upstreamCode == http.StatusForbidden:
		// 默认关闭：key 配错时转移只会连锁失败，且掩盖真实原因
		return failoverDecision{reason: fmt.Sprintf("%d/auth", upstreamCode), transfer: f.TransferOnAuthError()}
	case upstreamCode >= 500:
		return failoverDecision{reason: fmt.Sprintf("%d/server", upstreamCode), transfer: f.TransferOnServerError()}
	default:
		// 4xx 业务错误（400 参数错、404 模型不存在等）换 provider 也是同样结果
		return failoverDecision{}
	}
}

// breakerOutcomeFor 把一次尝试结果归类成熔断判据。
//
// 计入失败：传输错误、超时、5xx、以及「拿到 2xx 响应头之后才失败」（流中途断开、
// 流式活跃超时、响应体转换失败）。
// 不计入：429（上游正常限流，判成故障比不熔断更糟）、401/403（配置问题，熔断修不了
// 还会掩盖密钥过期）、客户端主动断开、普通 4xx（请求本身的问题）。
//
// err 在任何状态码下都要参与判定，不能只在 upstreamCode == 0 时看。
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
		// 2xx/3xx 也要看 err：上游给了成功响应头之后仍可能中途失败（流被掐断、
		// 流式活跃超时、响应体超限或转换失败）。只按状态码判会把这些记成成功，
		// 而 OutcomeSuccess 会清零失败计数并强制闭合，导致一个每次都在流中途
		// 崩掉的上游永远熔断不了；还会让粘性把整条会话钉在它上面。
		switch {
		case errors.Is(err, context.Canceled):
			// 客户端自己断开，与上游健康无关
			return breaker.OutcomeIgnored
		case err != nil:
			return breaker.OutcomeFailure
		default:
			return breaker.OutcomeSuccess
		}
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

// mergeTranslatedResponsesInput 是 Responses 直通模式的视觉翻译合并。
// Responses input 不是 Chat 的 messages 数组：function_call / function_call_output
// 与 message 同级。只替换 message.content，其他原生 item 完整保留，避免把
// Responses 的 item id、状态或扩展字段在“直通 + vision”时意外丢掉。
func mergeTranslatedResponsesInput(original, translated any) any {
	originalItems, originalOK := original.([]any)
	translatedItems, translatedOK := translated.([]any)
	if !originalOK || !translatedOK || len(originalItems) != len(translatedItems) {
		return translated
	}
	merged := make([]any, len(originalItems))
	for i := range originalItems {
		originalItem, originalOK := originalItems[i].(map[string]any)
		translatedItem, translatedOK := translatedItems[i].(map[string]any)
		if !originalOK || !translatedOK {
			return translated
		}
		originalType := originalItem["type"]
		if originalType == nil {
			originalType = "message"
		}
		translatedType := translatedItem["type"]
		if translatedType == nil {
			translatedType = "message"
		}
		if originalType != translatedType {
			return translated
		}
		if originalType != "message" {
			merged[i] = originalItem
			continue
		}
		if originalItem["role"] != translatedItem["role"] {
			return translated
		}
		item := make(map[string]any, len(originalItem))
		for key, value := range originalItem {
			item[key] = value
		}
		item["content"] = translatedItem["content"]
		merged[i] = item
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
	cs, heapAllocMB, sysMB := s.healthStats(time.Now())
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
		// stickyMappings 当前有效的 prompt cache 粘性映射数，
		// 用来判断粘性是否真的在生效（长期为 0 说明前缀太短或策略是 failover）
		"stickyMappings": s.stickyMappings(),
		"memory": map[string]any{
			"heapAllocMB": heapAllocMB,
			"sysMB":       sysMB,
		},
	}
	out, err := json.Marshal(health)
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

// healthStats 返回 /health 展示的缓存与内存概览。两类数据共用 10 秒 TTL，
// 并在独立锁下完成一次刷新，避免并发 /health 请求同时触发 SQLite 查询和 STW。
func (s *server) healthStats(now time.Time) (cache.Stats, uint64, uint64) {
	s.healthStatsMu.Lock()
	defer s.healthStatsMu.Unlock()

	if !s.healthStatsAt.IsZero() && now.Sub(s.healthStatsAt) < healthStatsCacheTTL {
		return s.healthCache, s.healthHeapMB, s.healthSysMB
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var cs cache.Stats
	if s.cache != nil {
		cs = s.cache.GetStats()
	}

	s.healthCache = cs
	s.healthHeapMB = m.HeapAlloc / 1024 / 1024
	s.healthSysMB = m.Sys / 1024 / 1024
	s.healthStatsAt = now
	return s.healthCache, s.healthHeapMB, s.healthSysMB
}

// stickyMappings 返回当前有效的 prompt cache 粘性映射数，未装 selector 时返回 0。
func (s *server) stickyMappings() int {
	if s.selector == nil {
		return 0
	}
	return s.selector.StickyMappings()
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
	out, _ := json.Marshal(payload)
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleProviderHealthCheck 触发健康检测。
//
// 带 ?provider= 时只检测该 provider，用于表格里的单行检测按钮：
// provider 一多时整表检测要等最慢的那个，而用户往往只关心刚改过的那一个。
// 响应结构与整表检测保持一致（都返回 providers 映射），前端只需合并同一份数据。
func (s *server) handleProviderHealthCheck(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	if name := r.URL.Query().Get("provider"); name != "" {
		status, ok := s.providerHealth.CheckProvider(r.Context(), cfg, s.httpClient, name)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "gateway_error", fmt.Sprintf("未找到 provider: %s", name))
			return
		}
		out, _ := json.Marshal(map[string]any{
			"providers": map[string]providerhealth.Status{name: status},
		})
		w.Header().Set("content-type", "application/json")
		w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	}

	statuses := s.providerHealth.CheckAll(r.Context(), cfg, s.httpClient)
	out, _ := json.Marshal(map[string]any{
		"providers": statuses,
	})
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// handleProviderModels 拉取指定 provider 上游的真实 /v1/models 列表。
//
// 用已落盘配置里的 apiKey 探测，不复用 providerhealth：那个只判断状态码、
// 不解析模型列表，且带结果缓存与并发节流，语义不匹配。这里是一次性透传查询，
// 供前端配置路由时选择目标模型。失败时返回 502 并带上上游状态码与消息，
// 前端据此弹提示、保留手动输入。
func (s *server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "gateway_error", "缺少 provider 参数")
		return
	}

	s.cfgMu.RLock()
	cfg := s.cfg
	provider, ok := cfg.Providers[name]
	s.cfgMu.RUnlock()
	if !ok || provider == nil {
		writeJSONError(w, http.StatusNotFound, "gateway_error", fmt.Sprintf("未找到 provider: %s", name))
		return
	}

	models, err := fetchUpstreamModels(r.Context(), provider, s.httpClient)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	out, _ := json.Marshal(map[string]any{
		"provider": name,
		"models":   models,
	})
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// fetchUpstreamModels 用 provider 配置的 apiKey 调上游 /v1/models，返回模型 id 列表。
func fetchUpstreamModels(parent context.Context, p *config.Provider, client *http.Client) ([]string, error) {
	endpoint := modelsEndpoint(p.BaseURL)
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("accept", "application/json")
	// 与转发路径同源的需求：部分上游按 User-Agent 做准入（实测 agentrouter.org 在
	// Go 默认 UA 下对 /v1/models 返回 401 unauthorized_client_error，换成配置的
	// UA 即返回完整列表）。不带上这个头，UA 门禁型 provider 的模型列表永远查不了。
	// 这里没有客户端请求可转发（网关自己发起），故只有「配了就用」一档。
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	if p.APIKey != "" {
		if p.Format == "anthropic" {
			req.Header.Set("x-api-key", p.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("authorization", "Bearer "+p.APIKey)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游不可达: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("上游返回 %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg = "上游鉴权失败（apiKey 无效或无权访问 /v1/models）"
		} else if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			msg = "上游不支持 /v1/models 端点"
		}
		return nil, errors.New(msg)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	// OpenAI 与 Anthropic 的 /v1/models 都是 {data:[{id:"..."}, ...]}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析上游模型列表失败: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// modelsEndpoint 由 baseURL 拼出标准 /v1/models 路径。
func modelsEndpoint(baseURL string) string {
	base := baseURL
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/models"
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	out, _ := json.Marshal(s.metrics.Metrics(time.Now()))
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := metrics.LogFilter{
		Provider:        q.Get("provider"),
		Model:           q.Get("model"),
		Status:          q.Get("status"),
		Stream:          q.Get("stream"),
		Query:           q.Get("q"),
		AttemptProvider: q.Get("attemptProvider"),
		AttemptStatus:   q.Get("attemptStatus"),
		AttemptOutcome:  q.Get("attemptOutcome"),
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
	out, _ := json.Marshal(map[string]any{
		"data":  logs,
		"limit": filter.Limit,
	})
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
			// 必须走 TargetList()：多候选写法下 route.Provider / route.Model 恒为空
			// （校验强制二者互斥），直接读会让所有 targets 路由输出空的 owned_by。
			targets := route.TargetList()
			entry := map[string]any{
				"id":           route.Match,
				"object":       "model",
				"owned_by":     targets[0].Provider,
				"target_model": targets[0].Model,
			}
			if len(targets) > 1 {
				// 多候选时额外列出全部目标，首个即默认起点（strategy 会改变实际顺序）
				list := make([]map[string]any, 0, len(targets))
				for _, t := range targets {
					list = append(list, map[string]any{"provider": t.Provider, "model": t.Model})
				}
				entry["targets"] = list
				if route.Strategy != "" {
					entry["strategy"] = route.Strategy
				}
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
	out, _ := json.Marshal(result)
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
//
// 开发模式优先实时拼装 src/ 下的模板与片段，而不是读产物 index.html：
// 页面已拆成 src/app/*.js.part，只读产物的话，改片段必须先跑一遍 `make web-html`
// 才能在浏览器里看到效果，热加载就等于半废。src/ 不存在时（比如挂载的是产物快照）
// 退回直接读 index.html，保持旧行为。
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	if s.webDevDir != "" {
		if webbuild.HasSources(s.webDevDir) {
			data, err = webbuild.Assemble(s.webDevDir)
		} else {
			data, err = os.ReadFile(filepath.Join(s.webDevDir, "index.html"))
		}
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
	lastMod := s.webSourcesModTime()
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
			current := s.webSourcesModTime()
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

// webSourcesModTime 取开发目录下参与页面构建的全部文件里最新的 mtime。
//
// 页面拆分后不能只盯 index.html：那是构建产物，改片段时它根本不变，
// 只盯它会让 SSE 永不触发、浏览器不刷新——热加载看着还在，实际已经废了。
// src/ 不存在时退回只看 index.html，与拆分前行为一致。
func (s *server) webSourcesModTime() time.Time {
	paths := []string{filepath.Join(s.webDevDir, "index.html")}
	if webbuild.HasSources(s.webDevDir) {
		if sources, err := webbuild.SourcePaths(s.webDevDir); err == nil {
			paths = sources
		}
	}
	var latest time.Time
	for _, path := range paths {
		if mod := fileModTime(path); mod.After(latest) {
			latest = mod
		}
	}
	return latest
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
		// 只校验磁盘上该 provider 名确有密钥可保留，不再用 SameProviderIdentity
		// 限制「url/format 必须没变」：编辑 provider 改了 url 或格式时，
		// 只要用户在弹窗里把 apiKey 留空（发回 sentinel），就沿用原密钥，
		// 不强制重新填写。若该 provider 是新加的、磁盘上没有已存密钥，照旧拒绝——
		// 那是凭空带 sentinel，不该静默吞成空密钥。
		oldProvider, exists := oldCfg.Providers[name]
		if !exists || oldProvider.APIKey == "" {
			writeJSONError(w, http.StatusBadRequest, "config_validation_error", fmt.Sprintf("providers.%s 无法保留已有 apiKey：磁盘上没有已存密钥", name))
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
	if s.selector != nil {
		// 路由状态按 route.match 归属：热重载删掉或改名的路由，
		// 其轮转计数器与粘性映射都该跟着走，否则 rr 会随历史 match 单调增长。
		activeRoutes := make(map[string]struct{}, len(newCfg.Routes))
		for _, route := range newCfg.Routes {
			activeRoutes[route.Match] = struct{}{}
		}
		s.selector.Reconcile(activeRoutes)
	}
	if s.metrics != nil {
		// 窗口变了才重建桶；SetWindow 内部会保留仍落在新窗口内的历史桶
		s.metrics.SetWindow(newCfg.Metrics.WindowMinutes)
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
		ConsecutiveFailures: cfg.Breaker.FailureThreshold(),
		OpenMs:              cfg.Breaker.CooldownMs(),
		HalfOpenProbes:      cfg.Breaker.ProbeLimit(),
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
