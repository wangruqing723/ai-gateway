package converter

// Codex 的 namespace 工具展平与还原。
//
// Codex CLI 用一个 Responses 私有扩展声明插件/MCP 工具：
//
//	{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function",...},...]}
//
// ChatGPT 后端认这个形状，第三方上游一律不认。同时 Codex 侧的工具调用回程用的是
// 「裸 name + 独立 namespace 字段」这一对来匹配自己的注册表：
//
//	{"type":"function_call","name":"close_agent","namespace":"multi_agent_v1",...}
//
// 实测（用抓包服务回不同的名字给 Codex CLI）：只给裸名 `close_agent` 或点号名
// `multi_agent_v1.close_agent`，Codex 都报 `unsupported call`；而顶层工具
// `update_plan` 能正常识别。所以还原时必须把 namespace 拆成独立字段，光拼名字没用。
//
// 于是需要一对互逆操作：
//   - 请求侧把 namespace 子工具提到顶层，名字取确定性的 <namespace>__<child>
//   - 响应侧把该扁平名还原成 {name, namespace}
//
// 两侧都从同一个请求 tools 推导名字（flattenNamespaceToolName），因此不必在请求与
// 响应之间传递额外状态，只传一张由请求推出的映射表即可。
//
// 设计参考 cc-switch 的 transform_codex_responses_namespace.rs。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// toolNameMaxLen 是工具名上限。Anthropic 要求 ^[a-zA-Z0-9_-]{1,64}$，OpenAI Chat
// 同为 64，取两者共同的下限，保证展平后的名字对三种上游都合法。
const toolNameMaxLen = 64

// NamespacedName 是一个扁平工具名对应的原始命名空间与裸名。
type NamespacedName struct {
	Namespace string
	Name      string
}

// flattenNamespaceToolName 生成确定性的扁平名。
//
// 超长时截断并接 sha256 前 8 位，保证：同一对 (namespace, name) 永远得到同一个名字，
// 且不同的对极难碰撞。请求侧与响应侧各自独立调用它也能对上账。
func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= toolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:])[:8]
	limit := toolNameMaxLen - len(suffix)
	// 按 rune 截断，避免把多字节字符切成半个。
	var b strings.Builder
	for _, r := range full {
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + suffix
}

// namespaceChildren 取 namespace 工具的子工具。Codex 用 tools，也见过 children。
func namespaceChildren(tool map[string]any) []any {
	if children, ok := tool["tools"].([]any); ok {
		return children
	}
	if children, ok := tool["children"].([]any); ok {
		return children
	}
	return nil
}

// namespaceRestoreMap 从 Responses 请求的 tools 推出「扁平名 → {namespace, 裸名}」。
// 映射为空表示这个请求没有 namespace 工具，响应侧无需做任何还原。
func namespaceRestoreMap(tools any) map[string]NamespacedName {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	out := map[string]NamespacedName{}
	for _, value := range arr {
		tool, ok := value.(map[string]any)
		if !ok || getString(tool, "type") != "namespace" {
			continue
		}
		namespace := strings.TrimSpace(getString(tool, "name"))
		if namespace == "" {
			continue
		}
		for _, childValue := range namespaceChildren(tool) {
			child, ok := childValue.(map[string]any)
			if !ok || getString(child, "type") != "function" {
				continue
			}
			name := strings.TrimSpace(getString(child, "name"))
			if name == "" {
				continue
			}
			flat := flattenNamespaceToolName(namespace, name)
			if _, exists := out[flat]; !exists {
				out[flat] = NamespacedName{Namespace: namespace, Name: name}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// flattenNamespaceTools 把 namespace 工具展平成顶层 function 工具列表。
//
// 返回展平后的工具数组与「扁平名 → 原始身份」映射。碰撞时报错而不静默丢工具：
// 上游无法区分两个同名工具，丢掉哪个都会让模型调用落空，报错更诚实。
func flattenNamespaceTools(tools []any) ([]any, map[string]NamespacedName, error) {
	// 顶层已占用的名字：namespace 子工具展平后撞上它属于不可恢复的冲突。
	topLevel := map[string]bool{}
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch getString(tool, "type") {
		case "function", "custom":
			if name := strings.TrimSpace(getString(tool, "name")); name != "" {
				topLevel[name] = true
			}
		}
	}

	owners := map[string]NamespacedName{}
	out := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if getString(tool, "type") != "namespace" {
			out = append(out, value)
			continue
		}
		namespace := strings.TrimSpace(getString(tool, "name"))
		if namespace == "" {
			continue
		}
		for _, childValue := range namespaceChildren(tool) {
			child, ok := childValue.(map[string]any)
			if !ok || getString(child, "type") != "function" {
				continue
			}
			name := strings.TrimSpace(getString(child, "name"))
			if name == "" {
				continue
			}
			flat := flattenNamespaceToolName(namespace, name)
			if topLevel[flat] {
				return nil, nil, fmt.Errorf(
					"namespace 工具 %q/%q 展平后为 %q，与同名顶层工具冲突，请重命名其中一个",
					namespace, name, flat)
			}
			entry := NamespacedName{Namespace: namespace, Name: name}
			if prev, exists := owners[flat]; exists {
				if prev != entry {
					return nil, nil, fmt.Errorf(
						"namespace 工具 %q/%q 与 %q/%q 展平后同为 %q，请重命名其中一个",
						prev.Namespace, prev.Name, namespace, name, flat)
				}
				// 完全相同的重复声明：跳过，不重复添加。
				continue
			}
			owners[flat] = entry

			lifted := make(map[string]any, len(child)+1)
			for k, v := range child {
				lifted[k] = v
			}
			lifted["name"] = flat
			out = append(out, lifted)
		}
	}
	if len(owners) == 0 {
		owners = nil
	}
	return out, owners, nil
}

// rewriteNamespacedCalls 把回放历史里带 namespace 的 function_call 改写成扁平名。
//
// 客户端回传的是自己那套 {name, namespace}，上游只认展平后的名字。不改写的话上游
// 看到的历史调用名与它收到的工具列表对不上。递归遍历以覆盖嵌套结构。
func rewriteNamespacedCalls(value any, owners map[string]NamespacedName) {
	if len(owners) == 0 {
		return
	}
	switch node := value.(type) {
	case []any:
		for _, item := range node {
			rewriteNamespacedCalls(item, owners)
		}
	case map[string]any:
		if getString(node, "type") == "function_call" {
			rewriteNamespacedCall(node, owners)
			return
		}
		for _, child := range node {
			rewriteNamespacedCalls(child, owners)
		}
	}
}

func rewriteNamespacedCall(call map[string]any, owners map[string]NamespacedName) bool {
	namespace := strings.TrimSpace(getString(call, "namespace"))
	name := strings.TrimSpace(getString(call, "name"))
	if namespace == "" || name == "" {
		return false
	}
	flat := flattenNamespaceToolName(namespace, name)
	entry, exists := owners[flat]
	if !exists || entry.Namespace != namespace || entry.Name != name {
		return false
	}
	call["name"] = flat
	delete(call, "namespace")
	return true
}

// RestoreNamespacedCalls 把响应里的扁平 function_call 名还原成 {name, namespace}。
//
// 供 proxy 在响应转换之后（或透传时）统一施加，覆盖流式与非流式。返回是否改动过。
func RestoreNamespacedCalls(value any, owners map[string]NamespacedName) bool {
	if len(owners) == 0 {
		return false
	}
	changed := false
	switch node := value.(type) {
	case []any:
		for _, item := range node {
			if RestoreNamespacedCalls(item, owners) {
				changed = true
			}
		}
	case map[string]any:
		if getString(node, "type") == "function_call" {
			if entry, exists := owners[getString(node, "name")]; exists {
				node["name"] = entry.Name
				node["namespace"] = entry.Namespace
				changed = true
			}
		}
		for _, child := range node {
			if RestoreNamespacedCalls(child, owners) {
				changed = true
			}
		}
	}
	return changed
}

// sortedFlatNames 供测试与日志用，保证输出顺序稳定。
func sortedFlatNames(owners map[string]NamespacedName) []string {
	out := make([]string, 0, len(owners))
	for k := range owners {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// namespaceRestorer 在转换/透传之后，把输出 SSE 行里的扁平 function_call 名还原。
//
// 做成装饰器而不是塞进每个 transformer：需要覆盖的是「所有客户端为 Responses 的
// 输出路径」，包括同格式透传（passthrough{}）。逐个 transformer 改要动 6 处且会漏掉
// 透传。
type namespaceRestorer struct {
	inner  StreamTransformer
	owners map[string]NamespacedName
}

// WithNamespaceRestore 在 inner 之外套一层 namespace 还原；owners 为空时原样返回 inner，
// 不引入任何额外解析开销。
func WithNamespaceRestore(inner StreamTransformer, owners map[string]NamespacedName) StreamTransformer {
	if len(owners) == 0 {
		return inner
	}
	return &namespaceRestorer{inner: inner, owners: owners}
}

func (t *namespaceRestorer) Transform(line string) []string {
	out := t.inner.Transform(line)
	for i, chunk := range out {
		out[i] = restoreSSEChunk(chunk, t.owners)
	}
	return out
}

// 转发内层的可选接口，否则 proxy 的错误终态、完成判定、中止会全部失效。
func (t *namespaceRestorer) Err() error      { return StreamError(t.inner) }
func (t *namespaceRestorer) Completed() bool { return StreamCompleted(t.inner) }

func (t *namespaceRestorer) Failure() []string {
	return t.restoreAll(StreamFailure(t.inner))
}

func (t *namespaceRestorer) Abort(errType, message string) []string {
	return t.restoreAll(AbortStream(t.inner, errType, message))
}

func (t *namespaceRestorer) restoreAll(chunks []string) []string {
	for i, chunk := range chunks {
		chunks[i] = restoreSSEChunk(chunk, t.owners)
	}
	return chunks
}

// restoreSSEChunk 只在 data 行确实含扁平名时才重建这一块，避免为每个事件付出
// 一次 JSON 解析 + 序列化的代价。
//
// 快速预筛用字符串包含判断：绝大多数事件（文本增量）不含任何工具名，直接原样返回。
func restoreSSEChunk(chunk string, owners map[string]NamespacedName) string {
	hit := false
	for flat := range owners {
		if strings.Contains(chunk, flat) {
			hit = true
			break
		}
	}
	if !hit {
		return chunk
	}

	// 逐行重建：一个 chunk 形如 "event: X\ndata: {...}\n\n"，只有 data 行需要改。
	lines := strings.Split(chunk, "\n")
	changed := false
	for i, line := range lines {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok || raw == "[DONE]" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			// 解析不了就别动它：原样透传比丢事件安全。
			continue
		}
		if !RestoreNamespacedCalls(payload, owners) {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		lines[i] = "data: " + string(encoded)
		changed = true
	}
	if !changed {
		return chunk
	}
	return strings.Join(lines, "\n")
}
