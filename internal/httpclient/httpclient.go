// Package httpclient 提供按 provider 代理配置复用连接池的 HTTP 客户端。
package httpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Pool 按规范化代理 URL 缓存 HTTP client；同一代理共享同一个连接池。
type Pool struct {
	mu            sync.Mutex
	defaultClient *http.Client
	clients       map[string]*http.Client
}

// NewPool 创建连接池。默认 client 继承全局环境代理，参数须与原网关 client 保持一致。
func NewPool() *Pool {
	return &Pool{
		defaultClient: newClient(http.ProxyFromEnvironment),
		clients:       make(map[string]*http.Client),
	}
}

// Default 返回走全局环境代理（或直连）的默认 client。
func (p *Pool) Default() *http.Client {
	return p.defaultClient
}

// For 返回指定代理对应的 client。空串始终返回 Default 的同一个指针。
func (p *Pool) For(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return p.Default(), nil
	}
	key, parsed, err := normalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.clients[key]; client != nil {
		return client, nil
	}
	client := newClient(http.ProxyURL(parsed))
	p.clients[key] = client
	return client, nil
}

// Reconcile 丢弃当前配置不再引用的代理 client，并关闭其空闲连接。
// active 的键可以是原始或规范化后的代理 URL；非法项由上层配置校验拦截，此处忽略。
func (p *Pool) Reconcile(active map[string]struct{}) {
	keep := make(map[string]struct{}, len(active))
	for proxyURL := range active {
		key, _, err := normalizeProxyURL(proxyURL)
		if err == nil {
			keep[key] = struct{}{}
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for key, client := range p.clients {
		if _, ok := keep[key]; ok {
			continue
		}
		client.CloseIdleConnections()
		delete(p.clients, key)
	}
}

func newClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               proxy,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func normalizeProxyURL(raw string) (string, *url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", nil, fmt.Errorf("无效代理 URL: %q", raw)
	}
	// 白名单与 config.validate 保持一致，含 socks5h：实测 Go 1.23 的 http.Transport
	// 对 socks5h 与 socks5 行为一致，都会去拨代理，不会报 unsupported protocol scheme。
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" && parsed.Scheme != "socks5h" {
		return "", nil, fmt.Errorf("不支持的代理 URL scheme: %q", parsed.Scheme)
	}
	// URL.String 会保留根路径的 /，但 HTTP 代理语义下它与无路径完全等价；统一它们
	// 避免 http://proxy:7890 和 http://proxy:7890/ 创建两份空闲连接池。
	if parsed.Path == "/" && parsed.RawPath == "" && !parsed.ForceQuery && parsed.RawQuery == "" && parsed.Fragment == "" {
		parsed.Path = ""
	}
	return parsed.String(), parsed, nil
}
