package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/contextgrip-io/ai-chat/server/internal/approvalstore"
	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/taskstore"
)

// taskView matches the Task schema.
type taskView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Prompt    string `json:"prompt"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Answer    string `json:"answer,omitempty"`
	Error     string `json:"error,omitempty"`
}

func toTaskView(task taskstore.Task) taskView {
	return taskView{
		ID:        task.ID,
		Title:     task.Title,
		Prompt:    task.Prompt,
		Status:    task.Status,
		CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: task.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Answer:    task.Answer,
		Error:     task.Error,
	}
}

var taskStatuses = map[string]bool{
	taskstore.StatusQueued: true, taskstore.StatusRunning: true, taskstore.StatusNeedsApproval: true,
	taskstore.StatusDone: true, taskstore.StatusFailed: true, taskstore.StatusCanceled: true,
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !taskStatuses[status] {
		writeError(w, http.StatusBadRequest, "VALIDATION", "unknown status filter")
		return
	}
	rows, err := s.cfg.Tasks.List(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to list tasks")
		return
	}
	out := make([]taskView, 0, len(rows))
	for _, task := range rows {
		out = append(out, toTaskView(task))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"tasks are not available: ANTHROPIC_API_KEY and DATABASE_URL are required")
		return
	}
	var body struct {
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid request body")
		return
	}
	title := strings.TrimSpace(body.Title)
	prompt := strings.TrimSpace(body.Prompt)
	switch {
	case title == "":
		writeError(w, http.StatusBadRequest, "VALIDATION", "title is required")
		return
	case len(title) > 200:
		writeError(w, http.StatusBadRequest, "VALIDATION", "title is too long (max 200 characters)")
		return
	case prompt == "":
		writeError(w, http.StatusBadRequest, "VALIDATION", "prompt is required")
		return
	case len(prompt) > maxQuestionChars:
		writeError(w, http.StatusBadRequest, "VALIDATION", "prompt is too long")
		return
	}
	task, err := s.cfg.Tasks.Create(r.Context(), uuid.NewString(), title, prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to file task")
		return
	}
	s.wakeTaskRunner()
	writeJSON(w, http.StatusCreated, toTaskView(*task))
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.cfg.Tasks.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load task")
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return
	}
	detail := map[string]any{
		"task":  toTaskView(*task),
		"steps": task.Steps,
	}
	if task.PendingApprovalID != "" {
		if appr, err := s.cfg.Approvals.Get(r.Context(), task.PendingApprovalID); err == nil && appr != nil {
			detail["pendingApproval"] = toApprovalView(*appr)
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.cfg.Tasks.Delete(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to delete task")
		return
	}
	if !deleted {
		writeError(w, http.StatusConflict, "TASK_ACTIVE", "the task is still active — cancel it first")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// handleCancelTask cancels a task: queued flips immediately, running is
// cooperatively stopped between steps, needs_approval also auto-rejects its
// pending approval.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.cfg.Tasks.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load task")
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return
	}
	ok, err := s.cfg.Tasks.SetStatus(r.Context(), task.ID, taskstore.StatusCanceled,
		taskstore.StatusQueued, taskstore.StatusRunning, taskstore.StatusNeedsApproval)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to cancel task")
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "TASK_FINISHED", "the task already finished")
		return
	}
	// A task waiting on an approval takes the proposal down with it.
	if task.Status == taskstore.StatusNeedsApproval && task.PendingApprovalID != "" {
		if _, err := s.cfg.Approvals.Decide(context.WithoutCancel(r.Context()),
			task.PendingApprovalID, approvalstore.StatusRejected); err != nil {
			log.Printf("cancel task %s: auto-reject approval %s: %v", task.ID, task.PendingApprovalID, err)
		}
	}
	task, err = s.cfg.Tasks.Get(r.Context(), task.ID)
	if err != nil || task == nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to reload task")
		return
	}
	writeJSON(w, http.StatusOK, toTaskView(*task))
}

// ── background runner ───────────────────────────────────────────────────────

// taskPollInterval is the fallback poll cadence when no wake signal arrives
// (test-tunable).
var taskPollInterval = 2 * time.Second

// wakeTaskRunner nudges the runner (task filed, approval decided).
func (s *Server) wakeTaskRunner() {
	select {
	case s.taskWake <- struct{}{}:
	default:
	}
}

// RunTaskWorker is the single background task runner: it claims queued tasks
// oldest-first and runs each through the agent loop, until ctx is canceled
// (graceful shutdown). Call it from main in its own goroutine when the
// "board" feature is enabled.
func (s *Server) RunTaskWorker(ctx context.Context) {
	if s.cfg.Tasks == nil {
		return
	}
	// Tasks stuck in running from a previous crash get another run.
	if err := s.cfg.Tasks.RequeueRunning(ctx); err != nil {
		log.Printf("task runner: requeue running: %v", err)
	}
	for {
		task, err := s.cfg.Tasks.ClaimNextQueued(ctx)
		if err != nil {
			log.Printf("task runner: claim: %v", err)
		}
		if task != nil {
			s.runTask(ctx, *task)
			continue // drain the queue before sleeping
		}
		select {
		case <-ctx.Done():
			return
		case <-s.taskWake:
		case <-time.After(taskPollInterval):
		}
	}
}

// runTask executes one claimed task through the agent loop.
func (s *Server) runTask(ctx context.Context, task taskstore.Task) {
	// Store writes survive shutdown teardown while the loop itself follows
	// ctx.
	persistCtx := context.WithoutCancel(ctx)

	if !s.configured() {
		_, _ = s.cfg.Tasks.Transition(persistCtx, task.ID, taskstore.StatusRunning, taskstore.Task{
			Status: taskstore.StatusFailed, Steps: task.Steps, Transcript: task.Transcript,
			Error: "the model or database is not configured",
		})
		return
	}

	steps := task.Steps
	transcript := task.Transcript

	outcome, loopErr := s.runAgent(ctx, persistCtx, agentRun{
		prompt:       task.Prompt,
		exchanges:    transcript,
		steps:        steps,
		sourceTaskID: task.ID,
		onStep: func(step chatstore.Step, ex assistant.AgentExchange) {
			steps = append(steps, step)
			transcript = append(transcript, ex)
			// Persist progress as each step completes, and capture
			// successful queries for training (session "task").
			if err := s.cfg.Tasks.SaveProgress(persistCtx, task.ID, steps, transcript); err != nil {
				log.Printf("task %s: save progress: %v", task.ID, err)
			}
			if step.Kind == "query" && step.Error == "" && step.SQL != "" {
				s.captureTrainingStep(persistCtx, "task",
					taskStepSourceID(task.ID, step.Index), task.Prompt, step)
			}
		},
		checkCancel: func() bool {
			current, err := s.cfg.Tasks.Get(ctx, task.ID)
			return err == nil && current != nil && current.Status == taskstore.StatusCanceled
		},
	})
	// runAgent appends to its own copies; persist the authoritative ones
	// from the outcome where available.
	switch {
	case loopErr != nil && loopErr.code == errAgentCanceled:
		// The cancel endpoint already set the status; persist progress only.
		_ = s.cfg.Tasks.SaveProgress(persistCtx, task.ID, steps, transcript)

	case loopErr != nil:
		if _, err := s.cfg.Tasks.Transition(persistCtx, task.ID, taskstore.StatusRunning, taskstore.Task{
			Status: taskstore.StatusFailed, Steps: steps, Transcript: transcript, Error: loopErr.message,
		}); err != nil {
			log.Printf("task %s: mark failed: %v", task.ID, err)
		}

	case outcome.approval != nil:
		if _, err := s.cfg.Tasks.Transition(persistCtx, task.ID, taskstore.StatusRunning, taskstore.Task{
			Status: taskstore.StatusNeedsApproval, Steps: outcome.steps, Transcript: outcome.exchanges,
			PendingApprovalID: outcome.approval.ID,
		}); err != nil {
			log.Printf("task %s: mark needs_approval: %v", task.ID, err)
		}

	default:
		if _, err := s.cfg.Tasks.Transition(persistCtx, task.ID, taskstore.StatusRunning, taskstore.Task{
			Status: taskstore.StatusDone, Steps: outcome.steps, Transcript: outcome.exchanges,
			Answer: outcome.answer,
		}); err != nil {
			log.Printf("task %s: mark done: %v", task.ID, err)
		}
	}
}

// taskStepSourceID is the training-record dedupe key for a task step.
func taskStepSourceID(taskID string, index int) string {
	return taskID + ":" + strconv.Itoa(index)
}
