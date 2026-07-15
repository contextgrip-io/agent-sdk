package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
)

// proposeWrite drives one agent chat turn that ends in a pending approval
// and returns (approvalID, assistantMessageID, conversationID).
func proposeWrite(t *testing.T, env *testEnv, sql string) (string, string, string) {
	t.Helper()
	env.model.agentTurn = agentScript(writeCall(sql, "test rationale"))
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "please fix the data", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[askResponse](t, rec)
	require.NotEmpty(t, resp.PendingApprovalID)
	return resp.PendingApprovalID, resp.AssistantMessageID, resp.ConversationID
}

// TestApprovalApprove: approving executes the EXACT stored SQL on the write
// executor, returns the outcome, appends an outcome message, and clears the
// pending marker.
func TestApprovalApprove(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	sql := "UPDATE orders SET status = 'ok' WHERE status = 'broken'"
	approvalID, msgID, convID := proposeWrite(t, env, sql)

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[struct {
		Approval approvalView             `json:"approval"`
		Result   *chatstore.ResultSummary `json:"result"`
		Error    string                   `json:"error"`
	}](t, rec)
	require.Equal(t, "approved", resp.Approval.Status)
	require.NotEmpty(t, resp.Approval.DecidedAt)
	require.NotNil(t, resp.Result)
	require.Equal(t, 3, resp.Result.RowCount) // fake executor reports 3 rows
	require.Empty(t, resp.Error)

	// The exact statement ran on the write pool.
	require.Equal(t, []string{sql}, env.writeDB.executed)

	// Outcome message appended to the source conversation; pending marker
	// cleared on the proposing message.
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	last := msgs[len(msgs)-1]
	require.Equal(t, "assistant", last.Role)
	require.Contains(t, last.Text, "executed (3 rows affected)")
	require.Equal(t, sql, last.SQL)
	src, err := env.chat.GetMessage(context.Background(), msgID)
	require.NoError(t, err)
	require.Empty(t, src.PendingApprovalID)

	// The pending list is now empty; re-deciding conflicts.
	require.Empty(t, decodeBody[[]approvalView](t, env.do(t, http.MethodGet, "/api/v1/approvals", testPrimaryToken, nil)))
	rec = env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "reject"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "ALREADY_DECIDED", decodeBody[errorBody](t, rec).Code)
}

// TestApprovalReject: rejecting records the decision, appends a note, and
// never touches the write executor.
func TestApprovalReject(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	approvalID, msgID, convID := proposeWrite(t, env, "DELETE FROM orders")

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "reject"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[map[string]any](t, rec)
	require.Equal(t, "rejected", resp["approval"].(map[string]any)["status"])
	require.NotContains(t, resp, "result")

	require.Empty(t, env.writeDB.executed)

	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	last := msgs[len(msgs)-1]
	require.Contains(t, last.Text, "rejected")
	src, err := env.chat.GetMessage(context.Background(), msgID)
	require.NoError(t, err)
	require.Empty(t, src.PendingApprovalID)
}

// TestApprovalWritesDisabled: with no write pool, approve is a 409
// WRITES_DISABLED (and stays pending), while reject still works.
func TestApprovalWritesDisabled(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) { cfg.WriteDB = nil })
	approvalID, _, _ := proposeWrite(t, env, "UPDATE t SET x = 1")

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "WRITES_DISABLED", decodeBody[errorBody](t, rec).Code)

	// Still pending: the failed approve did not consume the decision.
	pending := decodeBody[[]approvalView](t, env.do(t, http.MethodGet, "/api/v1/approvals", testPrimaryToken, nil))
	require.Len(t, pending, 1)

	rec = env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "reject"})
	require.Equal(t, http.StatusOK, rec.Code)

	// Status reports writesEnabled=false.
	status := decodeBody[statusResponse](t, env.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil))
	require.False(t, status.WritesEnabled)
}

// TestApprovalExecutionFailure: the decision sticks even when the write
// fails; the error is reported and appended to the conversation.
func TestApprovalExecutionFailure(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.writeDB.err = errors.New("permission denied for table orders")
	approvalID, _, convID := proposeWrite(t, env, "UPDATE orders SET x = 1")

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[map[string]any](t, rec)
	require.Equal(t, "approved", resp["approval"].(map[string]any)["status"])
	require.Contains(t, resp["error"], "permission denied")
	require.NotContains(t, resp, "result")

	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Contains(t, msgs[len(msgs)-1].Text, "failed")
}

// TestApprovalValidation: unknown ids 404; bad decisions 400.
func TestApprovalValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/nope", testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeBody[errorBody](t, rec).Code)

	approvalID, _, _ := proposeWrite(t, env, "UPDATE t SET x = 1")
	rec = env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "maybe"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)
}

// TestStatusFeatures: the status document reports features + writesEnabled.
func TestStatusFeatures(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	status := decodeBody[statusResponse](t, env.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil))
	require.Equal(t, []string{"chat", "agent", "board"}, status.Features)
	require.True(t, status.WritesEnabled)

	env2 := newTestEnv(t, func(cfg *Config) { cfg.Features = ParseFeatures("chat") })
	status = decodeBody[statusResponse](t, env2.do(t, http.MethodGet, "/api/v1/status", testPrimaryToken, nil))
	require.Equal(t, []string{"chat"}, status.Features)
}
