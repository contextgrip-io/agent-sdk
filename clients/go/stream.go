package aichat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Meta is the payload of the SSE "meta" event, sent first on every stream.
type Meta struct {
	ConversationID string `json:"conversationId"`
	UserMessageID  string `json:"userMessageId"`
}

// Done is the payload of the terminal SSE "done" event.
type Done struct {
	ConversationID     string `json:"conversationId"`
	AssistantMessageID string `json:"assistantMessageId"`
}

// StreamResult is the payload of the SSE "result" event: a ResultSummary on
// success, or Error set (alongside ExecutionTimeMs) when query execution
// failed. Execution failure does not terminate the stream — the answer
// deltas explain the failure.
type StreamResult struct {
	Columns         []string `json:"columns,omitempty"`
	RowSample       [][]any  `json:"rowSample,omitempty"`
	RowCount        int      `json:"rowCount"`
	Truncated       bool     `json:"truncated"`
	ExecutionTimeMs int      `json:"executionTimeMs"`
	Error           string   `json:"error,omitempty"`
}

// StreamHandlers receives SSE events from StreamMessage. Every field is
// optional; nil handlers are skipped. Handlers are called sequentially from
// the goroutine that called StreamMessage, in server event order:
// meta -> sql -> result -> delta* -> done, or a terminal error event.
type StreamHandlers struct {
	OnMeta   func(Meta)
	OnSQL    func(string)
	OnResult func(StreamResult)
	OnDelta  func(string)
	OnDone   func(Done)
	OnError  func(string)
}

// StreamMessage asks a question via POST /api/v1/messages and dispatches the
// Server-Sent Events stream to handlers.
//
// A terminal SSE "error" event calls OnError and returns nil — the stream
// itself completed. Pre-stream failures (validation, auth, unknown
// conversation) return *APIError; transport failures return the underlying
// error; context cancellation returns ctx.Err().
func (c *Client) StreamMessage(ctx context.Context, req AskRequest, handlers StreamHandlers) error {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/api/v1/messages", req)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(httpReq)
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
	return dispatchSSE(ctx, bufio.NewReader(resp.Body), handlers)
}

// dispatchSSE reads SSE frames until a terminal done or error event.
// Malformed frames (unknown event names, undecodable JSON) are skipped.
func dispatchSSE(ctx context.Context, r *bufio.Reader, h StreamHandlers) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := readSSEEvent(r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return errors.New("aichat: stream ended without a terminal done or error event")
			}
			return err
		}
		data := []byte(ev.data)
		switch ev.name {
		case "meta":
			var m Meta
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if h.OnMeta != nil {
				h.OnMeta(m)
			}
		case "sql":
			var p struct {
				SQL string `json:"sql"`
			}
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			if h.OnSQL != nil {
				h.OnSQL(p.SQL)
			}
		case "result":
			var res StreamResult
			if json.Unmarshal(data, &res) != nil {
				continue
			}
			if h.OnResult != nil {
				h.OnResult(res)
			}
		case "delta":
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			if h.OnDelta != nil {
				h.OnDelta(p.Text)
			}
		case "done":
			var d Done
			if json.Unmarshal(data, &d) != nil {
				continue
			}
			if h.OnDone != nil {
				h.OnDone(d)
			}
			return nil
		case "error":
			var p struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			if h.OnError != nil {
				h.OnError(p.Message)
			}
			return nil
		default:
			// Unknown event name: skip the frame.
		}
	}
}

// sseEvent is one parsed Server-Sent Events frame.
type sseEvent struct {
	name string
	data string
}

// readSSEEvent reads the next complete SSE frame from r, blocking until a
// terminating blank line arrives, so frames split across reads are handled
// naturally. Per the SSE spec: fields are split on the first ":", one
// leading space of the value is stripped, comment lines (leading ":") are
// ignored, multiple data lines are joined with "\n", and frames whose data
// buffer is empty are not dispatched. An incomplete trailing frame at EOF is
// discarded and io.EOF returned.
func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var dataLines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			// End of frame. Dispatch only when data was seen.
			if len(dataLines) == 0 {
				ev.name = ""
				continue
			}
			ev.data = strings.Join(dataLines, "\n")
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment (e.g. keep-alive)
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.name = value
		case "data":
			dataLines = append(dataLines, value)
		default:
			// id, retry, and unknown fields are ignored.
		}
	}
}
