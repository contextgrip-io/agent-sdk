// Package approvalstore persists proposed writes awaiting a human decision.
// It shares the SQLite file at AI_CHAT_DB_PATH with the other stores.
package approvalstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/textutil"
)

// Statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// Approval is one proposed write. Exactly one of MessageID (chat source) or
// TaskID (board source) is set.
type Approval struct {
	ID        string
	SQL       string
	Rationale string
	Status    string
	MessageID string
	TaskID    string
	CreatedAt time.Time
	DecidedAt *time.Time
}

// Store is the SQLite-backed approval store.
type Store struct {
	db *sql.DB
}

const maxSQLChars = 100_000

// New opens the approval store at path.
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
CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  sql_text TEXT NOT NULL,
  rationale TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  source_message_id TEXT NOT NULL DEFAULT '',
  source_task_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  decided_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_approvals_status_created
ON approvals (status, created_at ASC);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init approvalstore schema: %w", err)
	}
	return nil
}

// Create stores a new pending approval.
func (s *Store) Create(ctx context.Context, appr Approval) (*Approval, error) {
	appr.Status = StatusPending
	appr.SQL = textutil.TruncateUTF8(appr.SQL, maxSQLChars)
	appr.Rationale = textutil.TruncateUTF8(appr.Rationale, 4_000)
	appr.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO approvals (id, sql_text, rationale, status, source_message_id, source_task_id, created_at, decided_at)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
`, appr.ID, appr.SQL, appr.Rationale, appr.Status, appr.MessageID, appr.TaskID,
		textutil.FormatSortable(appr.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create approval: %w", err)
	}
	return &appr, nil
}

// Get returns one approval, or (nil, nil) when absent.
func (s *Store) Get(ctx context.Context, id string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, sql_text, rationale, status, source_message_id, source_task_id, created_at, decided_at
FROM approvals WHERE id = ?
`, id)
	appr, err := scanApproval(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &appr, nil
}

// ListPending returns pending approvals, oldest first.
func (s *Store) ListPending(ctx context.Context) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, sql_text, rationale, status, source_message_id, source_task_id, created_at, decided_at
FROM approvals WHERE status = ? ORDER BY created_at ASC, id ASC
`, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("list pending approvals: %w", err)
	}
	defer rows.Close()
	var result []Approval
	for rows.Next() {
		appr, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, appr)
	}
	return result, rows.Err()
}

// Decide records the decision with a compare-and-set on pending status.
// Returns false when the approval was already decided (or does not exist).
func (s *Store) Decide(ctx context.Context, id, status string) (bool, error) {
	if status != StatusApproved && status != StatusRejected {
		return false, fmt.Errorf("invalid decision status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE approvals SET status = ?, decided_at = ? WHERE id = ? AND status = ?
`, status, textutil.FormatSortable(time.Now()), id, StatusPending)
	if err != nil {
		return false, fmt.Errorf("decide approval: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide approval rows: %w", err)
	}
	return affected == 1, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanApproval(row scanner) (Approval, error) {
	var appr Approval
	var createdAt string
	var decidedAt sql.NullString
	if err := row.Scan(&appr.ID, &appr.SQL, &appr.Rationale, &appr.Status,
		&appr.MessageID, &appr.TaskID, &createdAt, &decidedAt); err != nil {
		return Approval{}, err
	}
	var err error
	if appr.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Approval{}, fmt.Errorf("parse created_at: %w", err)
	}
	if decidedAt.Valid && decidedAt.String != "" {
		t, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return Approval{}, fmt.Errorf("parse decided_at: %w", err)
		}
		appr.DecidedAt = &t
	}
	return appr, nil
}
