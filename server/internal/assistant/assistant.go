// Package assistant is the model boundary for the NL->SQL chat loop. The
// only network egress of the service happens here, authenticated with the
// operator's own Anthropic API key.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is used when AI_CHAT_MODEL is unset.
const DefaultModel = "claude-opus-4-8"

// Model calls are bounded so a hung upstream cannot hold a request goroutine
// for as long as the client stays connected.
const (
	generateTimeout = 2 * time.Minute
	answerTimeout   = 5 * time.Minute
)

// Turn is one prior question->SQL exchange, replayed so follow-up questions
// ("now only last month") resolve against earlier answers.
type Turn struct {
	Question string
	SQL      string
}

// GenerateSQLInput is everything the model needs to produce one statement.
type GenerateSQLInput struct {
	SchemaContext string
	Question      string
	History       []Turn
}

// ResultInfo is the bounded execution outcome handed to the explanation call.
type ResultInfo struct {
	Columns         []string
	RowSample       [][]any
	RowCount        int
	Truncated       bool
	ExecutionTimeMs int
	Error           string
}

// AnswerInput feeds the explanation call.
type AnswerInput struct {
	Question string
	SQL      string
	Result   ResultInfo
}

// Client is the model boundary; handlers depend on this interface so tests
// can run against a fake without network access.
type Client interface {
	Model() string
	GenerateSQL(ctx context.Context, in GenerateSQLInput) (string, error)
	StreamAnswer(ctx context.Context, in AnswerInput, emit func(delta string) error) (string, error)
	// AgentTurn runs one agent-mode model turn: given the prompt and the
	// tool exchanges so far, the model either requests one more tool call
	// or finishes with a plain-text answer.
	AgentTurn(ctx context.Context, in AgentTurnInput) (*AgentTurnOutput, error)
}

// ── Anthropic implementation ────────────────────────────────────────────────

// AnthropicClient implements Client against the Anthropic Messages API with
// adaptive thinking.
type AnthropicClient struct {
	client anthropic.Client
	model  string
}

// NewAnthropicClient builds the production model client. baseURL overrides
// the API endpoint when non-empty (AI_CHAT_ANTHROPIC_BASE_URL /
// ANTHROPIC_BASE_URL).
func NewAnthropicClient(apiKey, model, baseURL string) *AnthropicClient {
	if model == "" {
		model = DefaultModel
	}
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &AnthropicClient{
		client: anthropic.NewClient(opts...),
		model:  model,
	}
}

func (c *AnthropicClient) Model() string { return c.model }

// GenerateSQL asks the model for exactly one read-only SQL statement.
func (c *AnthropicClient) GenerateSQL(ctx context.Context, in GenerateSQLInput) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()

	system := fmt.Sprintf(`You are a SQL assistant for a PostgreSQL database. Generate one read-only SQL statement that answers the user's question.

Rules:
- Respond with ONLY the SQL statement. No prose, no markdown fences, no explanations.
- Exactly one statement, and it must be read-only: SELECT or WITH ... SELECT. Never INSERT, UPDATE, DELETE, or DDL.
- Only reference tables and columns from the schema below.
- Add a LIMIT when the question implies a potentially large list.

Database schema:
%s`, in.SchemaContext)

	messages := make([]anthropic.MessageParam, 0, len(in.History)*2+1)
	for _, turn := range in.History {
		if turn.Question == "" || turn.SQL == "" {
			continue
		}
		messages = append(messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(turn.Question)),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock(turn.SQL)),
		)
	}
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(in.Question)))

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8192,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  messages,
	})
	if err != nil {
		return "", fmt.Errorf("generate sql: %w", err)
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	sql := CleanGeneratedSQL(text.String())
	if sql == "" {
		return "", fmt.Errorf("model returned no SQL")
	}
	return sql, nil
}

// StreamAnswer streams a natural-language explanation of the query outcome,
// calling emit for every text delta. It returns the accumulated text even on
// error so callers can persist partial answers.
func (c *AnthropicClient) StreamAnswer(ctx context.Context, in AnswerInput, emit func(delta string) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()

	system := `You are a data assistant. The user asked a question about their PostgreSQL database; a SQL query was generated and executed. Explain the outcome.

- Lead with the direct answer to the question, using the result data.
- Be concise: a short paragraph, under 120 words. No headings.
- If the query failed, explain the error plainly and suggest how to fix the query.
- rowCount is only the number of rows the query RETURNED (capped, see truncated) — it is not a data total. For aggregate questions ("how many…", sums, averages) the answer is the VALUE inside rowSample (e.g. a count(*) query returns one row whose cell holds the total). Only cite rowCount when the question is about the returned list itself, and note when truncation means more rows exist.`

	userPayload := map[string]any{
		"question": in.Question,
		"sql":      in.SQL,
		"result": map[string]any{
			"columns":         in.Result.Columns,
			"rowSample":       in.Result.RowSample,
			"rowCount":        in.Result.RowCount,
			"truncated":       in.Result.Truncated,
			"executionTimeMs": in.Result.ExecutionTimeMs,
			"error":           in.Result.Error,
		},
	}
	encoded, err := json.Marshal(userPayload)
	if err != nil {
		return "", fmt.Errorf("encode answer input: %w", err)
	}

	stream := c.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 8192,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(string(encoded)))},
	})
	var text strings.Builder
	for stream.Next() {
		event := stream.Current()
		switch e := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			if d, ok := e.Delta.AsAny().(anthropic.TextDelta); ok && d.Text != "" {
				text.WriteString(d.Text)
				if err := emit(d.Text); err != nil {
					return text.String(), err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return text.String(), fmt.Errorf("stream answer: %w", err)
	}
	return text.String(), nil
}

// CleanGeneratedSQL strips markdown fences and surrounding noise from a
// model response that should contain only SQL.
func CleanGeneratedSQL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```sql")
		trimmed = strings.TrimPrefix(trimmed, "```SQL")
		trimmed = strings.TrimPrefix(trimmed, "```")
		if end := strings.LastIndex(trimmed, "```"); end != -1 {
			trimmed = trimmed[:end]
		}
	}
	return strings.TrimSpace(trimmed)
}
