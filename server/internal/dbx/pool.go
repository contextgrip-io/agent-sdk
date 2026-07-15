package dbx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a lazily-created pgx pool over DATABASE_URL. The pool is created
// on first use and reused; creation failures are not sticky, so a database
// that comes up later is picked up on the next call.
type DB struct {
	url string

	mu   sync.Mutex
	pool *pgxpool.Pool
}

// Open prepares a lazy DB handle. No connection is attempted here.
func Open(url string) *DB {
	return &DB{url: url}
}

func (d *DB) getPool(ctx context.Context) (*pgxpool.Pool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pool != nil {
		return d.pool, nil
	}
	cfg, err := pgxpool.ParseConfig(d.url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}
	d.pool = pool
	return pool, nil
}

// Ping verifies the database is reachable.
func (d *DB) Ping(ctx context.Context) error {
	pool, err := d.getPool(ctx)
	if err != nil {
		return err
	}
	return pool.Ping(ctx)
}

// SchemaContext introspects the database and renders a bounded,
// model-friendly schema summary.
func (d *DB) SchemaContext(ctx context.Context, maxChars int) (string, error) {
	pool, err := d.getPool(ctx)
	if err != nil {
		return "", err
	}
	tree, err := FetchTree(ctx, pool)
	if err != nil {
		return "", err
	}
	return BuildSchemaContext(tree, maxChars), nil
}

// RunReadOnly executes one verified read-only statement inside a READ ONLY
// transaction with a statement timeout.
func (d *DB) RunReadOnly(ctx context.Context, sql string, limit int, timeout time.Duration) (*QueryResult, error) {
	pool, err := d.getPool(ctx)
	if err != nil {
		return nil, err
	}
	return RunQueryReadOnly(ctx, pool, sql, limit, timeout)
}

// ExecuteWrite runs one approved statement in a plain transaction with a
// statement timeout and reports the rows affected. The statement must be a
// single statement (raw interior-semicolon rule); there is deliberately no
// read-only requirement — this is the approval execution path, pointed at
// AI_CHAT_WRITE_DATABASE_URL.
func (d *DB) ExecuteWrite(ctx context.Context, sql string) (int64, error) {
	if err := VerifySingleStatement(sql); err != nil {
		return 0, err
	}
	pool, err := d.getPool(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin write transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", (30*time.Second).Milliseconds())); err != nil {
		return 0, fmt.Errorf("set statement timeout: %w", err)
	}
	tag, err := tx.Exec(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("execute write: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit write: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Close releases the pool if one was created.
func (d *DB) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pool != nil {
		d.pool.Close()
		d.pool = nil
	}
}
