package chatstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "chat.sqlite"), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestConversationCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	conv, err := store.CreateConversation(ctx, "c1", "How many orders?", "chat")
	require.NoError(t, err)
	require.Equal(t, "c1", conv.ID)
	require.Equal(t, "How many orders?", conv.Title)

	got, err := store.GetConversation(ctx, "c1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, conv.Title, got.Title)

	missing, err := store.GetConversation(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	_, err = store.CreateConversation(ctx, "c2", "Second", "chat")
	require.NoError(t, err)
	// c2 is newer, so it lists first.
	list, err := store.ListConversations(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "c2", list[0].ID)

	require.NoError(t, store.DeleteConversation(ctx, "c1"))
	list, err = store.ListConversations(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Deleting an unknown conversation is not an error.
	require.NoError(t, store.DeleteConversation(ctx, "nope"))
}

func TestAppendAndListMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)
	_, err := store.CreateConversation(ctx, "c1", "t", "chat")
	require.NoError(t, err)

	_, err = store.AppendMessage(ctx, Message{ID: "m1", ConversationID: "c1", Role: "user", Text: "How many orders?"})
	require.NoError(t, err)
	result := &ResultSummary{
		Columns:         []string{"count"},
		RowSample:       [][]any{{float64(42)}},
		RowCount:        1,
		ExecutionTimeMs: 12,
	}
	_, err = store.AppendMessage(ctx, Message{
		ID: "m2", ConversationID: "c1", Role: "assistant",
		Text: "There are 42 orders.", SQL: "SELECT count(*) FROM orders", Result: result,
	})
	require.NoError(t, err)

	msgs, err := store.ListMessages(ctx, "c1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, []int{1, 2}, []int{msgs[0].Seq, msgs[1].Seq})
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "assistant", msgs[1].Role)
	require.Equal(t, "SELECT count(*) FROM orders", msgs[1].SQL)
	require.NotNil(t, msgs[1].Result)
	require.Equal(t, 1, msgs[1].Result.RowCount)
	require.Equal(t, []string{"count"}, msgs[1].Result.Columns)

	// updated_at bumps on append: c1 must lead a fresh conversation created
	// before the append... (covered implicitly by prune test ordering)
}

func TestConversationFull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t, WithLimits(10, 2))
	_, err := store.CreateConversation(ctx, "c1", "t", "chat")
	require.NoError(t, err)

	_, err = store.AppendMessage(ctx, Message{ID: "m1", ConversationID: "c1", Role: "user", Text: "q"})
	require.NoError(t, err)
	_, err = store.AppendMessage(ctx, Message{ID: "m2", ConversationID: "c1", Role: "assistant", Text: "a"})
	require.NoError(t, err)
	_, err = store.AppendMessage(ctx, Message{ID: "m3", ConversationID: "c1", Role: "user", Text: "q2"})
	require.ErrorIs(t, err, ErrConversationFull)
}

func TestPruneOldestConversations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t, WithLimits(2, 200))

	_, err := store.CreateConversation(ctx, "c1", "first", "chat")
	require.NoError(t, err)
	_, err = store.AppendMessage(ctx, Message{ID: "m1", ConversationID: "c1", Role: "user", Text: "q"})
	require.NoError(t, err)
	_, err = store.CreateConversation(ctx, "c2", "second", "chat")
	require.NoError(t, err)
	// Cap is 2: creating c3 prunes the stalest conversation (c1).
	_, err = store.CreateConversation(ctx, "c3", "third", "chat")
	require.NoError(t, err)

	list, err := store.ListConversations(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []string{"c3", "c2"}, []string{list[0].ID, list[1].ID})

	// The pruned conversation and its messages are gone.
	gone, err := store.GetConversation(ctx, "c1")
	require.NoError(t, err)
	require.Nil(t, gone)
	msgs, err := store.ListMessages(ctx, "c1")
	require.NoError(t, err)
	require.Empty(t, msgs)

	// Appending bumps updated_at, so a touched conversation survives the
	// next prune while the untouched one is evicted.
	_, err = store.AppendMessage(ctx, Message{ID: "m2", ConversationID: "c2", Role: "user", Text: "q"})
	require.NoError(t, err)
	_, err = store.CreateConversation(ctx, "c4", "fourth", "chat")
	require.NoError(t, err)
	list, err = store.ListConversations(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []string{"c4", "c2"}, []string{list[0].ID, list[1].ID})
}

func TestSanitizeBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)
	_, err := store.CreateConversation(ctx, "c1", strings.Repeat("t", 500), "chat")
	require.NoError(t, err)

	conv, err := store.GetConversation(ctx, "c1")
	require.NoError(t, err)
	require.LessOrEqual(t, len(conv.Title), 120)

	// 25 rows with huge cells: stored sample is capped at 20 rows and 256
	// chars per cell even if the caller forgot to bound it.
	rows := make([][]any, 25)
	for i := range rows {
		rows[i] = []any{strings.Repeat("x", 1000)}
	}
	_, err = store.AppendMessage(ctx, Message{
		ID: "m1", ConversationID: "c1", Role: "assistant",
		Text:   "a",
		Result: &ResultSummary{RowSample: rows, RowCount: 100},
	})
	require.NoError(t, err)
	msgs, err := store.ListMessages(ctx, "c1")
	require.NoError(t, err)
	require.Len(t, msgs[0].Result.RowSample, 20)
	for _, row := range msgs[0].Result.RowSample {
		require.LessOrEqual(t, len(row[0].(string)), 256)
	}
}
