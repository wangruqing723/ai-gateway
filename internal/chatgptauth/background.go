package chatgptauth

import (
	"context"
	"errors"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 后台刷新的固定口径（DESIGN 7.1）。周期类数值由配置传入，这里只放不可配置的部分。
const (
	backgroundTick        = 30 * time.Second
	backgroundTasksPerRun = 10
	backgroundWorkers     = 2
	backgroundTaskBudget  = 45 * time.Second
	// backgroundTokenLead 是后台预刷新的余量：访问令牌剩余有效期不超过它就提前交换。
	backgroundTokenLead     = 10 * time.Minute
	backgroundTokenBackoff  = time.Minute
	backgroundTokenMaxDelay = 60 * time.Minute
	backgroundUsageMaxDelay = 24 * time.Hour
)

// ErrBackgroundUnsupported 表示当前平台没有跨进程文件锁，拒绝启用后台刷新。
var ErrBackgroundUnsupported = errors.New("当前平台不支持跨进程文件锁，未启用 ChatGPT 后台刷新")

// 后台任务跳过原因，对应账户视图 background.skipped。
const (
	BackgroundSkipInactive    = "inactive"
	BackgroundSkipPaused      = "paused"
	BackgroundSkipReauth      = "reauthorization_required"
	BackgroundSkipNoRefresh   = "no_refresh_token"
	backgroundReasonPlatform  = "unsupported_platform"
	backgroundResultOK        = "ok"
	backgroundResultError     = "error"
	backgroundResultSkipped   = "skipped"
	backgroundTaskToken       = "token"
	backgroundTaskUsage       = "usage"
	backgroundMessageCanceled = "后台任务已取消"
)

// BackgroundSettings 是后台刷新的运行设置，由网关从配置换算后传入。
type BackgroundSettings struct {
	TokenRefresh  bool
	UsageRefresh  bool
	UsageInterval time.Duration
	ActiveWindow  time.Duration
}

// Enabled 判断是否开启了任一后台功能。
func (s BackgroundSettings) Enabled() bool { return s.TokenRefresh || s.UsageRefresh }

// BackgroundStatus 是 GET /api/chatgpt/background 的返回体。
type BackgroundStatus struct {
	TokenRefresh        bool   `json:"tokenRefresh"`
	UsageRefresh        bool   `json:"usageRefresh"`
	Running             bool   `json:"running"`
	CrossProcessLocking bool   `json:"crossProcessLocking"`
	Reason              string `json:"reason"`
	LastTickAt          string `json:"lastTickAt,omitempty"`
}

// BackgroundView 是账户视图中的后台调度状态，只在任一后台功能开启时出现。
type BackgroundView struct {
	Active     bool                `json:"active"`
	LastUsedAt string              `json:"lastUsedAt,omitempty"`
	Skipped    string              `json:"skipped"`
	Token      *BackgroundTaskView `json:"token,omitempty"`
	Usage      *BackgroundTaskView `json:"usage,omitempty"`
}

// BackgroundTaskView 描述一类后台任务的上次运行、结果、下次计划与连续失败次数。
type BackgroundTaskView struct {
	LastRunAt   string `json:"lastRunAt,omitempty"`
	LastResult  string `json:"lastResult,omitempty"`
	LastMessage string `json:"lastMessage,omitempty"`
	NextAt      string `json:"nextAt,omitempty"`
	Failures    int    `json:"failures"`
}

// backgroundTask 是单类任务的进程内状态。
type backgroundTask struct {
	lastRunAt    time.Time
	lastResult   string
	lastMessage  string
	failures     int
	backoffUntil time.Time
}

// backgroundState 是账户的后台调度状态，只在当前进程内，由 m.bgMu 保护。
type backgroundState struct {
	lastUsedAt time.Time
	token      backgroundTask
	usage      backgroundTask
	// usageRetryUntil 来自任何入口拿到的用量 Retry-After，推迟后台下一次用量刷新。
	usageRetryUntil time.Time
}

// backgroundRuntime 是唯一的后台循环。字段由 m.bgMu 保护，settings 另用原子指针供视图无锁读取。
type backgroundRuntime struct {
	// requested 是最近一次传入的设置（含平台不支持而未启动的情况），只用于状态展示。
	requested  BackgroundSettings
	cancel     context.CancelFunc
	done       chan struct{}
	wake       chan struct{}
	running    bool
	reason     string
	lastTickAt time.Time
}

// laterOf 返回几个时间中最晚的一个。
func laterOf(times ...time.Time) time.Time {
	var latest time.Time
	for _, t := range times {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// usageJitter 按账户 ID 确定性地错开最多十分之一周期，避免多个账户在同一轮集中查询。
// 取整到秒，与共享文件中秒级精度的尝试时间对齐。
func usageJitter(id string, interval time.Duration) time.Duration {
	span := int64(interval / 10 / time.Second)
	if span <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(id))
	return time.Duration(hash.Sum64()%uint64(span)) * time.Second
}

// usageLastAttempt 读取共享文件中的最近一次用量尝试时间；两个实例据此互相推迟。
func usageLastAttempt(usage *UsageSnapshot) time.Time {
	if usage == nil {
		return time.Time{}
	}
	value := usage.LastAttemptAt
	if value == "" {
		value = usage.LastCheckedAt
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return at
}

// backgroundBackoff 计算第 failures 次连续失败后的退避：从 base 起翻倍，不超过 limit。
func backgroundBackoff(base, limit time.Duration, failures int) time.Duration {
	delay := base
	for i := 1; i < failures && delay < limit; i++ {
		delay *= 2
	}
	if delay > limit || delay <= 0 {
		return limit
	}
	return delay
}

// backgroundPlan 是单个账户此刻的调度结论。tokenNext、usageNext 为零值表示立即到期。
type backgroundPlan struct {
	active    bool
	skipped   string
	tokenNext time.Time
	usageNext time.Time
}

// backgroundPlanLocked 计算账户的调度结论。调用方持有 m.mu（读锁或写锁）与 m.bgMu。
func (m *Manager) backgroundPlanLocked(item *account, settings BackgroundSettings, now time.Time) backgroundPlan {
	state := &item.background
	plan := backgroundPlan{active: !state.lastUsedAt.IsZero() && now.Sub(state.lastUsedAt) <= settings.ActiveWindow}
	switch {
	case item.Paused:
		plan.skipped = BackgroundSkipPaused
	case item.NeedsReauth:
		plan.skipped = BackgroundSkipReauth
	case item.RefreshToken == "":
		plan.skipped = BackgroundSkipNoRefresh
	case !plan.active:
		plan.skipped = BackgroundSkipInactive
	}
	if settings.TokenRefresh {
		var next time.Time
		if item.AccessToken != "" && !item.ExpiresAt.IsZero() {
			next = item.ExpiresAt.Add(-backgroundTokenLead)
		}
		// 上次后台运行后至少间隔一个基础退避，防止上游给出极短有效期时每轮都交换。
		var spacing time.Time
		if !state.token.lastRunAt.IsZero() {
			spacing = state.token.lastRunAt.Add(backgroundTokenBackoff)
		}
		plan.tokenNext = laterOf(next, spacing, state.token.backoffUntil)
	}
	if settings.UsageRefresh {
		var base time.Time
		if last := usageLastAttempt(item.Usage); !last.IsZero() {
			base = last.Add(settings.UsageInterval + usageJitter(item.ID, settings.UsageInterval))
		}
		plan.usageNext = laterOf(base, state.usage.backoffUntil, state.usageRetryUntil)
	}
	return plan
}

// backgroundJob 是一轮中待执行的一个任务。
type backgroundJob struct {
	item *account
	kind string
	due  time.Time
}

// dueBackgroundJobs 选出已到期的任务，按到期时间排序，同一时刻令牌任务优先，每轮最多 10 个。
func (m *Manager) dueBackgroundJobs(settings BackgroundSettings, now time.Time) []backgroundJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	var jobs []backgroundJob
	for _, item := range m.accounts {
		plan := m.backgroundPlanLocked(item, settings, now)
		if plan.skipped != "" {
			continue
		}
		if settings.TokenRefresh && !plan.tokenNext.After(now) {
			jobs = append(jobs, backgroundJob{item: item, kind: backgroundTaskToken, due: plan.tokenNext})
		}
		if settings.UsageRefresh && !plan.usageNext.After(now) {
			jobs = append(jobs, backgroundJob{item: item, kind: backgroundTaskUsage, due: plan.usageNext})
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].due.Equal(jobs[j].due) {
			return jobs[i].due.Before(jobs[j].due)
		}
		if jobs[i].kind != jobs[j].kind {
			return jobs[i].kind == backgroundTaskToken
		}
		return jobs[i].item.ID < jobs[j].item.ID
	})
	if len(jobs) > backgroundTasksPerRun {
		jobs = jobs[:backgroundTasksPerRun]
	}
	return jobs
}

// backgroundSettingsOf 补齐未设置的周期，返回实际使用的设置。
func backgroundSettingsOf(settings BackgroundSettings) BackgroundSettings {
	if settings.UsageInterval <= 0 {
		settings.UsageInterval = 30 * time.Minute
	}
	if settings.ActiveWindow <= 0 {
		settings.ActiveWindow = 60 * time.Minute
	}
	return settings
}

// backgroundSupported 判断能否启用后台刷新：两个实例同时预刷新必须依赖跨进程锁。
func (m *Manager) backgroundSupported() bool {
	if m.lockingOverride != nil {
		return *m.lockingOverride
	}
	return crossProcessLocking
}

// SetBackground 应用后台刷新设置。热重载只更新设置并立即重算，不会出现第二个循环；
// 全部关闭时停止循环并等它退出。平台不支持跨进程锁时不启动，返回 ErrBackgroundUnsupported。
func (m *Manager) SetBackground(settings BackgroundSettings) error {
	m.bgCtlMu.Lock()
	defer m.bgCtlMu.Unlock()
	settings = backgroundSettingsOf(settings)
	m.bgMu.Lock()
	m.bg.requested = settings
	m.bgMu.Unlock()
	if !settings.Enabled() {
		m.stopBackgroundLocked("")
		return nil
	}
	if !m.backgroundSupported() {
		m.stopBackgroundLocked(backgroundReasonPlatform)
		return ErrBackgroundUnsupported
	}
	m.bgSettings.Store(&settings)
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if m.bg.running {
		select {
		case m.bg.wake <- struct{}{}:
		default:
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.bg.cancel, m.bg.done, m.bg.wake = cancel, make(chan struct{}), make(chan struct{}, 1)
	m.bg.running, m.bg.reason = true, ""
	go m.backgroundLoop(ctx, m.bg.done, m.bg.wake)
	return nil
}

// StopBackground 取消等待与出网，并等后台循环退出。网关关闭时调用。
func (m *Manager) StopBackground() {
	m.bgCtlMu.Lock()
	defer m.bgCtlMu.Unlock()
	m.stopBackgroundLocked("")
}

// stopBackgroundLocked 停止循环。调用方持有 m.bgCtlMu；等待循环退出时不持有 m.bgMu。
func (m *Manager) stopBackgroundLocked(reason string) {
	m.bgSettings.Store(nil)
	m.bgMu.Lock()
	cancel, done := m.bg.cancel, m.bg.done
	m.bg.cancel, m.bg.done, m.bg.wake = nil, nil, nil
	m.bg.running, m.bg.reason = false, reason
	m.bgMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// backgroundLoop 是唯一的后台循环：启动、每个周期和设置变化时各检查一次。
func (m *Manager) backgroundLoop(ctx context.Context, done chan struct{}, wake <-chan struct{}) {
	defer close(done)
	interval := backgroundTick
	if m.tickOverride > 0 {
		interval = m.tickOverride
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.runBackgroundTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// runBackgroundTick 执行一轮：同步共享文件，选出到期任务，以并发 2 执行并等全部结束。
func (m *Manager) runBackgroundTick(ctx context.Context) {
	settings := m.bgSettings.Load()
	if settings == nil || ctx.Err() != nil {
		return
	}
	m.syncShared()
	now := m.now()
	m.bgMu.Lock()
	m.bg.lastTickAt = now
	m.bgMu.Unlock()
	jobs := m.dueBackgroundJobs(*settings, now)
	semaphore := make(chan struct{}, backgroundWorkers)
	var workers sync.WaitGroup
	for _, job := range jobs {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			return
		}
		workers.Add(1)
		go func(job backgroundJob) {
			defer workers.Done()
			defer func() { <-semaphore }()
			m.runBackgroundJob(ctx, *settings, job)
		}(job)
	}
	workers.Wait()
}

// runBackgroundJob 执行单个任务，单任务预算 45 秒。网关关闭导致的取消不计结果。
func (m *Manager) runBackgroundJob(ctx context.Context, settings BackgroundSettings, job backgroundJob) {
	jobCtx, cancel := context.WithTimeout(ctx, backgroundTaskBudget)
	defer cancel()
	switch job.kind {
	case backgroundTaskToken:
		_, _, exchanged, err := m.accessTokenLead(jobCtx, job.item.ID, m.client, false, backgroundTokenLead)
		if ctx.Err() != nil || errors.Is(err, ErrAccountNotFound) {
			return
		}
		if err != nil {
			m.finishBackgroundTask(job.item, settings, job.kind, false, DescribeCredentialError(err).Message)
			return
		}
		message := "访问令牌仍在提前刷新余量之外，未交换"
		if exchanged {
			message = "已提前刷新访问令牌"
		}
		m.finishBackgroundTask(job.item, settings, job.kind, true, message)
	case backgroundTaskUsage:
		outcome := m.refreshAccountUsage(jobCtx, job.item, false)
		if ctx.Err() != nil || outcome.Category == "cancelled" || outcome.Category == "not_found" {
			return
		}
		m.finishBackgroundTask(job.item, settings, job.kind, outcome.Category == "", outcome.Message)
	}
}

// finishBackgroundTask 记录后台任务结果。失败按类型退避：令牌从 1 分钟起翻倍至 60 分钟，
// 用量从一个周期起翻倍至 24 小时。
func (m *Manager) finishBackgroundTask(item *account, settings BackgroundSettings, kind string, ok bool, message string) {
	now := m.now()
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	task, base, limit := &item.background.token, backgroundTokenBackoff, backgroundTokenMaxDelay
	if kind == backgroundTaskUsage {
		task, base, limit = &item.background.usage, settings.UsageInterval, backgroundUsageMaxDelay
	}
	task.lastRunAt, task.lastMessage = now, message
	if ok {
		task.lastResult, task.failures, task.backoffUntil = backgroundResultOK, 0, time.Time{}
		return
	}
	task.lastResult = backgroundResultError
	task.failures++
	task.backoffUntil = now.Add(backgroundBackoff(base, limit, task.failures))
}

// noteUsageResult 记录任何入口（人工刷新、诊断、后台）的用量结果：成功清零后台失败计数，
// 失败时更长的 Retry-After 推迟后台下一次。调用方可持有 m.mu；bgMu 是叶子锁。
func (m *Manager) noteUsageResult(item *account, success bool, retryAfter time.Duration) {
	now := m.now()
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	state := &item.background
	if success {
		state.usage.failures, state.usage.backoffUntil, state.usageRetryUntil = 0, time.Time{}, time.Time{}
		return
	}
	if retryAfter <= 0 {
		return
	}
	if retryAfter > backgroundUsageMaxDelay {
		retryAfter = backgroundUsageMaxDelay
	}
	if until := now.Add(retryAfter); until.After(state.usageRetryUntil) {
		state.usageRetryUntil = until
	}
}

// noteTokenSuccess 记录任何入口的令牌刷新成功（含导入或重新授权替换凭据），清零后台失败计数。
func (m *Manager) noteTokenSuccess(item *account) {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	item.background.token.failures, item.background.token.backoffUntil = 0, time.Time{}
}

// noteAccountUsed 记录本进程为账户发起真实上游调用的时间，用于判定活跃账户。
func (m *Manager) noteAccountUsed(item *account) {
	now := m.now()
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if now.After(item.background.lastUsedAt) {
		item.background.lastUsedAt = now
	}
}

// usageRetryAfter 解析用量接口的 Retry-After：整数秒或 HTTP 日期，缺失或无法解析返回 0。
func usageRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(backgroundUsageMaxDelay/time.Second) {
			return backgroundUsageMaxDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// usageRetryAfterOf 从用量错误中取出 Retry-After。
func usageRetryAfterOf(err error) time.Duration {
	var statusErr *usageHTTPError
	if errors.As(err, &statusErr) {
		return statusErr.retryAfter
	}
	return 0
}

// backgroundViewLocked 生成账户视图的 background 字段；后台未运行时返回 nil。调用方持有 m.mu。
func (m *Manager) backgroundViewLocked(item *account, now time.Time) *BackgroundView {
	settings := m.bgSettings.Load()
	if settings == nil {
		return nil
	}
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	plan := m.backgroundPlanLocked(item, *settings, now)
	view := &BackgroundView{Active: plan.active, Skipped: plan.skipped}
	if !item.background.lastUsedAt.IsZero() {
		view.LastUsedAt = item.background.lastUsedAt.UTC().Format(time.RFC3339)
	}
	taskView := func(task backgroundTask, next time.Time) *BackgroundTaskView {
		result := &BackgroundTaskView{LastResult: task.lastResult, LastMessage: task.lastMessage, Failures: task.failures}
		if !task.lastRunAt.IsZero() {
			result.LastRunAt = task.lastRunAt.UTC().Format(time.RFC3339)
		}
		if plan.skipped == "" {
			if next.Before(now) {
				next = now
			}
			result.NextAt = next.UTC().Format(time.RFC3339)
		}
		return result
	}
	if settings.TokenRefresh {
		view.Token = taskView(item.background.token, plan.tokenNext)
	}
	if settings.UsageRefresh {
		view.Usage = taskView(item.background.usage, plan.usageNext)
	}
	return view
}

// BackgroundStatus 返回后台循环状态，供 GET /api/chatgpt/background 使用。
func (m *Manager) BackgroundStatus() BackgroundStatus {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	status := BackgroundStatus{
		TokenRefresh: m.bg.requested.TokenRefresh, UsageRefresh: m.bg.requested.UsageRefresh,
		Running: m.bg.running, CrossProcessLocking: m.backgroundSupported(), Reason: m.bg.reason,
	}
	if !m.bg.lastTickAt.IsZero() {
		status.LastTickAt = m.bg.lastTickAt.UTC().Format(time.RFC3339)
	}
	return status
}
