package chatgptauth

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const (
	maxAccountTransferBytes = 16 << 20
	maxAccountTransferItem  = 2 << 20
	maxAccountTransferCount = 100
	maxArchiveEntries       = 200
)

var (
	ErrInvalidImport        = errors.New("ChatGPT OAuth 导入文件无效")
	ErrImportTooLarge       = errors.New("ChatGPT OAuth 导入文件超过大小限制")
	ErrAccountImportPersist = errors.New("保存导入账户失败")
	ErrInvalidExport        = errors.New("ChatGPT OAuth 导出请求无效")
)

// ImportResult 只包含公开账户视图和批次统计，不包含任何凭据。
type ImportResult struct {
	DetectedFormat    string        `json:"detectedFormat"`
	Imported          int           `json:"imported"`
	SkippedDuplicates int           `json:"skippedDuplicates"`
	Accounts          []AccountView `json:"accounts"`
	Warnings          []string      `json:"warnings,omitempty"`
}

// ExportFile 是显式导出请求返回给管理界面的文件内容。
type ExportFile struct {
	Name        string
	ContentType string
	Data        []byte
}

type importRecord struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	AccountID    string
	Email        string
	Plan         string
	ExpiresAt    time.Time
	LastRefresh  time.Time
	Source       string
	Index        int
}

// transferAccountSnapshot 只复制导入导出所需字段，避免复制含互斥锁的 account。
type transferAccountSnapshot struct {
	ID           string
	Email        string
	Plan         string
	AccountID    string
	IDToken      string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	LastRefresh  time.Time
}

// Import 解析并原子导入 JSON 或 ZIP 中的 ChatGPT OAuth 账户。
func (m *Manager) Import(data []byte, archive bool) (ImportResult, error) {
	if len(data) > maxAccountTransferBytes {
		return ImportResult{}, ErrImportTooLarge
	}

	var records []importRecord
	var err error
	if archive {
		records, err = parseImportArchive(data)
	} else {
		records, err = parseImportDocument(data)
	}
	if err != nil {
		return ImportResult{}, err
	}
	if len(records) == 0 {
		return ImportResult{}, fmt.Errorf("%w: 文件中没有账户", ErrInvalidImport)
	}
	if len(records) > maxAccountTransferCount {
		return ImportResult{}, ErrImportTooLarge
	}
	format := detectedImportFormat(records)

	m.mu.Lock()
	defer m.mu.Unlock()

	result := ImportResult{
		DetectedFormat: format,
		Accounts:       make([]AccountView, 0, len(records)),
		Warnings:       make([]string, 0),
	}
	seen := make(map[string]struct{}, len(records))
	newAccounts := make([]*account, 0, len(records))
	for _, record := range records {
		if _, exists := seen[record.AccountID]; exists || m.hasAccountIDLocked(record.AccountID) {
			result.SkippedDuplicates++
			result.Warnings = append(result.Warnings, fmt.Sprintf("第 %d 条账户已存在，未覆盖现有凭据", record.Index))
			continue
		}
		seen[record.AccountID] = struct{}{}
		id, err := randomHex(16)
		if err != nil {
			return ImportResult{}, fmt.Errorf("%w: 无法生成本地账户 ID", ErrAccountImportPersist)
		}
		now := time.Now().UTC()
		lastRefresh := record.LastRefresh
		if lastRefresh.IsZero() {
			lastRefresh = now
		}
		authorizedAt := now
		item := &account{
			ID:           "acc_" + id,
			Email:        record.Email,
			Plan:         record.Plan,
			AccountID:    record.AccountID,
			IDToken:      record.IDToken,
			AccessToken:  record.AccessToken,
			RefreshToken: record.RefreshToken,
			ExpiresAt:    record.ExpiresAt,
			LastRefresh:  lastRefresh,
			AuthorizedAt: &authorizedAt,
			NeedsReauth:  false,
		}
		if record.RefreshToken == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("第 %d 条账户没有 refresh_token，到期后需要重新授权", record.Index))
		}
		newAccounts = append(newAccounts, item)
	}

	if len(newAccounts) == 0 {
		return result, nil
	}
	for _, item := range newAccounts {
		m.accounts[item.ID] = item
	}
	if err := m.persistLocked(); err != nil {
		for _, item := range newAccounts {
			delete(m.accounts, item.ID)
		}
		return ImportResult{}, fmt.Errorf("%w: %v", ErrAccountImportPersist, err)
	}
	for _, item := range newAccounts {
		result.Accounts = append(result.Accounts, viewOf(item))
	}
	result.Imported = len(newAccounts)
	return result, nil
}

func (m *Manager) hasAccountIDLocked(accountID string) bool {
	for _, item := range m.accounts {
		if item.AccountID == accountID {
			return true
		}
	}
	return false
}

func parseImportArchive(data []byte) ([]importRecord, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: ZIP 文件无法读取", ErrInvalidImport)
	}
	if len(reader.File) > maxArchiveEntries {
		return nil, ErrImportTooLarge
	}
	var all []importRecord
	var total int64
	jsonFiles := 0
	for _, file := range reader.File {
		if !safeArchiveName(file.Name) {
			return nil, fmt.Errorf("%w: ZIP 包含不安全路径", ErrInvalidImport)
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() || !strings.EqualFold(path.Ext(file.Name), ".json") {
			return nil, fmt.Errorf("%w: ZIP 只能包含 JSON 文件", ErrInvalidImport)
		}
		jsonFiles++
		if jsonFiles > maxAccountTransferCount || file.UncompressedSize64 > maxAccountTransferItem {
			return nil, ErrImportTooLarge
		}
		total += int64(file.UncompressedSize64)
		if total > maxAccountTransferBytes {
			return nil, ErrImportTooLarge
		}
		entry, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: ZIP 中 JSON 文件无法读取", ErrInvalidImport)
		}
		content, readErr := io.ReadAll(io.LimitReader(entry, maxAccountTransferItem+1))
		closeErr := entry.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("%w: ZIP 中 JSON 文件无法读取", ErrInvalidImport)
		}
		if len(content) > maxAccountTransferItem {
			return nil, ErrImportTooLarge
		}
		records, err := parseImportDocument(content)
		if err != nil {
			return nil, err
		}
		for i := range records {
			records[i].Index = len(all) + i + 1
		}
		all = append(all, records...)
		if len(all) > maxAccountTransferCount {
			return nil, ErrImportTooLarge
		}
	}
	if jsonFiles == 0 {
		return nil, fmt.Errorf("%w: ZIP 中没有 JSON 文件", ErrInvalidImport)
	}
	return all, nil
}

func safeArchiveName(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, ":") || strings.HasPrefix(name, "/") {
		return false
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || path.Clean(trimmed) != trimmed {
		return false
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return false
		}
	}
	return true
}

func parseImportDocument(data []byte) ([]importRecord, error) {
	if len(data) == 0 || len(data) > maxAccountTransferBytes {
		if len(data) > maxAccountTransferBytes {
			return nil, ErrImportTooLarge
		}
		return nil, fmt.Errorf("%w: 文件为空", ErrInvalidImport)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: JSON 无法解析", ErrInvalidImport)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: JSON 文件只能包含一个值", ErrInvalidImport)
	}
	return parseImportValue(value, "", 0)
}

func parseImportValue(value any, sourceHint string, indexOffset int) ([]importRecord, error) {
	switch typed := value.(type) {
	case []any:
		if len(typed) == 0 {
			return nil, fmt.Errorf("%w: 账户数组为空", ErrInvalidImport)
		}
		if len(typed) > maxAccountTransferCount {
			return nil, ErrImportTooLarge
		}
		out := make([]importRecord, 0, len(typed))
		for i, item := range typed {
			records, err := parseImportValue(item, sourceHint, indexOffset+i)
			if err != nil {
				return nil, err
			}
			for j := range records {
				records[j].Index = indexOffset + i + 1
			}
			out = append(out, records...)
		}
		return out, nil
	case map[string]any:
		if kind := stringValue(typed["type"]); kind == "sub2api-data" {
			accounts, ok := typed["accounts"].([]any)
			if !ok || len(accounts) == 0 {
				return nil, fmt.Errorf("%w: sub2api accounts 必须是非空数组", ErrInvalidImport)
			}
			return parseImportValue(accounts, "sub2api", indexOffset)
		}
		if accounts, ok := typed["accounts"].([]any); ok {
			return parseImportValue(accounts, "sub2api", indexOffset)
		}
		record, err := parseImportObject(typed, sourceHint, indexOffset+1)
		if err != nil {
			return nil, err
		}
		return []importRecord{record}, nil
	default:
		return nil, fmt.Errorf("%w: 每条账户必须是 JSON 对象", ErrInvalidImport)
	}
}

func parseImportObject(object map[string]any, sourceHint string, index int) (importRecord, error) {
	if credentials, ok := object["credentials"].(map[string]any); ok {
		object = mergeMaps(object, credentials)
		delete(object, "credentials")
		sourceHint = "sub2api"
	}

	authMode := strings.ToLower(stringValue(object["auth_mode"]))
	if authMode != "" && authMode != "chatgpt" {
		return importRecord{}, importFieldError(index, "auth_mode 不属于 ChatGPT OAuth")
	}
	isOfficial := authMode == "chatgpt"
	if tokens, ok := object["tokens"].(map[string]any); ok {
		object = mergeMaps(object, tokens)
		delete(object, "tokens")
		if isOfficial {
			sourceHint = "auth_json"
		}
	}

	format := sourceHint
	if format == "" {
		switch {
		case isOfficial:
			format = "auth_json"
		case strings.EqualFold(stringValue(object["type"]), "codex"):
			format = "cliproxyapi"
		default:
			format = "cockpit_tools_or_cliproxyapi"
		}
	}
	if isOfficial {
		format = "auth_json"
	}

	accessToken := strings.TrimSpace(firstString(object["access_token"], object["accessToken"]))
	refreshToken := strings.TrimSpace(firstString(object["refresh_token"], object["refreshToken"]))
	idToken := strings.TrimSpace(firstString(object["id_token"], object["idToken"]))
	claims := tokenClaims(idToken, accessToken)
	accountID := strings.TrimSpace(firstString(
		object["account_id"], object["chatgpt_account_id"], object["chatgptAccountId"], object["accountId"],
		claims.Auth["chatgpt_account_id"], claims.Auth["account_id"], claims.Root["chatgpt_account_id"], claims.Root["account_id"],
	))
	email := strings.TrimSpace(firstString(object["email"], object["account_email"], object["accountEmail"], object["name"], claims.Root["email"], claims.Auth["email"]))
	plan := strings.TrimSpace(firstString(object["plan_type"], object["planType"], object["plan"], claims.Auth["chatgpt_plan_type"], claims.Auth["plan_type"]))
	expiresAt := claimTime(firstValue(object, "expires_at", "expiresAt", "expired", "expires"))
	if expiresAt.IsZero() {
		expiresAt = claimTime(tokenClaims(accessToken).Root["exp"])
	}
	lastRefresh := claimTime(firstValue(object, "last_refresh", "lastRefresh"))

	if accessToken == "" {
		return importRecord{}, importFieldError(index, "缺少 access_token")
	}
	if accountID == "" {
		return importRecord{}, importFieldError(index, "无法解析 ChatGPT account_id")
	}
	if refreshToken == "" && (expiresAt.IsZero() || !expiresAt.After(time.Now())) {
		return importRecord{}, importFieldError(index, "缺少 refresh_token，且 access token 没有未来的到期时间")
	}
	return importRecord{
		IDToken: idToken, AccessToken: accessToken, RefreshToken: refreshToken,
		AccountID: accountID, Email: email, Plan: plan, ExpiresAt: expiresAt,
		LastRefresh: lastRefresh, Source: format, Index: index,
	}, nil
}

func importFieldError(index int, message string) error {
	return fmt.Errorf("%w: 第 %d 条账户%s", ErrInvalidImport, index, message)
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overlay {
		merged[key] = value
	}
	return merged
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok && value != nil && stringValue(value) != "" {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

func detectedImportFormat(records []importRecord) string {
	format := ""
	for _, record := range records {
		if format == "" {
			format = record.Source
			continue
		}
		if format != record.Source {
			return "mixed"
		}
	}
	if format == "" {
		return "unknown"
	}
	return format
}

type portableToken struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email,omitempty"`
	PlanType     string `json:"plan_type,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
}

type cliProxyAuth struct {
	Type         string `json:"type"`
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email,omitempty"`
	PlanType     string `json:"plan_type,omitempty"`
	Expired      string `json:"expired,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
}

type officialAuth struct {
	AuthMode    string         `json:"auth_mode"`
	APIKey      any            `json:"OPENAI_API_KEY"`
	Tokens      portableTokens `json:"tokens"`
	LastRefresh string         `json:"last_refresh,omitempty"`
}

type portableTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id"`
}

type sub2apiEnvelope struct {
	Type       string           `json:"type"`
	Version    int              `json:"version"`
	ExportedAt string           `json:"exported_at"`
	Proxies    []any            `json:"proxies"`
	Accounts   []sub2apiAccount `json:"accounts"`
}

type sub2apiAccount struct {
	Name        string        `json:"name,omitempty"`
	Platform    string        `json:"platform"`
	Type        string        `json:"type"`
	Credentials portableToken `json:"credentials"`
}

// Export 只导出调用方明确列出的本地账户。
func (m *Manager) Export(format string, accountIDs []string) (ExportFile, error) {
	if len(accountIDs) == 0 || len(accountIDs) > maxAccountTransferCount {
		return ExportFile{}, fmt.Errorf("%w: 必须选择 1 到 %d 个账户", ErrInvalidExport, maxAccountTransferCount)
	}
	if format != "cockpit_tools" && format != "cliproxyapi" && format != "sub2api" && format != "auth_json" {
		return ExportFile{}, fmt.Errorf("%w: 不支持的导出格式", ErrInvalidExport)
	}

	m.mu.RLock()
	selected := make([]transferAccountSnapshot, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if _, exists := seen[id]; exists {
			m.mu.RUnlock()
			return ExportFile{}, fmt.Errorf("%w: 账户列表包含重复 ID", ErrInvalidExport)
		}
		seen[id] = struct{}{}
		item := m.accounts[id]
		if item == nil {
			m.mu.RUnlock()
			return ExportFile{}, fmt.Errorf("%w: 所选账户不存在", ErrInvalidExport)
		}
		selected = append(selected, transferAccountSnapshot{
			ID: item.ID, Email: item.Email, Plan: item.Plan, AccountID: item.AccountID,
			IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
			ExpiresAt: item.ExpiresAt, LastRefresh: item.LastRefresh,
		})
	}
	m.mu.RUnlock()

	if format == "auth_json" {
		for i, item := range selected {
			if item.IDToken == "" {
				return ExportFile{}, fmt.Errorf("%w: 第 %d 个账户缺少 id_token，无法生成官方 auth.json", ErrInvalidExport, i+1)
			}
		}
	}

	if (format == "cliproxyapi" || format == "auth_json") && len(selected) > 1 {
		return exportArchive(format, selected)
	}
	return exportJSON(format, selected)
}

func exportJSON(format string, accounts []transferAccountSnapshot) (ExportFile, error) {
	var value any
	name := "chatgpt-accounts.json"
	contentType := "application/json"
	switch format {
	case "cockpit_tools":
		items := make([]portableToken, 0, len(accounts))
		for _, item := range accounts {
			items = append(items, portableFromAccount(item))
		}
		value = items
	case "cliproxyapi":
		name = "codex-auth.json"
		value = cliProxyFromAccount(accounts[0])
	case "sub2api":
		envelope := sub2apiEnvelope{
			Type: "sub2api-data", Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339),
			Proxies: []any{}, Accounts: make([]sub2apiAccount, 0, len(accounts)),
		}
		for _, item := range accounts {
			envelope.Accounts = append(envelope.Accounts, sub2apiAccount{
				Name: item.Email, Platform: "openai", Type: "oauth", Credentials: portableFromAccount(item),
			})
		}
		value = envelope
	case "auth_json":
		name = "auth.json"
		item := accounts[0]
		value = officialAuth{
			AuthMode: "chatgpt", APIKey: nil,
			Tokens:      portableTokens{IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, AccountID: item.AccountID},
			LastRefresh: timeString(item.LastRefresh),
		}
	default:
		return ExportFile{}, fmt.Errorf("%w: 不支持的导出格式", ErrInvalidExport)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ExportFile{}, err
	}
	return ExportFile{Name: name, ContentType: contentType, Data: append(data, '\n')}, nil
}

func exportArchive(format string, accounts []transferAccountSnapshot) (ExportFile, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, item := range accounts {
		var name string
		var value any
		if format == "cliproxyapi" {
			name = archiveSlug(item) + ".json"
			value = cliProxyFromAccount(item)
		} else {
			name = archiveSlug(item) + "/auth.json"
			value = officialAuth{
				AuthMode: "chatgpt", APIKey: nil,
				Tokens:      portableTokens{IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, AccountID: item.AccountID},
				LastRefresh: timeString(item.LastRefresh),
			}
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			_ = writer.Close()
			return ExportFile{}, err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return ExportFile{}, err
		}
		if _, err := entry.Write(append(data, '\n')); err != nil {
			_ = writer.Close()
			return ExportFile{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return ExportFile{}, err
	}
	return ExportFile{Name: "chatgpt-accounts.zip", ContentType: "application/zip", Data: buffer.Bytes()}, nil
}

func portableFromAccount(item transferAccountSnapshot) portableToken {
	return portableToken{
		IDToken: item.IDToken, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken,
		AccountID: item.AccountID, Email: item.Email, PlanType: item.Plan,
		ExpiresAt: timeString(item.ExpiresAt), LastRefresh: timeString(item.LastRefresh),
	}
}

func cliProxyFromAccount(item transferAccountSnapshot) cliProxyAuth {
	return cliProxyAuth{
		Type: "codex", IDToken: item.IDToken, AccessToken: item.AccessToken,
		RefreshToken: item.RefreshToken, AccountID: item.AccountID, Email: item.Email,
		PlanType: item.Plan, Expired: timeString(item.ExpiresAt), LastRefresh: timeString(item.LastRefresh),
	}
}

func timeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func archiveSlug(item transferAccountSnapshot) string {
	base := strings.ToLower(strings.TrimSpace(item.Email))
	if base == "" {
		base = "chatgpt-account"
	}
	var slug strings.Builder
	lastDash := false
	for _, r := range base {
		safe := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if safe {
			slug.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			slug.WriteByte('-')
			lastDash = true
		}
	}
	value := strings.Trim(slug.String(), "-_")
	if value == "" {
		value = "chatgpt-account"
	}
	if len(value) > 48 {
		value = value[:48]
	}
	return value + "-" + item.ID
}
