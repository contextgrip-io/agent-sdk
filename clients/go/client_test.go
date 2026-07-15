package aichat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	aichat "github.com/contextgrip-io/agent-sdk/clients/go"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/status" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"version":"0.1.0","model":"claude-sonnet-4-5","engine":"postgresql","ready":true,"features":["chat","agent","board"],"writesEnabled":true}`)
	}))
	defer srv.Close()

	got, err := aichat.New(srv.URL, "tok").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := aichat.Status{
		Version:       "0.1.0",
		Model:         "claude-sonnet-4-5",
		Engine:        "postgresql",
		Ready:         true,
		Features:      []string{"chat", "agent", "board"},
		WritesEnabled: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Status = %+v, want %+v", got, want)
	}
}

func TestAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/ask" {
			t.Errorf("path = %s, want /api/v1/ask", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var req aichat.AskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		want := aichat.AskRequest{Question: "how many users signed up last week?", ConversationID: "c1"}
		if req != want {
			t.Errorf("request = %+v, want %+v", req, want)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"conversationId": "c1",
			"userMessageId": "u1",
			"assistantMessageId": "a1",
			"sql": "SELECT count(*) FROM users WHERE created_at > now() - interval '7 days'",
			"result": {"columns":["count"],"rowSample":[[42]],"rowCount":1,"truncated":false,"executionTimeMs":12},
			"answer": "42 users signed up last week."
		}`)
	}))
	defer srv.Close()

	c := aichat.New(srv.URL, "test-token")
	got, err := c.Ask(context.Background(), aichat.AskRequest{
		Question:       "how many users signed up last week?",
		ConversationID: "c1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	want := aichat.AskResponse{
		ConversationID:     "c1",
		UserMessageID:      "u1",
		AssistantMessageID: "a1",
		SQL:                "SELECT count(*) FROM users WHERE created_at > now() - interval '7 days'",
		Result: &aichat.ResultSummary{
			Columns:         []string{"count"},
			RowSample:       [][]any{{float64(42)}},
			RowCount:        1,
			Truncated:       false,
			ExecutionTimeMs: 12,
		},
		Answer: "42 users signed up last week.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AskResponse = %+v, want %+v", got, want)
	}
}

func TestAskExecutionFailure(t *testing.T) {
	// Failed query execution is NOT an HTTP error: 200 with resultError set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"conversationId": "c1",
			"userMessageId": "u1",
			"assistantMessageId": "a1",
			"sql": "SELECT nope FROM users",
			"resultError": "column \"nope\" does not exist",
			"answer": "The query failed: the column does not exist."
		}`)
	}))
	defer srv.Close()

	got, err := aichat.New(srv.URL, "tok").Ask(context.Background(), aichat.AskRequest{Question: "q"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Result != nil {
		t.Errorf("Result = %+v, want nil", got.Result)
	}
	if want := `column "nope" does not exist`; got.ResultError != want {
		t.Errorf("ResultError = %q, want %q", got.ResultError, want)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"missing or invalid bearer token","code":"UNAUTHORIZED"}`)
	}))
	defer srv.Close()

	_, err := aichat.New(srv.URL, "bad-token").Status(context.Background())
	if err == nil {
		t.Fatal("Status: expected error, got nil")
	}
	var apiErr *aichat.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*APIError) failed for %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Code != "UNAUTHORIZED" {
		t.Errorf("Code = %q, want UNAUTHORIZED", apiErr.Code)
	}
	if apiErr.Message != "missing or invalid bearer token" {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

func TestAPIErrorNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream exploded")
	}))
	defer srv.Close()

	_, err := aichat.New(srv.URL, "tok").Status(context.Background())
	var apiErr *aichat.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*APIError) failed for %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Code != "" || apiErr.Message != "upstream exploded" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestConversationsCRUD(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"id":"c2","title":"Newest","createdAt":"2026-07-15T12:00:00Z","updatedAt":"2026-07-15T13:00:00Z"},
			{"id":"c1","title":"Older","createdAt":"2026-07-14T09:00:00Z","updatedAt":"2026-07-14T09:05:00Z"}
		]`)
	})
	mux.HandleFunc("GET /api/v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "c1" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"conversation not found","code":"NOT_FOUND"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"conversation": {"id":"c1","title":"Older","createdAt":"2026-07-14T09:00:00Z","updatedAt":"2026-07-14T09:05:00Z"},
			"messages": [
				{"id":"u1","role":"user","text":"how many users?","createdAt":"2026-07-14T09:00:00Z"},
				{"id":"a1","role":"assistant","text":"There are 42 users.","sql":"SELECT count(*) FROM users",
				 "result":{"columns":["count"],"rowSample":[[42]],"rowCount":1,"truncated":false,"executionTimeMs":9},
				 "createdAt":"2026-07-14T09:00:05Z"}
			]
		}`)
	})
	var deleted string
	mux.HandleFunc("DELETE /api/v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"deleted":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := aichat.New(srv.URL, "tok")
	ctx := context.Background()

	list, err := c.ListConversations(ctx)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 2 || list[0].ID != "c2" || list[1].ID != "c1" {
		t.Errorf("ListConversations = %+v", list)
	}
	if !list[0].UpdatedAt.Equal(mustTime(t, "2026-07-15T13:00:00Z")) {
		t.Errorf("UpdatedAt = %v", list[0].UpdatedAt)
	}

	detail, err := c.GetConversation(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if detail.Conversation.ID != "c1" || len(detail.Messages) != 2 {
		t.Fatalf("GetConversation = %+v", detail)
	}
	assistant := detail.Messages[1]
	if assistant.Role != "assistant" || assistant.SQL != "SELECT count(*) FROM users" {
		t.Errorf("assistant message = %+v", assistant)
	}
	if assistant.Result == nil || assistant.Result.RowCount != 1 || assistant.Result.ExecutionTimeMs != 9 {
		t.Errorf("assistant result = %+v", assistant.Result)
	}

	var apiErr *aichat.APIError
	if _, err := c.GetConversation(ctx, "missing"); !errors.As(err, &apiErr) || apiErr.Code != "NOT_FOUND" {
		t.Errorf("GetConversation(missing) err = %v, want NOT_FOUND APIError", err)
	}

	if err := c.DeleteConversation(ctx, "c1"); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if deleted != "c1" {
		t.Errorf("deleted id = %q, want c1", deleted)
	}
}

func TestTokenAdmin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"id":"t1","label":"ci","fingerprint":"deadbeef","createdAt":"2026-07-01T00:00:00Z","lastUsedAt":"2026-07-15T08:00:00Z"},
			{"id":"t2","label":"fresh","fingerprint":"cafef00d","createdAt":"2026-07-10T00:00:00Z"}
		]`)
	})
	mux.HandleFunc("POST /api/v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body struct {
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Label != "reporting" {
			t.Errorf("body = %+v, err = %v, want label reporting", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"t3","label":"reporting","fingerprint":"01234567","createdAt":"2026-07-15T12:00:00Z","token":"tok_raw_value_shown_once"}`)
	})
	var revoked string
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		revoked = r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"deleted":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := aichat.New(srv.URL, "primary-token")
	ctx := context.Background()

	tokens, err := c.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("ListTokens returned %d tokens", len(tokens))
	}
	if tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.Equal(mustTime(t, "2026-07-15T08:00:00Z")) {
		t.Errorf("tokens[0].LastUsedAt = %v", tokens[0].LastUsedAt)
	}
	if tokens[1].LastUsedAt != nil {
		t.Errorf("tokens[1].LastUsedAt = %v, want nil", tokens[1].LastUsedAt)
	}

	created, err := c.CreateToken(ctx, "reporting")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.ID != "t3" || created.Label != "reporting" || created.Token != "tok_raw_value_shown_once" {
		t.Errorf("CreateToken = %+v", created)
	}

	if err := c.RevokeToken(ctx, "t1"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if revoked != "t1" {
		t.Errorf("revoked id = %q, want t1", revoked)
	}
}

func TestNoAuthorizationHeaderWhenTokenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("Authorization header sent for empty token")
		}
		io.WriteString(w, `{"version":"0.1.0","model":"m","engine":"postgresql","ready":true}`)
	}))
	defer srv.Close()

	if _, err := aichat.New(srv.URL, "").Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
}
