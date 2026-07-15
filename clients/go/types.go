package aichat

import "time"

// Status is the response of GET /api/v1/status.
type Status struct {
	Version string `json:"version"`
	Model   string `json:"model"`
	Engine  string `json:"engine"`
	Ready   bool   `json:"ready"`
	// Features lists the enabled surfaces from AI_CHAT_FEATURES: "chat",
	// "agent", "board".
	Features []string `json:"features"`
	// WritesEnabled is true when AI_CHAT_WRITE_DATABASE_URL is configured,
	// so approvals can execute.
	WritesEnabled bool `json:"writesEnabled"`
}

// Mode values for AskRequest.Mode.
const (
	ModeChat  = "chat"
	ModeAgent = "agent"
)

// AskRequest is the request body for Ask and StreamMessage.
type AskRequest struct {
	Question string `json:"question"`
	// ConversationID continues an existing conversation; leave empty to
	// start a new one.
	ConversationID string `json:"conversationId,omitempty"`
	// Mode is "chat" (default when empty) or "agent". Agent mode lets the
	// model take multiple tool steps: read-only queries run automatically
	// and writes become approvals. It requires the "agent" feature; a
	// conversation keeps the mode of its first message.
	Mode string `json:"mode,omitempty"`
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
	ConversationID     string `json:"conversationId"`
	UserMessageID      string `json:"userMessageId"`
	AssistantMessageID string `json:"assistantMessageId"`
	// SQL carries the generated read-only SQL in chat mode; in agent mode
	// it may be empty, with Steps carrying the queries instead.
	SQL         string         `json:"sql,omitempty"`
	Result      *ResultSummary `json:"result,omitempty"`
	ResultError string         `json:"resultError,omitempty"`
	Answer      string         `json:"answer"`
	// Steps holds agent-mode tool steps, in execution order.
	Steps []Step `json:"steps,omitempty"`
	// PendingApprovalID is set when the turn ended awaiting a write
	// approval.
	PendingApprovalID string `json:"pendingApprovalId,omitempty"`
}

// Conversation is a conversation summary.
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Mode      string    `json:"mode"` // fixed by the conversation's first message
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
	ID     string         `json:"id"`
	Role   string         `json:"role"` // "user" or "assistant"
	Text   string         `json:"text,omitempty"`
	SQL    string         `json:"sql,omitempty"`
	Result *ResultSummary `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
	// Steps holds agent-mode tool steps persisted with the message.
	Steps []Step `json:"steps,omitempty"`
	// PendingApprovalID is set while this message's proposed write awaits a
	// decision.
	PendingApprovalID string    `json:"pendingApprovalId,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
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
