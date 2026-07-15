package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/contextgrip-io/agent-sdk/server/internal/chatstore"
	"github.com/contextgrip-io/agent-sdk/server/internal/trainingstore"
)

// Training data: completed exchanges are captured silently (toggle, default
// on) and answers can be rated explicitly; records are dumped as JSONL whose
// line format matches ContextGrip's training export so dumps from both
// sources merge downstream without transformation.

// trainingExportMaxBytes bounds the serialized dump so a pathological store
// (many records at the per-record SQL cap) cannot balloon a single response.
// Package variable so tests can shrink it.
var trainingExportMaxBytes = 64 << 20

// ── export line format (merge-compatible with the ContextGrip export) ──────

type trainingExportConnection struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Engine string `json:"engine,omitempty"`
}

type trainingExportContext struct {
	Session         string `json:"session,omitempty"`
	Seq             int    `json:"seq,omitempty"`
	SourceMessageID string `json:"sourceMessageId,omitempty"`
}

type trainingExportQuery struct {
	SQL    string `json:"sql"`
	Intent string `json:"intent,omitempty"`
}

type trainingExportEval struct {
	Verdict string `json:"verdict"`
}

type trainingExportResponse struct {
	Columns         []string `json:"columns"`
	RowCount        int      `json:"rowCount"`
	Truncated       bool     `json:"truncated"`
	ExecutionTimeMs int      `json:"executionTimeMs"`
	Error           *string  `json:"error"`
	RowSample       [][]any  `json:"rowSample,omitempty"`
}

type trainingExportLine struct {
	ID         string                   `json:"id"`
	CapturedAt string                   `json:"capturedAt"`
	Connection trainingExportConnection `json:"connection"`
	Context    trainingExportContext    `json:"context"`
	Query      trainingExportQuery      `json:"query"`
	Response   trainingExportResponse   `json:"response"`
	Eval       *trainingExportEval      `json:"eval,omitempty"`
}

func (s *Server) exportLine(rec trainingstore.Record) trainingExportLine {
	line := trainingExportLine{
		ID:         rec.ID,
		CapturedAt: rec.CreatedAt.UTC().Format(time.RFC3339Nano),
		Connection: trainingExportConnection{
			ID:     s.cfg.ConnectionID,
			Name:   s.cfg.ConnectionName,
			Engine: "postgresql",
		},
		Context: trainingExportContext{Session: rec.Session, SourceMessageID: rec.SourceMessageID},
		Query:   trainingExportQuery{SQL: rec.SQL, Intent: rec.Intent},
		Response: trainingExportResponse{
			Columns:         rec.Columns,
			RowCount:        rec.RowCount,
			Truncated:       rec.Truncated,
			ExecutionTimeMs: rec.ExecutionTimeMs,
		},
	}
	if rec.ErrorMessage != "" {
		errMsg := rec.ErrorMessage
		line.Response.Error = &errMsg
	}
	if rec.Verdict != "" {
		line.Eval = &trainingExportEval{Verdict: rec.Verdict}
	}
	return line
}

// ── auto-capture ────────────────────────────────────────────────────────────

// captureTraining records one completed exchange when the capture toggle is
// on. Failures are logged and swallowed — capture must never fail the chat
// response — and ctx must be a persist context (context.WithoutCancel) so a
// client disconnect cannot lose the record.
func (s *Server) captureTraining(ctx context.Context, session, assistantMessageID, question, sqlText string, summary *chatstore.ResultSummary, execErr string) {
	if s.cfg.Training == nil {
		return
	}
	enabled, err := s.cfg.Training.CaptureEnabled(ctx)
	if err != nil {
		log.Printf("training capture: read setting: %v", err)
		return
	}
	if !enabled {
		return
	}
	rec := trainingstore.Record{
		ID:              uuid.NewString(),
		Session:         session,
		SourceMessageID: assistantMessageID,
		SQL:             sqlText,
		Intent:          question,
		ErrorMessage:    execErr,
	}
	if summary != nil {
		rec.Columns = summary.Columns
		rec.RowSample = summary.RowSample
		rec.RowCount = summary.RowCount
		rec.Truncated = summary.Truncated
		rec.ExecutionTimeMs = summary.ExecutionTimeMs
	}
	if err := s.cfg.Training.Upsert(ctx, rec); err != nil {
		log.Printf("training capture failed: %v", err)
	}
}

// captureTrainingStep records one successfully executed agent run_query
// step. sourceID is "<messageId>:<index>" (chat agent, session "agent") or
// "<taskId>:<index>" (board, session "task") — steps have no standalone ids
// and several may share one message/task, so the composite keeps the
// dedupe-by-source upsert unique per step.
func (s *Server) captureTrainingStep(ctx context.Context, session, sourceID, intent string, step chatstore.Step) {
	s.captureTraining(ctx, session, sourceID, intent, step.SQL, step.Result, step.Error)
}

// ── handlers ────────────────────────────────────────────────────────────────

// handleEvalMessage rates an assistant answer. Explicit evals bypass the
// capture toggle; upserts by message id so a re-rating updates the verdict.
func (s *Server) handleEvalMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Verdict string `json:"verdict"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid request body")
		return
	}
	if body.Verdict != "good" && body.Verdict != "bad" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "verdict must be good or bad")
		return
	}
	msg, err := s.cfg.Chat.GetMessage(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load message")
		return
	}
	if msg == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if msg.Role != "assistant" || msg.SQL == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "only assistant messages that carry SQL can be rated")
		return
	}

	rec := trainingstore.Record{
		ID:              uuid.NewString(),
		SourceMessageID: msg.ID,
		Verdict:         body.Verdict,
		SQL:             msg.SQL,
		Intent:          s.intentFor(r.Context(), msg),
		ErrorMessage:    msg.Error,
	}
	if msg.Result != nil {
		rec.Columns = msg.Result.Columns
		rec.RowSample = msg.Result.RowSample
		rec.RowCount = msg.Result.RowCount
		rec.Truncated = msg.Result.Truncated
		rec.ExecutionTimeMs = msg.Result.ExecutionTimeMs
	}
	if err := s.cfg.Training.Upsert(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to store eval")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"recorded": true})
}

// intentFor finds the natural-language question that produced an assistant
// message: the nearest preceding user message in the conversation.
func (s *Server) intentFor(ctx context.Context, msg *chatstore.Message) string {
	messages, err := s.cfg.Chat.ListMessages(ctx, msg.ConversationID)
	if err != nil {
		return ""
	}
	intent := ""
	for _, m := range messages {
		if m.Seq >= msg.Seq {
			break
		}
		if m.Role == "user" {
			intent = m.Text
		}
	}
	return intent
}

func (s *Server) handleGetTrainingCapture(w http.ResponseWriter, r *http.Request) {
	enabled, err := s.cfg.Training.CaptureEnabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to read training capture setting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
}

func (s *Server) handleSetTrainingCapture(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "enabled (boolean) is required")
		return
	}
	if err := s.cfg.Training.SetCaptureEnabled(r.Context(), *body.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to update training capture setting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": *body.Enabled})
}

// trainingStatsResponse matches the TrainingStats schema.
type trainingStatsResponse struct {
	Records         int    `json:"records"`
	Evaluated       int    `json:"evaluated"`
	FirstCapturedAt string `json:"firstCapturedAt,omitempty"`
	LastCapturedAt  string `json:"lastCapturedAt,omitempty"`
}

func (s *Server) handleTrainingStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.cfg.Training.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load training stats")
		return
	}
	resp := trainingStatsResponse{Records: stats.Records, Evaluated: stats.Evaluated}
	if stats.FirstAt != nil {
		resp.FirstCapturedAt = stats.FirstAt.UTC().Format(time.RFC3339Nano)
	}
	if stats.LastAt != nil {
		resp.LastCapturedAt = stats.LastAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTrainingExport streams records as JSONL (application/x-ndjson),
// oldest first, stopping at the byte budget. Compare line count with
// /api/v1/training/stats to detect truncation.
func (s *Server) handleTrainingExport(w http.ResponseWriter, r *http.Request) {
	includeRows := queryBool(r, "includeRows", true)
	evaluatedOnly := queryBool(r, "evaluatedOnly", false)

	records, err := s.cfg.Training.ListAll(r.Context(), evaluatedOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to load training records")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	written := 0
	for _, rec := range records {
		line := s.exportLine(rec)
		if includeRows {
			line.Response.RowSample = rec.RowSample
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			log.Printf("training export: encode record %s: %v", rec.ID, err)
			continue
		}
		if written+len(encoded)+1 > trainingExportMaxBytes {
			break
		}
		if _, err := w.Write(append(encoded, '\n')); err != nil {
			return // client went away
		}
		written += len(encoded) + 1
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *Server) handleDeleteTrainingRecords(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.cfg.Training.DeleteAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STORE_ERROR", "failed to delete training records")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

// queryBool parses a boolean query parameter with a default.
func queryBool(r *http.Request, name string, fallback bool) bool {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
