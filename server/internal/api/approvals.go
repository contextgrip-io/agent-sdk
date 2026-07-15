package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/contextgrip-io/agent-sdk/server/internal/approvalstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/taskstore"
)

// WriteExecutor executes one approved statement against the write connection
// (AI_CHAT_WRITE_DATABASE_URL); *dbx.DB implements it and tests substitute a
// fake. nil means writes are disabled.
type WriteExecutor interface {
	ExecuteWrite(ctx context.Context, sql string) (rowsAffected int64, err error)
}

// approvalView matches the Approval schema.
type approvalView struct {
	ID        string             `json:"id"`
	SQL       string             `json:"sql"`
	Rationale string             `json:"rationale,omitempty"`
	Status    string             `json:"status"`
	Source    approvalSourceView `json:"source"`
	CreatedAt string             `json:"createdAt"`
	DecidedAt string             `json:"decidedAt,omitempty"`
}

type approvalSourceView struct {
	ConversationID string `json:"conversationId,omitempty"`
	MessageID      string `json:"messageId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
}

func toApprovalView(appr approvalstore.Approval) approvalView {
	view := approvalView{
		ID:        appr.ID,
		SQL:       appr.SQL,
		Rationale: appr.Rationale,
		Status:    appr.Status,
		Source:    approvalSourceView{MessageID: appr.MessageID, TaskID: appr.TaskID},
		CreatedAt: appr.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if appr.DecidedAt != nil {
		view.DecidedAt = appr.DecidedAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cfg.Approvals.ListPending(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to list approvals")
		return
	}
	out := make([]approvalView, 0, len(rows))
	for _, appr := range rows {
		out = append(out, toApprovalView(appr))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDecideApproval approves or rejects a proposed write. Approving
// executes the EXACT stored SQL on the write connection.
func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid request body")
		return
	}
	if body.Decision != "approve" && body.Decision != "reject" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "decision must be approve or reject")
		return
	}
	appr, err := s.cfg.Approvals.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load approval")
		return
	}
	if appr == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "approval not found")
		return
	}
	if appr.Status != approvalstore.StatusPending {
		writeError(w, http.StatusConflict, "ALREADY_DECIDED", "this approval was already decided")
		return
	}
	if body.Decision == "approve" && s.cfg.WriteDB == nil {
		writeError(w, http.StatusConflict, "WRITES_DISABLED",
			"no write connection is configured (AI_CHAT_WRITE_DATABASE_URL); the proposal can only be rejected")
		return
	}

	newStatus := approvalstore.StatusRejected
	if body.Decision == "approve" {
		newStatus = approvalstore.StatusApproved
	}
	claimed, err := s.cfg.Approvals.Decide(r.Context(), appr.ID, newStatus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to record the decision")
		return
	}
	if !claimed {
		writeError(w, http.StatusConflict, "ALREADY_DECIDED", "this approval was already decided")
		return
	}
	appr, err = s.cfg.Approvals.Get(r.Context(), appr.ID)
	if err != nil || appr == nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to reload approval")
		return
	}

	// Execute on approve. The decision is already recorded: an execution
	// failure is reported in the response (and the source follow-ups), not
	// rolled back into pending.
	var result *chatstore.ResultSummary
	execErr := ""
	if newStatus == approvalstore.StatusApproved {
		started := time.Now()
		rows, err := s.cfg.WriteDB.ExecuteWrite(r.Context(), appr.SQL)
		elapsed := int(time.Since(started).Milliseconds())
		if err != nil {
			execErr = err.Error()
		} else {
			result = &chatstore.ResultSummary{RowCount: int(rows), Truncated: false, ExecutionTimeMs: elapsed}
		}
	}

	// Follow-ups survive a client disconnect.
	s.settleApprovalSource(context.WithoutCancel(r.Context()), *appr, result, execErr)

	response := map[string]any{"approval": toApprovalView(*appr)}
	if result != nil {
		response["result"] = result
	}
	if execErr != "" {
		response["error"] = execErr
	}
	writeJSON(w, http.StatusOK, response)
}

// settleApprovalSource propagates a decision to where the proposal came
// from: an outcome note on the source conversation, or a tool_result +
// requeue on the source task. Best-effort — failures are logged.
func (s *Server) settleApprovalSource(ctx context.Context, appr approvalstore.Approval, result *chatstore.ResultSummary, execErr string) {
	switch {
	case appr.MessageID != "":
		s.settleChatApproval(ctx, appr, result, execErr)
	case appr.TaskID != "":
		s.settleTaskApproval(ctx, appr, result, execErr)
	}
}

func approvalOutcomeText(appr approvalstore.Approval, result *chatstore.ResultSummary, execErr string) string {
	switch {
	case appr.Status == approvalstore.StatusRejected:
		return "The proposed write was rejected and was not executed."
	case execErr != "":
		return "The approved write failed: " + execErr
	case result != nil:
		return fmt.Sprintf("The approved write was executed (%d rows affected).", result.RowCount)
	default:
		return "The approved write was executed."
	}
}

func (s *Server) settleChatApproval(ctx context.Context, appr approvalstore.Approval, result *chatstore.ResultSummary, execErr string) {
	msg, err := s.cfg.Chat.GetMessage(ctx, appr.MessageID)
	if err != nil || msg == nil {
		log.Printf("approval %s: source message %s not found", appr.ID, appr.MessageID)
		return
	}
	if _, err := s.cfg.Chat.AppendMessage(ctx, chatstore.Message{
		ID:             uuid.NewString(),
		ConversationID: msg.ConversationID,
		Role:           "assistant",
		Text:           approvalOutcomeText(appr, result, execErr),
		SQL:            appr.SQL,
		Result:         result,
		Error:          execErr,
	}); err != nil {
		log.Printf("approval %s: append outcome message: %v", appr.ID, err)
	}
	if err := s.cfg.Chat.SetMessagePendingApproval(ctx, appr.MessageID, ""); err != nil {
		log.Printf("approval %s: clear pending marker: %v", appr.ID, err)
	}
}

// settleTaskApproval injects the decision as the tool_result of the task's
// pending propose_write exchange and requeues the task so the runner resumes
// it.
func (s *Server) settleTaskApproval(ctx context.Context, appr approvalstore.Approval, result *chatstore.ResultSummary, execErr string) {
	if s.cfg.Tasks == nil {
		return
	}
	task, err := s.cfg.Tasks.Get(ctx, appr.TaskID)
	if err != nil || task == nil {
		log.Printf("approval %s: source task %s not found", appr.ID, appr.TaskID)
		return
	}

	toolResult := map[string]any{"status": appr.Status}
	if appr.Status == approvalstore.StatusApproved {
		if execErr != "" {
			toolResult["error"] = execErr
		} else if result != nil {
			toolResult["rowsAffected"] = result.RowCount
		}
	}
	encoded, err := json.Marshal(toolResult)
	if err != nil {
		encoded = []byte(`{"status":"unknown"}`)
	}
	// The pending write is the last exchange (its Result is empty).
	for i := len(task.Transcript) - 1; i >= 0; i-- {
		if task.Transcript[i].Result == "" {
			task.Transcript[i].Result = string(encoded)
			break
		}
	}
	task.Steps = append(task.Steps, chatstore.Step{
		Index:   len(task.Steps),
		Kind:    "note",
		Summary: approvalOutcomeText(appr, result, execErr),
		SQL:     appr.SQL,
		Result:  result,
		Error:   execErr,
	})

	resumed := taskstore.Task{
		Status:     taskstore.StatusQueued,
		Steps:      task.Steps,
		Transcript: task.Transcript,
		// pending_approval_id cleared on resume.
	}
	ok, err := s.cfg.Tasks.Transition(ctx, task.ID, taskstore.StatusNeedsApproval, resumed)
	if err != nil {
		log.Printf("approval %s: requeue task %s: %v", appr.ID, task.ID, err)
		return
	}
	if !ok {
		// The task was canceled while the approval was pending; nothing to
		// resume.
		return
	}
	s.wakeTaskRunner()
}
