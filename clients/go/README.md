# ContextGrip AI Chat — Go client

Go client for the ContextGrip AI Chat API (self-hosted NL→SQL chat over a
PostgreSQL database). The authoritative contract is
[`openapi.yaml`](../../openapi.yaml) at the repository root. Zero
dependencies outside the Go standard library.

## Install

```sh
go get github.com/contextgrip-io/agent-sdk/clients/go
```

The module lives in a subdirectory of the repository, so release tags for it
are prefixed with the module path: `clients/go/vX.Y.Z`. To pin a specific
version:

```sh
go get github.com/contextgrip-io/agent-sdk/clients/go@clients/go/v0.1.0
```

The package name is `aichat`; since the import path ends in `go`, import it
with an explicit name:

```go
import aichat "github.com/contextgrip-io/agent-sdk/clients/go"
```

## Ask (one-shot)

`Ask` blocks until the full answer is ready. Failed query execution is not an
error: the response carries `ResultError` and an `Answer` explaining the
failure.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	aichat "github.com/contextgrip-io/agent-sdk/clients/go"
)

func main() {
	client := aichat.New("http://localhost:8080", "your-app-access-token")

	resp, err := client.Ask(context.Background(), aichat.AskRequest{
		Question: "How many users signed up last week?",
		// ConversationID: "existing-id",  // omit to start a new conversation
	})
	if err != nil {
		var apiErr *aichat.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "UNAUTHORIZED" {
			log.Fatal("bad token")
		}
		log.Fatal(err)
	}

	fmt.Println("SQL:   ", resp.SQL)
	if resp.ResultError != "" {
		fmt.Println("failed:", resp.ResultError)
	} else if resp.Result != nil {
		fmt.Println("rows:  ", resp.Result.RowCount)
	}
	fmt.Println("answer:", resp.Answer)
}
```

## StreamMessage (SSE)

`StreamMessage` streams the answer over Server-Sent Events and dispatches
each event to the handler you provide; every handler is optional. Event
order is `meta → sql → result → delta… → done`. A terminal `error` event
calls `OnError` and returns `nil` (the stream itself completed); pre-stream
failures return `*APIError`, transport failures return the underlying error,
and cancelling the context returns `ctx.Err()`.

```go
err := client.StreamMessage(ctx, aichat.AskRequest{Question: "Top 5 products by revenue?"}, aichat.StreamHandlers{
	OnMeta: func(m aichat.Meta) {
		fmt.Println("conversation:", m.ConversationID)
	},
	OnSQL: func(sql string) {
		fmt.Println("SQL:", sql)
	},
	OnResult: func(res aichat.StreamResult) {
		if res.Error != "" {
			fmt.Println("query failed:", res.Error)
			return
		}
		fmt.Printf("%d rows in %dms\n", res.RowCount, res.ExecutionTimeMs)
	},
	OnDelta: func(text string) {
		fmt.Print(text) // answer text, streamed
	},
	OnDone: func(d aichat.Done) {
		fmt.Println("\nassistant message:", d.AssistantMessageID)
	},
	OnError: func(msg string) {
		fmt.Println("stream error:", msg)
	},
})
if err != nil {
	log.Fatal(err)
}
```

## Training data

Every completed exchange can be captured as a training record (question as
intent, generated SQL, bounded result summary), and answers can be rated
explicitly — explicit evals are kept regardless of the capture toggle.

```go
// Rate an assistant answer ("good" or "bad"); upserts by message id.
err = client.RateMessage(ctx, assistantMessageID, aichat.VerdictGood)

// Automatic capture toggle (Set is admin-only):
enabled, err := client.TrainingCapture(ctx)           // GET /api/v1/training/capture
enabled, err = client.SetTrainingCapture(ctx, false)  // PUT /api/v1/training/capture

stats, err := client.TrainingStats(ctx)               // GET /api/v1/training/stats
n, err := client.DeleteTrainingRecords(ctx)           // DELETE /api/v1/training/records (admin, deletes ALL)
```

### ExportTraining (JSONL stream)

`ExportTraining` streams the training dump (`application/x-ndjson`), decoding
one `TrainingExportLine` per line and calling your callback for each — the
whole dump is never buffered in memory. Returning an error from the callback
stops the stream and returns that error unchanged. The line format matches
ContextGrip's training export, so dumps merge downstream without
transformation.

```go
out := json.NewEncoder(file) // e.g. re-emit selected records
err := client.ExportTraining(ctx, aichat.ExportOptions{
	EvaluatedOnly: true, // only records with an eval verdict
	// IncludeRows: nil leaves the server default (true); point at a bool
	// to force: IncludeRows: &noRows
}, func(line aichat.TrainingExportLine) error {
	if line.Eval != nil && line.Eval.Verdict == aichat.VerdictBad {
		return nil // skip bad examples
	}
	return out.Encode(line) // returning an error stops the stream
})
if err != nil {
	log.Fatal(err)
}
```

The server stops the stream at a 64 MiB byte budget; compare the number of
callback calls with `TrainingStats` to detect truncation.

## Other endpoints

```go
status, err := client.Status(ctx)                  // GET  /api/v1/status
convos, err := client.ListConversations(ctx)       // GET  /api/v1/conversations
detail, err := client.GetConversation(ctx, id)     // GET  /api/v1/conversations/{id}
err = client.DeleteConversation(ctx, id)           // DELETE /api/v1/conversations/{id}

// Token management (primary APP_ACCESS_TOKEN only):
tokens, err := client.ListTokens(ctx)              // GET  /api/v1/tokens
created, err := client.CreateToken(ctx, "ci")      // POST /api/v1/tokens — created.Token shown once
err = client.RevokeToken(ctx, created.ID)          // DELETE /api/v1/tokens/{id}
```

Admin-only calls made with a named token fail with `*APIError` code
`ADMIN_REQUIRED` (HTTP 403).

To use a custom HTTP client (timeouts, proxies, transports):

```go
client := aichat.New(baseURL, token, aichat.WithHTTPClient(&http.Client{
	Timeout: 30 * time.Second, // note: also bounds long-lived SSE streams
}))
```
