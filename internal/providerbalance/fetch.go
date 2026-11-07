// Package providerbalance 查询明确配置的上游余额，不维护缓存或修改运行状态。
package providerbalance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Query 的 Source 必须已解析为 account 或 apiKey；BaseURL 是管理站点根前缀。
type Query struct{ Type, Source, BaseURL, Token, UserID, KeyID, QuotaPerUnit, UserAgent string }

type Snapshot struct {
	QueriedAt string   `json:"queriedAt"`
	Items     []Item   `json:"items"`
	Warnings  []string `json:"warnings"`
}

// Clone 为缓存和调用方提供完全隔离的快照，不共享 slice 或金额指针。
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	result := *s
	result.Warnings = append([]string{}, s.Warnings...)
	result.Items = make([]Item, len(s.Items))
	for i, item := range s.Items {
		result.Items[i] = item
		dst := &result.Items[i]
		fields := []**string{&dst.Remaining, &dst.Used, &dst.Limit, &dst.KeyID, &dst.RawRemaining, &dst.RawUsed, &dst.RawUnit, &dst.QuotaPerUnit, &dst.ConversionSource}
		for j, src := range []*string{item.Remaining, item.Used, item.Limit, item.KeyID, item.RawRemaining, item.RawUsed, item.RawUnit, item.QuotaPerUnit, item.ConversionSource} {
			if src != nil {
				*fields[j] = ptr(*src)
			}
		}
		dst.Windows = make([]Window, len(item.Windows))
		for j, window := range item.Windows {
			dst.Windows[j] = window
			w := &dst.Windows[j]
			fields := []**string{&w.Remaining, &w.Used, &w.Limit, &w.ResetAt, &w.ExpiresAt}
			for k, src := range []*string{window.Remaining, window.Used, window.Limit, window.ResetAt, window.ExpiresAt} {
				if src != nil {
					*fields[k] = ptr(*src)
				}
			}
		}
	}
	return &result
}

type Item struct {
	Kind             string   `json:"kind"`
	Source           string   `json:"source"`
	Remaining        *string  `json:"remaining"`
	Used             *string  `json:"used"`
	Limit            *string  `json:"limit"`
	Unit             string   `json:"unit"`
	Unlimited        bool     `json:"unlimited"`
	KeyID            *string  `json:"keyId"`
	Windows          []Window `json:"windows"`
	RawRemaining     *string  `json:"rawRemaining"`
	RawUsed          *string  `json:"rawUsed"`
	RawUnit          *string  `json:"rawUnit"`
	QuotaPerUnit     *string  `json:"quotaPerUnit"`
	ConversionSource *string  `json:"conversionSource"`
}

type Window struct {
	Name      string  `json:"name"`
	Limit     *string `json:"limit"`
	Used      *string `json:"used"`
	Remaining *string `json:"remaining"`
	Unit      string  `json:"unit"`
	ResetAt   *string `json:"resetAt"`
	ExpiresAt *string `json:"expiresAt"`
}

// QueryError 只含固定安全文案，不保留上游正文或出网错误。
type QueryError struct {
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	HTTPStatus int           `json:"httpStatus"`
	RetryAfter time.Duration `json:"-"`
}

func (e *QueryError) Error() string { return e.Message }
func failure(code string, status int) *QueryError {
	messages := map[string]string{
		"credentials_required": "余额查询需要凭据", "credential_rejected": "余额查询凭据被拒绝",
		"permission_denied": "没有余额查询权限", "unsupported_endpoint": "上游不支持余额查询",
		"upstream_timeout": "余额查询超时", "upstream_unreachable": "无法连接余额查询服务",
		"invalid_response": "上游余额响应无效", "response_too_large": "上游余额响应超过限制",
		"rate_limited": "余额查询被限流", "query_cancelled": "余额查询已取消",
	}
	return &QueryError{Code: code, Message: messages[code], HTTPStatus: status}
}
func invalid() *QueryError { return failure("invalid_response", http.StatusBadGateway) }
func contextError(ctx context.Context) *QueryError {
	if errors.Is(ctx.Err(), context.Canceled) {
		return failure("query_cancelled", 499)
	}
	return failure("upstream_timeout", http.StatusGatewayTimeout)
}

// Fetch 在共享的十五秒预算内读取余额和必要的单位信息，不跟随任何重定向。
func Fetch(ctx context.Context, client *http.Client, query Query) (*Snapshot, *QueryError) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if query.Token == "" {
		return nil, failure("credentials_required", 400)
	}
	if strings.IndexFunc(query.Token, unicode.IsControl) >= 0 || len(query.Token) > 16384 || strings.IndexFunc(query.UserAgent, unicode.IsControl) >= 0 {
		return nil, invalid()
	}
	if query.UserID != "" {
		id, e := strconv.ParseInt(query.UserID, 10, 64)
		if e != nil || id <= 0 || strings.Trim(query.UserID, "0123456789") != "" {
			return nil, invalid()
		}
	}
	u, err := url.Parse(query.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (query.Source != "account" && query.Source != "apiKey") {
		return nil, invalid()
	}
	if query.QuotaPerUnit != "" {
		if len(query.QuotaPerUnit) > 64 || !configDecimalPattern.MatchString(query.QuotaPerUnit) {
			return nil, invalid()
		}
		// 配置允许前导零；只在配置边界规范化，保持上游 JSON 数字语法严格。
		normalized := strings.TrimLeft(query.QuotaPerUnit, "0")
		if normalized == "" || strings.HasPrefix(normalized, ".") {
			normalized = "0" + normalized
		}
		n, ok := decimal(normalized)
		if !ok || !positive(n) {
			return nil, invalid()
		}
		query.QuotaPerUnit = n
	}
	if query.KeyID != "" && (query.KeyID == query.Token || !keyIDPattern.MatchString(query.KeyID)) {
		return nil, invalid()
	}
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	copied.Jar = nil
	copied.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	var path string
	switch query.Type + "/" + query.Source {
	case "newapi/account":
		path = "/api/user/self"
	case "newapi/apiKey":
		path = "/api/usage/token"
	case "sub2api/account":
		path = "/api/v1/auth/me"
	case "sub2api/apiKey":
		path = "/v1/usage"
	default:
		return nil, failure("unsupported_endpoint", 400)
	}
	body, qerr := request(ctx, &copied, query, path, true)
	if qerr != nil {
		return nil, qerr
	}
	var items []Item
	if query.Type == "newapi" {
		items, qerr = parseNewAPI(body, query)
	} else {
		items, qerr = parseSub2API(body, query)
	}
	if qerr != nil {
		return nil, qerr
	}
	snapshot := &Snapshot{QueriedAt: time.Now().UTC().Format(time.RFC3339), Items: items, Warnings: []string{}}
	if query.Type == "newapi" {
		ratio := query.QuotaPerUnit
		source := "configuration"
		if ratio == "" {
			source = "upstream_status"
			status, e := request(ctx, &copied, query, "/api/status", false)
			if e == nil && successfulNewAPI(status) {
				if data, ok := object(status["data"]); ok {
					if n, ok := decimal(data["quota_per_unit"]); ok && positive(n) {
						ratio = n
					}
				}
			}
		}
		normalized, ok := decimal(ratio)
		if !ok || !positive(normalized) {
			snapshot.Warnings = append(snapshot.Warnings, "无法确认配额换算单位，保留原始 quota")
		} else {
			for i := range snapshot.Items {
				convertQuota(&snapshot.Items[i], normalized, source)
			}
		}
	}
	return snapshot, nil
}

func request(ctx context.Context, client *http.Client, q Query, path string, auth bool) (map[string]any, *QueryError) {
	endpoint := strings.TrimRight(q.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, invalid()
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+q.Token)
		if q.Type == "newapi" && q.Source == "account" && q.UserID != "" {
			req.Header.Set("New-Api-User", q.UserID)
		}
	}
	if q.UserAgent != "" {
		req.Header.Set("User-Agent", q.UserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextError(ctx)
		}
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, failure("upstream_timeout", 504)
		}
		return nil, failure("upstream_unreachable", 502)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "invalid_response"
		switch resp.StatusCode {
		case 401:
			code = "credential_rejected"
		case 403:
			code = "permission_denied"
		case 404, 405, 501:
			code = "unsupported_endpoint"
		case 429:
			code = "rate_limited"
		}
		e := failure(code, resp.StatusCode)
		e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
		return nil, e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if len(raw) > 1<<20 {
		return nil, failure("response_too_large", 502)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextError(ctx)
		}
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, failure("upstream_timeout", 504)
		}
		return nil, invalid()
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var result map[string]any
	if dec.Decode(&result) != nil || result == nil {
		return nil, invalid()
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, invalid()
	}
	return result, nil
}
func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if until, err := http.ParseTime(value); err == nil && until.After(now) {
		return until.Sub(now)
	}
	return 0
}
func object(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok && m != nil }
func ptr(s string) *string                { return &s }
func newItem(kind, source, unit string, q Query) Item {
	item := Item{Kind: kind, Source: source, Unit: unit, Windows: []Window{}}
	if q.Source == "apiKey" && q.KeyID != "" {
		item.KeyID = ptr(q.KeyID)
	}
	return item
}

// 限制尾数、指数和绝对值，避免不可信数字触发无界大整数分配。
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var configDecimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,18})?$`)
var decimalPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]{1,2})?$`)

func decimal(v any) (string, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = string(n)
	case string:
		s = n
	default:
		return "", false
	}
	if len(s) == 0 || len(s) > 128 || !decimalPattern.MatchString(s) {
		return "", false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", false
	}
	bound := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	if new(big.Rat).Abs(r).Cmp(bound) > 0 {
		return "", false
	}
	normalized := trimDecimal(r.FloatString(18))
	exact, _ := new(big.Rat).SetString(normalized)
	if exact.Cmp(r) != 0 {
		return "", false
	}
	return normalized, true
}
func trimDecimal(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		return "0"
	}
	return s
}
func positive(s string) bool { r, ok := new(big.Rat).SetString(s); return ok && r.Sign() > 0 }
func amount(m map[string]any, key string, nonnegative bool) (*string, bool) {
	v, exists := m[key]
	if !exists || v == nil {
		return nil, true
	}
	n, ok := decimal(v)
	if !ok || (nonnegative && strings.HasPrefix(n, "-")) {
		return nil, false
	}
	return ptr(n), true
}
func subtractFloorZero(limit, used string) string {
	a, _ := new(big.Rat).SetString(limit)
	b, _ := new(big.Rat).SetString(used)
	a.Sub(a, b)
	if a.Sign() < 0 {
		return "0"
	}
	return trimDecimal(a.FloatString(18))
}
func convertQuota(item *Item, ratio, source string) {
	item.RawUnit = ptr("quota")
	item.QuotaPerUnit = ptr(ratio)
	item.ConversionSource = ptr(source)
	item.Unit = "USD"
	divisor, _ := new(big.Rat).SetString(ratio)
	// 输入值至多十八位小数，换算结果最多三十六位，不能把非零配额舍入成零。
	for _, field := range []**string{&item.Remaining, &item.Used, &item.Limit} {
		if *field != nil {
			r, _ := new(big.Rat).SetString(**field)
			*field = ptr(trimDecimal(r.Quo(r, divisor).FloatString(36)))
		}
	}
}
