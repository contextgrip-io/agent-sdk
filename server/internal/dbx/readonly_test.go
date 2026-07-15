package dbx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVerifyReadOnlySQL(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"SELECT 1",
		"select id from customers;",
		"  WITH recent AS (SELECT * FROM orders) SELECT count(*) FROM recent",
		"-- top customers\nSELECT id FROM customers",
		"/* revenue */ SELECT sum(total_cents) FROM orders;",
		"SELECT 'no semicolon in this literal' FROM t",
		`SELECT "quoted column" FROM t`,
		"SELECT id FROM customers\n;",
	}
	for _, sql := range allowed {
		assert.NoError(t, VerifyReadOnlySQL(sql), sql)
	}

	rejected := []string{
		"",
		"   ",
		"-- only a comment",
		"/* only a block comment */",
		"DELETE FROM sessions",
		"UPDATE orders SET status = 'x'",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE customers",
		"TRUNCATE orders",
		"CREATE INDEX idx ON t (a)",
		"SELECT 1; DELETE FROM t",
		"SELECT 1;;",
		"WITH d AS (DELETE FROM t RETURNING id) SELECT * FROM d; SELECT 1",
		"EXPLAIN ANALYZE DELETE FROM t",

		// The raw-semicolon rule is quote-blind BY DESIGN. This is the
		// E''-escape desync attack: a scanner that blanks quoted regions but
		// treats backslash as inert sees the literal end at the wrong quote
		// and misses the smuggled DELETE. The raw rule rejects it outright.
		`SELECT E'\''; DELETE FROM t; --'`,
		// Dollar-quoted strings are over-rejected on purpose — the raw rule
		// cannot (and must not) understand quoting.
		"SELECT $$a;b$$",
		// Quoted semicolons in ordinary literals/identifiers are likewise
		// over-rejected (previously allowed by the quote-aware verifier).
		"SELECT 'text with; semicolon and -- dashes' FROM t",
		`SELECT "weird;col" FROM t`,
	}
	for _, sql := range rejected {
		assert.Error(t, VerifyReadOnlySQL(sql), sql)
	}
}

func TestVerifyReadOnlySQLTrailingSemicolonOnly(t *testing.T) {
	t.Parallel()
	// Exactly one trailing semicolon is tolerated; anything beyond that is
	// treated as an interior semicolon.
	assert.NoError(t, VerifyReadOnlySQL("SELECT 1;"))
	assert.Error(t, VerifyReadOnlySQL("SELECT 1; ;"))
}

func TestVerifySingleStatement(t *testing.T) {
	t.Parallel()
	// The approval-execution gate: exactly one statement (raw interior
	// semicolons rejected regardless of quoting) but no read-only
	// requirement — mutating statements pass.
	assert.NoError(t, VerifySingleStatement("UPDATE orders SET status = 'ok'"))
	assert.NoError(t, VerifySingleStatement("DELETE FROM orders WHERE id = 1;"))
	assert.NoError(t, VerifySingleStatement("SELECT 1"))
	assert.Error(t, VerifySingleStatement(""))
	assert.Error(t, VerifySingleStatement("   ;  "))
	assert.Error(t, VerifySingleStatement("UPDATE t SET x = 1; DELETE FROM t"))
	assert.Error(t, VerifySingleStatement(`UPDATE t SET x = 'a;b'`)) // documented over-rejection
}
