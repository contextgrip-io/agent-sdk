package aichat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aichat "github.com/contextgrip-io/agent-sdk/clients/go"
)

func TestRateMessage(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/messages/a1/eval" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body struct {
			Verdict string `json:"verdict"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Verdict != "good" {
			t.Errorf("body = %+v, err = %v, want verdict good", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"recorded":true}`)
	}))
	defer srv.Close()

	c := aichat.New(srv.URL, "tok")
	ctx := context.Background()

	// Invalid verdicts and empty ids are rejected client-side, before any
	// request is sent.
	if err := c.RateMessage(ctx, "a1", "excellent"); err == nil {
		t.Error("RateMessage accepted verdict \"excellent\"")
	}
	if err := c.RateMessage(ctx, "a1", ""); err == nil {
		t.Error("RateMessage accepted empty verdict")
	}
	if err := c.RateMessage(ctx, "", "good"); err == nil {
		t.Error("RateMessage accepted empty message id")
	}
	if hits != 0 {
		t.Fatalf("server hit %d times by client-side-rejected calls", hits)
	}

	if err := c.RateMessage(ctx, "a1", "good"); err != nil {
		t.Fatalf("RateMessage: %v", err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
}

func TestTrainingCaptureGetAndSet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/training/capture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"enabled":true}`)
	})
	mux.HandleFunc("PUT /api/v1/training/capture", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Enabled {
			t.Errorf("body.enabled = true, want false")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"enabled": body.Enabled})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := aichat.New(srv.URL, "primary-token")
	ctx := context.Background()

	enabled, err := c.TrainingCapture(ctx)
	if err != nil {
		t.Fatalf("TrainingCapture: %v", err)
	}
	if !enabled {
		t.Error("TrainingCapture = false, want true")
	}

	enabled, err = c.SetTrainingCapture(ctx, false)
	if err != nil {
		t.Fatalf("SetTrainingCapture: %v", err)
	}
	if enabled {
		t.Error("SetTrainingCapture returned true, want false")
	}
}

func TestSetTrainingCaptureAdminRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"named tokens cannot manage training capture","code":"ADMIN_REQUIRED"}`)
	}))
	defer srv.Close()

	_, err := aichat.New(srv.URL, "named-token").SetTrainingCapture(context.Background(), false)
	var apiErr *aichat.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*APIError) failed for %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
	if apiErr.Code != "ADMIN_REQUIRED" {
		t.Errorf("Code = %q, want ADMIN_REQUIRED", apiErr.Code)
	}
}

func TestTrainingStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/training/stats" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"records":120,"evaluated":17,"firstCapturedAt":"2026-07-01T00:00:00Z","lastCapturedAt":"2026-07-15T09:30:00Z"}`)
	}))
	defer srv.Close()

	stats, err := aichat.New(srv.URL, "tok").TrainingStats(context.Background())
	if err != nil {
		t.Fatalf("TrainingStats: %v", err)
	}
	if stats.Records != 120 || stats.Evaluated != 17 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.FirstCapturedAt == nil || !stats.FirstCapturedAt.Equal(mustTime(t, "2026-07-01T00:00:00Z")) {
		t.Errorf("FirstCapturedAt = %v", stats.FirstCapturedAt)
	}
	if stats.LastCapturedAt == nil || !stats.LastCapturedAt.Equal(mustTime(t, "2026-07-15T09:30:00Z")) {
		t.Errorf("LastCapturedAt = %v", stats.LastCapturedAt)
	}
}

func TestTrainingStatsEmptyStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"records":0,"evaluated":0}`)
	}))
	defer srv.Close()

	stats, err := aichat.New(srv.URL, "tok").TrainingStats(context.Background())
	if err != nil {
		t.Fatalf("TrainingStats: %v", err)
	}
	if stats.FirstCapturedAt != nil || stats.LastCapturedAt != nil {
		t.Errorf("capture range = %v..%v, want nil..nil", stats.FirstCapturedAt, stats.LastCapturedAt)
	}
}

// exportLine builds one NDJSON line with the given id, intent, and optional
// verdict.
func exportLine(t *testing.T, id, intent, verdict string) string {
	t.Helper()
	rec := map[string]any{
		"id":         id,
		"capturedAt": "2026-07-15T10:00:00Z",
		"connection": map[string]any{"id": "conn-hash", "name": "appdb", "engine": "postgresql"},
		"context":    map[string]any{"session": "chat", "sourceMessageId": "m-" + id},
		"query":      map[string]any{"sql": "SELECT 1", "intent": intent},
		"response": map[string]any{
			"columns": []string{"?column?"}, "rowCount": 1, "truncated": false,
			"executionTimeMs": 2, "rowSample": []any{[]any{1}},
		},
	}
	if verdict != "" {
		rec["eval"] = map[string]any{"verdict": verdict}
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal export line: %v", err)
	}
	return string(buf)
}

// ndjsonServer serves body as application/x-ndjson in flushed chunks so
// lines arrive split across many reads.
func ndjsonServer(t *testing.T, wantQuery string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/training/export" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != wantQuery {
			t.Errorf("query = %q, want %q", r.URL.RawQuery, wantQuery)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		for i := 0; i < len(body); i += 1024 {
			end := min(i+1024, len(body))
			io.WriteString(w, body[i:end])
			flusher.Flush()
		}
	}))
}

func TestExportTraining(t *testing.T) {
	// The middle record's intent pushes its line well past bufio.Scanner's
	// default 64 KiB token limit, so the scanner buffer must grow.
	longIntent := strings.Repeat("q", 96*1024)
	body := exportLine(t, "r1", "how many users?", "good") + "\n" +
		exportLine(t, "r2", longIntent, "") + "\n" +
		"\n" + // blank line: skipped
		exportLine(t, "r3", "top products", "bad") + "\n"
	includeRows := false
	srv := ndjsonServer(t, "evaluatedOnly=true&includeRows=false", body)
	defer srv.Close()

	var got []aichat.TrainingExportLine
	err := aichat.New(srv.URL, "tok").ExportTraining(context.Background(), aichat.ExportOptions{
		IncludeRows:   &includeRows,
		EvaluatedOnly: true,
	}, func(line aichat.TrainingExportLine) error {
		got = append(got, line)
		return nil
	})
	if err != nil {
		t.Fatalf("ExportTraining: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("decoded %d records, want 3", len(got))
	}
	if got[0].ID != "r1" || got[1].ID != "r2" || got[2].ID != "r3" {
		t.Errorf("record order = %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
	first := got[0]
	if first.Connection != (aichat.TrainingConnection{ID: "conn-hash", Name: "appdb", Engine: "postgresql"}) {
		t.Errorf("connection = %+v", first.Connection)
	}
	if first.Context.Session != "chat" || first.Context.SourceMessageID != "m-r1" {
		t.Errorf("context = %+v", first.Context)
	}
	if first.Query.SQL != "SELECT 1" || first.Query.Intent != "how many users?" {
		t.Errorf("query = %+v", first.Query)
	}
	if first.Response.RowCount != 1 || first.Response.ExecutionTimeMs != 2 {
		t.Errorf("response = %+v", first.Response)
	}
	if !first.CapturedAt.Equal(mustTime(t, "2026-07-15T10:00:00Z")) {
		t.Errorf("capturedAt = %v", first.CapturedAt)
	}
	if first.Eval == nil || first.Eval.Verdict != "good" {
		t.Errorf("eval = %+v", first.Eval)
	}
	if len(got[1].Query.Intent) != len(longIntent) {
		t.Errorf("long line intent length = %d, want %d", len(got[1].Query.Intent), len(longIntent))
	}
	if got[1].Eval != nil {
		t.Errorf("got[1].Eval = %+v, want nil", got[1].Eval)
	}
	if got[2].Eval == nil || got[2].Eval.Verdict != "bad" {
		t.Errorf("got[2].Eval = %+v", got[2].Eval)
	}
}

func TestExportTrainingEarlyStop(t *testing.T) {
	body := exportLine(t, "r1", "a", "") + "\n" +
		exportLine(t, "r2", "b", "") + "\n" +
		exportLine(t, "r3", "c", "") + "\n"
	// Zero options: no query parameters are sent, leaving server defaults.
	srv := ndjsonServer(t, "", body)
	defer srv.Close()

	sentinel := errors.New("stop after first record")
	var calls int
	err := aichat.New(srv.URL, "tok").ExportTraining(context.Background(), aichat.ExportOptions{},
		func(line aichat.TrainingExportLine) error {
			calls++
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExportTraining returned %v, want the sentinel error", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
}

func TestExportTrainingUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"missing or invalid bearer token","code":"UNAUTHORIZED"}`)
	}))
	defer srv.Close()

	err := aichat.New(srv.URL, "bad").ExportTraining(context.Background(), aichat.ExportOptions{},
		func(aichat.TrainingExportLine) error {
			t.Error("fn called for a failed request")
			return nil
		})
	var apiErr *aichat.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "UNAUTHORIZED" {
		t.Fatalf("err = %v, want UNAUTHORIZED APIError", err)
	}
}

func TestDeleteTrainingRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/training/records" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"deleted":42}`)
	}))
	defer srv.Close()

	n, err := aichat.New(srv.URL, "primary-token").DeleteTrainingRecords(context.Background())
	if err != nil {
		t.Fatalf("DeleteTrainingRecords: %v", err)
	}
	if n != 42 {
		t.Errorf("deleted = %d, want 42", n)
	}
}
