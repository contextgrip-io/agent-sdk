package api

import (
	"context"
	"net/http"
)

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readiness matches the Readiness schema.
type readiness struct {
	Ready    bool `json:"ready"`
	Model    bool `json:"model"`
	Database bool `json:"database"`
}

func (s *Server) checkReadiness(ctx context.Context) readiness {
	r := readiness{Model: s.cfg.Model != nil}
	if s.cfg.DB != nil {
		pingCtx, cancel := context.WithTimeout(ctx, readyPingTimeout)
		defer cancel()
		r.Database = s.cfg.DB.Ping(pingCtx) == nil
	}
	r.Ready = r.Model && r.Database
	return r
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ready := s.checkReadiness(r.Context())
	status := http.StatusOK
	if !ready.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, ready)
}

// statusResponse matches the Status schema.
type statusResponse struct {
	Version string `json:"version"`
	Model   string `json:"model"`
	Engine  string `json:"engine"`
	Ready   bool   `json:"ready"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	model := s.cfg.ModelID
	if s.cfg.Model != nil {
		model = s.cfg.Model.Model()
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Version: Version,
		Model:   model,
		Engine:  "postgresql",
		Ready:   s.checkReadiness(r.Context()).Ready,
	})
}
