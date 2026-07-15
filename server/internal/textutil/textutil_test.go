package textutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "abc", TruncateUTF8("abc", 10))
	assert.Equal(t, "ab", TruncateUTF8("abc", 2))
	assert.Equal(t, "", TruncateUTF8("abc", 0))
	// Never splits a multi-byte rune.
	assert.Equal(t, "é", TruncateUTF8("éé", 3)) // é is 2 bytes; 3 keeps only one rune
	assert.Equal(t, "", TruncateUTF8("é", 1))
}

func TestBoundCellValue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, nil, BoundCellValue(nil, 10))
	assert.Equal(t, true, BoundCellValue(true, 10))
	assert.Equal(t, float64(42), BoundCellValue(float64(42), 10))
	assert.Equal(t, "short", BoundCellValue("short", 10))
	assert.Equal(t, "aaaaaaaaaa", BoundCellValue(strings.Repeat("a", 100), 10))
	// Composite values are re-encoded and truncated so nested data cannot
	// smuggle unbounded payloads past the per-cell cap.
	huge := map[string]any{"k": strings.Repeat("x", 1000)}
	bounded := BoundCellValue(huge, 16)
	s, ok := bounded.(string)
	assert.True(t, ok)
	assert.LessOrEqual(t, len(s), 16)
	// Small composites pass through untouched.
	small := map[string]any{"k": "v"}
	assert.Equal(t, small, BoundCellValue(small, 100))
}

func TestBoundCells(t *testing.T) {
	t.Parallel()
	rows := [][]any{{strings.Repeat("a", 50), int64(7)}}
	BoundCells(rows, 10)
	assert.Equal(t, "aaaaaaaaaa", rows[0][0])
	assert.Equal(t, int64(7), rows[0][1])
}
