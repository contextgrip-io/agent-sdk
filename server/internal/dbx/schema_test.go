package dbx

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func sampleTree() []SchemaNode {
	return []SchemaNode{
		{
			Name: "public",
			Tables: []TableNode{
				{
					Name: "orders",
					Type: "table",
					Columns: []ColumnNode{
						{Name: "id", DataType: "bigint"},
						{Name: "total_cents", DataType: "integer"},
					},
				},
				{
					Name:    "daily_revenue",
					Type:    "view",
					Columns: []ColumnNode{{Name: "day", DataType: "date"}},
				},
			},
		},
		{
			Name:   "audit",
			Tables: []TableNode{{Name: "events", Type: "table", Columns: []ColumnNode{{Name: "id", DataType: "uuid"}}}},
		},
	}
}

func TestBuildSchemaContext(t *testing.T) {
	t.Parallel()
	out := BuildSchemaContext(sampleTree(), 10_000)
	assert.Contains(t, out, "schema public:\n")
	assert.Contains(t, out, "  table orders (id bigint, total_cents integer)\n")
	assert.Contains(t, out, "  view daily_revenue (day date)\n")
	assert.Contains(t, out, "schema audit:\n")
	assert.NotContains(t, out, "truncated")
}

func TestBuildSchemaContextBounded(t *testing.T) {
	t.Parallel()
	out := BuildSchemaContext(sampleTree(), 40)
	assert.LessOrEqual(t, len(out), 40+len("-- schema truncated; more tables exist\n"))
	assert.True(t, strings.HasSuffix(out, "-- schema truncated; more tables exist\n"), out)
}

func TestBuildSchemaContextEmpty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "(no tables found)", BuildSchemaContext(nil, 10_000))
}
