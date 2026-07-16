package taskstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "tasks.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestTaskCRUDAndClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	_, err := store.Create(ctx, "t1", "First", "do the first thing")
	require.NoError(t, err)
	_, err = store.Create(ctx, "t2", "Second", "do the second thing")
	require.NoError(t, err)

	// Claim is oldest-first and marks running atomically.
	claimed, err := store.ClaimNextQueued(ctx)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, "t1", claimed.ID)
	require.Equal(t, StatusRunning, claimed.Status)

	claimed2, err := store.ClaimNextQueued(ctx)
	require.NoError(t, err)
	require.Equal(t, "t2", claimed2.ID)

	empty, err := store.ClaimNextQueued(ctx)
	require.NoError(t, err)
	require.Nil(t, empty)

	// Status filter and ordering.
	running, err := store.List(ctx, StatusRunning)
	require.NoError(t, err)
	require.Len(t, running, 2)
	all, err := store.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 2)

	missing, err := store.Get(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestTaskProgressAndTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)
	_, err := store.Create(ctx, "t1", "T", "p")
	require.NoError(t, err)
	claimed, err := store.ClaimNextQueued(ctx)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	steps := []chatstore.Step{{Index: 0, Kind: "query", Summary: "s", SQL: "SELECT 1"}}
	transcript := []assistant.AgentExchange{{
		Call:   assistant.AgentCall{Tool: assistant.ToolRunQuery, SQL: "SELECT 1", Summary: "s"},
		Result: `{"rowCount":1}`,
	}}
	require.NoError(t, store.SaveProgress(ctx, "t1", steps, transcript))

	got, err := store.Get(ctx, "t1")
	require.NoError(t, err)
	require.Len(t, got.Steps, 1)
	require.Len(t, got.Transcript, 1)
	require.Equal(t, `{"rowCount":1}`, got.Transcript[0].Result)

	// CAS transition succeeds from running, then fails from done.
	ok, err := store.Transition(ctx, "t1", StatusRunning, Task{
		Status: StatusDone, Steps: steps, Transcript: transcript, Answer: "done!",
	})
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.Transition(ctx, "t1", StatusRunning, Task{Status: StatusFailed})
	require.NoError(t, err)
	require.False(t, ok)

	got, err = store.Get(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, StatusDone, got.Status)
	require.Equal(t, "done!", got.Answer)
}

func TestTaskSetStatusAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)
	_, err := store.Create(ctx, "t1", "T", "p")
	require.NoError(t, err)

	// Delete refuses live tasks, allows finished ones, and is idempotent
	// for unknown ids.
	deleted, err := store.Delete(ctx, "t1")
	require.NoError(t, err)
	require.False(t, deleted)

	ok, err := store.SetStatus(ctx, "t1", StatusCanceled, StatusQueued, StatusRunning)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.SetStatus(ctx, "t1", StatusCanceled, StatusQueued, StatusRunning)
	require.NoError(t, err)
	require.False(t, ok)

	deleted, err = store.Delete(ctx, "t1")
	require.NoError(t, err)
	require.True(t, deleted)
	deleted, err = store.Delete(ctx, "unknown")
	require.NoError(t, err)
	require.True(t, deleted)
}

func TestRequeueRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)
	_, err := store.Create(ctx, "t1", "T", "p")
	require.NoError(t, err)
	_, err = store.ClaimNextQueued(ctx)
	require.NoError(t, err)

	require.NoError(t, store.RequeueRunning(ctx))
	got, err := store.Get(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, StatusQueued, got.Status)
}
