package chatgptauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 导入预检与显式更新：预检只读解析、不落盘、不刷新令牌、不调用上游；
// 应用时在同一把写锁内重建计划，经调用方校验与预检一致后一次提交。

var (
	// ErrInvalidImportAction 表示逐项动作的结构本身不合法（序号越界、重复、未知动作）。
	ErrInvalidImportAction = errors.New("ChatGPT OAuth 导入动作无效")
	// ErrImportPlanNotApplicable 表示重建后的计划存在阻断项或没有可应用的变更。
	ErrImportPlanNotApplicable = errors.New("ChatGPT OAuth 导入计划不可应用")
)

// ImportAction 是用户对单条解析结果的显式选择。Index 是解析后的稳定序号（从 1 开始）。
type ImportAction struct {
	Index           int    `json:"index"`
	Action          string `json:"action"`
	TargetAccountID string `json:"targetAccountId,omitempty"`
}

// ImportPlanEntry 是单条记录的安全元信息与建议，不含任何令牌。
type ImportPlanEntry struct {
	Index           int    `json:"index"`
	Source          string `json:"source,omitempty"`
	Email           string `json:"email,omitempty"`
	Plan            string `json:"plan,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	LastRefresh     string `json:"lastRefresh,omitempty"`
	HasRefreshToken bool   `json:"hasRefreshToken"`
	// Status 取值：new（本地没有该身份）、existing（本地已有同一上游身份）、invalid（条目无效）。
	Status string `json:"status"`
	// DuplicateOf 指向本文件中更早出现的同一上游身份条目序号。
	DuplicateOf       int      `json:"duplicateOf,omitempty"`
	MatchedAccountID  string   `json:"matchedAccountId,omitempty"`
	MatchedEmail      string   `json:"matchedEmail,omitempty"`
	AffectedProviders []string `json:"affectedProviders,omitempty"`
	SuggestedAction   string   `json:"suggestedAction,omitempty"`
	// Action 是本次计划实际采用的动作；存在阻断项时为空。
	Action         string   `json:"action,omitempty"`
	AllowedActions []string `json:"allowedActions"`
	// InvalidReason 是条目本身的无效原因；用户显式选择跳过后不再阻断。
	InvalidReason string   `json:"invalidReason,omitempty"`
	Errors        []string `json:"errors,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// ImportPlanSummary 汇总计划将产生的变化。
type ImportPlanSummary struct {
	Total   int `json:"total"`
	Add     int `json:"add"`
	Update  int `json:"update"`
	Skip    int `json:"skip"`
	Invalid int `json:"invalid"`
	Blocked int `json:"blocked"`
}

// ImportPlan 是导入预检结果。CanApply 为 false 时 Blockers 说明原因；NoChanges 表示没有需要应用的变更。
type ImportPlan struct {
	DetectedFormat string            `json:"detectedFormat"`
	BaseVersion    string            `json:"baseVersion"`
	CanApply       bool              `json:"canApply"`
	NoChanges      bool              `json:"noChanges"`
	Summary        ImportPlanSummary `json:"summary"`
	Entries        []ImportPlanEntry `json:"entries"`
	Blockers       []string          `json:"blockers,omitempty"`

	steps []importStep
}

// importStep 是应用计划所需的内部数据，不进入响应。
type importStep struct {
	action        string
	record        importRecord
	target        *account
	targetVersion uint64
}

// importEntry 是逐条收集的解析结果；err 非空表示该条目无效。
type importEntry struct {
	record importRecord
	err    error
}

// Fingerprint 返回计划的内部绑定摘要：基础版本、逐条动作、目标账户及其凭据世代。
// 调用方应再用服务端密钥做 HMAC 后才放进预检凭证，不直接对外暴露。
func (p *ImportPlan) Fingerprint() string {
	var builder strings.Builder
	builder.WriteString("v1\x00")
	builder.WriteString(p.BaseVersion)
	for i, step := range p.steps {
		target, version := "", uint64(0)
		if step.target != nil {
			target, version = step.target.ID, step.targetVersion
		}
		fmt.Fprintf(&builder, "\x00%d|%s|%s|%d|%s", i+1, step.action, target, version, step.record.AccountID)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// collectImportEntries 与旧导入使用相同的文件结构限制，但逐条收集无效原因，不在第一条错误处停止。
// 文件级错误（JSON 无法解析、ZIP 结构不安全、超限）仍整体返回错误。
func collectImportEntries(data []byte, archive bool, now time.Time) ([]importEntry, error) {
	if len(data) > maxAccountTransferBytes {
		return nil, ErrImportTooLarge
	}
	var entries []importEntry
	collect := func(content []byte) error {
		value, err := decodeImportDocument(content)
		if err != nil {
			return err
		}
		if err := collectImportValue(value, "", now, &entries); err != nil {
			return err
		}
		if len(entries) > maxAccountTransferCount {
			return ErrImportTooLarge
		}
		return nil
	}
	var err error
	if archive {
		err = walkImportArchive(data, collect)
	} else {
		err = collect(data)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: 文件中没有账户", ErrInvalidImport)
	}
	return entries, nil
}

func collectImportValue(value any, sourceHint string, now time.Time, entries *[]importEntry) error {
	switch typed := value.(type) {
	case []any:
		if len(typed) == 0 {
			return fmt.Errorf("%w: 账户数组为空", ErrInvalidImport)
		}
		if len(*entries)+len(typed) > maxAccountTransferCount {
			return ErrImportTooLarge
		}
		for _, item := range typed {
			if err := collectImportValue(item, sourceHint, now, entries); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if kind := stringValue(typed["type"]); kind == "sub2api-data" {
			accounts, ok := typed["accounts"].([]any)
			if !ok || len(accounts) == 0 {
				return fmt.Errorf("%w: sub2api accounts 必须是非空数组", ErrInvalidImport)
			}
			return collectImportValue(accounts, "sub2api", now, entries)
		}
		if accounts, ok := typed["accounts"].([]any); ok {
			return collectImportValue(accounts, "sub2api", now, entries)
		}
		index := len(*entries) + 1
		record, err := parseImportObject(typed, sourceHint, index, now)
		record.Index = index
		*entries = append(*entries, importEntry{record: record, err: err})
		return nil
	default:
		index := len(*entries) + 1
		*entries = append(*entries, importEntry{
			record: importRecord{Index: index},
			err:    importFieldError(index, "不是 JSON 对象"),
		})
		return nil
	}
}

// normalizeImportActions 校验动作结构并按序号建立索引；与条目状态相关的合法性在计划中逐条判断。
func normalizeImportActions(actions []ImportAction, total int) (map[int]ImportAction, error) {
	chosen := make(map[int]ImportAction, len(actions))
	for _, action := range actions {
		if action.Index < 1 || action.Index > total {
			return nil, fmt.Errorf("%w: 序号 %d 超出范围", ErrInvalidImportAction, action.Index)
		}
		if _, exists := chosen[action.Index]; exists {
			return nil, fmt.Errorf("%w: 序号 %d 重复", ErrInvalidImportAction, action.Index)
		}
		switch action.Action {
		case "add", "skip":
			if action.TargetAccountID != "" {
				return nil, fmt.Errorf("%w: 序号 %d 的 %s 动作不能指定目标账户", ErrInvalidImportAction, action.Index, action.Action)
			}
		case "update":
			if strings.TrimSpace(action.TargetAccountID) == "" {
				return nil, fmt.Errorf("%w: 序号 %d 的更新动作必须指定目标账户", ErrInvalidImportAction, action.Index)
			}
		default:
			return nil, fmt.Errorf("%w: 序号 %d 的动作只能是 add、skip 或 update", ErrInvalidImportAction, action.Index)
		}
		chosen[action.Index] = action
	}
	return chosen, nil
}

// SortImportActions 返回按序号排序的动作副本，供调用方计算稳定的输入摘要。
func SortImportActions(actions []ImportAction) []ImportAction {
	sorted := append([]ImportAction(nil), actions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	return sorted
}

func importEntryMessage(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalidImport.Error()+": ")
}

// PlanImport 只读解析导入源并生成计划。references 是本地账户 ID 到绑定 Provider 的映射。
// 不刷新令牌、不访问上游、不写账户文件。
func (m *Manager) PlanImport(data []byte, archive bool, actions []ImportAction, references map[string][]string) (*ImportPlan, error) {
	entries, err := collectImportEntries(data, archive, m.now())
	if err != nil {
		return nil, err
	}
	chosen, err := normalizeImportActions(actions, len(entries))
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildImportPlanLocked(entries, chosen, references), nil
}

// buildImportPlanLocked 按当前账户状态生成计划。调用方持有 m.mu（读锁或写锁）。
func (m *Manager) buildImportPlanLocked(entries []importEntry, chosen map[int]ImportAction, references map[string][]string) *ImportPlan {
	byRemote := make(map[string][]*account)
	for _, item := range m.accounts {
		byRemote[item.AccountID] = append(byRemote[item.AccountID], item)
	}
	for _, items := range byRemote {
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	}
	plan := &ImportPlan{BaseVersion: m.versionLocked(), Entries: make([]ImportPlanEntry, len(entries)), steps: make([]importStep, len(entries))}
	firstOfRemote := make(map[string]int)
	writerOfRemote := make(map[string]int)
	records := make([]importRecord, 0, len(entries))
	for i, entry := range entries {
		index := i + 1
		view := &plan.Entries[i]
		view.Index = index
		choice, hasChoice := chosen[index]
		if entry.err != nil {
			planInvalidEntry(view, entry.err, choice, hasChoice)
			plan.steps[i] = importStep{action: view.Action}
			continue
		}
		record := entry.record
		records = append(records, record)
		matches := byRemote[record.AccountID]
		if first, seen := firstOfRemote[record.AccountID]; seen {
			view.DuplicateOf = first
		} else {
			firstOfRemote[record.AccountID] = index
		}
		describeImportEntry(view, record, matches, references)
		step := importStep{record: record}
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
			if writer, exists := writerOfRemote[record.AccountID]; exists {
				view.Errors = append(view.Errors, fmt.Sprintf("与第 %d 条是同一上游身份，同一身份只能选择一条记录新增或更新", writer))
			} else {
				writerOfRemote[record.AccountID] = index
			}
		}
		if len(view.Errors) == 0 {
			view.Action = action
			step.action = action
			if action == "update" {
				step.target = matches[0]
				step.targetVersion = matches[0].credentialVersion
			}
		}
		plan.steps[i] = step
	}
	plan.DetectedFormat = detectedImportFormat(records)
	plan.summarize()
	return plan
}

// planInvalidEntry 处理无效条目：默认阻断整批，只有显式选择跳过才放行。
func planInvalidEntry(view *ImportPlanEntry, err error, choice ImportAction, hasChoice bool) {
	view.Status = "invalid"
	view.InvalidReason = importEntryMessage(err)
	view.SuggestedAction = "skip"
	view.AllowedActions = []string{"skip"}
	switch {
	case hasChoice && choice.Action == "skip":
		view.Action = "skip"
	case hasChoice:
		view.Errors = append(view.Errors, "无效条目只能跳过")
	default:
		view.Errors = append(view.Errors, "条目无效；修正文件或明确选择跳过后才能应用")
	}
}

// describeImportEntry 填写安全元信息、匹配关系、建议动作和提醒；不写入任何令牌。
func describeImportEntry(view *ImportPlanEntry, record importRecord, matches []*account, references map[string][]string) {
	view.Source = record.Source
	view.Email = record.Email
	view.Plan = record.Plan
	view.HasRefreshToken = record.RefreshToken != ""
	if !record.ExpiresAt.IsZero() {
		view.ExpiresAt = record.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if !record.LastRefresh.IsZero() {
		view.LastRefresh = record.LastRefresh.UTC().Format(time.RFC3339)
	}
	identityConflict := record.ClaimAccountID != "" && record.ClaimAccountID != record.AccountID
	if identityConflict {
		view.Warnings = append(view.Warnings, "文件声明的账户 ID 与令牌中的账户 ID 不一致")
	}
	if view.DuplicateOf > 0 {
		view.Warnings = append(view.Warnings, fmt.Sprintf("与第 %d 条是同一上游身份", view.DuplicateOf))
	}
	if len(matches) == 0 {
		view.Status = "new"
		view.AllowedActions = []string{"add", "skip"}
		view.SuggestedAction = "add"
		if view.DuplicateOf > 0 {
			view.SuggestedAction = "skip"
		}
		if record.RefreshToken == "" {
			view.Warnings = append(view.Warnings, "没有 refresh_token，访问令牌到期后需要重新授权")
		}
		return
	}
	target := matches[0]
	view.Status = "existing"
	view.MatchedAccountID = target.ID
	view.MatchedEmail = target.Email
	view.AffectedProviders = append([]string(nil), references[target.ID]...)
	sort.Strings(view.AffectedProviders)
	view.SuggestedAction = "skip"
	view.AllowedActions = []string{"skip"}
	switch {
	case len(matches) > 1:
		view.Warnings = append(view.Warnings, "本地有多个账户使用该上游身份，不能自动选择更新目标")
	case identityConflict:
		view.Warnings = append(view.Warnings, "身份声明不一致，不能用于更新已有账户")
	case record.RefreshToken == "":
		view.Warnings = append(view.Warnings, "没有 refresh_token，不能用于更新已有账户")
	default:
		view.AllowedActions = append(view.AllowedActions, "update")
	}
	if !record.LastRefresh.IsZero() && !target.LastRefresh.IsZero() && record.LastRefresh.Before(target.LastRefresh) {
		view.Warnings = append(view.Warnings, "文件中的凭据更新时间早于本地凭据，可能是旧文件")
	}
	if target.NeedsReauth {
		view.Warnings = append(view.Warnings, "本地账户当前需要重新授权，更新凭据可恢复")
	}
}

func (p *ImportPlan) summarize() {
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

func importActionLabel(action string) string {
	switch action {
	case "add":
		return "新增"
	case "update":
		return "更新"
	case "skip":
		return "跳过"
	}
	return action
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// ApplyImport 重新解析导入源，在写锁内按当前状态重建计划，交给 verify 确认与预检一致后一次提交。
// verify 返回错误时不做任何改动并原样返回该错误。落盘失败时整批回滚并返回 ErrAccountImportPersist。
func (m *Manager) ApplyImport(data []byte, archive bool, actions []ImportAction, references map[string][]string, verify func(*ImportPlan) error) (ImportResult, error) {
	entries, err := collectImportEntries(data, archive, m.now())
	if err != nil {
		return ImportResult{}, err
	}
	chosen, err := normalizeImportActions(actions, len(entries))
	if err != nil {
		return ImportResult{}, err
	}
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %v", ErrAccountImportPersist, err)
	}
	defer unlock()
	plan := m.buildImportPlanLocked(entries, chosen, references)
	if verify != nil {
		if err := verify(plan); err != nil {
			return ImportResult{}, err
		}
	}
	if !plan.CanApply {
		return ImportResult{}, ErrImportPlanNotApplicable
	}
	return m.commitImportPlanLocked(plan)
}

// updateUndo 保存被更新账户的原凭据字段，落盘失败时恢复。
type updateUndo struct {
	item                                  *account
	email, plan, idToken, access, refresh string
	expiresAt, lastRefresh                time.Time
	needsReauth                           bool
}

func (m *Manager) commitImportPlanLocked(plan *ImportPlan) (ImportResult, error) {
	now := m.now().UTC()
	result := ImportResult{DetectedFormat: plan.DetectedFormat, Accounts: make([]AccountView, 0), Warnings: make([]string, 0)}
	var added []*account
	var updated []*account
	var undo []updateUndo
	for i, step := range plan.steps {
		entry := plan.Entries[i]
		for _, warning := range entry.Warnings {
			result.Warnings = append(result.Warnings, fmt.Sprintf("第 %d 条：%s", entry.Index, warning))
		}
		switch step.action {
		case "add":
			item, err := newImportedAccount(step.record, now)
			if err != nil {
				for _, created := range added {
					delete(m.accounts, created.ID)
				}
				m.restoreImportUndoLocked(undo)
				return ImportResult{}, err
			}
			m.accounts[item.ID] = item
			added = append(added, item)
		case "update":
			target := step.target
			undo = append(undo, updateUndo{
				item: target, email: target.Email, plan: target.Plan, idToken: target.IDToken,
				access: target.AccessToken, refresh: target.RefreshToken,
				expiresAt: target.ExpiresAt, lastRefresh: target.LastRefresh, needsReauth: target.NeedsReauth,
			})
			applyImportedCredentials(target, step.record, now)
			updated = append(updated, target)
		case "skip":
			if entry.Status == "invalid" {
				result.SkippedInvalid++
			} else {
				result.SkippedDuplicates++
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		for _, item := range added {
			delete(m.accounts, item.ID)
		}
		m.restoreImportUndoLocked(undo)
		return ImportResult{}, fmt.Errorf("%w: %v", ErrAccountImportPersist, err)
	}
	for _, item := range updated {
		// 显式导入更新属于外部凭据替换：推进凭据世代并隔离旧凭据的刷新失败和调用结果。
		item.credentialVersion++
		item.lastRefreshFailure = nil
		m.resetInvocationsLocked(item)
	}
	m.bumpVersionLocked()
	m.emitCredentialsUpdatedLocked(updated, "导入更新")
	for _, item := range added {
		result.Accounts = append(result.Accounts, m.viewOf(item))
	}
	for _, item := range updated {
		result.Accounts = append(result.Accounts, m.viewOf(item))
	}
	result.Imported = len(added)
	result.Updated = len(updated)
	return result, nil
}

func (m *Manager) restoreImportUndoLocked(undo []updateUndo) {
	for _, saved := range undo {
		item := saved.item
		item.Email, item.Plan, item.IDToken = saved.email, saved.plan, saved.idToken
		item.AccessToken, item.RefreshToken = saved.access, saved.refresh
		item.ExpiresAt, item.LastRefresh, item.NeedsReauth = saved.expiresAt, saved.lastRefresh, saved.needsReauth
	}
}

// newImportedAccount 用导入记录创建新账户，旧导入与导入应用共用。
func newImportedAccount(record importRecord, now time.Time) (*account, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法生成本地账户 ID", ErrAccountImportPersist)
	}
	lastRefresh := record.LastRefresh
	if lastRefresh.IsZero() {
		lastRefresh = now
	}
	authorizedAt := now
	return &account{
		ID: "acc_" + id, Email: record.Email, Plan: record.Plan, AccountID: record.AccountID,
		IDToken: record.IDToken, AccessToken: record.AccessToken, RefreshToken: record.RefreshToken,
		ExpiresAt: record.ExpiresAt, LastRefresh: lastRefresh, AuthorizedAt: &authorizedAt,
	}, nil
}

// applyImportedCredentials 用导入记录整组替换凭据，不与旧令牌拼接。
// 本地 ID、首次授权时间、标签备注、用量快照和绑定关系保持不变；邮箱与套餐只在文件提供时更新。
func applyImportedCredentials(item *account, record importRecord, now time.Time) {
	item.IDToken = record.IDToken
	item.AccessToken = record.AccessToken
	item.RefreshToken = record.RefreshToken
	item.ExpiresAt = record.ExpiresAt
	item.LastRefresh = record.LastRefresh
	if item.LastRefresh.IsZero() {
		item.LastRefresh = now
	}
	if record.Email != "" {
		item.Email = record.Email
	}
	if record.Plan != "" {
		item.Plan = record.Plan
	}
	item.NeedsReauth = false
}
