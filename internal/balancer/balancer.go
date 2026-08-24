// Package balancer 决定一条路由的候选尝试顺序，是阶段三负载均衡的落点。
//
// 阶段一把候选列表铺好后，三种策略的差别只在「顺序怎么排」：
//   - failover：按配置顺序，即阶段一的既有行为
//   - round-robin：按 per-route 计数器轮转起点
//   - least-queue：按队列负载升序，负载相同时按轮转顺序打散
//
// 无论哪种策略，选中的目标之外仍保留剩余候选，失败继续按该顺序转移 ——
// 负载均衡只改「先试谁」，不改「失败了还能试谁」。
//
// 这里刻意不依赖 router / config：Selector 只认下标与身份字符串，
// 由调用方把 Candidate 翻译进来。否则 router 想反向用它就会成环。
package balancer

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// 策略取值。配置里的 route.strategy 与这些常量一一对应。
const (
	StrategyFailover   = "failover"
	StrategyRoundRobin = "round-robin"
	StrategyLeastQueue = "least-queue"
)

// prompt cache 粘性参数。
//
// 这不是可选优化：Anthropic 与 OpenAI 都按上游侧前缀缓存计费，同一会话在多个
// 上游间跳会让缓存全部失效，可能又慢又贵。协议里没有会话 ID，只能对可缓存前缀
// 做哈希来近似会话身份。
//
// 参数不开放配置：多一个旋钮的收益远小于「这条路由到底怎么选目标」的解释成本。
const (
	// stickyTTL 与 Anthropic prompt cache 的默认 TTL 对齐；映射过期后重新按策略选。
	stickyTTL = 5 * time.Minute
	// stickyMaxEntries 粘性映射条数上限，超出按 LRU 淘汰。
	stickyMaxEntries = 1000
	// stickyMinPrefixLen 前缀短于该长度就不做粘性：
	// 上游侧缓存本身有最小 token 门槛，短前缀既进不了缓存，又容易在无关请求间撞同一个键。
	stickyMinPrefixLen = 256
)

// ValidStrategy 判断策略取值是否受支持，供 config 校验复用，
// 避免校验规则与实现各写一份取值表。
func ValidStrategy(s string) bool {
	switch s {
	case StrategyFailover, StrategyRoundRobin, StrategyLeastQueue:
		return true
	}
	return false
}

// UsesSticky 判断该策略是否参与 prompt cache 粘性。
//
// failover 的语义是「按配置顺序」，不掺任何跨请求状态，因此也不该记粘性 ——
// 否则会为永远不查询的路由白占 LRU 容量。
func UsesSticky(strategy string) bool {
	return strategy != "" && strategy != StrategyFailover && ValidStrategy(strategy)
}

// Selector 持有跨请求的选择状态：per-route 轮转计数器与 prompt cache 粘性映射。
//
// router 是无状态纯函数，这些状态只能由 server 持有并显式传入。
type Selector struct {
	mu     sync.Mutex
	rr     map[string]uint64
	sticky map[string]*stickyEntry
	lru    *list.List // 元素为 *stickyEntry，队首最旧
	now    func() time.Time
}

type stickyEntry struct {
	key     string
	target  string
	expires time.Time
	elem    *list.Element
}

// New 构造 Selector。
func New() *Selector {
	return &Selector{
		rr:     make(map[string]uint64),
		sticky: make(map[string]*stickyEntry),
		lru:    list.New(),
		now:    time.Now,
	}
}

// Select 返回候选下标的尝试顺序，长度恒等于 len(keys)，不丢候选。
//
// keys[i] 是候选 i 的稳定身份（形如 "provider/model"），用于粘性映射 ——
// 存下标会在配置热重载后指向另一个 provider。
// load 返回候选 i 的当前负载，只在 least-queue 下调用；
// directMode 无队列时它恒返回 0，least-queue 因此自然退化成轮询，
// 而不是静默退回配置顺序（那等于用户选的策略被忽略）。
// stickyKey 为空表示本次请求不参与粘性。
func (s *Selector) Select(routeKey, strategy string, keys []string, stickyKey string, load func(int) int) []int {
	n := len(keys)
	if n <= 1 {
		return identityOrder(n)
	}
	if strategy == "" {
		strategy = StrategyFailover
	}
	// failover 不轮转也不粘性：它的语义就是「按配置顺序」，
	// 掺入状态会让「为什么这次先试第二个」无法解释。
	if strategy == StrategyFailover {
		return identityOrder(n)
	}

	offset := s.nextOffset(routeKey, n)

	var order []int
	switch strategy {
	case StrategyLeastQueue:
		order = leastQueueOrder(n, offset, load)
	default:
		order = rotatedOrder(n, offset)
	}

	if stickyKey != "" {
		if target, ok := s.lookupSticky(routeKey, stickyKey); ok {
			order = hoist(order, indexOf(keys, target))
		}
	}
	return order
}

// Remember 把「该前缀下次仍走这个目标」记下来，只在候选真正成功后调用。
//
// 绑定失败过的目标会把整条会话钉在坏上游上，所以不在选中时绑定。
func (s *Selector) Remember(routeKey, stickyKey, target string) {
	if stickyKey == "" || target == "" {
		return
	}
	mapKey := routeKey + "\x00" + stickyKey

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e, ok := s.sticky[mapKey]; ok {
		e.target = target
		e.expires = now.Add(stickyTTL)
		s.lru.MoveToBack(e.elem)
		return
	}
	e := &stickyEntry{key: mapKey, target: target, expires: now.Add(stickyTTL)}
	e.elem = s.lru.PushBack(e)
	s.sticky[mapKey] = e
	s.evictLocked(now)
}

// Reconcile 丢弃已从配置中删除的路由状态，避免热重载反复改 match 后 map 无界增长。
//
// 轮转计数器尤其需要它：粘性映射有 TTL 和 LRU 兜底，rr 两者都没有，
// 只会随「历史上出现过的 routeKey」单调增长。
//
// active 是当前配置里的 route.match 集合。
func (s *Selector) Reconcile(active map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.rr {
		if _, ok := active[key]; !ok {
			delete(s.rr, key)
		}
	}
	// 粘性键是 routeKey + "\x00" + stickyKey，按前缀判断归属。
	for _, e := range s.stickyEntriesLocked() {
		routeKey, _, ok := strings.Cut(e.key, "\x00")
		if !ok {
			continue
		}
		if _, live := active[routeKey]; !live {
			s.removeLocked(e)
		}
	}
}

// stickyEntriesLocked 快照当前粘性项，便于遍历中安全删除。调用方须持锁。
func (s *Selector) stickyEntriesLocked() []*stickyEntry {
	out := make([]*stickyEntry, 0, len(s.sticky))
	for _, e := range s.sticky {
		out = append(out, e)
	}
	return out
}

// RouteCounters 返回当前保有轮转计数器的路由数，供测试与 /health 观测容量。
func (s *Selector) RouteCounters() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rr)
}

// StickyMappings 返回当前有效的粘性映射数，供 /health 观测。
func (s *Selector) StickyMappings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(s.now())
	return len(s.sticky)
}

// lookupSticky 读取未过期的粘性目标。
func (s *Selector) lookupSticky(routeKey, stickyKey string) (string, bool) {
	mapKey := routeKey + "\x00" + stickyKey

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sticky[mapKey]
	if !ok {
		return "", false
	}
	now := s.now()
	if now.After(e.expires) {
		s.removeLocked(e)
		return "", false
	}
	s.lru.MoveToBack(e.elem)
	return e.target, true
}

// nextOffset 取该路由的轮转起点并推进计数器。
//
// 粘性命中时也会推进：命中后仍要给剩余候选排序，且少一次推进换来的分布偏差
// 远小于为此多维护一条分支的代价。
func (s *Selector) nextOffset(routeKey string, n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.rr[routeKey]
	s.rr[routeKey] = v + 1
	return int(v % uint64(n))
}

// evictLocked 清掉过期项，再按 LRU 压到容量上限。调用方须持锁。
func (s *Selector) evictLocked(now time.Time) {
	for e := s.lru.Front(); e != nil; {
		entry := e.Value.(*stickyEntry)
		next := e.Next()
		if now.After(entry.expires) {
			s.removeLocked(entry)
		}
		e = next
	}
	for len(s.sticky) > stickyMaxEntries {
		front := s.lru.Front()
		if front == nil {
			return
		}
		s.removeLocked(front.Value.(*stickyEntry))
	}
}

func (s *Selector) removeLocked(e *stickyEntry) {
	s.lru.Remove(e.elem)
	delete(s.sticky, e.key)
}

// identityOrder 返回 0..n-1。
func identityOrder(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// rotatedOrder 从 offset 开始环形排列，保证每个候选都在结果里出现一次。
func rotatedOrder(n, offset int) []int {
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = (offset + i) % n
	}
	return out
}

// leastQueueOrder 按负载升序排列，负载相同时保持轮转顺序。
//
// 用 queue 的 running+queued 而不是轮询计数：它是真实在途量，比「上次给了谁」更准。
func leastQueueOrder(n, offset int, load func(int) int) []int {
	order := rotatedOrder(n, offset)
	if load == nil {
		return order
	}
	loads := make([]int, n)
	for i := 0; i < n; i++ {
		loads[i] = load(i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		return loads[order[a]] < loads[order[b]]
	})
	return order
}

// hoist 把下标 target 提到队首，其余保持原相对顺序。target < 0 时原样返回。
func hoist(order []int, target int) []int {
	if target < 0 {
		return order
	}
	out := make([]int, 0, len(order))
	out = append(out, target)
	for _, v := range order {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func indexOf(keys []string, target string) int {
	for i, k := range keys {
		if k == target {
			return i
		}
	}
	return -1
}

// StickyKey 对可缓存前缀（system + 首条 user 消息的文本）做 SHA-256，
// 用作近似会话身份。返回空字符串表示本次请求不参与粘性。
//
// 只取文本块，忽略图片（图片块不带 text 字段，天然被跳过）。
// 调用方必须在 vision 翻译之前取键：翻译会把图片块换成文字描述，
// 而描述可能因缓存命中情况而不同，掺进哈希会让同一会话前后算出两个键。
func StickyKey(system any, messages []any) string {
	var b strings.Builder
	appendText(&b, system)
	b.WriteByte('\x00')
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		appendText(&b, m["content"])
		break
	}
	prefix := b.String()
	if len(strings.TrimSpace(strings.ReplaceAll(prefix, "\x00", ""))) < stickyMinPrefixLen {
		return ""
	}
	sum := sha256.Sum256([]byte(prefix))
	return hex.EncodeToString(sum[:])
}

// appendText 把 string / []any 内容块里的文本追加进 builder，其他类型忽略。
func appendText(b *strings.Builder, content any) {
	switch v := content.(type) {
	case string:
		b.WriteString(v)
	case []any:
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok {
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
	}
}
