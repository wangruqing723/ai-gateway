package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"ai-gateway/internal/metrics"
	"ai-gateway/internal/proxy"
)

// finalizeErrorProvenance 汇总最后一次尝试，成功请求不继承历史失败来源。
// Kind/Reason 为产生错误的边界写入的枚举，不解析错误自由文本。
func finalizeErrorProvenance(record *metrics.RequestLog, request *http.Request) {
	for i := range record.AttemptDetails {
		d := &record.AttemptDetails[i]
		if d.Outcome == "success" {
			continue
		}
		if d.ErrorSource == "" {
			switch {
			case d.Reason == "client_disconnected":
				d.ErrorSource = "client"
				d.ErrorCategory = "client_disconnected"
			case d.Kind != "request":
				d.ErrorSource = "gateway"
				d.ErrorCategory = d.Reason
				if d.Kind == "build_skip" && d.Reason == "conversion_error" {
					d.ErrorCategory = "gateway_conversion_unsupported"
				}
			case d.UpstreamStatus >= 400:
				d.ErrorSource = "upstream"
				d.ErrorCategory = "upstream_http_error"
			default:
				d.ErrorSource = "unknown"
				d.ErrorCategory = d.Reason
			}
		}
		if d.ErrorBodyState == "" {
			d.ErrorBodyState = "not_captured"
		}
	}
	if record.Error == "" && record.Status < 400 {
		record.ErrorSource = ""
		record.ErrorCategory = ""
		return
	}
	if record.ErrorSource == "" {
		record.ErrorSource = "unknown"
	}
	if record.ErrorCategory == "" {
		record.ErrorCategory = "unknown"
	}
	if record.ErrorSource == "unknown" && len(record.AttemptDetails) > 0 {
		for i := len(record.AttemptDetails) - 1; i >= 0; i-- {
			d := record.AttemptDetails[i]
			if d.ErrorSource == "" {
				continue
			}
			// 优先采用带诊断证据的真实请求失败；末尾准入跳过不覆盖它。
			if d.Kind == "request" {
				record.ErrorSource = d.ErrorSource
				record.ErrorCategory = d.ErrorCategory
				break
			}
		}
	}
	if request != nil {
		if errors.Is(request.Context().Err(), context.Canceled) {
			record.ErrorSource = "client"
			record.ErrorCategory = "client_disconnected"
		}
		_, token, _ := strings.Cut(request.Header.Get("Authorization"), " ")
		record.Error = proxy.SanitizeDiagnostic(record.Error, request.Header.Get("x-api-key"), token)
	}
}
