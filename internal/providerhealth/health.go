// Package providerhealth 提供上游 provider 的轻量健康检测。
package providerhealth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/config"
)

const checkTimeout = 5 * time.Second

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
	statuses map[string]Status
}

func NewChecker() *Checker {
	return &Checker{statuses: make(map[string]Status)}
}

// Snapshot 返回所有配置 provider 的最近检测状态。
func (c *Checker) Snapshot(cfg *config.Config) map[string]Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]Status, len(cfg.Providers))
	for name := range cfg.Providers {
		if st, ok := c.statuses[name]; ok {
			out[name] = st
		} else {
			out[name] = Status{Name: name, Status: "unchecked", Message: "未检测"}
		}
	}
	return out
}

// CheckAll 并发检测所有 provider，返回检测后的快照。
func (c *Checker) CheckAll(ctx context.Context, cfg *config.Config, client *http.Client) map[string]Status {
	type result struct {
		name   string
		status Status
	}
	sem := make(chan struct{}, 4)
	results := make(chan result, len(cfg.Providers))
	var wg sync.WaitGroup

	for name, p := range cfg.Providers {
		pCopy := *p
		if pCopy.Name == "" {
			pCopy.Name = name
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- result{name: pCopy.Name, status: errorStatus(pCopy.Name, "", ctx.Err().Error(), 0)}
				return
			}
			results <- result{name: pCopy.Name, status: checkOne(ctx, &pCopy, client)}
		}()
	}

	wg.Wait()
	close(results)

	c.mu.Lock()
	for res := range results {
		c.statuses[res.name] = res.status
	}
	c.mu.Unlock()
	return c.Snapshot(cfg)
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
