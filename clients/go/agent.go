package aichat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Decision values accepted by DecideApproval.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Task status values.
const (
	TaskStatusQueued        = "queued"
	TaskStatusRunning       = "running"
	TaskStatusNeedsApproval = "needs_approval"
	TaskStatusDone          = "done"
	TaskStatusFailed        = "failed"
	TaskStatusCanceled      = "canceled"
)

// Step is one completed agent-mode tool step.
type Step struct {
	Index int `json:"index"`
	// Kind is "query", "schema", or "note".
	Kind string `json:"kind"`
	// Summary is a one-line description of the step.
	Summary string `json:"summary"`
	// SQL is the read-only SQL this step executed (kind "query").
	SQL    string         `json:"sql,omitempty"`
	Result *ResultSummary `json:"result,omitempty"`
	// Error is the step execution error, when it failed.
	Error string `json:"error,omitempty"`
}

// Approval is a proposed write awaiting (or carrying) a decision.
type Approval struct {
	ID string `json:"id"`
	// SQL is the exact statement that will run if approved.
	SQL string `json:"sql"`
	// Rationale is the model's stated reason for the write.
	Rationale string `json:"rationale,omitempty"`
	// Status is "pending", "approved", or "rejected".
	Status string `json:"status"`
	// Source says where the proposal came from; exactly one of its ids is
	// set.
	Source    ApprovalSource `json:"source"`
	CreatedAt time.Time      `json:"createdAt"`
	DecidedAt *time.Time     `json:"decidedAt,omitempty"`
}

// ApprovalSource identifies the origin of a proposed write.
type ApprovalSource struct {
	ConversationID string `json:"conversationId,omitempty"`
	MessageID      string `json:"messageId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
}

// DecideApprovalResult is the response of DecideApproval. Result and Error
// describe the execution outcome when the decision was "approve".
type DecideApprovalResult struct {
	Approval Approval       `json:"approval"`
	Result   *ResultSummary `json:"result,omitempty"`
	// Error is the execution error when the approved write failed.
	Error string `json:"error,omitempty"`
}

// Task is a board task run by the agent in the background.
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
	// Status is one of the TaskStatus* values.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Answer is the final answer when the task is done.
	Answer string `json:"answer,omitempty"`
	// Error is the failure reason when the task failed.
	Error string `json:"error,omitempty"`
}

// TaskDetail is one task with its transcript steps and any pending approval.
type TaskDetail struct {
	Task            Task      `json:"task"`
	Steps           []Step    `json:"steps"`
	PendingApproval *Approval `json:"pendingApproval,omitempty"`
}

// ListApprovals lists pending write approvals from both chat and board
// sources.
func (c *Client) ListApprovals(ctx context.Context) ([]Approval, error) {
	var out []Approval
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/approvals", nil, &out)
	return out, err
}

// DecideApproval approves or rejects a proposed write. Approving executes
// the exact proposed SQL against the write connection in a single
// transaction and returns the execution outcome in Result or Error.
//
// Decisions are idempotent-once: deciding an already-decided approval fails
// with *APIError code ALREADY_DECIDED (409), and approving without a
// configured write connection fails with WRITES_DISABLED (409).
func (c *Client) DecideApproval(ctx context.Context, id string, decision string) (DecideApprovalResult, error) {
	var out DecideApprovalResult
	if id == "" {
		return out, errors.New("aichat: approval id must not be empty")
	}
	if decision != DecisionApprove && decision != DecisionReject {
		return out, fmt.Errorf("aichat: decision must be %q or %q, got %q", DecisionApprove, DecisionReject, decision)
	}
	body := struct {
		Decision string `json:"decision"`
	}{Decision: decision}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/approvals/"+url.PathEscape(id), body, &out)
	return out, err
}

// CreateTask files a board task for the agent. Tasks run in the background
// through the same agent loop as agent-mode chat, one at a time, oldest
// first; proposed writes pause the task in needs_approval until decided.
// Requires the "board" feature.
func (c *Client) CreateTask(ctx context.Context, title, prompt string) (Task, error) {
	var out Task
	body := struct {
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
	}{Title: title, Prompt: prompt}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks", body, &out)
	return out, err
}

// ListTasks lists board tasks, most recently updated first. A non-empty
// status (one of the TaskStatus* values) filters the list; empty returns
// all tasks.
func (c *Client) ListTasks(ctx context.Context, status string) ([]Task, error) {
	path := "/api/v1/tasks"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var out []Task
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetTask fetches one task with its transcript steps and any pending
// approval.
func (c *Client) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	var out TaskDetail
	if id == "" {
		return out, errors.New("aichat: task id must not be empty")
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(id), nil, &out)
	return out, err
}

// CancelTask cancels a queued, running, or approval-blocked task and returns
// the canceled task. Canceling an already-finished task fails with *APIError
// code TASK_FINISHED (409).
func (c *Client) CancelTask(ctx context.Context, id string) (Task, error) {
	var out Task
	if id == "" {
		return out, errors.New("aichat: task id must not be empty")
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(id)+"/cancel", nil, &out)
	return out, err
}

// DeleteTask deletes a finished task (done, failed, or canceled). Deleting
// an active task fails with *APIError code TASK_ACTIVE (409).
func (c *Client) DeleteTask(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("aichat: task id must not be empty")
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/tasks/"+url.PathEscape(id), nil, nil)
}
