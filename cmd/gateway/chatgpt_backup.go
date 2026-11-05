package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"ai-gateway/internal/backupcrypto"
	"ai-gateway/internal/chatgptauth"
)

// ChatGPT 账户原生加密备份的导出、恢复预检与应用（API_SPEC 8.5、DESIGN 8.4、ADR 0001）。
// 密码只存在于单次请求作用域内：不写日志、不进 URL、不进预检凭证，错误响应只用固定文案。
const (
	// maxChatGPTBackupExportRequestBytes 足够容纳 100 个账户 ID 与 1024 字节密码。
	maxChatGPTBackupExportRequestBytes = 64 << 10
	// maxChatGPTBackupRequestBytes 是恢复请求外层上限：容器上限 16 MiB，JSON 字符串转义后仍可放下。
	maxChatGPTBackupRequestBytes = 24 << 20
	// chatGPTBackupPreviewTTL 与导入预检一致，为 10 分钟。
	chatGPTBackupPreviewTTL = chatGPTImportPreviewTTL
)

var errChatGPTBackupAccountConflict = errors.New("账户状态在恢复预检后已变化")

type chatGPTBackupExportRequest struct {
	AccountIDs []string `json:"accountIds"`
	Password   string   `json:"password"`
}

type chatGPTBackupRestoreRequest struct {
	Backup        string                      `json:"backup"`
	Password      string                      `json:"password"`
	Metadata      string                      `json:"metadata,omitempty"`
	RestorePaused bool                        `json:"restorePaused,omitempty"`
	Actions       []chatgptauth.RestoreAction `json:"actions,omitempty"`
	PreviewToken  string                      `json:"previewToken,omitempty"`
}

type chatGPTBackupPreview struct {
	*chatgptauth.RestorePlan
	PreviewToken *string `json:"previewToken"`
	ExpiresAt    *string `json:"expiresAt"`
}

// chatGPTBackupKey 从进程签名密钥派生备份专用密钥；域分隔与导入、配置迁移不同，凭证不能互相冒用。
func (s *server) chatGPTBackupKey() ([]byte, error) {
	base, err := s.configTransferSigningKey()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, base)
	_, _ = mac.Write([]byte("chatgpt-account-backup\x00v1"))
	return mac.Sum(nil), nil
}

// chatGPTBackupInputDigest 绑定容器原字节、metadata 规范值、restorePaused 与按序号排序的动作；不含密码。
func chatGPTBackupInputDigest(key, container []byte, metadata string, restorePaused bool, actions []chatgptauth.RestoreAction) (string, error) {
	encoded, err := json.Marshal(chatgptauth.SortImportActions(actions))
	if err != nil {
		return "", err
	}
	return chatGPTImportMAC(key, "input", container, []byte(metadata), []byte(strconv.FormatBool(restorePaused)), encoded), nil
}

// normalizeChatGPTBackupMetadata 只接受空、keep、replace；空按 keep 处理。
func normalizeChatGPTBackupMetadata(value string) (string, bool) {
	switch value {
	case "", chatgptauth.RestoreMetadataKeep:
		return chatgptauth.RestoreMetadataKeep, true
	case chatgptauth.RestoreMetadataReplace:
		return chatgptauth.RestoreMetadataReplace, true
	default:
		return "", false
	}
}

func setChatGPTBackupHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// checkChatGPTBackupRequest 统一方法、本机同源 Origin 与媒体类型边界。
func checkChatGPTBackupRequest(w http.ResponseWriter, r *http.Request) bool {
	setChatGPTBackupHeaders(w)
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return false
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return false
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "备份请求须使用 application/json")
		return false
	}
	return true
}

// handleChatGPTBackupExport 导出所选账户的加密容器，作为无缓存附件返回。
func (s *server) handleChatGPTBackupExport(w http.ResponseWriter, r *http.Request) {
	if !checkChatGPTBackupRequest(w, r) {
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if !strictETag(ifMatch) {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请先读取账户列表并携带精确 If-Match 版本")
		return
	}
	body, err := readBodyLimited(r, maxChatGPTBackupExportRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "备份导出请求超过大小限制")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "读取备份导出请求失败")
		}
		return
	}
	var input chatGPTBackupExportRequest
	if err := decodeConfigTransferJSON(body, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "备份导出请求 JSON 不合法")
		return
	}
	version := ifMatch[1 : len(ifMatch)-1]
	container, err := s.chatgptAuth.ExportBackup(input.AccountIDs, input.Password, version)
	switch {
	case errors.Is(err, chatgptauth.ErrAccountVersionConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户版本已变化，请刷新账户列表后重试")
		return
	case errors.Is(err, chatgptauth.ErrBackupSelection):
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "须选择 1–100 个不重复且存在的账户")
		return
	case errors.Is(err, backupcrypto.ErrInvalidPassword):
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "备份密码须为 12 个字符以上、不超过 1024 字节的合法文本")
		return
	case errors.Is(err, backupcrypto.ErrTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "所选账户的备份内容超过大小上限")
		return
	case err != nil:
		// 契约错误表未给导出定义 500 类型，沿用通用 gateway_error 与固定文案。
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "导出 ChatGPT 账户备份失败")
		return
	}
	filename := "chatgpt-accounts-" + s.configTransferTime().UTC().Format("20060102-150405") + ".aigw-backup"
	w.Header().Set("Content-Type", "application/octet-stream")
	// 文件名只含 ASCII 字母、数字、连字符和点，按契约原样加引号。
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(container)))
	w.Header().Set("ETag", strconv.Quote(version))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(container)
}

// handleChatGPTBackupRestore 处理 /backup/preview 与 /backup/apply。
func (s *server) handleChatGPTBackupRestore(w http.ResponseWriter, r *http.Request, apply bool) {
	if !checkChatGPTBackupRequest(w, r) {
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if apply && !strictETag(ifMatch) {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请先读取账户列表并携带精确 If-Match 版本")
		return
	}
	body, err := readBodyLimited(r, maxChatGPTBackupRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "备份恢复请求超过大小限制")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "读取备份恢复请求失败")
		}
		return
	}
	var input chatGPTBackupRestoreRequest
	if err := decodeChatGPTBackupJSON(body, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "备份恢复请求 JSON 不合法")
		return
	}
	if !apply {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		if _, present := fields["previewToken"]; present {
			writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "预检请求不能携带应用凭证")
			return
		}
	}
	if len(input.Backup) > backupcrypto.MaxContainerBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "备份文件超过 16 MiB 上限")
		return
	}
	metadata, ok := normalizeChatGPTBackupMetadata(input.Metadata)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "metadata 只能为 keep 或 replace")
		return
	}
	container := []byte(input.Backup)
	options := chatgptauth.RestoreOptions{Metadata: metadata, RestorePaused: input.RestorePaused}
	key, err := s.chatGPTBackupKey()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "backup_unavailable", "安全预检凭证不可用")
		return
	}
	inputDigest, err := chatGPTBackupInputDigest(key, container, metadata, input.RestorePaused, input.Actions)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "计算恢复摘要失败")
		return
	}
	// 与导入、绑定、删除账户及配置迁移共用事务锁：预检看到的绑定关系在应用前不会被并发改写。
	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()
	s.cfgMu.Lock()
	if s.revision == "" {
		if s.revision, err = s.issueConfigRevision(); err != nil {
			s.cfgMu.Unlock()
			writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置版本失败")
			return
		}
	}
	references, revision := chatGPTAccountReferences(s.cfg), s.revision
	s.cfgMu.Unlock()
	if apply {
		s.applyChatGPTBackup(w, r, key, input, container, options, inputDigest, references, revision, ifMatch)
		return
	}
	plan, err := s.chatgptAuth.PlanRestore(container, input.Password, options, input.Actions, references)
	if err != nil {
		writeChatGPTBackupError(w, err)
		return
	}
	response := chatGPTBackupPreview{RestorePlan: plan}
	if plan.CanApply {
		// 凭证结构与导入预检相同，但签名密钥按备份域派生，两者不能互换。
		token, expires, err := s.signChatGPTImportToken(key, chatGPTImportToken{
			Accounts: plan.BaseVersion, Revision: revision, InputDigest: inputDigest,
			PlanDigest: chatGPTImportMAC(key, "plan", []byte(plan.Fingerprint())),
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "gateway_error", "签发安全预检凭证失败")
			return
		}
		response.PreviewToken, response.ExpiresAt = &token, &expires
	}
	w.Header().Set("ETag", strconv.Quote(plan.BaseVersion))
	writeJSON(w, http.StatusOK, map[string]any{"data": response})
}

// applyChatGPTBackup 校验预检凭证与当前状态后应用恢复。调用方持有 configOpMu。
func (s *server) applyChatGPTBackup(w http.ResponseWriter, r *http.Request, key []byte, input chatGPTBackupRestoreRequest,
	container []byte, options chatgptauth.RestoreOptions, inputDigest string, references map[string][]string, revision, ifMatch string) {
	token, err := verifyChatGPTImportToken(input.PreviewToken, key)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_preview_token", "预检凭证无效，请重新预检")
		return
	}
	if s.configTransferTime().Unix() >= token.ExpiresAt {
		writeJSONError(w, http.StatusGone, "preview_expired", "预检已过期，请重新预检")
		return
	}
	if !hmac.Equal([]byte(token.InputDigest), []byte(inputDigest)) {
		writeJSONError(w, http.StatusConflict, "preview_mismatch", "备份内容或恢复选择与预检不一致，请重新预检")
		return
	}
	if token.Revision != revision {
		writeJSONError(w, http.StatusConflict, "config_conflict", "预检涉及的绑定配置已变化，请重新预检")
		return
	}
	if ifMatch != strconv.Quote(token.Accounts) {
		writeJSONError(w, http.StatusConflict, "account_conflict", "If-Match 与预检时的账户版本不一致，请重新预检")
		return
	}
	if r.Context().Err() != nil {
		// 开始写入前尊重客户端取消；一旦进入应用则由同一把账户写锁完整收口。
		return
	}
	result, err := s.chatgptAuth.ApplyRestore(container, input.Password, options, input.Actions, references, func(plan *chatgptauth.RestorePlan) error {
		if plan.BaseVersion != token.Accounts {
			return errChatGPTBackupAccountConflict
		}
		if !hmac.Equal([]byte(chatGPTImportMAC(key, "plan", []byte(plan.Fingerprint()))), []byte(token.PlanDigest)) {
			// 账户控制版本没变但目标凭据已轮换或计划内容不同：拒绝用陈旧计划覆盖。
			return errChatGPTBackupAccountConflict
		}
		return nil
	})
	switch {
	case errors.Is(err, errChatGPTBackupAccountConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户或目标凭据在预检后已变化，请重新预检")
		return
	case err != nil:
		writeChatGPTBackupError(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(s.chatgptAuth.Version()))
	writeJSON(w, http.StatusOK, map[string]any{"data": result})
}

// writeChatGPTBackupError 不泄露底层错误文本或解密内容。
func writeChatGPTBackupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backupcrypto.ErrTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "备份文件或内容超过大小上限")
	case errors.Is(err, backupcrypto.ErrInvalidContainer), errors.Is(err, chatgptauth.ErrInvalidBackup):
		writeJSONError(w, http.StatusBadRequest, "invalid_backup", "备份文件结构或内容无效")
	case errors.Is(err, backupcrypto.ErrUnsupported):
		writeJSONError(w, http.StatusBadRequest, "unsupported_backup", "备份格式、版本、算法或参数不受支持")
	case errors.Is(err, backupcrypto.ErrDecrypt):
		writeJSONError(w, http.StatusBadRequest, "backup_decrypt_failed", "密码错误或备份已损坏")
	case errors.Is(err, backupcrypto.ErrInvalidPassword):
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", "备份密码不能为空，且须为不超过 1024 字节的合法文本")
	case errors.Is(err, chatgptauth.ErrInvalidRestoreAction):
		// 动作结构说明只描述请求中的序号与动作名，不含备份解密内容。
		writeJSONError(w, http.StatusBadRequest, "invalid_backup_request", err.Error())
	case errors.Is(err, chatgptauth.ErrRestorePlanNotApplicable), errors.Is(err, chatgptauth.ErrAccountVersionConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "计划不再可应用，请重新预检")
	case errors.Is(err, chatgptauth.ErrAccountRestorePersist):
		writeJSONError(w, http.StatusInternalServerError, "account_restore_failed", "保存恢复的账户失败，未保留本批次变更")
	default:
		writeJSONError(w, http.StatusInternalServerError, "account_restore_failed", "恢复 ChatGPT 账户备份失败")
	}
}

// decodeChatGPTBackupJSON 保留共享解码器对重复键、null、未知字段、尾随内容与字段名的严格校验，
// 仅先把布尔字段转换为共享形状检查支持的字符串，最终仍对原始 JSON 做类型检查。
func decodeChatGPTBackupJSON(data []byte, target *chatGPTBackupRestoreRequest) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if value, ok := raw["restorePaused"]; ok {
		if !bytes.Equal(bytes.TrimSpace(value), []byte("true")) && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
			return errors.New("restorePaused 须为布尔值")
		}
	}
	// 先通过原始 token 流拒绝重复键及 null，不能依赖 map 解码（重复键会被覆盖）。
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateConfigTransferJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON 只能包含一个对象")
	}
	// 去掉已经独立验证过的布尔字段，其余字段直接复用共享严格解码。
	delete(raw, "restorePaused")
	remaining, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	type remainingRequest struct {
		Backup       string                      `json:"backup"`
		Password     string                      `json:"password"`
		Metadata     string                      `json:"metadata,omitempty"`
		Actions      []chatgptauth.RestoreAction `json:"actions,omitempty"`
		PreviewToken string                      `json:"previewToken,omitempty"`
	}
	var checked remainingRequest
	if err := decodeConfigTransferJSON(remaining, &checked); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
