package converter

import (
	"strings"
	"testing"
)

func TestUpstreamErrorPreservesLegacyMessagePriority(t *testing.T) {
	for _, data := range []map[string]any{
		{"type": "error", "error": map[string]any{"code": "failed"}, "message": "top message"},
		{"type": "error", "response": map[string]any{"error": map[string]any{"message": "response message"}}, "error": map[string]any{"message": "nested message"}},
	} {
		e := ParseUpstreamError(data, "openai-responses")
		if e == nil || e.Message != responsesErrorMessage(data) {
			t.Fatalf("错误消息兼容失败: %+v", e)
		}
	}
}

func TestUpstreamErrorPreservesUnknownErrorFields(t *testing.T) {
	e := ParseUpstreamError(map[string]any{"type": "response.failed", "response": map[string]any{"output": []any{"private"}, "error": map[string]any{"message": "failed", "details": map[string]any{"reason": "custom"}}}}, "openai-responses")
	if !strings.Contains(string(e.Raw), "custom") || strings.Contains(string(e.Raw), "private") {
		t.Fatalf("错误证据=%s", e.Raw)
	}
}
