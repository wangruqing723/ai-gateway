package vision

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/config"
	"ai-gateway/internal/queue"
)

func TestDoRecognizeRejectsOversizedResponse(t *testing.T) {
	const responseLimit = 4 << 20
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", responseLimit+1))),
		}, nil
	})}
	translator := &Translator{resolve: func(string) (*http.Client, error) { return client, nil }}
	provider := &config.Provider{BaseURL: "https://vision.example", APIKey: "test-key", Format: "openai"}

	_, err := translator.doRecognize(context.Background(), imageBlock("oversized-response"), provider, "vision-model")
	if err == nil || !strings.Contains(err.Error(), "超过大小限制") {
		t.Fatalf("doRecognize() error = %v, want bounded response rejection", err)
	}
}

func TestHasImagesMatchesOneLevelToolResultGate(t *testing.T) {
	tests := []struct {
		name     string
		content  []any
		expected bool
	}{
		{name: "top-level image", content: []any{imageBlock("top")}, expected: true},
		{
			name: "one tool-result level",
			content: []any{map[string]any{
				"type":    "tool_result",
				"content": []any{imageBlock("one-level")},
			}},
			expected: true,
		},
		{
			name: "two tool-result levels",
			content: []any{map[string]any{
				"type": "tool_result",
				"content": []any{map[string]any{
					"type":    "tool_result",
					"content": []any{imageBlock("two-level")},
				}},
			}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := []any{map[string]any{"role": "user", "content": tt.content}}
			if got := HasImages(messages); got != tt.expected {
				t.Fatalf("HasImages() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestTranslateStopsBeforeNextImageWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			cancel()
		}
		writeVisionResponse(t, w, "图片描述")
	}))
	defer server.Close()

	translator, provider := newTestTranslator(t, server, true)
	messages := []any{map[string]any{
		"role": "user",
		"content": []any{
			imageBlock("first-image"),
			imageBlock("second-image"),
		},
	}}

	translator.Translate(ctx, messages, provider, "vision-model", discardLog)

	if got := requests.Load(); got != 1 {
		t.Fatalf("请求取消后仍继续识别后续图片：请求次数 = %d，期望 1", got)
	}
}

func TestCallVisionCanceledWaiterDoesNotCancelSharedRecognition(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	respond := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	var respondOnce sync.Once
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			canceledOnce.Do(func() { close(upstreamCanceled) })
		case <-respond:
			writeVisionResponse(t, w, "共享图片描述")
		case <-time.After(2 * time.Second):
			t.Error("视觉上游等待测试信号超时")
		}
	}))
	defer server.Close()
	defer respondOnce.Do(func() { close(respond) })

	translator, provider := newTestTranslator(t, server, true)
	image := imageBlock("shared-image-one-cancel")
	hash := cache.ImageHash(image)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	result1 := callVisionAsync(translator, ctx1, image, provider)
	waitForSignal(t, started, "视觉上游请求未启动")
	result2 := callVisionAsync(translator, ctx2, image, provider)
	waitForWaiterCount(t, translator, hash, 2)

	cancel1()
	got1 := waitForVisionResult(t, result1)
	if !errors.Is(got1.err, context.Canceled) {
		t.Fatalf("被取消 waiter 的错误 = %v，期望 context.Canceled", got1.err)
	}
	select {
	case <-upstreamCanceled:
		t.Fatal("仍有 waiter 时共享视觉上游被取消")
	case <-time.After(100 * time.Millisecond):
	}

	respondOnce.Do(func() { close(respond) })
	got2 := waitForVisionResult(t, result2)
	if got2.err != nil {
		t.Fatalf("存活 waiter 收到共享错误: %v", got2.err)
	}
	if got2.text != "共享图片描述" {
		t.Fatalf("存活 waiter 结果 = %q，期望共享图片描述", got2.text)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("同图并发请求次数 = %d，期望 1", got)
	}
	if cached, ok := translator.cache.Get(hash); !ok || cached != "共享图片描述" {
		t.Fatalf("共享任务未正确写入缓存：命中=%v，内容=%q", ok, cached)
	}
}

func TestCallVisionRechecksCacheAfterPendingCompletes(t *testing.T) {
	firstRequestStarted := make(chan struct{})
	releaseFirstRequest := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(firstRequestStarted)
			<-releaseFirstRequest
		}
		writeVisionResponse(t, w, "共享完成结果")
	}))
	defer server.Close()

	translator, provider := newTestTranslator(t, server, true)
	cacheSpy := newRecognitionCacheSpy()
	translator.cache = cacheSpy
	image := imageBlock("cache-pending-race")
	hash := cache.ImageHash(image)

	first := callVisionAsync(translator, context.Background(), image, provider)
	waitForSignal(t, firstRequestStarted, "首个视觉请求未启动")
	cacheSpy.blockNextGet.Store(true)
	second := callVisionAsync(translator, context.Background(), image, provider)
	waitForSignal(t, cacheSpy.secondGetStarted, "第二个调用未完成首次 cache miss")

	close(releaseFirstRequest)
	waitForSignal(t, cacheSpy.setDone, "首个识别未写入缓存")
	waitForNoPendingRecognition(t, translator, hash)
	close(cacheSpy.releaseSecondGet)

	firstResult := waitForVisionResult(t, first)
	secondResult := waitForVisionResult(t, second)
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("vision results = first %v, second %v", firstResult.err, secondResult.err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("completed pending result triggered %d upstream requests, want 1", got)
	}
	if !secondResult.fromCache || secondResult.text != "共享完成结果" {
		t.Fatalf("second result = %#v, want cache hit from completed pending", secondResult)
	}
}

func TestCallVisionCancelsUpstreamWhenAllWaitersLeave(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	var requests atomic.Int32

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	translator, provider := newTestTranslator(t, server, true)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(upstreamCanceled) })
		return nil, r.Context().Err()
	})}
	translator.resolve = func(string) (*http.Client, error) { return client, nil }

	image := imageBlock("shared-image-all-cancel")
	hash := cache.ImageHash(image)
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	result1 := callVisionAsync(translator, ctx1, image, provider)
	waitForSignal(t, started, "视觉上游请求未启动")
	result2 := callVisionAsync(translator, ctx2, image, provider)
	waitForWaiterCount(t, translator, hash, 2)
	translator.mu.Lock()
	shared := translator.pending[hash]
	translator.mu.Unlock()

	cancel1()
	cancel2()
	for i, result := range []<-chan visionCallResult{result1, result2} {
		got := waitForVisionResult(t, result)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("waiter %d 的错误 = %v，期望 context.Canceled", i+1, got.err)
		}
	}
	select {
	case <-shared.done:
		if !errors.Is(shared.err, context.Canceled) {
			t.Fatalf("共享识别错误 = %v，期望 context.Canceled", shared.err)
		}
	case <-time.After(time.Second):
		t.Fatal("共享识别在全部 waiter 离开后未结束")
	}
	waitForSignal(t, upstreamCanceled, "最后一个 waiter 离开后未取消视觉上游")
	waitForNoPendingRecognition(t, translator, hash)

	if got := requests.Load(); got != 1 {
		t.Fatalf("同图并发请求次数 = %d，期望 1", got)
	}
	if cached, ok := translator.cache.Get(hash); ok {
		t.Fatalf("被取消的共享任务不应写入缓存，实际内容 = %q", cached)
	}
}

func TestTranslatorSetDirectModeTakesEffectForNewRecognition(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeVisionResponse(t, w, "动态模式图片描述")
	}))
	defer server.Close()

	translator, provider := newTestTranslator(t, server, false)
	releaseBlocker, _, err := translator.qm.Acquire(
		context.Background(),
		provider.Name,
		provider.MaxConcurrent,
		provider.MaxPerSecond,
		provider.MaxQueueWait,
	)
	if err != nil {
		t.Fatalf("占用视觉队列 slot 失败: %v", err)
	}
	defer releaseBlocker()

	translator.SetDirectMode(true)
	directResult := callVisionAsync(translator, context.Background(), imageBlock("direct-mode-image"), provider)
	if got := waitForVisionResult(t, directResult); got.err != nil {
		t.Fatalf("开启 direct mode 后识别失败: %v", got.err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("开启 direct mode 后请求次数 = %d，期望 1", got)
	}

	translator.SetDirectMode(false)
	queuedResult := callVisionAsync(translator, context.Background(), imageBlock("queue-mode-image"), provider)
	waitForQueuedCount(t, translator, provider, 1)
	select {
	case got := <-queuedResult:
		t.Fatalf("关闭 direct mode 后请求未等待队列：结果错误 = %v", got.err)
	default:
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("队列 slot 未释放前请求次数 = %d，期望 1", got)
	}

	releaseBlocker()
	if got := waitForVisionResult(t, queuedResult); got.err != nil {
		t.Fatalf("释放队列 slot 后识别失败: %v", got.err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("释放队列 slot 后请求次数 = %d，期望 2", got)
	}
}

func newTestTranslator(t *testing.T, server *httptest.Server, directMode bool) (*Translator, *config.Provider) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取当前目录失败: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("切换到测试目录失败: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Errorf("恢复当前目录失败: %v", err)
		}
	})

	c, err := cache.Open()
	if err != nil {
		t.Fatalf("创建测试缓存失败: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("关闭测试缓存失败: %v", err)
		}
	})

	provider := &config.Provider{
		Name:          "vision-test",
		BaseURL:       server.URL,
		APIKey:        "test-key",
		Format:        "openai",
		MaxConcurrent: 1,
		MaxQueueWait:  1000,
	}
	return New(c, queue.NewManager(), func(string) (*http.Client, error) { return server.Client(), nil }, directMode), provider
}

func imageBlock(data string) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       data,
		},
	}
}

type visionCallResult struct {
	text      string
	fromCache bool
	err       error
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type recognitionCacheSpy struct {
	mu               sync.Mutex
	values           map[string]string
	blockNextGet     atomic.Bool
	secondGetStarted chan struct{}
	releaseSecondGet chan struct{}
	setDone          chan struct{}
	setOnce          sync.Once
}

func newRecognitionCacheSpy() *recognitionCacheSpy {
	return &recognitionCacheSpy{
		values:           make(map[string]string),
		secondGetStarted: make(chan struct{}),
		releaseSecondGet: make(chan struct{}),
		setDone:          make(chan struct{}),
	}
}

func (c *recognitionCacheSpy) Get(hash string) (string, bool) {
	c.mu.Lock()
	value, ok := c.values[hash]
	c.mu.Unlock()
	if c.blockNextGet.CompareAndSwap(true, false) {
		close(c.secondGetStarted)
		<-c.releaseSecondGet
	}
	return value, ok
}

func (c *recognitionCacheSpy) Set(hash, description string) error {
	c.mu.Lock()
	c.values[hash] = description
	c.mu.Unlock()
	c.setOnce.Do(func() { close(c.setDone) })
	return nil
}

func callVisionAsync(translator *Translator, ctx context.Context, image map[string]any, provider *config.Provider) <-chan visionCallResult {
	result := make(chan visionCallResult, 1)
	go func() {
		text, fromCache, err := translator.callVision(ctx, image, provider, "vision-model")
		result <- visionCallResult{text: text, fromCache: fromCache, err: err}
	}()
	return result
}

func waitForVisionResult(t *testing.T, result <-chan visionCallResult) visionCallResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatal("等待视觉识别调用返回超时")
		return visionCallResult{}
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func waitForWaiterCount(t *testing.T, translator *Translator, hash string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		translator.mu.Lock()
		got := 0
		if r := translator.pending[hash]; r != nil {
			got = r.waiters
		}
		translator.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 waiter 数量达到 %d 超时", want)
}

func waitForNoPendingRecognition(t *testing.T, translator *Translator, hash string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		translator.mu.Lock()
		_, ok := translator.pending[hash]
		translator.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("被取消的共享识别仍残留在 pending 中")
}

func waitForQueuedCount(t *testing.T, translator *Translator, provider *config.Provider, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := translator.qm.StatusOf(provider.Name, provider.MaxConcurrent, provider.MaxPerSecond)
		if status.Queued == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待视觉队列长度达到 %d 超时", want)
}

func writeVisionResponse(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if _, err := w.Write([]byte(`{"choices":[{"message":{"content":"` + text + `"}}]}`)); err != nil {
		t.Errorf("写入视觉响应失败: %v", err)
	}
}

func discardLog(string, ...any) {}
