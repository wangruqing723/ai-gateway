// Package chatgptauth 管理 Codex 使用的 ChatGPT OAuth 授权与凭据。
package chatgptauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthBaseURL  = "https://auth.openai.com"
	usageURL      = "https://chatgpt.com/backend-api/wham/usage"
	chatGPTIssuer = "https://api.openai.com/auth"
	loginTTL      = 10 * time.Minute
	refreshBefore = 2 * time.Minute
	maxOAuthBody  = 1 << 20
	maxUsageBody  = 1 << 20
	usageTimeout  = 15 * time.Second
	usageCacheTTL = 5 * time.Minute
	usageWorkers  = 3
)

var ErrAccountNotFound = errors.New("ChatGPT OAuth 账户不存在")
var ErrInvalidOAuthCallback = errors.New("ChatGPT OAuth 回调链接无效或与当前授权会话不匹配")

// AccountView 是不含任何令牌的账户管理视图。
type AccountView struct {
	ID                      string         `json:"id"`
	Email                   string         `json:"email,omitempty"`
	Plan                    string         `json:"plan,omitempty"`
	Status                  string         `json:"status"`
	ExpiresAt               string         `json:"expiresAt,omitempty"`
	LastRefresh             string         `json:"lastRefresh,omitempty"`
	AuthorizedAt            string         `json:"authorizedAt,omitempty"`
	SubscriptionActiveStart string         `json:"subscriptionActiveStart,omitempty"`
	SubscriptionActiveUntil string         `json:"subscriptionActiveUntil,omitempty"`
	Usage                   *UsageSnapshot `json:"usage,omitempty"`
	Providers               []string       `json:"providers,omitempty"`
}

// UsageSnapshot 是 usage 接口返回的可公开额度快照，不含凭据或上游原始响应。
type UsageSnapshot struct {
	PlanType      string        `json:"planType,omitempty"`
	Windows       []UsageWindow `json:"windows,omitempty"`
	Credits       *UsageCredits `json:"credits,omitempty"`
	LastAttemptAt string        `json:"lastAttemptAt,omitempty"`
	LastSuccessAt string        `json:"lastSuccessAt,omitempty"`
	// LastCheckedAt 保留为兼容字段，含义与 LastAttemptAt 相同。
	LastCheckedAt string `json:"lastCheckedAt,omitempty"`
	Error         string `json:"error,omitempty"`
}

// UsageWindow 表示上游当前返回的一个额度窗口。
type UsageWindow struct {
	Name               string  `json:"name"`
	LimitWindowSeconds int64   `json:"limitWindowSeconds,omitempty"`
	UsedPercent        float64 `json:"usedPercent"`
	RemainingPercent   float64 `json:"remainingPercent"`
	ResetAt            string  `json:"resetAt,omitempty"`
}

// UsageCredits 是上游可选的 credits 余额。
type UsageCredits struct {
	Balance string `json:"balance"`
}

// LoginStart 是新 OAuth 授权会话的安全前端视图。
type LoginStart struct {
	SessionID string    `json:"sessionId"`
	AuthURL   string    `json:"authUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// LoginStatus 是授权状态轮询结果。
type LoginStatus struct {
	Status  string       `json:"status"`
	Message string       `json:"message,omitempty"`
	Account *AccountView `json:"account,omitempty"`
}

type account struct {
	ID                      string         `json:"id"`
	Email                   string         `json:"email,omitempty"`
	Plan                    string         `json:"plan,omitempty"`
	AccountID               string         `json:"chatgptAccountId"`
	IDToken                 string         `json:"idToken,omitempty"`
	AccessToken             string         `json:"accessToken"`
	RefreshToken            string         `json:"refreshToken"`
	ExpiresAt               time.Time      `json:"expiresAt"`
	LastRefresh             time.Time      `json:"lastRefresh"`
	AuthorizedAt            *time.Time     `json:"authorizedAt,omitempty"`
	SubscriptionActiveStart *time.Time     `json:"subscriptionActiveStart,omitempty"`
	SubscriptionActiveUntil *time.Time     `json:"subscriptionActiveUntil,omitempty"`
	Usage                   *UsageSnapshot `json:"usage,omitempty"`
	NeedsReauth             bool           `json:"needsReauth,omitempty"`
	refreshMu               sync.Mutex
	usageMu                 sync.Mutex
}

type diskStore struct {
	Version  int        `json:"version"`
	Accounts []*account `json:"accounts"`
}

type loginSession struct {
	ID          string
	State       string
	Verifier    string
	RedirectURI string
	ExpiresAt   time.Time
	Status      string
	Message     string
	Account     *AccountView
}

// Manager 把 OAuth 凭据与可公开的账户元信息隔离管理。
type Manager struct {
	mu          sync.RWMutex
	path        string
	accounts    map[string]*account
	sessions    map[string]*loginSession
	stateToID   map[string]string
	client      *http.Client
	callback    string
	callbackErr error
}

// New 从路径加载 OAuth 账户。令牌文件只允许当前进程用户读写。
func New(path string, client *http.Client) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("ChatGPT OAuth 存储路径为空")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	m := &Manager{
		path:      path,
		accounts:  make(map[string]*account),
		sessions:  make(map[string]*loginSession),
		stateToID: make(map[string]string),
		client:    client,
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 ChatGPT OAuth 账户文件失败: %w", err)
	}
	if err == nil {
		var store diskStore
		if err := json.Unmarshal(data, &store); err != nil {
			return nil, fmt.Errorf("解析 ChatGPT OAuth 账户文件失败: %w", err)
		}
		for _, item := range store.Accounts {
			if item == nil || item.ID == "" || item.AccountID == "" || (item.AccessToken == "" && item.RefreshToken == "") {
				continue
			}
			m.accounts[item.ID] = item
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("设置 ChatGPT OAuth 账户文件权限失败: %w", err)
		}
	}
	return m, nil
}

// StartCallbackServer 启动 Codex OAuth 固定回调端点。优先使用 1455，端口占用时回退 1457。
// bindHost 默认传 127.0.0.1；Docker Compose 通过端口映射时传 0.0.0.0。
func (m *Manager) StartCallbackServer(bindHost string) (*http.Server, error) {
	if strings.TrimSpace(bindHost) == "" {
		bindHost = "127.0.0.1"
	}
	var lastErr error
	for _, port := range []int{1455, 1457} {
		listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, fmt.Sprint(port)))
		if err != nil {
			lastErr = err
			continue
		}
		m.mu.Lock()
		m.callback = fmt.Sprintf("http://localhost:%d/auth/callback", port)
		m.callbackErr = nil
		m.mu.Unlock()
		srv := &http.Server{
			Handler:           http.HandlerFunc(m.handleCallback),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() { _ = srv.Serve(listener) }()
		return srv, nil
	}
	m.mu.Lock()
	m.callbackErr = lastErr
	m.mu.Unlock()
	return nil, fmt.Errorf("ChatGPT OAuth 回调端口 1455 和 1457 均不可用: %w", lastErr)
}

// StartLogin 创建包含一次性 state 与 PKCE 的 OAuth 授权会话。
func (m *Manager) StartLogin() (LoginStart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupSessionsLocked(time.Now())
	if m.callback == "" {
		if m.callbackErr != nil {
			return LoginStart{}, fmt.Errorf("OAuth 回调服务不可用: %w", m.callbackErr)
		}
		return LoginStart{}, errors.New("OAuth 回调服务尚未启动")
	}
	state, err := randomURLToken(32)
	if err != nil {
		return LoginStart{}, err
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return LoginStart{}, err
	}
	sessionID, err := randomURLToken(24)
	if err != nil {
		return LoginStart{}, err
	}
	expiresAt := time.Now().Add(loginTTL)
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {oauthClientID},
		"redirect_uri":               {m.callback},
		"scope":                      {"openid profile email offline_access api.connectors.read api.connectors.invoke"},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"codex_cli_simplified_flow":  {"true"},
		"id_token_add_organizations": {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	authURL := oauthBaseURL + "/oauth/authorize?" + query.Encode()
	m.sessions[sessionID] = &loginSession{
		ID: sessionID, State: state, Verifier: verifier, RedirectURI: m.callback,
		ExpiresAt: expiresAt, Status: "pending",
	}
	m.stateToID[state] = sessionID
	return LoginStart{SessionID: sessionID, AuthURL: authURL, ExpiresAt: expiresAt}, nil
}

// Status 返回当前授权会话状态，不包含任何令牌。
func (m *Manager) Status(sessionID string) (LoginStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupSessionsLocked(time.Now())
	session, ok := m.sessions[sessionID]
	if !ok {
		return LoginStatus{}, false
	}
	return LoginStatus{Status: session.Status, Message: session.Message, Account: session.Account}, true
}

// List 返回账户元信息及各账户绑定的 Provider 名称。
func (m *Manager) List(references map[string][]string) []AccountView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AccountView, 0, len(m.accounts))
	for _, item := range m.accounts {
		view := viewOf(item)
		view.Providers = append([]string(nil), references[item.ID]...)
		sort.Strings(view.Providers)
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Email == out[j].Email {
			return out[i].ID < out[j].ID
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// RefreshUsage 刷新各账户的用量快照。force=false 时会复用五分钟内的结果。
func (m *Manager) RefreshUsage(ctx context.Context, references map[string][]string, force bool) []AccountView {
	m.mu.RLock()
	accounts := make([]*account, 0, len(m.accounts))
	for _, item := range m.accounts {
		accounts = append(accounts, item)
	}
	m.mu.RUnlock()

	semaphore := make(chan struct{}, usageWorkers)
	var workers sync.WaitGroup
	for _, item := range accounts {
		workers.Add(1)
		go func(item *account) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			m.refreshAccountUsage(ctx, item, force)
		}(item)
	}
	workers.Wait()
	return m.List(references)
}

// RefreshAccountUsage 只刷新指定账户，供账户卡片的单账户刷新按钮使用。
func (m *Manager) RefreshAccountUsage(ctx context.Context, id string, force bool) (AccountView, error) {
	m.mu.RLock()
	item := m.accounts[id]
	m.mu.RUnlock()
	if item == nil {
		return AccountView{}, ErrAccountNotFound
	}
	m.refreshAccountUsage(ctx, item, force)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.accounts[id] != item {
		return AccountView{}, ErrAccountNotFound
	}
	return viewOf(item), nil
}

func (m *Manager) refreshAccountUsage(ctx context.Context, item *account, force bool) {
	item.usageMu.Lock()
	defer item.usageMu.Unlock()

	now := time.Now().UTC()
	m.mu.RLock()
	if m.accounts[item.ID] != item {
		m.mu.RUnlock()
		return
	}
	lastUsage := cloneUsage(item.Usage)
	m.mu.RUnlock()
	if !force && lastUsage != nil {
		lastAttemptAt := lastUsage.LastAttemptAt
		if lastAttemptAt == "" {
			lastAttemptAt = lastUsage.LastCheckedAt
		}
		if checkedAt, err := time.Parse(time.RFC3339, lastAttemptAt); err == nil && now.Sub(checkedAt) < usageCacheTTL {
			return
		}
	}

	requestCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	accessToken, accountID, err := m.AccessToken(requestCtx, item.ID, m.client)
	if err != nil {
		message := "无法获取访问令牌，请检查 OAuth 代理或网络设置后重试"
		if errors.Is(err, errInvalidGrant) || strings.Contains(err.Error(), "需要重新授权") {
			message = "OAuth 刷新凭据已失效，请重新授权"
		}
		m.setUsageError(item, message)
		return
	}
	usage, err := m.fetchUsage(requestCtx, accessToken, accountID)
	if err != nil {
		m.setUsageError(item, usageErrorMessage(err))
		return
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	usage.LastAttemptAt = checkedAt
	usage.LastCheckedAt = checkedAt
	usage.LastSuccessAt = checkedAt
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accounts[item.ID] != item {
		return
	}
	item.Usage = &usage
	m.persistUsageLocked()
}

func (m *Manager) setUsageError(item *account, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accounts[item.ID] != item {
		return
	}
	usage := cloneUsage(item.Usage)
	if usage == nil {
		usage = &UsageSnapshot{}
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	usage.LastAttemptAt = checkedAt
	usage.LastCheckedAt = checkedAt
	usage.Error = message
	item.Usage = usage
	m.persistUsageLocked()
}

// persistUsageLocked 将不含凭据的额度快照与账户状态一起原子保存。
// 当前调用方持有 m.mu 写锁；缓存落盘失败不影响刚查询到的内存快照。
func (m *Manager) persistUsageLocked() {
	if err := m.persistLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT usage cache persistence failed: %v\n", err)
	}
}

type usageHTTPError struct {
	status int
}

func (e *usageHTTPError) Error() string {
	return fmt.Sprintf("usage endpoint returned HTTP %d", e.status)
}

func usageErrorMessage(err error) string {
	var statusErr *usageHTTPError
	if errors.As(err, &statusErr) {
		if statusErr.status == http.StatusUnauthorized || statusErr.status == http.StatusForbidden {
			return "ChatGPT 拒绝了用量查询，请确认账户状态或稍后重试"
		}
		return fmt.Sprintf("ChatGPT 用量服务返回 HTTP %d", statusErr.status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ChatGPT 用量查询超时，请稍后重试"
	}
	return "无法读取 ChatGPT 用量，请稍后重试"
}

func (m *Manager) fetchUsage(ctx context.Context, accessToken, accountID string) (UsageSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return UsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	resp, err := m.client.Do(req)
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return UsageSnapshot{}, &usageHTTPError{status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	if err != nil {
		return UsageSnapshot{}, err
	}
	if len(body) > maxUsageBody {
		return UsageSnapshot{}, errors.New("usage response too large")
	}
	var payload usageResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageSnapshot{}, errors.New("invalid usage response")
	}
	return payload.snapshot(time.Now().UTC()), nil
}

type usageResponse struct {
	PlanType             string                `json:"plan_type"`
	RateLimit            *usageRateLimit       `json:"rate_limit"`
	CodeReviewRateLimit  *usageRateLimit       `json:"code_review_rate_limit"`
	AdditionalRateLimits []usageNamedRateLimit `json:"additional_rate_limits"`
	Credits              *usageCreditsResponse `json:"credits"`
}

type usageRateLimit struct {
	PrimaryWindow   *usageWindowResponse `json:"primary_window"`
	SecondaryWindow *usageWindowResponse `json:"secondary_window"`
}

type usageNamedRateLimit struct {
	LimitName string          `json:"limit_name"`
	RateLimit *usageRateLimit `json:"rate_limit"`
}

type usageWindowResponse struct {
	UsedPercent        *float64        `json:"used_percent"`
	LimitWindowSeconds int64           `json:"limit_window_seconds"`
	ResetAfterSeconds  int64           `json:"reset_after_seconds"`
	ResetAt            json.RawMessage `json:"reset_at"`
}

type usageCreditsResponse struct {
	Balance json.RawMessage `json:"balance"`
}

func (payload usageResponse) snapshot(now time.Time) UsageSnapshot {
	snapshot := UsageSnapshot{PlanType: strings.TrimSpace(payload.PlanType)}
	appendRateLimit := func(name string, limit *usageRateLimit) {
		if limit == nil {
			return
		}
		appendWindow := func(windowName string, window *usageWindowResponse) {
			if window == nil || window.UsedPercent == nil {
				return
			}
			used := *window.UsedPercent
			remaining := 100 - used
			if remaining < 0 {
				remaining = 0
			} else if remaining > 100 {
				remaining = 100
			}
			entry := UsageWindow{
				Name:               windowName,
				LimitWindowSeconds: window.LimitWindowSeconds,
				UsedPercent:        used,
				RemainingPercent:   remaining,
			}
			if resetAt := parseUsageResetAt(window.ResetAt); !resetAt.IsZero() {
				entry.ResetAt = resetAt.UTC().Format(time.RFC3339)
			} else if window.ResetAfterSeconds > 0 {
				entry.ResetAt = now.Add(time.Duration(window.ResetAfterSeconds) * time.Second).UTC().Format(time.RFC3339)
			}
			snapshot.Windows = append(snapshot.Windows, entry)
		}
		primaryName, secondaryName := "primary", "secondary"
		if name != "" {
			primaryName = name + " primary"
			secondaryName = name + " secondary"
		}
		appendWindow(primaryName, limit.PrimaryWindow)
		appendWindow(secondaryName, limit.SecondaryWindow)
	}
	appendRateLimit("", payload.RateLimit)
	appendRateLimit("code review", payload.CodeReviewRateLimit)
	for i, additional := range payload.AdditionalRateLimits {
		name := strings.TrimSpace(additional.LimitName)
		if name == "" {
			name = fmt.Sprintf("additional %d", i+1)
		}
		appendRateLimit(name, additional.RateLimit)
	}
	if payload.Credits != nil {
		if balance := rawJSONScalar(payload.Credits.Balance); balance != "" {
			snapshot.Credits = &UsageCredits{Balance: balance}
		}
	}
	return snapshot
}

func parseUsageResetAt(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed
		}
		if seconds, err := strconv.ParseFloat(text, 64); err == nil && seconds > 0 {
			return unixTime(seconds)
		}
		return time.Time{}
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&number) == nil {
		if seconds, err := number.Float64(); err == nil && seconds > 0 {
			return unixTime(seconds)
		}
	}
	return time.Time{}
}

func unixTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 1_000_000_000_000 {
		seconds /= 1000
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return time.Unix(whole, nanos).UTC()
}

func rawJSONScalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&number) == nil {
		return number.String()
	}
	return ""
}

func cloneUsage(usage *UsageSnapshot) *UsageSnapshot {
	if usage == nil {
		return nil
	}
	copy := *usage
	copy.Windows = append([]UsageWindow(nil), usage.Windows...)
	if usage.Credits != nil {
		credits := *usage.Credits
		copy.Credits = &credits
	}
	return &copy
}

// Account 返回不含令牌的单账户状态。
func (m *Manager) Account(id string) (AccountView, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.accounts[id]
	if !ok {
		return AccountView{}, false
	}
	return viewOf(item), true
}

// Delete 删除本地 OAuth 凭据。调用方负责先确认账户未被 Provider 引用。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[id]; !ok {
		return ErrAccountNotFound
	}
	item := m.accounts[id]
	delete(m.accounts, id)
	if err := m.persistLocked(); err != nil {
		m.accounts[id] = item
		return err
	}
	return nil
}

// AccessToken 返回可用的 access token 与 ChatGPT 账户请求头值，必要时刷新并持久化。
func (m *Manager) AccessToken(ctx context.Context, id string, client *http.Client) (string, string, error) {
	m.mu.RLock()
	item, ok := m.accounts[id]
	m.mu.RUnlock()
	if !ok {
		return "", "", ErrAccountNotFound
	}
	item.refreshMu.Lock()
	defer item.refreshMu.Unlock()
	m.mu.RLock()
	if m.accounts[id] != item {
		m.mu.RUnlock()
		return "", "", ErrAccountNotFound
	}
	accessToken, refreshToken, accountID, expiresAt, needsReauth := item.AccessToken, item.RefreshToken, item.AccountID, item.ExpiresAt, item.NeedsReauth
	m.mu.RUnlock()
	if !needsReauth && accessToken != "" {
		if refreshToken == "" && time.Now().Before(expiresAt) {
			return accessToken, accountID, nil
		}
		if refreshToken != "" && time.Until(expiresAt) > refreshBefore {
			return accessToken, accountID, nil
		}
	}
	if refreshToken == "" || needsReauth {
		return "", "", errors.New("ChatGPT OAuth 账户需要重新授权")
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tokens, err := m.exchange(refreshCtx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {oauthClientID},
	}, client)
	if err != nil {
		if errors.Is(err, errInvalidGrant) {
			m.mu.Lock()
			if m.accounts[id] == item {
				item.NeedsReauth = true
				_ = m.persistLocked()
			}
			m.mu.Unlock()
		}
		return "", "", fmt.Errorf("刷新 ChatGPT OAuth 令牌失败: %w", err)
	}
	claims := tokenClaims(tokens.IDToken, tokens.AccessToken)
	refreshedAccountID := firstString(claims.Auth["chatgpt_account_id"], claims.Auth["account_id"], claims.Root["chatgpt_account_id"])
	metadata := metadataFromClaims(claims)
	if refreshedAccountID == "" {
		refreshedAccountID = accountID
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accounts[id] != item {
		return "", "", ErrAccountNotFound
	}
	item.AccessToken = tokens.AccessToken
	if tokens.IDToken != "" {
		item.IDToken = tokens.IDToken
	}
	if tokens.RefreshToken != "" {
		item.RefreshToken = tokens.RefreshToken
	}
	item.AccountID = refreshedAccountID
	if metadata.Email != "" {
		item.Email = metadata.Email
	}
	if metadata.Plan != "" {
		item.Plan = metadata.Plan
	}
	if !metadata.SubscriptionActiveStart.IsZero() {
		value := metadata.SubscriptionActiveStart
		item.SubscriptionActiveStart = &value
	}
	if !metadata.SubscriptionActiveUntil.IsZero() {
		value := metadata.SubscriptionActiveUntil
		item.SubscriptionActiveUntil = &value
	}
	item.ExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	item.LastRefresh = now
	item.NeedsReauth = false
	if err := m.persistLocked(); err != nil {
		return "", "", fmt.Errorf("保存刷新后的 ChatGPT OAuth 令牌失败: %w", err)
	}
	return item.AccessToken, item.AccountID, nil
}

func (m *Manager) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/auth/callback" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	callbackURL := (&url.URL{
		Scheme:   "http",
		Host:     r.Host,
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}).String()
	status, err := m.CompleteLoginFromCallback(r.Context(), callbackURL)
	if err != nil {
		writeCallbackPage(w, "授权链接无效或已使用", false)
		return
	}
	switch status.Status {
	case "completed":
		writeCallbackPage(w, "ChatGPT 账户已连接，可以关闭此页面", true)
	case "processing":
		writeCallbackPage(w, "授权结果正在处理，可以返回管理页面查看状态", false)
	default:
		writeCallbackPage(w, "授权未完成，可以返回管理页面查看状态并重试", false)
	}
}

// CompleteLoginFromCallback 校验并处理用户手动提交的完整 OAuth 回调链接。
func (m *Manager) CompleteLoginFromCallback(ctx context.Context, callbackURL string) (LoginStatus, error) {
	callbackURL = strings.TrimSpace(callbackURL)
	u, err := url.Parse(callbackURL)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.User != nil || u.Scheme != "http" || u.Host == "" ||
		u.Path != "/auth/callback" || u.EscapedPath() != "/auth/callback" || u.Fragment != "" ||
		u.RawFragment != "" || strings.Contains(callbackURL, "#") || u.RawQuery == "" {
		return LoginStatus{}, ErrInvalidOAuthCallback
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query["state"]) != 1 || len(query["code"]) > 1 || len(query["error"]) > 1 {
		return LoginStatus{}, ErrInvalidOAuthCallback
	}
	state := query.Get("state")
	code := query.Get("code")
	oauthError := query.Get("error")
	if state == "" || (code == "" && oauthError == "") || (code != "" && oauthError != "") {
		return LoginStatus{}, ErrInvalidOAuthCallback
	}
	callbackURI := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
	return m.processLoginCallback(ctx, callbackURI, state, code, oauthError)
}

func (m *Manager) processLoginCallback(ctx context.Context, callbackURI, state, code, oauthError string) (LoginStatus, error) {
	m.mu.Lock()
	_ = m.cleanupSessionsLocked(time.Now())
	sessionID := m.stateToID[state]
	session := m.sessions[sessionID]
	if state == "" || session == nil || session.State != state || !sameCallbackURI(callbackURI, session.RedirectURI) {
		m.mu.Unlock()
		return LoginStatus{}, ErrInvalidOAuthCallback
	}
	if session.Status != "pending" {
		status := LoginStatus{Status: session.Status, Message: session.Message, Account: session.Account}
		m.mu.Unlock()
		return status, nil
	}
	session.Status = "processing"
	m.mu.Unlock()

	if oauthError != "" {
		m.finishLogin(sessionID, nil, "用户取消了授权", false)
		status, _ := m.Status(sessionID)
		return status, nil
	}
	if code == "" {
		m.finishLogin(sessionID, nil, "授权回调缺少 code", false)
		status, _ := m.Status(sessionID)
		return status, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tokens, err := m.exchange(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {session.RedirectURI},
		"client_id":     {oauthClientID},
		"code_verifier": {session.Verifier},
	})
	if err != nil {
		logOAuthCodeExchangeFailure(err)
		m.finishLogin(sessionID, nil, publicOAuthCodeExchangeFailure(err), false)
		status, _ := m.Status(sessionID)
		return status, nil
	}
	claims := tokenClaims(tokens.IDToken, tokens.AccessToken)
	accountID := firstString(claims.Auth["chatgpt_account_id"], claims.Auth["account_id"], claims.Root["chatgpt_account_id"])
	if accountID == "" {
		m.finishLogin(sessionID, nil, "OAuth 返回的账户信息不完整，请重试", false)
		status, _ := m.Status(sessionID)
		return status, nil
	}
	metadata := metadataFromClaims(claims)
	metadata.AccountID = accountID
	item, err := m.saveTokens(tokens, metadata)
	if err != nil {
		m.finishLogin(sessionID, nil, "保存 ChatGPT OAuth 账户失败", false)
		status, _ := m.Status(sessionID)
		return status, nil
	}
	view, _ := m.Account(item.ID)
	m.finishLogin(sessionID, &view, "", true)
	status, _ := m.Status(sessionID)
	return status, nil
}

func sameCallbackURI(actual, expected string) bool {
	a, err := url.Parse(actual)
	if err != nil {
		return false
	}
	e, err := url.Parse(expected)
	if err != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, e.Scheme) && strings.EqualFold(a.Hostname(), e.Hostname()) &&
		a.Port() == e.Port() && a.EscapedPath() == e.EscapedPath()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

var errInvalidGrant = errors.New("invalid_grant")

type oauthExchangeFailure struct {
	category   string
	httpStatus int
	err        error
}

func (e *oauthExchangeFailure) Error() string { return e.err.Error() }

func (e *oauthExchangeFailure) Unwrap() error { return e.err }

func newOAuthExchangeFailure(category string, httpStatus int, err error) error {
	return &oauthExchangeFailure{category: category, httpStatus: httpStatus, err: err}
}

// 只记录固定错误类别和 HTTP 状态，避免日志包含授权码、令牌、响应正文或代理细节。
func logOAuthCodeExchangeFailure(err error) {
	var failure *oauthExchangeFailure
	if !errors.As(err, &failure) {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth authorization code exchange failed (category=unknown)\n")
		return
	}
	if failure.httpStatus > 0 {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth authorization code exchange failed (category=%s, http_status=%d)\n", failure.category, failure.httpStatus)
		return
	}
	fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth authorization code exchange failed (category=%s)\n", failure.category)
}

// publicOAuthCodeExchangeFailure 仅向管理页面暴露固定错误类别与 HTTP 状态，不包含上游响应正文。
func publicOAuthCodeExchangeFailure(err error) string {
	var failure *oauthExchangeFailure
	if !errors.As(err, &failure) {
		return "ChatGPT 令牌交换失败（未知错误），请检查网络或代理设置后重试。"
	}
	message := "ChatGPT 令牌交换失败"
	switch failure.category {
	case "transport":
		message = "无法连接 ChatGPT OAuth 服务，请检查网络或 AI_GATEWAY_OAUTH_PROXY 设置后重试"
	case "invalid_grant":
		message = "授权码或刷新凭据已失效，请重新发起授权"
	case "oauth_invalid_client":
		message = "OAuth 客户端被服务端拒绝；请稍后重试，若持续发生请检查网关版本兼容性"
	case "oauth_access_denied":
		message = "ChatGPT 未批准本次授权，请重新发起并完成授权"
	case "invalid_response":
		message = "OAuth 服务返回了无法识别的响应，请检查网络或代理返回内容"
	}
	detail := message + "（" + failure.category
	if failure.httpStatus > 0 {
		detail += fmt.Sprintf("，HTTP %d", failure.httpStatus)
	}
	return detail + "）。"
}

func (m *Manager) exchange(ctx context.Context, form url.Values, override ...*http.Client) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, newOAuthExchangeFailure("request_build", 0, errors.New("无法构建 OAuth 令牌请求"))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := m.client
	if len(override) > 0 && override[0] != nil {
		client = override[0]
	}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, newOAuthExchangeFailure("transport", 0, fmt.Errorf("连接 OAuth 服务失败: %w", err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
	if err != nil {
		return tokenResponse{}, newOAuthExchangeFailure("response_read", resp.StatusCode, errors.New("读取 OAuth 响应失败"))
	}
	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return tokenResponse{}, newOAuthExchangeFailure("invalid_response", resp.StatusCode, fmt.Errorf("OAuth 服务返回了无法解析的响应（HTTP %d）", resp.StatusCode))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if tokens.Error == "invalid_grant" {
			return tokenResponse{}, newOAuthExchangeFailure("invalid_grant", resp.StatusCode, errInvalidGrant)
		}
		if category := safeOAuthErrorCategory(tokens.Error); category != "" {
			return tokenResponse{}, newOAuthExchangeFailure("oauth_"+category, resp.StatusCode, fmt.Errorf("OAuth 服务返回 HTTP %d", resp.StatusCode))
		}
		return tokenResponse{}, newOAuthExchangeFailure("http_error", resp.StatusCode, fmt.Errorf("OAuth 服务返回 HTTP %d", resp.StatusCode))
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn <= 0 {
		return tokenResponse{}, newOAuthExchangeFailure("missing_tokens", resp.StatusCode, errors.New("OAuth 响应缺少 access_token、refresh_token 或有效期"))
	}
	return tokens, nil
}

// 只允许输出标准 OAuth 错误码；服务端错误描述和响应正文不会进入日志。
func safeOAuthErrorCategory(raw string) string {
	switch raw {
	case "invalid_request", "invalid_client", "unauthorized_client", "unsupported_grant_type",
		"invalid_scope", "access_denied", "server_error", "temporarily_unavailable":
		return raw
	default:
		return ""
	}
}

type accountMetadata struct {
	AccountID               string
	Email                   string
	Plan                    string
	SubscriptionActiveStart time.Time
	SubscriptionActiveUntil time.Time
}

func metadataFromClaims(claims jwtClaims) accountMetadata {
	return accountMetadata{
		Email:                   firstString(claims.Root["email"], claims.Auth["email"]),
		Plan:                    firstString(claims.Auth["chatgpt_plan_type"], claims.Auth["plan_type"]),
		SubscriptionActiveStart: claimTime(claims.Auth["chatgpt_subscription_active_start"], claims.Root["chatgpt_subscription_active_start"]),
		SubscriptionActiveUntil: claimTime(claims.Auth["chatgpt_subscription_active_until"], claims.Root["chatgpt_subscription_active_until"]),
	}
}

func (m *Manager) saveTokens(tokens tokenResponse, meta accountMetadata) (*account, error) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	var item *account
	for _, existing := range m.accounts {
		if existing.AccountID == meta.AccountID {
			item = existing
			break
		}
	}
	created := item == nil
	if item == nil {
		id, err := randomHex(16)
		if err != nil {
			return nil, err
		}
		item = &account{ID: "acc_" + id}
		m.accounts[item.ID] = item
	}
	previous := account{
		ID: item.ID, Email: item.Email, Plan: item.Plan, AccountID: item.AccountID,
		IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
		ExpiresAt: item.ExpiresAt, LastRefresh: item.LastRefresh, AuthorizedAt: item.AuthorizedAt,
		SubscriptionActiveStart: item.SubscriptionActiveStart, SubscriptionActiveUntil: item.SubscriptionActiveUntil,
		NeedsReauth: item.NeedsReauth,
	}
	if created && (item.AuthorizedAt == nil || item.AuthorizedAt.IsZero()) {
		item.AuthorizedAt = &now
	}
	if meta.Email != "" {
		item.Email = meta.Email
	}
	if meta.Plan != "" {
		item.Plan = meta.Plan
	}
	if !meta.SubscriptionActiveStart.IsZero() {
		value := meta.SubscriptionActiveStart
		item.SubscriptionActiveStart = &value
	}
	if !meta.SubscriptionActiveUntil.IsZero() {
		value := meta.SubscriptionActiveUntil
		item.SubscriptionActiveUntil = &value
	}
	item.AccountID = meta.AccountID
	if tokens.IDToken != "" {
		item.IDToken = tokens.IDToken
	}
	item.AccessToken = tokens.AccessToken
	item.RefreshToken = tokens.RefreshToken
	item.ExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	item.LastRefresh = now
	item.NeedsReauth = false
	if err := m.persistLocked(); err != nil {
		if created {
			delete(m.accounts, item.ID)
		} else {
			item.Email = previous.Email
			item.Plan = previous.Plan
			item.AccountID = previous.AccountID
			item.IDToken = previous.IDToken
			item.AccessToken = previous.AccessToken
			item.RefreshToken = previous.RefreshToken
			item.ExpiresAt = previous.ExpiresAt
			item.LastRefresh = previous.LastRefresh
			item.AuthorizedAt = previous.AuthorizedAt
			item.SubscriptionActiveStart = previous.SubscriptionActiveStart
			item.SubscriptionActiveUntil = previous.SubscriptionActiveUntil
			item.NeedsReauth = previous.NeedsReauth
		}
		return nil, err
	}
	return item, nil
}

func (m *Manager) finishLogin(sessionID string, view *AccountView, message string, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil {
		return
	}
	if success {
		session.Status = "completed"
		session.Account = view
	} else {
		session.Status = "error"
		session.Message = message
	}
	session.ExpiresAt = time.Now().Add(loginTTL)
}

func (m *Manager) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return fmt.Errorf("创建账户数据目录失败: %w", err)
	}
	accounts := make([]*account, 0, len(m.accounts))
	for _, item := range m.accounts {
		accounts = append(accounts, item)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	data, err := json.MarshalIndent(diskStore{Version: 1, Accounts: accounts}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账户数据失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".chatgpt-accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("创建账户临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置账户文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入账户文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步账户文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭账户文件失败: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("替换账户文件失败: %w", err)
	}
	if err := os.Chmod(m.path, 0o600); err != nil {
		return fmt.Errorf("设置账户文件权限失败: %w", err)
	}
	return nil
}

func (m *Manager) cleanupSessionsLocked(now time.Time) error {
	for id, session := range m.sessions {
		if now.After(session.ExpiresAt) {
			delete(m.stateToID, session.State)
			delete(m.sessions, id)
		}
	}
	return nil
}

func viewOf(item *account) AccountView {
	status := "ready"
	if item.NeedsReauth || (item.RefreshToken == "" && (item.ExpiresAt.IsZero() || !item.ExpiresAt.After(time.Now()))) {
		status = "reauthorization_required"
	}
	plan := item.Plan
	if item.Usage != nil && item.Usage.PlanType != "" {
		plan = item.Usage.PlanType
	}
	view := AccountView{ID: item.ID, Email: item.Email, Plan: plan, Status: status, Usage: cloneUsage(item.Usage)}
	if !item.ExpiresAt.IsZero() {
		view.ExpiresAt = item.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if !item.LastRefresh.IsZero() {
		view.LastRefresh = item.LastRefresh.UTC().Format(time.RFC3339)
	}
	if item.AuthorizedAt != nil && !item.AuthorizedAt.IsZero() {
		view.AuthorizedAt = item.AuthorizedAt.UTC().Format(time.RFC3339)
	}
	if item.SubscriptionActiveStart != nil && !item.SubscriptionActiveStart.IsZero() {
		view.SubscriptionActiveStart = item.SubscriptionActiveStart.UTC().Format(time.RFC3339)
	}
	if item.SubscriptionActiveUntil != nil && !item.SubscriptionActiveUntil.IsZero() {
		view.SubscriptionActiveUntil = item.SubscriptionActiveUntil.UTC().Format(time.RFC3339)
	}
	return view
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 OAuth 随机值失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成账户 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

type jwtClaims struct {
	Root map[string]any
	Auth map[string]any
}

func tokenClaims(tokens ...string) jwtClaims {
	combined := jwtClaims{Root: map[string]any{}, Auth: map[string]any{}}
	for _, token := range tokens {
		parts := strings.Split(token, ".")
		if len(parts) < 2 {
			continue
		}
		data, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var root map[string]any
		if json.Unmarshal(data, &root) != nil {
			continue
		}
		for key, value := range root {
			if _, exists := combined.Root[key]; !exists {
				combined.Root[key] = value
			}
		}
		if auth, ok := root[chatGPTIssuer].(map[string]any); ok {
			for key, value := range auth {
				if _, exists := combined.Auth[key]; !exists {
					combined.Auth[key] = value
				}
			}
		}
	}
	return combined
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func claimTime(values ...any) time.Time {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			text := strings.TrimSpace(typed)
			if text == "" {
				continue
			}
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return parsed.UTC()
			}
			if seconds, err := strconv.ParseFloat(text, 64); err == nil && seconds > 0 {
				return unixTime(seconds)
			}
		case float64:
			if typed > 0 {
				return unixTime(typed)
			}
		case json.Number:
			if seconds, err := typed.Float64(); err == nil && seconds > 0 {
				return unixTime(seconds)
			}
		}
	}
	return time.Time{}
}

func writeCallbackPage(w http.ResponseWriter, message string, success bool) {
	color := "#f87171"
	if success {
		color = "#34d399"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>ChatGPT 授权</title><body style="margin:0;background:#111827;color:#f9fafb;font:16px system-ui;display:grid;min-height:100vh;place-items:center"><main style="max-width:32rem;padding:2rem;text-align:center"><h1 style="color:%s">%s</h1><p>请返回 ai-gateway 管理页面查看授权状态。</p></main></body></html>`, color, html.EscapeString(message))
}
