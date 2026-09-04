package tokenest

import "testing"

func message(content any) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

func TestEstimateCharacterHeuristic(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "ASCII", content: "12345678", want: 2},
		{name: "中文", content: "你好", want: 2},
		{name: "混合", content: "abcd你好", want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Estimate(nil, []any{message(tt.content)}, nil); got != tt.want+perMessageOverhead {
				t.Fatalf("Estimate() = %d, 期望 %d", got, tt.want+perMessageOverhead)
			}
		})
	}
}

func TestEstimateImagesHaveFixedCostIndependentOfBase64(t *testing.T) {
	short := []any{
		map[string]any{"type": "text", "text": "说明"},
		map[string]any{"type": "image", "source": map[string]any{"data": "short"}},
	}
	long := []any{
		map[string]any{"type": "text", "text": "说明"},
		map[string]any{"type": "image", "source": map[string]any{"data": "longlonglonglonglonglonglonglonglonglong"}},
	}
	if got, want := Estimate(nil, []any{message(short)}, nil), Estimate(nil, []any{message(long)}, nil); got != want {
		t.Fatalf("图片 base64 长度改变了估算值: short=%d long=%d", got, want)
	}
	base := Estimate(nil, []any{message([]any{map[string]any{"type": "text", "text": "说明"}})}, nil)
	withImages := Estimate(nil, []any{message([]any{
		map[string]any{"type": "text", "text": "说明"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
		map[string]any{"type": "input_image", "image_url": "x"},
	})}, nil)
	if got, want := withImages-base, 2*perImageTokens; got != want {
		t.Fatalf("图片贡献 = %d, 期望 %d", got, want)
	}
}

func TestEstimateDoesNotFindImagesInsideTools(t *testing.T) {
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"parameters": map[string]any{
				"properties": map[string]any{
					"type": map[string]any{"const": "image"},
				},
			},
		},
	}}
	if got := Estimate(nil, nil, tools); got >= perImageTokens {
		t.Fatalf("工具 schema 被误判成图片块: %d", got)
	}
	if got := Estimate(nil, nil, tools); got == 0 {
		t.Fatal("工具 schema 应按 JSON 字符估算")
	}
}

func TestEstimateSupportsStringAndBlockContent(t *testing.T) {
	messages := []any{
		message("plain text"),
		message([]any{map[string]any{"type": "text", "text": "block text"}}),
	}
	if got := Estimate(nil, messages, nil); got <= 2*perMessageOverhead {
		t.Fatalf("string/块数组内容没有被估算: %d", got)
	}
}

func TestEstimateEmptyInput(t *testing.T) {
	if got := Estimate(nil, nil, nil); got != 0 {
		t.Fatalf("nil 输入 = %d, 期望 0", got)
	}
	if got := Estimate("", []any{}, []any{}); got != 0 {
		t.Fatalf("空输入 = %d, 期望 0", got)
	}
}
