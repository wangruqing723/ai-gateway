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
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	oauthClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultOAuthBaseURL  = "https://auth.openai.com"
	defaultUsageURL      = "https://chatgpt.com/backend-api/wham/usage"
	chatGPTIssuer        = "https://api.openai.com/auth"
	loginTTL             = 10 * time.Minute
	refreshBefore        = 2 * time.Minute
	maxOAuthBody         = 1 << 20
	maxUsageBody         = 1 << 20
	usageTimeout         = 15 * time.Second
	usageCacheTTL        = 30 * time.Minute
	usageWorkers         = 3
	maxAccountTags       = 20
	maxAccountTagLength  = 32
	maxAccountNoteLength = 2000
)

var ErrAccountNotFound = errors.New("ChatGPT OAuth 账户不存在")
var ErrInvalidOAuthCallback = errors.New("ChatGPT OAuth 回调链接无效或与当前授权会话不匹配")
var ErrInvalidAccountMetadata = errors.New("ChatGPT OAuth 账户标签或备注无效")
var ErrAccountMetadataPersist = errors.New("ChatGPT OAuth 账户元数据保存失败")

// ErrReauthorizationRequired 表示账户没有可用的刷新凭据或已被标记需要重新授权，不发起网络请求。
var ErrReauthorizationRequired = errors.New("ChatGPT OAuth 账户需要重新授权")

// ErrCredentialsReplaced 表示刷新交换期间凭据被导入更新或重新授权替换，且替换后的访问令牌不可直接使用。
var ErrCredentialsReplaced = errors.New("ChatGPT OAuth 凭据在刷新期间已被替换，请重试")

// errRefreshPersist 包装刷新成功但新令牌写盘失败的情况；新令牌仍保留在内存中。
var errRefreshPersist = errors.New("保存刷新后的 ChatGPT OAuth 令牌失败")

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
	Tags                    []string       `json:"tags,omitempty"`
	Note                    string         `json:"note,omitempty"`
	Usage                   *UsageSnapshot `json:"usage,omitempty"`
	Providers               []string       `json:"providers,omitempty"`

	// 以下三个维度分别描述授权、用量查询和真实调用；旧 status 字段保留原兼容含义。
	Authorization AuthorizationView `json:"authorization"`
	UsageState    UsageStateView    `json:"usageState"`
	Invocation    InvocationView    `json:"invocation"`
	// Enabled 是管理启停状态：false 表示账户已暂停，不准入新请求；不改变 Provider 的 enabled。
	Enabled  bool   `json:"enabled"`
	PausedAt string `json:"pausedAt,omitempty"`
	// CooldownUntil 是共享额度冷却的截止时间，只在冷却未到期时出现。
	CooldownUntil string `json:"cooldownUntil,omitempty"`
	// Background 是后台刷新调度状态，只在任一后台功能运行时出现。
	Background *BackgroundView `json:"background,omitempty"`
}

// AccountMetadataPatch 是对账户本地标签与备注的部分更新。
type AccountMetadataPatch struct {
	Tags *[]string `json:"tags"`
	Note *string   `json:"note"`
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
	Tags                    []string       `json:"tags,omitempty"`
	Note                    string         `json:"note,omitempty"`
	Usage                   *UsageSnapshot `json:"usage,omitempty"`
	NeedsReauth             bool           `json:"needsReauth,omitempty"`
	// Paused 是用户显式设置的管理暂停；旧文件缺失时按启用兼容。不由网络错误自动修改。
	Paused    bool       `json:"paused,omitempty"`
	PausedAt  *time.Time `json:"pausedAt,omitempty"`
	refreshMu contextMutex
	usageMu   contextMutex
	// watchers 是等待准入的暂停监听，只属于当前进程。
	watchers pauseWatchers
	// cooldownUntil 是共享额度冷却截止时间，cooldownFloor 是手动解除或关闭冷却时的请求序号下限，
	// 不大于它的请求迟到的额度错误不再生效。两者只在内存中，由 m.mu 保护。
	cooldownUntil time.Time
	cooldownFloor uint64

	// 以下是只属于当前进程的运行状态，不写入账户文件（未导出字段不参与 JSON）。
	// lastRefreshFailure 与 credentialsUnsaved 由 m.mu 保护。
	lastRefreshFailure *refreshFailure
	credentialsUnsaved bool
	// 调用记录由 m.invocationMu 保护，与 m.mu 分开：每次真实上游尝试都会写，
	// 不能排在账户文件 fsync 后面。
	invocation          *invocationRecord
	providerInvocations map[string]*invocationRecord
	// invocationFloor 是凭据被外部替换（重新授权）时的序号；不大于它的在途尝试结果
	// 属于旧凭据，迟到后不能写进新凭据的调用状态。
	invocationFloor uint64
	// awaitingRecovery 表示异常账户已替换凭据，等待之后的第一次成功调用产生恢复事件。由 m.invocationMu 保护。
	awaitingRecovery bool
	// alertKind 是当前授权或刷新告警，用于只在状态转移时产生事件。由 m.mu 保护。
	alertKind string
	// stats 是当前进程内的调用统计，由 m.statsMu 保护。
	stats *accountStats
	// credentialVersion 在每次凭据写入（自动轮换、重新授权、显式导入更新）时递增，由 m.mu 保护。
	// 刷新写回与导入应用都按它做比较后交换，避免在途刷新把新导入的凭据覆盖回旧值。
	credentialVersion uint64
	// background 是后台调度状态，由 m.bgMu 保护。
	background backgroundState
}

// refreshFailure 记录最近一次自动刷新失败的安全类别，不含上游正文或令牌。
type refreshFailure struct {
	At         time.Time
	Category   string
	HTTPStatus int
}

func refreshFailureOf(err error, at time.Time) refreshFailure {
	failure := refreshFailure{At: at, Category: "unknown"}
	var exchangeErr *oauthExchangeFailure
	if errors.As(err, &exchangeErr) {
		failure.Category = exchangeErr.category
		failure.HTTPStatus = exchangeErr.httpStatus
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.Category = "timeout"
	}
	return failure
}

// refreshFailureMessage 把刷新失败类别转换成管理页面可展示的固定文案。
func refreshFailureMessage(failure *refreshFailure) string {
	switch failure.Category {
	case "invalid_grant":
		return "刷新凭据已被 ChatGPT 拒绝，需要重新授权"
	case "transport":
		return "最近一次自动刷新无法连接 OAuth 服务，可能是网络或代理问题；下次使用时会重试"
	case "timeout":
		return "最近一次自动刷新超时；下次使用时会重试"
	case "missing_tokens", "invalid_response", "response_read":
		return "最近一次自动刷新收到无法识别的 OAuth 响应；下次使用时会重试"
	}
	message := "最近一次自动刷新失败"
	if failure.HTTPStatus > 0 {
		message += fmt.Sprintf("（HTTP %d）", failure.HTTPStatus)
	}
	return message + "；下次使用时会重试"
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

	// invocationMu 保护各账户的调用记录；锁顺序为先 mu 后 invocationMu。
	invocationMu  sync.Mutex
	invocationSeq atomic.Uint64

	// statsMu 保护各账户统计与窗口设置，是叶子锁：持有时不再获取其他锁。
	statsMu            sync.Mutex
	statsWindowMinutes int
	statsSince         time.Time
	// sharedCooldown 是共享额度冷却开关，由 m.mu 保护。
	sharedCooldown bool

	// 后台刷新（background.go）。bgCtlMu 串行化启停；bgMu 是叶子锁，保护 bg 与各账户 background；
	// bgSettings 是正在运行的设置，未运行时为 nil，供视图无锁读取。
	bgCtlMu    sync.Mutex
	bgMu       sync.Mutex
	bg         backgroundRuntime
	bgSettings atomic.Pointer[BackgroundSettings]

	// activeWatches 是正在等待准入的监听数；watchPolling 表示共享文件轮询正在运行，由 watchPollMu 保护。
	activeWatches atomic.Int64
	watchPollMu   sync.Mutex
	watchPolling  bool
	// eventHandler 接收账户状态事件，由网关写入运行事件流。
	eventHandler atomic.Pointer[func(Event)]

	// versionEpoch 每次进程启动随机生成，versionSeq 在账户集合、元数据或凭据被显式替换时递增，
	// 两者组成账户控制版本。用量轮询和普通令牌轮换不推进它。均由 mu 保护。
	versionEpoch string
	versionSeq   uint64

	// 跨进程存储协调（store.go）。storeMu 是进程内存储锁，storeHeld 与 lockFile 只在持有它时改写；
	// diskSum、diskLoaded 由存储锁与 m.mu 共同保护；diskStamp、lastSharedSync 供只读路径无锁读取。
	storeMu        contextMutex
	storeHeld      atomic.Bool
	lockFile       *os.File
	diskSum        [32]byte
	diskLoaded     bool
	diskStamp      atomic.Pointer[storeStamp]
	lastSharedSync atomic.Int64

	// 以下字段只在包内测试中替换：时钟、OAuth 与用量端点、落盘替换前的故障注入、
	// 共享文件检查间隔（负数表示每次都检查）与存储锁等待上限。
	syncIntervalOverride time.Duration
	storeWaitOverride    time.Duration
	// lockingOverride 模拟平台是否支持跨进程锁，tickOverride 替换后台检查周期。
	lockingOverride *bool
	tickOverride    time.Duration
	// backupIterations 降低测试中的备份密钥派生次数；零值使用 backupcrypto.DefaultIterations。
	backupIterations int
	// 生产路径保持零值，由对应 helper 回落到真实时钟和固定上游地址。
	clock         func() time.Time
	oauthBase     string
	usageEndpoint string
	beforeReplace func() error
}

func (m *Manager) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// Version 返回账户控制版本，作为账户管理接口的 ETag。值不含任何凭据信息，进程重启后必然变化。
func (m *Manager) Version() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.versionLocked()
}

// ListWithVersion 在同一把读锁内返回账户列表和对应的控制版本。
func (m *Manager) ListWithVersion(references map[string][]string) ([]AccountView, string) {
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listLocked(references), m.versionLocked()
}

func (m *Manager) versionLocked() string {
	return fmt.Sprintf("%s.%d", m.versionEpoch, m.versionSeq)
}

// bumpVersionLocked 在账户控制状态成功落盘后调用。调用方持有 m.mu 写锁。
func (m *Manager) bumpVersionLocked() {
	m.versionSeq++
}

func (m *Manager) oauthBaseURL() string {
	if m.oauthBase != "" {
		return m.oauthBase
	}
	return defaultOAuthBaseURL
}

func (m *Manager) usageURL() string {
	if m.usageEndpoint != "" {
		return m.usageEndpoint
	}
	return defaultUsageURL
}

// New 从路径加载 OAuth 账户。令牌文件只允许当前进程用户读写。
func New(path string, client *http.Client) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("ChatGPT OAuth 存储路径为空")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	epoch, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		path:         path,
		accounts:     make(map[string]*account),
		sessions:     make(map[string]*loginSession),
		stateToID:    make(map[string]string),
		client:       client,
		versionEpoch: epoch,
		versionSeq:   1,

		statsWindowMinutes: defaultStatsWindowMinutes,
		statsSince:         time.Now().UTC(),
	}
	if _, err := os.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 ChatGPT OAuth 账户文件失败: %w", err)
	} else if err == nil {
		// 与写入入口共用同一条加锁读取链路，避免读到另一实例正在替换的中间状态。
		ctx, cancel := context.WithTimeout(context.Background(), defaultStoreWait)
		defer cancel()
		if err := m.acquireStore(ctx); err != nil {
			return nil, err
		}
		m.mu.Lock()
		err := m.syncFromDiskLocked()
		m.mu.Unlock()
		m.releaseStore()
		if err != nil {
			return nil, err
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
	m.cleanupSessionsLocked(m.now())
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
	expiresAt := m.now().Add(loginTTL)
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
	authURL := m.oauthBaseURL() + "/oauth/authorize?" + query.Encode()
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
	m.cleanupSessionsLocked(m.now())
	session, ok := m.sessions[sessionID]
	if !ok {
		return LoginStatus{}, false
	}
	return LoginStatus{Status: session.Status, Message: session.Message, Account: session.Account}, true
}

// List 返回账户元信息及各账户绑定的 Provider 名称。
func (m *Manager) List(references map[string][]string) []AccountView {
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listLocked(references)
}

func (m *Manager) listLocked(references map[string][]string) []AccountView {
	out := make([]AccountView, 0, len(m.accounts))
	for _, item := range m.accounts {
		view := m.viewOf(item)
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

// RefreshUsage 刷新各账户的用量快照。force=false 时会复用半小时内的结果。
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
	return m.viewOf(item), nil
}

// refreshAccountUsage 查询并保存单账户用量，返回不含令牌与上游正文的安全结论。
// 调用方主动取消（客户端断开、诊断取消）时不改写已有快照：取消不说明用量服务有问题。
func (m *Manager) refreshAccountUsage(ctx context.Context, item *account, force bool) CheckOutcome {
	if err := item.usageMu.Lock(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return CheckOutcome{Category: "cancelled", Message: "等待同账户用量查询时已取消，保留原有快照"}
		}
		return CheckOutcome{Category: "timeout", Message: "等待同账户用量查询超时，保留原有快照"}
	}
	defer item.usageMu.Unlock()

	now := m.now().UTC()
	m.mu.RLock()
	if m.accounts[item.ID] != item {
		m.mu.RUnlock()
		return CheckOutcome{Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
	}
	lastUsage := cloneUsage(item.Usage)
	m.mu.RUnlock()
	if !force && lastUsage != nil {
		lastAttemptAt := lastUsage.LastAttemptAt
		if lastAttemptAt == "" {
			lastAttemptAt = lastUsage.LastCheckedAt
		}
		if checkedAt, err := time.Parse(time.RFC3339, lastAttemptAt); err == nil && now.Sub(checkedAt) < usageCacheTTL {
			return CheckOutcome{Message: "缓存周期内复用已有用量快照，未重新查询", Cached: true}
		}
	}

	requestCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	accessToken, accountID, exchanged, err := m.accessToken(requestCtx, item.ID, m.client, false)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return CheckOutcome{Category: "cancelled", Message: "用量查询已取消，保留原有用量快照", Exchanged: exchanged}
		}
		message := "无法获取访问令牌，请检查 OAuth 代理或网络设置后重试"
		if errors.Is(err, errInvalidGrant) || errors.Is(err, ErrReauthorizationRequired) {
			message = "OAuth 刷新凭据已失效，请重新授权"
		}
		m.setUsageError(item, message, 0)
		outcome := DescribeCredentialError(err)
		outcome.Exchanged = exchanged
		return outcome
	}
	usage, err := m.fetchUsage(requestCtx, accessToken, accountID)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return CheckOutcome{Category: "cancelled", Message: "用量查询已取消，保留原有用量快照", Exchanged: exchanged}
		}
		m.recordStat(item, func(c *statCounters) { c.usageFailures++ })
		m.setUsageError(item, usageErrorMessage(err), usageRetryAfterOf(err))
		outcome := usageFailureOutcome(err)
		outcome.Exchanged = exchanged
		return outcome
	}
	m.recordStat(item, func(c *statCounters) { c.usageSuccesses++ })
	checkedAt := m.now().UTC().Format(time.RFC3339)
	usage.LastAttemptAt = checkedAt
	usage.LastCheckedAt = checkedAt
	usage.LastSuccessAt = checkedAt
	unlock := m.lockForCommit()
	defer unlock()
	if m.accounts[item.ID] != item {
		return CheckOutcome{Category: "not_found", Message: "ChatGPT OAuth 账户在查询期间已被删除", Exchanged: exchanged}
	}
	item.Usage = &usage
	m.persistUsageLocked()
	m.noteUsageResult(item, true, 0)
	return CheckOutcome{Message: usageSuccessMessage(usage), Exchanged: exchanged}
}

func (m *Manager) setUsageError(item *account, message string, retryAfter time.Duration) {
	m.noteUsageResult(item, false, retryAfter)
	unlock := m.lockForCommit()
	defer unlock()
	if m.accounts[item.ID] != item {
		return
	}
	usage := cloneUsage(item.Usage)
	if usage == nil {
		usage = &UsageSnapshot{}
	}
	checkedAt := m.now().UTC().Format(time.RFC3339)
	usage.LastAttemptAt = checkedAt
	usage.LastCheckedAt = checkedAt
	usage.Error = message
	item.Usage = usage
	m.persistUsageLocked()
}

// lockForCommit 为已完成的网络结果（用量快照、令牌交换结果）写回取得写锁，不再受请求 context 约束。
// 等不到跨进程存储锁时只持有 m.mu、不写盘：用量快照只更新内存，交换得到的新凭据标记为未保存，
// 都不会因另一实例的长写入而丢失。
func (m *Manager) lockForCommit() func() {
	if unlock, err := m.lockForWrite(context.Background()); err == nil {
		return unlock
	} else {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth account store busy, keeping result in memory: %v\n", err)
	}
	m.mu.Lock()
	return m.mu.Unlock
}

// persistUsageLocked 将不含凭据的额度快照与账户状态一起原子保存。
// 当前调用方持有 m.mu 写锁；缓存落盘失败不影响刚查询到的内存快照。
func (m *Manager) persistUsageLocked() {
	if !m.storeHeld.Load() {
		return
	}
	if err := m.persistLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT usage cache persistence failed: %v\n", err)
	}
}

type usageHTTPError struct {
	status int
	// retryAfter 是上游 Retry-After，推迟后台下一次用量刷新。
	retryAfter time.Duration
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.usageURL(), nil)
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
		return UsageSnapshot{}, &usageHTTPError{status: resp.StatusCode, retryAfter: usageRetryAfter(resp.Header.Get("Retry-After"), m.now())}
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
	return payload.snapshot(m.now().UTC()), nil
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
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.accounts[id]
	if !ok {
		return AccountView{}, false
	}
	return m.viewOf(item), true
}

// UpdateMetadata 修改账户标签和备注，并通过原子文件替换持久化。
func (m *Manager) UpdateMetadata(id string, patch AccountMetadataPatch) (AccountView, error) {
	view, _, err := m.UpdateMetadataVersioned(id, patch)
	return view, err
}

// UpdateMetadataVersioned 与 UpdateMetadata 相同，另外返回写入后的账户控制版本。
// 版本在同一把写锁内读取，不会混入之后并发写入推进的版本。
func (m *Manager) UpdateMetadataVersioned(id string, patch AccountMetadataPatch) (AccountView, string, error) {
	if patch.Tags == nil && patch.Note == nil {
		return AccountView{}, "", ErrInvalidAccountMetadata
	}

	var tags []string
	if patch.Tags != nil {
		var err error
		tags, err = normalizeAccountTags(*patch.Tags)
		if err != nil {
			return AccountView{}, "", err
		}
	}
	if patch.Note != nil && utf8.RuneCountInString(*patch.Note) > maxAccountNoteLength {
		return AccountView{}, "", fmt.Errorf("%w: 备注最多 %d 个字符", ErrInvalidAccountMetadata, maxAccountNoteLength)
	}

	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return AccountView{}, "", fmt.Errorf("%w: %w", ErrAccountMetadataPersist, err)
	}
	defer unlock()
	item := m.accounts[id]
	if item == nil {
		return AccountView{}, "", ErrAccountNotFound
	}

	previousTags := append([]string(nil), item.Tags...)
	previousNote := item.Note
	if patch.Tags != nil {
		item.Tags = tags
	}
	if patch.Note != nil {
		item.Note = *patch.Note
	}
	if err := m.persistLocked(); err != nil {
		item.Tags = previousTags
		item.Note = previousNote
		return AccountView{}, "", fmt.Errorf("%w: %v", ErrAccountMetadataPersist, err)
	}
	m.bumpVersionLocked()
	return m.viewOf(item), m.versionLocked(), nil
}

func normalizeAccountTags(input []string) ([]string, error) {
	if len(input) > maxAccountTags {
		return nil, fmt.Errorf("%w: 标签最多 %d 个", ErrInvalidAccountMetadata, maxAccountTags)
	}

	tags := make([]string, 0, len(input))
	for _, tag := range input {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > maxAccountTagLength {
			return nil, fmt.Errorf("%w: 每个标签最多 %d 个字符", ErrInvalidAccountMetadata, maxAccountTagLength)
		}
		duplicate := false
		for _, existing := range tags {
			if strings.EqualFold(existing, tag) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}

// Delete 删除本地 OAuth 凭据。调用方负责先确认账户未被 Provider 引用。
func (m *Manager) Delete(id string) error {
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok := m.accounts[id]; !ok {
		return ErrAccountNotFound
	}
	item := m.accounts[id]
	delete(m.accounts, id)
	if err := m.persistLocked(); err != nil {
		m.accounts[id] = item
		return err
	}
	m.bumpVersionLocked()
	return nil
}

// AccessToken 返回可用的 access token 与 ChatGPT 账户请求头值，必要时刷新并持久化。
func (m *Manager) AccessToken(ctx context.Context, id string, client *http.Client) (string, string, error) {
	token, accountID, _, err := m.accessToken(ctx, id, client, false)
	return token, accountID, err
}

// RefreshCredentials 供管理诊断使用：force 为 true 时即使访问令牌仍有效也执行一次刷新交换。
// 返回值 exchanged 表示本次是否真的向 OAuth 服务交换了令牌。
func (m *Manager) RefreshCredentials(ctx context.Context, id string, client *http.Client, force bool) (bool, error) {
	_, _, exchanged, err := m.accessToken(ctx, id, client, force)
	return exchanged, err
}

func (m *Manager) accessToken(ctx context.Context, id string, client *http.Client, force bool) (string, string, bool, error) {
	return m.accessTokenLead(ctx, id, client, force, refreshBefore)
}

// tokenState 是一次取令牌判断所需的凭据快照。
type tokenState struct {
	access, refresh, accountID string
	expiresAt                  time.Time
	needsReauth                bool
	version                    uint64
}

// readTokenState 在读锁内读取凭据快照；账户已删除或被替换为新对象时返回 false。
func (m *Manager) readTokenState(id string, item *account) (tokenState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.accounts[id] != item {
		return tokenState{}, false
	}
	return tokenState{
		access: item.AccessToken, refresh: item.RefreshToken, accountID: item.AccountID,
		expiresAt: item.ExpiresAt, needsReauth: item.NeedsReauth, version: item.credentialVersion,
	}, true
}

// usable 判断不交换也能直接使用的访问令牌。lead 是提前刷新的余量：请求路径用 refreshBefore，
// 后台预刷新用更长的余量。force 只对有 refresh token 的账户生效（诊断强制交换）。
func (s tokenState) usable(now time.Time, force bool, lead time.Duration) bool {
	if s.needsReauth || s.access == "" || (force && s.refresh != "") {
		return false
	}
	if s.refresh == "" {
		return now.Before(s.expiresAt)
	}
	return s.expiresAt.Sub(now) > lead
}

// accessTokenLead 是取令牌的完整链路。需要交换时在跨进程存储锁内完成「重新读取 → 判断 → 交换 → 写回」：
// 另一实例已经轮换过时直接使用新令牌，不再用已被消费的旧 refresh token 交换（DESIGN 7.1）。
func (m *Manager) accessTokenLead(ctx context.Context, id string, client *http.Client, force bool, lead time.Duration) (string, string, bool, error) {
	m.syncShared()
	m.mu.RLock()
	item, ok := m.accounts[id]
	m.mu.RUnlock()
	if !ok {
		return "", "", false, ErrAccountNotFound
	}
	if err := item.refreshMu.Lock(ctx); err != nil {
		return "", "", false, err
	}
	defer item.refreshMu.Unlock()
	state, ok := m.readTokenState(id, item)
	if !ok {
		return "", "", false, ErrAccountNotFound
	}
	if state.usable(m.now(), force, lead) {
		return state.access, state.accountID, false, nil
	}
	if state.refresh == "" || state.needsReauth {
		return "", "", false, ErrReauthorizationRequired
	}
	releaseLease, err := m.acquireRefreshLease(ctx, id)
	if err != nil {
		return "", "", false, err
	}
	defer releaseLease()
	if err := m.syncStore(ctx); err != nil {
		return "", "", false, err
	}
	// 合并磁盘后再判断一次：另一实例可能刚刚完成轮换。force 的诊断交换仍执行一次真实交换。
	state, ok = m.readTokenState(id, item)
	if !ok {
		return "", "", false, ErrAccountNotFound
	}
	if state.usable(m.now(), force, lead) {
		return state.access, state.accountID, false, nil
	}
	if state.refresh == "" || state.needsReauth {
		return "", "", false, ErrReauthorizationRequired
	}
	// readVersion 是本次读取的凭据世代；交换期间凭据若被导入更新或重新授权替换，写回时放弃。
	refreshToken, accountID, readVersion := state.refresh, state.accountID, state.version
	now := m.now()
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tokens, err := m.exchange(refreshCtx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {oauthClientID},
	}, client)
	if err != nil {
		// 调用方自己取消（客户端断开、诊断取消）不代表刷新链路有问题，不记失败。
		// 交换期间凭据已被替换时，失败属于旧凭据，同样不能记到新凭据上。
		if ctx.Err() == nil {
			m.recordStat(item, func(c *statCounters) { c.refreshFailures++ })
			failure := refreshFailureOf(err, m.now().UTC())
			unlock := m.lockForCommit()
			if m.accounts[id] == item {
				if item.credentialVersion != readVersion {
					// 失败属于已被替换的旧凭据；替换后的访问令牌可用时直接使用。
					if !item.NeedsReauth && item.AccessToken != "" && item.ExpiresAt.After(m.now()) {
						token, remoteID := item.AccessToken, item.AccountID
						unlock()
						return token, remoteID, true, nil
					}
				} else {
					item.lastRefreshFailure = &failure
					if errors.Is(err, errInvalidGrant) {
						item.NeedsReauth = true
						m.persistOrMarkUnsavedLocked(item)
						m.raiseAlertLocked(item, alertReauth)
					} else {
						m.raiseAlertLocked(item, alertRefresh)
					}
				}
			}
			unlock()
		}
		return "", "", true, fmt.Errorf("刷新 ChatGPT OAuth 令牌失败: %w", err)
	}
	m.recordStat(item, func(c *statCounters) { c.refreshSuccesses++ })
	claims := tokenClaims(tokens.IDToken, tokens.AccessToken)
	refreshedAccountID := firstString(claims.Auth["chatgpt_account_id"], claims.Auth["account_id"], claims.Root["chatgpt_account_id"])
	metadata := metadataFromClaims(claims)
	if refreshedAccountID == "" {
		refreshedAccountID = accountID
	}
	now = m.now().UTC()
	// 写回前合并磁盘：交换期间另一实例的导入或重新授权会推进凭据世代，下面按世代放弃本次结果。
	unlock := m.lockForCommit()
	defer unlock()
	if m.accounts[id] != item {
		return "", "", true, ErrAccountNotFound
	}
	if item.credentialVersion != readVersion {
		// 交换期间凭据被显式替换（导入更新或重新授权）：新凭据优先，丢弃本次交换结果。
		// 替换后的访问令牌仍有效时直接使用它，否则让调用方按失败处理并稍后重试。
		if !item.NeedsReauth && item.AccessToken != "" && item.ExpiresAt.After(now) {
			return item.AccessToken, item.AccountID, true, nil
		}
		return "", "", true, ErrCredentialsReplaced
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
	item.lastRefreshFailure = nil
	m.clearRefreshAlertLocked(item)
	// 普通轮换只推进凭据世代，不推进账户控制版本：用户看到的账户集合与元数据没有变化。
	item.credentialVersion++
	// 上游可能已轮换 refresh token，落盘失败时不能回滚到旧凭据：内存保留新令牌，
	// 并标记「凭据未保存」，直到之后任一次落盘成功；本次调用仍按失败返回。
	if err := m.persistLocked(); err != nil {
		item.credentialsUnsaved = true
		return "", "", true, fmt.Errorf("%w: %w", errRefreshPersist, err)
	}
	m.noteTokenSuccess(item)
	return item.AccessToken, item.AccountID, true, nil
}

// persistOrMarkUnsavedLocked 落盘失败时只标记账户凭据状态未保存，不回滚内存。
// 调用方持有 m.mu 写锁。
func (m *Manager) persistOrMarkUnsavedLocked(item *account) {
	if err := m.persistLocked(); err != nil {
		item.credentialsUnsaved = true
		fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth account state persistence failed: %v\n", err)
	}
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
	_ = m.cleanupSessionsLocked(m.now())
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.oauthBaseURL()+"/oauth/token", strings.NewReader(form.Encode()))
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
	now := m.now().UTC()
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return nil, err
	}
	defer unlock()
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
	previousFailure := item.lastRefreshFailure
	item.lastRefreshFailure = nil
	if err := m.persistLocked(); err != nil {
		item.lastRefreshFailure = previousFailure
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
	if !created {
		// 重新授权是外部凭据替换：旧凭据的调用结果不再描述当前凭据。
		m.resetInvocationsLocked(item)
	}
	item.credentialVersion++
	m.bumpVersionLocked()
	if !created {
		m.emitCredentialsUpdatedLocked([]*account{item}, "重新授权")
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
	session.ExpiresAt = m.now().Add(loginTTL)
}

func (m *Manager) persistLocked() error {
	if !m.storeHeld.Load() {
		// 写入前必须经 lockForWrite 合并磁盘内容，否则会覆盖另一实例的修改。
		return errors.New("内部错误：写入 ChatGPT OAuth 账户文件前未取得存储锁")
	}
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
	if m.beforeReplace != nil {
		if err := m.beforeReplace(); err != nil {
			return fmt.Errorf("替换账户文件失败: %w", err)
		}
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("替换账户文件失败: %w", err)
	}
	if err := os.Chmod(m.path, 0o600); err != nil {
		return fmt.Errorf("设置账户文件权限失败: %w", err)
	}
	m.rememberDiskLocked(data)
	// 每次落盘写入全部账户的当前内存状态，之前未保存的凭据此刻也已落盘。
	for _, item := range m.accounts {
		item.credentialsUnsaved = false
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

// viewOf 生成不含令牌的账户视图。调用方持有 m.mu（读锁或写锁）。
func (m *Manager) viewOf(item *account) AccountView {
	now := m.now()
	status := "ready"
	if item.NeedsReauth || (item.RefreshToken == "" && (item.ExpiresAt.IsZero() || !item.ExpiresAt.After(now))) {
		status = "reauthorization_required"
	}
	plan := item.Plan
	if item.Usage != nil && item.Usage.PlanType != "" {
		plan = item.Usage.PlanType
	}
	view := AccountView{
		ID: item.ID, Email: item.Email, Plan: plan, Status: status,
		Tags: append([]string(nil), item.Tags...), Note: item.Note, Usage: cloneUsage(item.Usage),
		Authorization: authorizationViewOf(item, now),
		UsageState:    usageStateViewOf(item.Usage, now),
		Invocation:    m.invocationViewLocked(item),
		Enabled:       !item.Paused,
	}
	if item.Paused && item.PausedAt != nil && !item.PausedAt.IsZero() {
		view.PausedAt = item.PausedAt.UTC().Format(time.RFC3339)
	}
	if until := m.cooldownUntilLocked(item); !until.IsZero() {
		view.CooldownUntil = until.UTC().Format(time.RFC3339)
	}
	view.Background = m.backgroundViewLocked(item, now)
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
