package aichat

import "time"

// Status is the response of GET /api/v1/status.
type Status struct {
	Version string `json:"version"`
	Model   string `json:"model"`
	Engine  string `json:"engine"`
	Ready   bool   `json:"ready"`
}

// AskRequest is the request body for Ask and StreamMessage.
type AskRequest struct {
	Question string `json:"question"`
	// ConversationID continues an existing conversation; leave empty to
	// start a new one.
	ConversationID string `json:"conversationId,omitempty"`
}

// ResultSummary describes the outcome of executing the generated SQL.
type ResultSummary struct {
	Columns []string `json:"columns,omitempty"`
	// RowSample holds up to 20 rows; string cells are bounded to 256 chars.
	RowSample       [][]any `json:"rowSample,omitempty"`
	RowCount        int     `json:"rowCount"`
	Truncated       bool    `json:"truncated"`
	ExecutionTimeMs int     `json:"executionTimeMs"`
}

// AskResponse is the response of POST /api/v1/ask.
//
// Failed query execution is not an HTTP error: Result is nil, ResultError
// carries the failure, and Answer explains it.
type AskResponse struct {
	ConversationID     string         `json:"conversationId"`
	UserMessageID      string         `json:"userMessageId"`
	AssistantMessageID string         `json:"assistantMessageId"`
	SQL                string         `json:"sql"`
	Result             *ResultSummary `json:"result,omitempty"`
	ResultError        string         `json:"resultError,omitempty"`
	Answer             string         `json:"answer"`
}

// Conversation is a conversation summary.
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ConversationDetail is one conversation with its messages in order.
type ConversationDetail struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
}

// Message is a single message within a conversation.
type Message struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"` // "user" or "assistant"
	Text      string         `json:"text,omitempty"`
	SQL       string         `json:"sql,omitempty"`
	Result    *ResultSummary `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// TokenInfo describes a named API token. Raw token values are never
// returned by list endpoints.
type TokenInfo struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

// CreatedToken is the response of POST /api/v1/tokens. Token holds the raw
// token value, shown only in this response.
type CreatedToken struct {
	TokenInfo
	Token string `json:"token"`
}
