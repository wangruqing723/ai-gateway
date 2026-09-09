package vision

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"ai-gateway/internal/queue"
)

// Result 是一次请求内图片识别的独立统计结果。
// 图片识别失败属于软降级，不参与网关请求本身的成功/失败判定。
type Result struct {
	Total        int
	Cached       int
	Recognized   int
	Failed       int
	FirstFailure *Failure
}

// Failure 是一次请求内首个图片识别失败的摘要。
type Failure struct {
	Category string
	Message  string
}

const maxFailureMessageRunes = 200

// ClassifyFailure 将视觉识别错误归入稳定的大类，供指标和前端展示使用。
func ClassifyFailure(err error) string {
	if err == nil {
		return "other"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout_or_cancel"
	}
	if errors.Is(err, queue.ErrQueueTimeout) || errors.Is(err, queue.ErrProviderRemoved) {
		return "queue"
	}

	message := err.Error()
	if strings.Contains(message, "图片块缺少 source") || strings.Contains(message, "不支持的图片 source 类型") {
		return "image_format"
	}
	if status, ok := visionHTTPStatus(message); ok {
		if status < 200 || status >= 300 {
			return "upstream_http"
		}
		// 「视觉 API 响应异常 (HTTP 200)」表示响应可解析但没有有效文本，
		// 仍属于响应解析问题，而不是上游 HTTP 状态异常。
		return "response_parse"
	}
	if strings.Contains(message, "视觉 API 响应异常") ||
		strings.Contains(message, "解析视觉响应失败") ||
		strings.Contains(message, "读取视觉响应失败") ||
		strings.Contains(message, "视觉 API 响应超过大小限制") {
		return "response_parse"
	}
	if strings.Contains(message, "视觉 API 网络请求失败") ||
		strings.Contains(message, "解析视觉 provider 代理失败") {
		return "network"
	}
	return "other"
}

func visionHTTPStatus(message string) (int, bool) {
	const marker = "HTTP "
	index := strings.Index(message, marker)
	if index < 0 {
		return 0, false
	}
	start := index + len(marker)
	end := start
	for end < len(message) && message[end] >= '0' && message[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	status, err := strconv.Atoi(message[start:end])
	if err != nil {
		return 0, false
	}
	return status, true
}

func truncateFailureMessage(message string) string {
	runes := []rune(message)
	if len(runes) <= maxFailureMessageRunes {
		return message
	}
	return string(runes[:maxFailureMessageRunes])
}
