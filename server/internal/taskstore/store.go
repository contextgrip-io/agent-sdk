// Package taskstore persists board tasks: background agent runs with a
// serialized tool transcript so a task can pause on a proposed write and
// resume after the approval decision. It shares the SQLite file at
// AI_CHAT_DB_PATH with the other stores.
package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/textutil"
)

// Statuses.
const (
	StatusQueued        = "queued"
	StatusRunning       = "running"
	StatusNeedsApproval = "needs_approval"
	StatusDone          = "done"
	StatusFailed        = "failed"
	StatusCanceled      = "canceled"
)

// FinishedStatuses are the terminal states (deletable; not cancelable).
var FinishedStatuses = []string{StatusDone, StatusFailed, StatusCanceled}

// Task is one board task.
type Task struct {
	ID                string
	Title             string
	Prompt            string
	Status            string
	Answer            string
	Error             string
	Steps             []chatstore.Step
	Transcript        []assistant.AgentExchange
	PendingApprovalID string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Store is the SQLite-backed task store.
type Store struct {
	db *sql.DB
}

// New opens the task store at path.
func New(path string) (*Store, error) {
	db, err := chatstore.OpenDB(path)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  prompt TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  answer TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  steps TEXT NOT NULL DEFAULT '[]',
  transcript TEXT NOT NULL DEFAULT '[]',
  pending_approval_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_created
ON tasks (status, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_tasks_updated
ON tasks (updated_at DESC, id DESC);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init taskstore schema: %w", err)
	}
	return nil
}

// Create files a new queued task.
func (s *Store) Create(ctx context.Context, id, title, prompt string) (*Task, error) {
	now := time.Now().UTC()
	task := Task{
		ID:        id,
		Title:     textutil.TruncateUTF8(title, 200),
		Prompt:    textutil.TruncateUTF8(prompt, 4_000),
		Status:    StatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tasks (id, title, prompt, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
`, task.ID, task.Title, task.Prompt, task.Status,
		textutil.FormatSortable(now), textutil.FormatSortable(now))
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	return &task, nil
}

// Get returns one task, or (nil, nil) when absent.
func (s *Store) Get(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, selectTask+` WHERE id = ?`, id)
	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

// List returns tasks most recently updated first, optionally filtered by
// status.
func (s *Store) List(ctx context.Context, status string) ([]Task, error) {
	query := selectTask
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var result []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

// ClaimNextQueued atomically claims the oldest queued task, marking it
// running. Returns (nil, nil) when the queue is empty.
func (s *Store) ClaimNextQueued(ctx context.Context) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `
UPDATE tasks SET status = ?, updated_at = ?
WHERE id = (
  SELECT id FROM tasks WHERE status = ? ORDER BY created_at ASC, id ASC LIMIT 1
) AND status = ?
RETURNING id
`, StatusRunning, textutil.FormatSortable(time.Now()), StatusQueued, StatusQueued)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim queued task: %w", err)
	}
	return s.Get(ctx, id)
}

// SaveProgress persists steps and transcript mid-run (status untouched).
func (s *Store) SaveProgress(ctx context.Context, id string, steps []chatstore.Step, transcript []assistant.AgentExchange) error {
	stepsJSON, transcriptJSON, err := encodeProgress(steps, transcript)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE tasks SET steps = ?, transcript = ?, updated_at = ? WHERE id = ?
`, stepsJSON, transcriptJSON, textutil.FormatSortable(time.Now()), id)
	if err != nil {
		return fmt.Errorf("save task progress: %w", err)
	}
	return nil
}

// Transition performs a compare-and-set status change with the given final
// fields, returning false when the task was not in fromStatus (e.g. it was
// canceled underneath the runner).
func (s *Store) Transition(ctx context.Context, id, fromStatus string, task Task) (bool, error) {
	stepsJSON, transcriptJSON, err := encodeProgress(task.Steps, task.Transcript)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET status = ?, answer = ?, error = ?, steps = ?, transcript = ?, pending_approval_id = ?, updated_at = ?
WHERE id = ? AND status = ?
`, task.Status, textutil.TruncateUTF8(task.Answer, 100_000), textutil.TruncateUTF8(task.Error, 4_000),
		stepsJSON, transcriptJSON, task.PendingApprovalID,
		textutil.FormatSortable(time.Now()), id, fromStatus)
	if err != nil {
		return false, fmt.Errorf("transition task: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition task rows: %w", err)
	}
	return affected == 1, nil
}

// SetStatus performs a bare compare-and-set status change from any of the
// given statuses; returns false when the task was in none of them.
func (s *Store) SetStatus(ctx context.Context, id, toStatus string, fromStatuses ...string) (bool, error) {
	if len(fromStatuses) == 0 {
		return false, fmt.Errorf("fromStatuses required")
	}
	query := `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status IN (?` +
		repeat(",?", len(fromStatuses)-1) + `)`
	args := []any{toStatus, textutil.FormatSortable(time.Now()), id}
	for _, st := range fromStatuses {
		args = append(args, st)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("set task status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set task status rows: %w", err)
	}
	return affected == 1, nil
}

// RequeueRunning resets tasks stuck in running (e.g. after a crash) back to
// queued. Called once at runner startup.
func (s *Store) RequeueRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE tasks SET status = ?, updated_at = ? WHERE status = ?
`, StatusQueued, textutil.FormatSortable(time.Now()), StatusRunning)
	if err != nil {
		return fmt.Errorf("requeue running tasks: %w", err)
	}
	return nil
}

// Delete removes a finished task; returns false when the task exists but is
// not in a terminal state.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM tasks WHERE id = ? AND status IN (?, ?, ?)
`, id, StatusDone, StatusFailed, StatusCanceled)
	if err != nil {
		return false, fmt.Errorf("delete task: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete task rows: %w", err)
	}
	if affected == 1 {
		return true, nil
	}
	task, err := s.Get(ctx, id)
	if err != nil {
		return false, err
	}
	// Unknown tasks delete idempotently; live ones refuse.
	return task == nil, nil
}

const selectTask = `
SELECT id, title, prompt, status, answer, error, steps, transcript, pending_approval_id, created_at, updated_at
FROM tasks`

func encodeProgress(steps []chatstore.Step, transcript []assistant.AgentExchange) (string, string, error) {
	if steps == nil {
		steps = []chatstore.Step{}
	}
	if transcript == nil {
		transcript = []assistant.AgentExchange{}
	}
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return "", "", fmt.Errorf("encode task steps: %w", err)
	}
	transcriptJSON, err := json.Marshal(transcript)
	if err != nil {
		return "", "", fmt.Errorf("encode task transcript: %w", err)
	}
	return string(stepsJSON), string(transcriptJSON), nil
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (Task, error) {
	var task Task
	var stepsJSON, transcriptJSON, createdAt, updatedAt string
	if err := row.Scan(&task.ID, &task.Title, &task.Prompt, &task.Status, &task.Answer, &task.Error,
		&stepsJSON, &transcriptJSON, &task.PendingApprovalID, &createdAt, &updatedAt); err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &task.Steps); err != nil {
		return Task{}, fmt.Errorf("decode task steps: %w", err)
	}
	if err := json.Unmarshal([]byte(transcriptJSON), &task.Transcript); err != nil {
		return Task{}, fmt.Errorf("decode task transcript: %w", err)
	}
	var err error
	if task.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Task{}, fmt.Errorf("parse created_at: %w", err)
	}
	if task.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Task{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return task, nil
}
