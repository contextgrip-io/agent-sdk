package dbx

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ColumnNode describes a single column in a table.
type ColumnNode struct {
	Name     string
	DataType string
}

// TableNode describes a table or view with its columns.
type TableNode struct {
	Name    string
	Type    string // "table", "view", "materialized view", "foreign table"
	Columns []ColumnNode
}

// SchemaNode describes a database schema with its tables.
type SchemaNode struct {
	Name   string
	Tables []TableNode
}

// QueryResult holds the result of a SQL query execution.
type QueryResult struct {
	Columns   []string
	Rows      [][]any
	RowCount  int
	Truncated bool
}

// FetchTree returns a schema -> tables -> columns(name, data_type) tree for
// the connected database, built from a single information_schema query.
func FetchTree(ctx context.Context, pool *pgxpool.Pool) ([]SchemaNode, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.table_schema, t.table_name, t.table_type, c.column_name, c.data_type
		FROM information_schema.tables t
		LEFT JOIN information_schema.columns c
		  ON c.table_schema = t.table_schema AND c.table_name = t.table_name
		WHERE t.table_schema NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		ORDER BY t.table_schema, t.table_name, c.ordinal_position
	`)
	if err != nil {
		return nil, fmt.Errorf("fetch schema tree: %w", err)
	}
	defer rows.Close()

	var schemas []SchemaNode
	schemaIndex := map[string]int{}
	tableIndex := map[string][2]int{}

	for rows.Next() {
		var schemaName, tableName, tableType string
		var columnName, dataType *string
		if err := rows.Scan(&schemaName, &tableName, &tableType, &columnName, &dataType); err != nil {
			return nil, fmt.Errorf("scan schema row: %w", err)
		}
		si, ok := schemaIndex[schemaName]
		if !ok {
			si = len(schemas)
			schemaIndex[schemaName] = si
			schemas = append(schemas, SchemaNode{Name: schemaName})
		}
		tableKey := schemaName + "." + tableName
		pos, ok := tableIndex[tableKey]
		if !ok {
			nodeType := "table"
			switch tableType {
			case "VIEW":
				nodeType = "view"
			case "FOREIGN TABLE":
				nodeType = "foreign table"
			}
			pos = [2]int{si, len(schemas[si].Tables)}
			tableIndex[tableKey] = pos
			schemas[si].Tables = append(schemas[si].Tables, TableNode{Name: tableName, Type: nodeType})
		}
		if columnName != nil && dataType != nil {
			table := &schemas[pos[0]].Tables[pos[1]]
			table.Columns = append(table.Columns, ColumnNode{Name: *columnName, DataType: *dataType})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema rows: %w", err)
	}
	return schemas, nil
}

// BuildSchemaContext renders a compact, model-friendly schema summary from a
// schema tree, bounded so huge schemas cannot blow up the prompt.
func BuildSchemaContext(tree []SchemaNode, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 10_000
	}
	var out strings.Builder
	truncated := false
outer:
	for _, schema := range tree {
		line := fmt.Sprintf("schema %s:\n", schema.Name)
		if out.Len()+len(line) > maxChars {
			truncated = true
			break
		}
		out.WriteString(line)
		for _, table := range schema.Tables {
			var cols []string
			for _, col := range table.Columns {
				cols = append(cols, fmt.Sprintf("%s %s", col.Name, col.DataType))
			}
			line := fmt.Sprintf("  %s %s (%s)\n", strings.ToLower(table.Type), table.Name, strings.Join(cols, ", "))
			if out.Len()+len(line) > maxChars {
				truncated = true
				break outer
			}
			out.WriteString(line)
		}
	}
	if truncated {
		out.WriteString("-- schema truncated; more tables exist\n")
	}
	if out.Len() == 0 {
		return "(no tables found)"
	}
	return out.String()
}
