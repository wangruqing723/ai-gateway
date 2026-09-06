// Package vision 对齐 Node 版 lib/vision.js：将消息中的图片块替换为视觉模型生成的文字描述。
//
// 关键能力：SQLite 缓存命中复用、同一图片并发识别去重（singleflight 语义）、
// 通过 queue 控制对视觉 provider 的并发、tool_result 内嵌图片递归处理。
package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/queue"
)

const (
	visionRequestTimeout       = 120 * time.Second
	maxVisionResponseBodyBytes = 4 << 20
)

const visionPrompt = "请对这张图片进行全面详细的描述，包括所有可见文字（原文）、数据与图表数值、代码内容、UI布局与元素等，确保纯文本模型仅凭此描述即可完整理解图片。"

// LogFunc 绑定了 reqId 的日志函数
type LogFunc func(format string, args ...any)

// ClientResolver 按视觉 provider 的代理配置返回该用的 HTTP client。
type ClientResolver func(proxyURL string) (*http.Client, error)

// Translator 图片识别翻译器
type Translator struct {
	cache      recognitionCache
	qm         *queue.Manager
	resolve    ClientResolver
	directMode atomic.Bool // 直通模式：跳过视觉队列的并发/限速控制

	mu      sync.Mutex
	pending map[string]*recognition // 正在识别中的图片，按 hash 去重
}

type recognitionCache interface {
	Get(hash string) (string, bool)
	Set(hash, description string) error
}

type recognition struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	text    string
	err     error
}

// New 创建翻译器。resolve 从主程序的连接池按 provider 代理取 client。
func New(c *cache.Cache, qm *queue.Manager, resolve ClientResolver, directMode bool) *Translator {
	t := &Translator{
		cache:   c,
		qm:      qm,
		resolve: resolve,
		pending: make(map[string]*recognition),
	}
	t.directMode.Store(directMode)
	return t
}

// SetDirectMode 动态更新后续视觉识别是否跳过队列。
func (t *Translator) SetDirectMode(enabled bool) {
	t.directMode.Store(enabled)
}

// HasImages 检测消息数组是否包含图片块（对齐 Node hasImages：最多检查一层 tool_result）。
func HasImages(messages []any) bool {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		if blocksHaveImage(content) {
			return true
		}
	}
	return false
}

// blocksHaveImage 检测当前层及一层 tool_result，保持与历史 Node gate 一致。
func blocksHaveImage(blocks []any) bool {
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "image" {
			return true
		}
		// 检查一层 tool_result 内嵌图片，不继续向下递归。
		if block["type"] == "tool_result" {
			if inner, ok := block["content"].([]any); ok {
				for _, child := range inner {
					childBlock, ok := child.(map[string]any)
					if ok && childBlock["type"] == "image" {
						return true
					}
				}
			}
		}
	}
	return false
}

type stats struct{ cached, recognized, failed, skipped int }

// Translate 遍历消息，将图片块替换为文字描述，对齐 Node 版 translateImages。
func (t *Translator) Translate(ctx context.Context, messages []any, vision *config.Provider, visionModel string, log LogFunc) []any {
	idx := 0
	st := &stats{}
	out := make([]any, 0, len(messages))

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok || !blocksHaveImage(content) {
			out = append(out, m)
			continue
		}
		newContent := t.processBlocks(ctx, content, vision, visionModel, &idx, st, log)
		newMsg := make(map[string]any, len(msg))
		for k, v := range msg {
			newMsg[k] = v
		}
		newMsg["content"] = newContent
		out = append(out, newMsg)
	}

	total := st.cached + st.recognized + st.failed
	if total > 0 {
		var parts []string
		if st.cached > 0 {
			parts = append(parts, fmt.Sprintf("%d 缓存", st.cached))
		}
		if st.recognized > 0 {
			parts = append(parts, fmt.Sprintf("%d 识别", st.recognized))
		}
		if st.failed > 0 {
			parts = append(parts, fmt.Sprintf("%d 失败", st.failed))
		}
		if st.skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d 跳过", st.skipped))
		}
		log("  图片: %s [%s]", strings.Join(parts, ", "), visionModel)
	}
	return out
}

// processBlocks 递归处理 content 数组，替换 image 块为文字描述。
func (t *Translator) processBlocks(ctx context.Context, blocks []any, vision *config.Provider, visionModel string, idx *int, st *stats, log LogFunc) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		if ctx.Err() != nil {
			break
		}
		block, ok := b.(map[string]any)
		if !ok {
			out = append(out, b)
			continue
		}
		switch block["type"] {
		case "image":
			*idx++
			n := *idx
			text, fromCache, err := t.callVision(ctx, block, vision, visionModel)
			if err != nil {
				st.failed++
				log("  图片 #%d 识别失败: %s", n, err.Error())
				out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("[图片 #%d 识别失败: %s]", n, err.Error())})
				continue
			}
			if fromCache {
				st.cached++
			} else {
				st.recognized++
				log("  图片 #%d 识别完成 (%d 字) [%s]", n, len([]rune(text)), visionModel)
			}
			out = append(out, map[string]any{"type": "text", "text": fmt.Sprintf("[图片描述 #%d]\n%s\n[/图片描述 #%d]", n, text, n)})
		case "tool_result":
			if inner, ok := block["content"].([]any); ok {
				newInner := t.processBlocks(ctx, inner, vision, visionModel, idx, st, log)
				newBlock := make(map[string]any, len(block))
				for k, v := range block {
					newBlock[k] = v
				}
				newBlock["content"] = newInner
				out = append(out, newBlock)
			} else {
				out = append(out, block)
			}
		default:
			out = append(out, block)
		}
	}
	return out
}

// callVision 识别单张图片：先查缓存，再 singleflight 去重，最后经队列调用视觉 API。
func (t *Translator) callVision(ctx context.Context, imageBlock map[string]any, vision *config.Provider, visionModel string) (text string, fromCache bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}

	hash := cache.ImageHash(imageBlock)

	if cached, ok := t.cache.Get(hash); ok {
		return cached, true, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}

	// singleflight：同一图片并发识别时复用同一次请求
	t.mu.Lock()
	if err := ctx.Err(); err != nil {
		t.mu.Unlock()
		return "", false, err
	}
	r, ok := t.pending[hash]
	if ok {
		r.waiters++
	} else {
		if cached, cacheOK := t.cache.Get(hash); cacheOK {
			t.mu.Unlock()
			return cached, true, nil
		}
		if err := ctx.Err(); err != nil {
			t.mu.Unlock()
			return "", false, err
		}
		sharedCtx, cancel := context.WithTimeout(context.Background(), visionRequestTimeout)
		r = &recognition{
			done:    make(chan struct{}),
			cancel:  cancel,
			waiters: 1,
		}
		t.pending[hash] = r
		go t.runRecognition(sharedCtx, hash, r, imageBlock, vision, visionModel)
	}
	t.mu.Unlock()
	defer t.leaveRecognition(hash, r)

	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case <-r.done:
		return r.text, false, r.err
	}
}

// runRecognition 在独立 goroutine 中执行一次共享识别，并统一发布结果和写入缓存。
func (t *Translator) runRecognition(ctx context.Context, hash string, r *recognition, imageBlock map[string]any, vision *config.Provider, visionModel string) {
	defer r.cancel()

	text, err := t.recognize(ctx, imageBlock, vision, visionModel)
	if err == nil {
		_ = t.cache.Set(hash, text)
	}

	t.mu.Lock()
	r.text, r.err = text, err
	if t.pending[hash] == r {
		delete(t.pending, hash)
	}
	close(r.done)
	t.mu.Unlock()
}

func (t *Translator) recognize(ctx context.Context, imageBlock map[string]any, vision *config.Provider, visionModel string) (string, error) {
	if t.directMode.Load() {
		// 直通模式：跳过队列，直接识别
		return t.doRecognize(ctx, imageBlock, vision, visionModel)
	}

	// 经队列控制视觉 provider 并发
	release, _, err := t.qm.Acquire(ctx, vision.Name, vision.MaxConcurrent, vision.MaxPerSecond, vision.MaxQueueWait)
	if err != nil {
		return "", err
	}
	defer release()
	return t.doRecognize(ctx, imageBlock, vision, visionModel)
}

// leaveRecognition 注销一个调用方；最后一个 waiter 离开时取消共享上游。
func (t *Translator) leaveRecognition(hash string, r *recognition) {
	var cancel context.CancelFunc
	t.mu.Lock()
	if r.waiters > 0 {
		r.waiters--
	}
	if r.waiters == 0 && t.pending[hash] == r {
		delete(t.pending, hash)
		cancel = r.cancel
	}
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// doRecognize 实际调用视觉模型（OpenAI /chat/completions 格式）。
func (t *Translator) doRecognize(ctx context.Context, imageBlock map[string]any, vision *config.Provider, visionModel string) (string, error) {
	openAIBlock, err := toOpenAIImageBlock(imageBlock)
	if err != nil {
		return "", err
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model":                 visionModel,
		"max_completion_tokens": 3000,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{openAIBlock, map[string]any{"type": "text", "text": visionPrompt}},
		}},
	})

	url := buildVisionURL(vision.BaseURL)
	// 超时由共享识别 context 控制，无需在 HTTP 层重复叠加。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+vision.APIKey)
	// 视觉识别也是网关自己发起的上游请求，同样受 UA 门禁型 provider 影响
	// （实测 agentrouter.org 系上游对 Go 默认 UA 返回 401 unauthorized client）。
	// 与 providerhealth / fetchUpstreamModels 同语义：配了就用，没配不设头。
	if vision.UserAgent != "" {
		req.Header.Set("User-Agent", vision.UserAgent)
	}

	if t.resolve == nil {
		return "", fmt.Errorf("HTTP client 解析器未初始化")
	}
	client, err := t.resolve(vision.Proxy)
	if err != nil {
		return "", fmt.Errorf("解析视觉 provider 代理失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.ContentLength > maxVisionResponseBodyBytes {
		return "", fmt.Errorf("视觉 API 响应超过大小限制 (%d bytes)", maxVisionResponseBodyBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVisionResponseBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取视觉响应失败: %w", err)
	}
	if len(raw) > maxVisionResponseBodyBytes {
		return "", fmt.Errorf("视觉 API 响应超过大小限制 (%d bytes)", maxVisionResponseBodyBytes)
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("解析视觉响应失败: %w", err)
	}

	text := extractVisionText(response)
	if text == "" {
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return "", fmt.Errorf("视觉 API 响应异常 (HTTP %d): %s", resp.StatusCode, snippet)
	}
	return text, nil
}

// extractVisionText 优先 content，其次 reasoning_content（对齐 Node 版兼容逻辑）。
func extractVisionText(response map[string]any) string {
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	if c, ok := msg["content"].(string); ok && c != "" {
		return c
	}
	if rc, ok := msg["reasoning_content"].(string); ok {
		return rc
	}
	return ""
}

// toOpenAIImageBlock Anthropic image 块 → OpenAI image_url 块。
func toOpenAIImageBlock(block map[string]any) (map[string]any, error) {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return nil, fmt.Errorf("图片块缺少 source")
	}
	switch src["type"] {
	case "base64":
		mediaType, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data}}, nil
	case "url":
		u, _ := src["url"].(string)
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}, nil
	}
	return nil, fmt.Errorf("不支持的图片 source 类型: %v", src["type"])
}

// buildVisionURL 去掉末尾 /v1 后拼 /v1/chat/completions（对齐 Node 版）。
func buildVisionURL(baseURL string) string {
	base := baseURL
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/chat/completions"
}
