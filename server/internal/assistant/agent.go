package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Agent mode: the model drives a tool loop with two tools — run_query
// (read-only SQL, executed automatically) and propose_write (a mutation,
// which becomes a human approval and ends the turn) — and finishes with a
// plain-text answer.

// Tool names the agent loop understands.
const (
	ToolRunQuery     = "run_query"
	ToolProposeWrite = "propose_write"
)

// AgentCall is one tool invocation by the model.
type AgentCall struct {
	Tool      string `json:"tool"` // ToolRunQuery | ToolProposeWrite
	SQL       string `json:"sql"`
	Summary   string `json:"summary,omitempty"`   // run_query: one-line description
	Rationale string `json:"rationale,omitempty"` // propose_write: why the write is needed
}

// AgentExchange is one completed (or, for a pending write, half-completed)
// tool exchange. The transcript of exchanges is serializable so board tasks
// can persist it and resume after an approval decision injects the Result.
type AgentExchange struct {
	Call   AgentCall `json:"call"`
	Result string    `json:"result,omitempty"` // JSON tool result; "" while a write awaits approval
}

// AgentHistoryTurn is one prior question->answer pair from the conversation,
// replayed so agent follow-ups resolve against earlier turns.
type AgentHistoryTurn struct {
	Question string
	Answer   string
}

// AgentTurnInput is everything one agent model turn needs. The Anthropic
// transcript is reconstructed from Prompt + Exchanges with synthetic tool-use
// ids, so it never depends on provider-side state.
type AgentTurnInput struct {
	SchemaContext string
	Prompt        string
	History       []AgentHistoryTurn
	Exchanges     []AgentExchange
}

// AgentTurnOutput is what the model decided: exactly one of Call (execute a
// tool) or Answer (finish the turn) is set.
type AgentTurnOutput struct {
	Call   *AgentCall
	Answer string
}

// AgentTurn implements the agent model turn for the Anthropic client.
//
// Thinking is deliberately NOT enabled here: the transcript is rebuilt from
// the serialized exchanges on every turn, and extended thinking with tool use
// requires replaying signed thinking blocks that a synthetic transcript
// cannot provide.
func (c *AnthropicClient) AgentTurn(ctx context.Context, in AgentTurnInput) (*AgentTurnOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()

	system := fmt.Sprintf(`You are a data agent for a PostgreSQL database. Answer the user's request by taking tool steps, then finish with a plain-text answer.

Rules:
- Use run_query for read-only investigation: exactly one SELECT (or WITH ... SELECT) statement per call, plus a one-line summary of why. Results are returned to you; large results are truncated.
- Never put data-modifying SQL in run_query. If fulfilling the request requires INSERT/UPDATE/DELETE/DDL, call propose_write with the exact single statement and a short rationale. A human must approve it; the turn ends after a proposal.
- Take as few steps as possible. When you have what you need, reply with a concise plain-text answer (no tool call). Lead with the direct answer; under 120 words; no headings.
- Only reference tables and columns from the schema below.

Database schema:
%s`, in.SchemaContext)

	tools := []anthropic.ToolUnionParam{
		{OfTool: &anthropic.ToolParam{
			Name:        ToolRunQuery,
			Description: anthropic.String("Execute one read-only SQL statement (SELECT or WITH ... SELECT) against the database and get the result."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"sql":     map[string]any{"type": "string", "description": "One read-only SQL statement. No semicolons except an optional trailing one."},
					"summary": map[string]any{"type": "string", "description": "One line: what this query checks."},
				},
				Required: []string{"sql", "summary"},
			},
		}},
		{OfTool: &anthropic.ToolParam{
			Name:        ToolProposeWrite,
			Description: anthropic.String("Propose one data-modifying SQL statement for human approval. The turn ends after this call; the write only runs if a human approves it."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"sql":       map[string]any{"type": "string", "description": "Exactly one SQL statement to run if approved."},
					"rationale": map[string]any{"type": "string", "description": "One or two sentences: why this write fulfills the request."},
				},
				Required: []string{"sql", "rationale"},
			},
		}},
	}

	messages := make([]anthropic.MessageParam, 0, len(in.History)*2+len(in.Exchanges)*2+1)
	for _, turn := range in.History {
		if turn.Question == "" || turn.Answer == "" {
			continue
		}
		messages = append(messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(turn.Question)),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock(turn.Answer)),
		)
	}
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(in.Prompt)))
	for i, ex := range in.Exchanges {
		// Synthetic, position-stable tool ids: both sides of every exchange
		// are rebuilt here, so the ids only need to match each other.
		id := fmt.Sprintf("toolu_step_%d", i+1)
		input := map[string]string{"sql": ex.Call.SQL}
		switch ex.Call.Tool {
		case ToolProposeWrite:
			input["rationale"] = ex.Call.Rationale
		default:
			input["summary"] = ex.Call.Summary
		}
		result := ex.Result
		if result == "" {
			result = `{"status":"pending approval"}`
		}
		messages = append(messages,
			anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(id, input, ex.Call.Tool)),
			anthropic.NewUserMessage(anthropic.NewToolResultBlock(id, result, false)),
		)
	}

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8192,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  messages,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("agent turn: %w", err)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(b.Text)
		case anthropic.ToolUseBlock:
			// One tool call per turn: the transcript is rebuilt around a
			// single call+result pair, so additional parallel calls in the
			// same response are ignored (the model re-requests them next
			// turn if still needed).
			call, err := parseAgentCall(b.Name, b.Input)
			if err != nil {
				return nil, err
			}
			return &AgentTurnOutput{Call: call}, nil
		}
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" {
		return nil, fmt.Errorf("agent turn: model returned neither a tool call nor an answer")
	}
	return &AgentTurnOutput{Answer: answer}, nil
}

func parseAgentCall(name string, input json.RawMessage) (*AgentCall, error) {
	var args struct {
		SQL       string `json:"sql"`
		Summary   string `json:"summary"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("agent turn: decode %s input: %w", name, err)
	}
	return &AgentCall{Tool: name, SQL: args.SQL, Summary: args.Summary, Rationale: args.Rationale}, nil
}
