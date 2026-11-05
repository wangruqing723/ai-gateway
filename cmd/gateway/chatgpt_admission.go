package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/queue"
	"ai-gateway/internal/router"
)

// 第五阶段：账户共享约束、共享冷却与额度感知排序（DESIGN 6.4）。

// applyChatGPTAccountLimits 把账户共享约束同步到队列和账户管理器。调用方不得持有 cfgMu，
// 账户锁不与配置锁嵌套。
func (s *server) applyChatGPTAccountLimits(cfg *config.Config) {
	if cfg == nil {
		return
	}
	limits := cfg.ChatGPTAccountLimits
	if s.qm != nil {
		s.qm.SetGroupLimits(queue.GroupLimits{MaxConcurrent: limits.ConcurrencyLimit(), MaxPerSecond: limits.MaxPerSecond})
	}
	if s.chatgptAuth != nil {
		s.chatgptAuth.SetSharedCooldown(limits.SharedCooldown)
	}
}

// candidateAccountCooldown 返回候选绑定账户仍未到期的共享冷却截止时间；非 OAuth 或未冷却时为零值。
func (s *server) candidateAccountCooldown(p *config.Provider) time.Time {
	if p == nil || p.AuthType != "chatgptOAuth" || s.chatgptAuth == nil {
		return time.Time{}
	}
	return s.chatgptAuth.CooldownUntil(p.AccountID)
}

// candidateAccountBlocked 判断候选是否一定会因账户暂停或冷却被跳过。
func (s *server) candidateAccountBlocked(p *config.Provider) bool {
	return s.candidateAccountPaused(p) || !s.candidateAccountCooldown(p).IsZero()
}

// usageLimitResetAt 解析 ChatGPT 的额度耗尽错误正文：HTTP 429 且 error.type 为
// usage_limit_reached 时返回 ok，并给出 resets_at、resets_in_seconds、Retry-After 中最晚的截止时间；
// 三者都没有时返回零值，由账户管理器使用默认冷却。其他错误一律不识别。
func usageLimitResetAt(status int, body string, retryAfter time.Duration, now time.Time) (time.Time, bool) {
	if status != http.StatusTooManyRequests || body == "" {
		return time.Time{}, false
	}
	var payload struct {
		Error *struct {
			Type            string          `json:"type"`
			ResetsAt        json.RawMessage `json:"resets_at"`
			ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil || payload.Error == nil || payload.Error.Type != "usage_limit_reached" {
		return time.Time{}, false
	}
	var until time.Time
	later := func(candidate time.Time) {
		if candidate.After(until) {
			until = candidate
		}
	}
	if seconds, ok := jsonNumber(payload.Error.ResetsAt); ok && seconds > 0 {
		if seconds > 1e12 {
			seconds /= 1000 // 毫秒时间戳
		}
		// 超远的时间由账户管理器夹到冷却上限；这里只防止整数溢出。
		if seconds < math.MaxInt32*4 {
			later(time.Unix(int64(seconds), 0))
		}
	}
	if seconds, ok := jsonNumber(payload.Error.ResetsInSeconds); ok && seconds > 0 && seconds < float64(chatgptauth.MaxCooldown/time.Second)*2 {
		later(now.Add(time.Duration(seconds * float64(time.Second))))
	}
	if retryAfter > 0 {
		later(now.Add(retryAfter))
	}
	if !until.After(now) {
		until = time.Time{}
	}
	return until, true
}

// jsonNumber 接受 JSON 数字或数字字符串。
func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// reportChatGPTUsageLimit 在真实上游尝试返回额度耗尽时让账户进入共享冷却，返回生效的截止时间。
// 诊断调用不改变运行时准入状态。
func (s *server) reportChatGPTUsageLimit(ticket *chatgptauth.InvocationTicket, diagnostic bool, status int, body string, retryAfter time.Duration) time.Time {
	if ticket == nil || diagnostic || s.chatgptAuth == nil {
		return time.Time{}
	}
	until, ok := usageLimitResetAt(status, body, retryAfter, time.Now())
	if !ok {
		return time.Time{}
	}
	return s.chatgptAuth.ReportUsageLimit(ticket, until)
}

// writeAccountUsageLimited 写出全部候选因账户共享冷却而没有可用请求的终态。
func writeAccountUsageLimited(w http.ResponseWriter, soonest time.Time) string {
	message := "全部候选上游绑定的 ChatGPT OAuth 账户额度已耗尽，处于冷却中"
	if !soonest.IsZero() {
		remaining := time.Until(soonest)
		seconds := int((remaining + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("retry-after", strconv.Itoa(seconds))
	}
	writeJSONError(w, http.StatusTooManyRequests, "account_usage_limited", message)
	return message
}

// handleChatGPTAccountCooldown 手动解除账户共享冷却：DELETE /api/chatgpt/accounts/{id}/cooldown。
func (s *server) handleChatGPTAccountCooldown(w http.ResponseWriter, r *http.Request, escapedID string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodDelete {
		writeMethodNotAllowed(w, http.MethodDelete)
		return
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return
	}
	id, err := url.PathUnescape(escapedID)
	if err != nil || !validChatGPTAccountID(id) {
		writeJSONError(w, http.StatusBadRequest, "invalid_account_id", "账户 ID 无效")
		return
	}
	view, changed, err := s.chatgptAuth.ClearCooldown(id)
	if errors.Is(err, chatgptauth.ErrAccountNotFound) {
		writeJSONError(w, http.StatusNotFound, "account_not_found", "ChatGPT OAuth 账户不存在")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "account_cooldown_clear_failed", "解除账户冷却失败")
		return
	}
	s.cfgMu.RLock()
	view.Providers = append([]string(nil), chatGPTAccountReferences(s.cfg)[id]...)
	s.cfgMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"account": view, "changed": changed}})
}

// chatGPTAdmissionView 是账户统计接口的准入快照（API_SPEC 7.1）。
type chatGPTAdmissionView struct {
	Limited        bool   `json:"limited"`
	Running        int    `json:"running"`
	Queued         int    `json:"queued"`
	MaxConcurrent  int    `json:"maxConcurrent,omitempty"`
	MaxPerSecond   int    `json:"maxPerSecond,omitempty"`
	SharedCooldown bool   `json:"sharedCooldown"`
	CooldownUntil  string `json:"cooldownUntil,omitempty"`
}

func (s *server) chatGPTAdmission(id string) chatGPTAdmissionView {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	var view chatGPTAdmissionView
	if cfg != nil {
		limits := cfg.ChatGPTAccountLimits
		view.SharedCooldown = limits.SharedCooldown
		// 直通模式跳过本地排队，账户并发与速率不生效（D04）。
		if limits.Enabled() && !cfg.DirectMode {
			view.Limited = true
			view.MaxConcurrent = limits.ConcurrencyLimit()
			view.MaxPerSecond = limits.MaxPerSecond
			if s.qm != nil {
				status := s.qm.GroupStatusOf(id)
				view.Running, view.Queued = status.Running, status.Queued
			}
		}
	}
	if s.chatgptAuth != nil {
		if until := s.chatgptAuth.CooldownUntil(id); !until.IsZero() {
			view.CooldownUntil = until.UTC().Format(time.RFC3339)
		}
	}
	return view
}

// applyQuotaOrder 在策略顺序之后按账户额度做稳定分层：正常、偏低、已耗尽，层内保持原顺序。
// sticky 是粘性命中的候选下标，命中时保持在队首；它是否可准入由候选循环的跳过逻辑决定。
func (s *server) applyQuotaOrder(matched *router.Match, order []int, sticky int) []int {
	if matched == nil || !matched.QuotaAware || len(order) <= 1 || s.chatgptAuth == nil {
		return order
	}
	tiers := make(map[int]int, len(order))
	for _, index := range order {
		p := matched.Candidates[index].Provider
		if p != nil && p.AuthType == "chatgptOAuth" {
			tiers[index] = s.chatgptAuth.QuotaTier(p.AccountID)
		}
	}
	out := append([]int(nil), order...)
	start := 0
	if sticky >= 0 && len(out) > 0 && out[0] == sticky {
		start = 1
	}
	rest := out[start:]
	sort.SliceStable(rest, func(a, b int) bool { return tiers[rest[a]] < tiers[rest[b]] })
	return out
}
