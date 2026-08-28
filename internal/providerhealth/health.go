// Package providerhealth 提供上游 provider 的轻量健康检测。
package providerhealth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-gateway/internal/config"
)

const (
	checkTimeout        = 5 * time.Second
	checkConcurrency    = 4
	checkResultCooldown = time.Second
)

// Status 是单个 provider 的最近健康检测结果。
type Status struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // unchecked | ok | warn | error
	Endpoint  string `json:"endpoint,omitempty"`
	HTTPCode  int    `json:"httpCode,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	Message   string `json:"message,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

// Checker 保存最近一次检测结果。
type Checker struct {
	mu       sync.RWMutex
	statuses map[string]cachedStatus
	sem      chan struct{}

	runMu           sync.Mutex
	inflight        *checkRun
	currentConfig   string
	lastFingerprint string
	lastCompleted   time.Time
	generation      atomic.Uint64
}

type cachedStatus struct {
	status      Status
	fingerprint string
}

type checkRun struct {
	fingerprint string
	generation  uint64
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewChecker() *Checker {
	return &Checker{
		statuses: make(map[string]cachedStatus),
		sem:      make(chan struct{}, checkConcurrency),
	}
}

// Snapshot 返回所有配置 provider 的最近检测状态。
func (c *Checker) Snapshot(cfg *config.Config) map[string]Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]Status, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		fingerprint := providerFingerprint(name, provider)
		if cached, ok := c.statuses[name]; ok && cached.fingerprint == fingerprint {
			out[name] = cached.status
		} else {
			out[name] = Status{Name: name, Status: "unchecked", Message: "未检测"}
		}
	}
	return out
}

// CheckAll 并发检测所有 provider，返回检测后的快照。
func (c *Checker) CheckAll(ctx context.Context, cfg *config.Config, client *http.Client) map[string]Status {
	fingerprint := configFingerprint(cfg)
	for {
		generation := c.generation.Load()
		c.runMu.Lock()
		if c.currentConfig == "" {
			c.currentConfig = fingerprint
		}
		if fingerprint != c.currentConfig {
			c.runMu.Unlock()
			return c.Snapshot(cfg)
		}
		if c.inflight != nil {
			done := c.inflight.done
			c.runMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return c.Snapshot(cfg)
			}
		}
		if fingerprint == c.lastFingerprint && time.Since(c.lastCompleted) < checkResultCooldown {
			c.runMu.Unlock()
			return c.Snapshot(cfg)
		}
		batches := (len(cfg.Providers) + checkConcurrency - 1) / checkConcurrency
		if batches < 1 {
			batches = 1
		}
		runCtx, cancel := context.WithTimeout(context.Background(), time.Duration(batches)*checkTimeout+time.Second)
		run := &checkRun{
			fingerprint: fingerprint,
			generation:  generation,
			ctx:         runCtx,
			cancel:      cancel,
			done:        make(chan struct{}),
		}
		c.inflight = run
		c.runMu.Unlock()

		cfgCopy := copyConfig(cfg)
		go c.executeRun(run, cfgCopy, client)
		select {
		case <-run.done:
			return c.Snapshot(cfg)
		case <-ctx.Done():
			return c.Snapshot(cfg)
		}
	}
}

func (c *Checker) executeRun(run *checkRun, cfg *config.Config, client *http.Client) {
	defer run.cancel()
	c.checkAll(run.ctx, cfg, client, run.generation)

	c.runMu.Lock()
	if c.inflight == run {
		if c.generation.Load() == run.generation {
			c.lastFingerprint = run.fingerprint
			c.lastCompleted = time.Now()
		}
		c.inflight = nil
	}
	close(run.done)
	c.runMu.Unlock()
}

func (c *Checker) checkAll(ctx context.Context, cfg *config.Config, client *http.Client, generation uint64) {
	type result struct {
		name        string
		status      Status
		fingerprint string
	}
	results := make(chan result, len(cfg.Providers))
	var wg sync.WaitGroup

	for name, p := range cfg.Providers {
		if p == nil {
			results <- result{name: name, status: errorStatus(name, "", "provider 配置为空", 0), fingerprint: providerFingerprint(name, nil)}
			continue
		}
		pCopy := *p
		if pCopy.Name == "" {
			pCopy.Name = name
		}
		fingerprint := providerFingerprint(name, &pCopy)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case c.sem <- struct{}{}:
				defer func() { <-c.sem }()
			case <-ctx.Done():
				results <- result{name: pCopy.Name, status: errorStatus(pCopy.Name, "", ctx.Err().Error(), 0), fingerprint: fingerprint}
				return
			}
			results <- result{name: pCopy.Name, status: checkOne(ctx, &pCopy, client), fingerprint: fingerprint}
		}()
	}

	wg.Wait()
	close(results)

	c.mu.Lock()
	if c.generation.Load() == generation {
		for res := range results {
			c.statuses[res.name] = cachedStatus{status: res.status, fingerprint: res.fingerprint}
		}
	}
	c.mu.Unlock()
}

// CheckProvider 只检测一个 provider，返回它的检测结果。
//
// 不复用 CheckAll 的去重与冷却：那套是为「整表刷新」设计的，
// 按整份配置指纹判定，而单点检测是用户对着某一行明确点的，
// 撞上冷却窗口就什么也不做、界面毫无反应，比多探一次更糟。
// 并发上限仍走同一个 sem，避免绕开全局节流。
//
// generation 判据保留：配置在探测期间被改掉时结果直接丢弃，
// 否则会把旧上游的结论写到新配置的名字上。
func (c *Checker) CheckProvider(ctx context.Context, cfg *config.Config, client *http.Client, name string) (Status, bool) {
	provider, ok := cfg.Providers[name]
	if !ok {
		return Status{}, false
	}

	generation := c.generation.Load()
	if provider == nil {
		status := errorStatus(name, "", "provider 配置为空", 0)
		c.storeStatus(name, status, providerFingerprint(name, nil), generation)
		return status, true
	}

	pCopy := *provider
	if pCopy.Name == "" {
		pCopy.Name = name
	}
	fingerprint := providerFingerprint(name, &pCopy)

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		// 没拿到槽位就退出：不写缓存，避免把「网关侧排队超时」
		// 记成上游异常，那会让用户以为上游挂了。
		return Status{}, false
	}

	status := checkOne(ctx, &pCopy, client)
	c.storeStatus(name, status, fingerprint, generation)
	return status, true
}

// storeStatus 在 generation 未变时写入检测结果。
func (c *Checker) storeStatus(name string, status Status, fingerprint string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation.Load() != generation {
		return
	}
	c.statuses[name] = cachedStatus{status: status, fingerprint: fingerprint}
}

// InvalidateChanged 清理被删除或身份字段发生变化的 provider 健康状态。
func (c *Checker) InvalidateChanged(oldCfg *config.Config, newCfg *config.Config) {
	oldFingerprint := configFingerprint(oldCfg)
	newFingerprint := configFingerprint(newCfg)
	fingerprintChanged := oldFingerprint != newFingerprint
	c.mu.Lock()
	if fingerprintChanged {
		c.generation.Add(1)
	}
	for name, cached := range c.statuses {
		provider, ok := newCfg.Providers[name]
		if !ok || cached.fingerprint != providerFingerprint(name, provider) {
			delete(c.statuses, name)
		}
	}
	c.mu.Unlock()

	c.runMu.Lock()
	c.currentConfig = newFingerprint
	var cancel context.CancelFunc
	if fingerprintChanged {
		c.lastFingerprint = ""
		c.lastCompleted = time.Time{}
		if c.inflight != nil {
			cancel = c.inflight.cancel
		}
	}
	c.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func copyConfig(cfg *config.Config) *config.Config {
	copyCfg := *cfg
	copyCfg.Providers = make(map[string]*config.Provider, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		if provider == nil {
			copyCfg.Providers[name] = nil
			continue
		}
		providerCopy := *provider
		copyCfg.Providers[name] = &providerCopy
	}
	return &copyCfg
}

func configFingerprint(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintln(h, providerFingerprint(name, cfg.Providers[name]))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func providerFingerprint(name string, provider *config.Provider) string {
	h := sha256.New()
	if provider == nil {
		fmt.Fprintf(h, "%s\x00<nil>", name)
	} else {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", name, provider.BaseURL, provider.Format, provider.APIKey)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func checkOne(parent context.Context, p *config.Provider, client *http.Client) Status {
	endpoint := modelsEndpoint(p.BaseURL)
	ctx, cancel := context.WithTimeout(parent, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errorStatus(p.Name, endpoint, err.Error(), 0)
	}
	req.Header.Set("accept", "application/json")
	if p.APIKey != "" {
		if p.Format == "anthropic" {
			req.Header.Set("x-api-key", p.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("authorization", "Bearer "+p.APIKey)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return errorStatus(p.Name, endpoint, err.Error(), latencyMs)
	}
	defer resp.Body.Close()

	st := Status{
		Name:      p.Name,
		Endpoint:  endpoint,
		HTTPCode:  resp.StatusCode,
		LatencyMs: latencyMs,
		CheckedAt: time.Now().Format(time.RFC3339),
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		st.Status = "ok"
		st.Message = "可用"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		st.Status = "error"
		st.Message = "鉴权失败"
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		st.Status = "warn"
		st.Message = "检测端点不可用，但上游可达"
	case resp.StatusCode >= 500:
		st.Status = "error"
		st.Message = "上游服务异常"
	default:
		st.Status = "warn"
		st.Message = http.StatusText(resp.StatusCode)
		if st.Message == "" {
			st.Message = "状态未知"
		}
	}
	return st
}

func errorStatus(name, endpoint, message string, latencyMs int64) Status {
	return Status{
		Name:      name,
		Status:    "error",
		Endpoint:  endpoint,
		LatencyMs: latencyMs,
		Message:   message,
		CheckedAt: time.Now().Format(time.RFC3339),
	}
}

func modelsEndpoint(baseURL string) string {
	base := baseURL
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/models"
}
