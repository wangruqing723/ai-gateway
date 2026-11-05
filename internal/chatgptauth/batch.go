package chatgptauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 批量操作上限：与导入导出的账户数量上限一致。
const maxBatchAccounts = maxAccountTransferCount

var (
	// ErrInvalidBatch 表示批量请求的结构本身不合法（操作、ID、标签）。
	ErrInvalidBatch = errors.New("ChatGPT OAuth 批量操作无效")
	// ErrBatchNotApplicable 表示重建后的批量计划存在阻断项或没有变更。
	ErrBatchNotApplicable = errors.New("ChatGPT OAuth 批量计划不可应用")
	// ErrAccountBatchPersist 表示批量写盘失败，整批已回滚。
	ErrAccountBatchPersist = errors.New("保存 ChatGPT OAuth 批量变更失败")
)

// BatchMissingError 列出批量请求中不存在的账户 ID。
type BatchMissingError struct {
	IDs []string
}

func (e *BatchMissingError) Error() string {
	return "ChatGPT OAuth 账户不存在: " + strings.Join(e.IDs, ", ")
}

// BatchRequest 是规范化前的批量请求。AddTags、RemoveTags 只用于 tags 操作。
type BatchRequest struct {
	Operation  string
	AccountIDs []string
	AddTags    []string
	RemoveTags []string
}

// BatchAccountState 是批量计划中一个账户的前后状态，只含本地元数据。
type BatchAccountState struct {
	Exists  bool     `json:"exists"`
	Enabled bool     `json:"enabled"`
	Tags    []string `json:"tags"`
}

// BatchEntry 是单个账户的计划项。Action 取 change、unchanged 或 blocked。
type BatchEntry struct {
	AccountID string            `json:"accountId"`
	Email     string            `json:"email,omitempty"`
	Action    string            `json:"action"`
	Before    BatchAccountState `json:"before"`
	After     BatchAccountState `json:"after"`
	Providers []string          `json:"providers"`
	Errors    []string          `json:"errors,omitempty"`
}

// BatchSummary 汇总计划。
type BatchSummary struct {
	Total     int `json:"total"`
	Change    int `json:"change"`
	Unchanged int `json:"unchanged"`
	Blocked   int `json:"blocked"`
}

// BatchPlan 是批量预检结果。CanApply 为 false 时 Blockers 说明原因。
type BatchPlan struct {
	Operation   string       `json:"operation"`
	BaseVersion string       `json:"baseVersion"`
	CanApply    bool         `json:"canApply"`
	NoChanges   bool         `json:"noChanges"`
	Summary     BatchSummary `json:"summary"`
	Entries     []BatchEntry `json:"entries"`
	Blockers    []string     `json:"blockers,omitempty"`
	AddTags     []string     `json:"addTags,omitempty"`
	RemoveTags  []string     `json:"removeTags,omitempty"`

	items []*account
}

// NormalizeBatchRequest 校验结构并规范化标签。它不读账户状态，可在加锁前调用。
func NormalizeBatchRequest(input BatchRequest) (BatchRequest, error) {
	out := BatchRequest{Operation: input.Operation}
	switch input.Operation {
	case "tags", "pause", "resume", "delete":
	default:
		return out, fmt.Errorf("%w: operation 只能是 tags、pause、resume 或 delete", ErrInvalidBatch)
	}
	if len(input.AccountIDs) == 0 {
		return out, fmt.Errorf("%w: 至少选择一个账户", ErrInvalidBatch)
	}
	if len(input.AccountIDs) > maxBatchAccounts {
		return out, fmt.Errorf("%w: 一次最多处理 %d 个账户", ErrInvalidBatch, maxBatchAccounts)
	}
	seen := make(map[string]bool, len(input.AccountIDs))
	for _, id := range input.AccountIDs {
		if id == "" || strings.ContainsAny(id, "/\x00") || len(id) > 128 {
			return out, fmt.Errorf("%w: 账户 ID 无效", ErrInvalidBatch)
		}
		if seen[id] {
			return out, fmt.Errorf("%w: 账户 %s 重复", ErrInvalidBatch, id)
		}
		seen[id] = true
	}
	out.AccountIDs = append([]string(nil), input.AccountIDs...)
	if input.Operation != "tags" {
		if len(input.AddTags) > 0 || len(input.RemoveTags) > 0 {
			return out, fmt.Errorf("%w: 只有 tags 操作可以指定标签", ErrInvalidBatch)
		}
		return out, nil
	}
	add, err := normalizeAccountTags(input.AddTags)
	if err != nil {
		return out, fmt.Errorf("%w: %s", ErrInvalidBatch, strings.TrimPrefix(err.Error(), ErrInvalidAccountMetadata.Error()+": "))
	}
	remove, err := normalizeAccountTags(input.RemoveTags)
	if err != nil {
		return out, fmt.Errorf("%w: %s", ErrInvalidBatch, strings.TrimPrefix(err.Error(), ErrInvalidAccountMetadata.Error()+": "))
	}
	if len(add) == 0 && len(remove) == 0 {
		return out, fmt.Errorf("%w: 请填写要添加或移除的标签", ErrInvalidBatch)
	}
	for _, tag := range add {
		if containsFold(remove, tag) {
			return out, fmt.Errorf("%w: 标签「%s」不能同时添加和移除", ErrInvalidBatch, tag)
		}
	}
	out.AddTags, out.RemoveTags = add, remove
	return out, nil
}

func containsFold(values []string, value string) bool {
	for _, item := range values {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

// PlanBatch 只读生成批量计划。request 必须已经过 NormalizeBatchRequest。
func (m *Manager) PlanBatch(request BatchRequest, references map[string][]string) (*BatchPlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildBatchPlanLocked(request, references)
}

func (m *Manager) buildBatchPlanLocked(request BatchRequest, references map[string][]string) (*BatchPlan, error) {
	var missing []string
	for _, id := range request.AccountIDs {
		if m.accounts[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, &BatchMissingError{IDs: missing}
	}
	plan := &BatchPlan{
		Operation: request.Operation, BaseVersion: m.versionLocked(),
		AddTags: request.AddTags, RemoveTags: request.RemoveTags,
		Entries: make([]BatchEntry, 0, len(request.AccountIDs)), items: make([]*account, 0, len(request.AccountIDs)),
	}
	for _, id := range request.AccountIDs {
		item := m.accounts[id]
		providers := append([]string{}, references[id]...)
		sort.Strings(providers)
		before := BatchAccountState{Exists: true, Enabled: !item.Paused, Tags: append([]string{}, item.Tags...)}
		entry := BatchEntry{AccountID: id, Email: item.Email, Before: before, After: before, Providers: providers}
		entry.After.Tags = append([]string{}, before.Tags...)
		switch request.Operation {
		case "tags":
			entry.After.Tags = applyTagChange(before.Tags, request.AddTags, request.RemoveTags)
			if len(entry.After.Tags) > maxAccountTags {
				entry.Errors = append(entry.Errors, fmt.Sprintf("添加后超过 %d 个标签", maxAccountTags))
			}
		case "pause":
			entry.After.Enabled = false
		case "resume":
			entry.After.Enabled = true
		case "delete":
			entry.After = BatchAccountState{Tags: []string{}}
			if len(providers) > 0 {
				entry.Errors = append(entry.Errors, "仍被 Provider 引用："+strings.Join(providers, ", ")+"；请先解除绑定")
			}
		}
		switch {
		case len(entry.Errors) > 0:
			entry.Action = "blocked"
		case batchStateEqual(entry.Before, entry.After):
			entry.Action = "unchanged"
		default:
			entry.Action = "change"
		}
		plan.Entries = append(plan.Entries, entry)
		plan.items = append(plan.items, item)
	}
	plan.summarize()
	return plan, nil
}

// applyTagChange 先移除再追加，保持已有标签的原始顺序与大小写；比较不区分大小写。
func applyTagChange(current, add, remove []string) []string {
	out := make([]string, 0, len(current)+len(add))
	for _, tag := range current {
		if !containsFold(remove, tag) {
			out = append(out, tag)
		}
	}
	for _, tag := range add {
		if !containsFold(out, tag) {
			out = append(out, tag)
		}
	}
	return out
}

func batchStateEqual(a, b BatchAccountState) bool {
	if a.Exists != b.Exists || a.Enabled != b.Enabled || len(a.Tags) != len(b.Tags) {
		return false
	}
	for i := range a.Tags {
		if a.Tags[i] != b.Tags[i] {
			return false
		}
	}
	return true
}

func (p *BatchPlan) summarize() {
	p.Summary = BatchSummary{Total: len(p.Entries)}
	for _, entry := range p.Entries {
		switch entry.Action {
		case "blocked":
			p.Summary.Blocked++
			for _, message := range entry.Errors {
				label := entry.Email
				if label == "" {
					label = entry.AccountID
				}
				p.Blockers = append(p.Blockers, label+"："+message)
			}
		case "change":
			p.Summary.Change++
		default:
			p.Summary.Unchanged++
		}
	}
	p.CanApply = p.Summary.Blocked == 0 && p.Summary.Change > 0
	p.NoChanges = p.Summary.Blocked == 0 && p.Summary.Change == 0
}

// Fingerprint 返回计划的内部绑定摘要：操作、基础版本、规范化标签和逐项前后状态。
// 调用方再用服务端密钥做 HMAC 后放进预检凭证，不直接对外暴露。
func (p *BatchPlan) Fingerprint() string {
	var builder strings.Builder
	builder.WriteString("batch-v1\x00" + p.Operation + "\x00" + p.BaseVersion)
	builder.WriteString("\x00add:" + strings.Join(p.AddTags, "\x01"))
	builder.WriteString("\x00remove:" + strings.Join(p.RemoveTags, "\x01"))
	for _, entry := range p.Entries {
		fmt.Fprintf(&builder, "\x00%s|%s|%t>%t|%s>%s|%s", entry.AccountID, entry.Action,
			entry.Before.Enabled, entry.After.Enabled,
			strings.Join(entry.Before.Tags, "\x01"), strings.Join(entry.After.Tags, "\x01"),
			strings.Join(entry.Providers, "\x01"))
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// ApplyBatch 在账户写锁内按当前状态重建计划，交给 verify 确认与预检一致后一次写盘。
// verify 返回错误时不做任何改动并原样返回该错误。写盘失败整批回滚，返回 ErrAccountBatchPersist。
func (m *Manager) ApplyBatch(request BatchRequest, references map[string][]string, verify func(*BatchPlan) error) (*BatchPlan, string, error) {
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrAccountBatchPersist, err)
	}
	defer unlock()
	plan, err := m.buildBatchPlanLocked(request, references)
	if err != nil {
		return nil, "", err
	}
	if verify != nil {
		if err := verify(plan); err != nil {
			return nil, "", err
		}
	}
	if !plan.CanApply {
		return nil, "", ErrBatchNotApplicable
	}
	changed := make([]*account, 0, len(plan.items))
	for i, entry := range plan.Entries {
		if entry.Action == "change" {
			changed = append(changed, plan.items[i])
		}
	}
	switch request.Operation {
	case "pause", "resume":
		if err := m.setPausedLocked(changed, request.Operation == "pause"); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrAccountBatchPersist, err)
		}
	case "tags":
		previous := make([][]string, len(changed))
		next := make(map[*account][]string, len(changed))
		for i, entry := range plan.Entries {
			if entry.Action == "change" {
				next[plan.items[i]] = entry.After.Tags
			}
		}
		for i, item := range changed {
			previous[i] = item.Tags
			if tags := next[item]; len(tags) > 0 {
				item.Tags = append([]string(nil), tags...)
			} else {
				item.Tags = nil
			}
		}
		if err := m.persistLocked(); err != nil {
			for i, item := range changed {
				item.Tags = previous[i]
			}
			return nil, "", fmt.Errorf("%w: %v", ErrAccountBatchPersist, err)
		}
		m.bumpVersionLocked()
	case "delete":
		for _, item := range changed {
			delete(m.accounts, item.ID)
		}
		if err := m.persistLocked(); err != nil {
			for _, item := range changed {
				m.accounts[item.ID] = item
			}
			return nil, "", fmt.Errorf("%w: %v", ErrAccountBatchPersist, err)
		}
		m.bumpVersionLocked()
	}
	return plan, m.versionLocked(), nil
}

// Views 返回指定 ID 中仍存在账户的安全视图，按请求顺序。
func (m *Manager) Views(ids []string, references map[string][]string) []AccountView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AccountView, 0, len(ids))
	for _, id := range ids {
		item := m.accounts[id]
		if item == nil {
			continue
		}
		view := m.viewOf(item)
		view.Providers = append([]string(nil), references[id]...)
		sort.Strings(view.Providers)
		out = append(out, view)
	}
	return out
}
