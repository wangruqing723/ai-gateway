package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/chatgptauth"
)

// 第三阶段账户管理接口：单账户暂停/恢复、批量预检/应用、批量用量刷新（API_SPEC §5）。
// 三者都只接受可信本机同源 Origin，并严格解码 JSON。

// maxChatGPTManagementRequestBytes 足够容纳 100 个账户 ID、标签和预检凭证。
const maxChatGPTManagementRequestBytes = 64 << 10

// chatGPTUsageBatchBudget 是批量用量刷新的总等待预算（T17 确认 60 秒）；测试可调小。
var chatGPTUsageBatchBudget = 60 * time.Second

// validChatGPTAccountID 与批量规范化使用同一 ID 规则：非空、不含路径分隔符或 NUL、最长 128 字节。
func validChatGPTAccountID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "/\x00")
}

// readChatGPTManagementRequest 统一管理写入接口的来源、大小和严格解码边界。
func readChatGPTManagementRequest(w http.ResponseWriter, r *http.Request, input any, category string) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return false
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return false
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "账户管理请求须使用 application/json")
		return false
	}
	body, err := readBodyLimited(r, maxChatGPTManagementRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "账户管理请求超过大小限制")
		} else {
			writeJSONError(w, http.StatusBadRequest, category, "读取请求失败")
		}
		return false
	}
	if err := decodeConfigTransferJSON(body, input); err != nil {
		writeJSONError(w, http.StatusBadRequest, category, "账户管理请求 JSON 不合法")
		return false
	}
	return true
}

// handleChatGPTAccountState 暂停或恢复单个账户。不修改 Provider 的 enabled 或绑定；
// 与绑定、删除共用配置事务锁，返回的 Provider 有效状态与账户状态来自同一时刻。
func (s *server) handleChatGPTAccountState(w http.ResponseWriter, r *http.Request, escapedID string) {
	var input struct {
		State string `json:"state"`
	}
	if !readChatGPTManagementRequest(w, r, &input, "invalid_account_state") {
		return
	}
	id, err := url.PathUnescape(escapedID)
	if err != nil || !validChatGPTAccountID(id) {
		writeJSONError(w, http.StatusBadRequest, "invalid_account_id", "账户 ID 无效")
		return
	}
	if input.State != "active" && input.State != "paused" {
		writeJSONError(w, http.StatusBadRequest, "invalid_account_state", "state 只能是 active 或 paused")
		return
	}
	etag := strings.TrimSpace(r.Header.Get("If-Match"))
	if !strictETag(etag) {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请携带账户列表的精确 If-Match 版本")
		return
	}
	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	view, changed, version, err := s.chatgptAuth.SetPaused(id, input.State == "paused", etag[1:len(etag)-1])
	switch {
	case errors.Is(err, chatgptauth.ErrAccountNotFound):
		writeJSONError(w, http.StatusNotFound, "account_not_found", "ChatGPT OAuth 账户不存在")
		return
	case errors.Is(err, chatgptauth.ErrAccountVersionConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户版本已变化，请重新读取")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "account_state_update_failed", "保存账户状态失败，未保留本次变更")
		return
	}
	s.cfgMu.RLock()
	view.Providers = append([]string(nil), chatGPTAccountReferences(s.cfg)[id]...)
	providers := make([]map[string]any, 0, len(view.Providers))
	for _, name := range view.Providers {
		enabled := s.cfg.Providers[name].IsEnabled()
		effective := "active"
		if !enabled {
			effective = "provider_disabled"
		} else if !view.Enabled {
			effective = "account_paused"
		}
		providers = append(providers, map[string]any{"name": name, "configEnabled": enabled, "effective": effective})
	}
	s.cfgMu.RUnlock()
	w.Header().Set("ETag", strconv.Quote(version))
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"account": view, "changed": changed, "providers": providers}})
}

// chatGPTBatchRequest 是批量预检与应用的请求体；应用额外携带 previewToken。
type chatGPTBatchRequest struct {
	Operation    string   `json:"operation"`
	AccountIDs   []string `json:"accountIds"`
	AddTags      []string `json:"addTags,omitempty"`
	RemoveTags   []string `json:"removeTags,omitempty"`
	PreviewToken string   `json:"previewToken,omitempty"`
}

// chatGPTBatchPreview 在计划之外附带凭证；不可应用或无变更时两个字段输出 null。
type chatGPTBatchPreview struct {
	*chatgptauth.BatchPlan
	PreviewToken *string `json:"previewToken"`
	ExpiresAt    *string `json:"expiresAt"`
}

// 批量凭证使用独立派生密钥，不能与导入凭证互换。
func (s *server) chatGPTBatchKey() ([]byte, error) {
	base, err := s.configTransferSigningKey()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, base)
	_, _ = mac.Write([]byte("chatgpt-account-batch\x00v1"))
	return mac.Sum(nil), nil
}

// writeChatGPTBatchError 把账户包的批量错误映射为 API_SPEC §5.3 的错误类型。
func writeChatGPTBatchError(w http.ResponseWriter, err error) {
	var missing *chatgptauth.BatchMissingError
	switch {
	case errors.As(err, &missing):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "account_not_found", "message": "部分账户不存在", "accountIds": missing.IDs}})
	case errors.Is(err, chatgptauth.ErrInvalidBatch):
		writeJSONError(w, http.StatusBadRequest, "invalid_batch", err.Error())
	case errors.Is(err, chatgptauth.ErrAccountVersionConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户在预检后已变化，请重新预检")
	case errors.Is(err, chatgptauth.ErrBatchNotApplicable):
		writeJSONError(w, http.StatusConflict, "preview_mismatch", "批量计划已变化或不可应用，请重新预检")
	default:
		writeJSONError(w, http.StatusInternalServerError, "account_batch_failed", "保存批量变更失败，未保留本批次变更")
	}
}

// handleChatGPTAccountBatch 处理 /batch/preview 与 /batch/apply。凭证绑定账户版本、配置版本、
// 规范化输入摘要和计划摘要；应用在配置事务锁与账户写锁内重建计划、比对后一次写盘。
func (s *server) handleChatGPTAccountBatch(w http.ResponseWriter, r *http.Request, apply bool) {
	var input chatGPTBatchRequest
	if !readChatGPTManagementRequest(w, r, &input, "invalid_batch") {
		return
	}
	etag := strings.TrimSpace(r.Header.Get("If-Match"))
	if apply && !strictETag(etag) {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请携带预检返回的精确 If-Match 版本")
		return
	}
	if !apply && input.PreviewToken != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_batch", "预检请求不能携带应用凭证")
		return
	}
	request, err := chatgptauth.NormalizeBatchRequest(chatgptauth.BatchRequest{Operation: input.Operation, AccountIDs: input.AccountIDs, AddTags: input.AddTags, RemoveTags: input.RemoveTags})
	if err != nil {
		writeChatGPTBatchError(w, err)
		return
	}
	key, err := s.chatGPTBatchKey()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "account_batch_failed", "安全预检凭证不可用")
		return
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		writeChatGPTBatchError(w, err)
		return
	}
	digest := chatGPTImportMAC(key, "input", encoded)
	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()
	s.cfgMu.Lock()
	if s.revision == "" {
		s.revision, err = s.issueConfigRevision()
	}
	refs, revision := chatGPTAccountReferences(s.cfg), s.revision
	s.cfgMu.Unlock()
	if err != nil {
		writeChatGPTBatchError(w, err)
		return
	}
	if !apply {
		plan, err := s.chatgptAuth.PlanBatch(request, refs)
		if err != nil {
			writeChatGPTBatchError(w, err)
			return
		}
		result := chatGPTBatchPreview{BatchPlan: plan}
		if plan.CanApply {
			token, expires, err := s.signChatGPTImportToken(key, chatGPTImportToken{Accounts: plan.BaseVersion, Revision: revision, InputDigest: digest, PlanDigest: chatGPTImportMAC(key, "plan", []byte(plan.Fingerprint()))})
			if err != nil {
				writeChatGPTBatchError(w, err)
				return
			}
			result.PreviewToken, result.ExpiresAt = &token, &expires
		}
		w.Header().Set("ETag", strconv.Quote(plan.BaseVersion))
		writeJSON(w, http.StatusOK, map[string]any{"data": result})
		return
	}
	token, err := verifyChatGPTImportToken(input.PreviewToken, key)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_preview_token", "预检凭证无效，请重新预检")
		return
	}
	if s.configTransferTime().Unix() >= token.ExpiresAt {
		writeJSONError(w, http.StatusGone, "preview_expired", "预检已过期，请重新预检")
		return
	}
	if !hmac.Equal([]byte(digest), []byte(token.InputDigest)) {
		writeJSONError(w, http.StatusConflict, "preview_mismatch", "输入与预检不一致")
		return
	}
	if revision != token.Revision {
		writeJSONError(w, http.StatusConflict, "config_conflict", "绑定配置已变化，请重新预检")
		return
	}
	if etag != strconv.Quote(token.Accounts) {
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户版本与预检不一致")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	plan, version, err := s.chatgptAuth.ApplyBatch(request, refs, func(plan *chatgptauth.BatchPlan) error {
		if plan.BaseVersion != token.Accounts {
			return chatgptauth.ErrAccountVersionConflict
		}
		if !hmac.Equal([]byte(chatGPTImportMAC(key, "plan", []byte(plan.Fingerprint()))), []byte(token.PlanDigest)) {
			return chatgptauth.ErrBatchNotApplicable
		}
		return nil
	})
	if err != nil {
		writeChatGPTBatchError(w, err)
		return
	}
	s.reconcileAccountCatalogs()
	w.Header().Set("ETag", strconv.Quote(version))
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"operation": plan.Operation, "summary": plan.Summary, "entries": plan.Entries, "accounts": s.chatgptAuth.Views(request.AccountIDs, refs)}})
}

// handleChatGPTUsageBatchRefresh 只刷新所选账户的用量快照：并发 3、总预算 60 秒、不自动重试。
// 同一时间只允许一个批量刷新；不推进账户控制版本，暂停账户也可以刷新。
func (s *server) handleChatGPTUsageBatchRefresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountIDs  []string `json:"accountIds"`
		RefreshMode string   `json:"refreshMode"`
	}
	if !readChatGPTManagementRequest(w, r, &input, "invalid_batch") {
		return
	}
	if len(input.AccountIDs) == 0 || len(input.AccountIDs) > chatgptauth.MaxUsageBatchAccounts {
		writeJSONError(w, http.StatusBadRequest, "invalid_batch", "一次刷新 1 至 50 个账户")
		return
	}
	seen := make(map[string]bool, len(input.AccountIDs))
	for _, id := range input.AccountIDs {
		if !validChatGPTAccountID(id) || seen[id] {
			writeJSONError(w, http.StatusBadRequest, "invalid_batch", "账户 ID 无效或重复")
			return
		}
		seen[id] = true
	}
	if input.RefreshMode != "" && input.RefreshMode != "auto" && input.RefreshMode != "force" {
		writeJSONError(w, http.StatusBadRequest, "invalid_batch", "refreshMode 只能是 auto 或 force")
		return
	}
	if !s.chatGPTBatchRefreshing.CompareAndSwap(false, true) {
		writeJSONError(w, http.StatusConflict, "batch_refresh_in_progress", "已有批量刷新正在执行")
		return
	}
	defer s.chatGPTBatchRefreshing.Store(false)
	budget := chatGPTUsageBatchBudget
	started := time.Now()
	results := s.chatgptAuth.RefreshUsageBatch(r.Context(), input.AccountIDs, input.RefreshMode == "force", budget)
	if r.Context().Err() != nil {
		return
	}
	s.cfgMu.RLock()
	refs := chatGPTAccountReferences(s.cfg)
	s.cfgMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"results": results, "accounts": s.chatgptAuth.Views(input.AccountIDs, refs), "durationMs": time.Since(started).Milliseconds(), "budgetMs": budget.Milliseconds()}})
}
