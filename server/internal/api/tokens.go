package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/contextgrip-io/agent-sdk/server/internal/tokenstore"
)

// tokenView matches the TokenInfo schema.
type tokenView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"createdAt"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
}

// createdTokenView matches the CreatedToken schema — the raw value is shown
// only here.
type createdTokenView struct {
	tokenView
	Token string `json:"token"`
}

func toTokenView(t tokenstore.Token) tokenView {
	view := tokenView{
		ID:          t.ID,
		Label:       t.Label,
		Fingerprint: t.Fingerprint,
		CreatedAt:   t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.LastUsedAt != nil {
		view.LastUsedAt = t.LastUsedAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cfg.Tokens.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to list tokens")
		return
	}
	out := make([]tokenView, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTokenView(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid request body")
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "label is required")
		return
	}
	if len(label) > 120 {
		writeError(w, http.StatusBadRequest, "VALIDATION", "label is too long (max 120 characters)")
		return
	}
	token, raw, err := s.cfg.Tokens.Create(r.Context(), label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to create token")
		return
	}
	writeJSON(w, http.StatusCreated, createdTokenView{tokenView: toTokenView(*token), Token: raw})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Tokens.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to delete token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
