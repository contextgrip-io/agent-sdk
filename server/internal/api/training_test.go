package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
)

// askQuestion runs one /api/v1/ask exchange and returns the response.
func askQuestion(t *testing.T, env *testEnv, question string) askResponse {
	t.Helper()
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken, map[string]string{"question": question})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decodeBody[askResponse](t, rec)
}

// exportLines fetches /api/v1/training/export and decodes each NDJSON line
// into a generic map for exact-shape assertions.
func exportLines(t *testing.T, env *testEnv, query string) []map[string]any {
	t.Helper()
	rec := env.do(t, http.MethodGet, "/api/v1/training/export"+query, testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "application/x-ndjson", rec.Header().Get("Content-Type"))
	var lines []map[string]any
	scanner := bufio.NewScanner(rec.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), "line: %s", scanner.Text())
		lines = append(lines, line)
	}
	require.NoError(t, scanner.Err())
	return lines
}

// TestTrainingAutoCaptureAsk: with the toggle on (the default), a completed
// /ask exchange writes one record with the exact export-line shape.
func TestTrainingAutoCaptureAsk(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// Capture defaults to enabled.
	rec := env.do(t, http.MethodGet, "/api/v1/training/capture", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, decodeBody[map[string]bool](t, rec)["enabled"])

	resp := askQuestion(t, env, "How many orders?")

	lines := exportLines(t, env, "")
	require.Len(t, lines, 1)
	line := lines[0]

	// Exact top-level key set per the TrainingExportLine schema (no eval yet).
	for _, key := range []string{"id", "capturedAt", "connection", "context", "query", "response"} {
		require.Contains(t, line, key)
	}
	require.NotContains(t, line, "eval")

	connection := line["connection"].(map[string]any)
	require.Equal(t, testConnectionID, connection["id"])
	require.Equal(t, testConnectionName, connection["name"])
	require.Equal(t, "postgresql", connection["engine"])

	ctxObj := line["context"].(map[string]any)
	require.Equal(t, "ask", ctxObj["session"])
	require.Equal(t, resp.AssistantMessageID, ctxObj["sourceMessageId"])

	query := line["query"].(map[string]any)
	require.Equal(t, "SELECT count(*) FROM orders", query["sql"])
	require.Equal(t, "How many orders?", query["intent"])

	response := line["response"].(map[string]any)
	require.Equal(t, []any{"count"}, response["columns"])
	require.Equal(t, float64(1), response["rowCount"])
	require.Equal(t, false, response["truncated"])
	require.Contains(t, response, "executionTimeMs")
	require.Nil(t, response["error"]) // merge-compat: error key present, null
	require.Equal(t, []any{[]any{float64(42)}}, response["rowSample"])
}

// TestTrainingAutoCaptureSSE: the SSE path captures too, including
// execution-error exchanges (error recorded, no result fields beyond zeros).
func TestTrainingAutoCaptureSSE(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.db.execErr = errors.New(`relation "nope" does not exist`)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "How many nopes?"})
	require.Equal(t, http.StatusOK, rec.Code)
	events := parseSSE(t, rec.Body.String())
	done := events[len(events)-1]
	require.Equal(t, "done", done.name)

	lines := exportLines(t, env, "")
	require.Len(t, lines, 1)
	line := lines[0]
	require.Equal(t, "chat", line["context"].(map[string]any)["session"])
	require.Equal(t, done.data["assistantMessageId"], line["context"].(map[string]any)["sourceMessageId"])
	require.Contains(t, line["response"].(map[string]any)["error"], "does not exist")
	require.Equal(t, "How many nopes?", line["query"].(map[string]any)["intent"])
}

// TestTrainingNoCaptureWithoutSQL: failures before SQL exists (generation
// error, verifier rejection) never produce a training record.
func TestTrainingNoCaptureWithoutSQL(t *testing.T) {
	t.Parallel()

	t.Run("generation failure", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, nil)
		env.model.generate = func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
			return "", errors.New("model down")
		}
		rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken, map[string]string{"question": "hi"})
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Empty(t, exportLines(t, env, ""))
	})

	t.Run("verifier rejection", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, nil)
		env.model.generate = func(_ context.Context, _ assistant.GenerateSQLInput) (string, error) {
			return "DELETE FROM orders", nil
		}
		rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken, map[string]string{"question": "hi"})
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Empty(t, exportLines(t, env, ""))
	})
}

// TestTrainingToggle: turning capture off stops auto-capture, but explicit
// evals still record; turning it back on resumes capture. PUT is admin-only.
func TestTrainingToggle(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// Named tokens may read the setting but not change it.
	mint := decodeBody[struct {
		Token string `json:"token"`
	}](t, env.do(t, http.MethodPost, "/api/v1/tokens", testPrimaryToken, map[string]string{"label": "reader"}))
	rec := env.do(t, http.MethodPut, "/api/v1/training/capture", mint.Token, map[string]bool{"enabled": false})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "ADMIN_REQUIRED", decodeBody[errorBody](t, rec).Code)
	require.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/api/v1/training/capture", mint.Token, nil).Code)

	// Disable with the primary token.
	rec = env.do(t, http.MethodPut, "/api/v1/training/capture", testPrimaryToken, map[string]bool{"enabled": false})
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, decodeBody[map[string]bool](t, rec)["enabled"])

	// No auto-capture while disabled.
	resp := askQuestion(t, env, "quiet question")
	require.Empty(t, exportLines(t, env, ""))

	// ...but an explicit eval bypasses the toggle.
	rec = env.do(t, http.MethodPost, "/api/v1/messages/"+resp.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, decodeBody[map[string]bool](t, rec)["recorded"])

	lines := exportLines(t, env, "")
	require.Len(t, lines, 1)
	require.Equal(t, "good", lines[0]["eval"].(map[string]any)["verdict"])
	require.Equal(t, "quiet question", lines[0]["query"].(map[string]any)["intent"])

	// Re-enable: capture resumes.
	rec = env.do(t, http.MethodPut, "/api/v1/training/capture", testPrimaryToken, map[string]bool{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code)
	askQuestion(t, env, "loud question")
	require.Len(t, exportLines(t, env, ""), 2)
}

// TestTrainingEvalUpsert: rating an auto-captured answer attaches the
// verdict to the existing record; re-rating updates it in place.
func TestTrainingEvalUpsert(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	resp := askQuestion(t, env, "How many orders?")
	require.Len(t, exportLines(t, env, ""), 1)

	rec := env.do(t, http.MethodPost, "/api/v1/messages/"+resp.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "bad"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	lines := exportLines(t, env, "")
	require.Len(t, lines, 1) // still one record: upserted, not duplicated
	require.Equal(t, "bad", lines[0]["eval"].(map[string]any)["verdict"])
	// The auto-captured session survives the eval upsert.
	require.Equal(t, "ask", lines[0]["context"].(map[string]any)["session"])

	// Re-rate: verdict updates, still one record.
	rec = env.do(t, http.MethodPost, "/api/v1/messages/"+resp.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusOK, rec.Code)
	lines = exportLines(t, env, "")
	require.Len(t, lines, 1)
	require.Equal(t, "good", lines[0]["eval"].(map[string]any)["verdict"])

	stats := decodeBody[trainingStatsResponse](t, env.do(t, http.MethodGet, "/api/v1/training/stats", testPrimaryToken, nil))
	require.Equal(t, 1, stats.Records)
	require.Equal(t, 1, stats.Evaluated)
}

// TestTrainingEvalValidation: 404 for unknown messages, 400 for non-ratable
// ones and bad verdicts.
func TestTrainingEvalValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	resp := askQuestion(t, env, "How many orders?")

	// Unknown message id -> 404.
	rec := env.do(t, http.MethodPost, "/api/v1/messages/nope/eval", testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeBody[errorBody](t, rec).Code)

	// A user message (no SQL) is not ratable -> 400 VALIDATION.
	rec = env.do(t, http.MethodPost, "/api/v1/messages/"+resp.UserMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)

	// Bad verdict -> 400 VALIDATION.
	rec = env.do(t, http.MethodPost, "/api/v1/messages/"+resp.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "meh"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)

	// An assistant message without SQL (an error turn) is not ratable.
	conv, err := env.chat.CreateConversation(context.Background(), "c-x", "t", "chat")
	require.NoError(t, err)
	msg, err := env.chat.AppendMessage(context.Background(), chatstore.Message{
		ID: "m-nosql", ConversationID: conv.ID, Role: "assistant",
		Error: "the assistant could not generate a query",
	})
	require.NoError(t, err)
	rec = env.do(t, http.MethodPost, "/api/v1/messages/"+msg.ID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)
}

// TestTrainingCaptureFailureDoesNotFailChat: a broken training store is
// logged and swallowed; the chat response still succeeds.
func TestTrainingCaptureFailureDoesNotFailChat(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	require.NoError(t, env.training.Close()) // capture writes will now fail

	resp := askQuestion(t, env, "still works")
	require.Equal(t, "The answer is 42.", resp.Answer)
}

// TestTrainingExportFilters: includeRows=false omits rowSample and
// evaluatedOnly restricts to rated records.
func TestTrainingExportFilters(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	first := askQuestion(t, env, "first")
	askQuestion(t, env, "second")

	// Rate only the first exchange.
	rec := env.do(t, http.MethodPost, "/api/v1/messages/"+first.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusOK, rec.Code)

	// Default: rows included on both lines.
	lines := exportLines(t, env, "")
	require.Len(t, lines, 2)
	for _, line := range lines {
		require.Contains(t, line["response"].(map[string]any), "rowSample")
	}

	// includeRows=false: rowSample omitted entirely.
	lines = exportLines(t, env, "?includeRows=false")
	require.Len(t, lines, 2)
	for _, line := range lines {
		require.NotContains(t, line["response"].(map[string]any), "rowSample")
	}

	// evaluatedOnly=true: only the rated record.
	lines = exportLines(t, env, "?evaluatedOnly=true")
	require.Len(t, lines, 1)
	require.Equal(t, "first", lines[0]["query"].(map[string]any)["intent"])
	require.Equal(t, "good", lines[0]["eval"].(map[string]any)["verdict"])

	// The export never contains credentials — only the derived identity.
	raw := env.do(t, http.MethodGet, "/api/v1/training/export", testPrimaryToken, nil).Body.String()
	require.NotContains(t, raw, "postgres://")
	require.NotContains(t, raw, "password")
}

// TestTrainingExportBudget: the stream stops at the byte budget.
// Not parallel: it lowers the package-level budget while sequential tests
// run (parallel tests resume only after it restores the value).
func TestTrainingExportBudget(t *testing.T) {
	saved := trainingExportMaxBytes
	defer func() { trainingExportMaxBytes = saved }()

	env := newTestEnv(t, nil)
	askQuestion(t, env, "first")
	askQuestion(t, env, "second")
	askQuestion(t, env, "third")
	require.Len(t, exportLines(t, env, ""), 3)

	full := env.do(t, http.MethodGet, "/api/v1/training/export", testPrimaryToken, nil).Body.String()
	lineLen := len(strings.SplitN(full, "\n", 2)[0]) + 1

	// Budget for exactly one line: the export truncates to one record while
	// stats still reports three (the documented truncation detector).
	trainingExportMaxBytes = lineLen + 10
	require.Len(t, exportLines(t, env, ""), 1)
	stats := decodeBody[trainingStatsResponse](t, env.do(t, http.MethodGet, "/api/v1/training/stats", testPrimaryToken, nil))
	require.Equal(t, 3, stats.Records)
}

// TestTrainingStatsAndDelete: stats counts records/evaluated with a capture
// range; DELETE /records is admin-only and reports the removed count.
func TestTrainingStatsAndDelete(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	// Empty store: zeros, no range timestamps.
	stats := decodeBody[trainingStatsResponse](t, env.do(t, http.MethodGet, "/api/v1/training/stats", testPrimaryToken, nil))
	require.Equal(t, 0, stats.Records)
	require.Equal(t, 0, stats.Evaluated)
	require.Empty(t, stats.FirstCapturedAt)
	require.Empty(t, stats.LastCapturedAt)

	first := askQuestion(t, env, "first")
	askQuestion(t, env, "second")
	rec := env.do(t, http.MethodPost, "/api/v1/messages/"+first.AssistantMessageID+"/eval",
		testPrimaryToken, map[string]string{"verdict": "good"})
	require.Equal(t, http.StatusOK, rec.Code)

	stats = decodeBody[trainingStatsResponse](t, env.do(t, http.MethodGet, "/api/v1/training/stats", testPrimaryToken, nil))
	require.Equal(t, 2, stats.Records)
	require.Equal(t, 1, stats.Evaluated)
	require.NotEmpty(t, stats.FirstCapturedAt)
	require.NotEmpty(t, stats.LastCapturedAt)

	// Named tokens cannot delete records.
	mint := decodeBody[struct {
		Token string `json:"token"`
	}](t, env.do(t, http.MethodPost, "/api/v1/tokens", testPrimaryToken, map[string]string{"label": "reader"}))
	rec = env.do(t, http.MethodDelete, "/api/v1/training/records", mint.Token, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "ADMIN_REQUIRED", decodeBody[errorBody](t, rec).Code)

	// Primary token deletes everything and reports the count.
	rec = env.do(t, http.MethodDelete, "/api/v1/training/records", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, float64(2), decodeBody[map[string]any](t, rec)["deleted"])
	require.Empty(t, exportLines(t, env, ""))
}
