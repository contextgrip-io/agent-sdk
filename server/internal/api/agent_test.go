package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
)

// TestAgentSSEMultiStep: a multi-step read-only agent run streams
// meta -> step -> step -> delta -> done, persists the steps on the message,
// and captures each successful query for training (session "agent").
func TestAgentSSEMultiStep(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		queryCall("SELECT count(*) FROM orders", "count all orders"),
		queryCall("SELECT max(total_cents) FROM orders", "find the biggest order"),
		finalAnswer("There are 42 orders; the biggest is $99."),
	)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "Orders overview?", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code)
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "step", "step", "delta", "done"}, eventNames(events))

	step0 := events[1].data
	require.Equal(t, float64(0), step0["index"])
	require.Equal(t, "query", step0["kind"])
	require.Equal(t, "count all orders", step0["summary"])
	require.Equal(t, "SELECT count(*) FROM orders", step0["sql"])
	require.Equal(t, float64(1), step0["result"].(map[string]any)["rowCount"])

	step1 := events[2].data
	require.Equal(t, float64(1), step1["index"])
	require.Equal(t, "SELECT max(total_cents) FROM orders", step1["sql"])

	require.Equal(t, "There are 42 orders; the biggest is $99.", events[3].data["text"])

	done := events[4].data
	require.NotEmpty(t, done["assistantMessageId"])
	require.NotContains(t, done, "pendingApprovalId")

	// The assistant message persists text + steps; no single sql field.
	convID := events[0].data["conversationId"].(string)
	msgs, err := env.chat.ListMessages(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "There are 42 orders; the biggest is $99.", msgs[1].Text)
	require.Empty(t, msgs[1].SQL)
	require.Len(t, msgs[1].Steps, 2)
	require.Equal(t, "SELECT count(*) FROM orders", msgs[1].Steps[0].SQL)

	// Both query steps were captured (session "agent", per-step source ids).
	records, err := env.training.ListAll(context.Background(), false)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "agent", records[0].Session)
	require.Equal(t, msgs[1].ID+":0", records[0].SourceMessageID)
	require.Equal(t, "Orders overview?", records[0].Intent)
	require.Equal(t, msgs[1].ID+":1", records[1].SourceMessageID)
}

// TestAgentSSEProposeWrite: a proposed write emits approval_required, ends
// the turn with done.pendingApprovalId, and persists the pending marker.
func TestAgentSSEProposeWrite(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		queryCall("SELECT id FROM orders WHERE status = 'dup'", "find duplicates"),
		writeCall("DELETE FROM orders WHERE status = 'dup'", "remove the duplicate rows"),
	)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "Clean up duplicate orders", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code)
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "step", "approval_required", "done"}, eventNames(events))

	approval := events[2].data
	approvalID := approval["id"].(string)
	require.Equal(t, "DELETE FROM orders WHERE status = 'dup'", approval["sql"])
	require.Equal(t, "remove the duplicate rows", approval["rationale"])
	require.Equal(t, "pending", approval["status"])

	done := events[3].data
	require.Equal(t, approvalID, done["pendingApprovalId"])
	assistantMsgID := done["assistantMessageId"].(string)
	require.Equal(t, assistantMsgID, approval["source"].(map[string]any)["messageId"])

	// Message persisted with steps + pending marker; the write SQL is not
	// executed and not on the message as runnable SQL.
	msg, err := env.chat.GetMessage(context.Background(), assistantMsgID)
	require.NoError(t, err)
	require.NotNil(t, msg)
	require.Equal(t, approvalID, msg.PendingApprovalID)
	require.Len(t, msg.Steps, 1)
	require.Empty(t, env.writeDB.executed)

	// The approval shows in the pending list.
	pending := decodeBody[[]approvalView](t, env.do(t, http.MethodGet, "/api/v1/approvals", testPrimaryToken, nil))
	require.Len(t, pending, 1)
	require.Equal(t, approvalID, pending[0].ID)
}

// TestAskAgentMode: the one-shot endpoint collects steps and may return a
// pendingApprovalId.
func TestAskAgentMode(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		queryCall("SELECT count(*) FROM orders", "count"),
		finalAnswer("42 orders."),
	)

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "How many orders?", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeBody[askResponse](t, rec)
	require.Equal(t, "42 orders.", resp.Answer)
	require.Empty(t, resp.SQL)
	require.Len(t, resp.Steps, 1)
	require.Equal(t, "SELECT count(*) FROM orders", resp.Steps[0].SQL)
	require.Empty(t, resp.PendingApprovalID)

	// Pending-approval variant.
	env2 := newTestEnv(t, nil)
	env2.model.agentTurn = agentScript(writeCall("UPDATE orders SET status = 'ok'", "fix statuses"))
	rec = env2.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "Fix the statuses", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = decodeBody[askResponse](t, rec)
	require.NotEmpty(t, resp.PendingApprovalID)
	require.Empty(t, resp.Answer)
}

// TestAgentModeStickiness: a conversation keeps the mode of its first
// message — follow-ups without mode run the agent loop, with prior turns as
// history.
func TestAgentModeStickiness(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(finalAnswer("First answer."))

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "first question", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	convID := decodeBody[askResponse](t, rec).ConversationID

	env.model.lastAgentTurnInput = nil
	env.model.agentTurn = agentScript(finalAnswer("Second answer."))
	rec = env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "follow-up", "conversationId": convID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "Second answer.", decodeBody[askResponse](t, rec).Answer)

	// The agent path ran (not the chat loop) and saw the first turn.
	require.NotNil(t, env.model.lastAgentTurnInput)
	require.Len(t, env.model.lastAgentTurnInput.History, 1)
	require.Equal(t, "first question", env.model.lastAgentTurnInput.History[0].Question)
	require.Equal(t, "First answer.", env.model.lastAgentTurnInput.History[0].Answer)
}

// TestAgentFeatureDisabled: mode=agent without the "agent" feature is a 403
// FEATURE_DISABLED on both chat endpoints.
func TestAgentFeatureDisabled(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) { cfg.Features = ParseFeatures("chat,board") })

	for _, path := range []string{"/api/v1/ask", "/api/v1/messages"} {
		rec := env.do(t, http.MethodPost, path, testPrimaryToken,
			map[string]string{"question": "hi", "mode": "agent"})
		require.Equal(t, http.StatusForbidden, rec.Code, path)
		require.Equal(t, "FEATURE_DISABLED", decodeBody[errorBody](t, rec).Code, path)
	}
	// Plain chat still works.
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken, map[string]string{"question": "hi"})
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAgentMaxSteps: exceeding AI_CHAT_AGENT_MAX_STEPS fails the turn like a
// model error, with an error record persisted.
func TestAgentMaxSteps(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) { cfg.AgentMaxSteps = 2 })
	env.model.agentTurn = agentScript(
		queryCall("SELECT 1", "one"),
		queryCall("SELECT 2", "two"),
		queryCall("SELECT 3", "three"), // over the limit
		finalAnswer("never reached"),
	)

	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "loop forever", "mode": "agent"})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	body := decodeBody[errorBody](t, rec)
	require.Equal(t, "MODEL_ERROR", body.Code)
	require.Contains(t, body.Error, "maximum number of steps")
}

// TestAgentModeValidation: unknown modes are rejected.
func TestAgentModeValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "hi", "mode": "autopilot"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "VALIDATION", decodeBody[errorBody](t, rec).Code)
}

// TestAgentNonReadOnlyQueryStep: a run_query with mutating SQL is refused
// as a step error and fed back to the model, which can still answer.
func TestAgentNonReadOnlyQueryStep(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		queryCall("DELETE FROM orders", "oops"),
		finalAnswer("I could not run that query."),
	)

	rec := env.do(t, http.MethodPost, "/api/v1/messages", testPrimaryToken,
		map[string]string{"question": "delete things", "mode": "agent"})
	events := parseSSE(t, rec.Body.String())
	require.Equal(t, []string{"meta", "step", "delta", "done"}, eventNames(events))
	require.Contains(t, events[1].data["error"], "read-only")
	// Nothing executed, nothing captured for the failed step.
	require.Empty(t, env.db.lastSQL)
	records, err := env.training.ListAll(context.Background(), false)
	require.NoError(t, err)
	require.Empty(t, records)
}

// TestAgentTurnInputPropagation: schema context and prompt reach the model.
func TestAgentTurnInputPropagation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = func(_ context.Context, in assistant.AgentTurnInput) (*assistant.AgentTurnOutput, error) {
		require.Equal(t, env.db.schema, in.SchemaContext)
		require.Equal(t, "check schema", in.Prompt)
		return &assistant.AgentTurnOutput{Answer: "ok"}, nil
	}
	rec := env.do(t, http.MethodPost, "/api/v1/ask", testPrimaryToken,
		map[string]string{"question": "check schema", "mode": "agent"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
