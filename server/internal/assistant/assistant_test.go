package assistant

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCleanGeneratedSQL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1", "SELECT 1"},
		{"  SELECT 1  \n", "SELECT 1"},
		{"```sql\nSELECT 1\n```", "SELECT 1"},
		{"```SQL\nSELECT 1\n```", "SELECT 1"},
		{"```\nSELECT 1\n```", "SELECT 1"},
		{"```sql\nSELECT id\nFROM t\n```", "SELECT id\nFROM t"},
		{"", ""},
		{"```sql\n```", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, CleanGeneratedSQL(tc.in), "input %q", tc.in)
	}
}

func TestDefaultModel(t *testing.T) {
	t.Parallel()
	c := NewAnthropicClient("test-key", "", "")
	assert.Equal(t, DefaultModel, c.Model())
	c = NewAnthropicClient("test-key", "custom-model", "http://127.0.0.1:1")
	assert.Equal(t, "custom-model", c.Model())
}
