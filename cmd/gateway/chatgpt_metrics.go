package main

import (
	"net/http"
	"net/url"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/metrics"
)

// chatGPTAccountEventLimit 是账户统计响应附带的最近事件条数上限。
const chatGPTAccountEventLimit = 10

// chatGPTAccountMetricsView 是账户统计接口的响应体：统计视图加该账户最近的运行事件。
type chatGPTAccountMetricsView struct {
	chatgptauth.AccountStatsView
	Admission chatGPTAdmissionView `json:"admission"`
	Events    []metrics.Event      `json:"events"`
}

// handleChatGPTAccountMetrics 返回单个账户的窗口统计、进程累计与最近事件。只读，不发出网络请求。
func (s *server) handleChatGPTAccountMetrics(w http.ResponseWriter, r *http.Request, escapedID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	id, err := url.PathUnescape(escapedID)
	if err != nil || !validChatGPTAccountID(id) {
		writeJSONError(w, http.StatusBadRequest, "invalid_account_id", "账户 ID 无效")
		return
	}
	stats, ok := s.chatgptAuth.AccountStats(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "account_not_found", "ChatGPT OAuth 账户不存在")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	view := chatGPTAccountMetricsView{AccountStatsView: stats, Admission: s.chatGPTAdmission(id), Events: s.eventLog.AccountEvents(id, chatGPTAccountEventLimit)}
	writeJSON(w, http.StatusOK, map[string]any{"data": view})
}

// handleChatGPTAccountEvents 返回含账户维度的运行事件，按时间倒序，结构与 /api/metrics 的 events 相同。
func (s *server) handleChatGPTAccountEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"data": s.eventLog.AccountEvents("", 0)})
}
