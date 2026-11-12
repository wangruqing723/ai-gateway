package main

import (
	"ai-gateway/internal/metrics"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorProvenanceDoesNotInheritEarlierFailureOnSuccess(t *testing.T) {
	r := metrics.RequestLog{Status: 200, AttemptDetails: []metrics.AttemptDetail{{Kind: "request", Outcome: "transferred", ErrorSource: "upstream", ErrorCategory: "upstream_http_error"}, {Kind: "request", Outcome: "success"}}}
	finalizeErrorProvenance(&r, nil)
	if r.ErrorSource != "" || r.Error != "" {
		t.Fatalf("成功请求继承失败: %+v", r)
	}
}

func TestErrorProvenanceSkipDoesNotHideRequestFailure(t *testing.T) {
	r := metrics.RequestLog{Status: 502, Error: "全部候选不可用", AttemptDetails: []metrics.AttemptDetail{{Kind: "request", Outcome: "transferred", ErrorSource: "upstream", ErrorCategory: "upstream_http_error"}, {Kind: "context_skip", Outcome: "skipped"}}}
	finalizeErrorProvenance(&r, nil)
	if r.ErrorSource != "upstream" {
		t.Fatalf("来源=%s", r.ErrorSource)
	}
}

func TestLocalErrorProvenanceAtHTTPBoundary(t *testing.T) {
	for _, body := range []string{`{`, `{"model":"missing","messages":[]}`} {
		srv := newBoundaryTestServer()
		req := httptest.NewRequest("POST", "http://127.0.0.1:7789/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		srv.handle(httptest.NewRecorder(), req)
		logs := srv.metrics.Logs(metrics.LogFilter{})
		if len(logs) != 1 || logs[0].ErrorSource != "gateway" {
			t.Fatalf("本地来源错误: %+v", logs)
		}
	}
}

func TestAggregateTerminalKeepsItsSource(t *testing.T) {
	r := metrics.RequestLog{Status: 503, Error: "无可用密钥", ErrorSource: "gateway", ErrorCategory: "gateway_admission_error", AttemptDetails: []metrics.AttemptDetail{{Kind: "request", Outcome: "transferred", ErrorSource: "upstream", Error: "上游拒绝"}}}
	finalizeErrorProvenance(&r, nil)
	if r.ErrorSource != "gateway" || r.Error != "无可用密钥" {
		t.Fatalf("聚合终态被覆盖: %+v", r)
	}
}
