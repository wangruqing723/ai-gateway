package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func transferTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := DecodeAndValidate([]byte(validConfigYAML()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = "/tmp/fake-config.yaml"
	cfg.Providers["primary"].APIKey = "fake-existing-secret"
	return cfg
}

func transferTestProvider(secret string) *Provider {
	return &Provider{BaseURL: "https://example.invalid/v1", Format: "openai", APIKey: secret, MaxConcurrent: 5, MaxQueueWait: 30000}
}

func transferTestBundle(t *testing.T, providers map[string]*Provider, routes []Route) []byte {
	t.Helper()
	if providers == nil {
		providers = map[string]*Provider{}
	}
	if routes == nil {
		routes = []Route{}
	}
	data, err := yaml.Marshal(transferBundle{Kind: TransferKind, Version: TransferVersion, Providers: providers, Routes: routes})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func transferHasIssue(issues []TransferIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestTransferExportIncludesAllRouteDependenciesAndSecrets(t *testing.T) {
	current := transferTestConfig(t)
	current.Providers["secondary"] = transferTestProvider("")
	current.Providers["secondary"].APIKeys = []ProviderKey{{ID: "b", Value: "fake-second-key", Enabled: boolPtr(false)}, {ID: "a", Value: "fake-first-key"}}
	current.Providers["vision"] = transferTestProvider("fake-vision-key")
	current.Providers["unused"] = transferTestProvider("fake-unused-key")
	current.Providers["primary"].Proxy = "http://u:fake-proxy-password@proxy.invalid:8080"
	current.Providers["primary"].ExtraHeaders = map[string]string{"X-Private": "fake-private-header"}
	current.Providers["primary"].ExtraBody = map[string]any{"private": "fake-private-body"}
	current.Routes = []Route{{Match: "selected-*", Targets: []Target{{Provider: "secondary", Model: "model-b"}, {Provider: "primary", Model: "model-a"}}, Vision: &Vision{Provider: "vision", Model: "vision"}, Enabled: boolPtr(false)}, {Match: "other-*", Provider: "unused", Model: "other"}}
	before, _ := yaml.Marshal(current)
	data, err := ExportTransfer(current, TransferExportRequest{Scope: "selected", Selection: &TransferSelection{Providers: []string{}, Routes: []int{0}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fake-existing-secret", "fake-second-key", "fake-first-key", "fake-proxy-password", "fake-private-header", "fake-private-body"} {
		if !bytes.Contains(data, []byte(secret)) {
			t.Errorf("导出未保留假凭据 %s", secret)
		}
	}
	if bytes.Contains(data, []byte("fake-unused-key")) || bytes.Contains(data, []byte("host:")) || bytes.Contains(data, []byte("cooldownUntil")) {
		t.Fatal("导出包含范围外配置")
	}
	document, err := decodeTransfer(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.config.Providers) != 3 || len(document.config.Routes) != 1 {
		t.Fatalf("依赖闭包不完整：%#v", document.config)
	}
	route := document.config.Routes[0]
	if route.IsEnabled() || route.Targets[0].Provider != "secondary" || route.Targets[1].Provider != "primary" {
		t.Fatal("路由启停或候选顺序改变")
	}
	keys := document.config.Providers["secondary"].APIKeys
	if keys[0].ID != "b" || keys[0].IsEnabled() || keys[1].ID != "a" {
		t.Fatal("密钥顺序或启停改变")
	}
	after, _ := yaml.Marshal(current)
	if !bytes.Equal(before, after) {
		t.Fatal("导出修改了源配置")
	}
}

func TestTransferExportProviderOnlyDoesNotIncludeRoutes(t *testing.T) {
	current := transferTestConfig(t)
	data, err := ExportTransfer(current, TransferExportRequest{Scope: "selected", Selection: &TransferSelection{Providers: []string{"primary"}, Routes: []int{}}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeTransfer(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.config.Routes) != 0 || len(doc.config.Providers) != 1 {
		t.Fatal("提供商选择反向扩展了路由")
	}
}

func TestTransferExportRejectsDuplicateRoutesOnlyWithinSelection(t *testing.T) {
	current := transferTestConfig(t)
	current.Routes = []Route{{Match: "Claude-*", Provider: "primary", Model: "m"}, {Match: "claude-*", Provider: "primary", Model: "m", Enabled: boolPtr(false)}}
	_, err := ExportTransfer(current, TransferExportRequest{Scope: "all"})
	var issue *TransferError
	if !errors.As(err, &issue) || issue.Code != "duplicate_route_match" {
		t.Fatalf("未拒绝重复：%v", err)
	}
	for _, selection := range []*TransferSelection{{Providers: []string{}, Routes: []int{0}}, {Providers: []string{"primary"}, Routes: []int{}}} {
		if _, err := ExportTransfer(current, TransferExportRequest{Scope: "selected", Selection: selection}); err != nil {
			t.Fatalf("合法子集被拒绝：%v", err)
		}
	}
}

func TestTransferMergeKeepsConflictsAndOverwritesWholeProvider(t *testing.T) {
	current := transferTestConfig(t)
	current.Providers["primary"].APIKey = ""
	current.Providers["primary"].APIKeys = []ProviderKey{{ID: "old", Value: "fake-old-key"}}
	sourceProvider := transferTestProvider("fake-new-single-key")
	content := transferTestBundle(t, map[string]*Provider{"primary": sourceProvider, "new": transferTestProvider("fake-new-provider")}, []Route{{Match: "*", Provider: "primary", Model: "replacement"}, {Match: "new-*", Provider: "new", Model: "new-model"}})
	plan, err := PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CanApply || plan.Config.Providers["primary"].APIKeys[0].ID != "old" || plan.Config.Routes[0].Model != "upstream-model" {
		t.Fatal("默认合并未保留目标冲突")
	}
	if plan.Summary.Providers.Keep != 1 || plan.Summary.Routes.Keep != 1 || !transferHasIssue(plan.Warnings, "route_shadowed") {
		t.Fatalf("保留或遮挡预览不正确：%#v", plan)
	}
	options := TransferOptions{Conflicts: &TransferConflicts{Providers: map[string]string{"primary": "overwrite"}, Routes: map[string]string{"0": "overwrite"}}, InsertNewRoutes: "before"}
	plan, err = PlanTransfer(current, content, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Config.Providers["primary"].APIKey != "fake-new-single-key" || len(plan.Config.Providers["primary"].APIKeys) != 0 {
		t.Fatal("提供商覆盖拼接了旧凭据")
	}
	if plan.Config.Routes[0].Match != "new-*" || plan.Config.Routes[1].Model != "replacement" {
		t.Fatal("新增前插或原位覆盖失败")
	}
	if plan.FinalRouteOrder[0].Match != plan.Config.Routes[0].Match || plan.FinalRouteOrder[1].Source != "overwritten" {
		t.Fatal("预览顺序与实际计划不符")
	}
	if current.Providers["primary"].APIKeys[0].Value != "fake-old-key" || current.Routes[0].Model != "upstream-model" {
		t.Fatal("规划修改了目标")
	}
}

func TestTransferMergeOrderAndCaseSensitiveProviderNames(t *testing.T) {
	current := transferTestConfig(t)
	current.Routes = []Route{{Match: "One-*", Provider: "primary", Model: "one"}, {Match: "two-*", Provider: "primary", Model: "two"}}
	content := transferTestBundle(t, map[string]*Provider{"Primary": transferTestProvider("fake-uppercase")}, []Route{{Match: "ONE-*", Provider: "primary", Model: "updated"}, {Match: "new-1", Provider: "Primary", Model: "m1"}, {Match: "new-2", Provider: "Primary", Model: "m2"}})
	for _, placement := range []string{"before", "after"} {
		t.Run(placement, func(t *testing.T) {
			plan, err := PlanTransfer(current, content, TransferOptions{InsertNewRoutes: placement, Conflicts: &TransferConflicts{Routes: map[string]string{"0": "overwrite"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.CanApply || len(plan.Config.Providers) != 2 {
				t.Fatal("提供商名称被忽略大小写合并")
			}
			want := []string{"ONE-*", "two-*", "new-1", "new-2"}
			if placement == "before" {
				want = []string{"new-1", "new-2", "ONE-*", "two-*"}
			}
			for index, match := range want {
				if plan.Config.Routes[index].Match != match || plan.FinalRouteOrder[index].Position != index {
					t.Fatal("合并路由位置不正确")
				}
			}
		})
	}
}

func TestTransferReplacePreservesGlobalsAndPointerSemantics(t *testing.T) {
	current := transferTestConfig(t)
	current.Timeout = 45678
	current.DirectMode = true
	current.ContextSafetyMargin = intPtr(0)
	current.Failover.MaxRetryAfterMs = intPtr(0)
	current.ProviderHealth.CheckIntervalSeconds = 42
	provider := transferTestProvider("")
	provider.Enabled = boolPtr(false)
	provider.LocalRetry = &LocalRetry{MaxRetries: intPtr(0), IntervalMs: intPtr(0)}
	provider.APIKeys = []ProviderKey{{ID: "b", Value: "fake-b", Enabled: boolPtr(false)}, {ID: "a", Value: "fake-a"}}
	provider.KeyCooldown = &KeyCooldown{AuthMs: intPtr(0)}
	content := transferTestBundle(t, map[string]*Provider{"new": provider}, []Route{{Match: "new-*", Provider: "new", Model: "new-model", Enabled: boolPtr(false)}})
	plan, err := PlanTransfer(current, content, TransferOptions{Mode: "replace"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CanApply || len(plan.Config.Providers) != 1 || plan.Config.Providers["primary"] != nil || len(plan.Config.Routes) != 1 {
		t.Fatal("整体替换未移除旧集合")
	}
	if plan.Config.Timeout != current.Timeout || plan.Config.DirectMode != current.DirectMode || plan.Config.Path != current.Path || *plan.Config.ContextSafetyMargin != 0 || *plan.Config.Failover.MaxRetryAfterMs != 0 || plan.Config.ProviderHealth != current.ProviderHealth {
		t.Fatal("全局或路径被覆盖")
	}
	got := plan.Config.Providers["new"]
	if got.IsEnabled() || got.MaxTokens != nil || got.LocalRetry.MaxRetries == nil || *got.LocalRetry.MaxRetries != 0 || got.KeyCooldown.AuthMs == nil || *got.KeyCooldown.AuthMs != 0 || got.APIKeys[0].ID != "b" || got.APIKeys[0].IsEnabled() {
		t.Fatal("未配置／显式零／false／密钥顺序不保真")
	}
	if plan.Summary.Providers.Delete != 1 || plan.Summary.Routes.Delete != 1 {
		t.Fatal("删除摘要缺失")
	}
}

func TestTransferFragmentsAndActualDependencies(t *testing.T) {
	current := transferTestConfig(t)
	routeOnly := transferTestBundle(t, nil, []Route{{Match: "new-*", Provider: "primary", Model: "m"}})
	plan, err := PlanTransfer(current, routeOnly, TransferOptions{}, nil)
	if err != nil || !plan.CanApply || plan.CanReplace {
		t.Fatalf("路由片段不能复用已有依赖：%v %#v", err, plan)
	}
	plan, err = PlanTransfer(current, routeOnly, TransferOptions{Mode: "replace"}, nil)
	if err != nil || plan.CanApply || !transferHasIssue(plan.Blockers, "replace_requires_complete_bundle") {
		t.Fatalf("片段可整体替换：%v %#v", err, plan)
	}
	missing := transferTestBundle(t, nil, []Route{{Match: "missing-*", Provider: "missing", Model: "m", Vision: &Vision{Provider: "vision", Model: "v"}}})
	plan, err = PlanTransfer(current, missing, TransferOptions{}, nil)
	if err != nil || plan.CanApply || !transferHasIssue(plan.Blockers, "missing_provider_reference") {
		t.Fatalf("悬挂引用未拦截：%v %#v", err, plan)
	}
}

func TestTransferOAuthMappingOnlyForAppliedProviders(t *testing.T) {
	current := transferTestConfig(t)
	oauth := func() *Provider {
		p := transferTestProvider("")
		p.AuthType = "chatgptOAuth"
		p.AccountID = "acc_source"
		p.Format = "openai-responses"
		p.BaseURL = "https://chatgpt.com/backend-api/codex"
		p.Enabled = boolPtr(false)
		return p
	}
	content := transferTestBundle(t, map[string]*Provider{"primary": oauth(), "oauth-a": oauth(), "oauth-b": oauth()}, nil)
	selection := &TransferSelection{Providers: []string{"primary"}, Routes: []int{}}
	plan, err := PlanTransfer(current, content, TransferOptions{Selection: selection}, nil)
	if err != nil || plan.CanApply || len(plan.Blockers) != 0 {
		t.Fatalf("保留冲突被未用源账户阻断：%v %#v", err, plan)
	}
	selection.Providers = []string{"oauth-a", "oauth-b"}
	plan, err = PlanTransfer(current, content, TransferOptions{Selection: selection}, map[string]bool{"acc_target": true})
	if err != nil || plan.CanApply || !transferHasIssue(plan.Blockers, "unresolved_account_mapping") {
		t.Fatal("缺少映射未阻断")
	}
	options := TransferOptions{Selection: selection, AccountMappings: map[string]string{"acc_source": "acc_target"}}
	plan, err = PlanTransfer(current, content, options, map[string]bool{"acc_target": true})
	if err != nil || !plan.CanApply {
		t.Fatalf("映射无法应用：%v %#v", err, plan)
	}
	for _, name := range selection.Providers {
		if plan.Config.Providers[name].AccountID != "acc_target" || plan.Config.Providers[name].IsEnabled() {
			t.Fatal("共享映射或停用状态错误")
		}
	}
	if plan.Catalog.Providers[0].SourceAccountID != "acc_source" {
		t.Fatal("源目录账户被改写成目标账户")
	}
	if !bytes.Contains(content, []byte("acc_source")) {
		t.Fatal("规划修改源字节")
	}
}

func TestTransferTargetLegacyDuplicates(t *testing.T) {
	current := transferTestConfig(t)
	current.Routes = []Route{{Match: "dup-*", Provider: "primary", Model: "a"}, {Match: "DUP-*", Provider: "primary", Model: "b"}}
	content := transferTestBundle(t, nil, []Route{{Match: "new-*", Provider: "primary", Model: "c"}})
	plan, err := PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil || !plan.CanApply || len(plan.Config.Routes) != 3 {
		t.Fatal("不涉及的遗留重复被拒绝或被删")
	}
	content = transferTestBundle(t, nil, []Route{{Match: "dup-*", Provider: "primary", Model: "c"}})
	plan, err = PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil || plan.CanApply || !transferHasIssue(plan.Blockers, "ambiguous_target_route") {
		t.Fatal("重复目标被猜测覆盖")
	}
	content = transferTestBundle(t, map[string]*Provider{"primary": transferTestProvider("fake-replacement")}, []Route{{Match: "dup-*", Provider: "primary", Model: "c"}})
	plan, err = PlanTransfer(current, content, TransferOptions{Mode: "replace"}, nil)
	if err != nil || !plan.CanApply || len(plan.Config.Routes) != 1 || plan.Summary.Routes.Delete != 1 {
		t.Fatalf("替换删除摘要遗漏遗留重复项：%v %#v", err, plan)
	}
}

func TestTransferRejectsFileErrorsEvenWhenUnselected(t *testing.T) {
	current := transferTestConfig(t)
	base := `providers:
  new:
    baseUrl: https://example.invalid
    format: openai
    apiKey: fake-secret-never-echo
routes:
  - match: new-*
    provider: new
    model: m
`
	tests := []struct{ name, content string }{
		{"unknown", base + "private-never-echo: fake-secret-never-echo\n"},
		{"multiple", base + "---\nproviders: {}\n"},
		{"duplicate-key", strings.Replace(base, "    format: openai", "    format: openai\n    format: anthropic", 1)},
		{"scalar-type", strings.Replace(base, "apiKey: fake-secret-never-echo", "apiKey: 123", 1)},
		{"bool-string", strings.Replace(base, "    format: openai", "    format: openai\n    enabled: \"false\"", 1)},
		{"invalid-number", strings.Replace(base, "    format: openai", "    format: openai\n    maxTokens: 0", 1)},
		{"sentinel", strings.Replace(base, "fake-secret-never-echo", APIKeyKeepSentinel, 1)},
		{"proxy-sentinel", strings.Replace(base, "    format: openai", "    format: openai\n    proxy: http://u:"+ProxyPasswordKeepSentinel+"@proxy.invalid", 1)},
		{"null-provider", "providers:\n  new: null\nroutes: []\n"},
		{"duplicate-route", base + "  - match: NEW-*\n    provider: new\n    model: m2\n    enabled: false\n"},
		{"private-header-value", strings.Replace(base, "    format: openai", "    format: openai\n    extraHeaders:\n      X-Private: [fake-secret-never-echo]", 1)},
		{"nested-body-non-json", strings.Replace(base, "    format: openai", "    format: openai\n    extraBody:\n      nested: {1: fake-secret-never-echo}", 1)},
		{"parser-error", base + "[fake-secret-never-echo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanTransfer(current, []byte(test.content), TransferOptions{}, nil)
			if err == nil {
				t.Fatal("非法文件未拒绝")
			}
			if strings.Contains(err.Error(), "fake-secret-never-echo") {
				t.Fatal("错误泄露凭据")
			}
		})
	}
	invalid := transferTestProvider("fake-unselected")
	invalid.MaxTokens = intPtr(0)
	content := transferTestBundle(t, map[string]*Provider{"valid": transferTestProvider("fake-ok"), "invalid": invalid}, nil)
	_, err := PlanTransfer(current, content, TransferOptions{Selection: &TransferSelection{Providers: []string{"valid"}, Routes: []int{}}}, nil)
	if err == nil {
		t.Fatal("取消勾选绕过了非法数值")
	}
}

func TestTransferOriginalConfigIgnoresGlobalValues(t *testing.T) {
	current := transferTestConfig(t)
	raw := `host: other.invalid
port: 9999
timeout: -123
failover:
  maxAttempts: 0
providers:
  imported:
    baseUrl: https://example.invalid
    format: openai
    apiKey: ""
routes: []
`
	plan, err := PlanTransfer(current, []byte(raw), TransferOptions{}, nil)
	if err != nil || !plan.CanApply {
		t.Fatalf("全局忽略失败：%v %#v", err, plan)
	}
	if plan.Config.Host != current.Host || plan.Config.Port != current.Port || plan.Config.Timeout != current.Timeout || plan.Config.Failover.AttemptLimit() != current.Failover.AttemptLimit() {
		t.Fatal("源全局改变了目标")
	}
	if !reflect.DeepEqual(plan.IgnoredGlobalFields, []string{"failover", "host", "port", "timeout"}) || plan.SourceFormat != "config-yaml" {
		t.Fatal("忽略字段目录错误")
	}
}

func TestTransferPreviewDoesNotSerializeConfigValues(t *testing.T) {
	current := transferTestConfig(t)
	provider := transferTestProvider("fake-raw-secret-never-preview")
	provider.Proxy = "http://u:fake-proxy-never-preview@proxy.invalid"
	provider.ExtraHeaders = map[string]string{"X-Fake-Private-Key": "fake-header-never-preview"}
	provider.ExtraBody = map[string]any{"nested": map[string]any{"private": "fake-body-never-preview"}}
	content := transferTestBundle(t, map[string]*Provider{"new": provider}, []Route{{Match: "new-*", Provider: "new", Model: "fake-model-never-preview"}})
	plan, err := PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil || !plan.CanApply {
		t.Fatalf("计划失败：%v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fake-raw-secret-never-preview", "fake-proxy-never-preview", "fake-header-never-preview", "fake-body-never-preview", "fake-model-never-preview", "X-Fake-Private-Key", "https://example.invalid"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("预览包含私有原值：%s", forbidden)
		}
	}
}

func TestTransferKeepsUnrelatedUnboundOAuthProvider(t *testing.T) {
	current := transferTestConfig(t)
	provider := transferTestProvider("")
	provider.AuthType = "chatgptOAuth"
	provider.Format = "openai-responses"
	provider.BaseURL = "https://chatgpt.com/backend-api/codex"
	provider.Enabled = boolPtr(false)
	current.Providers["unbound"] = provider
	content := transferTestBundle(t, map[string]*Provider{"new": transferTestProvider("fake-new")}, nil)
	plan, err := PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil || !plan.CanApply || plan.Config.Providers["unbound"].AccountID != "" {
		t.Fatalf("无关的停用 OAuth 配置被阻断：%v %#v", err, plan)
	}
}

func TestTransferSupportsYAMLAnchorsWithoutImplicitScalarConversion(t *testing.T) {
	current := transferTestConfig(t)
	content := `providers:
  a: &provider
    baseUrl: https://example.invalid
    format: openai
    apiKey: fake-anchor-secret
  b:
    <<: *provider
routes: []
`
	plan, err := PlanTransfer(current, []byte(content), TransferOptions{}, nil)
	if err != nil || !plan.CanApply || plan.Config.Providers["b"].APIKey != "fake-anchor-secret" {
		t.Fatalf("合法锚点被拒绝：%v", err)
	}
	bad := `providers:
  recursive: &provider
    baseUrl: https://example.invalid
    format: openai
    extraBody:
      private: &cycle [*cycle]
routes: []
`
	if _, err := PlanTransfer(current, []byte(bad), TransferOptions{}, nil); err == nil {
		t.Fatal("循环别名未拒绝")
	}
}

func TestTransferNormalizesEquivalentSelectionAndConflictDefaults(t *testing.T) {
	current := transferTestConfig(t)
	content := transferTestBundle(t, map[string]*Provider{"primary": transferTestProvider("fake-source")}, []Route{{Match: "*", Provider: "primary", Model: "new-model"}})
	first, err := PlanTransfer(current, content, TransferOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanTransfer(current, content, TransferOptions{Selection: &TransferSelection{Providers: []string{"primary"}, Routes: []int{0}}, Conflicts: &TransferConflicts{Providers: map[string]string{"primary": "keep"}, Routes: map[string]string{"0": "keep"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := TransferOptionsJSON(first.Options)
	b, _ := TransferOptionsJSON(second.Options)
	if !bytes.Equal(a, b) {
		t.Fatalf("等价默认选项未规范化：%s %s", a, b)
	}
}

func TestTransferSizeAndSelections(t *testing.T) {
	current := transferTestConfig(t)
	if _, err := PlanTransfer(current, bytes.Repeat([]byte(" "), MaxTransferBytes+1), TransferOptions{}, nil); err == nil {
		t.Fatal("源大小未限制")
	}
	provider := transferTestProvider(strings.Repeat("x", MaxTransferBytes))
	content := transferTestBundle(t, map[string]*Provider{"new": provider}, nil)
	if _, err := PlanTransfer(current, content, TransferOptions{}, nil); err == nil {
		t.Fatal("超大源未拒绝")
	}
	// 源片段很小，但已保存目标接近上限；最终完整配置必须再次测量。
	current.Providers["primary"].APIKey = strings.Repeat("x", MaxTransferBytes)
	small := transferTestBundle(t, map[string]*Provider{"new": transferTestProvider("fake-small")}, nil)
	_, err := PlanTransfer(current, small, TransferOptions{}, nil)
	var issue *TransferError
	if !errors.As(err, &issue) || issue.Code != "config_too_large" {
		t.Fatalf("完整配置大小未限制：%v", err)
	}
	for index, selection := range []*TransferSelection{{Providers: nil, Routes: []int{}}, {Providers: []string{}, Routes: []int{}}, {Providers: []string{"missing"}, Routes: []int{}}, {Providers: []string{"primary", "primary"}, Routes: []int{}}, {Providers: []string{}, Routes: []int{-1}}, {Providers: []string{}, Routes: []int{1}}, {Providers: []string{}, Routes: []int{0, 0}}} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			if _, err := ExportTransfer(current, TransferExportRequest{Scope: "selected", Selection: selection}); err == nil {
				t.Fatal("非法选择未拒绝")
			}
		})
	}
}
