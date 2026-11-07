// Package providermodels 查询 ChatGPT OAuth 的真实远端模型目录。
// 本包只解析目录，不进行模型生成，也不把目录查询成功解释为模型调用健康。
package providermodels

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultClientVersion = "0.167.0"
	MaxResponseBytes     = 8 << 20
	MaxModels            = 2000
	MaxModelNameBytes    = 256
	RequestTimeout       = 10 * time.Second
)

// Query 的秘密只保留在单次查询作用域，禁止序列化或记录整个对象。
type Query struct {
	BaseURL, ClientVersion, Token, AccountID, UserAgent string
	ExtraHeaders                                        map[string]string
}

// Result 仅包含目录名称与查询时间，不带上游原文和凭据。
type Result struct {
	Models    []string `json:"models"`
	QueriedAt string   `json:"queriedAt"`
}

// QueryError 用固定的错误类别与文案表达目录失败，不包装上游正文或 URL 错误。
type QueryError struct {
	Code       string
	Message    string
	HTTPStatus int
	RetryAfter time.Duration
}

func (e *QueryError) Error() string { return e.Message }

// Fetch 发送一次目录请求，不自动强制刷新或跨账号重试。
// 身份头在额外头之后写入，额外设置不得覆盖绑定账户或泄露 cookie。
func Fetch(parent context.Context, client *http.Client, query Query) (*Result, *QueryError) {
	if query.Token == "" || query.AccountID == "" {
		return nil, queryError("credentials_required", "模型目录查询缺少账户凭据", 0)
	}
	base, err := url.Parse(query.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, queryError("invalid_configuration", "模型目录地址无效", 0)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/models"
	base.RawPath = ""
	version := query.ClientVersion
	if version == "" {
		version = DefaultClientVersion
	}
	values := base.Query()
	values.Set("client_version", version)
	base.RawQuery = values.Encode()
	ctx, cancel := context.WithTimeout(parent, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, queryError("invalid_configuration", "模型目录请求无法构建", 0)
	}
	for key, value := range query.ExtraHeaders {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "authorization", "x-api-key", "chatgpt-account-id", "cookie", "host", "content-type", "user-agent":
			continue
		}
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+query.Token)
	req.Header.Set("ChatGPT-Account-Id", query.AccountID)
	if query.UserAgent != "" {
		req.Header.Set("User-Agent", query.UserAgent)
	}
	if client == nil {
		client = http.DefaultClient
	}
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := isolated.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return nil, queryError("query_cancelled", "模型目录查询已取消", 0)
		}
		if ctx.Err() != nil {
			return nil, queryError("upstream_timeout", "模型目录查询超时", 0)
		}
		return nil, queryError("upstream_unreachable", "无法连接模型目录服务", 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := queryError("upstream_error", "模型目录服务拒绝了查询", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			failure = queryError("credential_rejected", "账户无权查询该模型目录，不能据此判断刷新凭据失效", resp.StatusCode)
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			failure = queryError("unsupported_endpoint", "上游暂不支持模型目录，可手动填写模型名", resp.StatusCode)
		case http.StatusTooManyRequests:
			failure = queryError("rate_limited", "模型目录查询受到限流，请稍后刷新", resp.StatusCode)
			failure.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return nil, failure
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		if parent.Err() != nil {
			return nil, queryError("query_cancelled", "模型目录查询已取消", resp.StatusCode)
		}
		if ctx.Err() != nil {
			return nil, queryError("upstream_timeout", "模型目录查询超时", resp.StatusCode)
		}
		return nil, queryError("invalid_response", "模型目录响应读取失败", resp.StatusCode)
	}
	if len(body) > MaxResponseBytes {
		return nil, queryError("response_too_large", "模型目录响应超过大小上限", resp.StatusCode)
	}
	models, failure := Parse(body)
	if failure != nil {
		failure.HTTPStatus = resp.StatusCode
		return nil, failure
	}
	return &Result{Models: models, QueriedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// Parse 仅取可见 slug，未知附加字段可忽略；非法核心结构不会变成空目录。
func Parse(body []byte) ([]string, *QueryError) {
	invalid := func() ([]string, *QueryError) {
		return nil, queryError("invalid_response", "上游返回了无法识别的模型目录", 0)
	}
	if len(body) > MaxResponseBytes || !utf8.Valid(body) {
		return invalid()
	}
	var envelope struct {
		Models *[]struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil || envelope.Models == nil {
		return invalid()
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(*envelope.Models) > MaxModels {
		return invalid()
	}
	models := make([]string, 0, len(*envelope.Models))
	seen := make(map[string]bool)
	for _, entry := range *envelope.Models {
		name := strings.TrimSpace(entry.Slug)
		if name == "" || len(name) > MaxModelNameBytes || containsControl(entry.Slug) {
			return invalid()
		}
		if entry.Visibility != "list" {
			continue
		}
		if !seen[name] {
			models = append(models, name)
			seen[name] = true
		}
	}
	return models, nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func queryError(code, message string, status int) *QueryError {
	return &QueryError{Code: code, Message: message, HTTPStatus: status}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	var seconds json.Number = json.Number(value)
	if n, err := seconds.Int64(); err == nil && n >= 0 && n <= int64((365*24*time.Hour)/time.Second) {
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
