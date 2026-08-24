package balancer

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// keysOf 生成 n 个形如 "p0/m0" 的候选身份。
func keysOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("p%d/m%d", i, i)
	}
	return out
}

// zeroLoad 模拟 directMode：队列未被使用，负载恒为 0。
func zeroLoad(int) int { return 0 }

func TestValidStrategy(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{StrategyFailover, true},
		{StrategyRoundRobin, true},
		{StrategyLeastQueue, true},
		{"", false},
		{"Failover", false},
		{"roundrobin", false},
		{"random", false},
	}
	for _, c := range cases {
		if got := ValidStrategy(c.in); got != c.want {
			t.Errorf("ValidStrategy(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// failover 必须逐字保持配置顺序：它是默认策略，行为要与阶段一完全一致。
func TestSelectFailoverKeepsConfigOrder(t *testing.T) {
	s := New()
	keys := keysOf(3)
	for i := 0; i < 5; i++ {
		order := s.Select("route", StrategyFailover, keys, "", nil)
		if want := []int{0, 1, 2}; !equalInts(order, want) {
			t.Fatalf("第 %d 次 order = %v, 期望 %v", i+1, order, want)
		}
	}
}

// 空 strategy 等同 failover，不能因为「没写」就变成轮转。
func TestSelectEmptyStrategyTreatedAsFailover(t *testing.T) {
	s := New()
	keys := keysOf(3)
	for i := 0; i < 3; i++ {
		if order := s.Select("route", "", keys, "", nil); !equalInts(order, []int{0, 1, 2}) {
			t.Fatalf("order = %v, 期望 [0 1 2]", order)
		}
	}
}

func TestSelectRoundRobinRotatesStart(t *testing.T) {
	s := New()
	keys := keysOf(3)
	want := [][]int{
		{0, 1, 2},
		{1, 2, 0},
		{2, 0, 1},
		{0, 1, 2},
	}
	for i, expect := range want {
		order := s.Select("route", StrategyRoundRobin, keys, "", nil)
		if !equalInts(order, expect) {
			t.Fatalf("第 %d 次 order = %v, 期望 %v", i+1, order, expect)
		}
	}
}

// 轮转计数器必须 per-route 独立，否则两条路由会互相推进对方的起点。
func TestSelectRoundRobinCountersAreIndependentPerRoute(t *testing.T) {
	s := New()
	keys := keysOf(2)
	if order := s.Select("route-a", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{0, 1}) {
		t.Fatalf("route-a 首次 order = %v, 期望 [0 1]", order)
	}
	if order := s.Select("route-b", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{0, 1}) {
		t.Fatalf("route-b 首次 order = %v, 期望 [0 1]（不应被 route-a 推进）", order)
	}
	if order := s.Select("route-a", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{1, 0}) {
		t.Fatalf("route-a 第二次 order = %v, 期望 [1 0]", order)
	}
}

// 顺序是「先试谁」，不是「只试谁」：无论怎么重排，全部候选都要在结果里各出现一次。
func TestSelectNeverDropsCandidates(t *testing.T) {
	strategies := []string{StrategyFailover, StrategyRoundRobin, StrategyLeastQueue}
	loads := []int{5, 0, 3, 1}
	load := func(i int) int { return loads[i] }
	for _, strategy := range strategies {
		s := New()
		keys := keysOf(len(loads))
		for i := 0; i < 6; i++ {
			order := s.Select("route", strategy, keys, "", load)
			if len(order) != len(keys) {
				t.Fatalf("%s: len(order) = %d, 期望 %d", strategy, len(order), len(keys))
			}
			seen := make(map[int]bool, len(order))
			for _, idx := range order {
				if idx < 0 || idx >= len(keys) {
					t.Fatalf("%s: order 含越界下标 %d", strategy, idx)
				}
				if seen[idx] {
					t.Fatalf("%s: order 含重复下标 %d（%v）", strategy, idx, order)
				}
				seen[idx] = true
			}
		}
	}
}

func TestSelectLeastQueueOrdersByLoad(t *testing.T) {
	s := New()
	keys := keysOf(4)
	loads := []int{7, 2, 9, 0}
	order := s.Select("route", StrategyLeastQueue, keys, "", func(i int) int { return loads[i] })
	if want := []int{3, 1, 0, 2}; !equalInts(order, want) {
		t.Fatalf("order = %v, 期望 %v（按负载升序）", order, want)
	}
}

// directMode 下没有队列，负载恒 0。此时 least-queue 必须退化成轮询，
// 而不是静默退回配置顺序 —— 后者等于用户选的策略被忽略（LiteLLM #32425 那类 bug）。
func TestSelectLeastQueueDegradesToRoundRobinWhenLoadsEqual(t *testing.T) {
	s := New()
	keys := keysOf(3)
	want := [][]int{
		{0, 1, 2},
		{1, 2, 0},
		{2, 0, 1},
	}
	for i, expect := range want {
		order := s.Select("route", StrategyLeastQueue, keys, "", zeroLoad)
		if !equalInts(order, expect) {
			t.Fatalf("第 %d 次 order = %v, 期望 %v（负载相同应轮转）", i+1, order, expect)
		}
	}
}

// load 为 nil 时不能 panic，退化成轮转即可。
func TestSelectLeastQueueNilLoadFallsBackToRotation(t *testing.T) {
	s := New()
	keys := keysOf(3)
	if order := s.Select("route", StrategyLeastQueue, keys, "", nil); !equalInts(order, []int{0, 1, 2}) {
		t.Fatalf("order = %v, 期望 [0 1 2]", order)
	}
	if order := s.Select("route", StrategyLeastQueue, keys, "", nil); !equalInts(order, []int{1, 2, 0}) {
		t.Fatalf("order = %v, 期望 [1 2 0]", order)
	}
}

// 单候选时不该动用任何状态，也不该推进轮转计数器。
func TestSelectSingleCandidateShortCircuits(t *testing.T) {
	s := New()
	if order := s.Select("route", StrategyRoundRobin, keysOf(1), "", nil); !equalInts(order, []int{0}) {
		t.Fatalf("order = %v, 期望 [0]", order)
	}
	if order := s.Select("route", StrategyRoundRobin, keysOf(0), "", nil); len(order) != 0 {
		t.Fatalf("order = %v, 期望空", order)
	}
	// 单候选没走 nextOffset，多候选请求应从 offset 0 起算
	if order := s.Select("route", StrategyRoundRobin, keysOf(2), "", nil); !equalInts(order, []int{0, 1}) {
		t.Fatalf("order = %v, 期望 [0 1]（单候选不应推进计数器）", order)
	}
}

func TestRememberHoistsStickyTargetToFront(t *testing.T) {
	s := New()
	keys := keysOf(3)
	// 绑定候选 2，之后无论轮转到哪，它都应排在最前
	s.Remember("route", "sticky-key", keys[2])
	for i := 0; i < 4; i++ {
		order := s.Select("route", StrategyRoundRobin, keys, "sticky-key", nil)
		if order[0] != 2 {
			t.Fatalf("第 %d 次 order = %v, 期望首位为 2", i+1, order)
		}
		if len(order) != 3 {
			t.Fatalf("order = %v, 期望仍含 3 个候选", order)
		}
	}
}

// 粘性对 failover 不生效：failover 的语义是「按配置顺序」，掺状态会让顺序无法解释。
func TestStickyIgnoredUnderFailover(t *testing.T) {
	s := New()
	keys := keysOf(3)
	s.Remember("route", "sticky-key", keys[2])
	if order := s.Select("route", StrategyFailover, keys, "sticky-key", nil); !equalInts(order, []int{0, 1, 2}) {
		t.Fatalf("order = %v, 期望 [0 1 2]", order)
	}
}

// 粘性键为空（前缀太短）时不查映射，按纯策略走。
func TestEmptyStickyKeySkipsLookup(t *testing.T) {
	s := New()
	keys := keysOf(3)
	s.Remember("route", "", keys[2]) // 空键不该被记下
	if got := s.StickyMappings(); got != 0 {
		t.Fatalf("StickyMappings = %d, 期望 0", got)
	}
	if order := s.Select("route", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{0, 1, 2}) {
		t.Fatalf("order = %v, 期望 [0 1 2]", order)
	}
}

// 粘性映射按 routeKey 隔离：两条路由用同一段前缀不该串台。
func TestStickyMappingsScopedByRoute(t *testing.T) {
	s := New()
	keys := keysOf(3)
	s.Remember("route-a", "same-prefix", keys[2])
	if order := s.Select("route-b", StrategyRoundRobin, keys, "same-prefix", nil); order[0] == 2 {
		t.Fatalf("route-b order = %v, 不应命中 route-a 的粘性绑定", order)
	}
}

// 存 provider/model 而非下标：热重载改了 targets 顺序后，旧下标会指向另一个上游。
// 目标已不在候选列表里时，粘性应静默失效而不是打乱顺序。
func TestStickyTargetMissingFromCandidatesIsIgnored(t *testing.T) {
	s := New()
	keys := keysOf(3)
	s.Remember("route", "sticky-key", "removed-provider/removed-model")
	order := s.Select("route", StrategyRoundRobin, keys, "sticky-key", nil)
	if !equalInts(order, []int{0, 1, 2}) {
		t.Fatalf("order = %v, 期望 [0 1 2]（失效绑定应被忽略）", order)
	}
}

func TestStickyExpiresAfterTTL(t *testing.T) {
	s := New()
	now := time.Now()
	s.now = func() time.Time { return now }
	keys := keysOf(3)

	s.Remember("route", "sticky-key", keys[2])
	if order := s.Select("route", StrategyRoundRobin, keys, "sticky-key", nil); order[0] != 2 {
		t.Fatalf("TTL 内 order = %v, 期望首位为 2", order)
	}

	now = now.Add(stickyTTL + time.Second)
	if order := s.Select("route", StrategyRoundRobin, keys, "sticky-key", nil); order[0] == 2 {
		t.Fatalf("TTL 过后 order = %v, 粘性应已过期", order)
	}
	if got := s.StickyMappings(); got != 0 {
		t.Fatalf("StickyMappings = %d, 期望 0（过期项应被清掉）", got)
	}
}

func TestStickyLRUEvictsOldestBeyondCapacity(t *testing.T) {
	s := New()
	keys := keysOf(2)
	// 写满并超出上限
	for i := 0; i < stickyMaxEntries+50; i++ {
		s.Remember("route", fmt.Sprintf("key-%d", i), keys[1])
	}
	if got := s.StickyMappings(); got != stickyMaxEntries {
		t.Fatalf("StickyMappings = %d, 期望 %d", got, stickyMaxEntries)
	}
	// 最早写入的应已被淘汰
	if order := s.Select("route", StrategyRoundRobin, keys, "key-0", nil); order[0] == 1 {
		t.Fatalf("key-0 仍命中粘性（order = %v），期望已被 LRU 淘汰", order)
	}
	// 最后写入的仍应在
	last := fmt.Sprintf("key-%d", stickyMaxEntries+49)
	if order := s.Select("route", StrategyRoundRobin, keys, last, nil); order[0] != 1 {
		t.Fatalf("%s order = %v, 期望仍命中粘性", last, order)
	}
}

// 重复 Remember 同一个键只应更新，不应堆积条目。
func TestRememberSameKeyUpdatesInPlace(t *testing.T) {
	s := New()
	keys := keysOf(3)
	s.Remember("route", "sticky-key", keys[1])
	s.Remember("route", "sticky-key", keys[2])
	if got := s.StickyMappings(); got != 1 {
		t.Fatalf("StickyMappings = %d, 期望 1", got)
	}
	if order := s.Select("route", StrategyRoundRobin, keys, "sticky-key", nil); order[0] != 2 {
		t.Fatalf("order = %v, 期望首位为 2（应指向最后一次绑定）", order)
	}
}

func TestStickyKey(t *testing.T) {
	longText := strings.Repeat("这是一段足够长的系统提示词，用来越过粘性最小前缀门槛。", 20)
	shortText := "hi"

	cases := []struct {
		name     string
		system   any
		messages []any
		wantKey  bool
	}{
		{
			name:     "长 system 加首条 user 消息产生键",
			system:   longText,
			messages: []any{map[string]any{"role": "user", "content": "第一个问题"}},
			wantKey:  true,
		},
		{
			name:     "前缀过短不参与粘性",
			system:   shortText,
			messages: []any{map[string]any{"role": "user", "content": "hi"}},
			wantKey:  false,
		},
		{
			name:     "无 system 但首条 user 消息够长",
			system:   nil,
			messages: []any{map[string]any{"role": "user", "content": longText}},
			wantKey:  true,
		},
		{
			name:     "内容块数组里的文本被计入",
			system:   nil,
			messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": longText}}}},
			wantKey:  true,
		},
		{
			name:     "没有 user 消息",
			system:   longText,
			messages: []any{map[string]any{"role": "assistant", "content": "答复"}},
			wantKey:  true, // system 本身已够长
		},
		{
			name:     "空输入",
			system:   nil,
			messages: nil,
			wantKey:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StickyKey(c.system, c.messages)
			if c.wantKey && got == "" {
				t.Fatalf("StickyKey 返回空，期望产生键")
			}
			if !c.wantKey && got != "" {
				t.Fatalf("StickyKey = %q, 期望空", got)
			}
			if c.wantKey && len(got) != 64 {
				t.Fatalf("len(StickyKey) = %d, 期望 64（SHA-256 十六进制）", len(got))
			}
		})
	}
}

// 同一会话的相同前缀必须稳定算出同一个键，否则粘性永远命中不了。
func TestStickyKeyStableForSamePrefix(t *testing.T) {
	system := strings.Repeat("稳定的系统提示词，长度足够越过门槛。", 20)
	first := []any{
		map[string]any{"role": "user", "content": "第一个问题"},
	}
	// 会话推进后追加了新消息，但可缓存前缀不变
	second := []any{
		map[string]any{"role": "user", "content": "第一个问题"},
		map[string]any{"role": "assistant", "content": "答复"},
		map[string]any{"role": "user", "content": "第二个问题"},
	}
	a := StickyKey(system, first)
	b := StickyKey(system, second)
	if a == "" {
		t.Fatal("StickyKey 返回空")
	}
	if a != b {
		t.Fatalf("同一前缀算出两个键:\n%s\n%s", a, b)
	}
}

// 只取首条 user 消息：后续轮次的内容变化不能改变键。
func TestStickyKeyDiffersForDifferentPrefix(t *testing.T) {
	long := strings.Repeat("系统提示词内容，长度足够越过门槛。", 20)
	a := StickyKey(long, []any{map[string]any{"role": "user", "content": "问题 A"}})
	b := StickyKey(long, []any{map[string]any{"role": "user", "content": "问题 B"}})
	if a == "" || b == "" {
		t.Fatal("StickyKey 返回空")
	}
	if a == b {
		t.Fatal("不同首条 user 消息算出同一个键")
	}
}

// 图片块不带 text 字段，天然不进哈希：vision 翻译前后不应改变键。
func TestStickyKeyIgnoresImageBlocks(t *testing.T) {
	long := strings.Repeat("系统提示词内容，长度足够越过门槛。", 20)
	withImage := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": long},
		map[string]any{"type": "image", "source": map[string]any{"data": "BASE64"}},
	}}}
	textOnly := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": long},
	}}}
	if a, b := StickyKey(nil, withImage), StickyKey(nil, textOnly); a != b {
		t.Fatalf("图片块影响了粘性键:\n%s\n%s", a, b)
	}
}

// Selector 被所有请求共享，并发下不能有数据竞争。配合 -race 运行。
func TestSelectorConcurrentAccess(t *testing.T) {
	s := New()
	keys := keysOf(4)
	loads := []int{3, 1, 4, 0}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			route := fmt.Sprintf("route-%d", n%3)
			sticky := fmt.Sprintf("sticky-%d", n%5)
			for j := 0; j < 60; j++ {
				order := s.Select(route, StrategyLeastQueue, keys, sticky, func(i int) int { return loads[i] })
				if len(order) != len(keys) {
					t.Errorf("len(order) = %d, 期望 %d", len(order), len(keys))
					return
				}
				s.Remember(route, sticky, keys[order[0]])
				s.StickyMappings()
			}
		}(i)
	}
	wg.Wait()
}

// Reconcile 要丢掉已删除路由的轮转计数器：粘性映射有 TTL 和 LRU 兜底，
// rr 两者都没有，只会随「历史上出现过的 routeKey」单调增长。
func TestReconcileDropsRemovedRouteCounters(t *testing.T) {
	s := New()
	keys := keysOf(2)
	s.Select("keep", StrategyRoundRobin, keys, "", nil)
	s.Select("drop", StrategyRoundRobin, keys, "", nil)
	if got := s.RouteCounters(); got != 2 {
		t.Fatalf("RouteCounters = %d, 期望 2", got)
	}

	s.Reconcile(map[string]struct{}{"keep": {}})
	if got := s.RouteCounters(); got != 1 {
		t.Fatalf("Reconcile 后 RouteCounters = %d, 期望 1", got)
	}
}

// 仍在配置里的路由，轮转进度不能被 Reconcile 顺手清零 ——
// 清零会让每次保存配置都把全部流量重新打回第一个候选。
func TestReconcilePreservesRotationForLiveRoute(t *testing.T) {
	s := New()
	keys := keysOf(2)
	if order := s.Select("keep", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{0, 1}) {
		t.Fatalf("首次 order = %v, 期望 [0 1]", order)
	}

	s.Reconcile(map[string]struct{}{"keep": {}})

	// 计数器保住的话下一次应从 1 起转；被清零则又是 [0 1]。
	if order := s.Select("keep", StrategyRoundRobin, keys, "", nil); !equalInts(order, []int{1, 0}) {
		t.Fatalf("Reconcile 后 order = %v, 期望 [1 0]（轮转进度被清零了）", order)
	}
}

// 粘性映射按 routeKey 前缀归属，删掉的路由要连带清掉。
func TestReconcileDropsStickyForRemovedRoutes(t *testing.T) {
	s := New()
	keys := keysOf(2)
	s.Remember("keep", "sticky-a", keys[0])
	s.Remember("drop", "sticky-a", keys[1])
	if got := s.StickyMappings(); got != 2 {
		t.Fatalf("StickyMappings = %d, 期望 2", got)
	}

	s.Reconcile(map[string]struct{}{"keep": {}})
	if got := s.StickyMappings(); got != 1 {
		t.Fatalf("Reconcile 后 StickyMappings = %d, 期望 1", got)
	}
	if target, ok := s.lookupSticky("keep", "sticky-a"); !ok || target != keys[0] {
		t.Errorf("保留路由的粘性丢了: target=%q ok=%v", target, ok)
	}
	if _, ok := s.lookupSticky("drop", "sticky-a"); ok {
		t.Error("已删除路由的粘性仍能命中")
	}
}

// 同名 stickyKey 落在不同路由上，只清掉被删的那条。
func TestReconcileScopedByRouteNotByStickyKey(t *testing.T) {
	s := New()
	keys := keysOf(2)
	const shared = "same-prefix-hash"
	s.Remember("route-a", shared, keys[0])
	s.Remember("route-b", shared, keys[1])

	s.Reconcile(map[string]struct{}{"route-b": {}})
	if _, ok := s.lookupSticky("route-a", shared); ok {
		t.Error("route-a 已删除，其粘性应被清掉")
	}
	if target, ok := s.lookupSticky("route-b", shared); !ok || target != keys[1] {
		t.Errorf("route-b 仍在配置里，粘性不该被同名 stickyKey 带走: target=%q ok=%v", target, ok)
	}
}

// 配置里一条路由都不剩（极端热重载）时全部清空，且不 panic。
func TestReconcileEmptyActiveClearsAll(t *testing.T) {
	s := New()
	keys := keysOf(2)
	s.Select("r1", StrategyRoundRobin, keys, "", nil)
	s.Remember("r1", "sticky", keys[0])

	s.Reconcile(map[string]struct{}{})
	if got := s.RouteCounters(); got != 0 {
		t.Errorf("RouteCounters = %d, 期望 0", got)
	}
	if got := s.StickyMappings(); got != 0 {
		t.Errorf("StickyMappings = %d, 期望 0", got)
	}
}

// Reconcile 走 removeLocked，必须同时摘掉 LRU 链表节点，
// 否则后续淘汰会遍历到已删除的项。
func TestReconcileKeepsLRUConsistent(t *testing.T) {
	s := New()
	keys := keysOf(2)
	for i := 0; i < 10; i++ {
		s.Remember(fmt.Sprintf("route-%d", i), "sticky", keys[0])
	}

	s.Reconcile(map[string]struct{}{"route-3": {}})
	if got := s.StickyMappings(); got != 1 {
		t.Fatalf("StickyMappings = %d, 期望 1", got)
	}
	// 清理后继续写入、继续淘汰都应正常。
	s.Remember("route-3", "sticky-2", keys[1])
	if got := s.StickyMappings(); got != 2 {
		t.Fatalf("Reconcile 后再写入 StickyMappings = %d, 期望 2", got)
	}
	if got := s.lru.Len(); got != 2 {
		t.Fatalf("LRU 链表长度 = %d, 期望 2（与 map 一致）", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
