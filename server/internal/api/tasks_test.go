package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/taskstore"
)

// fileTask posts a task and returns its view.
func fileTask(t *testing.T, env *testEnv, title, prompt string) taskView {
	t.Helper()
	rec := env.do(t, http.MethodPost, "/api/v1/tasks", testPrimaryToken,
		map[string]string{"title": title, "prompt": prompt})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	task := decodeBody[taskView](t, rec)
	require.Equal(t, taskstore.StatusQueued, task.Status)
	return task
}

// waitTaskStatus polls the API until the task reaches the wanted status.
func waitTaskStatus(t *testing.T, env *testEnv, id, want string) map[string]any {
	t.Helper()
	var detail map[string]any
	require.Eventually(t, func() bool {
		rec := env.do(t, http.MethodGet, "/api/v1/tasks/"+id, testPrimaryToken, nil)
		if rec.Code != http.StatusOK {
			return false
		}
		detail = decodeBody[map[string]any](t, rec)
		return detail["task"].(map[string]any)["status"] == want
	}, 5*time.Second, 10*time.Millisecond, "task %s never reached %s", id, want)
	return detail
}

// TestTaskLifecycleDone: file -> runner claims -> steps persist -> done with
// answer; the query step is captured for training (session "task").
func TestTaskLifecycleDone(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		queryCall("SELECT count(*) FROM orders", "count"),
		finalAnswer("There are 42 orders."),
	)
	env.startTaskRunner(t)

	task := fileTask(t, env, "Count orders", "How many orders are there?")
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusDone)

	taskDoc := detail["task"].(map[string]any)
	require.Equal(t, "There are 42 orders.", taskDoc["answer"])
	steps := detail["steps"].([]any)
	require.Len(t, steps, 1)
	require.Equal(t, "SELECT count(*) FROM orders", steps[0].(map[string]any)["sql"])
	require.NotContains(t, detail, "pendingApproval")

	records, err := env.training.ListAll(context.Background(), false)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "task", records[0].Session)
	require.Equal(t, task.ID+":0", records[0].SourceMessageID)
	require.Equal(t, "How many orders are there?", records[0].Intent)

	// Listing filters by status.
	list := decodeBody[[]taskView](t, env.do(t, http.MethodGet, "/api/v1/tasks?status=done", testPrimaryToken, nil))
	require.Len(t, list, 1)
	require.Equal(t, task.ID, list[0].ID)
	require.Empty(t, decodeBody[[]taskView](t, env.do(t, http.MethodGet, "/api/v1/tasks?status=queued", testPrimaryToken, nil)))
}

// TestTaskApprovalResume: a proposed write pauses the task in
// needs_approval; approving executes the write and resumes the task to done.
func TestTaskApprovalResume(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		writeCall("UPDATE orders SET status = 'ok'", "fix them"),
		finalAnswer("Statuses fixed."),
	)
	env.startTaskRunner(t)

	task := fileTask(t, env, "Fix statuses", "Fix the broken statuses")
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusNeedsApproval)

	pending := detail["pendingApproval"].(map[string]any)
	approvalID := pending["id"].(string)
	require.Equal(t, "UPDATE orders SET status = 'ok'", pending["sql"])
	require.Equal(t, task.ID, pending["source"].(map[string]any)["taskId"])

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"UPDATE orders SET status = 'ok'"}, env.writeDB.executed)

	detail = waitTaskStatus(t, env, task.ID, taskstore.StatusDone)
	require.Equal(t, "Statuses fixed.", detail["task"].(map[string]any)["answer"])

	// The decision landed in the transcript (injected tool_result) and left
	// a visible note step.
	stored, err := env.tasks.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.Contains(t, stored.Transcript[0].Result, "approved")
	foundNote := false
	for _, step := range stored.Steps {
		foundNote = foundNote || (step.Kind == "note" && step.SQL == "UPDATE orders SET status = 'ok'")
	}
	require.True(t, foundNote, "expected a note step recording the write outcome")
}

// TestTaskRejectionResume: rejecting the write also resumes the task — the
// model sees the rejection and answers accordingly.
func TestTaskRejectionResume(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(
		writeCall("DELETE FROM orders", "clear everything"),
		finalAnswer("Understood — I did not delete anything."),
	)
	env.startTaskRunner(t)

	task := fileTask(t, env, "Dangerous", "Delete all orders")
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusNeedsApproval)
	approvalID := detail["pendingApproval"].(map[string]any)["id"].(string)

	rec := env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "reject"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, env.writeDB.executed)

	detail = waitTaskStatus(t, env, task.ID, taskstore.StatusDone)
	require.Contains(t, detail["task"].(map[string]any)["answer"], "did not delete")
	stored, err := env.tasks.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.Contains(t, stored.Transcript[0].Result, "rejected")
}

// TestTaskCancelQueued: canceling a queued task flips it immediately (no
// runner involved).
func TestTaskCancelQueued(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil) // runner not started
	task := fileTask(t, env, "Waiting", "never runs")

	rec := env.do(t, http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, taskstore.StatusCanceled, decodeBody[taskView](t, rec).Status)

	// Canceling again: already finished.
	rec = env.do(t, http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "TASK_FINISHED", decodeBody[errorBody](t, rec).Code)
}

// TestTaskCancelRunning: a running task is cooperatively canceled between
// steps and stays canceled.
func TestTaskCancelRunning(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	turnStarted := make(chan struct{})
	release := make(chan struct{})
	env.model.agentTurn = func(ctx context.Context, in assistant.AgentTurnInput) (*assistant.AgentTurnOutput, error) {
		if len(in.Exchanges) == 0 {
			close(turnStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			out := queryCall("SELECT 1", "blocked turn")
			return &out, nil
		}
		out := finalAnswer("should never finish")
		return &out, nil
	}
	env.startTaskRunner(t)

	task := fileTask(t, env, "Long", "runs for a while")
	select {
	case <-turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never started the task")
	}

	rec := env.do(t, http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, taskstore.StatusCanceled, decodeBody[taskView](t, rec).Status)

	close(release) // the loop notices the cancel between steps and stops

	// The task stays canceled — the runner must not overwrite it.
	time.Sleep(150 * time.Millisecond)
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusCanceled)
	require.NotEqual(t, "should never finish", detail["task"].(map[string]any)["answer"])
}

// TestTaskCancelNeedsApproval: canceling an approval-blocked task also
// auto-rejects its pending approval.
func TestTaskCancelNeedsApproval(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)
	env.model.agentTurn = agentScript(writeCall("UPDATE t SET x = 1", "r"))
	env.startTaskRunner(t)

	task := fileTask(t, env, "Blocked", "propose a write")
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusNeedsApproval)
	approvalID := detail["pendingApproval"].(map[string]any)["id"].(string)

	rec := env.do(t, http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, taskstore.StatusCanceled, decodeBody[taskView](t, rec).Status)

	// The approval went down with the task.
	appr, err := env.approvals.Get(context.Background(), approvalID)
	require.NoError(t, err)
	require.Equal(t, "rejected", appr.Status)
	require.Empty(t, decodeBody[[]approvalView](t, env.do(t, http.MethodGet, "/api/v1/approvals", testPrimaryToken, nil)))

	// Deciding it afterwards conflicts.
	rec = env.do(t, http.MethodPost, "/api/v1/approvals/"+approvalID, testPrimaryToken,
		map[string]string{"decision": "approve"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "ALREADY_DECIDED", decodeBody[errorBody](t, rec).Code)
}

// TestTaskDelete: finished tasks delete; active ones 409 TASK_ACTIVE.
func TestTaskDelete(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil) // runner not started: tasks stay queued
	task := fileTask(t, env, "Active", "still queued")

	rec := env.do(t, http.MethodDelete, "/api/v1/tasks/"+task.ID, testPrimaryToken, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "TASK_ACTIVE", decodeBody[errorBody](t, rec).Code)

	rec = env.do(t, http.MethodPost, "/api/v1/tasks/"+task.ID+"/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = env.do(t, http.MethodDelete, "/api/v1/tasks/"+task.ID, testPrimaryToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, decodeBody[map[string]bool](t, rec)["deleted"])

	rec = env.do(t, http.MethodGet, "/api/v1/tasks/"+task.ID, testPrimaryToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskMaxStepsFailure: a task that exceeds the step budget fails with
// the error recorded.
func TestTaskMaxStepsFailure(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) { cfg.AgentMaxSteps = 1 })
	env.model.agentTurn = agentScript(
		queryCall("SELECT 1", "one"),
		queryCall("SELECT 2", "two"),
		finalAnswer("never"),
	)
	env.startTaskRunner(t)

	task := fileTask(t, env, "Runaway", "loop forever")
	detail := waitTaskStatus(t, env, task.ID, taskstore.StatusFailed)
	require.Contains(t, detail["task"].(map[string]any)["error"], "maximum number of steps")
}

// TestTaskFeatureDisabled: every /api/v1/tasks route is 403 without the
// "board" feature.
func TestTaskFeatureDisabled(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *Config) { cfg.Features = ParseFeatures("chat,agent") })

	probes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tasks"},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/tasks/some-id"},
		{http.MethodDelete, "/api/v1/tasks/some-id"},
		{http.MethodPost, "/api/v1/tasks/some-id/cancel"},
	}
	for _, probe := range probes {
		rec := env.do(t, probe.method, probe.path, testPrimaryToken, map[string]string{"title": "t", "prompt": "p"})
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s", probe.method, probe.path)
		require.Equal(t, "FEATURE_DISABLED", decodeBody[errorBody](t, rec).Code)
	}
}

// TestTaskValidation: title/prompt bounds and unknown-task 404s.
func TestTaskValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, nil)

	rec := env.do(t, http.MethodPost, "/api/v1/tasks", testPrimaryToken, map[string]string{"title": "", "prompt": "p"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = env.do(t, http.MethodPost, "/api/v1/tasks", testPrimaryToken, map[string]string{"title": "t", "prompt": ""})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = env.do(t, http.MethodGet, "/api/v1/tasks/nope", testPrimaryToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	rec = env.do(t, http.MethodPost, "/api/v1/tasks/nope/cancel", testPrimaryToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// NOT_CONFIGURED without model/DB.
	env2 := newTestEnv(t, func(cfg *Config) { cfg.Model = nil })
	rec = env2.do(t, http.MethodPost, "/api/v1/tasks", testPrimaryToken, map[string]string{"title": "t", "prompt": "p"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "NOT_CONFIGURED", decodeBody[errorBody](t, rec).Code)
}
