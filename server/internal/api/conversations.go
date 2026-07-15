package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
)

// conversationView matches the Conversation schema.
type conversationView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// messageView matches the Message schema.
type messageView struct {
	ID        string                   `json:"id"`
	Role      string                   `json:"role"`
	Text      string                   `json:"text,omitempty"`
	SQL       string                   `json:"sql,omitempty"`
	Result    *chatstore.ResultSummary `json:"result,omitempty"`
	Error     string                   `json:"error,omitempty"`
	CreatedAt string                   `json:"createdAt"`
}

func toConversationView(conv chatstore.Conversation) conversationView {
	return conversationView{
		ID:        conv.ID,
		Title:     conv.Title,
		CreatedAt: conv.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: conv.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func toMessageView(msg chatstore.Message) messageView {
	return messageView{
		ID:        msg.ID,
		Role:      msg.Role,
		Text:      msg.Text,
		SQL:       msg.SQL,
		Result:    msg.Result,
		Error:     msg.Error,
		CreatedAt: msg.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cfg.Chat.ListConversations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to list conversations")
		return
	}
	out := make([]conversationView, 0, len(rows))
	for _, conv := range rows {
		out = append(out, toConversationView(conv))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	conv, err := s.cfg.Chat.GetConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load conversation")
		return
	}
	if conv == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "conversation not found")
		return
	}
	messages, err := s.cfg.Chat.ListMessages(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load messages")
		return
	}
	views := make([]messageView, 0, len(messages))
	for _, msg := range messages {
		views = append(views, toMessageView(msg))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation": toConversationView(*conv),
		"messages":     views,
	})
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Chat.DeleteConversation(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to delete conversation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
