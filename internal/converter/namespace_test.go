package converter

import (
	"encoding/json"
	"strings"
	"testing"
)

func namespaceRequestBody() map[string]any {
	return map[string]any{
		"model": "gpt-5.6-sol",
		"tools": []any{
			map[string]any{
				"type": "function", "name": "update_plan",
				"parameters": map[string]any{"type": "object"},
			},
			map[string]any{
				"type": "namespace", "name": "multi_agent_v1",
				"description": "Tools for spawning and managing sub-agents.",
				"tools": []any{
					map[string]any{
						"type": "function", "name": "close_agent",
						"description": "Close an agent.",
						"parameters":  map[string]any{"type": "object"},
					},
					map[string]any{
						"type": "function", "name": "spawn_agent",
						"parameters": map[string]any{"type": "object"},
					},
				},
			},
			map[string]any{"type": "web_search", "external_web_access": true},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "关掉那个 agent"},
			}},
			// 回放历史里的调用用的是 Codex 那套 {name, namespace}
			map[string]any{
				"type": "function_call", "name": "close_agent",
				"namespace": "multi_agent_v1", "call_id": "call_1", "arguments": "{}",
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call_1", "output": "closed",
			},
		},
		"tool_choice": map[string]any{"type": "namespace", "name": "multi_agent_v1"},
	}
}

// namespace 子工具必须提到顶层，且内置工具（web_search）丢弃。
func TestNamespaceToolsFlattenToTopLevelFunctions(t *testing.T) {
	in := FromOpenAIResponses(namespaceRequestBody())
	if in.Err != nil {
		t.Fatalf("Err = %v, want nil", in.Err)
	}
	tools, _ := in.Tools.([]any)
	names := map[string]bool{}
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		names[getString(tool, "name")] = true
	}
	for _, want := range []string{
		"update_plan",
		"multi_agent_v1__close_agent",
		"multi_agent_v1__spawn_agent",
	} {
		if !names[want] {
			t.Errorf("缺少工具 %q，实际: %v", want, names)
		}
	}
	if len(tools) != 3 {
		t.Errorf("工具数 = %d, want 3（web_search 应被丢弃）: %v", len(tools), names)
	}
	// 子工具的元数据要跟着提上来，否则模型不知道这个工具干什么。
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if getString(tool, "name") == "multi_agent_v1__close_agent" {
			if getString(tool, "description") != "Close an agent." {
				t.Errorf("子工具描述丢失: %#v", tool)
			}
		}
	}
}

// 还原映射要能把扁平名反解回 {namespace, 裸名}；顶层工具不进映射。
func TestNamespaceRestoreMapInvertsFlatten(t *testing.T) {
	in := FromOpenAIResponses(namespaceRequestBody())
	entry, exists := in.ToolNamespaces["multi_agent_v1__close_agent"]
	if !exists {
		t.Fatalf("映射缺少扁平名，实际键: %v", sortedFlatNames(in.ToolNamespaces))
	}
	if entry.Namespace != "multi_agent_v1" || entry.Name != "close_agent" {
		t.Fatalf("映射项 = %#v", entry)
	}
	if _, exists := in.ToolNamespaces["update_plan"]; exists {
		t.Error("顶层工具不该进还原映射")
	}
}

// 回放历史里的 function_call 要改写成扁平名：上游收到的是展平后的工具列表。
func TestNamespacedHistoryCallsRewrittenToFlatName(t *testing.T) {
	in := FromOpenAIResponses(namespaceRequestBody())
	body, err := ToAnthropicBodyChecked(in, "claude-upstream")
	if err != nil {
		t.Fatalf("转 Anthropic 失败: %v", err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "multi_agent_v1__close_agent") {
		t.Fatalf("历史调用未改写成扁平名: %s", encoded)
	}
	// 裸名不能残留成独立的 tool_use name，否则上游对不上工具声明。
	var parsed map[string]any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	messages, _ := parsed["messages"].([]any)
	for _, m := range messages {
		message, _ := m.(map[string]any)
		blocks, _ := message["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if getString(block, "type") != "tool_use" {
				continue
			}
			if name := getString(block, "name"); name == "close_agent" {
				t.Errorf("tool_use 仍是裸名 %q", name)
			}
		}
	}
}

// namespace 类型的 tool_choice 在展平后无法表达，降级为 auto 而不是让请求失败。
func TestNamespaceToolChoiceDegradesToAuto(t *testing.T) {
	in := FromOpenAIResponses(namespaceRequestBody())

	anthropicBody, err := ToAnthropicBodyChecked(in, "claude-upstream")
	if err != nil {
		t.Fatalf("转 Anthropic 失败: %v", err)
	}
	choice, _ := anthropicBody["tool_choice"].(map[string]any)
	if getString(choice, "type") != "auto" {
		t.Errorf("Anthropic tool_choice = %#v, want type=auto", anthropicBody["tool_choice"])
	}

	responsesBody, err := ToOpenAIResponsesBodyChecked(in, "gpt-upstream")
	if err != nil {
		t.Fatalf("转 Responses 失败: %v", err)
	}
	if responsesBody["tool_choice"] != "auto" {
		t.Errorf("Responses tool_choice = %#v, want \"auto\"", responsesBody["tool_choice"])
	}
}

// 扁平名撞车必须显式报错：上游无法区分同名工具，丢掉哪个都会让模型调用落空。
func TestNamespaceFlatNameCollisionIsRejected(t *testing.T) {
	t.Run("撞上顶层工具", func(t *testing.T) {
		in := FromOpenAIResponses(map[string]any{"tools": []any{
			map[string]any{"type": "function", "name": "ns__read"},
			map[string]any{"type": "namespace", "name": "ns", "tools": []any{
				map[string]any{"type": "function", "name": "read"},
			}},
		}})
		if in.Err == nil || !strings.Contains(in.Err.Error(), "冲突") {
			t.Fatalf("Err = %v, want 冲突错误", in.Err)
		}
	})

	t.Run("两个子工具互撞", func(t *testing.T) {
		// "a__b" + "c" 与 "a" + "b__c" 都展平成 "a__b__c"
		in := FromOpenAIResponses(map[string]any{"tools": []any{
			map[string]any{"type": "namespace", "name": "a__b", "tools": []any{
				map[string]any{"type": "function", "name": "c"},
			}},
			map[string]any{"type": "namespace", "name": "a", "tools": []any{
				map[string]any{"type": "function", "name": "b__c"},
			}},
		}})
		if in.Err == nil || !strings.Contains(in.Err.Error(), "同为") {
			t.Fatalf("Err = %v, want 同名冲突错误", in.Err)
		}
	})
}

// 超长名字要截断到 64 以内（Anthropic/OpenAI 的工具名上限），且展平与还原必须
// 算出同一个名字，否则响应侧反解不回来。
func TestNamespaceLongNameTruncationStaysConsistent(t *testing.T) {
	longChild := strings.Repeat("a", 80)
	body := map[string]any{"tools": []any{
		map[string]any{"type": "namespace", "name": "mcp__srv__", "tools": []any{
			map[string]any{"type": "function", "name": longChild},
		}},
	}}
	in := FromOpenAIResponses(body)
	if in.Err != nil {
		t.Fatalf("Err = %v", in.Err)
	}
	tools, _ := in.Tools.([]any)
	tool, _ := tools[0].(map[string]any)
	flat := getString(tool, "name")
	if len(flat) > toolNameMaxLen {
		t.Fatalf("扁平名长度 %d 超过上限 %d: %q", len(flat), toolNameMaxLen, flat)
	}
	entry, exists := in.ToolNamespaces[flat]
	if !exists {
		t.Fatalf("截断后的名字 %q 不在还原映射里: %v", flat, sortedFlatNames(in.ToolNamespaces))
	}
	if entry.Name != longChild {
		t.Fatalf("还原项 = %#v", entry)
	}
}

// 非流式响应侧还原：上游按扁平名返回，客户端要看到 {name, namespace}。
func TestRestoreNamespacedCallsRoundTrip(t *testing.T) {
	in := FromOpenAIResponses(namespaceRequestBody())
	response := map[string]any{
		"object": "response",
		"output": []any{
			map[string]any{
				"type": "function_call", "name": "multi_agent_v1__close_agent",
				"call_id": "call_2", "arguments": "{}",
			},
			map[string]any{
				"type": "function_call", "name": "update_plan",
				"call_id": "call_3", "arguments": "{}",
			},
		},
	}
	if !RestoreNamespacedCalls(response, in.ToolNamespaces) {
		t.Fatal("应报告发生了还原")
	}
	output, _ := response["output"].([]any)
	first, _ := output[0].(map[string]any)
	if getString(first, "name") != "close_agent" || getString(first, "namespace") != "multi_agent_v1" {
		t.Errorf("还原后 = %#v", first)
	}
	// 顶层工具不在映射里，必须原样不动，尤其不能凭空加 namespace 字段。
	second, _ := output[1].(map[string]any)
	if getString(second, "name") != "update_plan" {
		t.Errorf("顶层工具名被改动: %#v", second)
	}
	if _, exists := second["namespace"]; exists {
		t.Errorf("顶层工具被塞了 namespace: %#v", second)
	}
}

// 流式还原：装饰器要覆盖跨格式转换器与同格式透传两条路径。
func TestNamespaceRestoreWrapsStreamTransformer(t *testing.T) {
	owners := map[string]NamespacedName{
		"multi_agent_v1__close_agent": {Namespace: "multi_agent_v1", Name: "close_agent"},
	}

	t.Run("透传路径", func(t *testing.T) {
		// 同格式（responses→responses）拿到的是 passthrough，装饰器必须照样生效，
		// 否则 Codex 配 Responses 上游时完全收不到还原。
		inner := NewStreamTransformer("openai-responses", "openai-responses")
		transformer := WithNamespaceRestore(inner, owners)
		out := transformAll(t, transformer,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"multi_agent_v1__close_agent","call_id":"c1","arguments":"{}"}}`,
		)
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"name":"close_agent"`) {
			t.Fatalf("裸名未还原: %s", joined)
		}
		if !strings.Contains(joined, `"namespace":"multi_agent_v1"`) {
			t.Fatalf("namespace 字段未补上: %s", joined)
		}
	})

	t.Run("跨格式转换路径", func(t *testing.T) {
		// anthropic 上游按扁平名返回 tool_use，转成 Responses 的 function_call
		// 之后仍需还原。
		inner := NewStreamTransformer("anthropic", "openai-responses")
		transformer := WithNamespaceRestore(inner, owners)
		out := transformAll(t, transformer,
			`data: {"type":"message_start","message":{"model":"claude-upstream"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"multi_agent_v1__close_agent","input":{}}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`data: {"type":"message_stop"}`,
		)
		joined := strings.Join(out, "")
		if !strings.Contains(joined, `"name":"close_agent"`) {
			t.Fatalf("裸名未还原: %s", joined)
		}
		if !strings.Contains(joined, `"namespace":"multi_agent_v1"`) {
			t.Fatalf("namespace 字段未补上: %s", joined)
		}
	})

	t.Run("无关事件不受影响", func(t *testing.T) {
		inner := NewStreamTransformer("openai-responses", "openai-responses")
		transformer := WithNamespaceRestore(inner, owners)
		line := `data: {"type":"response.output_text.delta","delta":"普通文本"}`
		out := transformAll(t, transformer, line)
		if len(out) != 1 || strings.TrimSpace(out[0]) != line {
			t.Fatalf("无关事件被改动: %#v", out)
		}
	})

	t.Run("空映射返回原对象", func(t *testing.T) {
		inner := NewStreamTransformer("openai-responses", "openai-responses")
		if WithNamespaceRestore(inner, nil) != inner {
			t.Fatal("空映射应原样返回 inner，不引入额外开销")
		}
	})
}

// 装饰器必须转发内层的可选接口，否则 proxy 的错误终态、完成判定、中止全部失效。
func TestNamespaceRestorerForwardsOptionalInterfaces(t *testing.T) {
	owners := map[string]NamespacedName{
		"ns__t": {Namespace: "ns", Name: "t"},
	}
	inner := NewStreamTransformer("anthropic", "openai-responses")
	transformer := WithNamespaceRestore(inner, owners)

	// 喂一个不支持的块类型，让内层进入错误态
	transformAll(t, transformer,
		`data: {"type":"message_start","message":{"model":"m"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1"}}`,
	)
	if StreamError(transformer) == nil {
		t.Fatal("Err() 未透传内层错误")
	}
	if len(StreamFailure(transformer)) == 0 {
		t.Fatal("Failure() 未透传内层失败终态")
	}
	if StreamCompleted(transformer) {
		t.Fatal("Completed() 应为 false")
	}
}

func TestRestoreNamespacedCallsNoopWithoutMap(t *testing.T) {
	response := map[string]any{"output": []any{
		map[string]any{"type": "function_call", "name": "multi_agent_v1__close_agent"},
	}}
	if RestoreNamespacedCalls(response, nil) {
		t.Fatal("空映射不该改动任何内容")
	}
}
