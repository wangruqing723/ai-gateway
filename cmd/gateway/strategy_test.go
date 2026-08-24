package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/balancer"
	"ai-gateway/internal/config"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/queue"
)

// newStrategyTestServer 在 failover 测试网关上补一个 selector。
//
// newFailoverTestServer 故意不装 selector（阶段一的行为就是按配置顺序），
// 策略测试必须显式装上，否则 candidateOrder 会走 nil 分支返回配置顺序，
// 测试就会「通过」得毫无意义。
func newStrategyTestServer(providers map[string]*config.Provider, route config.Route, client *http.Client, failover config.Failover) *server {
	srv := newFailoverTestServer(providers, route, client, failover)
	srv.selector = balancer.New()
	return srv
}

func strategyFailoverDisabled() config.Failover {
	f := defaultTestFailover()
	f.Enabled = false
	return f
}

// shortAnthropicReq 前缀短于 stickyMinPrefixLen，StickyKey 返回空，
// 因此这些请求纯靠策略排序，不受粘性干扰。
const shortAnthropicReq = `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// longPrefixRequest 构造一个前缀足够长的请求，用来触发 prompt cache 粘性。
func longPrefixRequest(t *testing.T) string {
	t.Helper()
	system := strings.Repeat("这是一段用于测试 prompt cache 粘性的长系统提示。", 20)
	body, err := json.Marshal(map[string]any{
		"model":      "client-model",
		"max_tokens": 16,
		"system":     system,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	// 前置自检：前缀不够长的话粘性根本不会启用，后面的断言会变成假阳性。
	if balancer.StickyKey(system, []any{map[string]any{"role": "user", "content": "hi"}}) == "" {
		t.Fatal("测试用前缀太短，StickyKey 返回空，粘性断言无效")
	}
	return string(body)
}

// countingUpstream 返回一台记命中次数的 anthropic 上游。
func countingUpstream(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
}

// twoCandidateRoute 组一条双候选路由，候选顺序固定为 a → b。
func twoCandidateRoute(strategy, aURL, bURL string) (map[string]*config.Provider, config.Route) {
	providers := map[string]*config.Provider{
		"a": {Name: "a", BaseURL: aURL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
		"b": {Name: "b", BaseURL: bURL, Format: "anthropic", MaxConcurrent: 1, MaxQueueWait: 1000},
	}
	route := config.Route{Match: "*", Strategy: strategy, Targets: []config.Target{
		{Provider: "a", Model: "model-a"},
		{Provider: "b", Model: "model-b"},
	}}
	return providers, route
}

// TestStrategyRoundRobinDistributesAcrossCandidates round-robin 应把请求均分到两个候选。
func TestStrategyRoundRobinDistributesAcrossCandidates(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	const total = 6
	for i := 0; i < total; i++ {
		recorder := postInference(srv, "/v1/messages", shortAnthropicReq)
		if recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status/body = %d/%s", i+1, recorder.Code, recorder.Body.String())
		}
	}
	if aHits.Load() != total/2 || bHits.Load() != total/2 {
		t.Fatalf("命中次数 a=%d b=%d, 期望各 %d", aHits.Load(), bHits.Load(), total/2)
	}
}

// TestStrategyFailoverKeepsConfigOrder failover 策略不轮转，全部请求都落在首个候选。
func TestStrategyFailoverKeepsConfigOrder(t *testing.T) {
	for _, strategy := range []string{balancer.StrategyFailover, ""} {
		name := strategy
		if name == "" {
			name = "空字符串"
		}
		t.Run(name, func(t *testing.T) {
			var aHits, bHits atomic.Int32
			a := countingUpstream(&aHits)
			defer a.Close()
			b := countingUpstream(&bHits)
			defer b.Close()

			providers, route := twoCandidateRoute(strategy, a.URL, b.URL)
			srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

			for i := 0; i < 4; i++ {
				if recorder := postInference(srv, "/v1/messages", shortAnthropicReq); recorder.Code != http.StatusOK {
					t.Fatalf("第 %d 次 status/body = %d/%s", i+1, recorder.Code, recorder.Body.String())
				}
			}
			if aHits.Load() != 4 || bHits.Load() != 0 {
				t.Fatalf("命中次数 a=%d b=%d, 期望 4/0（按配置顺序，不轮转）", aHits.Load(), bHits.Load())
			}
		})
	}
}

// TestStrategyLeastQueueDegradesToRotationInDirectMode 直通模式没有队列，
// StatusOf 恒返回 0，least-queue 应退化成轮询而不是静默退回配置顺序。
func TestStrategyLeastQueueDegradesToRotationInDirectMode(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyLeastQueue, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	const total = 6
	for i := 0; i < total; i++ {
		if recorder := postInference(srv, "/v1/messages", shortAnthropicReq); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status/body = %d/%s", i+1, recorder.Code, recorder.Body.String())
		}
	}
	if aHits.Load() != total/2 || bHits.Load() != total/2 {
		t.Fatalf("命中次数 a=%d b=%d, 期望各 %d（负载相同按轮转打散）", aHits.Load(), bHits.Load(), total/2)
	}
}

// TestStrategyRoundRobinWithFailoverDisabled 关掉 failover 后策略仍生效，
// 但选中的候选失败就直接返回，不往下转移。
func TestStrategyRoundRobinWithFailoverDisabled(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}))
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), strategyFailoverDisabled())

	// 第一次轮到 a：502 直接透传给客户端，不转移到 b。
	first := postInference(srv, "/v1/messages", shortAnthropicReq)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("第 1 次 status/body = %d/%s, 期望 502 直接透传", first.Code, first.Body.String())
	}
	if bHits.Load() != 0 {
		t.Fatalf("failover 关闭却转移到了 b（b 命中 %d 次）", bHits.Load())
	}

	// 第二次轮到 b：策略照常轮转。
	second := postInference(srv, "/v1/messages", shortAnthropicReq)
	if second.Code != http.StatusOK {
		t.Fatalf("第 2 次 status/body = %d/%s, 期望轮到 b 后 200", second.Code, second.Body.String())
	}
	if got := second.Header().Get("x-ai-gateway-provider"); got != "b" {
		t.Errorf("x-ai-gateway-provider = %q, 期望 b", got)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("命中次数 a=%d b=%d, 期望各 1", aHits.Load(), bHits.Load())
	}
}

// TestStrategyStickyKeepsSessionOnOneTarget 长前缀请求应始终落在同一个候选，
// 否则上游侧 prompt cache 每次都作废。
func TestStrategyStickyKeepsSessionOnOneTarget(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	body := longPrefixRequest(t)
	const total = 4
	for i := 0; i < total; i++ {
		recorder := postInference(srv, "/v1/messages", body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status/body = %d/%s", i+1, recorder.Code, recorder.Body.String())
		}
		// 首次按轮转落在 a，之后应被粘性钉住。
		if got := recorder.Header().Get("x-ai-gateway-provider"); got != "a" {
			t.Fatalf("第 %d 次 provider = %q, 期望始终为 a", i+1, got)
		}
	}
	if aHits.Load() != total || bHits.Load() != 0 {
		t.Fatalf("命中次数 a=%d b=%d, 期望 %d/0（粘性钉住同一目标）", aHits.Load(), bHits.Load(), total)
	}
	if got := srv.selector.StickyMappings(); got != 1 {
		t.Errorf("粘性映射数 = %d, 期望 1（同一前缀只占一条）", got)
	}
}

// TestStrategyStickyIgnoredForShortPrefix 短前缀进不了上游缓存，不该占粘性容量。
func TestStrategyStickyIgnoredForShortPrefix(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	for i := 0; i < 2; i++ {
		if recorder := postInference(srv, "/v1/messages", shortAnthropicReq); recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status/body = %d/%s", i+1, recorder.Code, recorder.Body.String())
		}
	}
	if got := srv.selector.StickyMappings(); got != 0 {
		t.Errorf("粘性映射数 = %d, 期望 0（短前缀不记粘性）", got)
	}
}

// TestStrategyStickyNotRecordedUnderFailoverStrategy failover 策略不查粘性，
// 也就不该记，否则白占 LRU 容量挤掉真正需要的路由。
func TestStrategyStickyNotRecordedUnderFailoverStrategy(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyFailover, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	if recorder := postInference(srv, "/v1/messages", longPrefixRequest(t)); recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	if got := srv.selector.StickyMappings(); got != 0 {
		t.Errorf("粘性映射数 = %d, 期望 0（failover 策略不记粘性）", got)
	}
}

// TestStrategyStickyRebindsAfterFailover 粘性目标坏了以后应重新绑到转移成功的候选，
// 而不是把整条会话一直钉在坏上游上。
func TestStrategyStickyRebindsAfterFailover(t *testing.T) {
	var aHits, bHits atomic.Int32
	var aDown atomic.Bool
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aHits.Add(1)
		if aDown.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"down"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())
	body := longPrefixRequest(t)

	// 第 1 次：a 健康，绑定到 a。
	if recorder := postInference(srv, "/v1/messages", body); recorder.Header().Get("x-ai-gateway-provider") != "a" {
		t.Fatalf("第 1 次 provider = %q, 期望 a", recorder.Header().Get("x-ai-gateway-provider"))
	}

	// 第 2 次：a 挂了，粘性仍先试 a，失败后转移到 b 并改绑 b。
	aDown.Store(true)
	second := postInference(srv, "/v1/messages", body)
	if second.Code != http.StatusOK {
		t.Fatalf("第 2 次 status/body = %d/%s, 期望转移后 200", second.Code, second.Body.String())
	}
	if got := second.Header().Get("x-ai-gateway-provider"); got != "b" {
		t.Fatalf("第 2 次 provider = %q, 期望转移到 b", got)
	}

	// 第 3 次：应直接走 b，不再碰 a。
	aBefore := aHits.Load()
	third := postInference(srv, "/v1/messages", body)
	if got := third.Header().Get("x-ai-gateway-provider"); got != "b" {
		t.Fatalf("第 3 次 provider = %q, 期望仍为 b（已改绑）", got)
	}
	if aHits.Load() != aBefore {
		t.Errorf("第 3 次又打到了坏上游 a（命中从 %d 变成 %d）", aBefore, aHits.Load())
	}
	if got := srv.selector.StickyMappings(); got != 1 {
		t.Errorf("粘性映射数 = %d, 期望 1（改绑而不是新增）", got)
	}
}

// TestStrategyLeastQueuePicksIdleCandidate 队列模式下 least-queue 应挑在途量更小的候选。
//
// 用一个不返回的上游把 a 的唯一并发槽占住，此时 a 的 running=1、b 的为 0，
// 新请求必须落到 b。
func TestStrategyLeastQueuePicksIdleCandidate(t *testing.T) {
	var bHits atomic.Int32
	block := make(chan struct{})
	// release 幂等：正常路径在断言后主动放行，断言失败时由 defer 兜底，
	// 两条路径都可能触发，直接 close 两次会 panic。
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	defer release()
	aEntered := make(chan struct{}, 1)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case aEntered <- struct{}{}:
		default:
		}
		<-block
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyLeastQueue, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())
	// 队列模式才有 running/queued 可比；直通模式下负载恒为 0。
	srv.cfg.DirectMode = false

	// 先发一个卡住的请求占住 a 的并发槽。
	done := make(chan struct{})
	go func() {
		defer close(done)
		postInference(srv, "/v1/messages", shortAnthropicReq)
	}()
	<-aEntered

	// 此刻 a 在途 1、b 在途 0，least-queue 必须选 b。
	recorder := postInference(srv, "/v1/messages", shortAnthropicReq)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("x-ai-gateway-provider"); got != "b" {
		t.Fatalf("provider = %q, 期望负载更低的 b", got)
	}
	if bHits.Load() != 1 {
		t.Errorf("b 命中 %d 次, 期望 1", bHits.Load())
	}

	release()
	<-done
}

// TestStrategyRequestLogRecordsActualProvider 策略重排后日志必须记真实使用的候选，
// 不能记配置里的第一个。
func TestStrategyRequestLogRecordsActualProvider(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	// 第一次落 a，第二次轮到 b。
	postInference(srv, "/v1/messages", shortAnthropicReq)
	postInference(srv, "/v1/messages", shortAnthropicReq)

	logs := srv.metrics.Logs(metrics.LogFilter{Limit: 10})
	if len(logs) != 2 {
		t.Fatalf("请求日志 = %d 条, 期望 2", len(logs))
	}
	got := map[string]string{}
	for _, entry := range logs {
		got[entry.Provider] = entry.TargetModel
	}
	if got["a"] != "model-a" {
		t.Errorf("a 的 TargetModel = %q, 期望 model-a", got["a"])
	}
	if got["b"] != "model-b" {
		t.Errorf("b 的 TargetModel = %q, 期望 model-b（轮转到 b 时日志不能还记 model-a）", got["b"])
	}
}

// TestHealthExposesStickyMappings /health 带出粘性映射数，
// 让人能判断粘性是否真的在生效（长期为 0 说明前缀太短或策略是 failover）。
func TestHealthExposesStickyMappings(t *testing.T) {
	var aHits, bHits atomic.Int32
	a := countingUpstream(&aHits)
	defer a.Close()
	b := countingUpstream(&bHits)
	defer b.Close()

	providers, route := twoCandidateRoute(balancer.StrategyRoundRobin, a.URL, b.URL)
	srv := newStrategyTestServer(providers, route, a.Client(), defaultTestFailover())

	if recorder := postInference(srv, "/v1/messages", longPrefixRequest(t)); recorder.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
	}

	var health struct {
		StickyMappings int `json:"stickyMappings"`
	}
	recorder := httptest.NewRecorder()
	srv.handle(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.test/health", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatalf("解析 /health 失败: %v, body=%s", err, recorder.Body.String())
	}
	if health.StickyMappings != 1 {
		t.Errorf("stickyMappings = %d, 期望 1", health.StickyMappings)
	}

	// 未装 selector 时不能 panic，取 0。
	off := newFailoverTestServer(providers, route, a.Client(), defaultTestFailover())
	recorder = httptest.NewRecorder()
	off.handle(recorder, httptest.NewRequest(http.MethodGet, "http://gateway.test/health", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatalf("解析 /health 失败: %v", err)
	}
	if health.StickyMappings != 0 {
		t.Errorf("未装 selector 时 stickyMappings = %d, 期望 0", health.StickyMappings)
	}
}

// strategyReloadYAML 生成一份只有一条路由的合法配置，match 可换，用于热重载对照。
func strategyReloadYAML(match string) string {
	return fmt.Sprintf(`host: "127.0.0.1"
port: 7789
providers:
  primary:
    baseUrl: "https://a.example.com"
    apiKey: ""
    format: openai
    maxConcurrent: 5
    maxPerSecond: 0
    maxQueueWait: 30000
routes:
  - match: %q
    provider: primary
    model: upstream
`, match)
}

// seedSelector 给指定路由灌一条轮转计数器和一条粘性映射。
func seedSelector(t *testing.T, sel *balancer.Selector, routeKey string) {
	t.Helper()
	keys := []string{"primary/upstream", "backup/upstream"}
	sel.Select(routeKey, balancer.StrategyRoundRobin, keys, "sticky-1", nil)
	sel.Remember(routeKey, "sticky-1", keys[0])
}

// TestApplyRuntimeConfigReconcilesSelector 热重载删掉或改名路由后，该路由的
// 轮转计数器与粘性映射必须一起丢掉。
//
// rr 尤其重要：粘性映射有 TTL 和 LRU 兜底，rr 两者都没有，
// 不清理就会随「历史上出现过的 match」单调增长。
func TestApplyRuntimeConfigReconcilesSelector(t *testing.T) {
	oldCfg, err := config.DecodeAndValidate([]byte(strategyReloadYAML("claude-*")))
	if err != nil {
		t.Fatalf("解析旧配置失败: %v", err)
	}
	newCfg, err := config.DecodeAndValidate([]byte(strategyReloadYAML("gpt-*")))
	if err != nil {
		t.Fatalf("解析新配置失败: %v", err)
	}

	sel := balancer.New()
	seedSelector(t, sel, "claude-*")
	if got := sel.RouteCounters(); got != 1 {
		t.Fatalf("灌入后 RouteCounters = %d, 期望 1", got)
	}
	if got := sel.StickyMappings(); got != 1 {
		t.Fatalf("灌入后 StickyMappings = %d, 期望 1", got)
	}

	srv := &server{
		cfg: oldCfg, revision: "old-revision",
		listenHost: oldCfg.Host, listenPort: oldCfg.Port,
		qm: queue.NewManager(), selector: sel,
	}
	if restart := srv.applyRuntimeConfig(newCfg, "next-revision"); len(restart) != 0 {
		t.Fatalf("unexpected restartRequired: %#v", restart)
	}

	if got := sel.RouteCounters(); got != 0 {
		t.Errorf("改名后 RouteCounters = %d, 期望 0（旧 match 的计数器应被丢掉）", got)
	}
	if got := sel.StickyMappings(); got != 0 {
		t.Errorf("改名后 StickyMappings = %d, 期望 0（旧 match 的粘性应被丢掉）", got)
	}
}

// TestApplyRuntimeConfigKeepsSelectorStateForLiveRoute 只要 match 还在，
// 热重载（例如只改了 provider 的并发上限）不能把这条路由的会话粘性冲掉，
// 否则每次保存配置都会让所有会话的上游侧 prompt cache 作废。
func TestApplyRuntimeConfigKeepsSelectorStateForLiveRoute(t *testing.T) {
	oldCfg, err := config.DecodeAndValidate([]byte(strategyReloadYAML("claude-*")))
	if err != nil {
		t.Fatalf("解析旧配置失败: %v", err)
	}
	newCfg, err := config.DecodeAndValidate([]byte(strategyReloadYAML("claude-*")))
	if err != nil {
		t.Fatalf("解析新配置失败: %v", err)
	}
	newCfg.Providers["primary"].MaxConcurrent = 9

	sel := balancer.New()
	seedSelector(t, sel, "claude-*")

	srv := &server{
		cfg: oldCfg, revision: "old-revision",
		listenHost: oldCfg.Host, listenPort: oldCfg.Port,
		qm: queue.NewManager(), selector: sel,
	}
	if restart := srv.applyRuntimeConfig(newCfg, "next-revision"); len(restart) != 0 {
		t.Fatalf("unexpected restartRequired: %#v", restart)
	}

	if got := sel.RouteCounters(); got != 1 {
		t.Errorf("RouteCounters = %d, 期望 1（match 未变，计数器应保留）", got)
	}
	if got := sel.StickyMappings(); got != 1 {
		t.Errorf("StickyMappings = %d, 期望 1（match 未变，粘性应保留）", got)
	}
}
