package aichat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	aichat "github.com/contextgrip-io/agent-sdk/clients/go"
)

func TestStreamMessageAgentMode(t *testing.T) {
	stream := "event: meta\n" +
		"data: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n" +
		"\n" +
		"event: step\n" +
		"data: {\"index\":0,\"kind\":\"query\",\"summary\":\"count users\",\"sql\":\"SELECT count(*) FROM users\",\n" +
		"data:  \"result\":{\"columns\":[\"count\"],\"rowSample\":[[42]],\"rowCount\":1,\"truncated\":false,\"executionTimeMs\":8}}\n" +
		"\n" +
		"event: step\n" +
		"data: {\"index\":1,\"kind\":\"note\",\"summary\":\"users table has a status column\"}\n" +
		"\n" +
		"event: approval_required\n" +
		"data: {\"id\":\"ap1\",\"sql\":\"UPDATE users SET status = 'active' WHERE id = 7\",\n" +
		"data:  \"rationale\":\"reactivate the user as asked\",\"status\":\"pending\",\n" +
		"data:  \"source\":{\"conversationId\":\"c1\",\"messageId\":\"a1\"},\"createdAt\":\"2026-07-15T12:00:00Z\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"conversationId\":\"c1\",\"assistantMessageId\":\"a1\",\"pendingApprovalId\":\"ap1\"}\n" +
		"\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req aichat.AskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Mode != "agent" {
			t.Errorf("mode = %q, want agent", req.Mode)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < len(stream); i += 7 {
			end := min(i+7, len(stream))
			io.WriteString(w, stream[i:end])
			flusher.Flush()
		}
	}))
	defer srv.Close()

	var calls []string
	err := aichat.New(srv.URL, "tok").StreamMessage(context.Background(), aichat.AskRequest{
		Question: "reactivate user 7",
		Mode:     aichat.ModeAgent,
	}, aichat.StreamHandlers{
		OnMeta: func(m aichat.Meta) {
			calls = append(calls, "meta:"+m.ConversationID)
		},
		OnSQL: func(sql string) {
			calls = append(calls, "sql:"+sql)
		},
		OnStep: func(s aichat.Step) {
			rows := -1
			if s.Result != nil {
				rows = s.Result.RowCount
			}
			calls = append(calls, fmt.Sprintf("step:%d/%s/%s/sql=%q/rows=%d/err=%q",
				s.Index, s.Kind, s.Summary, s.SQL, rows, s.Error))
		},
		OnApprovalRequired: func(a aichat.Approval) {
			calls = append(calls, fmt.Sprintf("approval:%s/%s/%s/src=%s+%s/%s",
				a.ID, a.Status, a.SQL, a.Source.ConversationID, a.Source.MessageID, a.Rationale))
		},
		OnDelta: func(text string) {
			calls = append(calls, "delta:"+text)
		},
		OnDone: func(d aichat.Done) {
			calls = append(calls, fmt.Sprintf("done:%s/%s/pending=%s", d.ConversationID, d.AssistantMessageID, d.PendingApprovalID))
		},
	})
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	want := []string{
		"meta:c1",
		`step:0/query/count users/sql="SELECT count(*) FROM users"/rows=1/err=""`,
		`step:1/note/users table has a status column/sql=""/rows=-1/err=""`,
		"approval:ap1/pending/UPDATE users SET status = 'active' WHERE id = 7/src=c1+a1/reactivate the user as asked",
		"done:c1/a1/pending=ap1",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("handler calls:\n got  %q\n want %q", calls, want)
	}
}

func TestAskAgentMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req aichat.AskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Mode != "agent" {
			t.Errorf("request = %+v, err = %v, want mode agent", req, err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"conversationId": "c1",
			"userMessageId": "u1",
			"assistantMessageId": "a1",
			"answer": "I need approval to run this write.",
			"steps": [
				{"index":0,"kind":"schema","summary":"inspected users table"},
				{"index":1,"kind":"query","summary":"looked up user 7","sql":"SELECT * FROM users WHERE id = 7",
				 "result":{"rowCount":1,"truncated":false,"executionTimeMs":4}}
			],
			"pendingApprovalId": "ap9"
		}`)
	}))
	defer srv.Close()

	got, err := aichat.New(srv.URL, "tok").Ask(context.Background(), aichat.AskRequest{
		Question: "reactivate user 7",
		Mode:     aichat.ModeAgent,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.SQL != "" {
		t.Errorf("SQL = %q, want empty in agent mode", got.SQL)
	}
	if len(got.Steps) != 2 || got.Steps[0].Kind != "schema" || got.Steps[1].SQL != "SELECT * FROM users WHERE id = 7" {
		t.Errorf("Steps = %+v", got.Steps)
	}
	if got.PendingApprovalID != "ap9" {
		t.Errorf("PendingApprovalID = %q, want ap9", got.PendingApprovalID)
	}
}

func TestApprovals(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"id":"ap1","sql":"UPDATE users SET status = 'active' WHERE id = 7","rationale":"reactivate",
			 "status":"pending","source":{"conversationId":"c1","messageId":"a1"},"createdAt":"2026-07-15T12:00:00Z"},
			{"id":"ap2","sql":"DELETE FROM sessions WHERE expired","status":"pending",
			 "source":{"taskId":"t1"},"createdAt":"2026-07-15T11:00:00Z"}
		]`)
	})
	mux.HandleFunc("POST /api/v1/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		hits++
		var body struct {
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Decision != "approve" {
			t.Errorf("body = %+v, err = %v, want decision approve", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.PathValue("id") {
		case "ap1":
			io.WriteString(w, `{
				"approval": {"id":"ap1","sql":"UPDATE users SET status = 'active' WHERE id = 7","status":"approved",
				             "source":{"conversationId":"c1","messageId":"a1"},
				             "createdAt":"2026-07-15T12:00:00Z","decidedAt":"2026-07-15T12:05:00Z"},
				"result": {"rowCount":1,"truncated":false,"executionTimeMs":6}
			}`)
		case "decided":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"error":"approval already decided","code":"ALREADY_DECIDED"}`)
		case "nowrites":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"error":"no write connection configured","code":"WRITES_DISABLED"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"approval not found","code":"NOT_FOUND"}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := aichat.New(srv.URL, "tok")
	ctx := context.Background()

	list, err := c.ListApprovals(ctx)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListApprovals returned %d approvals", len(list))
	}
	if list[0].Source.ConversationID != "c1" || list[0].Source.TaskID != "" || list[0].Rationale != "reactivate" {
		t.Errorf("approvals[0] = %+v", list[0])
	}
	if list[1].Source.TaskID != "t1" || list[1].Source.ConversationID != "" || list[1].DecidedAt != nil {
		t.Errorf("approvals[1] = %+v", list[1])
	}

	// Invalid decisions and empty ids are rejected client-side.
	if _, err := c.DecideApproval(ctx, "ap1", "maybe"); err == nil {
		t.Error("DecideApproval accepted decision \"maybe\"")
	}
	if _, err := c.DecideApproval(ctx, "", "approve"); err == nil {
		t.Error("DecideApproval accepted empty id")
	}
	if hits != 0 {
		t.Fatalf("server hit %d times by client-side-rejected calls", hits)
	}

	res, err := c.DecideApproval(ctx, "ap1", aichat.DecisionApprove)
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if res.Approval.Status != "approved" || res.Approval.DecidedAt == nil ||
		!res.Approval.DecidedAt.Equal(mustTime(t, "2026-07-15T12:05:00Z")) {
		t.Errorf("approval = %+v", res.Approval)
	}
	if res.Result == nil || res.Result.RowCount != 1 || res.Error != "" {
		t.Errorf("result = %+v, error = %q", res.Result, res.Error)
	}

	var apiErr *aichat.APIError
	if _, err := c.DecideApproval(ctx, "decided", aichat.DecisionApprove); !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusConflict || apiErr.Code != "ALREADY_DECIDED" {
		t.Errorf("decided err = %v, want 409 ALREADY_DECIDED APIError", err)
	}
	if _, err := c.DecideApproval(ctx, "nowrites", aichat.DecisionApprove); !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusConflict || apiErr.Code != "WRITES_DISABLED" {
		t.Errorf("nowrites err = %v, want 409 WRITES_DISABLED APIError", err)
	}
}

func TestTasks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title  string `json:"title"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			body.Title != "Weekly cleanup" || body.Prompt != "archive stale sessions" {
			t.Errorf("body = %+v, err = %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"t1","title":"Weekly cleanup","prompt":"archive stale sessions",
			"status":"queued","createdAt":"2026-07-15T12:00:00Z","updatedAt":"2026-07-15T12:00:00Z"}`)
	})
	mux.HandleFunc("GET /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RawQuery == "status=needs_approval" {
			io.WriteString(w, `[{"id":"t2","title":"B","prompt":"p2","status":"needs_approval",
				"createdAt":"2026-07-15T10:00:00Z","updatedAt":"2026-07-15T11:00:00Z"}]`)
			return
		}
		if r.URL.RawQuery != "" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		io.WriteString(w, `[
			{"id":"t2","title":"B","prompt":"p2","status":"needs_approval","createdAt":"2026-07-15T10:00:00Z","updatedAt":"2026-07-15T11:00:00Z"},
			{"id":"t1","title":"Weekly cleanup","prompt":"archive stale sessions","status":"done",
			 "answer":"Archived 12 sessions.","createdAt":"2026-07-15T12:00:00Z","updatedAt":"2026-07-15T12:10:00Z"}
		]`)
	})
	mux.HandleFunc("GET /api/v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"task": {"id":"t2","title":"B","prompt":"p2","status":"needs_approval",
			         "createdAt":"2026-07-15T10:00:00Z","updatedAt":"2026-07-15T11:00:00Z"},
			"steps": [{"index":0,"kind":"query","summary":"found stale sessions","sql":"SELECT id FROM sessions WHERE expired",
			           "result":{"rowCount":12,"truncated":false,"executionTimeMs":5}}],
			"pendingApproval": {"id":"ap2","sql":"DELETE FROM sessions WHERE expired","status":"pending",
			                    "source":{"taskId":"t2"},"createdAt":"2026-07-15T11:00:00Z"}
		}`)
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") == "t-finished" {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"error":"task already finished","code":"TASK_FINISHED"}`)
			return
		}
		io.WriteString(w, `{"id":"t2","title":"B","prompt":"p2","status":"canceled",
			"createdAt":"2026-07-15T10:00:00Z","updatedAt":"2026-07-15T11:30:00Z"}`)
	})
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") == "t-active" {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"error":"task is still active","code":"TASK_ACTIVE"}`)
			return
		}
		io.WriteString(w, `{"deleted":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := aichat.New(srv.URL, "tok")
	ctx := context.Background()

	created, err := c.CreateTask(ctx, "Weekly cleanup", "archive stale sessions")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID != "t1" || created.Status != aichat.TaskStatusQueued {
		t.Errorf("CreateTask = %+v", created)
	}

	all, err := c.ListTasks(ctx, "")
	if err != nil {
		t.Fatalf("ListTasks(all): %v", err)
	}
	if len(all) != 2 || all[1].Answer != "Archived 12 sessions." {
		t.Errorf("ListTasks(all) = %+v", all)
	}

	filtered, err := c.ListTasks(ctx, aichat.TaskStatusNeedsApproval)
	if err != nil {
		t.Fatalf("ListTasks(needs_approval): %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "t2" {
		t.Errorf("ListTasks(needs_approval) = %+v", filtered)
	}

	detail, err := c.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Task.Status != aichat.TaskStatusNeedsApproval || len(detail.Steps) != 1 {
		t.Errorf("GetTask = %+v", detail)
	}
	if detail.Steps[0].Result == nil || detail.Steps[0].Result.RowCount != 12 {
		t.Errorf("steps[0] = %+v", detail.Steps[0])
	}
	if detail.PendingApproval == nil || detail.PendingApproval.ID != "ap2" ||
		detail.PendingApproval.Source.TaskID != "t2" {
		t.Errorf("pendingApproval = %+v", detail.PendingApproval)
	}

	canceled, err := c.CancelTask(ctx, "t2")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if canceled.Status != aichat.TaskStatusCanceled {
		t.Errorf("CancelTask status = %q, want canceled", canceled.Status)
	}

	var apiErr *aichat.APIError
	if _, err := c.CancelTask(ctx, "t-finished"); !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusConflict || apiErr.Code != "TASK_FINISHED" {
		t.Errorf("CancelTask(t-finished) err = %v, want 409 TASK_FINISHED APIError", err)
	}

	if err := c.DeleteTask(ctx, "t2"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if err := c.DeleteTask(ctx, "t-active"); !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusConflict || apiErr.Code != "TASK_ACTIVE" {
		t.Errorf("DeleteTask(t-active) err = %v, want 409 TASK_ACTIVE APIError", err)
	}
}
