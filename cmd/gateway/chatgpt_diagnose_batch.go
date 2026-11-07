package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const chatGPTDiagnoseBatchWorkers = 3
const chatGPTDiagnoseBatchMaxAccounts = 50

var chatGPTDiagnoseBatchBudget = 15 * time.Minute
var errChatGPTDiagnoseBatchBudget = errors.New("批量预检总预算已耗尽")

type chatGPTDiagnoseBatchRequest struct {
	AccountIDs  []string `json:"accountIds"`
	Checks      []string `json:"checks,omitempty"`
	RefreshMode string   `json:"refreshMode,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Model       string   `json:"model,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Confirm     string   `json:"confirm,omitempty"`
}
type chatGPTDiagnoseBatchResult struct {
	Index     int                    `json:"index"`
	AccountID string                 `json:"accountId"`
	Status    string                 `json:"status"`
	Message   string                 `json:"message,omitempty"`
	Report    *chatGPTDiagnoseReport `json:"report,omitempty"`
}

func (s *server) handleChatGPTDiagnoseBatch(w http.ResponseWriter, r *http.Request) {
	setProviderQueryHeaders(w)
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !configTransferOriginAllowed(r) {
		writeForbiddenOrigin(w)
		return
	}
	if requestMediaType(r) != "application/json" {
		writeJSONError(w, 415, "unsupported_media_type", "批量预检须使用 application/json")
		return
	}
	body, err := readBodyLimited(r, maxChatGPTDiagnoseRequestBytes)
	if err != nil {
		status := 400
		if errors.Is(err, errRequestBodyTooLarge) {
			status = 413
		}
		writeJSONError(w, status, "invalid_diagnose_request", "批量预检请求无法读取或超过大小限制")
		return
	}
	var input chatGPTDiagnoseBatchRequest
	if err := decodeChatGPTDiagnoseJSON(body, &input); err != nil {
		writeJSONError(w, 400, "invalid_diagnose_request", "批量预检 JSON 无效")
		return
	}
	plan, err := normalizeChatGPTDiagnoseRequest(chatGPTDiagnoseRequest{Checks: input.Checks, RefreshMode: input.RefreshMode, Mode: input.Mode, Provider: input.Provider, Model: input.Model, Prompt: input.Prompt, Confirm: input.Confirm})
	if err != nil {
		writeJSONError(w, 400, "invalid_diagnose_request", err.Error())
		return
	}
	if plan.mode == "provider" || input.Provider != "" {
		writeJSONError(w, 400, "invalid_diagnose_request", "批量预检只支持账户模式")
		return
	}
	if len(input.AccountIDs) == 0 || len(input.AccountIDs) > chatGPTDiagnoseBatchMaxAccounts {
		writeJSONError(w, 400, "invalid_batch", "请选择 1 至 50 个账户")
		return
	}
	seen := make(map[string]bool)
	for _, id := range input.AccountIDs {
		if !validChatGPTAccountID(id) || seen[id] {
			writeJSONError(w, 400, "invalid_batch", "账户 ID 无效或重复")
			return
		}
		seen[id] = true
	}
	if !s.chatGPTBatchDiagnosing.CompareAndSwap(false, true) {
		writeJSONError(w, 409, "diagnose_batch_in_progress", "已有批量预检正在执行")
		return
	}
	defer s.chatGPTBatchDiagnosing.Store(false)
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	timer := time.AfterFunc(chatGPTDiagnoseBatchBudget, func() { cancel(errChatGPTDiagnoseBatchBudget) })
	defer timer.Stop()
	plan.batch = true
	started := time.Now()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(event string, data any) bool {
		payload, err := json.Marshal(data)
		if err != nil {
			cancel(err)
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		if err == nil {
			err = controller.Flush()
		}
		if err != nil {
			cancel(err)
			return false
		}
		return true
	}
	defer controller.SetWriteDeadline(time.Time{})
	if !send("start", map[string]any{"total": len(input.AccountIDs), "concurrency": chatGPTDiagnoseBatchWorkers, "accountBudgetMs": chatGPTDiagnoseBudget.Milliseconds(), "budgetMs": chatGPTDiagnoseBatchBudget.Milliseconds()}) {
		return
	}
	jobs := make(chan int, len(input.AccountIDs))
	results := make(chan chatGPTDiagnoseBatchResult, len(input.AccountIDs))
	for index := range input.AccountIDs {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for i := 0; i < chatGPTDiagnoseBatchWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results <- s.runChatGPTDiagnoseBatchItem(ctx, index, input.AccountIDs[index], plan)
			}
		}()
	}
	defer workers.Wait()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	counts := make(map[string]int)
	completed := 0
	for completed < len(input.AccountIDs) {
		select {
		case result := <-results:
			if !send("result", result) {
				return
			}
			completed++
			counts[result.Status]++
		case <-heartbeat.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				cancel(err)
				return
			}
			if err := controller.Flush(); err != nil {
				cancel(err)
				return
			}
		case <-r.Context().Done():
			cancel(r.Context().Err())
			return
		}
	}
	send("done", map[string]any{"total": len(input.AccountIDs), "completed": completed, "durationMs": time.Since(started).Milliseconds(), "cancelled": ctx.Err() != nil, "counts": counts})
}

func (s *server) runChatGPTDiagnoseBatchItem(parent context.Context, index int, id string, plan chatGPTDiagnosePlan) chatGPTDiagnoseBatchResult {
	result := chatGPTDiagnoseBatchResult{Index: index, AccountID: id}
	if parent.Err() != nil {
		result.Status, result.Message = "cancelled", "批量预检已取消，此账户尚未开始"
		return result
	}
	if _, ok := s.chatgptAuth.Account(id); !ok {
		result.Status, result.Message = "not_found", "账户不存在"
		return result
	}
	if _, busy := s.chatGPTDiagnosing.LoadOrStore(id, struct{}{}); busy {
		result.Status, result.Message = "busy", "此账户已有诊断正在运行"
		return result
	}
	defer s.chatGPTDiagnosing.Delete(id)
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	timer := time.AfterFunc(chatGPTDiagnoseBudget, func() { cancel(errChatGPTDiagnoseBudget) })
	defer timer.Stop()
	report := s.runChatGPTDiagnose(ctx, id, plan)
	result.Report = &report
	result.Status = chatGPTDiagnoseBatchStatus(report, plan)
	return result
}

func chatGPTDiagnoseBatchStatus(report chatGPTDiagnoseReport, plan chatGPTDiagnosePlan) string {
	success, failure, skipped := false, false, false
	for _, step := range report.Steps {
		if !plan.selected[step.Name] {
			continue
		}
		switch step.Status {
		case "cancelled":
			return "cancelled"
		case "success":
			success = true
		case "failure":
			failure = true
		case "skipped":
			skipped = true
		}
	}
	switch {
	case success && (failure || skipped):
		return "partial"
	case failure:
		return "failure"
	case success:
		return "success"
	default:
		return "skipped"
	}
}
