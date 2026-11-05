package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/chatgptauth"
)

// ChatGPT 账户导入预检与应用。旧的 POST /api/chatgpt/accounts/import 保持原有单步语义不变。
//
// 以下两项是第二阶段确认值（2026-11-04 用户确认）：
//   - 外层请求上限 24 MiB：解码后的源仍复用旧导入的 16 MiB 上限；base64 封装 ZIP 约膨胀 4/3。
//   - 预检有效期 10 分钟：与配置迁移预检一致。
const (
	maxChatGPTImportRequestBytes = 24 << 20
	maxChatGPTImportSourceBytes  = 16 << 20
	chatGPTImportPreviewTTL      = 10 * time.Minute
)

var errChatGPTImportAccountConflict = errors.New("账户状态在预检后已变化")

type chatGPTImportSource struct {
	MediaType string `json:"mediaType"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
}

type chatGPTImportRequest struct {
	Source       chatGPTImportSource        `json:"source"`
	Actions      []chatgptauth.ImportAction `json:"actions,omitempty"`
	PreviewToken string                     `json:"previewToken,omitempty"`
}

type chatGPTImportPreview struct {
	*chatgptauth.ImportPlan
	PreviewToken *string `json:"previewToken"`
	ExpiresAt    *string `json:"expiresAt"`
}

// chatGPTImportToken 绑定账户版本、配置版本、输入摘要和计划摘要；不含源内容或凭据。
type chatGPTImportToken struct {
	Version     int    `json:"v"`
	Accounts    string `json:"a"`
	Revision    string `json:"r"`
	InputDigest string `json:"i"`
	PlanDigest  string `json:"p"`
	IssuedAt    int64  `json:"t"`
	ExpiresAt   int64  `json:"e"`
	Nonce       string `json:"n"`
}

func decodeChatGPTImportSource(source chatGPTImportSource) ([]byte, bool, error) {
	switch {
	case source.MediaType == "application/json" && source.Encoding == "utf8":
		if len(source.Content) > maxChatGPTImportSourceBytes {
			return nil, false, chatgptauth.ErrImportTooLarge
		}
		return []byte(source.Content), false, nil
	case source.MediaType == "application/zip" && source.Encoding == "base64":
		if base64.StdEncoding.DecodedLen(len(source.Content)) > maxChatGPTImportSourceBytes+3 {
			return nil, false, chatgptauth.ErrImportTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(source.Content)
		if err != nil {
			return nil, false, fmt.Errorf("%w: ZIP 内容不是有效的 base64", chatgptauth.ErrInvalidImport)
		}
		if len(data) > maxChatGPTImportSourceBytes {
			return nil, false, chatgptauth.ErrImportTooLarge
		}
		return data, true, nil
	default:
		return nil, false, fmt.Errorf("%w: source 只支持 application/json + utf8 或 application/zip + base64", chatgptauth.ErrInvalidImport)
	}
}

// chatGPTImportKey 从进程签名密钥派生导入专用密钥，避免与配置迁移凭证互相冒用。
func (s *server) chatGPTImportKey() ([]byte, error) {
	base, err := s.configTransferSigningKey()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, base)
	_, _ = mac.Write([]byte("chatgpt-account-import\x00v1"))
	return mac.Sum(nil), nil
}

func chatGPTImportMAC(key []byte, domain string, parts ...[]byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain + "\x00"))
	for _, part := range parts {
		_, _ = mac.Write([]byte(strconv.Itoa(len(part)) + ":"))
		_, _ = mac.Write(part)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// chatGPTImportInputDigest 绑定源字节、源类型和规范化（按序号排序）后的逐项选择。
func chatGPTImportInputDigest(key []byte, data []byte, archive bool, actions []chatgptauth.ImportAction) (string, error) {
	encoded, err := json.Marshal(chatgptauth.SortImportActions(actions))
	if err != nil {
		return "", err
	}
	kind := []byte("json")
	if archive {
		kind = []byte("zip")
	}
	return chatGPTImportMAC(key, "input", kind, data, encoded), nil
}

func (s *server) signChatGPTImportToken(key []byte, token chatGPTImportToken) (string, string, error) {
	now := s.configTransferTime()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	token.Version, token.IssuedAt = 1, now.Unix()
	token.ExpiresAt = now.Add(chatGPTImportPreviewTTL).Unix()
	token.Nonce = hex.EncodeToString(nonce[:])
	payload, err := json.Marshal(token)
	if err != nil {
		return "", "", err
	}
	text := base64.RawURLEncoding.EncodeToString(payload) + "." + chatGPTImportMAC(key, "token", payload)
	return text, time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339), nil
}

func verifyChatGPTImportToken(text string, key []byte) (chatGPTImportToken, error) {
	var token chatGPTImportToken
	parts := strings.Split(text, ".")
	if len(text) == 0 || len(text) > 4096 || len(parts) != 2 {
		return token, errors.New("无效预检凭证")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return token, err
	}
	if !hmac.Equal([]byte(parts[1]), []byte(chatGPTImportMAC(key, "token", payload))) {
		return token, errors.New("预检凭证签名无效")
	}
	if err := decodeConfigTransferJSON(payload, &token); err != nil {
		return token, err
	}
	if token.Version != 1 || token.Accounts == "" || token.Revision == "" || len(token.Nonce) != 32 ||
		len(token.InputDigest) != 64 || len(token.PlanDigest) != 64 ||
		token.ExpiresAt-token.IssuedAt != int64(chatGPTImportPreviewTTL/time.Second) {
		return token, errors.New("预检凭证字段无效")
	}
	return token, nil
}

// handleChatGPTImportPlan 处理 /import/preview 与 /import/apply。与配置迁移相同，要求本机同源浏览器 Origin。
func (s *server) handleChatGPTImportPlan(w http.ResponseWriter, r *http.Request, apply bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "导入预检请求须使用 application/json")
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if apply && !strictETag(ifMatch) {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请先读取账户列表并携带精确 If-Match 版本")
		return
	}
	body, err := readBodyLimited(r, maxChatGPTImportRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "导入预检请求超过大小限制")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid_import", "读取导入预检请求失败")
		}
		return
	}
	var input chatGPTImportRequest
	if err := decodeConfigTransferJSON(body, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_import", "导入预检请求 JSON 不合法")
		return
	}
	if !apply && input.PreviewToken != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_import", "预检请求不能携带应用凭证")
		return
	}
	data, archive, err := decodeChatGPTImportSource(input.Source)
	if err != nil {
		writeChatGPTImportError(w, err)
		return
	}
	key, err := s.chatGPTImportKey()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "import_unavailable", "安全预检凭证不可用")
		return
	}
	inputDigest, err := chatGPTImportInputDigest(key, data, archive, input.Actions)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "计算导入摘要失败")
		return
	}
	// 与绑定、解绑、删除账户及配置迁移共用事务锁：预检看到的绑定关系在应用前不会被并发改写。
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
		s.applyChatGPTImport(w, r, key, input, data, archive, inputDigest, references, revision, ifMatch)
		return
	}
	plan, err := s.chatgptAuth.PlanImport(data, archive, input.Actions, references)
	if err != nil {
		writeChatGPTImportError(w, err)
		return
	}
	response := chatGPTImportPreview{ImportPlan: plan}
	if plan.CanApply {
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

// applyChatGPTImport 校验预检凭证与当前状态后应用计划。调用方持有 configOpMu。
func (s *server) applyChatGPTImport(w http.ResponseWriter, r *http.Request, key []byte, input chatGPTImportRequest,
	data []byte, archive bool, inputDigest string, references map[string][]string, revision, ifMatch string) {
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
		writeJSONError(w, http.StatusConflict, "preview_mismatch", "导入内容或逐项选择与预检不一致，请重新预检")
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
	result, err := s.chatgptAuth.ApplyImport(data, archive, input.Actions, references, func(plan *chatgptauth.ImportPlan) error {
		if plan.BaseVersion != token.Accounts {
			return errChatGPTImportAccountConflict
		}
		if !hmac.Equal([]byte(chatGPTImportMAC(key, "plan", []byte(plan.Fingerprint()))), []byte(token.PlanDigest)) {
			// 账户控制版本没变但目标凭据已轮换或计划内容不同：拒绝用陈旧计划覆盖。
			return errChatGPTImportAccountConflict
		}
		return nil
	})
	switch {
	case errors.Is(err, errChatGPTImportAccountConflict):
		writeJSONError(w, http.StatusConflict, "account_conflict", "账户或目标凭据在预检后已变化，请重新预检")
		return
	case err != nil:
		writeChatGPTImportError(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(s.chatgptAuth.Version()))
	writeJSON(w, http.StatusOK, map[string]any{"data": result})
}

func writeChatGPTImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chatgptauth.ErrImportTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "导入内容、账户数量或 ZIP 解压大小超过限制")
	case errors.Is(err, chatgptauth.ErrInvalidImport), errors.Is(err, chatgptauth.ErrInvalidImportAction):
		writeJSONError(w, http.StatusBadRequest, "invalid_import", err.Error())
	case errors.Is(err, chatgptauth.ErrImportPlanNotApplicable):
		writeJSONError(w, http.StatusConflict, "account_conflict", "计划不再可应用，请重新预检")
	case errors.Is(err, chatgptauth.ErrAccountImportPersist):
		writeJSONError(w, http.StatusInternalServerError, "account_import_failed", "保存导入账户失败，未保留本批次变更")
	default:
		writeJSONError(w, http.StatusInternalServerError, "account_import_failed", "导入 ChatGPT 账户失败")
	}
}

// strictETag 只接受带引号的强 ETag，不接受弱 ETag 或通配符。
func strictETag(value string) bool {
	return len(value) >= 3 && value[0] == '"' && value[len(value)-1] == '"' && !strings.ContainsAny(value[1:len(value)-1], "\"\r\n")
}
