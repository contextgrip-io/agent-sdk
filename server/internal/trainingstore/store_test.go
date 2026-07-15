package trainingstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "training.sqlite"), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCaptureToggleDefaultsEnabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	enabled, err := store.CaptureEnabled(ctx)
	require.NoError(t, err)
	require.True(t, enabled, "capture must default to ENABLED")

	require.NoError(t, store.SetCaptureEnabled(ctx, false))
	enabled, err = store.CaptureEnabled(ctx)
	require.NoError(t, err)
	require.False(t, enabled)

	require.NoError(t, store.SetCaptureEnabled(ctx, true))
	enabled, err = store.CaptureEnabled(ctx)
	require.NoError(t, err)
	require.True(t, enabled)
}

func TestUpsertBySourceMessageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	// Auto-capture writes the exchange without a verdict.
	require.NoError(t, store.Upsert(ctx, Record{
		ID: "r1", Session: "ask", SourceMessageID: "msg-1",
		SQL: "SELECT 1", Intent: "one?",
		Columns: []string{"c"}, RowSample: [][]any{{float64(1)}}, RowCount: 1, ExecutionTimeMs: 5,
	}))
	// An eval for the same message attaches the verdict without duplicating
	// or clobbering the captured exchange.
	require.NoError(t, store.Upsert(ctx, Record{
		ID: "r2", SourceMessageID: "msg-1", Verdict: "bad", SQL: "SELECT 1", Intent: "one?",
	}))

	records, err := store.ListAll(ctx, false)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "r1", records[0].ID) // original record kept
	require.Equal(t, "ask", records[0].Session)
	require.Equal(t, "bad", records[0].Verdict)
	require.Equal(t, []string{"c"}, records[0].Columns)

	// Re-rating updates the verdict in place.
	require.NoError(t, store.Upsert(ctx, Record{
		ID: "r3", SourceMessageID: "msg-1", Verdict: "good", SQL: "SELECT 1",
	}))
	rec, err := store.GetBySourceMessageID(ctx, "msg-1")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "good", rec.Verdict)

	// A verdict-less upsert (repeat auto-capture) never clears a verdict.
	require.NoError(t, store.Upsert(ctx, Record{ID: "r4", SourceMessageID: "msg-1", SQL: "SELECT 1"}))
	rec, err = store.GetBySourceMessageID(ctx, "msg-1")
	require.NoError(t, err)
	require.Equal(t, "good", rec.Verdict)

	// SourceMessageID is mandatory.
	require.Error(t, store.Upsert(ctx, Record{ID: "r5", SQL: "SELECT 1"}))
}

func TestListAllAndEvaluatedOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	require.NoError(t, store.Upsert(ctx, Record{ID: "a", SourceMessageID: "m-a", SQL: "SELECT 1"}))
	require.NoError(t, store.Upsert(ctx, Record{ID: "b", SourceMessageID: "m-b", SQL: "SELECT 2", Verdict: "good"}))

	all, err := store.ListAll(ctx, false)
	require.NoError(t, err)
	require.Len(t, all, 2)
	// Oldest first (chronological dump order).
	require.Equal(t, "a", all[0].ID)

	evaluated, err := store.ListAll(ctx, true)
	require.NoError(t, err)
	require.Len(t, evaluated, 1)
	require.Equal(t, "b", evaluated[0].ID)
}

func TestStatsAndDeleteAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	stats, err := store.Stats(ctx)
	require.NoError(t, err)
	require.Zero(t, stats.Records)
	require.Zero(t, stats.Evaluated)
	require.Nil(t, stats.FirstAt)
	require.Nil(t, stats.LastAt)

	require.NoError(t, store.Upsert(ctx, Record{ID: "a", SourceMessageID: "m-a", SQL: "SELECT 1"}))
	require.NoError(t, store.Upsert(ctx, Record{ID: "b", SourceMessageID: "m-b", SQL: "SELECT 2", Verdict: "bad"}))

	stats, err = store.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Records)
	require.Equal(t, 1, stats.Evaluated)
	require.NotNil(t, stats.FirstAt)
	require.NotNil(t, stats.LastAt)
	require.False(t, stats.LastAt.Before(*stats.FirstAt))

	deleted, err := store.DeleteAll(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted)
	all, err := store.ListAll(ctx, false)
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestRecordCapPrunesOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t, WithMaxRecords(2))

	require.NoError(t, store.Upsert(ctx, Record{ID: "a", SourceMessageID: "m-a", SQL: "SELECT 1"}))
	require.NoError(t, store.Upsert(ctx, Record{ID: "b", SourceMessageID: "m-b", SQL: "SELECT 2"}))
	require.NoError(t, store.Upsert(ctx, Record{ID: "c", SourceMessageID: "m-c", SQL: "SELECT 3"}))

	all, err := store.ListAll(ctx, false)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "b", all[0].ID)
	require.Equal(t, "c", all[1].ID)
}

func TestSanitizeBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	rows := make([][]any, 25)
	for i := range rows {
		rows[i] = []any{strings.Repeat("x", 1000)}
	}
	require.NoError(t, store.Upsert(ctx, Record{
		ID: "a", SourceMessageID: "m-a",
		SQL:          strings.Repeat("s", 200_000),
		Intent:       strings.Repeat("i", 200_000),
		ErrorMessage: strings.Repeat("e", 10_000),
		RowSample:    rows,
	}))
	rec, err := store.GetBySourceMessageID(ctx, "m-a")
	require.NoError(t, err)
	require.LessOrEqual(t, len(rec.SQL), 100_000)
	require.LessOrEqual(t, len(rec.Intent), 100_000)
	require.LessOrEqual(t, len(rec.ErrorMessage), 4_000)
	require.Len(t, rec.RowSample, 20)
	for _, row := range rec.RowSample {
		require.LessOrEqual(t, len(row[0].(string)), 256)
	}
}
