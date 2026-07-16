// Package trainingstore persists captured NL->SQL exchanges for training-data
// export. It shares the SQLite file at AI_CHAT_DB_PATH with the chat and
// token stores (its own table set), with the same WAL/busy_timeout
// treatment. Records are instance-global, plaintext, and never leave the
// machine except through an explicit export download.
package trainingstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/textutil"
)

// Record is one captured exchange: the natural-language intent, the generated
// SQL, and the bounded execution outcome (result summary or error), plus an
// optional explicit eval verdict.
type Record struct {
	ID              string
	Session         string // "ask" (one-shot) | "chat" (SSE); "" for eval-only inserts
	SourceMessageID string // assistant message id — the upsert key
	Verdict         string // "" | "good" | "bad"
	SQL             string
	Intent          string
	Columns         []string
	RowSample       [][]any
	RowCount        int
	Truncated       bool
	ExecutionTimeMs int
	ErrorMessage    string
	CreatedAt       time.Time
}

// Stats summarizes the stored records for /api/v1/training/stats.
type Stats struct {
	Records   int
	Evaluated int
	FirstAt   *time.Time
	LastAt    *time.Time
}

// Store is the SQLite-backed training-record store.
type Store struct {
	db         *sql.DB
	maxRecords int
}

// DefaultMaxRecords caps stored records; the oldest are pruned beyond it so
// silent capture cannot grow without bound (mirrors the reference store).
const DefaultMaxRecords = 5000

const (
	maxSQLChars       = 100_000
	maxSampleRows     = 20
	maxSampleCellSize = 256
	maxErrorChars     = 4_000
)

const captureSettingKey = "capture_enabled"

// Option customizes a Store (used by tests to shrink the cap).
type Option func(*Store)

// WithMaxRecords overrides the record cap.
func WithMaxRecords(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxRecords = n
		}
	}
}

// New opens the training store at path (WAL + busy_timeout applied per
// connection via the shared OpenDB helper).
func New(path string, opts ...Option) (*Store, error) {
	db, err := chatstore.OpenDB(path)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, maxRecords: DefaultMaxRecords}
	for _, opt := range opts {
		opt(store)
	}
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
CREATE TABLE IF NOT EXISTS training_records (
  id TEXT PRIMARY KEY,
  session TEXT NOT NULL DEFAULT '',
  source_message_id TEXT NOT NULL UNIQUE,
  verdict TEXT NOT NULL DEFAULT '',
  row_count INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0,
  execution_time_ms INTEGER NOT NULL DEFAULT 0,
  payload TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_training_records_created
ON training_records (created_at ASC, id ASC);
CREATE TABLE IF NOT EXISTS training_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init trainingstore schema: %w", err)
	}
	return nil
}

// CaptureEnabled reports the automatic-capture toggle. Capture defaults to
// ENABLED until explicitly turned off.
func (s *Store) CaptureEnabled(ctx context.Context) (bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT value FROM training_settings WHERE key = ?`, captureSettingKey)
	var value string
	if err := row.Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, fmt.Errorf("read training capture setting: %w", err)
	}
	return value != "0", nil
}

// SetCaptureEnabled updates the automatic-capture toggle.
func (s *Store) SetCaptureEnabled(ctx context.Context, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO training_settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, captureSettingKey, value, textutil.FormatSortable(time.Now()))
	if err != nil {
		return fmt.Errorf("write training capture setting: %w", err)
	}
	return nil
}

// payload holds the free-form parts of a record as JSON.
type recordPayload struct {
	SQL       string   `json:"sql"`
	Intent    string   `json:"intent,omitempty"`
	Columns   []string `json:"columns"`
	RowSample [][]any  `json:"rowSample,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Upsert stores a record keyed by SourceMessageID. When a record for the
// same assistant message already exists, only the verdict is updated (a
// re-rating must not clobber the originally captured exchange), and then
// only when the new record carries one. Prunes the oldest records beyond
// the cap.
func (s *Store) Upsert(ctx context.Context, rec Record) error {
	if strings.TrimSpace(rec.SourceMessageID) == "" {
		return fmt.Errorf("sourceMessageId is required")
	}
	sanitizeRecord(&rec)
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(recordPayload{
		SQL:       rec.SQL,
		Intent:    rec.Intent,
		Columns:   rec.Columns,
		RowSample: rec.RowSample,
		Error:     rec.ErrorMessage,
	})
	if err != nil {
		return fmt.Errorf("encode training record payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO training_records (
  id, session, source_message_id, verdict,
  row_count, truncated, execution_time_ms, payload, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_message_id) DO UPDATE SET
  verdict = CASE WHEN excluded.verdict != '' THEN excluded.verdict ELSE verdict END
`, rec.ID, rec.Session, rec.SourceMessageID, rec.Verdict,
		rec.RowCount, boolToInt(rec.Truncated), rec.ExecutionTimeMs, string(payload),
		textutil.FormatSortable(rec.CreatedAt))
	if err != nil {
		return fmt.Errorf("upsert training record: %w", err)
	}
	return s.pruneBeyondCap(ctx)
}

func (s *Store) pruneBeyondCap(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM training_records
WHERE id NOT IN (
  SELECT id FROM training_records
  ORDER BY created_at DESC, id DESC
  LIMIT ?
)
`, s.maxRecords)
	if err != nil {
		return fmt.Errorf("prune training records: %w", err)
	}
	return nil
}

// ListAll returns records oldest-first (chronological order for training
// dumps). When evaluatedOnly is set, only records carrying a verdict are
// returned.
func (s *Store) ListAll(ctx context.Context, evaluatedOnly bool) ([]Record, error) {
	query := `
SELECT id, session, source_message_id, verdict,
       row_count, truncated, execution_time_ms, payload, created_at
FROM training_records`
	if evaluatedOnly {
		query += ` WHERE verdict != ''`
	}
	query += ` ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list training records: %w", err)
	}
	defer rows.Close()
	var result []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// GetBySourceMessageID returns the record for an assistant message, or
// (nil, nil) when absent.
func (s *Store) GetBySourceMessageID(ctx context.Context, sourceMessageID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, session, source_message_id, verdict,
       row_count, truncated, execution_time_ms, payload, created_at
FROM training_records WHERE source_message_id = ?
`, sourceMessageID)
	rec, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// Stats returns record counts and the capture time range.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN verdict != '' THEN 1 ELSE 0 END), 0),
       MIN(created_at),
       MAX(created_at)
FROM training_records
`)
	var stats Stats
	var firstRaw, lastRaw sql.NullString
	if err := row.Scan(&stats.Records, &stats.Evaluated, &firstRaw, &lastRaw); err != nil {
		return Stats{}, fmt.Errorf("count training records: %w", err)
	}
	var err error
	if stats.FirstAt, err = parseNullTimestamp(firstRaw); err != nil {
		return Stats{}, fmt.Errorf("parse first training record timestamp: %w", err)
	}
	if stats.LastAt, err = parseNullTimestamp(lastRaw); err != nil {
		return Stats{}, fmt.Errorf("parse last training record timestamp: %w", err)
	}
	return stats, nil
}

// DeleteAll removes every training record and reports how many were removed.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM training_records`)
	if err != nil {
		return 0, fmt.Errorf("delete training records: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete training records rows: %w", err)
	}
	return deleted, nil
}

// ── Sanitization / scanning ─────────────────────────────────────────────────

// sanitizeRecord bounds stored payloads (same limits as the reference store);
// the API layer already bounds samples before they reach the store.
func sanitizeRecord(rec *Record) {
	rec.SQL = textutil.TruncateUTF8(rec.SQL, maxSQLChars)
	rec.Intent = textutil.TruncateUTF8(rec.Intent, maxSQLChars)
	rec.ErrorMessage = textutil.TruncateUTF8(rec.ErrorMessage, maxErrorChars)
	if len(rec.RowSample) > maxSampleRows {
		rec.RowSample = rec.RowSample[:maxSampleRows]
	}
	textutil.BoundCells(rec.RowSample, maxSampleCellSize)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(row scanner) (Record, error) {
	var rec Record
	var truncated int
	var payload, createdAt string
	if err := row.Scan(
		&rec.ID, &rec.Session, &rec.SourceMessageID, &rec.Verdict,
		&rec.RowCount, &truncated, &rec.ExecutionTimeMs, &payload, &createdAt,
	); err != nil {
		return Record{}, err
	}
	var decoded recordPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return Record{}, fmt.Errorf("decode training record payload: %w", err)
	}
	rec.SQL = decoded.SQL
	rec.Intent = decoded.Intent
	rec.Columns = decoded.Columns
	rec.RowSample = decoded.RowSample
	rec.ErrorMessage = decoded.Error
	rec.Truncated = truncated != 0
	var err error
	if rec.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Record{}, fmt.Errorf("parse created_at: %w", err)
	}
	return rec, nil
}

func parseNullTimestamp(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
