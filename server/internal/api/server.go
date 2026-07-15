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

	"github.com/contextgrip-io/agent-sdk/server/internal/approvalstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/assistant"
	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/dbx"
	"github.com/contextgrip-io/agent-sdk/server/internal/taskstore"
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
	DB        Database
	Chat      *chatstore.Store
	Tokens    *tokenstore.Store
	Training  *trainingstore.Store
	Approvals *approvalstore.Store
	Tasks     *taskstore.Store
	// WriteDB is nil when AI_CHAT_WRITE_DATABASE_URL is not configured;
	// approvals can then only be rejected (approve -> 409 WRITES_DISABLED).
	WriteDB WriteExecutor
	// Features are the enabled surfaces from AI_CHAT_FEATURES ("chat" is
	// always present). Empty enables every surface.
	Features []string
	// AgentMaxSteps bounds tool steps per agent run (0 = default 8).
	AgentMaxSteps int
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
	cfg      Config
	metrics  *metrics
	router   chi.Router
	taskWake chan struct{}
}

// AllFeatures is the default AI_CHAT_FEATURES value.
var AllFeatures = []string{"chat", "agent", "board"}

// ParseFeatures normalizes an AI_CHAT_FEATURES value: a comma-separated
// subset of chat|agent|board. Unknown entries are dropped, "chat" is always
// implied, and empty input enables everything.
func ParseFeatures(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), AllFeatures...)
	}
	enabled := map[string]bool{"chat": true}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		switch name {
		case "chat", "agent", "board":
			enabled[name] = true
		}
	}
	out := make([]string, 0, len(AllFeatures))
	for _, name := range AllFeatures {
		if enabled[name] {
			out = append(out, name)
		}
	}
	return out
}

// featureEnabled reports whether a surface is enabled on this instance.
func (s *Server) featureEnabled(name string) bool {
	features := s.cfg.Features
	if len(features) == 0 {
		features = AllFeatures
	}
	for _, f := range features {
		if f == name {
			return true
		}
	}
	return false
}

// requireFeature guards a route subtree behind an AI_CHAT_FEATURES surface.
func (s *Server) requireFeature(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.featureEnabled(name) {
				writeError(w, http.StatusForbidden, "FEATURE_DISABLED",
					"the "+name+" feature is not enabled on this instance (AI_CHAT_FEATURES)")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// New builds the router.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, metrics: newMetrics(), taskWake: make(chan struct{}, 1)}

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
		r.Get("/approvals", s.handleListApprovals)
		r.Post("/approvals/{id}", s.handleDecideApproval)
		r.Route("/tasks", func(r chi.Router) {
			r.Use(s.requireFeature("board"))
			r.Get("/", s.handleListTasks)
			r.Post("/", s.handleCreateTask)
			r.Get("/{id}", s.handleGetTask)
			r.Delete("/{id}", s.handleDeleteTask)
			r.Post("/{id}/cancel", s.handleCancelTask)
		})
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
