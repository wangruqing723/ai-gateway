package main

import (
	"errors"
	"net/http"
	"time"

	"ai-gateway/internal/chatgptauth"
	"ai-gateway/internal/config"
)

// 第六阶段：ChatGPT 账户后台令牌预刷新与用量刷新（DESIGN 7.1）。

// chatGPTBackgroundSettings 把配置换算成账户管理器的后台设置。
func chatGPTBackgroundSettings(cfg *config.Config) chatgptauth.BackgroundSettings {
	background := cfg.ChatGPTBackground
	return chatgptauth.BackgroundSettings{
		TokenRefresh:  background.TokenRefresh,
		UsageRefresh:  background.UsageRefresh,
		UsageInterval: time.Duration(background.UsageInterval()) * time.Minute,
		ActiveWindow:  time.Duration(background.ActiveWindow()) * time.Minute,
	}
}

// applyChatGPTBackground 启动、更新或停止后台循环。调用方不得持有 cfgMu：
// 关闭时要等循环退出，而循环内的任务会读取账户状态。
func (s *server) applyChatGPTBackground(cfg *config.Config) {
	if cfg == nil || s.chatgptAuth == nil {
		return
	}
	err := s.chatgptAuth.SetBackground(chatGPTBackgroundSettings(cfg))
	if errors.Is(err, chatgptauth.ErrBackgroundUnsupported) {
		logSystem("%s", err.Error())
	}
}

// stopChatGPTBackground 在网关关闭时取消后台出网并等循环退出。
func (s *server) stopChatGPTBackground() {
	if s.chatgptAuth != nil {
		s.chatgptAuth.StopBackground()
	}
}

// handleChatGPTBackground 处理 GET /api/chatgpt/background。
func (s *server) handleChatGPTBackground(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"data": s.chatgptAuth.BackgroundStatus()})
}
