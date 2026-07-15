package dbx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VerifyReadOnlySQL rejects statements that are not a single read-only
// SELECT (or WITH ... SELECT).
//
// This is a BEST-EFFORT static gate for model-generated SQL, layered on top
// of the real guarantees: RunQueryReadOnly executes inside a READ ONLY
// transaction with a statement timeout, and the deployment is expected to
// use a read-only database role (the hard boundary — a READ ONLY tx alone
// does not block volatile admin functions).
//
// The rules, in order:
//
//  1. Trim whitespace and at most ONE trailing semicolon from the raw text.
//  2. Reject if the remaining RAW string contains ANY semicolon, regardless
//     of quoting. This deliberately over-rejects legitimate quoted
//     semicolons (e.g. SELECT 'a;b' or dollar-quoted $$a;b$$) because
//     quote-aware scanning is exactly what attackers desynchronize — a
//     PostgreSQL E'...' escape-string literal fools a scanner that treats
//     backslash as inert, letting SELECT E'\' followed by '; DELETE FROM t;
//     smuggle a second statement past it (see the verifier tests). Raw
//     rejection cannot be desynchronized.
//  3. Strip comments and blank quoted regions (normalizeForAnalysis), then
//     require the statement to start with SELECT or WITH.
func VerifyReadOnlySQL(sql string) error {
	raw := strings.TrimSpace(sql)
	raw = strings.TrimSpace(strings.TrimSuffix(raw, ";"))
	if raw == "" {
		return fmt.Errorf("empty statement")
	}
	// Rule 2: multi-statement gate on the RAW text. No quote tracking on
	// purpose — see the doc comment above.
	if strings.Contains(raw, ";") {
		return fmt.Errorf("multiple statements are not allowed (semicolons are rejected even inside quoted text)")
	}
	analyzable := strings.TrimSpace(normalizeForAnalysis(raw))
	if analyzable == "" {
		return fmt.Errorf("empty statement")
	}
	upper := strings.ToUpper(analyzable)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return fmt.Errorf("only read-only SELECT statements are allowed")
	}
	return nil
}

// normalizeForAnalysis removes comments and blanks the contents of string
// literals and quoted identifiers, so the leading-keyword check cannot be
// fooled by comment or quoted text. It is used only for the prefix check;
// the multi-statement gate runs on the raw text (see VerifyReadOnlySQL).
func normalizeForAnalysis(sql string) string {
	var out strings.Builder
	inLine, inBlock, inSingle, inDouble := false, false, false, false
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out.WriteRune(c)
			}
		case inBlock:
			if c == '*' && next == '/' {
				inBlock = false
				i++
			}
		case inSingle:
			if c == '\'' {
				if next == '\'' { // escaped quote inside literal
					i++
					continue
				}
				inSingle = false
				out.WriteRune(c)
			}
		case inDouble:
			if c == '"' {
				inDouble = false
				out.WriteRune(c)
			}
		case c == '-' && next == '-':
			inLine = true
			i++
		case c == '/' && next == '*':
			inBlock = true
			i++
		case c == '\'':
			inSingle = true
			out.WriteRune(c)
		case c == '"':
			inDouble = true
			out.WriteRune(c)
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// RunQueryReadOnly executes a statement inside a READ ONLY transaction with a
// statement timeout, so model-generated SQL cannot mutate data or run away.
// The static verifier runs first as defense in depth, but the transaction
// access mode (plus a read-only DB role) is the real guarantee.
func RunQueryReadOnly(ctx context.Context, pool *pgxpool.Pool, sql string, limit int, timeout time.Duration) (*QueryResult, error) {
	if err := VerifyReadOnlySQL(sql); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
		return nil, fmt.Errorf("set statement timeout: %w", err)
	}

	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	return scanQueryRows(rows, limit)
}

// scanQueryRows collects up to limit rows from a pgx result set.
func scanQueryRows(rows pgx.Rows, limit int) (*QueryResult, error) {
	fieldDescs := rows.FieldDescriptions()
	columns := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		columns[i] = fd.Name
	}

	var resultRows [][]any
	count := 0
	truncated := false

	for rows.Next() {
		if count >= limit {
			truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan values: %w", err)
		}
		resultRows = append(resultRows, values)
		count++
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	if resultRows == nil {
		resultRows = [][]any{}
	}

	return &QueryResult{
		Columns:   columns,
		Rows:      resultRows,
		RowCount:  count,
		Truncated: truncated,
	}, nil
}
