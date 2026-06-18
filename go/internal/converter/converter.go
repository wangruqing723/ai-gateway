// Package converter 对齐 Node 版 lib/converter.js：三种 API 格式（Anthropic / OpenAI Chat /
// OpenAI Responses）的请求/响应/流式双向转换。
//
// 内部统一用 Anthropic-like 结构表示；为贴合 Node 的动态语义，JSON 体一律用 map[string]any / []any，
// 而非强类型结构体，以最大程度保证与原实现行为等价（这是迁移的最大风险点，需对拍测试）。
package converter

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Internal 内部统一格式（Anthropic-like）
type Internal struct {
	Model     string
	Messages  []any          // 每个元素为 map[string]any: {role, content}
	System    any            // string 或 nil
	Stream    bool
	MaxTokens int
	Tools     any            // 数组或 nil
	Extra     map[string]any // 原始请求体，用于透传 temperature 等
}

// DetectClientFormat 按端点路径识别客户端格式（对齐 Node 版）。
func DetectClientFormat(path string) string {
	p := strings.Split(path, "?")[0]
	switch {
	case strings.HasSuffix(p, "/v1/messages"):
		return "anthropic"
	case strings.HasSuffix(p, "/v1/chat/completions"):
		return "openai-chat"
	case strings.HasSuffix(p, "/v1/responses"):
		return "openai-responses"
	}
	return ""
}

// IsPassthrough 判断 provider 和 client 格式是否可直接透传（不需逐行转换）。
// 供 proxy 和 stream 模块共用，消除判定逻辑分歧。
func IsPassthrough(providerFormat, clientFormat string) bool {
	return providerFormat == clientFormat ||
		(providerFormat == "openai" && clientFormat == "openai-chat")
}

// ── 客户端请求 → 内部格式 ──────────────────────

// FromAnthropic Anthropic 请求 → 内部格式（几乎原样保留）。
func FromAnthropic(body map[string]any) *Internal {
	return &Internal{
		Model:     getString(body, "model"),
		Messages:  getSlice(body, "messages"),
		System:    body["system"],
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_tokens", 4096),
		Tools:     body["tools"],
		Extra:     body,
	}
}

// FromOpenAIChat OpenAI Chat 请求 → 内部格式。
func FromOpenAIChat(body map[string]any) *Internal {
	var messages []any
	var system any

	for _, m := range getSlice(body, "messages") {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role := getString(msg, "role")
		if role == "system" {
			system = systemTextFromContent(msg["content"])
			continue
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": normalizeOpenAIContent(msg["content"]),
		})
	}

	return &Internal{
		Model:     getString(body, "model"),
		Messages:  messages,
		System:    system,
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_tokens", 4096),
		Tools:     body["tools"],
		Extra:     body,
	}
}

// FromOpenAIResponses OpenAI Responses 请求 → 内部格式。
func FromOpenAIResponses(body map[string]any) *Internal {
	var input []any
	switch v := body["input"].(type) {
	case []any:
		input = v
	case string:
		input = []any{map[string]any{"role": "user", "content": v}}
	}

	var messages []any
	for _, it := range input {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "message" || item["role"] != nil {
			messages = append(messages, map[string]any{
				"role":    item["role"],
				"content": normalizeResponsesContent(item["content"]),
			})
		}
	}

	var system any
	if v, ok := body["instructions"]; ok {
		system = v
	}

	return &Internal{
		Model:     getString(body, "model"),
		Messages:  messages,
		System:    system,
		Stream:    getBool(body, "stream"),
		MaxTokens: getIntDefault(body, "max_output_tokens", 4096),
		Tools:     body["tools"],
		Extra:     body,
	}
}

// ── 内部格式 → 上游 Provider 请求体 ───────────

// ToAnthropicBody 内部格式 → Anthropic 请求体。
func ToAnthropicBody(in *Internal, targetModel string) map[string]any {
	body := map[string]any{
		"model":      targetModel,
		"messages":   in.Messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.System != nil && in.System != "" {
		body["system"] = in.System
	}
	if in.Tools != nil {
		body["tools"] = in.Tools
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "stop_sequences", "metadata"} {
		if v, ok := in.Extra[k]; ok {
			body[k] = v
		}
	}
	return body
}

// ToOpenAIChatBody 内部格式 → OpenAI Chat 请求体。
func ToOpenAIChatBody(in *Internal, targetModel string) map[string]any {
	var messages []any
	if s, ok := in.System.(string); ok && s != "" {
		messages = append(messages, map[string]any{"role": "system", "content": s})
	}
	for _, m := range in.Messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		messages = append(messages, map[string]any{
			"role":    msg["role"],
			"content": toOpenAIContent(msg["content"]),
		})
	}

	body := map[string]any{
		"model":      targetModel,
		"messages":   messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.Tools != nil {
		body["tools"] = in.Tools
	}
	for _, k := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty"} {
		if v, ok := in.Extra[k]; ok {
			body[k] = v
		}
	}
	return body
}

// ── 内容块归一化 ──────────────────────────────

func normalizeOpenAIContent(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	arr, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "text":
			// 兼容客户端发 text 为 JSON 数字（float64）的情况，统一转为字符串
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		case "image_url":
			out = append(out, openAIImageToAnthropic(block["image_url"]))
		default:
			out = append(out, block)
		}
	}
	return out
}

func normalizeResponsesContent(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	arr, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("%v", block["text"])})
		case "input_image":
			out = append(out, responsesImageToAnthropic(block))
		default:
			out = append(out, block)
		}
	}
	return out
}

func openAIImageToAnthropic(imageURL any) map[string]any {
	var url string
	switch v := imageURL.(type) {
	case string:
		url = v
	case map[string]any:
		url = getString(v, "url")
	}
	// data:<media_type>;base64,<data>
	if strings.HasPrefix(url, "data:") {
		if i := strings.Index(url, ";base64,"); i > 5 {
			mediaType := url[5:i]
			data := url[i+len(";base64,"):]
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

func responsesImageToAnthropic(block map[string]any) map[string]any {
	if iu, ok := block["image_url"]; ok && iu != nil {
		return openAIImageToAnthropic(iu)
	}
	if u, ok := block["url"]; ok && u != nil {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
	}
	return block
}

func toOpenAIContent(content any) any {
	arr, ok := content.([]any)
	if !ok {
		return content
	}
	// 全是文本块则合并为字符串（对齐 Node）
	allText := true
	for _, b := range arr {
		if block, ok := b.(map[string]any); !ok || block["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		parts := make([]string, 0, len(arr))
		for _, b := range arr {
			parts = append(parts, getString(b.(map[string]any), "text"))
		}
		return strings.Join(parts, "\n")
	}
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": block["text"]})
		case "image":
			src, _ := block["source"].(map[string]any)
			var url string
			if getString(src, "type") == "base64" {
				url = "data:" + getString(src, "media_type") + ";base64," + getString(src, "data")
			} else {
				url = getString(src, "url")
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			out = append(out, block)
		}
	}
	return out
}

// extractText 提取 Anthropic content 数组里的纯文本拼接。
func extractText(content any) string {
	arr, ok := content.([]any)
	if !ok {
		if content == nil {
			return ""
		}
		return getStringValue(content)
	}
	var sb strings.Builder
	for _, b := range arr {
		if block, ok := b.(map[string]any); ok && block["type"] == "text" {
			sb.WriteString(getString(block, "text"))
		}
	}
	return sb.String()
}

// systemTextFromContent 提取 OpenAI system 消息文本（string 或数组首块）。
func systemTextFromContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok && len(arr) > 0 {
		if block, ok := arr[0].(map[string]any); ok {
			return getString(block, "text")
		}
	}
	return ""
}

// shortID 生成 8 位 hex，对应 Node 的 randomUUID().slice(0,8)。
func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── map 取值辅助 ──────────────────────────────

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func getStringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func getBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// getIntDefault JSON 数字默认解析为 float64，做兼容转换。
func getIntDefault(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func getSlice(m map[string]any, key string) []any {
	s, _ := m[key].([]any)
	return s
}
