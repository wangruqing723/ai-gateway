// Command ai-gateway-go 是 Node 版 gateway.js 的 Go PoC。
//
// 已打通：配置加载、路由匹配、per-provider 并发/限速队列、流式/非流式转发、context 超时、健康检查。
// 待补全（标注 TODO）：converter 三格式互转、vision 图片识别、SQLite 缓存。
// PoC 当前可完整代理 mimo 实际使用的 anthropic→anthropic 透传链路。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // 嵌入时区数据库，使 distroless 等无 tzdata 的镜像也能解析 Asia/Shanghai

	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/router"
)

var reqCounter uint64

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gateway] %s\n", err)
		os.Exit(1)
	}

	qm := queue.NewManager()

	// 复用连接池的 HTTP 客户端；流式不设整体 Timeout（靠 ctx 活跃超时控制）
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	srv := &server{cfg: cfg, qm: qm, httpClient: httpClient}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handle)

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	printBanner(cfg)

	httpServer := &http.Server{Addr: addr, Handler: mux}
	if err := httpServer.ListenAndServe(); err != nil {
		logSystem("服务器错误: %s", err)
		os.Exit(1)
	}
}

type server struct {
	cfg        *config.Config
	qm         *queue.Manager
	httpClient *http.Client
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.Split(r.URL.Path, "?")[0]

	// HEAD 健康探测
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// /health 端点
	if urlPath == "/health" && r.Method == http.MethodGet {
		s.handleHealth(w)
		return
	}

	clientFormat := converter.DetectClientFormat(urlPath)
	if clientFormat == "" {
		writeJSONError(w, http.StatusNotFound, "gateway_error", "未知端点: "+urlPath)
		return
	}

	reqID := nextReqID()
	start := time.Now()
	logf(reqID, "→ %s %s [%s]", r.Method, urlPath, clientFormat)

	raw, err := readBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "gateway_error", "读取请求失败: "+err.Error())
		return
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "gateway_error", "请求体 JSON 解析失败: "+err.Error())
		return
	}

	model, _ := body["model"].(string)

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

	// 路由匹配
	matched := router.MatchRoute(model, s.cfg)
	if matched == nil {
		writeJSONError(w, http.StatusBadRequest, "gateway_error", fmt.Sprintf("没有匹配 model %q 的路由规则", model))
		return
	}
	p := matched.Provider
	p.APIKey = router.ResolveAPIKey(p, r.Header)

	logf(reqID, "→ %s → %s [%d msgs, stream=%v]", model, matched.TargetModel, len(internal.Messages), internal.Stream)

	// TODO(vision): 若 matched.VisionProvider != nil 且消息含图片，先做图片识别翻译。

	// 内部格式 → 上游 provider 请求体
	var upstreamMap map[string]any
	if p.Format == "anthropic" {
		upstreamMap = converter.ToAnthropicBody(internal, matched.TargetModel)
	} else {
		upstreamMap = converter.ToOpenAIChatBody(internal, matched.TargetModel)
	}
	upstreamBody, err := json.Marshal(upstreamMap)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "上游请求体序列化失败: "+err.Error())
		return
	}

	// 通过队列获取执行槽位（并发 + 限速 + 等待超时）
	release, err := s.qm.Acquire(r.Context(), p.Name, p.MaxConcurrent, p.MaxPerSecond, p.MaxQueueWait)
	if err != nil {
		logf(reqID, "  队列处理异常: %s", err.Error())
		if err == queue.ErrQueueTimeout {
			writeJSONError(w, http.StatusServiceUnavailable, "queue_timeout", err.Error())
		}
		return
	}
	defer release()

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
		TimeoutMs:             s.cfg.Timeout,
		StreamActivityTimeout: s.cfg.StreamActivityTimeout,
		HTTPClient:            s.httpClient,
	}
	if err := proxy.Forward(opts); err != nil {
		logf(reqID, "  转发结束（异常）: %s", err.Error())
	}
}

func (s *server) handleHealth(w http.ResponseWriter) {
	queues := make(map[string]queue.Status)
	for name, p := range s.cfg.Providers {
		queues[name] = s.qm.StatusOf(name, p.MaxConcurrent, p.MaxPerSecond)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	health := map[string]any{
		"status":  "ok",
		"timeout": s.cfg.Timeout,
		"queues":  queues,
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

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	if n > 99999 {
		atomic.StoreUint64(&reqCounter, 1)
		n = 1
	}
	return fmt.Sprintf("r%05d", n)
}
