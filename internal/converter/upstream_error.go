package converter

import "encoding/json"

// UpstreamError 保留上游明确失败的最小证据，不包含成功输出或工具参数。
type UpstreamError struct {
	EventType string
	Type      string
	Code      string
	Message   string
	Raw       []byte
}

func (e *UpstreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "上游返回错误"
}

// ParseUpstreamError 只识别受支持协议的显式失败，不从自由文本猜测来源。
func ParseUpstreamError(data map[string]any, format string) *UpstreamError {
	event, _ := data["type"].(string)
	var obj map[string]any
	if format == "openai-responses" {
		response, _ := data["response"].(map[string]any)
		obj, _ = response["error"].(map[string]any)
	}
	if obj == nil {
		obj, _ = data["error"].(map[string]any)
	}
	if obj == nil && event != "error" && event != "response.failed" {
		return nil
	}
	flat := obj == nil
	if flat {
		obj = data
	}
	str := func(key string) string { v, _ := obj[key].(string); return v }
	e := &UpstreamError{EventType: event, Type: str("type"), Code: str("code"), Message: str("message")}
	if flat {
		e.Type = ""
	}
	if format == "openai-responses" {
		e.Message = responsesErrorMessage(data)
	}
	if e.Message == "" {
		e.Message = "上游返回错误"
		if format == "openai-responses" {
			e.Message = "Responses 上游流式响应失败"
		}
	}
	// 失败终态可能附带巨大 output；只提取错误对象与事件身份。
	errorObject := make(map[string]any, len(obj))
	for key, value := range obj {
		if flat && (key == "response" || key == "output") {
			continue
		}
		errorObject[key] = value
	}
	e.Raw, _ = json.Marshal(map[string]any{"type": event, "error": errorObject})
	return e
}
