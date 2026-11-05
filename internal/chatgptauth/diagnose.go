package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CheckOutcome 是一次诊断步骤的安全结论，不含令牌、代理地址或上游原始正文。
// Category 为空表示该步骤成功；"cancelled" 表示调用方取消，其余取值都表示失败。
type CheckOutcome struct {
	Category   string
	Message    string
	HTTPStatus int
	// Exchanged 表示本步骤是否真的向 OAuth 服务交换了令牌（可能已轮换刷新凭据）。
	Exchanged bool
	// Cached 表示用量查询在缓存周期内复用了已有快照，没有出网。
	Cached bool
}

// Failed 判断结论是否为失败；成功和调用方取消都不算失败。
func (o CheckOutcome) Failed() bool {
	return o.Category != "" && o.Category != "cancelled"
}

// DiagnoseRefresh 检查刷新链路。force 为 false 时只在令牌临近到期时交换；
// force 为 true 时即使访问令牌仍有效也执行一次交换，会轮换刷新凭据。
// 交换使用管理器自己的 OAuth 客户端（专用 OAuth 代理或环境代理）。
func (m *Manager) DiagnoseRefresh(ctx context.Context, id string, force bool) CheckOutcome {
	m.mu.RLock()
	item, ok := m.accounts[id]
	hasRefreshToken := ok && item.RefreshToken != ""
	m.mu.RUnlock()
	if !ok {
		return CheckOutcome{Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
	}
	if force && !hasRefreshToken {
		return CheckOutcome{Category: "no_refresh_token", Message: "账户没有刷新凭据，无法强制刷新；访问令牌到期后需要重新授权"}
	}
	exchanged, err := m.RefreshCredentials(ctx, id, nil, force)
	if err != nil {
		outcome := DescribeCredentialError(err)
		outcome.Exchanged = exchanged
		return outcome
	}
	switch {
	case exchanged:
		return CheckOutcome{Message: "已通过刷新凭据换得新的访问令牌并保存", Exchanged: true}
	case !hasRefreshToken:
		return CheckOutcome{Message: "访问令牌仍有效；账户没有刷新凭据，到期后需要重新授权"}
	default:
		return CheckOutcome{Message: "访问令牌仍有效，本次未交换令牌；可选择强制刷新以验证刷新凭据"}
	}
}

// DiagnoseUsage 强制查询一次用量并按现有规则保存快照；失败保留旧快照的成功数据。
func (m *Manager) DiagnoseUsage(ctx context.Context, id string) CheckOutcome {
	m.mu.RLock()
	item := m.accounts[id]
	m.mu.RUnlock()
	if item == nil {
		return CheckOutcome{Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
	}
	return m.refreshAccountUsage(ctx, item, true)
}

// CredentialVersion 返回账户当前凭据世代；诊断前后比较即可知道期间是否发生令牌轮换或替换。
func (m *Manager) CredentialVersion(id string) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.accounts[id]
	if !ok {
		return 0, false
	}
	return item.credentialVersion, true
}

// OAuthEndpoint 与 UsageEndpoint 只用于判断管理请求的代理来源，不含凭据。
func (m *Manager) OAuthEndpoint() string { return m.oauthBaseURL() + "/oauth/token" }

func (m *Manager) UsageEndpoint() string { return m.usageURL() }

// DescribeCredentialError 把获取或刷新访问令牌的错误归类为固定文案。
// 只有明确的 invalid_grant 或已标记需要重新授权才归为授权问题；网络、超时和未知响应不下此结论。
func DescribeCredentialError(err error) CheckOutcome {
	if err == nil {
		return CheckOutcome{}
	}
	var exchangeErr *oauthExchangeFailure
	hasExchange := errors.As(err, &exchangeErr)
	switch {
	case errors.Is(err, context.Canceled):
		return CheckOutcome{Category: "cancelled", Message: "刷新已取消，未记录为刷新失败"}
	case errors.Is(err, ErrAccountNotFound):
		return CheckOutcome{Category: "not_found", Message: "ChatGPT OAuth 账户不存在"}
	case errors.Is(err, ErrReauthorizationRequired):
		return CheckOutcome{Category: "authorization", Message: "账户刷新凭据已失效或缺失，需要重新授权"}
	case errors.Is(err, errInvalidGrant):
		return CheckOutcome{Category: "authorization", Message: "刷新凭据已被 ChatGPT 拒绝（invalid_grant），需要重新授权", HTTPStatus: exchangeErr.httpStatusOrZero()}
	case errors.Is(err, ErrCredentialsReplaced):
		return CheckOutcome{Category: "conflict", Message: "刷新期间账户凭据已被导入更新或重新授权替换，请重新诊断"}
	case errors.Is(err, errRefreshPersist):
		return CheckOutcome{Category: "storage", Message: "已换得新令牌但写入账户文件失败；新令牌只保存在内存中，请检查 data 目录权限"}
	case errors.Is(err, context.DeadlineExceeded):
		return CheckOutcome{Category: "timeout", Message: "连接 OAuth 服务超时，可能是网络或代理问题"}
	case !hasExchange:
		return CheckOutcome{Category: "unknown", Message: "获取访问令牌失败"}
	}
	switch exchangeErr.category {
	case "transport":
		return CheckOutcome{Category: "network", Message: "无法连接 OAuth 服务，可能是网络或代理问题"}
	case "missing_tokens", "invalid_response", "response_read":
		return CheckOutcome{Category: "invalid_response", Message: "OAuth 服务返回了无法识别的响应", HTTPStatus: exchangeErr.httpStatus}
	default:
		message := "OAuth 服务拒绝了刷新请求"
		if exchangeErr.httpStatus > 0 {
			message += fmt.Sprintf("（HTTP %d）", exchangeErr.httpStatus)
		}
		return CheckOutcome{Category: "upstream", Message: message, HTTPStatus: exchangeErr.httpStatus}
	}
}

func (e *oauthExchangeFailure) httpStatusOrZero() int {
	if e == nil {
		return 0
	}
	return e.httpStatus
}

// usageFailureOutcome 归类用量查询失败。用量接口的鉴权拒绝不直接证明刷新凭据失效。
func usageFailureOutcome(err error) CheckOutcome {
	var statusErr *usageHTTPError
	var urlErr *url.Error
	switch {
	case errors.As(err, &statusErr):
		message := fmt.Sprintf("ChatGPT 用量服务返回 HTTP %d", statusErr.status)
		if statusErr.status == http.StatusUnauthorized || statusErr.status == http.StatusForbidden {
			message = fmt.Sprintf("ChatGPT 拒绝了用量查询（HTTP %d）；这不单独证明刷新凭据失效", statusErr.status)
		}
		return CheckOutcome{Category: "query_failed", Message: message, HTTPStatus: statusErr.status}
	case errors.Is(err, context.DeadlineExceeded):
		return CheckOutcome{Category: "timeout", Message: "ChatGPT 用量查询超时，可能是网络或代理问题"}
	case errors.As(err, &urlErr):
		return CheckOutcome{Category: "network", Message: "无法连接 ChatGPT 用量服务，可能是网络或代理问题"}
	default:
		return CheckOutcome{Category: "invalid_response", Message: "ChatGPT 用量服务返回了无法识别的响应"}
	}
}

// usageSuccessMessage 只汇总窗口名称；额度数值由账户视图展示。
func usageSuccessMessage(usage UsageSnapshot) string {
	var exhausted []string
	for _, window := range usage.Windows {
		if window.RemainingPercent <= 0 {
			exhausted = append(exhausted, window.Name)
		}
	}
	if len(exhausted) == 0 {
		return fmt.Sprintf("用量查询成功，返回 %d 个额度窗口", len(usage.Windows))
	}
	return "用量查询成功；以下窗口额度已用尽：" + strings.Join(exhausted, "、")
}
