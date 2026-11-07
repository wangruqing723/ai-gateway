package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	TransferKind     = "ai-gateway.provider-route-transfer"
	TransferVersion  = 1
	MaxTransferBytes = 1 << 20
)

// TransferError 只携带安全说明，不能保留会回显凭据的原始解析错误。
type TransferError struct {
	Code    string
	Message string
	Field   string
	Line    int
}

func (e *TransferError) Error() string { return e.Message }

func transferError(code, message, field string) error {
	return &TransferError{Code: code, Message: message, Field: field}
}

// TransferSelection 的路由下标属于源配置，不属于筛选后的页面行号。
type TransferSelection struct {
	Providers []string `json:"providers"`
	Routes    []int    `json:"routes"`
}

type TransferExportRequest struct {
	Scope     string             `json:"scope"`
	Selection *TransferSelection `json:"selection,omitempty"`
}

type TransferConflicts struct {
	Providers map[string]string `json:"providers,omitempty"`
	Routes    map[string]string `json:"routes,omitempty"`
}

type TransferOptions struct {
	Mode            string             `json:"mode,omitempty"`
	Selection       *TransferSelection `json:"selection,omitempty"`
	Conflicts       *TransferConflicts `json:"conflicts,omitempty"`
	InsertNewRoutes string             `json:"insertNewRoutes,omitempty"`
	AccountMappings map[string]string  `json:"accountMappings,omitempty"`
}

type transferBundle struct {
	Kind      string               `yaml:"kind"`
	Version   int                  `yaml:"version"`
	Providers map[string]*Provider `yaml:"providers"`
	Routes    []Route              `yaml:"routes"`
}

type transferDocument struct {
	config  *Config
	format  string
	ignored []string
}

type TransferIssue struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Provider   string `json:"provider,omitempty"`
	RouteIndex *int   `json:"routeIndex,omitempty"`
	Field      string `json:"field,omitempty"`
}

type TransferRouteIdentity struct {
	Index int    `json:"index"`
	Match string `json:"match"`
}

type TransferProviderEntry struct {
	BalanceTokenConfigured bool   `json:"balanceTokenConfigured"`
	Name                   string `json:"name"`
	KeyCount               int    `json:"keyCount"`
	APIKeyConfigured       bool   `json:"apiKeyConfigured"`
	SourceAccountID        string `json:"sourceAccountId,omitempty"`
	RouteIndexes           []int  `json:"routeIndexes"`
	Selected               bool   `json:"selected"`
	Conflict               bool   `json:"conflict"`
}

type TransferRouteEntry struct {
	Index                int      `json:"index"`
	Match                string   `json:"match"`
	ProviderDependencies []string `json:"providerDependencies"`
	Selected             bool     `json:"selected"`
	TargetIndexes        []int    `json:"targetIndexes"`
}

type TransferCatalog struct {
	Providers        []TransferProviderEntry `json:"providers"`
	Routes           []TransferRouteEntry    `json:"routes"`
	SourceAccountIDs []string                `json:"sourceAccountIds"`
}

type TransferCounts struct {
	Add       int `json:"add"`
	Overwrite int `json:"overwrite"`
	Delete    int `json:"delete"`
	Keep      int `json:"keep"`
	Unchanged int `json:"unchanged"`
}

func (c *TransferCounts) count(action string) {
	switch action {
	case "add":
		c.Add++
	case "overwrite":
		c.Overwrite++
	case "delete":
		c.Delete++
	case "keep":
		c.Keep++
	case "unchanged":
		c.Unchanged++
	}
}

type TransferSummary struct {
	Providers TransferCounts `json:"providers"`
	Routes    TransferCounts `json:"routes"`
}

type TransferProviderChange struct {
	Name           string                  `json:"name"`
	Action         string                  `json:"action"`
	ChangedFields  []string                `json:"changedFields"`
	BeforeKeyCount int                     `json:"beforeKeyCount"`
	AfterKeyCount  int                     `json:"afterKeyCount"`
	AffectedRoutes []TransferRouteIdentity `json:"affectedRoutes"`
}

type TransferRouteChange struct {
	SourceIndex          *int     `json:"sourceIndex"`
	TargetIndex          *int     `json:"targetIndex"`
	Match                string   `json:"match"`
	Action               string   `json:"action"`
	ChangedFields        []string `json:"changedFields"`
	ProviderDependencies []string `json:"providerDependencies"`
}

type TransferChanges struct {
	Providers []TransferProviderChange `json:"providers"`
	Routes    []TransferRouteChange    `json:"routes"`
}

type TransferRoutePosition struct {
	Position int    `json:"position"`
	Match    string `json:"match"`
	Source   string `json:"source"`
	Index    int    `json:"index"`
}

// TransferPlan.Config 只能由服务端保存，绝不能序列化到普通预览响应。
type TransferPlan struct {
	SourceFormat        string                  `json:"sourceFormat"`
	IgnoredGlobalFields []string                `json:"ignoredGlobalFields"`
	CanReplace          bool                    `json:"canReplace"`
	CanApply            bool                    `json:"canApply"`
	Catalog             TransferCatalog         `json:"catalog"`
	Summary             TransferSummary         `json:"summary"`
	Changes             TransferChanges         `json:"changes"`
	FinalRouteOrder     []TransferRoutePosition `json:"finalRouteOrder"`
	Warnings            []TransferIssue         `json:"warnings"`
	Blockers            []TransferIssue         `json:"blockers"`
	Config              *Config                 `json:"-"`
	Options             TransferOptions         `json:"-"`
}

// ExportTransfer 在内存中生成完整迁移包；不读取磁盘，不访问上游，也不修改源配置。
func ExportTransfer(current *Config, request TransferExportRequest) ([]byte, error) {
	if current == nil {
		return nil, transferError("export_failed", "配置快照不可用", "")
	}
	var selection *TransferSelection
	switch request.Scope {
	case "all":
		if request.Selection != nil {
			return nil, transferError("invalid_export", "全部导出不能指定局部选择", "selection")
		}
	case "selected":
		if request.Selection == nil {
			return nil, transferError("invalid_export", "选择导出需要明确选择项", "selection")
		}
		selection = request.Selection
	default:
		return nil, transferError("invalid_export", "请选择全部导出或选择导出", "scope")
	}
	providers, routes, err := transferSelection(current, selection)
	if err != nil {
		return nil, transferError("invalid_export", "导出选择项不存在、重复或为空", "selection")
	}
	bundle := transferBundle{Kind: TransferKind, Version: TransferVersion, Providers: make(map[string]*Provider), Routes: []Route{}}
	for index, route := range current.Routes {
		if !routes[index] {
			continue
		}
		bundle.Routes = append(bundle.Routes, route)
		for _, name := range transferDependencies(route) {
			providers[name] = true
		}
	}
	if err := transferUniqueRoutes(bundle.Routes); err != nil {
		return nil, err
	}
	for name := range providers {
		provider := current.Providers[name]
		if provider == nil {
			return nil, transferError("invalid_export", "所选路由存在未定义的提供商依赖", "providers")
		}
		bundle.Providers[name] = provider
	}
	encoded, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, transferError("export_failed", "生成迁移文件失败", "")
	}
	if len(encoded) > MaxTransferBytes {
		return nil, transferError("transfer_too_large", "导出文件超过 1 MiB 限制", "")
	}
	if _, err := decodeTransfer(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// PlanTransfer 将迁移规则收口为无副作用的完整计划；账户集合只含目标本地 ID。
func PlanTransfer(current *Config, content []byte, options TransferOptions, accountIDs map[string]bool) (*TransferPlan, error) {
	if current == nil {
		return nil, transferError("invalid_import", "当前配置不可用", "")
	}
	document, err := decodeTransfer(content)
	if err != nil {
		return nil, err
	}
	source := document.config
	options, err = normalizeTransferOptions(source, current, options)
	if err != nil {
		return nil, err
	}
	selectedProviders, selectedRoutes, err := transferSelection(source, options.Selection)
	if err != nil {
		return nil, err
	}
	candidate, err := cloneTransferConfig(current)
	if err != nil {
		return nil, err
	}
	plan := &TransferPlan{
		SourceFormat: document.format, IgnoredGlobalFields: document.ignored,
		CanReplace:      len(source.Providers) > 0 && len(source.Routes) > 0,
		Catalog:         TransferCatalog{Providers: []TransferProviderEntry{}, Routes: []TransferRouteEntry{}, SourceAccountIDs: []string{}},
		Changes:         TransferChanges{Providers: []TransferProviderChange{}, Routes: []TransferRouteChange{}},
		FinalRouteOrder: []TransferRoutePosition{}, Warnings: []TransferIssue{}, Blockers: []TransferIssue{},
		Config: candidate, Options: options,
	}
	if len(document.ignored) > 0 {
		plan.Warnings = append(plan.Warnings, TransferIssue{Code: "globals_ignored", Message: "源文件的全局字段不会导入，目标全局设置保持不变"})
	}
	targetIndexes := make(map[string][]int)
	for index, route := range current.Routes {
		key := strings.ToLower(route.Match)
		targetIndexes[key] = append(targetIndexes[key], index)
	}
	accountSet := make(map[string]bool)
	for _, name := range transferProviderNames(source.Providers) {
		provider := source.Providers[name]
		entry := TransferProviderEntry{Name: name, KeyCount: transferKeyCount(provider), APIKeyConfigured: provider.APIKey != "", BalanceTokenConfigured: provider.Balance != nil && provider.Balance.AccessToken != "", SourceAccountID: provider.AccountID, RouteIndexes: []int{}, Selected: selectedProviders[name], Conflict: current.Providers[name] != nil}
		for index, route := range source.Routes {
			if transferReferences(route, name) {
				entry.RouteIndexes = append(entry.RouteIndexes, index)
			}
		}
		plan.Catalog.Providers = append(plan.Catalog.Providers, entry)
		if provider.AuthType == "chatgptOAuth" && provider.AccountID != "" {
			accountSet[provider.AccountID] = true
		}
	}
	for index, route := range source.Routes {
		indexes := append([]int{}, targetIndexes[strings.ToLower(route.Match)]...)
		plan.Catalog.Routes = append(plan.Catalog.Routes, TransferRouteEntry{Index: index, Match: route.Match, ProviderDependencies: transferDependencies(route), Selected: selectedRoutes[index], TargetIndexes: indexes})
	}
	for id := range accountSet {
		plan.Catalog.SourceAccountIDs = append(plan.Catalog.SourceAccountIDs, id)
	}
	sort.Strings(plan.Catalog.SourceAccountIDs)
	if len(accountSet) > 0 {
		plan.Warnings = append(plan.Warnings, TransferIssue{Code: "oauth_accounts_external", Message: "OAuth 账户凭据不在配置包中；实际导入项需绑定目标已有账户"})
	}
	if options.Mode == "replace" && !plan.CanReplace {
		plan.Blockers = append(plan.Blockers, TransferIssue{Code: "replace_requires_complete_bundle", Message: "整体替换需要非空的提供商和路由集合；配置片段只能合并"})
		plan.Config = nil
		return plan, nil
	}
	if options.Mode == "replace" {
		candidate.Providers = make(map[string]*Provider)
		candidate.Routes = []Route{}
	}
	for _, name := range transferProviderNames(source.Providers) {
		if !selectedProviders[name] {
			continue
		}
		before := current.Providers[name]
		// 仅改写候选账户 ID；其余嵌套字段在规划期间只读，源目录保持不变。
		providerCopy := *source.Providers[name]
		after := &providerCopy
		action := "add"
		if options.Mode == "merge" && before != nil && transferProviderConflict(options, name) == "keep" {
			action = "keep"
			after = before
		} else {
			if after.AuthType == "chatgptOAuth" {
				mapped := options.AccountMappings[after.AccountID]
				if mapped == "" && after.AccountID != "" && accountIDs[after.AccountID] {
					mapped = after.AccountID
				}
				if after.AccountID == "" || mapped == "" || !accountIDs[mapped] {
					plan.Blockers = append(plan.Blockers, TransferIssue{Code: "unresolved_account_mapping", Message: "该 OAuth 提供商需要有效的目标账户绑定", Provider: name, Field: "accountId"})
				} else {
					after.AccountID = mapped
				}
			}
			candidate.Providers[name] = after
			if before != nil {
				action = "overwrite"
				if transferValuesEqual(before, after) {
					action = "unchanged"
				}
			}
		}
		plan.addProviderChange(name, action, before, after, current.Routes)
	}
	if options.Mode == "replace" {
		for _, name := range transferProviderNames(current.Providers) {
			if source.Providers[name] == nil {
				plan.addProviderChange(name, "delete", current.Providers[name], nil, current.Routes)
			}
		}
	}
	positions := make([]TransferRoutePosition, 0, len(current.Routes)+len(source.Routes))
	if options.Mode == "merge" {
		for index, route := range current.Routes {
			positions = append(positions, TransferRoutePosition{Match: route.Match, Source: "existing", Index: index})
		}
	}
	newRoutes := []Route{}
	newPositions := []TransferRoutePosition{}
	matchedTargetRoutes := make(map[int]bool)
	for index, route := range source.Routes {
		if !selectedRoutes[index] {
			continue
		}
		indexes := targetIndexes[strings.ToLower(route.Match)]
		var targetIndex *int
		var before *Route
		action := "add"
		if options.Mode == "merge" && len(indexes) > 1 {
			plan.Blockers = append(plan.Blockers, TransferIssue{Code: "ambiguous_target_route", Message: "目标存在重复匹配模式，请先整理后再合并", RouteIndex: intPtr(index)})
			continue
		}
		if len(indexes) == 1 || (options.Mode == "replace" && len(indexes) > 0) {
			// 整体替换不合并旧重复组；首项仅用于差异展示，其余旧项计删除。
			targetIndex = intPtr(indexes[0])
			old := current.Routes[indexes[0]]
			before = &old
			matchedTargetRoutes[indexes[0]] = true
		}
		if options.Mode == "merge" && before != nil {
			if transferRouteConflict(options, index) == "keep" {
				action = "keep"
				plan.addRouteChange(index, targetIndex, action, before, *before)
				continue
			}
			action = "overwrite"
			if transferValuesEqual(*before, route) {
				action = "unchanged"
			}
			candidate.Routes[*targetIndex] = route
			positions[*targetIndex] = TransferRoutePosition{Match: route.Match, Source: "overwritten", Index: index}
		} else {
			if options.Mode == "replace" && before != nil {
				action = "overwrite"
				if transferValuesEqual(*before, route) {
					action = "unchanged"
				}
			}
			newRoutes = append(newRoutes, route)
			newPositions = append(newPositions, TransferRoutePosition{Match: route.Match, Source: "imported", Index: index})
		}
		plan.addRouteChange(index, targetIndex, action, before, route)
		for _, name := range transferDependencies(route) {
			if candidate.Providers[name] == nil {
				plan.Blockers = append(plan.Blockers, TransferIssue{Code: "missing_provider_reference", Message: "所选路由缺少最终提供商依赖", Provider: name, RouteIndex: intPtr(index)})
			} else if options.Mode == "merge" && source.Providers[name] != nil && current.Providers[name] != nil && (!selectedProviders[name] || transferProviderConflict(options, name) == "keep") {
				plan.Warnings = append(plan.Warnings, TransferIssue{Code: "existing_provider_retained", Message: "导入路由将使用保留的目标提供商，而不是源文件版本", Provider: name, RouteIndex: intPtr(index)})
			}
		}
	}
	if options.Mode == "replace" {
		for index, route := range current.Routes {
			if !matchedTargetRoutes[index] {
				plan.Changes.Routes = append(plan.Changes.Routes, TransferRouteChange{TargetIndex: intPtr(index), Match: route.Match, Action: "delete", ChangedFields: []string{}, ProviderDependencies: transferDependencies(route)})
				plan.Summary.Routes.Delete++
			}
		}
	}
	if options.Mode == "merge" && options.InsertNewRoutes == "before" {
		candidate.Routes = append(newRoutes, candidate.Routes...)
		positions = append(newPositions, positions...)
	} else {
		candidate.Routes = append(candidate.Routes, newRoutes...)
		positions = append(positions, newPositions...)
	}
	catchAll := false
	for index := range positions {
		positions[index].Position = index
		route := candidate.Routes[index]
		if catchAll && positions[index].Source == "imported" && route.IsEnabled() {
			plan.Warnings = append(plan.Warnings, TransferIssue{Code: "route_shadowed", Message: "新增路由前已有启用的 * 规则，可能无法命中；可改为前插", RouteIndex: intPtr(positions[index].Index)})
		}
		if route.IsEnabled() && route.Match == "*" {
			catchAll = true
		}
	}
	plan.FinalRouteOrder = positions
	if plan.Summary.Providers.Overwrite+plan.Summary.Providers.Delete+plan.Summary.Routes.Overwrite+plan.Summary.Routes.Delete > 0 {
		plan.Warnings = append(plan.Warnings, TransferIssue{Code: "manual_backup_recommended", Message: "覆盖或替换前，建议先手动导出当前配置备份"})
	}
	if len(plan.Blockers) == 0 {
		if err := validate(candidate); err != nil {
			plan.Blockers = append(plan.Blockers, TransferIssue{Code: "final_config_invalid", Message: "完整目标配置未通过校验，请检查所选配置及依赖"})
		}
	}
	encoded, err := yaml.Marshal(candidate)
	if err != nil {
		return nil, transferError("invalid_import", "目标配置无法序列化", "")
	}
	if len(encoded) > MaxTransferBytes {
		return nil, transferError("config_too_large", "合并后的完整配置超过 1 MiB 限制", "")
	}
	changed := !transferValuesEqual(current, candidate)
	plan.CanApply = len(plan.Blockers) == 0 && changed
	if !plan.CanApply {
		plan.Config = nil
	}
	return plan, nil
}

func (plan *TransferPlan) addProviderChange(name, action string, before, after *Provider, routes []Route) {
	change := TransferProviderChange{Name: name, Action: action, ChangedFields: []string{}, BeforeKeyCount: transferKeyCount(before), AfterKeyCount: transferKeyCount(after), AffectedRoutes: []TransferRouteIdentity{}}
	if action == "add" || action == "overwrite" {
		change.ChangedFields = transferChangedFields(before, after)
	}
	if action == "overwrite" || action == "delete" {
		for index, route := range routes {
			if transferReferences(route, name) {
				change.AffectedRoutes = append(change.AffectedRoutes, TransferRouteIdentity{Index: index, Match: route.Match})
			}
		}
	}
	plan.Changes.Providers = append(plan.Changes.Providers, change)
	plan.Summary.Providers.count(action)
}

func (plan *TransferPlan) addRouteChange(index int, targetIndex *int, action string, before *Route, after Route) {
	fields := []string{}
	if action == "add" || action == "overwrite" {
		fields = transferChangedFields(before, &after)
	}
	plan.Changes.Routes = append(plan.Changes.Routes, TransferRouteChange{SourceIndex: intPtr(index), TargetIndex: targetIndex, Match: after.Match, Action: action, ChangedFields: fields, ProviderDependencies: transferDependencies(after)})
	plan.Summary.Routes.count(action)
}

func decodeTransfer(content []byte) (*transferDocument, error) {
	if len(content) == 0 || !utf8.Valid(content) {
		return nil, transferError("invalid_import", "导入内容为空或不是有效 UTF-8 文本", "")
	}
	if len(content) > MaxTransferBytes {
		return nil, transferError("transfer_too_large", "导入文件超过 1 MiB 限制", "")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, transferError("invalid_import", "YAML 解析失败，请检查文件格式；不会回显原文", "")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, transferError("invalid_import", "导入内容只能包含一个 YAML 文档", "")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, transferError("invalid_import", "导入内容须为配置映射", "")
	}
	root := node.Content[0]
	if err := rejectDuplicateMappingKeys(root, make(map[*yaml.Node]bool)); err != nil {
		return nil, transferError("invalid_import", "配置包含重复字段或不支持的映射键", "")
	}
	var source Config
	format := "config-yaml"
	ignored := []string{}
	kind, version := mappingValue(root, "kind"), mappingValue(root, "version")
	if kind != nil || version != nil {
		if kind == nil || kind.Tag != "!!str" || kind.Value != TransferKind || version == nil || version.Tag != "!!int" || version.Value != strconv.Itoa(TransferVersion) {
			return nil, transferError("unsupported_transfer_version", "不支持该迁移包类型或版本", "version")
		}
		if err := transferNodeShape(root, reflect.TypeOf(transferBundle{}), "", 0); err != nil {
			return nil, err
		}
		var bundle transferBundle
		if err := strictTransferDecode(content, &bundle); err != nil {
			return nil, err
		}
		source.Providers, source.Routes = bundle.Providers, bundle.Routes
		format = "transfer-v1"
	} else {
		if err := transferNodeShape(root, reflect.TypeOf(Config{}), "", 0); err != nil {
			return nil, err
		}
		if err := strictTransferDecode(content, &source); err != nil {
			return nil, err
		}
		for index := 0; index+1 < len(root.Content); index += 2 {
			field := root.Content[index].Value
			if field != "providers" && field != "routes" {
				ignored = append(ignored, field)
			}
		}
		sort.Strings(ignored)
	}
	if len(source.Providers) == 0 && len(source.Routes) == 0 {
		return nil, transferError("invalid_import", "文件中没有可导入的提供商或路由", "")
	}
	if source.Providers == nil {
		source.Providers = make(map[string]*Provider)
	}
	if source.Routes == nil {
		source.Routes = []Route{}
	}
	// 只在独立源副本填提供商默认值，不能让源全局设置进入目标计划。
	applyDefaults(&source)
	for _, name := range transferProviderNames(source.Providers) {
		provider := source.Providers[name]
		if err := validateProvider(name, provider, true); err != nil {
			return nil, transferError("invalid_import", "提供商配置未通过字段或数值校验", "providers."+name)
		}
		if transferContainsSentinel(provider) {
			return nil, transferError("invalid_import", "文件包含保留凭据占位符，请使用含真实凭据的导出文件", "providers."+name)
		}
	}
	for index := range source.Routes {
		if err := validateRoute(nil, &source.Routes[index]); err != nil {
			return nil, transferError("invalid_import", "路由配置未通过字段或数值校验", fmt.Sprintf("routes[%d]", index))
		}
	}
	if err := transferUniqueRoutes(source.Routes); err != nil {
		return nil, err
	}
	return &transferDocument{config: &source, format: format, ignored: ignored}, nil
}

func strictTransferDecode(content []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return transferError("invalid_import", "配置存在未知字段、错误类型或非法结构；请检查文件", "")
	}
	return nil
}

func transferContainsSentinel(provider *Provider) bool {
	if provider.Balance != nil && provider.Balance.AccessToken == BalanceTokenKeepSentinel {
		return true
	}
	if provider.APIKey == APIKeyKeepSentinel {
		return true
	}
	for _, key := range provider.APIKeys {
		if key.Value == APIKeyKeepSentinel {
			return true
		}
	}
	parsed, err := url.Parse(provider.Proxy)
	if err == nil && parsed.User != nil {
		password, _ := parsed.User.Password()
		return password == ProxyPasswordKeepSentinel
	}
	return false
}

func transferUniqueRoutes(routes []Route) error {
	seen := make(map[string]bool, len(routes))
	for index, route := range routes {
		key := strings.ToLower(route.Match)
		if seen[key] {
			return transferError("duplicate_route_match", "文件所选路由存在忽略大小写重复的匹配模式，请先整理", fmt.Sprintf("routes[%d].match", index))
		}
		seen[key] = true
	}
	return nil
}

func transferSelection(source *Config, selection *TransferSelection) (map[string]bool, map[int]bool, error) {
	providers, routes := make(map[string]bool), make(map[int]bool)
	if selection == nil {
		for name := range source.Providers {
			providers[name] = true
		}
		for index := range source.Routes {
			routes[index] = true
		}
		return providers, routes, nil
	}
	if selection.Providers == nil || selection.Routes == nil || len(selection.Providers)+len(selection.Routes) == 0 {
		return nil, nil, transferError("invalid_import", "请选择至少一个条目并明确两个选择列表", "selection")
	}
	for _, name := range selection.Providers {
		if source.Providers[name] == nil || providers[name] {
			return nil, nil, transferError("invalid_import", "提供商选择项不存在或重复", "selection.providers")
		}
		providers[name] = true
	}
	for _, index := range selection.Routes {
		if index < 0 || index >= len(source.Routes) || routes[index] {
			return nil, nil, transferError("invalid_import", "路由选择项不存在或重复", "selection.routes")
		}
		routes[index] = true
	}
	return providers, routes, nil
}

func normalizeTransferOptions(source, current *Config, options TransferOptions) (TransferOptions, error) {
	if options.Mode == "" {
		options.Mode = "merge"
	}
	if options.Mode != "merge" && options.Mode != "replace" {
		return options, transferError("invalid_import", "导入模式须为合并或整体替换", "options.mode")
	}
	if options.Mode == "replace" {
		if options.Selection != nil || options.Conflicts != nil || options.InsertNewRoutes != "" {
			return options, transferError("invalid_import", "整体替换不能指定局部选择、冲突动作或插入位置", "options")
		}
	} else {
		if options.InsertNewRoutes == "" {
			options.InsertNewRoutes = "after"
		}
		if options.InsertNewRoutes != "after" && options.InsertNewRoutes != "before" {
			return options, transferError("invalid_import", "新增路由仅可整批前插或后插", "options.insertNewRoutes")
		}
	}
	providers, routes, err := transferSelection(source, options.Selection)
	if err != nil {
		return options, err
	}
	if options.Selection != nil {
		options.Selection = &TransferSelection{Providers: append([]string{}, options.Selection.Providers...), Routes: append([]int{}, options.Selection.Routes...)}
		sort.Strings(options.Selection.Providers)
		sort.Ints(options.Selection.Routes)
		// 省略选择与显式全选语义一致，凭证使用同一规范化编码。
		if len(providers) == len(source.Providers) && len(routes) == len(source.Routes) {
			options.Selection = nil
		}
	}
	if options.Conflicts != nil {
		canonical := &TransferConflicts{Providers: make(map[string]string), Routes: make(map[string]string)}
		for name, action := range options.Conflicts.Providers {
			if !providers[name] || current.Providers[name] == nil || (action != "keep" && action != "overwrite") {
				return options, transferError("invalid_import", "提供商冲突动作无效或不属于已选冲突项", "options.conflicts.providers")
			}
			if action == "overwrite" {
				canonical.Providers[name] = action
			}
		}
		for text, action := range options.Conflicts.Routes {
			index, err := strconv.Atoi(text)
			if err != nil || strconv.Itoa(index) != text || !routes[index] || !transferHasRoute(current.Routes, source.Routes[index].Match) || (action != "keep" && action != "overwrite") {
				return options, transferError("invalid_import", "路由冲突动作无效或不属于已选冲突项", "options.conflicts.routes")
			}
			if action == "overwrite" {
				canonical.Routes[text] = action
			}
		}
		options.Conflicts = canonical
		if len(canonical.Providers)+len(canonical.Routes) == 0 {
			options.Conflicts = nil
		}
	}
	ids := make(map[string]bool)
	for _, provider := range source.Providers {
		if provider.AuthType == "chatgptOAuth" && provider.AccountID != "" {
			ids[provider.AccountID] = true
		}
	}
	for sourceID, targetID := range options.AccountMappings {
		if !ids[sourceID] || targetID == "" {
			return options, transferError("invalid_import", "账户映射必须引用源文件中的非空账户身份", "options.accountMappings")
		}
	}
	return options, nil
}

func transferProviderConflict(options TransferOptions, name string) string {
	if options.Conflicts != nil && options.Conflicts.Providers[name] == "overwrite" {
		return "overwrite"
	}
	return "keep"
}

func transferRouteConflict(options TransferOptions, index int) string {
	if options.Conflicts != nil && options.Conflicts.Routes[strconv.Itoa(index)] == "overwrite" {
		return "overwrite"
	}
	return "keep"
}

func transferDependencies(route Route) []string {
	seen := make(map[string]bool)
	for _, target := range route.TargetList() {
		seen[target.Provider] = true
	}
	if route.Vision != nil {
		seen[route.Vision.Provider] = true
	}
	names := []string{}
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func transferReferences(route Route, name string) bool {
	for _, dependency := range transferDependencies(route) {
		if dependency == name {
			return true
		}
	}
	return false
}

func transferHasRoute(routes []Route, match string) bool {
	key := strings.ToLower(match)
	for _, route := range routes {
		if strings.ToLower(route.Match) == key {
			return true
		}
	}
	return false
}

func transferProviderNames(providers map[string]*Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func transferKeyCount(provider *Provider) int {
	if provider == nil {
		return 0
	}
	if provider.UsesKeyList() {
		return len(provider.APIKeys)
	}
	if provider.APIKey != "" {
		return 1
	}
	return 0
}

func cloneTransferConfig(source *Config) (*Config, error) {
	encoded, err := yaml.Marshal(source)
	if err != nil {
		return nil, transferError("invalid_import", "无法生成配置快照", "")
	}
	var clone Config
	if err := yaml.Unmarshal(encoded, &clone); err != nil {
		return nil, transferError("invalid_import", "无法生成配置快照", "")
	}
	clone.Path = source.Path
	for name, provider := range clone.Providers {
		provider.Name = name
	}
	return &clone, nil
}

// 比较 YAML 持久化语义，排除 Provider.Name／Config.Path 等运行时元数据。
func transferValuesEqual(before, after any) bool {
	a, errA := yaml.Marshal(before)
	b, errB := yaml.Marshal(after)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func transferChangedFields(before, after any) []string {
	fields := []string{}
	a, b := reflect.ValueOf(before), reflect.ValueOf(after)
	if a.Kind() == reflect.Pointer {
		if a.IsNil() {
			a = reflect.Value{}
		} else {
			a = a.Elem()
		}
	}
	if b.Kind() == reflect.Pointer {
		if b.IsNil() {
			b = reflect.Value{}
		} else {
			b = b.Elem()
		}
	}
	value := b
	if !value.IsValid() {
		value = a
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return fields
	}
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if !a.IsValid() || !b.IsValid() || !transferValuesEqual(a.Field(index).Interface(), b.Field(index).Interface()) {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

// TransferOptionsJSON 给凭证提供稳定编码；不向界面返回控制选项中的私有内容。
func TransferOptionsJSON(options TransferOptions) ([]byte, error) { return json.Marshal(options) }
