package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/contextgrip-io/ai-chat/server/internal/approvalstore"
	"github.com/contextgrip-io/ai-chat/server/internal/assistant"
	"github.com/contextgrip-io/ai-chat/server/internal/chatstore"
	"github.com/contextgrip-io/ai-chat/server/internal/dbx"
	"github.com/contextgrip-io/ai-chat/server/internal/textutil"
)

// askRequest matches the AskRequest schema.
type askRequest struct {
	Question       string `json:"question"`
	ConversationID string `json:"conversationId"`
	Mode           string `json:"mode"` // "chat" (default) | "agent"
}

// askResponse matches the AskResponse schema.
type askResponse struct {
	ConversationID     string                   `json:"conversationId"`
	UserMessageID      string                   `json:"userMessageId"`
	AssistantMessageID string                   `json:"assistantMessageId"`
	SQL                string                   `json:"sql,omitempty"`
	Result             *chatstore.ResultSummary `json:"result,omitempty"`
	ResultError        string                   `json:"resultError,omitempty"`
	Answer             string                   `json:"answer"`
	Steps              []chatstore.Step         `json:"steps,omitempty"`
	PendingApprovalID  string                   `json:"pendingApprovalId,omitempty"`
}

// preflightError carries a pre-stream failure back to the handler.
type preflightError struct {
	status  int
	code    string
	message string
}

func (e *preflightError) Error() string { return e.message }

// exchange is the validated, persisted starting state of one question.
type exchange struct {
	question string
	// mode is the conversation's effective mode: "chat" or "agent". A
	// conversation keeps the mode of its first message; the request mode
	// only applies to new conversations.
	mode    string
	conv    *chatstore.Conversation
	userMsg *chatstore.Message
	history []assistant.Turn
}

// preflight validates the request, resolves or creates the conversation, and
// persists the user message. All failures here are plain JSON errors — for
// the SSE endpoint they happen before any headers are written.
func (s *Server) preflight(r *http.Request) (*exchange, *preflightError) {
	if !s.configured() {
		return nil, &preflightError{http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"chat is not configured: ANTHROPIC_API_KEY and DATABASE_URL are required"}
	}
	var body askRequest
	if err := decodeJSON(r, &body); err != nil {
		return nil, &preflightError{http.StatusBadRequest, "VALIDATION", "invalid request body"}
	}
	question := strings.TrimSpace(body.Question)
	if question == "" {
		return nil, &preflightError{http.StatusBadRequest, "VALIDATION", "question is required"}
	}
	if len(question) > maxQuestionChars {
		return nil, &preflightError{http.StatusBadRequest, "VALIDATION", "question is too long"}
	}
	requestMode := strings.TrimSpace(body.Mode)
	if requestMode == "" {
		requestMode = "chat"
	}
	if requestMode != "chat" && requestMode != "agent" {
		return nil, &preflightError{http.StatusBadRequest, "VALIDATION", "mode must be chat or agent"}
	}

	ctx := r.Context()
	store := s.cfg.Chat

	var conv *chatstore.Conversation
	var err error
	if strings.TrimSpace(body.ConversationID) != "" {
		conv, err = store.GetConversation(ctx, strings.TrimSpace(body.ConversationID))
		if err != nil {
			return nil, &preflightError{http.StatusInternalServerError, "STORE_ERROR", "failed to load conversation"}
		}
		if conv == nil {
			return nil, &preflightError{http.StatusNotFound, "NOT_FOUND", "conversation not found"}
		}
	}

	// A conversation keeps the mode of its first message: for continuations
	// the stored mode wins; the request mode only picks the mode of a NEW
	// conversation.
	mode := requestMode
	if conv != nil {
		mode = conv.Mode
		if mode == "" {
			mode = "chat"
		}
	}
	if mode == "agent" && !s.featureEnabled("agent") {
		return nil, &preflightError{http.StatusForbidden, "FEATURE_DISABLED",
			"agent mode is not enabled on this instance (AI_CHAT_FEATURES)"}
	}
	if conv == nil {
		conv, err = store.CreateConversation(ctx, uuid.NewString(), textutil.TruncateUTF8(question, titleMaxChars), mode)
		if err != nil {
			return nil, &preflightError{http.StatusInternalServerError, "STORE_ERROR", "failed to create conversation"}
		}
	}

	history := s.chatHistory(ctx, conv.ID)

	userMsg, err := store.AppendMessage(ctx, chatstore.Message{
		ID:             uuid.NewString(),
		ConversationID: conv.ID,
		Role:           "user",
		Text:           question,
	})
	if err != nil {
		if errors.Is(err, chatstore.ErrConversationFull) {
			return nil, &preflightError{http.StatusBadRequest, "CONVERSATION_FULL", "this conversation is full — start a new one"}
		}
		return nil, &preflightError{http.StatusInternalServerError, "STORE_ERROR", "failed to store message"}
	}

	s.metrics.questions.Add(1)
	return &exchange{question: question, mode: mode, conv: conv, userMsg: userMsg, history: history}, nil
}

// chatHistory rebuilds recent question->SQL turns for follow-up context.
func (s *Server) chatHistory(ctx context.Context, conversationID string) []assistant.Turn {
	messages, err := s.cfg.Chat.ListMessages(ctx, conversationID)
	if err != nil {
		return nil
	}
	var turns []assistant.Turn
	lastQuestion := ""
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			lastQuestion = msg.Text
		case "assistant":
			if lastQuestion != "" && msg.SQL != "" && msg.Error == "" {
				turns = append(turns, assistant.Turn{Question: lastQuestion, SQL: msg.SQL})
			}
			lastQuestion = ""
		}
	}
	if len(turns) > historyTurns {
		turns = turns[len(turns)-historyTurns:]
	}
	return turns
}

// runOutcome is what the loop produced past the preflight stage.
type runOutcome struct {
	sql          string
	summary      *chatstore.ResultSummary
	resultError  string
	answer       string
	assistantMsg *chatstore.Message
}

// loopHooks lets the SSE handler observe the loop stage by stage; the
// one-shot handler leaves the optional hooks nil. onSQL/onResult emission
// failures are deliberately not fatal: a client disconnect cancels the
// request context, which winds the loop down through the model/database
// calls while persistCtx keeps every store write alive. Only onDelta
// returns an error, so a dead stream stops the answer stream promptly and
// the partial answer is persisted.
type loopHooks struct {
	onSQL    func(sql string)
	onResult func(summary *chatstore.ResultSummary, execErr string, elapsedMs int)
	onDelta  func(text string) error
}

// loopError is a mid-loop failure. An assistant error record has already
// been persisted by the time it is returned.
type loopError struct {
	code    string // NOT_CONFIGURED-style machine slug for the one-shot path
	status  int    // HTTP status for the one-shot path
	message string // human message (also what the SSE error event carries)
}

func (e *loopError) Error() string { return e.message }

// runExchange executes schema -> generate -> verify -> execute -> explain.
// ctx drives the expensive external work (model, database) and is the
// request context so client disconnects cancel it; persistCtx must survive
// disconnects (context.WithoutCancel) so records are never lost after SSE
// headers are sent. session ("ask" | "chat") tags the training capture.
func (s *Server) runExchange(ctx, persistCtx context.Context, ex *exchange, session string, hooks loopHooks) (*runOutcome, *loopError) {
	store := s.cfg.Chat

	// persistFailure records an assistant error turn; the record must
	// survive client disconnects.
	persistFailure := func(text, errMsg string) {
		_, _ = store.AppendMessage(persistCtx, chatstore.Message{
			ID:             uuid.NewString(),
			ConversationID: ex.conv.ID,
			Role:           "assistant",
			Text:           text,
			Error:          errMsg,
		})
	}

	schemaContext, err := s.cfg.DB.SchemaContext(ctx, schemaMaxChars)
	if err != nil {
		msg := "failed to read the database schema: " + err.Error()
		persistFailure("", msg)
		return nil, &loopError{code: "NOT_CONFIGURED", status: http.StatusServiceUnavailable, message: msg}
	}

	generatedSQL, err := s.cfg.Model.GenerateSQL(ctx, assistant.GenerateSQLInput{
		SchemaContext: schemaContext,
		Question:      ex.question,
		History:       ex.history,
	})
	if err != nil {
		s.metrics.modelErrors.Add(1)
		msg := "the assistant could not generate a query: " + err.Error()
		persistFailure("", msg)
		return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway, message: msg}
	}

	// Reject non-read-only output BEFORE the SQL reaches the client: the UI
	// renders copy actions on it, so a mutating statement must never be
	// surfaced even though execution would refuse it anyway.
	if err := dbx.VerifyReadOnlySQL(generatedSQL); err != nil {
		msg := "the assistant generated a query that is not read-only, so it was discarded (" + err.Error() + ") — try rephrasing the question"
		persistFailure("", msg)
		return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway, message: msg}
	}
	if hooks.onSQL != nil {
		hooks.onSQL(generatedSQL)
	}

	result := assistant.ResultInfo{}
	var summary *chatstore.ResultSummary
	started := time.Now()
	queryResult, execErr := s.cfg.DB.RunReadOnly(ctx, generatedSQL, queryRowLimit, queryTimeout)
	elapsed := int(time.Since(started).Milliseconds())
	if execErr != nil {
		result.Error = execErr.Error()
	} else {
		sample := queryResult.Rows
		if len(sample) > displaySampleRows {
			sample = sample[:displaySampleRows]
		}
		// Bound every cell before the sample reaches the model, the SSE
		// stream, or the chat store — a wide text/blob column must not turn
		// the sample into an unbounded payload.
		textutil.BoundCells(sample, sampleCellChars)
		result = assistant.ResultInfo{
			Columns:         queryResult.Columns,
			RowSample:       sample,
			RowCount:        queryResult.RowCount,
			Truncated:       queryResult.Truncated,
			ExecutionTimeMs: elapsed,
		}
		summary = &chatstore.ResultSummary{
			Columns:         queryResult.Columns,
			RowSample:       sample,
			RowCount:        queryResult.RowCount,
			Truncated:       queryResult.Truncated,
			ExecutionTimeMs: elapsed,
		}
	}
	result.ExecutionTimeMs = elapsed
	if hooks.onResult != nil {
		hooks.onResult(summary, result.Error, elapsed)
	}

	answer, err := s.cfg.Model.StreamAnswer(ctx, assistant.AnswerInput{
		Question: ex.question,
		SQL:      generatedSQL,
		Result:   result,
	}, func(delta string) error {
		if hooks.onDelta != nil {
			return hooks.onDelta(delta)
		}
		return nil
	})
	if err != nil {
		s.metrics.modelErrors.Add(1)
		if answer == "" {
			msg := "the assistant could not explain the result: " + err.Error()
			persistFailure("", msg)
			return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway, message: msg}
		}
		// A partial answer is not a complete answer: persist what streamed
		// with the failure attached and surface an error instead of done.
		streamErr := "the answer stream was interrupted: " + err.Error()
		_, _ = store.AppendMessage(persistCtx, chatstore.Message{
			ID:             uuid.NewString(),
			ConversationID: ex.conv.ID,
			Role:           "assistant",
			Text:           answer,
			SQL:            generatedSQL,
			Result:         summary,
			Error:          streamErr,
		})
		return nil, &loopError{code: "MODEL_ERROR", status: http.StatusBadGateway, message: streamErr}
	}

	assistantMsg, storeErr := store.AppendMessage(persistCtx, chatstore.Message{
		ID:             uuid.NewString(),
		ConversationID: ex.conv.ID,
		Role:           "assistant",
		Text:           answer,
		SQL:            generatedSQL,
		Result:         summary,
		Error:          result.Error,
	})
	if storeErr != nil {
		return nil, &loopError{code: "STORE_ERROR", status: http.StatusInternalServerError,
			message: "answer was generated but could not be saved"}
	}

	// The exchange completed (SQL + outcome + answer): auto-capture it as a
	// training record when the toggle is on. Runs on persistCtx and swallows
	// failures — capture must never fail or lose the chat response.
	s.captureTraining(persistCtx, session, assistantMsg.ID, ex.question, generatedSQL, summary, result.Error)

	return &runOutcome{
		sql:          generatedSQL,
		summary:      summary,
		resultError:  result.Error,
		answer:       answer,
		assistantMsg: assistantMsg,
	}, nil
}

// agentChatOutcome is a finished agent-mode chat turn.
type agentChatOutcome struct {
	assistantMsg      *chatstore.Message
	steps             []chatstore.Step
	answer            string
	pendingApprovalID string
}

// agentChatHooks lets the SSE handler observe agent progress; the one-shot
// handler leaves them nil.
type agentChatHooks struct {
	onStep     func(step chatstore.Step)
	onApproval func(view approvalView)
}

// runAgentChat runs one agent-mode turn for a chat conversation and persists
// the assistant message (steps, answer, pending approval marker). The
// assistant message id is pre-generated so a proposed write can reference it
// as its source before the message lands.
func (s *Server) runAgentChat(ctx, persistCtx context.Context, ex *exchange, hooks agentChatHooks) (*agentChatOutcome, *loopError) {
	assistantMsgID := uuid.NewString()

	outcome, loopErr := s.runAgent(ctx, persistCtx, agentRun{
		prompt:          ex.question,
		history:         s.agentHistory(ctx, ex.conv.ID),
		sourceMessageID: assistantMsgID,
		onStep: func(step chatstore.Step, _ assistant.AgentExchange) {
			// Successful reads are training data (session "agent").
			if step.Kind == "query" && step.Error == "" && step.SQL != "" {
				s.captureTrainingStep(persistCtx, "agent",
					agentStepSourceID(assistantMsgID, step.Index), ex.question, step)
			}
			if hooks.onStep != nil {
				hooks.onStep(step)
			}
		},
		onApproval: func(appr approvalstore.Approval) {
			if hooks.onApproval != nil {
				hooks.onApproval(toApprovalView(appr))
			}
		},
	})
	if loopErr != nil {
		_, _ = s.cfg.Chat.AppendMessage(persistCtx, chatstore.Message{
			ID:             uuid.NewString(),
			ConversationID: ex.conv.ID,
			Role:           "assistant",
			Error:          loopErr.message,
		})
		return nil, loopErr
	}

	pendingApprovalID := ""
	if outcome.approval != nil {
		pendingApprovalID = outcome.approval.ID
	}
	assistantMsg, err := s.cfg.Chat.AppendMessage(persistCtx, chatstore.Message{
		ID:                assistantMsgID,
		ConversationID:    ex.conv.ID,
		Role:              "assistant",
		Text:              outcome.answer,
		Steps:             outcome.steps,
		PendingApprovalID: pendingApprovalID,
	})
	if err != nil {
		return nil, &loopError{code: "STORE_ERROR", status: http.StatusInternalServerError,
			message: "the agent turn finished but could not be saved"}
	}
	return &agentChatOutcome{
		assistantMsg:      assistantMsg,
		steps:             outcome.steps,
		answer:            outcome.answer,
		pendingApprovalID: pendingApprovalID,
	}, nil
}

// agentStepSourceID is the training-record dedupe key for a chat agent step.
func agentStepSourceID(messageID string, index int) string {
	return fmt.Sprintf("%s:%d", messageID, index)
}

// handleAsk is the one-shot JSON endpoint: same loop as the SSE endpoint,
// deltas collected into a single answer (agent mode collects steps).
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	ex, pfErr := s.preflight(r)
	if pfErr != nil {
		writeError(w, pfErr.status, pfErr.code, pfErr.message)
		return
	}
	persistCtx := context.WithoutCancel(r.Context())

	if ex.mode == "agent" {
		outcome, loopErr := s.runAgentChat(r.Context(), persistCtx, ex, agentChatHooks{})
		if loopErr != nil {
			writeError(w, loopErr.status, loopErr.code, loopErr.message)
			return
		}
		writeJSON(w, http.StatusOK, askResponse{
			ConversationID:     ex.conv.ID,
			UserMessageID:      ex.userMsg.ID,
			AssistantMessageID: outcome.assistantMsg.ID,
			Answer:             outcome.answer,
			Steps:              outcome.steps,
			PendingApprovalID:  outcome.pendingApprovalID,
		})
		return
	}

	outcome, loopErr := s.runExchange(r.Context(), persistCtx, ex, "ask", loopHooks{})
	if loopErr != nil {
		writeError(w, loopErr.status, loopErr.code, loopErr.message)
		return
	}
	writeJSON(w, http.StatusOK, askResponse{
		ConversationID:     ex.conv.ID,
		UserMessageID:      ex.userMsg.ID,
		AssistantMessageID: outcome.assistantMsg.ID,
		SQL:                outcome.sql,
		Result:             outcome.summary,
		ResultError:        outcome.resultError,
		Answer:             outcome.answer,
	})
}

// handleMessages answers one question over SSE:
// meta -> sql -> result -> delta* -> done (or error terminally).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	// Check streamability before preflight so an unsupported writer doesn't
	// leave a dangling user message in the store.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "STREAM_UNSUPPORTED", "streaming is not supported")
		return
	}
	ex, pfErr := s.preflight(r)
	if pfErr != nil {
		writeError(w, pfErr.status, pfErr.code, pfErr.message)
		return
	}

	// From here on, all errors flow through the SSE stream — and every
	// store write goes through persistCtx so a client disconnect cannot
	// lose the record.
	persistCtx := context.WithoutCancel(r.Context())

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload any) error {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	_ = emit("meta", map[string]string{"conversationId": ex.conv.ID, "userMessageId": ex.userMsg.ID})

	if ex.mode == "agent" {
		// Agent mode: meta -> step* -> (approval_required | delta) -> done.
		outcome, loopErr := s.runAgentChat(r.Context(), persistCtx, ex, agentChatHooks{
			onStep: func(step chatstore.Step) {
				_ = emit("step", step)
			},
			onApproval: func(view approvalView) {
				_ = emit("approval_required", view)
			},
		})
		if loopErr != nil {
			_ = emit("error", map[string]string{"message": loopErr.message})
			return
		}
		if outcome.answer != "" {
			_ = emit("delta", map[string]string{"text": outcome.answer})
		}
		done := map[string]string{"conversationId": ex.conv.ID, "assistantMessageId": outcome.assistantMsg.ID}
		if outcome.pendingApprovalID != "" {
			done["pendingApprovalId"] = outcome.pendingApprovalID
		}
		_ = emit("done", done)
		return
	}

	outcome, loopErr := s.runExchange(r.Context(), persistCtx, ex, "chat", loopHooks{
		onSQL: func(sql string) {
			_ = emit("sql", map[string]string{"sql": sql})
		},
		onResult: func(summary *chatstore.ResultSummary, execErr string, elapsedMs int) {
			if execErr != "" {
				_ = emit("result", map[string]any{"error": execErr, "executionTimeMs": elapsedMs})
				return
			}
			_ = emit("result", summary)
		},
		onDelta: func(text string) error {
			return emit("delta", map[string]string{"text": text})
		},
	})
	if loopErr != nil {
		_ = emit("error", map[string]string{"message": loopErr.message})
		return
	}
	_ = emit("done", map[string]string{"conversationId": ex.conv.ID, "assistantMessageId": outcome.assistantMsg.ID})
}
