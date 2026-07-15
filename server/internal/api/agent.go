package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/contextgrip-io/agent-sdk/server/internal/approvalstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/textutil"
)

// DefaultAgentMaxSteps bounds tool steps per agent run (AI_CHAT_AGENT_MAX_STEPS).
const DefaultAgentMaxSteps = 8

// errAgentCanceled is the loopError code used when a cooperative cancel
// stops an agent run between steps (board tasks only).
const errAgentCanceled = "CANCELED"

// agentRun is one agent-loop invocation. Exchanges/steps are non-empty only
// when resuming a board task after an approval decision.
type agentRun struct {
	prompt    string
	history   []assistant.AgentHistoryTurn
	exchanges []assistant.AgentExchange
	steps     []chatstore.Step

	// Approval source: exactly one is set.
	sourceMessageID string
	sourceTaskID    string

	// hooks (all optional). onStep also receives the completed tool
	// exchange so callers (the task runner) can persist steps + transcript
	// in lockstep as they complete.
	onStep     func(step chatstore.Step, ex assistant.AgentExchange)
	onApproval func(appr approvalstore.Approval)
	// checkCancel is polled between steps; returning true stops the run
	// with errAgentCanceled (board cooperative cancel).
	checkCancel func() bool
}

// agentOutcome is how an agent run ended: Answer set (finished), or
// Approval set (turn ended awaiting a write decision).
type agentOutcome struct {
	steps     []chatstore.Step
	exchanges []assistant.AgentExchange
	answer    string
	approval  *approvalstore.Approval
}

// runAgent drives the agent tool loop: model turn -> execute run_query /
// persist propose_write -> repeat, until a plain-text answer or a pending
// approval ends the run. ctx drives model/database work; persistCtx
// (context.WithoutCancel) is used for store writes.
func (s *Server) runAgent(ctx, persistCtx context.Context, run agentRun) (*agentOutcome, *loopError) {
	schemaContext, err := s.cfg.DB.SchemaContext(ctx, schemaMaxChars)
	if err != nil {
		return nil, &loopError{code: "NOT_CONFIGURED", status: http.StatusServiceUnavailable,
			message: "failed to read the database schema: " + err.Error()}
	}

	maxSteps := s.cfg.AgentMaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultAgentMaxSteps
	}

	exchanges := run.exchanges
	steps := run.steps

	for {
		if run.checkCancel != nil && run.checkCancel() {
			return nil, &loopError{code: errAgentCanceled, status: 0, message: "task canceled"}
		}

		out, err := s.cfg.Model.AgentTurn(ctx, assistant.AgentTurnInput{
			SchemaContext: schemaContext,
			Prompt:        run.prompt,
			History:       run.history,
			Exchanges:     exchanges,
		})
		if err != nil {
			s.metrics.modelErrors.Add(1)
			return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway,
				message: "the agent turn failed: " + err.Error()}
		}
		if out.Call == nil {
			return &agentOutcome{steps: steps, exchanges: exchanges, answer: out.Answer}, nil
		}
		// A cancel during the model turn stops the run before the requested
		// tool executes.
		if run.checkCancel != nil && run.checkCancel() {
			return nil, &loopError{code: errAgentCanceled, status: 0, message: "task canceled"}
		}
		if len(exchanges) >= maxSteps {
			s.metrics.modelErrors.Add(1)
			return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway,
				message: fmt.Sprintf("the agent exceeded the maximum number of steps (%d) without answering", maxSteps)}
		}

		call := *out.Call
		switch call.Tool {
		case assistant.ToolRunQuery:
			step, resultJSON := s.executeAgentQuery(ctx, len(steps), call)
			exchange := assistant.AgentExchange{Call: call, Result: resultJSON}
			steps = append(steps, step)
			exchanges = append(exchanges, exchange)
			if run.onStep != nil {
				run.onStep(step, exchange)
			}

		case assistant.ToolProposeWrite:
			// Single-statement gate at proposal time: a multi-statement
			// proposal is refused back to the model, not surfaced to humans.
			if err := dbx.VerifySingleStatement(call.SQL); err != nil {
				exchanges = append(exchanges, assistant.AgentExchange{
					Call:   call,
					Result: encodeToolError("write proposal rejected: " + err.Error()),
				})
				continue
			}
			appr, storeErr := s.cfg.Approvals.Create(persistCtx, approvalstore.Approval{
				ID:        uuid.NewString(),
				SQL:       call.SQL,
				Rationale: call.Rationale,
				MessageID: run.sourceMessageID,
				TaskID:    run.sourceTaskID,
			})
			if storeErr != nil {
				return nil, &loopError{code: "STORE_ERROR", status: http.StatusInternalServerError,
					message: "failed to store the write proposal"}
			}
			// The pending write's exchange has no Result yet; the approval
			// decision injects it (board tasks resume from there).
			exchanges = append(exchanges, assistant.AgentExchange{Call: call})
			if run.onApproval != nil {
				run.onApproval(*appr)
			}
			return &agentOutcome{steps: steps, exchanges: exchanges, approval: appr}, nil

		default:
			exchanges = append(exchanges, assistant.AgentExchange{
				Call:   call,
				Result: encodeToolError(fmt.Sprintf("unknown tool %q", call.Tool)),
			})
		}
	}
}

// executeAgentQuery runs one run_query tool call through the read-only path
// and returns the visible step plus the JSON tool result for the model.
func (s *Server) executeAgentQuery(ctx context.Context, index int, call assistant.AgentCall) (chatstore.Step, string) {
	step := chatstore.Step{
		Index:   index,
		Kind:    "query",
		Summary: call.Summary,
		SQL:     call.SQL,
	}
	if step.Summary == "" {
		step.Summary = "run a read-only query"
	}
	if err := dbx.VerifyReadOnlySQL(call.SQL); err != nil {
		step.Error = "query rejected: " + err.Error()
		return step, encodeToolError(step.Error)
	}
	started := time.Now()
	queryResult, execErr := s.cfg.DB.RunReadOnly(ctx, call.SQL, queryRowLimit, queryTimeout)
	elapsed := int(time.Since(started).Milliseconds())
	if execErr != nil {
		step.Error = execErr.Error()
		return step, encodeToolError(step.Error)
	}
	sample := queryResult.Rows
	if len(sample) > displaySampleRows {
		sample = sample[:displaySampleRows]
	}
	// Bound before the sample reaches the model, the stream, or any store.
	textutil.BoundCells(sample, sampleCellChars)
	step.Result = &chatstore.ResultSummary{
		Columns:         queryResult.Columns,
		RowSample:       sample,
		RowCount:        queryResult.RowCount,
		Truncated:       queryResult.Truncated,
		ExecutionTimeMs: elapsed,
	}
	encoded, err := json.Marshal(step.Result)
	if err != nil {
		return step, encodeToolError("failed to encode result")
	}
	return step, string(encoded)
}

func encodeToolError(message string) string {
	encoded, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return `{"error":"internal error"}`
	}
	return string(encoded)
}

// agentHistory rebuilds prior question->answer pairs of an agent
// conversation for follow-up context.
func (s *Server) agentHistory(ctx context.Context, conversationID string) []assistant.AgentHistoryTurn {
	messages, err := s.cfg.Chat.ListMessages(ctx, conversationID)
	if err != nil {
		return nil
	}
	var turns []assistant.AgentHistoryTurn
	lastQuestion := ""
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			lastQuestion = msg.Text
		case "assistant":
			if lastQuestion != "" && msg.Text != "" && msg.Error == "" {
				turns = append(turns, assistant.AgentHistoryTurn{Question: lastQuestion, Answer: msg.Text})
			}
			lastQuestion = ""
		}
	}
	if len(turns) > historyTurns {
		turns = turns[len(turns)-historyTurns:]
	}
	return turns
}
