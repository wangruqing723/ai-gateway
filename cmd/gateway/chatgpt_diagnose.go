package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/converter"
	"ai-gateway/internal/metrics"
	"ai-gateway/internal/router"
)

// 账户分层诊断：本地检查 → 刷新链路 → 用量查询 → 实际调用。
// 只有调用方显式选择的步骤才会出网；实际调用会消耗额度，必须带确认字段。
// 诊断结果只返回固定类别和文案，不含令牌、代理地址、上游正文或模型输出；
// 诊断调用不写请求日志与指标，不上报熔断器，避免混入用户调用统计。

const (
	maxChatGPTDiagnoseRequestBytes = 4 << 10
	// chatGPTDiagnoseConfirm 是实际调用诊断的消费确认值，避免误触发消耗额度的请求。
	chatGPTDiagnoseConfirm = "consume_quota"
	// chatGPTDiagnoseMaxOutputTokens 限制测试调用的输出上限（暂定值，待确认）。
	chatGPTDiagnoseMaxOutputTokens = 256
	chatGPTDiagnoseMaxModelLength  = 200
	chatGPTDiagnosePrompt          = "Reply with OK."
)

// chatGPTDiagnoseBudget 是单次诊断的总时长预算（已确认 45 秒）；测试可调小。
var chatGPTDiagnoseBudget = 45 * time.Second

// errChatGPTDiagnoseBudget 是总预算耗尽时的取消原因，用于区分「超时」与「调用方取消」的文案。
var errChatGPTDiagnoseBudget = errors.New("诊断超过总时长预算")

var chatGPTDiagnoseStepOrder = []string{"local", "refresh", "usage", "inference"}

var diagnoseReqCounter uint64

type chatGPTDiagnoseRequest struct {
	Checks      []string `json:"checks,omitempty"`
	RefreshMode string   `json:"refreshMode,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Model       string   `json:"model,omitempty"`
	Confirm     string   `json:"confirm,omitempty"`
}

// chatGPTDiagnoseStep 是一个步骤的结论。status 取 success、failure、skipped、cancelled，
// 不用一个总「健康」掩盖部分失败。
type chatGPTDiagnoseStep struct {
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	Category       string   `json:"category,omitempty"`
	Message        string   `json:"message"`
	Details        []string `json:"details,omitempty"`
	StartedAt      string   `json:"startedAt,omitempty"`
	DurationMs     int64    `json:"durationMs"`
	ProxySource    string   `json:"proxySource,omitempty"`
	HTTPStatus     int      `json:"httpStatus,omitempty"`
	TokenExchanged bool     `json:"tokenExchanged,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	actualCall     bool
}

type chatGPTDiagnoseReport struct {
	AccountID      string                   `json:"accountId"`
	StartedAt      string                   `json:"startedAt"`
	DurationMs     int64                    `json:"durationMs"`
	BudgetMs       int64                    `json:"budgetMs"`
	TokenExchanged bool                     `json:"tokenExchanged"`
	ActualCall     bool                     `json:"actualCall"`
	Steps          []chatGPTDiagnoseStep    `json:"steps"`
	Account        *chatgptauth.AccountView `json:"account,omitempty"`
}

type chatGPTDiagnosePlan struct {
	selected     map[string]bool
	forceRefresh bool
	provider     string
	model        string
}

// normalizeChatGPTDiagnoseRequest 校验步骤组合。本地检查总会执行；未选择出网步骤时不得携带其参数，
// 避免调用方以为某个选项生效、实际却被忽略。
func normalizeChatGPTDiagnoseRequest(input chatGPTDiagnoseRequest) (chatGPTDiagnosePlan, error) {
	plan := chatGPTDiagnosePlan{selected: map[string]bool{"local": true}}
	if len(input.Checks) > len(chatGPTDiagnoseStepOrder) {
		return plan, errors.New("诊断步骤过多")
	}
	seen := make(map[string]bool, len(input.Checks))
	for _, check := range input.Checks {
		switch check {
		case "local", "refresh", "usage", "inference":
		default:
			return plan, fmt.Errorf("不支持的诊断步骤 %q", check)
		}
		if seen[check] {
			return plan, fmt.Errorf("诊断步骤 %q 重复", check)
		}
		seen[check] = true
		plan.selected[check] = true
	}
	if plan.selected["refresh"] {
		switch input.RefreshMode {
		case "", "auto":
		case "force":
			plan.forceRefresh = true
		default:
			return plan, errors.New("refreshMode 只能是 auto 或 force")
		}
	} else if input.RefreshMode != "" {
		return plan, errors.New("未选择刷新链路时不能指定 refreshMode")
	}
	if !plan.selected["inference"] {
		if input.Provider != "" || input.Model != "" || input.Confirm != "" {
			return plan, errors.New("未选择实际调用时不能指定 provider、model 或 confirm")
		}
		return plan, nil
	}
	plan.provider = input.Provider
	plan.model = strings.TrimSpace(input.Model)
	if plan.provider == "" || strings.Contains(plan.provider, "/") {
		return plan, errors.New("实际调用需要指定已绑定此账户的 Provider")
	}
	if plan.model == "" || len(plan.model) > chatGPTDiagnoseMaxModelLength || strings.IndexFunc(plan.model, unicode.IsSpace) >= 0 || strings.IndexFunc(plan.model, unicode.IsControl) >= 0 {
		return plan, errors.New("实际调用需要指定有效的模型名")
	}
	if input.Confirm != chatGPTDiagnoseConfirm {
		return plan, errors.New("实际调用会消耗账户额度，需要 confirm=consume_quota")
	}
	return plan, nil
}
func (s *server) handleChatGPTAccountDiagnose(w http.ResponseWriter, r *http.Request, escapedID string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	// 诊断可能轮换令牌或消耗额度，只接受本机浏览器同源请求。
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "诊断请求须使用 application/json")
		return
	}
	id, err := url.PathUnescape(escapedID)
	if err != nil || id == "" || strings.Contains(id, "/") {
		writeJSONError(w, http.StatusBadRequest, "invalid_account_id", "账户 ID 无效")
		return
	}
	body, err := readBodyLimited(r, maxChatGPTDiagnoseRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "诊断请求超过大小限制")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_diagnose_request", "读取诊断请求失败")
		return
	}
	var input chatGPTDiagnoseRequest
	if err := decodeConfigTransferJSON(body, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_diagnose_request", "诊断请求 JSON 无效")
		return
	}
	plan, err := normalizeChatGPTDiagnoseRequest(input)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_diagnose_request", err.Error())
		return
	}
	if _, ok := s.chatgptAuth.Account(id); !ok {
		writeJSONError(w, http.StatusNotFound, "account_not_found", "ChatGPT OAuth 账户不存在")
		return
	}
	if plan.selected["inference"] {
		s.cfgMu.RLock()
		provider := s.cfg.Providers[plan.provider]
		s.cfgMu.RUnlock()
		// 只接受已保存且绑定此账户的 OAuth Provider，不接受任意 baseUrl、代理或客户端令牌。
		if provider == nil || provider.AuthType != "chatgptOAuth" || provider.AccountID != id {
			writeJSONError(w, http.StatusBadRequest, "invalid_diagnose_target", "实际调用只能使用已绑定此账户的 ChatGPT OAuth Provider")
			return
		}
	}
	if _, running := s.chatGPTDiagnosing.LoadOrStore(id, struct{}{}); running {
		writeJSONError(w, http.StatusConflict, "diagnose_in_progress", "该账户已有诊断在进行，请等待完成")
		return
	}
	defer s.chatGPTDiagnosing.Delete(id)

	// 总预算耗尽按「取消」处理而不是 DeadlineExceeded：后者会被用量快照和调用记录当成
	// 网络超时写入账户状态，而这只是诊断自己的时间上限，不是上游或网络的证据。
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	budgetTimer := time.AfterFunc(chatGPTDiagnoseBudget, func() { cancel(errChatGPTDiagnoseBudget) })
	defer budgetTimer.Stop()
	report := s.runChatGPTDiagnose(ctx, id, plan)
	if r.Context().Err() != nil {
		// 客户端已断开，结果写不出去；已执行步骤的副作用按各自规则保留。
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": report})
}

// runChatGPTDiagnose 按固定顺序执行已选步骤。前置步骤已证明凭据不可用时，后续出网步骤跳过，
// 不重复制造同一失败；总预算耗尽或调用方取消后，未开始的步骤记为取消。
func (s *server) runChatGPTDiagnose(ctx context.Context, id string, plan chatGPTDiagnosePlan) chatGPTDiagnoseReport {
	started := time.Now()
	report := chatGPTDiagnoseReport{
		AccountID: id, StartedAt: started.UTC().Format(time.RFC3339), BudgetMs: chatGPTDiagnoseBudget.Milliseconds(),
	}
	versionBefore, _ := s.chatgptAuth.CredentialVersion(id)
	checks := make([]string, 0, len(plan.selected))
	for _, name := range chatGPTDiagnoseStepOrder {
		if plan.selected[name] {
			checks = append(checks, name)
		}
	}
	logSystem("ChatGPT 账户诊断开始: 账户 %s，步骤 %s", id, strings.Join(checks, ","))

	blocked := ""
	for _, name := range chatGPTDiagnoseStepOrder {
		var step chatGPTDiagnoseStep
		switch {
		case !plan.selected[name]:
			step = chatGPTDiagnoseStep{Name: name, Status: "skipped", Message: "未选择此步骤"}
		case name != "local" && ctx.Err() != nil:
			step = chatGPTDiagnoseStep{Name: name, Status: "cancelled", Message: chatGPTDiagnoseCancelMessage(ctx) + "，此步骤未执行"}
		case name != "local" && blocked != "":
			step = chatGPTDiagnoseStep{Name: name, Status: "skipped", Message: blocked}
		default:
			stepStarted := time.Now()
			switch name {
			case "local":
				step = s.diagnoseChatGPTLocal(id)
			case "refresh":
				step = s.diagnoseChatGPTRefresh(ctx, id, plan.forceRefresh)
			case "usage":
				step = s.diagnoseChatGPTUsage(ctx, id)
			case "inference":
				step = s.diagnoseChatGPTInference(ctx, id, plan.provider, plan.model)
			}
			step.Name = name
			step.StartedAt = stepStarted.UTC().Format(time.RFC3339)
			step.DurationMs = time.Since(stepStarted).Milliseconds()
		}
		switch {
		case name == "local" && step.Category == "authorization":
			blocked = "本地检查显示账户需要重新授权，跳过出网步骤"
		case name == "refresh" && step.Status == "failure":
			blocked = "刷新链路失败，未继续依赖访问令牌的步骤"
		}
		report.TokenExchanged = report.TokenExchanged || step.TokenExchanged
		report.ActualCall = report.ActualCall || step.actualCall
		report.Steps = append(report.Steps, step)
	}
	// 请求路径的自动刷新不返回是否交换；按凭据世代补充判断。并发的真实请求也可能在诊断期间轮换令牌。
	if versionAfter, ok := s.chatgptAuth.CredentialVersion(id); ok && versionAfter != versionBefore {
		report.TokenExchanged = true
	}
	report.DurationMs = time.Since(started).Milliseconds()
	if account, ok := s.chatgptAuth.Account(id); ok {
		s.cfgMu.RLock()
		account.Providers = append([]string(nil), chatGPTAccountReferences(s.cfg)[id]...)
		s.cfgMu.RUnlock()
		report.Account = &account
	}
	statuses := make([]string, 0, len(report.Steps))
	for _, step := range report.Steps {
		statuses = append(statuses, step.Name+"="+step.Status)
	}
	logSystem("ChatGPT 账户诊断结束: 账户 %s，%s，耗时 %dms", id, strings.Join(statuses, " "), report.DurationMs)
	return report
}

func chatGPTDiagnoseCancelMessage(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errChatGPTDiagnoseBudget) {
		return "诊断超过总时长 " + chatGPTDiagnoseBudget.String()
	}
	return "诊断已取消"
}

// diagnoseChatGPTLocal 只读本地状态，不出网，因此不能证明模型调用成功。
func (s *server) diagnoseChatGPTLocal(id string) chatGPTDiagnoseStep {
	account, ok := s.chatgptAuth.Account(id)
	if !ok {
		return chatGPTDiagnoseStep{Status: "failure", Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
	}
	s.cfgMu.RLock()
	providerNames := append([]string(nil), chatGPTAccountReferences(s.cfg)[id]...)
	enabled := make(map[string]bool, len(providerNames))
	for _, name := range providerNames {
		enabled[name] = s.cfg.Providers[name].IsEnabled()
	}
	s.cfgMu.RUnlock()

	var details []string
	if !account.Enabled {
		pausedText := "账户已暂停：网关不会经此账户转发新请求，实际调用步骤也会跳过"
		if account.PausedAt != "" {
			pausedText += "（暂停于 " + account.PausedAt + "）"
		}
		details = append(details, pausedText)
	}
	if account.ExpiresAt != "" {
		details = append(details, "访问令牌到期时间："+account.ExpiresAt)
	}
	if len(providerNames) == 0 {
		details = append(details, "未绑定任何 Provider，网关不会把请求转发到此账户")
	}
	for _, name := range providerNames {
		state := "已启用"
		switch {
		case !enabled[name]:
			state = "已停用，不参与转发"
		case !account.Enabled:
			state = "配置启用，但账户已暂停，不参与转发"
		}
		details = append(details, "绑定 Provider "+name+"："+state)
	}
	details = append(details, "最近一次真实调用："+chatGPTDiagnoseInvocationSummary(account.Invocation))
	details = append(details, "用量快照："+account.UsageState.Message)
	details = append(details, "本地检查不出网，不能证明模型调用可用")

	step := chatGPTDiagnoseStep{Status: "success", Message: account.Authorization.Message, Details: details}
	if account.Authorization.State == "reauthorization_required" {
		step.Status = "failure"
		step.Category = "authorization"
	}
	return step
}

func chatGPTDiagnoseInvocationSummary(invocation chatgptauth.InvocationView) string {
	if invocation.State == "unknown" || invocation.State == "" {
		return invocation.Message
	}
	summary := invocation.Message
	if invocation.Provider != "" {
		summary += "（Provider " + invocation.Provider
		if invocation.Model != "" {
			summary += "，模型 " + invocation.Model
		}
		summary += "）"
	}
	if invocation.ObservedAt != "" {
		summary += "，" + invocation.ObservedAt
	}
	return summary
}

func (s *server) diagnoseChatGPTRefresh(ctx context.Context, id string, force bool) chatGPTDiagnoseStep {
	step := chatGPTDiagnoseStepFromOutcome(ctx, s.chatgptAuth.DiagnoseRefresh(ctx, id, force))
	step.ProxySource = chatGPTManagementProxySource(s.chatgptAuth.OAuthEndpoint())
	if force {
		step.Details = append(step.Details, "强制刷新：即使访问令牌仍有效也交换一次令牌，会轮换刷新凭据")
	}
	return step
}

// diagnoseChatGPTUsage 强制查询一次用量；失败不等于账户整体失效。
func (s *server) diagnoseChatGPTUsage(ctx context.Context, id string) chatGPTDiagnoseStep {
	step := chatGPTDiagnoseStepFromOutcome(ctx, s.chatgptAuth.DiagnoseUsage(ctx, id))
	step.ProxySource = chatGPTManagementProxySource(s.chatgptAuth.UsageEndpoint())
	return step
}

// chatGPTDiagnoseStepFromOutcome 把账户包的安全结论转成步骤；诊断上下文结束时一律记为取消，
// 不把总预算耗尽或用户取消误报成网络超时。
func chatGPTDiagnoseStepFromOutcome(ctx context.Context, outcome chatgptauth.CheckOutcome) chatGPTDiagnoseStep {
	step := chatGPTDiagnoseStep{
		Status: "success", Category: outcome.Category, Message: outcome.Message,
		HTTPStatus: outcome.HTTPStatus, TokenExchanged: outcome.Exchanged,
	}
	switch {
	case outcome.Category == "":
	case outcome.Category == "cancelled" || ctx.Err() != nil:
		step.Status = "cancelled"
		step.Category = ""
		step.Message = chatGPTDiagnoseCancelMessage(ctx)
		if ctx.Err() == nil {
			step.Message = outcome.Message
		}
	default:
		step.Status = "failure"
	}
	return step
}

// diagnoseChatGPTInference 经指定 Provider 发起一次最小测试请求，沿用该 Provider 的代理、
// 队列限额和请求期刷新路径。结果会写回账户的最近调用记录（这是一次真实上游调用），
// 但不写请求日志与指标，也不上报熔断器。
func (s *server) diagnoseChatGPTInference(ctx context.Context, id, providerName, model string) chatGPTDiagnoseStep {
	step := chatGPTDiagnoseStep{Provider: providerName, Model: model}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	provider := cfg.Providers[providerName]
	if provider == nil || provider.AuthType != "chatgptOAuth" || provider.AccountID != id {
		step.Status, step.Message = "skipped", "Provider 绑定在诊断期间已变化，未发起实际调用"
		return step
	}
	step.ProxySource = chatGPTProviderProxySource(provider)
	if !provider.IsEnabled() {
		step.Status, step.Message = "skipped", "Provider 已停用；诊断不绕过启停设置，未发起实际调用"
		return step
	}
	if s.chatgptAuth.IsPaused(id) {
		step.Status, step.Message = "skipped", "账户已暂停；诊断不绕过暂停状态，未发起实际调用"
		return step
	}
	if s.breaker != nil {
		for _, name := range s.breaker.OpenProviders() {
			if name == providerName {
				step.Details = append(step.Details, "该 Provider 熔断器处于开路状态，普通请求当前不会路由到它；诊断仍单独发起一次调用")
			}
		}
	}

	raw := map[string]any{
		"model":             model,
		"stream":            false,
		"max_output_tokens": chatGPTDiagnoseMaxOutputTokens,
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": chatGPTDiagnosePrompt}}}},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	if err != nil {
		step.Status, step.Category, step.Message = "failure", "gateway", "无法构建诊断请求"
		return step
	}
	providerCopy := *provider
	writer := &chatGPTDiagnoseWriter{header: make(http.Header)}
	detail := &metrics.AttemptDetail{}
	outcome := s.forwardAttempt(writer, request, forwardAttemptInput{
		cfg: cfg, start: time.Now(), reqID: fmt.Sprintf("diag%04d", atomic.AddUint64(&diagnoseReqCounter, 1)%10000),
		clientFormat: "openai-responses", originalModel: model,
		internal: converter.FromOpenAIResponses(raw), rawBody: raw,
		candidate: router.Candidate{Provider: &providerCopy, TargetModel: model, MaxTokens: provider.MaxTokens, ExtraBody: provider.ExtraBody},
		attemptNo: 1, httpAttemptNo: 1, diagnostic: true,
		// Provider 的 maxTokens 可硬覆盖请求值；再用 contextBudget 向下压，避免诊断上限被抬高。
		contextBudget: chatGPTDiagnoseMaxOutputTokens,
		reqLog:        &metrics.RequestLog{}, detail: detail,
	})
	step.actualCall = outcome.requestStarted
	classifyChatGPTDiagnoseInference(ctx, &step, outcome, detail)
	return step
}

// classifyChatGPTDiagnoseInference 只按有证据的范围下结论：特定模型失败不等于全部模型失败。
func classifyChatGPTDiagnoseInference(ctx context.Context, step *chatGPTDiagnoseStep, outcome forwardAttemptOutcome, detail *metrics.AttemptDetail) {
	if outcome.oauthErr != nil {
		described := chatgptauth.DescribeCredentialError(outcome.oauthErr)
		*step = mergeChatGPTDiagnoseStep(*step, chatGPTDiagnoseStepFromOutcome(ctx, described))
		if step.Status == "failure" {
			step.Message = "经该 Provider 获取访问令牌失败：" + described.Message
		}
		return
	}
	if outcome.accountPaused {
		step.Status, step.Message = "skipped", "账户在诊断期间被暂停，未发起实际调用"
		return
	}
	if outcome.buildErr != "" {
		step.Status, step.Category = "failure", "gateway"
		// 构建错误可能包含代理地址，只返回固定文案。
		step.Message = "网关无法构建发往该 Provider 的请求，请检查 Provider 配置"
		if detail.Reason == "proxy_error" {
			step.Message = "无法解析该 Provider 的代理设置"
		}
		return
	}
	if !outcome.requestStarted {
		switch {
		case ctx.Err() != nil || detail.Reason == "client_disconnected":
			step.Status, step.Message = "cancelled", chatGPTDiagnoseCancelMessage(ctx)
		case detail.Reason == "queue_timeout":
			step.Status, step.Category, step.Message = "failure", "gateway", "等待该 Provider 的并发槽位超时，未发出请求"
		default:
			step.Status, step.Category, step.Message = "failure", "gateway", "该 Provider 的本地队列拒绝了请求，未发出请求"
		}
		return
	}
	if outcome.invocation == nil {
		if ctx.Err() != nil {
			step.Status, step.Message = "cancelled", chatGPTDiagnoseCancelMessage(ctx)+"，上游结果未知"
			return
		}
		step.Status, step.Category, step.Message = "failure", "unknown", "已发出请求但未得到可判定的上游结果"
		return
	}
	result := *outcome.invocation
	step.HTTPStatus = result.HTTPStatus
	step.Status, step.Category, step.Message = chatGPTDiagnoseInferenceVerdict(result)
	if ctx.Err() != nil && !result.Success {
		step.Details = append(step.Details, chatGPTDiagnoseCancelMessage(ctx)+"，请求在预算耗尽时被中止")
	}
}

func mergeChatGPTDiagnoseStep(base, update chatGPTDiagnoseStep) chatGPTDiagnoseStep {
	base.Status, base.Category, base.Message, base.HTTPStatus = update.Status, update.Category, update.Message, update.HTTPStatus
	return base
}

func chatGPTDiagnoseInferenceVerdict(result chatgptauth.InvocationResult) (string, string, string) {
	status := result.HTTPStatus
	switch {
	case result.Success:
		return "success", "", "测试调用成功：该账户经此 Provider 可以调用此模型"
	case result.Reason == "conversion_error":
		return "failure", "gateway", "上游已响应，但网关无法转换响应"
	case result.Reason == "stream_interrupted":
		return "failure", "network", "上游开始响应后中断，可能是网络、代理或超时问题"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "failure", "authorization", fmt.Sprintf("上游拒绝了请求（HTTP %d），请检查账户授权、套餐或模型权限", status)
	case status == http.StatusTooManyRequests:
		return "failure", "quota", "上游限流或额度已用尽（HTTP 429）"
	case status >= 500:
		return "failure", "upstream", fmt.Sprintf("上游服务异常（HTTP %d）", status)
	case status > 0:
		return "failure", "model", fmt.Sprintf("上游拒绝了请求（HTTP %d），可能与模型名或请求参数有关；不代表其他模型也不可用", status)
	case strings.Contains(result.Reason, "timeout"):
		return "failure", "timeout", "等待上游响应超时，可能是网络或代理问题"
	default:
		return "failure", "network", "没有得到上游响应，可能是网络或代理问题"
	}
}

// chatGPTDiagnoseWriter 丢弃测试调用的响应正文，只保留状态码：诊断不返回模型输出或上游原文。
type chatGPTDiagnoseWriter struct {
	header http.Header
	status int
	bytes  int64
}

func (w *chatGPTDiagnoseWriter) Header() http.Header { return w.header }

func (w *chatGPTDiagnoseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *chatGPTDiagnoseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(data))
	return len(data), nil
}

func (w *chatGPTDiagnoseWriter) Flush() {}

// chatGPTManagementProxySource 报告 OAuth 管理请求的代理来源类别，不暴露代理地址。
// 管理客户端在启动时按 AI_GATEWAY_OAUTH_PROXY 创建，未设置时继承环境代理。
func chatGPTManagementProxySource(target string) string {
	if os.Getenv("AI_GATEWAY_OAUTH_PROXY") != "" {
		return "oauth_proxy"
	}
	return environmentProxySource(target)
}

func chatGPTProviderProxySource(provider *config.Provider) string {
	if provider.Proxy != "" {
		return "provider_proxy"
	}
	return environmentProxySource(provider.BaseURL)
}

// environmentProxySource 与默认客户端同样经 http.ProxyFromEnvironment 判断，因此会遵守 NO_PROXY。
func environmentProxySource(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return "direct"
	}
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: parsed})
	if err != nil || proxyURL != nil {
		return "environment"
	}
	return "direct"
}
