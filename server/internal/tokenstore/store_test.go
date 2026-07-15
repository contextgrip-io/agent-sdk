package tokenstore

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "tokens.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestTokenLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestStore(t)

	token, raw, err := store.Create(ctx, "reporting-cron")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), raw)
	require.Equal(t, "reporting-cron", token.Label)
	require.Equal(t, HashToken(raw)[:8], token.Fingerprint)
	require.Nil(t, token.LastUsedAt)

	// Lookup by hash finds it; unknown hash is (nil, nil).
	found, err := store.FindByHash(ctx, HashToken(raw))
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, token.ID, found.ID)
	missing, err := store.FindByHash(ctx, HashToken("something-else"))
	require.NoError(t, err)
	require.Nil(t, missing)

	// Touch stamps lastUsedAt.
	require.NoError(t, store.TouchLastUsed(ctx, token.ID))
	found, err = store.FindByHash(ctx, HashToken(raw))
	require.NoError(t, err)
	require.NotNil(t, found.LastUsedAt)

	list, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Revoke: token disappears from lookup and listing.
	require.NoError(t, store.Delete(ctx, token.ID))
	found, err = store.FindByHash(ctx, HashToken(raw))
	require.NoError(t, err)
	require.Nil(t, found)
	list, err = store.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	// Deleting an unknown id is not an error.
	require.NoError(t, store.Delete(ctx, "nope"))
}

func TestSharedDBFileWithChatstore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// One file, two table sets: both stores open the same path (this is the
	// AI_CHAT_DB_PATH production layout).
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	tokens, err := New(path)
	require.NoError(t, err)
	defer tokens.Close()
	chat, err := chatstore.New(path)
	require.NoError(t, err)
	defer chat.Close()

	_, raw, err := tokens.Create(ctx, "shared")
	require.NoError(t, err)
	_, err = chat.CreateConversation(ctx, "c1", "shared file")
	require.NoError(t, err)

	found, err := tokens.FindByHash(ctx, HashToken(raw))
	require.NoError(t, err)
	require.NotNil(t, found)
	conv, err := chat.GetConversation(ctx, "c1")
	require.NoError(t, err)
	require.NotNil(t, conv)
}
