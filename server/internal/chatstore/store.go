// Package chatstore persists conversations and messages in SQLite.
// Conversations are instance-global (single-database, bearer-token
// deployment — no per-user scoping). Payloads are stored as plaintext JSON.
package chatstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/contextgrip-io/agent-sdk/server/internal/textutil"
)

// Conversation is one chat thread. Titles are derived from the first
// question, truncated.
type Conversation struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ResultSummary is the bounded query-result snapshot stored with an
// assistant message so a conversation can be re-rendered without re-running
// the query.
type ResultSummary struct {
	Columns         []string `json:"columns,omitempty"`
	RowSample       [][]any  `json:"rowSample,omitempty"`
	RowCount        int      `json:"rowCount"`
	Truncated       bool     `json:"truncated"`
	ExecutionTimeMs int      `json:"executionTimeMs"`
}

// Message is one user or assistant turn.
type Message struct {
	ID             string
	ConversationID string
	Seq            int
	Role           string // "user" | "assistant"
	Text           string
	SQL            string
	Result         *ResultSummary
	Error          string
	CreatedAt      time.Time
}

// Store is the SQLite-backed conversation store.
type Store struct {
	db               *sql.DB
	maxConversations int
	maxMessages      int
}

const (
	// DefaultMaxConversations caps stored conversations; the oldest are
	// pruned when the cap is exceeded.
	DefaultMaxConversations = 500
	// DefaultMaxMessagesPerConversation caps messages per conversation;
	// appends beyond it fail with ErrConversationFull.
	DefaultMaxMessagesPerConversation = 200

	maxTextChars      = 100_000
	maxSampleRows     = 20
	maxSampleCellSize = 256
)

// ErrConversationFull is returned when a conversation reaches the message cap.
var ErrConversationFull = errors.New("conversation is full")

// Option customizes a Store (used by tests to shrink the caps).
type Option func(*Store)

// WithLimits overrides the conversation and per-conversation message caps.
func WithLimits(maxConversations, maxMessagesPerConversation int) Option {
	return func(s *Store) {
		if maxConversations > 0 {
			s.maxConversations = maxConversations
		}
		if maxMessagesPerConversation > 0 {
			s.maxMessages = maxMessagesPerConversation
		}
	}
}

// OpenDB opens (creating if needed) the SQLite file at path with WAL and a
// busy timeout applied to every pooled connection via DSN pragmas. The
// chatstore and tokenstore may share one file — one file, two table sets.
func OpenDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
	}
	// _pragma DSN options apply per-connection, so every connection in the
	// database/sql pool gets journal_mode=WAL and busy_timeout=5000 — a
	// bare db.Exec("PRAGMA ...") would only configure one pooled conn.
	dsn := "file:" + url.PathEscape(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	return db, nil
}

// New opens the chat store at path.
func New(path string, opts ...Option) (*Store, error) {
	db, err := OpenDB(path)
	if err != nil {
		return nil, err
	}
	store := &Store{
		db:               db,
		maxConversations: DefaultMaxConversations,
		maxMessages:      DefaultMaxMessagesPerConversation,
	}
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
CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversations_updated
ON conversations (updated_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation
ON messages (conversation_id, seq ASC);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init chatstore schema: %w", err)
	}
	return nil
}

// CreateConversation inserts a new conversation and prunes the oldest ones
// beyond the cap.
func (s *Store) CreateConversation(ctx context.Context, id, title string) (*Conversation, error) {
	now := time.Now().UTC()
	conv := Conversation{
		ID:        id,
		Title:     textutil.TruncateUTF8(title, 120),
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO conversations (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)
`, conv.ID, conv.Title, textutil.FormatSortable(now), textutil.FormatSortable(now))
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	if err := s.pruneConversations(ctx); err != nil {
		return nil, err
	}
	return &conv, nil
}

func (s *Store) pruneConversations(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM conversations
WHERE id NOT IN (
  SELECT id FROM conversations
  ORDER BY updated_at DESC, id DESC
  LIMIT ?
)
`, s.maxConversations)
	if err != nil {
		return fmt.Errorf("list prunable conversations: %w", err)
	}
	defer rows.Close()
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan prunable conversation: %w", err)
		}
		expired = append(expired, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range expired {
		if err := s.DeleteConversation(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// GetConversation returns the conversation or (nil, nil) when absent.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, title, created_at, updated_at FROM conversations WHERE id = ?
`, id)
	conv, err := scanConversation(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &conv, nil
}

// ListConversations returns all conversations, most recently updated first.
func (s *Store) ListConversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, title, created_at, updated_at FROM conversations
ORDER BY updated_at DESC, id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var result []Conversation
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, conv)
	}
	return result, rows.Err()
}

// DeleteConversation removes a conversation and its messages. Deleting an
// unknown id is not an error.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
		return fmt.Errorf("delete conversation messages: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
}

// AppendMessage assigns the next sequence number, stores the message, and
// bumps the conversation's updated_at. Returns ErrConversationFull at the
// message cap.
func (s *Store) AppendMessage(ctx context.Context, msg Message) (*Message, error) {
	sanitizeMessage(&msg)
	now := time.Now().UTC()
	msg.CreatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin append message: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM messages WHERE conversation_id = ?
`, msg.ConversationID)
	var maxSeq, count int
	if err := row.Scan(&maxSeq, &count); err != nil {
		return nil, fmt.Errorf("read conversation sequence: %w", err)
	}
	if count >= s.maxMessages {
		return nil, ErrConversationFull
	}
	msg.Seq = maxSeq + 1

	payload, err := json.Marshal(messagePayload{Text: msg.Text, SQL: msg.SQL, Result: msg.Result, Error: msg.Error})
	if err != nil {
		return nil, fmt.Errorf("encode message payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages (id, conversation_id, seq, role, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?)
`, msg.ID, msg.ConversationID, msg.Seq, msg.Role, string(payload), textutil.FormatSortable(now)); err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conversations SET updated_at = ? WHERE id = ?
`, textutil.FormatSortable(now), msg.ConversationID); err != nil {
		return nil, fmt.Errorf("touch conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit append message: %w", err)
	}
	return &msg, nil
}

// GetMessage returns one message by id, or (nil, nil) when absent.
func (s *Store) GetMessage(ctx context.Context, id string) (*Message, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, conversation_id, seq, role, payload, created_at
FROM messages WHERE id = ?
`, id)
	msg, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &msg, nil
}

// ListMessages returns a conversation's messages in order.
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, conversation_id, seq, role, payload, created_at
FROM messages WHERE conversation_id = ?
ORDER BY seq ASC
`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var result []Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, msg)
	}
	return result, rows.Err()
}

// ── Sanitization / scanning ─────────────────────────────────────────────────

// sanitizeMessage bounds stored payloads as defense in depth; the API layer
// already bounds samples before they reach the store.
func sanitizeMessage(msg *Message) {
	msg.Text = textutil.TruncateUTF8(msg.Text, maxTextChars)
	msg.SQL = textutil.TruncateUTF8(msg.SQL, maxTextChars)
	if msg.Result == nil {
		return
	}
	if len(msg.Result.RowSample) > maxSampleRows {
		msg.Result.RowSample = msg.Result.RowSample[:maxSampleRows]
	}
	textutil.BoundCells(msg.Result.RowSample, maxSampleCellSize)
}

type messagePayload struct {
	Text   string         `json:"text,omitempty"`
	SQL    string         `json:"sql,omitempty"`
	Result *ResultSummary `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanConversation(row scanner) (Conversation, error) {
	var conv Conversation
	var createdAt, updatedAt string
	if err := row.Scan(&conv.ID, &conv.Title, &createdAt, &updatedAt); err != nil {
		return Conversation{}, err
	}
	var err error
	if conv.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Conversation{}, fmt.Errorf("parse created_at: %w", err)
	}
	if conv.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Conversation{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return conv, nil
}

func scanMessage(row scanner) (Message, error) {
	var msg Message
	var payload, createdAt string
	if err := row.Scan(&msg.ID, &msg.ConversationID, &msg.Seq, &msg.Role, &payload, &createdAt); err != nil {
		return Message{}, err
	}
	var decoded messagePayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return Message{}, fmt.Errorf("decode message payload: %w", err)
	}
	msg.Text = decoded.Text
	msg.SQL = decoded.SQL
	msg.Result = decoded.Result
	msg.Error = decoded.Error
	var err error
	if msg.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Message{}, fmt.Errorf("parse created_at: %w", err)
	}
	return msg, nil
}
