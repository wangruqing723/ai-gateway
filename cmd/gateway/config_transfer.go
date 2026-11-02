package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"ai-gateway/internal/config"
	"gopkg.in/yaml.v3"
)

const maxConfigTransferRequestBytes = 8 << 20
const configTransferPreviewTTL = 10 * time.Minute

type configTransferImportRequest struct {
	Content      string                 `json:"content"`
	Options      config.TransferOptions `json:"options,omitempty"`
	PreviewToken string                 `json:"previewToken,omitempty"`
}

type configTransferPreview struct {
	*config.TransferPlan
	BaseRevision string  `json:"baseRevision"`
	PreviewToken *string `json:"previewToken"`
	ExpiresAt    *string `json:"expiresAt"`
}

type configTransferToken struct {
	Version      int    `json:"v"`
	Revision     string `json:"r"`
	InputDigest  string `json:"i"`
	ConfigDigest string `json:"c"`
	IssuedAt     int64  `json:"t"`
	ExpiresAt    int64  `json:"e"`
	Nonce        string `json:"n"`
}

func (s *server) configTransferTime() time.Time {
	if s.transferNow != nil {
		return s.transferNow()
	}
	return time.Now()
}

func (s *server) configTransferSigningKey() ([]byte, error) {
	s.transferOnce.Do(func() { _, s.transferKeyErr = rand.Read(s.transferKey[:]) })
	return s.transferKey[:], s.transferKeyErr
}

// 新传输端点比原配置 API 更严格：没有 Origin 也必须拒绝，不能作为脚本后门。
func configTransferOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || isCrossSiteRequest(r) {
		return false
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && isLoopbackHostname(parsed.Hostname())
}

func (s *server) handleConfigTransfer(w http.ResponseWriter, r *http.Request, path string) {
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
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "传输请求须使用 JSON Content-Type")
		return
	}
	etag := strings.TrimSpace(r.Header.Get("If-Match"))
	if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' || strings.ContainsAny(etag[1:len(etag)-1], "\"\r\n") {
		writeJSONError(w, http.StatusPreconditionRequired, "precondition_required", "请先读取配置并携带精确 If-Match 版本")
		return
	}
	body, err := readBodyLimited(r, maxConfigTransferRequestBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large", "传输请求超过大小限制")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid_import", "读取请求失败")
		}
		return
	}
	// 同一把事务锁也覆盖现有账户删除／绑定；预览与应用不会看到一半的配置。
	s.configOpMu.Lock()
	defer s.configOpMu.Unlock()
	s.cfgMu.Lock()
	if s.revision == "" {
		s.revision, err = s.issueConfigRevision()
		if err != nil {
			s.cfgMu.Unlock()
			writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成配置版本失败")
			return
		}
	}
	current, revision := s.cfg, s.revision
	s.cfgMu.Unlock()
	if etag != strconv.Quote(revision) {
		writeJSONError(w, http.StatusConflict, "config_conflict", "配置版本已变化，请重新读取并预览")
		return
	}
	if path == "/export" {
		var input config.TransferExportRequest
		if err := decodeConfigTransferJSON(body, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_export", "导出请求 JSON 不合法")
			return
		}
		encoded, err := config.ExportTransfer(current, input)
		if err != nil {
			writeConfigTransferError(w, err)
			return
		}
		filename := "ai-gateway-provider-routes-" + s.configTransferTime().UTC().Format("20060102T150405Z") + ".yaml"
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
		w.Header().Set("ETag", strconv.Quote(revision))
		_, _ = w.Write(encoded)
		return
	}
	var input configTransferImportRequest
	if err := decodeConfigTransferJSON(body, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_import", "导入请求 JSON 不合法")
		return
	}
	if path == "/import/preview" && input.PreviewToken != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_import", "预览请求不能携带应用凭证")
		return
	}
	if len(input.Content) > config.MaxTransferBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "transfer_too_large", "源文件超过 1 MiB 限制")
		return
	}
	key, err := s.configTransferSigningKey()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "transfer_unavailable", "安全预览凭证不可用")
		return
	}
	var token configTransferToken
	if path == "/import/apply" {
		token, err = verifyConfigTransferToken(input.PreviewToken, key)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_preview_token", "预览凭证无效，请重新预览")
			return
		}
		if s.configTransferTime().Unix() >= token.ExpiresAt {
			writeJSONError(w, http.StatusConflict, "preview_expired", "预览凭证已过期，请重新预览")
			return
		}
		if token.Revision != revision {
			writeJSONError(w, http.StatusConflict, "config_conflict", "预览对应的配置版本已变化")
			return
		}
	}
	accounts := make(map[string]bool)
	if s.chatgptAuth != nil {
		for _, account := range s.chatgptAuth.List(nil) {
			accounts[account.ID] = true
		}
	}
	plan, err := config.PlanTransfer(current, []byte(input.Content), input.Options, accounts)
	if err != nil {
		writeConfigTransferError(w, err)
		return
	}
	if err := r.Context().Err(); err != nil {
		// 在保存开始前尊重客户端取消；一旦开始保存则由同一事务完整收口。
		return
	}
	if path == "/import/preview" {
		response := configTransferPreview{TransferPlan: plan, BaseRevision: revision}
		if plan.CanApply {
			tokenText, expires, err := s.signConfigTransferPlan(key, revision, input.Content, plan)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "gateway_error", "签发安全预览凭证失败")
				return
			}
			response.PreviewToken, response.ExpiresAt = &tokenText, &expires
		}
		w.Header().Set("ETag", strconv.Quote(revision))
		writeJSON(w, http.StatusOK, map[string]any{"data": response})
		return
	}
	if !plan.CanApply || plan.Config == nil {
		writeJSONError(w, http.StatusConflict, "import_dependency_changed", "计划不再可应用，请检查依赖并重新预览")
		return
	}
	inputDigest, configDigest, err := configTransferPlanDigests(key, input.Content, plan)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "校验预览计划失败")
		return
	}
	if inputDigest != token.InputDigest || configDigest != token.ConfigDigest {
		writeJSONError(w, http.StatusConflict, "preview_changed", "文件、选项或目标计划已改变，请重新预览")
		return
	}
	newRevision, err := s.issueConfigRevision()
	if err != nil || newRevision == revision {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "生成新配置版本失败")
		return
	}
	if err := config.Save(plan.Config); err != nil {
		if errors.Is(err, config.ErrRestoreConfigFailed) {
			writeJSONError(w, http.StatusInternalServerError, "import_storage_uncertain", "保存和原文件恢复均失败，运行时未发布计划；磁盘状态需人工确认")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "import_save_failed", "保存导入配置失败，未发布运行时变更")
		}
		return
	}
	restartRequired := s.applyRuntimeConfig(plan.Config, newRevision)
	w.Header().Set("ETag", strconv.Quote(newRevision))
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "配置已导入、保存并热重载", "revision": newRevision, "restartRequired": restartRequired, "summary": plan.Summary})
}

func (s *server) signConfigTransferPlan(key []byte, revision, content string, plan *config.TransferPlan) (string, string, error) {
	inputDigest, configDigest, err := configTransferPlanDigests(key, content, plan)
	if err != nil {
		return "", "", err
	}
	now := s.configTransferTime()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	token := configTransferToken{Version: 1, Revision: revision, InputDigest: inputDigest, ConfigDigest: configDigest, IssuedAt: now.Unix(), ExpiresAt: now.Add(configTransferPreviewTTL).Unix(), Nonce: hex.EncodeToString(nonce[:])}
	payload, err := json.Marshal(token)
	if err != nil {
		return "", "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	text := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return text, time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339), nil
}

func configTransferPlanDigests(key []byte, content string, plan *config.TransferPlan) (string, string, error) {
	options, err := config.TransferOptionsJSON(plan.Options)
	if err != nil {
		return "", "", err
	}
	input, err := json.Marshal(struct {
		Content string          `json:"content"`
		Options json.RawMessage `json:"options"`
	}{content, options})
	if err != nil {
		return "", "", err
	}
	encoded, err := yaml.Marshal(plan.Config)
	if err != nil {
		return "", "", err
	}
	// 摘要也使用进程密钥，预览凭证不泄露可离线猜测弱凭据的公开指纹。
	inputMAC := hmac.New(sha256.New, key)
	_, _ = inputMAC.Write(append([]byte("input\x00"), input...))
	configMAC := hmac.New(sha256.New, key)
	_, _ = configMAC.Write(append([]byte("config\x00"), encoded...))
	return hex.EncodeToString(inputMAC.Sum(nil)), hex.EncodeToString(configMAC.Sum(nil)), nil
}

func verifyConfigTransferToken(text string, key []byte) (configTransferToken, error) {
	var token configTransferToken
	if len(text) == 0 || len(text) > 4096 {
		return token, errors.New("无效预览凭证")
	}
	parts := strings.Split(text, ".")
	if len(parts) != 2 {
		return token, errors.New("无效预览凭证")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return token, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return token, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return token, errors.New("凭证签名无效")
	}
	if err := decodeConfigTransferJSON(payload, &token); err != nil {
		return token, err
	}
	if token.Version != 1 || token.Revision == "" || token.ExpiresAt-token.IssuedAt != int64(configTransferPreviewTTL/time.Second) || len(token.Nonce) != 32 || len(token.InputDigest) != 64 || len(token.ConfigDigest) != 64 {
		return token, errors.New("凭证字段无效")
	}
	return token, nil
}

// 严格 JSON 禁止重复键、未知字段、多值文档和 null 控制字段。
func decodeConfigTransferJSON(data []byte, target any) error {
	keys := json.NewDecoder(bytes.NewReader(data))
	if err := validateConfigTransferJSONValue(keys, 0); err != nil {
		return err
	}
	if _, err := keys.Token(); err != io.EOF {
		return errors.New("JSON 只能包含一个对象")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := validateConfigTransferJSONShape(raw, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("JSON 只能包含一个对象")
	}
	return nil
}

func validateConfigTransferJSONShape(value any, target reflect.Type) error {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	invalid := func() error { return errors.New("JSON 控制字段类型不正确") }
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		fields := make(map[string]reflect.Type)
		for index := 0; index < target.NumField(); index++ {
			item := target.Field(index)
			fields[strings.Split(item.Tag.Get("json"), ",")[0]] = item.Type
		}
		for name, item := range object {
			field, ok := fields[name]
			if !ok {
				return invalid()
			}
			if err := validateConfigTransferJSONShape(item, field); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		for _, item := range object {
			if err := validateConfigTransferJSONShape(item, target.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return invalid()
		}
		for _, item := range array {
			if err := validateConfigTransferJSONShape(item, target.Elem()); err != nil {
				return err
			}
		}
	case reflect.String:
		if _, ok := value.(string); !ok {
			return invalid()
		}
	case reflect.Int, reflect.Int64:
		if _, ok := value.(float64); !ok {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

func validateConfigTransferJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 100 {
		return errors.New("JSON 嵌套过深")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("JSON 控制字段不能为 null")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		if depth == 0 {
			return errors.New("JSON 须为对象")
		}
		return nil
	}
	if depth == 0 && delimiter != '{' {
		return errors.New("JSON 须为对象")
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			text, ok := key.(string)
			if !ok || seen[text] {
				return errors.New("JSON 字段重复")
			}
			seen[text] = true
			if err := validateConfigTransferJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateConfigTransferJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("JSON 分隔符无效")
	}
	_, err = decoder.Token()
	return err
}

func writeConfigTransferError(w http.ResponseWriter, err error) {
	var issue *config.TransferError
	if !errors.As(err, &issue) {
		writeJSONError(w, http.StatusInternalServerError, "gateway_error", "配置传输失败")
		return
	}
	status := http.StatusBadRequest
	switch issue.Code {
	case "transfer_too_large", "config_too_large":
		status = http.StatusRequestEntityTooLarge
	case "export_failed":
		status = http.StatusInternalServerError
	}
	payload := map[string]any{"type": issue.Code, "message": issue.Message}
	if issue.Field != "" || issue.Line > 0 {
		item := map[string]any{"code": "invalid_field"}
		if issue.Field != "" {
			item["field"] = issue.Field
		}
		if issue.Line > 0 {
			item["line"] = issue.Line
		}
		payload["issues"] = []any{item}
	}
	writeJSON(w, status, map[string]any{"error": payload})
}
