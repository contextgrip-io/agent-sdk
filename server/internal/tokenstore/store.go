// Package tokenstore persists named API tokens. Raw token values are never
// stored — only the SHA-256 hash and a short fingerprint for display.
package tokenstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/textutil"
)

// Token is the stored metadata for one named API token.
type Token struct {
	ID          string
	Label       string
	Fingerprint string // first 8 hex chars of SHA-256(raw token)
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

// Store is the SQLite-backed token store. It may share a database file with
// the chatstore (distinct table).
type Store struct {
	db *sql.DB
}

// New opens the token store at path (WAL + busy_timeout applied per
// connection via the shared OpenDB helper).
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
CREATE TABLE IF NOT EXISTS api_tokens (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  fingerprint TEXT NOT NULL,
  created_at TEXT NOT NULL,
  last_used_at TEXT
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("init tokenstore schema: %w", err)
	}
	return nil
}

// HashToken returns the lowercase hex SHA-256 of a raw token value.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create mints a new named token. The raw value is returned exactly once and
// stored only as a hash.
func (s *Store) Create(ctx context.Context, label string) (*Token, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}
	raw := hex.EncodeToString(buf)
	hash := HashToken(raw)
	now := time.Now().UTC()
	token := Token{
		ID:          uuid.NewString(),
		Label:       label,
		Fingerprint: hash[:8],
		CreatedAt:   now,
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO api_tokens (id, label, token_hash, fingerprint, created_at, last_used_at)
VALUES (?, ?, ?, ?, ?, NULL)
`, token.ID, token.Label, hash, token.Fingerprint, textutil.FormatSortable(now))
	if err != nil {
		return nil, "", fmt.Errorf("store token: %w", err)
	}
	return &token, raw, nil
}

// List returns all named tokens, newest first.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, label, fingerprint, created_at, last_used_at
FROM api_tokens ORDER BY created_at DESC, id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var result []Token
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

// FindByHash looks a token up by its hex SHA-256 hash; (nil, nil) when
// absent.
func (s *Store) FindByHash(ctx context.Context, hash string) (*Token, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, label, fingerprint, created_at, last_used_at
FROM api_tokens WHERE token_hash = ?
`, hash)
	token, err := scanToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &token, nil
}

// Delete revokes a token. Deleting an unknown id is not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}

// TouchLastUsed records a use of the token. Callers treat failures as
// best-effort.
func (s *Store) TouchLastUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE api_tokens SET last_used_at = ? WHERE id = ?
`, textutil.FormatSortable(time.Now().UTC()), id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanToken(row scanner) (Token, error) {
	var token Token
	var createdAt string
	var lastUsedAt sql.NullString
	if err := row.Scan(&token.ID, &token.Label, &token.Fingerprint, &createdAt, &lastUsedAt); err != nil {
		return Token{}, err
	}
	var err error
	if token.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Token{}, fmt.Errorf("parse created_at: %w", err)
	}
	if lastUsedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, lastUsedAt.String)
		if err != nil {
			return Token{}, fmt.Errorf("parse last_used_at: %w", err)
		}
		token.LastUsedAt = &t
	}
	return token, nil
}
