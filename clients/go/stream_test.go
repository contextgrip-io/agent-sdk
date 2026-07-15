package aichat_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	aichat "github.com/contextgrip-io/agent-sdk/clients/go"
)

// sseServer serves the given raw SSE stream in small flushed chunks so
// frames arrive split across multiple reads on the client side.
func sseServer(t *testing.T, stream string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < len(stream); i += 7 {
			end := min(i+7, len(stream))
			io.WriteString(w, stream[i:end])
			flusher.Flush()
		}
	}))
}

func TestStreamMessageFullSequence(t *testing.T) {
	// Full event sequence meta -> sql -> result -> delta -> delta -> done,
	// including: a keep-alive comment, an unknown event, a malformed JSON
	// frame (all skipped), and a multi-line data frame whose lines the
	// parser must join with "\n" (the newline lands between JSON tokens).
	stream := "event: meta\n" +
		"data: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n" +
		"\n" +
		": keep-alive\n" +
		"\n" +
		"event: sql\n" +
		"data: {\"sql\":\"SELECT count(*) FROM users\"}\n" +
		"\n" +
		"event: mystery\n" +
		"data: {\"x\":1}\n" +
		"\n" +
		"event: result\n" +
		"data: {\"columns\":[\"count\"],\"rowSample\":[[42]],\"rowCount\":1,\"truncated\":false,\"executionTimeMs\":12}\n" +
		"\n" +
		"event: delta\n" +
		"data: {\"text\":\n" +
		"data:  \"There are\"}\n" +
		"\n" +
		"event: delta\n" +
		"data: {not valid json\n" +
		"\n" +
		"event: delta\n" +
		"data: {\"text\":\" 42 users.\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"conversationId\":\"c1\",\"assistantMessageId\":\"a1\"}\n" +
		"\n"
	srv := sseServer(t, stream)
	defer srv.Close()

	var calls []string
	c := aichat.New(srv.URL, "tok")
	err := c.StreamMessage(context.Background(), aichat.AskRequest{Question: "how many users?"}, aichat.StreamHandlers{
		OnMeta: func(m aichat.Meta) {
			calls = append(calls, fmt.Sprintf("meta:%s/%s", m.ConversationID, m.UserMessageID))
		},
		OnSQL: func(sql string) {
			calls = append(calls, "sql:"+sql)
		},
		OnResult: func(res aichat.StreamResult) {
			calls = append(calls, fmt.Sprintf("result:rows=%d cols=%v sample=%v trunc=%t ms=%d err=%q",
				res.RowCount, res.Columns, res.RowSample, res.Truncated, res.ExecutionTimeMs, res.Error))
		},
		OnDelta: func(text string) {
			calls = append(calls, "delta:"+text)
		},
		OnDone: func(d aichat.Done) {
			calls = append(calls, fmt.Sprintf("done:%s/%s", d.ConversationID, d.AssistantMessageID))
		},
		OnError: func(msg string) {
			calls = append(calls, "error:"+msg)
		},
	})
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	want := []string{
		"meta:c1/u1",
		"sql:SELECT count(*) FROM users",
		"result:rows=1 cols=[count] sample=[[42]] trunc=false ms=12 err=\"\"",
		"delta:There are",
		"delta: 42 users.",
		"done:c1/a1",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("handler calls:\n got  %q\n want %q", calls, want)
	}
}

func TestStreamMessageResultExecutionError(t *testing.T) {
	// A failed query is a "result" event with {error, executionTimeMs} and
	// does not terminate the stream.
	stream := "event: meta\n" +
		"data: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n" +
		"\n" +
		"event: sql\n" +
		"data: {\"sql\":\"SELECT nope\"}\n" +
		"\n" +
		"event: result\n" +
		"data: {\"error\":\"column \\\"nope\\\" does not exist\",\"executionTimeMs\":3}\n" +
		"\n" +
		"event: delta\n" +
		"data: {\"text\":\"That column does not exist.\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"conversationId\":\"c1\",\"assistantMessageId\":\"a1\"}\n" +
		"\n"
	srv := sseServer(t, stream)
	defer srv.Close()

	var result aichat.StreamResult
	var done bool
	err := aichat.New(srv.URL, "tok").StreamMessage(context.Background(), aichat.AskRequest{Question: "q"}, aichat.StreamHandlers{
		OnResult: func(res aichat.StreamResult) { result = res },
		OnDone:   func(aichat.Done) { done = true },
	})
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	if result.Error != `column "nope" does not exist` || result.ExecutionTimeMs != 3 {
		t.Errorf("result = %+v", result)
	}
	if !done {
		t.Error("OnDone not called")
	}
}

func TestStreamMessageTerminalErrorEvent(t *testing.T) {
	stream := "event: meta\n" +
		"data: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n" +
		"\n" +
		"event: error\n" +
		"data: {\"message\":\"model request failed\"}\n" +
		"\n"
	srv := sseServer(t, stream)
	defer srv.Close()

	var errMsg string
	var done bool
	err := aichat.New(srv.URL, "tok").StreamMessage(context.Background(), aichat.AskRequest{Question: "q"}, aichat.StreamHandlers{
		OnError: func(msg string) { errMsg = msg },
		OnDone:  func(aichat.Done) { done = true },
	})
	if err != nil {
		t.Fatalf("StreamMessage returned %v, want nil for terminal error event", err)
	}
	if errMsg != "model request failed" {
		t.Errorf("OnError message = %q, want %q", errMsg, "model request failed")
	}
	if done {
		t.Error("OnDone called after terminal error event")
	}
}

func TestStreamMessagePreStreamAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"missing or invalid bearer token","code":"UNAUTHORIZED"}`)
	}))
	defer srv.Close()

	err := aichat.New(srv.URL, "bad").StreamMessage(context.Background(), aichat.AskRequest{Question: "q"}, aichat.StreamHandlers{
		OnError: func(msg string) { t.Errorf("OnError called for pre-stream failure: %q", msg) },
	})
	var apiErr *aichat.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*APIError) failed for %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "UNAUTHORIZED" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestStreamMessageContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "event: meta\ndata: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n\n")
		flusher.Flush()
		// Hold the stream open until the client gives up.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	err := aichat.New(srv.URL, "tok").StreamMessage(ctx, aichat.AskRequest{Question: "q"}, aichat.StreamHandlers{
		OnMeta: func(aichat.Meta) { cancel() },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StreamMessage returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
}

func TestStreamMessageTruncatedStream(t *testing.T) {
	// EOF before a terminal done/error event is a transport-level failure.
	stream := "event: meta\n" +
		"data: {\"conversationId\":\"c1\",\"userMessageId\":\"u1\"}\n" +
		"\n"
	srv := sseServer(t, stream)
	defer srv.Close()

	err := aichat.New(srv.URL, "tok").StreamMessage(context.Background(), aichat.AskRequest{Question: "q"}, aichat.StreamHandlers{})
	if err == nil {
		t.Fatal("StreamMessage returned nil for a stream with no terminal event")
	}
}
