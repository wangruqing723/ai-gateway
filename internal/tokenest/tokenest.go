// Package tokenest 使用零依赖的字符启发式估算请求输入 token 数。
package tokenest

import (
	"encoding/json"
	"math"
)

const (
	asciiCharsPerToken    = 4.0
	nonASCIICharsPerToken = 1.5
	perMessageOverhead    = 4
	// 图片真实占用会随尺寸变化；固定计值与 base64 体积解耦，避免把图片文本严重高估。
	// 计 0 是朝不安全方向低估：预算偏大仍可能撞上游的 context window。
	perImageTokens = 1500
)

var imageBlockTypes = map[string]struct{}{
	"image":       {},
	"image_url":   {},
	"input_image": {},
}

// Estimate 估算内部请求的输入 token 数。
//
// 参数使用纯数据签名，避免估算器反向依赖调用方的 converter 包。system 通常是
// string，messages 是 {role, content} map 的切片，content 可以是 string 或内容块数组；
// tools 整体经 json.Marshal 后按相同字符规则估算。估算值不是 tokenizer 的精确结果，
// 调用方必须另外预留安全余量。
func Estimate(system any, messages []any, tools any) int {
	total := estimateText(system)
	for _, value := range messages {
		total += perMessageOverhead
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		total += estimateMessageContent(message["content"])
	}
	if !isEmptyData(tools) {
		if encoded, err := json.Marshal(tools); err == nil {
			// 兼容 typed empty slice/map：它们不一定落到下面 isEmptyData 的
			// []any/map[string]any 分支，但空 JSON 本身不应凭空贡献 token。
			encodedText := string(encoded)
			if encodedText != "null" && encodedText != "[]" && encodedText != "{}" {
				total += estimateString(encodedText)
			}
		}
	}
	return total
}

func estimateMessageContent(content any) int {
	switch value := content.(type) {
	case string:
		return estimateString(value)
	case []any:
		var total int
		for _, blockValue := range value {
			block, ok := blockValue.(map[string]any)
			if !ok {
				continue
			}
			if blockType, ok := block["type"].(string); ok {
				if _, isImage := imageBlockTypes[blockType]; isImage {
					total += perImageTokens
					continue
				}
			}
			// 只读取消息内容块这一层的 text。不能递归遍历整个块：工具 schema
			// 里合法的 properties.type=\"image\" 不是协议图片块。
			total += estimateText(block["text"])
		}
		return total
	default:
		return 0
	}
}

func estimateText(value any) int {
	s, ok := value.(string)
	if !ok {
		return 0
	}
	return estimateString(s)
}

func estimateString(value string) int {
	if value == "" {
		return 0
	}
	var tokens float64
	for _, r := range value {
		if r < 0x80 {
			tokens += 1 / asciiCharsPerToken
		} else {
			tokens += 1 / nonASCIICharsPerToken
		}
	}
	// 向上取整，避免整数化时把预算朝不安全方向低估。
	return int(math.Ceil(tokens))
}

func isEmptyData(value any) bool {
	if value == nil {
		return true
	}
	switch v := value.(type) {
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}
