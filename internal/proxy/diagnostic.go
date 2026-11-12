package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"ai-gateway/internal/converter"
)

// ErrorDiagnostic 仅用于观测；不参与重试、熔断或客户端响应生成。
type ErrorDiagnostic struct {
	Source    string
	Category  string
	Type      string
	Code      string
	Message   string
	Body      string
	BodyState string
	Truncated bool
}

var credentialText = regexp.MustCompile(`(?i)(bearer\s+)[^\s"'<>\\,;]+|\bsk-[A-Za-z0-9_-]+`)

// SanitizeDiagnostic 统一处理错误消息和正文中的已知凭据及可识别认证文本。
func SanitizeDiagnostic(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			encoded, _ := json.Marshal(secret)
			if len(encoded) > 2 {
				value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "[REDACTED]")
			}
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = credentialText.ReplaceAllString(value, "[REDACTED]")
	return strings.ToValidUTF8(value, "")
}

func diagnosticSecrets(opts *Options) []string {
	secrets := []string{opts.OAuthAccessToken}
	if opts.Provider != nil {
		secrets = append(secrets, opts.Provider.APIKey)
	}
	if opts.ClientReq != nil {
		secrets = append(secrets, opts.ClientReq.Header.Get("x-api-key"))
		auth := opts.ClientReq.Header.Get("Authorization")
		if _, token, ok := strings.Cut(auth, " "); ok {
			secrets = append(secrets, token)
		}
	}
	return secrets
}

func reportDiagnostic(opts *Options, d ErrorDiagnostic) {
	secrets := diagnosticSecrets(opts)
	clean := func(s string) string { return SanitizeDiagnostic(s, secrets...) }
	d.Type = truncateUTF8(clean(d.Type), 512)
	d.Code = truncateUTF8(clean(d.Code), 512)
	d.Message = truncateUTF8(clean(d.Message), maxObservedErrorBodyBytes)
	d.Body = clean(d.Body)
	if len(d.Body) > maxObservedErrorBodyBytes {
		d.Truncated = true
		d.Body = truncateUTF8(d.Body, maxObservedErrorBodyBytes)
	}
	if d.BodyState == "" {
		d.BodyState = "not_captured"
		if d.Body != "" {
			d.BodyState = "available"
		}
	}
	if d.Truncated {
		d.BodyState = "truncated"
	}
	if opts.OnDiagnostic != nil {
		opts.OnDiagnostic(d)
	}
}

func reportUpstreamEvent(opts *Options, e *converter.UpstreamError) {
	if e == nil {
		return
	}
	reportDiagnostic(opts, ErrorDiagnostic{Source: "upstream", Category: "upstream_error", Type: func() string {
		if e.Type != "" {
			return e.Type
		}
		return e.EventType
	}(), Code: e.Code, Message: e.Message, Body: sanitizeErrorBody(e.Raw)})
}

// streamErrorObserver 按完整 SSE 事件旁路观察，不改写透传字节或改变流的结束条件。
// 每个事件最多保留既有 SSE 上限；超限丢弃至下个事件边界，不缓存整条响应。
type streamErrorObserver struct {
	opts       *Options
	line       []byte
	data       []byte
	event      string
	oversized  bool
	lineLength int
	lineOnlyCR bool
}

const maxDiagnosticEventBytes = maxSSEEventBytes

func (o *streamErrorObserver) feed(chunk []byte) {
	for len(chunk) > 0 {
		idx := bytes.IndexByte(chunk, '\n')
		if idx < 0 {
			idx = len(chunk)
		}
		if len(o.line)+len(o.data)+idx > maxDiagnosticEventBytes {
			o.oversized = true
			o.line = nil
			o.data = nil
		}
		if !o.oversized {
			o.line = append(o.line, chunk[:idx]...)
		}
		if idx == len(chunk) {
			o.lineLength += idx
			o.lineOnlyCR = idx == 1 && chunk[0] == '\r'
			return
		}
		lineLength := o.lineLength + idx
		lineOnlyCR := (o.lineOnlyCR && idx == 0) || (o.lineLength == 0 && idx == 1 && chunk[0] == '\r')
		chunk = chunk[idx+1:]
		line := strings.TrimSuffix(string(o.line), "\r")
		o.line = nil
		o.lineLength = 0
		o.lineOnlyCR = false
		if o.oversized {
			if lineLength == 0 || (lineLength == 1 && lineOnlyCR) {
				o.oversized = false
				o.data = nil
				o.event = ""
			}
			continue
		}
		if line == "" {
			o.dispatch()
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			o.event = value
		case "data":
			if len(o.data) > 0 {
				o.data = append(o.data, '\n')
			}
			o.data = append(o.data, value...)
		}
	}
}

func (o *streamErrorObserver) finish() {
	if o.oversized {
		return
	}
	if len(o.line) > 0 {
		o.feed([]byte("\n"))
	}
	o.dispatch()
}

func (o *streamErrorObserver) dispatch() {
	defer func() { o.data = nil; o.event = "" }()
	if len(o.data) == 0 {
		return
	}
	format := o.opts.Provider.Format
	if format != "anthropic" && format != "openai" && format != "openai-chat" && format != "openai-responses" {
		return
	}
	var data map[string]any
	if json.Unmarshal(o.data, &data) != nil {
		return
	}
	if _, ok := data["type"]; !ok && o.event != "" {
		data["type"] = o.event
	}
	reportUpstreamEvent(o.opts, converter.ParseUpstreamError(data, format))
}

func classifyDiagnosticError(opts *Options, err error) {
	if opts.ClientReq != nil && errors.Is(opts.ClientReq.Context().Err(), context.Canceled) {
		reportDiagnostic(opts, ErrorDiagnostic{Source: "client", Category: "client_disconnected"})
		return
	}
	if err == nil || errors.Is(err, ErrAttemptAbandoned) {
		return
	}
	var explicit *converter.UpstreamError
	if errors.As(err, &explicit) {
		reportUpstreamEvent(opts, explicit)
		return
	}
	var opening *UpstreamStreamError
	if errors.As(err, &opening) {
		return
	}
	source, category := "transport", "transport_error"
	if errors.Is(err, ErrConversion) {
		source, category = "unknown", "response_conversion_error"
	}
	reportDiagnostic(opts, ErrorDiagnostic{Source: source, Category: category})
}

func diagnosticLog(original LogFunc, opts *Options) LogFunc {
	return func(format string, args ...any) {
		if original != nil {
			original("%s", SanitizeDiagnostic(fmt.Sprintf(format, args...), diagnosticSecrets(opts)...))
		}
	}
}
