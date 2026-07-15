package approvalstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "approvals.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestApprovalLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	created, err := store.Create(ctx, Approval{
		ID: "a1", SQL: "UPDATE t SET x = 1", Rationale: "fix", MessageID: "m1",
	})
	require.NoError(t, err)
	require.Equal(t, StatusPending, created.Status)
	require.Nil(t, created.DecidedAt)

	got, err := store.Get(ctx, "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "UPDATE t SET x = 1", got.SQL)
	require.Equal(t, "m1", got.MessageID)
	require.Empty(t, got.TaskID)

	missing, err := store.Get(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	pending, err := store.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// First decision claims; the second loses the compare-and-set.
	claimed, err := store.Decide(ctx, "a1", StatusApproved)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = store.Decide(ctx, "a1", StatusRejected)
	require.NoError(t, err)
	require.False(t, claimed)

	got, err = store.Get(ctx, "a1")
	require.NoError(t, err)
	require.Equal(t, StatusApproved, got.Status)
	require.NotNil(t, got.DecidedAt)

	pending, err = store.ListPending(ctx)
	require.NoError(t, err)
	require.Empty(t, pending)

	// Invalid decision statuses are refused.
	_, err = store.Decide(ctx, "a1", "maybe")
	require.Error(t, err)
}
