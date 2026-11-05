package chatgptauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ai-gateway/internal/backupcrypto"
)

// 原生加密备份与恢复（DESIGN 8.4、API_SPEC 8.5）。加解密在 internal/backupcrypto；
// 这里负责明文载荷、恢复计划和原子应用。预检只读、不落盘、不刷新令牌、不调用上游。

var (
	// ErrInvalidBackup 表示解密后的明文无效（结构、版本、账户数或字段）。
	ErrInvalidBackup = errors.New("备份内容无效")
	// ErrInvalidRestoreAction 表示恢复动作或选项的结构本身不合法。
	ErrInvalidRestoreAction = errors.New("备份恢复动作无效")
	// ErrRestorePlanNotApplicable 表示重建后的计划存在阻断项或没有可应用的变更。
	ErrRestorePlanNotApplicable = errors.New("备份恢复计划不可应用")
	// ErrAccountRestorePersist 表示恢复落盘失败，整批已回滚。
	ErrAccountRestorePersist = errors.New("保存恢复的 ChatGPT OAuth 账户失败")
	// ErrBackupSelection 表示导出选择无效（为空、超过 100 个、重复或账户不存在）。
	ErrBackupSelection = errors.New("备份账户选择无效")
)

// 恢复元数据策略。
const (
	RestoreMetadataKeep    = "keep"
	RestoreMetadataReplace = "replace"
)

// MaxBackupAccounts 是单个备份最多包含的账户数。
const MaxBackupAccounts = 100

// RestoreOptions 是恢复的全局选择。Metadata 为空时按 keep 处理。
type RestoreOptions struct {
	Metadata      string `json:"metadata"`
	RestorePaused bool   `json:"restorePaused"`
}

// RestoreAction 是对单条备份记录的显式选择，结构与导入动作一致。
type RestoreAction = ImportAction

// RestorePlanEntry 是单条备份记录的安全元信息与计划，不含任何令牌、备注原文或解密原文。
type RestorePlanEntry struct {
	Index             int      `json:"index"`
	SourceAccountID   string   `json:"sourceAccountId"`
	Email             string   `json:"email,omitempty"`
	Plan              string   `json:"plan,omitempty"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
	LastRefresh       string   `json:"lastRefresh,omitempty"`
	HasRefreshToken   bool     `json:"hasRefreshToken"`
	Tags              []string `json:"tags"`
	HasNote           bool     `json:"hasNote"`
	Paused            bool     `json:"paused"`
	Status            string   `json:"status"`
	DuplicateOf       int      `json:"duplicateOf,omitempty"`
	MatchedAccountID  string   `json:"matchedAccountId,omitempty"`
	MatchedEmail      string   `json:"matchedEmail,omitempty"`
	AffectedProviders []string `json:"affectedProviders"`
	SuggestedAction   string   `json:"suggestedAction,omitempty"`
	Action            string   `json:"action,omitempty"`
	AllowedActions    []string `json:"allowedActions"`
	TargetAccountID   string   `json:"targetAccountId,omitempty"`
	Remapped          bool     `json:"remapped"`
	MetadataChanges   bool     `json:"metadataChanges"`
	PausedChange      bool     `json:"pausedChange"`
	InvalidReason     string   `json:"invalidReason,omitempty"`
	Errors            []string `json:"errors"`
	Warnings          []string `json:"warnings"`
}

// RestorePlan 是恢复预检结果。
type RestorePlan struct {
	BaseVersion   string             `json:"baseVersion"`
	CreatedAt     string             `json:"createdAt,omitempty"`
	Metadata      string             `json:"metadata"`
	RestorePaused bool               `json:"restorePaused"`
	CanApply      bool               `json:"canApply"`
	NoChanges     bool               `json:"noChanges"`
	Summary       ImportPlanSummary  `json:"summary"`
	Entries       []RestorePlanEntry `json:"entries"`
	Blockers      []string           `json:"blockers"`

	steps []restoreStep
}

// RestoreMapping 是源本地 ID 到目标 ID 的映射，供提供商／路由迁移的账户映射预填。
type RestoreMapping struct {
	SourceAccountID string `json:"sourceAccountId"`
	TargetAccountID string `json:"targetAccountId"`
	Action          string `json:"action"`
	Remapped        bool   `json:"remapped"`
}

// RestoreResult 是恢复应用结果。
type RestoreResult struct {
	Imported int              `json:"imported"`
	Updated  int              `json:"updated"`
	Skipped  int              `json:"skipped"`
	Accounts []AccountView    `json:"accounts"`
	Mappings []RestoreMapping `json:"mappings"`
}

// restoreStep 是应用计划所需的内部数据，不进入响应。
type restoreStep struct {
	action        string
	record        backupRecord
	target        *account
	targetVersion uint64
	// match 是唯一匹配到的本地账户；跳过时用于输出映射。
	match *account
	// newID 是新增时保留的源本地 ID；为空表示应用时生成新 ID。
	newID string
}

// Fingerprint 返回计划的内部绑定摘要：基础版本、全局选择、逐条动作、目标及其凭据世代、保留的 ID。
// 调用方再用服务端密钥做 HMAC 后放进预检凭证，不直接对外暴露。
func (p *RestorePlan) Fingerprint() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "restore-v1\x00%s\x00%s\x00%t", p.BaseVersion, p.Metadata, p.RestorePaused)
	for i, step := range p.steps {
		target, version := "", uint64(0)
		if step.target != nil {
			target, version = step.target.ID, step.targetVersion
		}
		fmt.Fprintf(&builder, "\x00%d|%s|%s|%d|%s|%s", i+1, step.action, target, version, step.record.source.ChatGPTAccountID, step.newID)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// backupPayload 是解密后的明文载荷（API_SPEC 8.5）。
type backupPayload struct {
	Version   int             `json:"version"`
	CreatedAt string          `json:"createdAt"`
	Accounts  []backupAccount `json:"accounts"`
}

type backupAccount struct {
	ID                      string   `json:"id"`
	ChatGPTAccountID        string   `json:"chatgptAccountId"`
	Email                   string   `json:"email"`
	Plan                    string   `json:"plan"`
	IDToken                 string   `json:"idToken"`
	AccessToken             string   `json:"accessToken"`
	RefreshToken            string   `json:"refreshToken"`
	ExpiresAt               string   `json:"expiresAt"`
	LastRefresh             string   `json:"lastRefresh"`
	AuthorizedAt            string   `json:"authorizedAt"`
	SubscriptionActiveStart string   `json:"subscriptionActiveStart"`
	SubscriptionActiveUntil string   `json:"subscriptionActiveUntil"`
	Tags                    []string `json:"tags"`
	Note                    string   `json:"note"`
	NeedsReauth             bool     `json:"needsReauth"`
	Paused                  bool     `json:"paused"`
	PausedAt                string   `json:"pausedAt"`
}

// backupRecord 是校验后的单条备份记录；err 非空表示该条目无效，只能跳过。
type backupRecord struct {
	source                               backupAccount
	tags                                 []string
	expiresAt, lastRefresh               time.Time
	authorizedAt, pausedAt               time.Time
	subscriptionStart, subscriptionUntil time.Time
	// validID 表示源本地 ID 格式合法，可以在空闲时保留。
	validID bool
	err     error
}

func formatBackupTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatBackupTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatBackupTime(*t)
}

func parseBackupTime(value, field string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s 不是合法的 RFC3339 时间", field)
	}
	return parsed.UTC(), nil
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t
	return &value
}

// validBackupAccountID 判断源本地 ID 是否可以原样保留：acc_ 加 1–64 位小写十六进制。
func validBackupAccountID(id string) bool {
	rest, ok := strings.CutPrefix(id, "acc_")
	if !ok || len(rest) == 0 || len(rest) > 64 {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// validateBackupAccount 校验单条记录。条目级错误进入 record.err，不让整份备份失败。
func validateBackupAccount(source backupAccount) backupRecord {
	source.ChatGPTAccountID = strings.TrimSpace(source.ChatGPTAccountID)
	record := backupRecord{source: source, validID: validBackupAccountID(source.ID)}
	fail := func(message string) backupRecord {
		record.err = errors.New(message)
		return record
	}
	if source.ChatGPTAccountID == "" {
		return fail("缺少上游账户 ID")
	}
	if source.AccessToken == "" && source.RefreshToken == "" {
		return fail("没有访问令牌或刷新凭据")
	}
	var err error
	for _, field := range []struct {
		value  string
		name   string
		target *time.Time
	}{
		{source.ExpiresAt, "expiresAt", &record.expiresAt},
		{source.LastRefresh, "lastRefresh", &record.lastRefresh},
		{source.AuthorizedAt, "authorizedAt", &record.authorizedAt},
		{source.SubscriptionActiveStart, "subscriptionActiveStart", &record.subscriptionStart},
		{source.SubscriptionActiveUntil, "subscriptionActiveUntil", &record.subscriptionUntil},
		{source.PausedAt, "pausedAt", &record.pausedAt},
	} {
		if *field.target, err = parseBackupTime(field.value, field.name); err != nil {
			return fail(err.Error())
		}
	}
	tags, err := normalizeAccountTags(source.Tags)
	if err != nil {
		return fail("标签不符合要求")
	}
	record.tags = tags
	if utf8.RuneCountInString(source.Note) > maxAccountNoteLength {
		return fail(fmt.Sprintf("备注超过 %d 个字符", maxAccountNoteLength))
	}
	return record
}

// decodeBackupPayload 严格解码明文。结构、版本和账户数是文件级错误；单条记录问题留给计划逐条展示。
// 明文已经过认证加密，这里不再做重复键检查。
func decodeBackupPayload(plaintext []byte) ([]backupRecord, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	var payload backupPayload
	if err := decoder.Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("%w: 明文结构不合法", ErrInvalidBackup)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("%w: 明文包含多余内容", ErrInvalidBackup)
	}
	if payload.Version != 1 {
		return nil, "", fmt.Errorf("%w: 不支持的明文版本 %d", ErrInvalidBackup, payload.Version)
	}
	if len(payload.Accounts) == 0 || len(payload.Accounts) > MaxBackupAccounts {
		return nil, "", fmt.Errorf("%w: 账户数应在 1-%d 之间", ErrInvalidBackup, MaxBackupAccounts)
	}
	createdAt := ""
	if at, err := parseBackupTime(payload.CreatedAt, "createdAt"); err == nil && !at.IsZero() {
		createdAt = at.Format(time.RFC3339)
	}
	records := make([]backupRecord, len(payload.Accounts))
	for i, source := range payload.Accounts {
		records[i] = validateBackupAccount(source)
	}
	return records, createdAt, nil
}

// sealBackup 加密明文；测试可降低迭代次数，生产固定使用 backupcrypto.DefaultIterations。
func (m *Manager) sealBackup(plaintext []byte, password string) ([]byte, error) {
	if m.backupIterations > 0 {
		return backupcrypto.SealWith(plaintext, password, backupcrypto.SealOptions{Iterations: m.backupIterations})
	}
	return backupcrypto.Seal(plaintext, password)
}

// ExportBackup 在账户版本等于 expectedVersion 时导出所选账户的加密容器。
// 版本不一致返回 ErrAccountVersionConflict；选择无效返回 ErrBackupSelection；
// 密码不合规返回 backupcrypto.ErrInvalidPassword。不刷新令牌、不写盘。
func (m *Manager) ExportBackup(ids []string, password, expectedVersion string) ([]byte, error) {
	if err := backupcrypto.ValidatePassword(password, true); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > MaxBackupAccounts {
		return nil, fmt.Errorf("%w: 账户数应在 1-%d 之间", ErrBackupSelection, MaxBackupAccounts)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, fmt.Errorf("%w: 账户 ID 为空或重复", ErrBackupSelection)
		}
		seen[id] = true
	}
	// 先合并另一实例的写入，再在同一把读锁内核对版本并取快照，所有账户来自一致的状态。
	m.syncShared()
	payload, err := m.backupSnapshot(ids, expectedVersion)
	if err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码备份内容失败: %w", err)
	}
	defer clear(plaintext)
	return m.sealBackup(plaintext, password)
}

func (m *Manager) backupSnapshot(ids []string, expectedVersion string) (backupPayload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if expectedVersion != m.versionLocked() {
		return backupPayload{}, ErrAccountVersionConflict
	}
	payload := backupPayload{Version: 1, CreatedAt: m.now().UTC().Format(time.RFC3339), Accounts: make([]backupAccount, 0, len(ids))}
	for _, id := range ids {
		item := m.accounts[id]
		if item == nil {
			return backupPayload{}, fmt.Errorf("%w: 账户不存在", ErrBackupSelection)
		}
		pausedAt := ""
		if item.Paused {
			pausedAt = formatBackupTimePtr(item.PausedAt)
		}
		payload.Accounts = append(payload.Accounts, backupAccount{
			ID: item.ID, ChatGPTAccountID: item.AccountID, Email: item.Email, Plan: item.Plan,
			IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
			ExpiresAt: formatBackupTime(item.ExpiresAt), LastRefresh: formatBackupTime(item.LastRefresh),
			AuthorizedAt:            formatBackupTimePtr(item.AuthorizedAt),
			SubscriptionActiveStart: formatBackupTimePtr(item.SubscriptionActiveStart),
			SubscriptionActiveUntil: formatBackupTimePtr(item.SubscriptionActiveUntil),
			Tags:                    append([]string{}, item.Tags...), Note: item.Note,
			NeedsReauth: item.NeedsReauth, Paused: item.Paused, PausedAt: pausedAt,
		})
	}
	return payload, nil
}

func normalizeRestoreOptions(options RestoreOptions) (RestoreOptions, error) {
	switch options.Metadata {
	case "":
		options.Metadata = RestoreMetadataKeep
	case RestoreMetadataKeep, RestoreMetadataReplace:
	default:
		return options, fmt.Errorf("%w: metadata 只能是 keep 或 replace", ErrInvalidRestoreAction)
	}
	return options, nil
}

// openBackup 校验选项、解密容器并校验动作。选项先于解密校验，避免为明显无效的请求做密钥派生。
func (m *Manager) openBackup(container []byte, password string, options RestoreOptions, actions []RestoreAction) ([]backupRecord, string, RestoreOptions, map[int]ImportAction, error) {
	options, err := normalizeRestoreOptions(options)
	if err != nil {
		return nil, "", options, nil, err
	}
	plaintext, err := backupcrypto.Open(container, password)
	if err != nil {
		return nil, "", options, nil, err
	}
	records, createdAt, err := decodeBackupPayload(plaintext)
	clear(plaintext)
	if err != nil {
		return nil, "", options, nil, err
	}
	chosen, err := normalizeImportActions(actions, len(records))
	if err != nil {
		return nil, "", options, nil, fmt.Errorf("%w: %s", ErrInvalidRestoreAction, strings.TrimPrefix(err.Error(), ErrInvalidImportAction.Error()+": "))
	}
	return records, createdAt, options, chosen, nil
}

// PlanRestore 解密容器并生成恢复计划。references 是本地账户 ID 到绑定 Provider 的映射。
// 解密错误原样返回 backupcrypto 的错误（ErrInvalidContainer / ErrUnsupported / ErrDecrypt / ErrTooLarge /
// ErrInvalidPassword）；明文无效返回 ErrInvalidBackup；动作或选项无效返回 ErrInvalidRestoreAction。
func (m *Manager) PlanRestore(container []byte, password string, options RestoreOptions, actions []RestoreAction, references map[string][]string) (*RestorePlan, error) {
	records, createdAt, options, chosen, err := m.openBackup(container, password, options, actions)
	if err != nil {
		return nil, err
	}
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildRestorePlanLocked(records, createdAt, options, chosen, references), nil
}

// buildRestorePlanLocked 按当前账户状态生成计划。调用方持有 m.mu（读锁或写锁）。
func (m *Manager) buildRestorePlanLocked(records []backupRecord, createdAt string, options RestoreOptions, chosen map[int]ImportAction, references map[string][]string) *RestorePlan {
	byRemote := make(map[string][]*account)
	for _, item := range m.accounts {
		byRemote[item.AccountID] = append(byRemote[item.AccountID], item)
	}
	for _, items := range byRemote {
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	}
	plan := &RestorePlan{
		BaseVersion: m.versionLocked(), CreatedAt: createdAt, Metadata: options.Metadata, RestorePaused: options.RestorePaused,
		Entries: make([]RestorePlanEntry, len(records)), Blockers: []string{}, steps: make([]restoreStep, len(records)),
	}
	firstOfRemote := make(map[string]int)
	writerOfRemote := make(map[string]int)
	claimedIDs := make(map[string]bool)
	for i, record := range records {
		index := i + 1
		view := &plan.Entries[i]
		*view = RestorePlanEntry{Index: index, SourceAccountID: record.source.ID, Tags: []string{}, AffectedProviders: []string{},
			AllowedActions: []string{}, Errors: []string{}, Warnings: []string{}}
		choice, hasChoice := chosen[index]
		step := restoreStep{record: record}
		if record.err != nil {
			planInvalidRestoreEntry(view, record.err, choice, hasChoice)
			step.action = view.Action
			plan.steps[i] = step
			continue
		}
		remote := record.source.ChatGPTAccountID
		matches := byRemote[remote]
		if first, seen := firstOfRemote[remote]; seen {
			view.DuplicateOf = first
		} else {
			firstOfRemote[remote] = index
		}
		describeRestoreEntry(view, record, matches, references)
		if len(matches) == 1 {
			step.match = matches[0]
		}
		action := view.SuggestedAction
		if hasChoice {
			action = choice.Action
		}
		switch {
		case !containsString(view.AllowedActions, action):
			view.Errors = append(view.Errors, fmt.Sprintf("该条目不能选择 %s", importActionLabel(action)))
		case action == "update" && (len(matches) != 1 || choice.TargetAccountID != matches[0].ID):
			view.Errors = append(view.Errors, "更新目标必须是上游身份一致的本地账户")
		case action != "skip":
			if writer, exists := writerOfRemote[remote]; exists {
				view.Errors = append(view.Errors, fmt.Sprintf("与第 %d 条是同一上游身份，同一身份只能选择一条记录新增或更新", writer))
			} else {
				writerOfRemote[remote] = index
			}
		}
		if len(view.Errors) == 0 {
			view.Action = action
			step.action = action
			m.planRestoreChange(view, &step, record, options, claimedIDs)
		}
		plan.steps[i] = step
	}
	plan.summarize()
	return plan
}

// planRestoreChange 为新增或更新填写目标 ID、重映射与元数据变化。调用方持有 m.mu。
func (m *Manager) planRestoreChange(view *RestorePlanEntry, step *restoreStep, record backupRecord, options RestoreOptions, claimedIDs map[string]bool) {
	switch step.action {
	case "add":
		id := record.source.ID
		switch {
		case !record.validID:
			view.Remapped = true
			if id != "" {
				view.Warnings = append(view.Warnings, "源本地 ID 格式不合法，恢复时生成新 ID")
			}
		case m.accounts[id] != nil || claimedIDs[id]:
			view.Remapped = true
			view.Warnings = append(view.Warnings, "源本地 ID 已被其他账户使用，恢复时生成新 ID；绑定需按映射调整")
		default:
			claimedIDs[id] = true
			step.newID = id
			view.TargetAccountID = id
		}
		view.MetadataChanges = len(record.tags) > 0 || record.source.Note != ""
		view.PausedChange = options.RestorePaused && record.source.Paused
	case "update":
		target := step.match
		step.target = target
		step.targetVersion = target.credentialVersion
		view.TargetAccountID = target.ID
		if options.Metadata == RestoreMetadataReplace {
			view.MetadataChanges = !equalTags(target.Tags, record.tags) || target.Note != record.source.Note
		}
		view.PausedChange = options.RestorePaused && target.Paused != record.source.Paused
	}
}

func equalTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// planInvalidRestoreEntry 处理无效条目：默认阻断整批，只有显式选择跳过才放行。
func planInvalidRestoreEntry(view *RestorePlanEntry, err error, choice ImportAction, hasChoice bool) {
	view.Status = "invalid"
	view.InvalidReason = err.Error()
	view.SuggestedAction = "skip"
	view.AllowedActions = []string{"skip"}
	switch {
	case hasChoice && choice.Action == "skip":
		view.Action = "skip"
	case hasChoice:
		view.Errors = append(view.Errors, "无效条目只能跳过")
	default:
		view.Errors = append(view.Errors, "条目无效；明确选择跳过后才能应用")
	}
}

// describeRestoreEntry 填写安全元信息、匹配关系、建议动作和提醒；不写入任何令牌或备注原文。
func describeRestoreEntry(view *RestorePlanEntry, record backupRecord, matches []*account, references map[string][]string) {
	source := record.source
	view.Email, view.Plan = source.Email, source.Plan
	view.HasRefreshToken = source.RefreshToken != ""
	view.ExpiresAt = formatViewTime(record.expiresAt)
	view.LastRefresh = formatViewTime(record.lastRefresh)
	view.Tags = append([]string{}, record.tags...)
	view.HasNote = source.Note != ""
	view.Paused = source.Paused
	if view.DuplicateOf > 0 {
		view.Warnings = append(view.Warnings, fmt.Sprintf("与第 %d 条是同一上游身份", view.DuplicateOf))
	}
	if source.NeedsReauth {
		view.Warnings = append(view.Warnings, "备份时该账户已需要重新授权，恢复后仍需重新授权")
	}
	if len(matches) == 0 {
		view.Status = "new"
		view.AllowedActions = []string{"add", "skip"}
		view.SuggestedAction = "add"
		if view.DuplicateOf > 0 {
			view.SuggestedAction = "skip"
		}
		if source.RefreshToken == "" {
			view.Warnings = append(view.Warnings, "没有 refresh_token，访问令牌到期后需要重新授权")
		}
		return
	}
	target := matches[0]
	view.Status = "existing"
	view.MatchedAccountID = target.ID
	view.MatchedEmail = target.Email
	view.AffectedProviders = append([]string{}, references[target.ID]...)
	sort.Strings(view.AffectedProviders)
	view.SuggestedAction = "skip"
	view.AllowedActions = []string{"skip"}
	switch {
	case len(matches) > 1:
		view.Warnings = append(view.Warnings, "本地有多个账户使用该上游身份，不能自动选择更新目标")
	case source.RefreshToken == "":
		view.Warnings = append(view.Warnings, "没有 refresh_token，不能用于更新已有账户")
	default:
		view.AllowedActions = append(view.AllowedActions, "update")
	}
	if !record.lastRefresh.IsZero() && !target.LastRefresh.IsZero() && record.lastRefresh.Before(target.LastRefresh) {
		view.Warnings = append(view.Warnings, "备份中的凭据早于本地凭据，可能是旧备份；旧的刷新凭据可能已被轮换失效")
	}
	if target.NeedsReauth && !source.NeedsReauth {
		view.Warnings = append(view.Warnings, "本地账户当前需要重新授权，更新凭据可恢复")
	}
}

func formatViewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (p *RestorePlan) summarize() {
	p.Summary = ImportPlanSummary{Total: len(p.Entries)}
	for _, entry := range p.Entries {
		if entry.Status == "invalid" {
			p.Summary.Invalid++
		}
		if len(entry.Errors) > 0 {
			p.Summary.Blocked++
			for _, message := range entry.Errors {
				p.Blockers = append(p.Blockers, fmt.Sprintf("第 %d 条：%s", entry.Index, message))
			}
			continue
		}
		switch entry.Action {
		case "add":
			p.Summary.Add++
		case "update":
			p.Summary.Update++
		case "skip":
			p.Summary.Skip++
		}
	}
	changes := p.Summary.Add + p.Summary.Update
	p.CanApply = p.Summary.Blocked == 0 && changes > 0
	p.NoChanges = p.Summary.Blocked == 0 && changes == 0
}

// ApplyRestore 重新解密，在写锁内按当前状态重建计划，交给 verify 确认与预检一致后一次提交。
// verify 返回的错误原样返回且不做改动；计划不可应用返回 ErrRestorePlanNotApplicable；
// 落盘失败整批回滚并返回 ErrAccountRestorePersist。
func (m *Manager) ApplyRestore(container []byte, password string, options RestoreOptions, actions []RestoreAction, references map[string][]string, verify func(*RestorePlan) error) (RestoreResult, error) {
	records, createdAt, options, chosen, err := m.openBackup(container, password, options, actions)
	if err != nil {
		return RestoreResult{}, err
	}
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: %w", ErrAccountRestorePersist, err)
	}
	defer unlock()
	plan := m.buildRestorePlanLocked(records, createdAt, options, chosen, references)
	if verify != nil {
		if err := verify(plan); err != nil {
			return RestoreResult{}, err
		}
	}
	if !plan.CanApply {
		return RestoreResult{}, ErrRestorePlanNotApplicable
	}
	return m.commitRestorePlanLocked(plan)
}

// restoreUndo 保存被更新账户的全部可持久化字段，落盘失败时整体复原。
type restoreUndo struct {
	item  *account
	saved *account
}

func (m *Manager) commitRestorePlanLocked(plan *RestorePlan) (RestoreResult, error) {
	now := m.now().UTC()
	replaceMetadata := plan.Metadata == RestoreMetadataReplace
	result := RestoreResult{Accounts: []AccountView{}, Mappings: []RestoreMapping{}}
	var added, updated []*account
	var undo []restoreUndo
	wasPaused := make(map[*account]bool)
	rollback := func() {
		for _, item := range added {
			delete(m.accounts, item.ID)
		}
		for _, entry := range undo {
			copyPersistentFields(entry.item, entry.saved)
		}
	}
	for i, step := range plan.steps {
		source := step.record.source
		switch step.action {
		case "add":
			id, remapped := step.newID, false
			if id == "" || m.accounts[id] != nil {
				random, err := randomHex(16)
				if err != nil {
					rollback()
					return RestoreResult{}, fmt.Errorf("%w: 无法生成本地账户 ID", ErrAccountRestorePersist)
				}
				id, remapped = "acc_"+random, true
			}
			item := newRestoredAccount(id, step.record, plan.RestorePaused, now)
			m.accounts[id] = item
			added = append(added, item)
			if source.ID != "" {
				result.Mappings = append(result.Mappings, RestoreMapping{SourceAccountID: source.ID, TargetAccountID: id, Action: "add", Remapped: remapped})
			}
		case "update":
			target := step.target
			saved := &account{}
			copyPersistentFields(saved, target)
			undo = append(undo, restoreUndo{item: target, saved: saved})
			wasPaused[target] = target.Paused
			applyRestoredAccount(target, step.record, replaceMetadata, plan.RestorePaused, now)
			updated = append(updated, target)
			if source.ID != "" {
				result.Mappings = append(result.Mappings, RestoreMapping{SourceAccountID: source.ID, TargetAccountID: target.ID, Action: "update", Remapped: source.ID != target.ID})
			}
		case "skip":
			result.Skipped++
			if step.match != nil && source.ID != "" && plan.Entries[i].Status == "existing" {
				result.Mappings = append(result.Mappings, RestoreMapping{SourceAccountID: source.ID, TargetAccountID: step.match.ID, Action: "skip", Remapped: source.ID != step.match.ID})
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		rollback()
		return RestoreResult{}, fmt.Errorf("%w: %w", ErrAccountRestorePersist, err)
	}
	var paused, resumed []*account
	for _, item := range updated {
		// 恢复更新属于外部凭据替换：推进凭据世代并隔离旧凭据的刷新失败和调用结果。
		item.credentialVersion++
		item.lastRefreshFailure = nil
		item.credentialsUnsaved = false
		m.resetInvocationsLocked(item)
		m.noteTokenSuccess(item)
		if item.NeedsReauth {
			m.raiseAlertLocked(item, alertReauth)
		}
		switch {
		case item.Paused && !wasPaused[item]:
			paused = append(paused, item)
		case !item.Paused && wasPaused[item]:
			resumed = append(resumed, item)
		}
	}
	for _, item := range added {
		if item.NeedsReauth {
			// 与加载文件一致：恢复告警状态但不重复产生事件。
			item.alertKind = alertReauth
		}
	}
	m.bumpVersionLocked()
	m.emitCredentialsUpdatedLocked(updated, "备份恢复")
	for _, item := range paused {
		wakeAdmissionWatchersLocked(item, ErrAccountPaused)
	}
	m.emitPausedLocked(paused, true)
	m.emitPausedLocked(resumed, false)
	for _, item := range append(added, updated...) {
		result.Accounts = append(result.Accounts, m.viewOf(item))
	}
	result.Imported, result.Updated = len(added), len(updated)
	return result, nil
}

// newRestoredAccount 用备份记录创建新账户：保留首次授权时间、标签备注；暂停状态只在 restorePaused 时取备份值。
func newRestoredAccount(id string, record backupRecord, restorePaused bool, now time.Time) *account {
	source := record.source
	item := &account{
		ID: id, Email: source.Email, Plan: source.Plan, AccountID: source.ChatGPTAccountID,
		IDToken: source.IDToken, AccessToken: source.AccessToken, RefreshToken: source.RefreshToken,
		ExpiresAt: record.expiresAt, LastRefresh: record.lastRefresh, NeedsReauth: source.NeedsReauth,
		AuthorizedAt: timePtr(record.authorizedAt), Note: source.Note,
		SubscriptionActiveStart: timePtr(record.subscriptionStart), SubscriptionActiveUntil: timePtr(record.subscriptionUntil),
	}
	if len(record.tags) > 0 {
		item.Tags = append([]string(nil), record.tags...)
	}
	if item.LastRefresh.IsZero() {
		item.LastRefresh = now
	}
	if item.AuthorizedAt == nil {
		item.AuthorizedAt = timePtr(now)
	}
	if restorePaused && source.Paused {
		item.Paused = true
		item.PausedAt = timePtr(record.pausedAt)
		if item.PausedAt == nil {
			item.PausedAt = timePtr(now)
		}
	}
	return item
}

// applyRestoredAccount 用备份整组替换已有账户的凭据。本地 ID、首次授权时间、绑定与用量快照不变；
// 标签备注只在 replace 策略下覆盖，暂停状态只在 restorePaused 时取备份值。
func applyRestoredAccount(item *account, record backupRecord, replaceMetadata, restorePaused bool, now time.Time) {
	source := record.source
	item.IDToken, item.AccessToken, item.RefreshToken = source.IDToken, source.AccessToken, source.RefreshToken
	item.ExpiresAt, item.LastRefresh, item.NeedsReauth = record.expiresAt, record.lastRefresh, source.NeedsReauth
	if item.LastRefresh.IsZero() {
		item.LastRefresh = now
	}
	if source.Email != "" {
		item.Email = source.Email
	}
	if source.Plan != "" {
		item.Plan = source.Plan
	}
	if !record.subscriptionStart.IsZero() {
		item.SubscriptionActiveStart = timePtr(record.subscriptionStart)
	}
	if !record.subscriptionUntil.IsZero() {
		item.SubscriptionActiveUntil = timePtr(record.subscriptionUntil)
	}
	if replaceMetadata {
		item.Tags = nil
		if len(record.tags) > 0 {
			item.Tags = append([]string(nil), record.tags...)
		}
		item.Note = source.Note
	}
	if restorePaused && item.Paused != source.Paused {
		item.Paused = source.Paused
		item.PausedAt = nil
		if source.Paused {
			item.PausedAt = timePtr(record.pausedAt)
			if item.PausedAt == nil {
				item.PausedAt = timePtr(now)
			}
		}
	}
}
