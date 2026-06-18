// Package httputil 提供 gateway 和 proxy 共用的 HTTP 工具函数。
package httputil

import (
	"encoding/json"
	"net/http"
)

// WriteJSONError 统一 JSON 错误响应，供 gateway handler 和 proxy 共用。
func WriteJSONError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errType, "message": msg},
	})
	w.Write(payload)
}
