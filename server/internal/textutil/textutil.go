// Package textutil holds small text-shaping helpers shared by the stores and
// the API layer.
package textutil

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// TruncateUTF8 shortens s to at most max bytes without splitting a multi-byte
// rune, so truncated values stay valid UTF-8.
func TruncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// SortableTimeFormat is a fixed-width UTC RFC3339 layout with nanosecond
// precision. Unlike time.RFC3339Nano it never trims trailing zeros, so the
// lexicographic order of stored strings matches chronological order (SQLite
// TEXT comparisons rely on this). Values remain parseable with
// time.RFC3339Nano.
const SortableTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatSortable renders t in SortableTimeFormat (UTC).
func FormatSortable(t time.Time) string {
	return t.UTC().Format(SortableTimeFormat)
}

// BoundCellValue enforces a size cap on one result cell of any JSON-decoded
// type. Scalars pass through (strings truncated); arrays, objects, and other
// composite values are re-encoded as JSON and truncated, so a nested value
// can never smuggle unbounded data past a per-cell cap.
func BoundCellValue(cell any, maxChars int) any {
	switch v := cell.(type) {
	case nil, bool, float64, float32, int, int32, int64, uint, uint32, uint64, json.Number:
		return v
	case string:
		return TruncateUTF8(v, maxChars)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return TruncateUTF8(fmt.Sprintf("%v", v), maxChars)
		}
		if len(encoded) <= maxChars {
			return v
		}
		return TruncateUTF8(string(encoded), maxChars)
	}
}

// BoundCells applies BoundCellValue across a row sample in place.
func BoundCells(rows [][]any, maxChars int) {
	for i, row := range rows {
		for j, cell := range row {
			rows[i][j] = BoundCellValue(cell, maxChars)
		}
	}
}
