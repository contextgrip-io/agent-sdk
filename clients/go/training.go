package aichat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Verdict values accepted by RateMessage.
const (
	VerdictGood = "good"
	VerdictBad  = "bad"
)

// maxExportLineBytes bounds a single NDJSON line in ExportTraining. Row
// samples are bounded server-side, so real lines stay far below this.
const maxExportLineBytes = 16 << 20 // 16 MiB

// TrainingStats is the response of GET /api/v1/training/stats.
type TrainingStats struct {
	Records int `json:"records"`
	// Evaluated counts records carrying an eval verdict.
	Evaluated       int        `json:"evaluated"`
	FirstCapturedAt *time.Time `json:"firstCapturedAt,omitempty"`
	LastCapturedAt  *time.Time `json:"lastCapturedAt,omitempty"`
}

// TrainingExportLine is one JSONL line of GET /api/v1/training/export. The
// field layout matches ContextGrip's training export, so dumps from both
// sources merge downstream without transformation.
type TrainingExportLine struct {
	ID         string             `json:"id"`
	CapturedAt time.Time          `json:"capturedAt"`
	Connection TrainingConnection `json:"connection"`
	Context    TrainingContext    `json:"context"`
	Query      TrainingQuery      `json:"query"`
	Response   TrainingResponse   `json:"response"`
	Eval       *TrainingEval      `json:"eval,omitempty"`
}

// TrainingConnection identifies the database a record was captured against.
type TrainingConnection struct {
	// ID is a stable non-secret hash of host:port/dbname.
	ID string `json:"id"`
	// Name is the database name from DATABASE_URL.
	Name   string `json:"name"`
	Engine string `json:"engine"`
}

// TrainingContext carries capture provenance.
type TrainingContext struct {
	// Session is "chat" (SSE) or "ask" (one-shot).
	Session string `json:"session,omitempty"`
	// SourceMessageID is the assistant message id, for dedupe.
	SourceMessageID string `json:"sourceMessageId,omitempty"`
}

// TrainingQuery is the captured question and generated SQL.
type TrainingQuery struct {
	SQL string `json:"sql"`
	// Intent is the natural-language question.
	Intent string `json:"intent,omitempty"`
}

// TrainingResponse is the bounded execution outcome of the captured query.
type TrainingResponse struct {
	Columns         []string `json:"columns,omitempty"`
	RowCount        int      `json:"rowCount"`
	Truncated       bool     `json:"truncated"`
	ExecutionTimeMs int      `json:"executionTimeMs"`
	Error           string   `json:"error,omitempty"`
	RowSample       [][]any  `json:"rowSample,omitempty"`
}

// TrainingEval is the explicit verdict attached to a record, when rated.
type TrainingEval struct {
	Verdict string `json:"verdict"` // "good" or "bad"
}

// ExportOptions filters GET /api/v1/training/export.
type ExportOptions struct {
	// IncludeRows includes bounded result row samples in each record. nil
	// leaves the server default (true).
	IncludeRows *bool
	// EvaluatedOnly restricts the export to records with an eval verdict.
	EvaluatedOnly bool
}

// RateMessage rates an assistant answer as "good" or "bad", writing a
// training record. Explicit evals bypass the capture toggle. Rating the same
// message again updates the verdict; only assistant messages that carry SQL
// can be rated.
func (c *Client) RateMessage(ctx context.Context, id string, verdict string) error {
	if id == "" {
		return errors.New("aichat: message id must not be empty")
	}
	if verdict != VerdictGood && verdict != VerdictBad {
		return fmt.Errorf("aichat: verdict must be %q or %q, got %q", VerdictGood, VerdictBad, verdict)
	}
	body := struct {
		Verdict string `json:"verdict"`
	}{Verdict: verdict}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/messages/"+url.PathEscape(id)+"/eval", body, nil)
}

// TrainingCapture reads the automatic-capture setting. When enabled (the
// server default), every completed chat exchange is recorded as a training
// record.
func (c *Client) TrainingCapture(ctx context.Context) (bool, error) {
	var out struct {
		Enabled bool `json:"enabled"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/training/capture", nil, &out)
	return out.Enabled, err
}

// SetTrainingCapture enables or disables automatic capture and returns the
// updated setting. Admin: only the primary APP_ACCESS_TOKEN may call this.
func (c *Client) SetTrainingCapture(ctx context.Context, enabled bool) (bool, error) {
	body := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	var out struct {
		Enabled bool `json:"enabled"`
	}
	err := c.doJSON(ctx, http.MethodPut, "/api/v1/training/capture", body, &out)
	return out.Enabled, err
}

// TrainingStats returns training-record counts and the capture time range.
func (c *Client) TrainingStats(ctx context.Context) (TrainingStats, error) {
	var out TrainingStats
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/training/stats", nil, &out)
	return out, err
}

// ExportTraining streams the training-record dump (application/x-ndjson),
// decoding one TrainingExportLine per line and calling fn for each. fn
// returning an error stops the stream and returns that error unchanged.
//
// The server stops the stream at a 64 MiB byte budget; compare the number of
// fn calls with TrainingStats to detect truncation.
func (c *Client) ExportTraining(ctx context.Context, opts ExportOptions, fn func(TrainingExportLine) error) error {
	if fn == nil {
		return errors.New("aichat: fn must not be nil")
	}
	path := "/api/v1/training/export"
	q := url.Values{}
	if opts.IncludeRows != nil {
		q.Set("includeRows", strconv.FormatBool(*opts.IncludeRows))
	}
	if opts.EvaluatedOnly {
		q.Set("evaluatedOnly", "true")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxExportLineBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec TrainingExportLine
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("aichat: decoding training export line: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// DeleteTrainingRecords deletes ALL training records and returns the number
// removed. Admin: only the primary APP_ACCESS_TOKEN may call this.
func (c *Client) DeleteTrainingRecords(ctx context.Context) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	err := c.doJSON(ctx, http.MethodDelete, "/api/v1/training/records", nil, &out)
	return out.Deleted, err
}
