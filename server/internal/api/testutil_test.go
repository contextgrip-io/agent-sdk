package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/tokenstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/trainingstore"
)

const testPrimaryToken = "primary-token"

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeModel struct {
	modelID  string
	generate func(ctx context.Context, in assistant.GenerateSQLInput) (string, error)
	stream   func(ctx context.Context, in assistant.AnswerInput, emit func(string) error) (string, error)

	lastGenerateInput *assistant.GenerateSQLInput
	lastAnswerInput   *assistant.AnswerInput
}

func (f *fakeModel) Model() string { return f.modelID }

func (f *fakeModel) GenerateSQL(ctx context.Context, in assistant.GenerateSQLInput) (string, error) {
	f.lastGenerateInput = &in
	return f.generate(ctx, in)
}

func (f *fakeModel) StreamAnswer(ctx context.Context, in assistant.AnswerInput, emit func(string) error) (string, error) {
	f.lastAnswerInput = &in
	return f.stream(ctx, in, emit)
}

func defaultFakeModel() *fakeModel {
	return &fakeModel{
		modelID: "fake-model",
		generate: func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
			return "SELECT count(*) FROM orders", nil
		},
		stream: func(_ context.Context, _ assistant.AnswerInput, emit func(string) error) (string, error) {
			for _, delta := range []string{"The answer ", "is 42."} {
				if err := emit(delta); err != nil {
					return "", err
				}
			}
			return "The answer is 42.", nil
		},
	}
}

type fakeDB struct {
	schema    string
	schemaErr error
	result    *dbx.QueryResult
	execErr   error
	pingErr   error

	lastSQL     string
	lastLimit   int
	lastTimeout time.Duration
}

func (f *fakeDB) SchemaContext(_ context.Context, _ int) (string, error) {
	return f.schema, f.schemaErr
}

func (f *fakeDB) RunReadOnly(_ context.Context, sql string, limit int, timeout time.Duration) (*dbx.QueryResult, error) {
	f.lastSQL, f.lastLimit, f.lastTimeout = sql, limit, timeout
	if f.execErr != nil {
		return nil, f.execErr
	}
	return f.result, nil
}

func (f *fakeDB) Ping(_ context.Context) error { return f.pingErr }

func defaultFakeDB() *fakeDB {
	return &fakeDB{
		schema: "schema public:\n  table orders (id bigint)\n",
		result: &dbx.QueryResult{
			Columns:   []string{"count"},
			Rows:      [][]any{{int64(42)}},
			RowCount:  1,
			Truncated: false,
		},
	}
}

// ── test environment ────────────────────────────────────────────────────────

type testEnv struct {
	server   *Server
	chat     *chatstore.Store
	tokens   *tokenstore.Store
	training *trainingstore.Store
	model    *fakeModel
	db       *fakeDB
}

const (
	testConnectionID   = "abc123def456"
	testConnectionName = "mydb"
)

func newTestEnv(t *testing.T, mutate func(cfg *Config)) *testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ai-chat.sqlite")
	chat, err := chatstore.New(path)
	require.NoError(t, err)
	tokens, err := tokenstore.New(path)
	require.NoError(t, err)
	training, err := trainingstore.New(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = chat.Close()
		_ = tokens.Close()
		_ = training.Close()
	})

	sum := sha256.Sum256([]byte(testPrimaryToken))
	env := &testEnv{chat: chat, tokens: tokens, training: training, model: defaultFakeModel(), db: defaultFakeDB()}
	cfg := Config{
		Model:              env.model,
		ModelID:            env.model.modelID,
		DB:                 env.db,
		Chat:               chat,
		Tokens:             tokens,
		Training:           training,
		ConnectionID:       testConnectionID,
		ConnectionName:     testConnectionName,
		PrimaryTokenSHA256: sum[:],
	}
	if mutate != nil {
		mutate(&cfg)
	}
	env.server = New(cfg)
	return env
}

// do issues a request against the in-process handler.
func (e *testEnv) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

// ── SSE parsing ─────────────────────────────────────────────────────────────

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		require.Len(t, lines, 2, "SSE block must be exactly `event:` + `data:` lines: %q", block)
		require.True(t, strings.HasPrefix(lines[0], "event: "), "bad event line: %q", lines[0])
		require.True(t, strings.HasPrefix(lines[1], "data: "), "bad data line: %q", lines[1])
		event := sseEvent{name: strings.TrimPrefix(lines[0], "event: ")}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event.data),
			"data must be single-line JSON: %q", lines[1])
		events = append(events, event)
	}
	return events
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.name
	}
	return names
}
