package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/dbx"
)

// TestSSEEventSequence is the full happy-path stream:
// meta -> sql -> result -> delta* -> done, with body assertions on every
// event, and the exchange persisted.
func TestSSEEventSequence(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "How many orders?"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))

	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "sql", "result", "delta", "delta", "done"}, eventNames(events))

	meta := events[0].data
	convID, _ := meta["conversationId"].(string)
	userMsgID, _ := meta["userMessageId"].(string)
	require.NotEmpty(t, convID)
	require.NotEmpty(t, userMsgID)

	require.Equal(t, "SELECT count(*) FROM orders", events[1].data["sql"])

	result := events[2].data
	require.Equal(t, []any{"count"}, result["columns"])
	require.Equal(t, float64(1), result["rowCount"])
	require.Equal(t, false, result["truncated"])
	require.Contains(t, result, "executionTimeMs")
	require.Equal(t, []any{[]any{float64(42)}}, result["rowSample"])

	require.Equal(t, "The answer ", events[3].data["text"])
	require.Equal(t, "is 42.", events[4].data["text"])

	done := events[5].data
	require.Equal(t, convID, done["conversationId"])
	require.NotEmpty(t, done["assistantMessageId"])

	// The verified SQL was executed with the documented bounds.
	require.Equal(t, "SELECT count(*) FROM orders", env.db.lastSQL)
	require.Equal(t, queryRowLimit, env.db.lastLimit)
	require.Equal(t, queryTimeout, env.db.lastTimeout)

	// Both turns persisted.
	detail := decodeBody[struct {
		Conversation conversationView `json:"conversation"`
		Messages     []messageView    `json:"messages"`
	}](t, env.do(t, http.MethodGet, "/api/v1/conversations/"+convID, testPrimaryToken, nil))
	require.Equal(t, "How many orders?", detail.Conversation.Title)
	require.Len(t, detail.Messages, 2)
	require.Equal(t, "user", detail.Messages[0].Role)
	require.Equal(t, "How many orders?", detail.Messages[0].Text)
	require.Equal(t, "assistant", detail.Messages[1].Role)
	require.Equal(t, "The answer is 42.", detail.Messages[1].Text)
	require.Equal(t, "SELECT count(*) FROM orders", detail.Messages[1].SQL)
	require.NotNil(t, detail.Messages[1].Result)
	require.Equal(t, 1, detail.Messages[1].Result.RowCount)
}

// TestSSEExecutionError: a failed query is NOT terminal — the result event
// carries the error, the model explains it, and the exchange persists with
// the error attached.
func TestSSEExecutionError(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.db.execErr = errors.New(`relation "nope" does not exist`)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "How many nopes?"})
	require.Equal(t, http.StatusOK, rec.Code)

	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "sql", "result", "delta", "delta", "done"}, eventNames(events))
	result := events[2].data
	require.Contains(t, result["error"], "does not exist")
	require.Contains(t, result, "executionTimeMs")
	require.NotContains(t, result, "rowCount")

	// The model saw the execution error.
	require.NotNil(t, env.model.lastAnswerInput)
	require.Contains(t, env.model.lastAnswerInput.Result.Error, "does not exist")

	convID := events[0].data["conversationId"].(string)
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Contains(t, msgs[1].Error, "does not exist")
	require.Nil(t, msgs[1].Result)
}

// TestSSEVerifierRejects: non-read-only model output is discarded before it
// reaches the stream, and an error record is persisted.
func TestSSEVerifierRejects(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.generate = func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
		return "DELETE FROM orders", nil
	}

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "Delete everything"})
	require.Equal(t, http.StatusOK, rec.Code)

	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "error"}, eventNames(events))
	require.Contains(t, events[1].data["message"], "not read-only")

	convID := events[0].data["conversationId"].(string)
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "assistant", msgs[1].Role)
	require.Contains(t, msgs[1].Error, "not read-only")
}

// TestSSEGenerationFailure: a model error before any SQL surfaces as an SSE
// error event and bumps the model-error metric.
func TestSSEGenerationFailure(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.generate = func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
		return "", errors.New("upstream 529")
	}

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "hi"})
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "error"}, eventNames(events))
	require.Contains(t, events[1].data["message"], "could not generate")

	metrics := env.do(t, http.MethodGet, "/metrics", "", nil).Body.String()
	require.Contains(t, metrics, "ai_chat_model_errors_total 1")
}

// TestSSESampleBounds: rows and cells are bounded before they reach the
// model, the stream, and the store.
func TestSSESampleBounds(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	rows := make([][]any, 25)
	for i := range rows {
		rows[i] = []any{strings.Repeat("x", 1000)}
	}
	env.db.result = &dbx.QueryResult{Columns: []string{"blob"}, Rows: rows, RowCount: 25, Truncated: false}

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "big cells"})
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, "result", events[2].name)
	sample := events[2].data["rowSample"].([]any)
	require.Len(t, sample, displaySampleRows)
	for _, row := range sample {
		cell := row.([]any)[0].(string)
		require.LessOrEqual(t, len(cell), sampleCellChars)
	}
	// rowCount reports rows returned (not the sample size).
	require.Equal(t, float64(25), events[2].data["rowCount"])

	// The model input was bounded too.
	require.Len(t, env.model.lastAnswerInput.Result.RowSample, displaySampleRows)
	for _, row := range env.model.lastAnswerInput.Result.RowSample {
		require.LessOrEqual(t, len(row[0].(string)), sampleCellChars)
	}

	// And the stored record.
	convID := events[0].data["conversationId"].(string)
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, msgs[1].Result.RowSample, displaySampleRows)
}

// TestAskOneShot: the JSON endpoint runs the same loop and collects deltas
// into a single answer.
func TestAskOneShot(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "How many orders?"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := decodeBody[askResponse](t, rec)
	assert.NotEmpty(t, resp.ConversationID)
	assert.NotEmpty(t, resp.UserMessageID)
	assert.NotEmpty(t, resp.AssistantMessageID)
	assert.Equal(t, "SELECT count(*) FROM orders", resp.SQL)
	assert.Equal(t, "The answer is 42.", resp.Answer)
	require.NotNil(t, resp.Result)
	assert.Equal(t, 1, resp.Result.RowCount)
	assert.Empty(t, resp.ResultError)

	// A follow-up in the same conversation replays history to the model.
	rec = env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "And last month?", "conversationId": resp.ConversationID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	followUp := decodeBody[askResponse](t, rec)
	assert.Equal(t, resp.ConversationID, followUp.ConversationID)
	require.NotNil(t, env.model.lastGenerateInput)
	require.Len(t, env.model.lastGenerateInput.History, 1)
	assert.Equal(t, "How many orders?", env.model.lastGenerateInput.History[0].Question)
	assert.Equal(t, "SELECT count(*) FROM orders", env.model.lastGenerateInput.History[0].SQL)
}

// TestAskExecutionFailure: failed execution is NOT an HTTP error — the
// response carries resultError and an answer explaining the failure.
func TestAskExecutionFailure(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.db.execErr = errors.New("syntax error at or near FROM")
	env.model.stream = func(_ context.Context, in assistant.AnswerInput, emit func(string) error) (string, error) {
		answer := "The query failed: " + in.Result.Error
		_ = emit(answer)
		return answer, nil
	}

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "broken"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[askResponse](t, rec)
	assert.Nil(t, resp.Result)
	assert.Contains(t, resp.ResultError, "syntax error")
	assert.Contains(t, resp.Answer, "syntax error")
}

// TestAskModelError: generation failure IS an HTTP error on the one-shot
// endpoint.
func TestAskModelError(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.generate = func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
		return "", errors.New("model overloaded")
	}
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "hi"})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	body := decodeBody[errorBody](t, rec)
	assert.Equal(t, "MODEL_ERROR", body.Code)
}

// TestChatValidation covers question bounds and unknown conversations.
func TestChatValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	for _, path := range []string{"/api/v1/ask", "/api/v1/messages"} {
		rec := env.do(t, http.MethodPost, path, testPrimaryToken, map[string]string{"question": "   "})
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
		require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code, path)

		rec = env.do(t, http.MethodPost, path, testPrimaryToken,
			map[string]string{"question": strings.Repeat("q", maxQuestionChars+1)})
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
		require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code, path)

		rec = env.do(t, http.MethodPost, path, testPrimaryToken,
			map[string]string{"question": "hi", "conversationId": "does-not-exist"})
		require.Equal(t, http.StatusNotFound, rec.Code, path)
		require.Equal(t, "NOT_FOUND", decodeBody[errorBody](t, rec).Code, path)
	}
}

// TestConversationFullPreStream: the cap failure happens before SSE headers,
// as a plain JSON 400.
func TestConversationFullPreStream(t *testing.T) {
	t.Parallel()
	var chat *chatstore.Store
	env := newTestEnv(t, func(cfg *Config) {
		var err error
		chat, err = chatstore.New(t.TempDir()+"/small.sqlite", chatstore.WithLimits(500, 2))
		require.NoError(t, err)
		cfg.Chat = chat
	})
	t.Cleanup(func() { _ = chat.Close() })
	env.chat = chat

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "first"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	convID := decodeBody[askResponse](t, rec).ConversationID

	for _, path := range []string{"/api/v1/ask", "/api/v1/messages"} {
		rec = env.do(t, http.MethodPost, path, testPrimaryToken,
			map[string]string{"question": "second", "conversationId": convID})
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
		body := decodeBody[errorBody](t, rec)
		require.Equal(t, "CONVERSATION_FULL", body.Code, path)
	}
}

// TestConversationCapPrunesOldest: the instance-global conversation cap
// evicts the least recently updated thread.
func TestConversationCapPrunesOldest(t *testing.T) {
	t.Parallel()
	var chat *chatstore.Store
	env := newTestEnv(t, func(cfg *Config) {
		var err error
		chat, err = chatstore.New(t.TempDir()+"/cap.sqlite", chatstore.WithLimits(2, 200))
		require.NoError(t, err)
		cfg.Chat = chat
	})
	t.Cleanup(func() { _ = chat.Close() })
	env.chat = chat

	var convIDs []string
	for i := 0; i < 3; i++ {
		rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
			map[string]string{"question": fmt.Sprintf("question %d", i)})
		require.Equal(t, http.StatusOK, rec.Code)
		convIDs = append(convIDs, decodeBody[askResponse](t, rec).ConversationID)
	}

	list := decodeBody[[]conversationView](t, env.do(t, http.MethodGet, "/api/v1/conversations", testPrimaryToken, nil))
	require.Len(t, list, 2)
	require.Equal(t, convIDs[2], list[0].ID)
	require.Equal(t, convIDs[1], list[1].ID)

	rec := env.do(t, http.MethodGet, "/api/v1/conversations/"+convIDs[0], testPrimaryToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestNotConfigured: without a model key or database URL the chat endpoints
// return 503 NOT_CONFIGURED while the rest of the service still works.
func TestNotConfigured(t *testing.T) {
	t.Parallel()
	cases := map[string]func(cfg *Config){
		"no model": func(cfg *Config) { cfg.Model = nil },
		"no db":    func(cfg *Config) { cfg.DB = nil },
		"neither":  func(cfg *Config) { cfg.Model = nil; cfg.DB = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, mutate)
			for _, path := range []string{"/api/v1/ask", "/api/v1/messages"} {
				rec := env.do(t, http.MethodPost, path, testPrimaryToken, map[string]string{"question": "hi"})
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
				require.Equal(t, "NOT_CONFIGURED", decodeBody[errorBody](t, rec).Code, path)
			}
			// Conversations and status still function.
			require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/conversations", testPrimaryToken, nil).Code)
			require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil).Code)
		})
	}
}

// TestSSEStreamFailurePersistsPartialAnswer: when the answer stream dies
// midway (upstream failure, not a disconnect), the deltas already sent are
// persisted with the error attached and the stream ends with error, not done.
func TestSSEStreamFailurePersistsPartialAnswer(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.stream = func(_ context.Context, _ assistant.AnswerInput, emit func(string) error) (string, error) {
		_ = emit("The answer starts ")
		return "The answer starts ", errors.New("upstream connection reset")
	}

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "flaky"})
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "sql", "result", "delta", "error"}, eventNames(events))
	require.Contains(t, events[4].data["message"], "interrupted")

	convID := events[0].data["conversationId"].(string)
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "The answer starts ", msgs[1].Text)
	require.Equal(t, "SELECT count(*) FROM orders", msgs[1].SQL)
	require.Contains(t, msgs[1].Error, "interrupted")

	metrics := env.do(t, http.MethodGet, "/metrics", "", nil).Body.String()
	require.Contains(t, metrics, "ai_chat_model_errors_total 1")
}

// TestDisconnectPersistence: after SSE headers are sent, a client disconnect
// must not lose the assistant record — the partial answer is persisted via
// context.WithoutCancel.
func TestDisconnectPersistence(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	deltaSent := make(chan struct{})
	env.model.stream = func(ctx context.Context, _ assistant.AnswerInput, emit func(string) error) (string, error) {
		_ = emit("Hello ")
		close(deltaSent)
		<-ctx.Done() // block until the client disconnect cancels the request
		return "Hello ", ctx.Err()
	}

	srv := httptest.NewServer(env.server)
	defer srv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/api/v1/messages",
		strings.NewReader(`{"question": "will disconnect"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testPrimaryToken)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case <-deltaSent:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never reached the delta stage")
	}
	cancel() // simulate the client vanishing mid-stream

	// The partial answer with the interruption error must land in the store
	// despite the canceled request context.
	require.Eventually(t, func() bool {
		convs, err := env.chat.ListConversations(context.Background())
		if err != nil || len(convs) != 1 {
			return false
		}
		msgs, err := env.chat.ListMessages(context.Background(), convs[0].ID)
		if err != nil || len(msgs) != 2 {
			return false
		}
		return msgs[1].Role == "assistant" &&
			msgs[1].Text == "Hello " &&
			strings.Contains(msgs[1].Error, "interrupted")
	}, 5*time.Second, 20*time.Millisecond, "partial answer was not persisted after client disconnect")
}
