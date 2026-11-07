package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"ai-gateway/internal/providermodels"
)

// decodeChatGPTDiagnoseJSON 在标准严格解码之前拒绝非法 UTF-8 和调用字段的 null，避免静默转为空值。
func decodeChatGPTDiagnoseJSON(body []byte, target any) error {
	if !utf8.Valid(body) {
		return errors.New("诊断请求必须是有效 UTF-8")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	for _, key := range []string{"mode", "provider", "model", "prompt", "confirm"} {
		if raw, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("调用参数不能为 null")
		}
	}
	var checks []string
	if raw, ok := fields["checks"]; ok {
		if err := json.Unmarshal(raw, &checks); err != nil {
			return err
		}
	}
	inference := false
	for _, check := range checks {
		inference = inference || check == "inference"
	}
	if !inference {
		for _, key := range []string{"mode", "provider", "model", "prompt", "confirm"} {
			if _, ok := fields[key]; ok {
				return errors.New("未选择实际调用时不能携带调用参数")
			}
		}
	}
	return decodeConfigTransferJSON(body, target)
}

// reconcileAccountCatalogs 回收已删除账户的缓存，独立于 Provider 配置的缓存命名空间。
func (s *server) reconcileAccountCatalogs() {
	connections := make(map[string]string)
	for _, account := range s.chatgptAuth.List(nil) {
		connections[account.ID] = chatGPTPreflightBaseURL
	}
	s.accountCatalogCache.Reconcile(connections)
}

func (s *server) handleChatGPTAccountModels(w http.ResponseWriter, r *http.Request, escapedID string, refresh bool) {
	setProviderQueryHeaders(w)
	// 浏览器同源 GET 通常不携带 Origin，沿用现有模型读取接口的 Host/跨站边界。
	// 显式刷新 POST 仍由管理写入校验要求可信 Origin。
	if isCrossSiteRequest(r) {
		writeForbiddenOrigin(w)
		return
	}
	if refresh {
		var input struct{}
		if !readChatGPTManagementRequest(w, r, &input, "invalid_query_request") {
			return
		}
	} else if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	id, err := url.PathUnescape(escapedID)
	if err != nil || !validChatGPTAccountID(id) {
		writeJSONError(w, 400, "invalid_account_id", "账户 ID 无效")
		return
	}
	if _, ok := s.chatgptAuth.Account(id); !ok {
		writeJSONError(w, 404, "account_not_found", "账户不存在")
		return
	}
	s.reconcileAccountCatalogs()
	defer s.reconcileAccountCatalogs()
	ctx, cancel := s.providerQueryContextWithTimeout(r.Context(), providerCatalogBudget)
	defer cancel()
	finish, err := s.accountCatalogCache.Lease(ctx, id, chatGPTPreflightBaseURL)
	if err != nil {
		writeJSONError(w, 502, "query_cancelled", "模型目录查询等待已结束")
		return
	}
	defer finish(nil, nil)
	identity, err := s.chatgptAuth.CatalogIdentity(id)
	if err != nil {
		s.writeCatalogCredentialError(w, err)
		return
	}
	old, fresh := s.accountCatalogCache.Lookup(id, chatGPTPreflightBaseURL, identity)
	if !s.chatgptAuth.CatalogIdentityCurrent(id, identity) {
		writeJSONError(w, 409, "account_changed", "账户身份已变化，请重新查询")
		return
	}
	if failure, wait := s.accountCatalogCache.Failure(id, chatGPTPreflightBaseURL, identity); failure != nil && wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		if old != nil {
			writeAccountCatalog(w, id, old, true, true, failure.Message)
		} else {
			writeJSONError(w, 429, failure.Code, failure.Message)
		}
		return
	}
	if !refresh && old != nil && fresh && s.chatgptAuth.CatalogIdentityCurrent(id, identity) {
		writeAccountCatalog(w, id, old, true, false, "")
		return
	}
	client := s.chatgptAuth.ManagementClient()
	token, remoteID, identity, err := s.chatgptAuth.CatalogCredentials(ctx, id, client)
	if err != nil {
		s.writeCatalogCredentialError(w, err)
		return
	}
	old, _ = s.accountCatalogCache.Lookup(id, chatGPTPreflightBaseURL, identity)
	result, failure := providermodels.Fetch(ctx, client, providermodels.Query{BaseURL: chatGPTPreflightBaseURL, ClientVersion: providermodels.DefaultClientVersion, Token: token, AccountID: remoteID})
	if !s.chatgptAuth.CatalogIdentityCurrent(id, identity) {
		writeJSONError(w, 409, "account_changed", "账户身份已变化，请重新查询")
		return
	}
	if failure != nil {
		if ctx.Err() == nil {
			s.accountCatalogCache.RecordFailure(id, chatGPTPreflightBaseURL, identity, failure)
		}
		if old != nil && ctx.Err() == nil {
			writeAccountCatalog(w, id, old, true, true, failure.Message)
			return
		}
		status := http.StatusBadGateway
		if failure.Code == "rate_limited" {
			status = 429
			if failure.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(failure.RetryAfter.Seconds())+1))
			}
		}
		writeJSONError(w, status, failure.Code, failure.Message)
		return
	}
	finish(identity, result)
	writeAccountCatalog(w, id, result, false, false, "")
}

func writeAccountCatalog(w http.ResponseWriter, id string, result *providermodels.Result, cached, stale bool, message string) {
	if message == "" {
		message = "目录来自此账户，不代表已完成模型调用验证，也不代表其他账户权限"
	}
	writeJSON(w, 200, map[string]any{"accountId": id, "models": result.Models, "source": "chatgpt-codex", "queriedAt": result.QueriedAt, "cached": cached, "stale": stale, "message": message})
}
