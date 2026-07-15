// Package api wires the HTTP surface defined by openapi.yaml: bearer-token
// auth, the NL->SQL chat loop (one-shot and SSE), conversations, named
// tokens, health/readiness, and the embedded UI.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/tokenstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/trainingstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/webui"
)

// Version is surfaced in /api/v1/status.
const Version = "0.1.0"

// Bounds for the chat loop. Everything user- or model-supplied is bounded
// BEFORE it reaches the model, the stream, or the store.
const (
	maxQuestionChars  = 4_000
	schemaMaxChars    = 10_000
	historyTurns      = 6
	queryRowLimit     = 100
	queryTimeout      = 30 * time.Second
	displaySampleRows = 20
	sampleCellChars   = 256
	titleMaxChars     = 80
	readyPingTimeout  = 2 * time.Second
)

// Database is the query surface the handlers need; *dbx.DB implements it and
// tests substitute a fake.
type Database interface {
	SchemaContext(ctx context.Context, maxChars int) (string, error)
	RunReadOnly(ctx context.Context, sql string, limit int, timeout time.Duration) (*dbx.QueryResult, error)
	Ping(ctx context.Context) error
}

// Config assembles a Server.
type Config struct {
	// Model is nil when ANTHROPIC_API_KEY is not configured; chat endpoints
	// then return 503 NOT_CONFIGURED.
	Model assistant.Client
	// ModelID is reported by /api/v1/status even when Model is nil.
	ModelID string
	// DB is nil when DATABASE_URL is not configured.
	DB       Database
	Chat     *chatstore.Store
	Tokens   *tokenstore.Store
	Training *trainingstore.Store
	// ConnectionID/ConnectionName tag training-export lines: a stable
	// non-secret hash of host:port/dbname and the database name, derived
	// from DATABASE_URL via dbx.ConnectionIdentity. Never credentials.
	ConnectionID   string
	ConnectionName string
	// PrimaryTokenSHA256 is the SHA-256 of APP_ACCESS_TOKEN. Required unless
	// DevNoAuth is set.
	PrimaryTokenSHA256 []byte
	// DevNoAuth disables auth entirely (AI_CHAT_DEV_NO_AUTH=1 — local
	// development only). Every request is treated as the primary token.
	DevNoAuth bool
}

// Server is the http.Handler for the whole service.
type Server struct {
	cfg     Config
	metrics *metrics
	router  chi.Router
}

// New builds the router.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, metrics: newMetrics()}

	r := chi.NewRouter()
	r.Use(s.metricsMiddleware)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Get("/metrics", s.handleMetrics)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authMiddleware)
		// Unknown /api/v1 routes are JSON 404s — note auth runs first, so an
		// unauthenticated probe of an unknown route still gets a 401.
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown API route")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, "NOT_FOUND", "method not allowed")
		})
		r.Get("/status", s.handleStatus)
		r.Post("/ask", s.handleAsk)
		r.Post("/messages", s.handleMessages)
		r.Post("/messages/{id}/eval", s.handleEvalMessage)
		r.Get("/conversations", s.handleListConversations)
		r.Get("/conversations/{id}", s.handleGetConversation)
		r.Delete("/conversations/{id}", s.handleDeleteConversation)
		r.Route("/training", func(r chi.Router) {
			r.Get("/capture", s.handleGetTrainingCapture)
			r.With(s.requireAdmin).Put("/capture", s.handleSetTrainingCapture)
			r.Get("/stats", s.handleTrainingStats)
			r.Get("/export", s.handleTrainingExport)
			r.With(s.requireAdmin).Delete("/records", s.handleDeleteTrainingRecords)
		})
		r.Route("/tokens", func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/", s.handleListTokens)
			r.Post("/", s.handleCreateToken)
			r.Delete("/{id}", s.handleDeleteToken)
		})
	})

	// Everything else: JSON 404 under /api, embedded UI (with SPA fallback)
	// otherwise. The UI is unauthenticated.
	ui := webui.Handler()
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown API route")
			return
		}
		ui.ServeHTTP(w, r)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			writeError(w, http.StatusMethodNotAllowed, "NOT_FOUND", "method not allowed")
			return
		}
		ui.ServeHTTP(w, r)
	})

	s.router = r
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// configured reports whether the chat loop can run at all.
func (s *Server) configured() bool {
	return s.cfg.Model != nil && s.cfg.DB != nil
}
