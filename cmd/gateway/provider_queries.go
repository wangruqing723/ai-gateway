package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/keypool"
	"ai-gateway/internal/providerbalance"
	"ai-gateway/internal/providermodels"
)

const (
	maxProviderQueryRequestBytes = 64 << 10
	providerCatalogBudget        = 30 * time.Second
	providerBalanceBatchBudget   = 60 * time.Second
)

// Provider 查询与模型调用分离：余额不更新健康、密钥冷却或调用统计。
// 锁顺序为 cfgMu → providerQueryMu → 管理器内部锁，网络回调不持有这些锁。

func setProviderQueryHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func isProviderQueryPath(path string) bool {
	return path == "/api/providers/models" || path == "/api/providers/models/refresh" || path == "/api/providers/balances" || path == "/api/providers/balances/refresh"
}

func (s *server) initProviderQueries() *providerbalance.Manager {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	s.providerQueryMu.Lock()
	defer s.providerQueryMu.Unlock()
	if s.providerQueriesStopped {
		return nil
	}
	if s.providerQueryContext == nil {
		s.providerQueryContext, s.providerQueryCancel = context.WithCancel(context.Background())
	}
	if s.providerBalances == nil {
		s.providerBalances = providerbalance.New()
		s.providerBalances.Reconcile(s.providerBalanceSources(s.cfg))
	}
	return s.providerBalances
}

// reconcileProviderQueriesLocked 随配置发布协调查询身份，调用方持有 cfgMu 写锁。
func (s *server) reconcileProviderQueriesLocked(cfg *config.Config) {
	connections := make(map[string]string)
	if cfg != nil {
		for name, p := range cfg.Providers {
			if p != nil && p.AuthType == "chatgptOAuth" {
				connections[name] = providerCatalogConnection(p)
			}
		}
	}
	s.modelCatalogCache.Reconcile(connections)
	s.providerQueryMu.Lock()
	defer s.providerQueryMu.Unlock()
	if s.providerBalances != nil && !s.providerQueriesStopped {
		s.providerBalances.Reconcile(s.providerBalanceSources(cfg))
	}
}

func (s *server) stopProviderQueries() {
	s.providerQueryMu.Lock()
	s.providerQueriesStopped = true
	cancel, balances := s.providerQueryCancel, s.providerBalances
	s.providerQueryMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if balances != nil {
		balances.Close()
	}
}

func (s *server) providerQueryContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	s.initProviderQueries()
	s.providerQueryMu.Lock()
	root, stopped := s.providerQueryContext, s.providerQueriesStopped
	s.providerQueryMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, timeout)
	if root == nil || stopped {
		cancel()
		return ctx, cancel
	}
	stop := context.AfterFunc(root, cancel)
	return ctx, func() { stop(); cancel() }
}

func (s *server) providerBalanceSources(cfg *config.Config) map[string]providerbalance.Source {
	sources := make(map[string]providerbalance.Source)
	if cfg == nil {
		return sources
	}
	for name, provider := range cfg.Providers {
		if provider == nil || provider.Balance == nil {
			continue
		}
		p := cloneQueryProvider(provider)
		p.Name = name
		mode := p.Balance.EffectiveSource()
		query := providerbalance.Query{Type: p.Balance.Type, Source: mode, BaseURL: p.BalanceBaseURL(), UserID: p.Balance.UserID, KeyID: p.Balance.APIKeyID, QuotaPerUnit: p.Balance.QuotaPerUnit, UserAgent: p.UserAgent}
		if mode == "account" {
			query.Token = p.Balance.AccessToken
		} else if p.UsesKeyList() {
			for _, key := range p.APIKeys {
				if key.ID == query.KeyID {
					query.Token = key.Value
					break
				}
			}
		} else {
			query.Token = p.APIKey
		}
		sourceKey := providerBalanceIdentity(p, query)
		providerName := name
		sources[name] = providerbalance.Source{
			Key: sourceKey, Type: query.Type, Mode: mode, Enabled: p.IsEnabled(), Interval: time.Duration(p.Balance.IntervalMinutes()) * time.Minute,
			Fetch: func(ctx context.Context) (*providerbalance.Snapshot, *providerbalance.QueryError) {
				if query.Token == "" {
					return nil, balanceQueryFailure("credentials_required", "请配置余额查询凭据")
				}
				if mode == "apiKey" && p.UsesKeyList() && !s.balanceKeySelectable(providerName, p, query.KeyID) {
					return nil, balanceQueryFailure("query_key_unavailable", "已选择的查询密钥停用或正在冷却，未自动切换其他密钥")
				}
				client, err := s.resolveClient(p.Proxy)
				if err != nil {
					return nil, balanceQueryFailure("upstream_unreachable", "余额查询的代理不可用")
				}
				return providerbalance.Fetch(ctx, client, query)
			},
		}
	}
	return sources
}

func providerBalanceIdentity(p *config.Provider, query providerbalance.Query) string {
	// 列表密钥的冷却属于 Provider，不能借另一 Provider 的同值密钥绕过。
	owner := ""
	if p.UsesKeyList() && query.Source == "apiKey" {
		owner = p.Name
	}
	return queryDigest(struct {
		Query        providerbalance.Query
		Proxy, Owner string
	}{query, p.Proxy, owner})
}

func (s *server) balanceKeySelectable(name string, p *config.Provider, id string) bool {
	views := s.keyPool.Snapshot(&config.Config{Providers: map[string]*config.Provider{name: p}})[name]
	for _, view := range views {
		if view.ID == id {
			return view.State == keypool.StateSelectable
		}
	}
	return false
}

func balanceQueryFailure(code, message string) *providerbalance.QueryError {
	return &providerbalance.QueryError{Code: code, Message: message}
}

func cloneQueryProvider(p *config.Provider) *config.Provider {
	if p == nil {
		return nil
	}
	out := *p
	out.APIKeys = append([]config.ProviderKey{}, p.APIKeys...)
	if p.Balance != nil {
		b := *p.Balance
		out.Balance = &b
	}
	if p.ModelCatalog != nil {
		c := *p.ModelCatalog
		out.ModelCatalog = &c
	}
	if p.ExtraHeaders != nil {
		out.ExtraHeaders = make(map[string]string, len(p.ExtraHeaders))
		for name, value := range p.ExtraHeaders {
			out.ExtraHeaders[name] = value
		}
	}
	return &out
}

func queryDigest(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	clear(encoded)
	return hex.EncodeToString(sum[:])
}

func providerCatalogConnection(p *config.Provider) string {
	return queryDigest(struct {
		BaseURL, Format, AuthType, AccountID, UserAgent, Proxy, ClientVersion string
		Headers                                                               map[string]string
	}{p.BaseURL, p.Format, p.AuthType, p.AccountID, p.UserAgent, p.Proxy, p.ModelCatalogVersion(), p.ExtraHeaders})
}

func (s *server) providerQuerySnapshot(name string) (*config.Provider, string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg == nil {
		return nil, s.revision
	}
	return cloneQueryProvider(s.cfg.Providers[name]), s.revision
}

func (s *server) providerCatalogCurrent(name, connection string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg == nil || s.cfg.Providers[name] == nil {
		return false
	}
	return providerCatalogConnection(s.cfg.Providers[name]) == connection
}

func (s *server) handleProviderBalances(w http.ResponseWriter, r *http.Request) {
	setProviderQueryHeaders(w)
	manager := s.initProviderQueries()
	if manager == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "query_unavailable", "查询服务已停止")
		return
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	views := make(map[string]providerbalance.View)
	selected := r.URL.Query().Get("provider")
	if selected != "" {
		if s.cfg == nil || s.cfg.Providers[selected] == nil {
			writeJSONError(w, http.StatusNotFound, "provider_not_found", "指定提供商不存在")
			return
		}
		views[selected] = manager.View(selected)
	} else if s.cfg != nil {
		for name := range s.cfg.Providers {
			views[name] = manager.View(name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": s.revision, "providers": views})
}

func checkProviderRefreshRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	setProviderQueryHeaders(w)
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return false
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return false
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "查询刷新请求须使用 application/json")
		return false
	}
	body, err := readBodyLimited(r, maxProviderQueryRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "查询请求超过大小上限")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid_query_request", "查询请求无法读取")
		}
		return false
	}
	if err := decodeConfigTransferJSON(body, target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_query_request", "查询请求 JSON 结构无效")
		return false
	}
	return true
}

func (s *server) handleProviderBalancesRefresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision  string   `json:"revision"`
		Providers []string `json:"providers"`
	}
	if !checkProviderRefreshRequest(w, r, &input) {
		return
	}
	if len(input.Providers) == 0 || len(input.Providers) > 50 || input.Revision == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_query_request", "请选择 1–50 个提供商并携带配置版本")
		return
	}
	manager := s.initProviderQueries()
	if manager == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "query_unavailable", "查询服务已停止")
		return
	}
	s.cfgMu.RLock()
	if input.Revision != s.revision {
		s.cfgMu.RUnlock()
		writeJSONError(w, http.StatusConflict, "revision_conflict", "配置已变化，请重新读取后刷新")
		return
	}
	seen := make(map[string]bool)
	for _, name := range input.Providers {
		if strings.TrimSpace(name) == "" || seen[name] {
			s.cfgMu.RUnlock()
			writeJSONError(w, http.StatusBadRequest, "invalid_query_request", "提供商名称为空或重复")
			return
		}
		if s.cfg == nil || s.cfg.Providers[name] == nil {
			s.cfgMu.RUnlock()
			writeJSONError(w, http.StatusNotFound, "provider_not_found", "指定提供商不存在")
			return
		}
		seen[name] = true
	}
	sources := s.providerBalanceSources(s.cfg)
	s.cfgMu.RUnlock()
	ctx, cancel := s.providerQueryContextWithTimeout(r.Context(), providerBalanceBatchBudget)
	defer cancel()
	results := make([]providerbalance.Result, len(input.Providers))
	var wg sync.WaitGroup
	for index, name := range input.Providers {
		expected := sources[name]
		if expected.Key == "" {
			results[index] = providerbalance.Result{Provider: name, Status: "skipped", Balance: manager.View(name)}
			continue
		}
		wg.Add(1)
		go func(index int, name, key string) {
			defer wg.Done()
			results[index] = manager.RefreshSource(ctx, name, key)
		}(index, name, expected.Key)
	}
	wg.Wait()
	s.cfgMu.RLock()
	revision := s.revision
	s.cfgMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision, "results": results})
}

func (s *server) handleProviderModelsRefresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		Revision string `json:"revision"`
	}
	if !checkProviderRefreshRequest(w, r, &input) {
		return
	}
	if input.Provider == "" || input.Revision == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_query_request", "缺少提供商名称或配置版本")
		return
	}
	p, revision := s.providerQuerySnapshot(input.Provider)
	if input.Revision != revision {
		writeJSONError(w, http.StatusConflict, "revision_conflict", "配置已变化，请重新读取后刷新")
		return
	}
	if p == nil {
		writeJSONError(w, http.StatusNotFound, "provider_not_found", "指定提供商不存在")
		return
	}
	s.writeProviderModels(w, r, input.Provider, p, true)
}

func (s *server) writeProviderModels(w http.ResponseWriter, r *http.Request, name string, p *config.Provider, force bool) {
	setProviderQueryHeaders(w)
	if p.AuthType == "chatgptOAuth" {
		s.writeChatGPTProviderModels(w, r, name, p, force)
		return
	}
	if p.Name == "" {
		p.Name = name
	}
	if p.UsesKeyList() && (s.keyPool == nil || !s.keyPool.HasSelectable(name, p, nil)) {
		writeJSONError(w, http.StatusServiceUnavailable, "no_selectable_provider_key", "提供商没有可选择的密钥")
		return
	}
	client, err := s.resolveClient(p.Proxy)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "解析提供商代理失败")
		return
	}
	models, err := fetchUpstreamModels(r.Context(), p, client, s.keyPool)
	if err != nil {
		var noSelectable *noSelectableProviderKeyError
		if errors.As(err, &noSelectable) {
			writeJSONError(w, http.StatusServiceUnavailable, "no_selectable_provider_key", err.Error())
		} else {
			writeJSONError(w, http.StatusBadGateway, "upstream_error", redactProbeMessage(err.Error(), p))
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "models": models, "source": "upstream-models", "queriedAt": time.Now().UTC().Format(time.RFC3339), "cached": false, "stale": false, "message": ""})
}

func (s *server) writeChatGPTProviderModels(w http.ResponseWriter, r *http.Request, name string, p *config.Provider, force bool) {
	if s.chatgptAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "oauth_unavailable", "OAuth 账户管理器不可用")
		return
	}
	connection := providerCatalogConnection(p)
	ctx, cancel := s.providerQueryContextWithTimeout(r.Context(), providerCatalogBudget)
	defer cancel()
	finish, err := s.modelCatalogCache.Lease(ctx, name, connection)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "query_cancelled", "模型目录查询等待已结束")
		return
	}
	defer finish(nil, nil)
	if !s.providerCatalogCurrent(name, connection) {
		writeJSONError(w, http.StatusConflict, "configuration_changed", "提供商配置已变化，请重新查询")
		return
	}
	identity, err := s.chatgptAuth.CatalogIdentity(p.AccountID)
	if err != nil {
		s.writeCatalogCredentialError(w, err)
		return
	}
	old, fresh := s.modelCatalogCache.Lookup(name, connection, identity)
	if failure, wait := s.modelCatalogCache.Failure(name, connection, identity); failure != nil && wait > 0 {
		seconds := int64((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		if old != nil {
			writeCatalogResult(w, name, old, true, true, failure.Message)
		} else {
			writeJSONError(w, http.StatusTooManyRequests, failure.Code, failure.Message)
		}
		return
	}
	if old != nil && fresh && !force && s.providerCatalogCurrent(name, connection) && s.chatgptAuth.CatalogIdentityCurrent(p.AccountID, identity) {
		writeCatalogResult(w, name, old, true, false, "")
		return
	}
	client, err := s.resolveClient(p.Proxy)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "模型目录查询代理不可用")
		return
	}
	token, remoteID, captured, err := s.chatgptAuth.CatalogCredentials(ctx, p.AccountID, client)
	if err != nil {
		s.writeCatalogCredentialError(w, err)
		return
	}
	identity = captured
	old, _ = s.modelCatalogCache.Lookup(name, connection, identity)
	result, failure := providermodels.Fetch(ctx, client, providermodels.Query{BaseURL: p.BaseURL, ClientVersion: p.ModelCatalogVersion(), Token: token, AccountID: remoteID, UserAgent: p.UserAgent, ExtraHeaders: p.ExtraHeaders})
	if !s.providerCatalogCurrent(name, connection) || !s.chatgptAuth.CatalogIdentityCurrent(p.AccountID, identity) {
		writeJSONError(w, http.StatusConflict, "configuration_changed", "提供商配置或账户凭据已变化，请重新查询")
		return
	}
	if failure != nil {
		if ctx.Err() == nil {
			s.modelCatalogCache.RecordFailure(name, connection, identity, failure)
		}
		if old != nil && ctx.Err() == nil {
			writeCatalogResult(w, name, old, true, true, failure.Message)
			return
		}
		status := http.StatusBadGateway
		if failure.Code == "rate_limited" {
			status = http.StatusTooManyRequests
			if failure.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(int64((failure.RetryAfter+time.Second-1)/time.Second), 10))
			}
		}
		writeJSONError(w, status, failure.Code, failure.Message)
		return
	}
	finish(identity, result)
	writeCatalogResult(w, name, result, false, false, "")
}

func writeCatalogResult(w http.ResponseWriter, name string, result *providermodels.Result, cached, stale bool, message string) {
	if message == "" {
		message = "目录来自绑定账户，不代表已完成模型调用验证"
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "models": result.Models, "source": "chatgpt-codex", "queriedAt": result.QueriedAt, "cached": cached, "stale": stale, "message": message})
}

func (s *server) writeCatalogCredentialError(w http.ResponseWriter, err error) {
	outcome := chatgptauth.DescribeCredentialError(err)
	status := http.StatusBadGateway
	if outcome.Category == "not_found" {
		status = http.StatusBadRequest
	}
	if outcome.Category == "conflict" {
		status = http.StatusConflict
	}
	writeJSONError(w, status, "oauth_"+outcome.Category, outcome.Message)
}

// 查询令牌的保留只在同名、同适配器与同目标站点下允许，不能静默换目标发送。
func refillBalanceToken(name string, p, old *config.Provider) error {
	if p.Balance == nil || p.Balance.AccessToken != config.BalanceTokenKeepSentinel {
		return nil
	}
	if old == nil || old.Balance == nil || old.Balance.AccessToken == "" || old.Balance.Type != p.Balance.Type || old.BalanceBaseURL() != p.BalanceBaseURL() {
		return errors.New("providers." + name + ".balance.accessToken 无法保留：余额查询目标已变化或没有已保存令牌")
	}
	p.Balance.AccessToken = old.Balance.AccessToken
	return nil
}

func sortedProviderNames(cfg *config.Config) []string {
	if cfg == nil {
		return []string{}
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
