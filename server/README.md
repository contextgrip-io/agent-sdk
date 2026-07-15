# server/ — ContextGrip AI Chat API server

Go implementation of the contract in [`../openapi.yaml`](../openapi.yaml):
POST a question → generate ONE read-only SQL statement via Anthropic → verify
it statically → execute in a `READ ONLY` transaction with a statement timeout
→ stream (SSE) or return (JSON) a natural-language explanation. Single
PostgreSQL database per deployment; conversations are instance-global.

Three surfaces, toggled by `AI_CHAT_FEATURES`:

- **chat** (always on) — the one-question/one-query loop above.
- **agent** — `AskRequest.mode: "agent"`: the model takes multiple tool
  steps (`run_query` read-only investigation runs automatically and streams
  `step` events; `propose_write` becomes a human **approval** and ends the
  turn with `done.pendingApprovalId`).
- **board** — background tasks (`/api/v1/tasks`) run through the same agent
  loop by a single worker goroutine, oldest first; a proposed write pauses
  the task in `needs_approval` and the approval decision resumes it.

Requests to a disabled surface get `403 FEATURE_DISABLED`.

## Layout

```
cmd/ai-chat/          entrypoint: env parsing, wiring, task runner start,
                      graceful shutdown
internal/api/         router (chi), handlers, auth middleware, SSE, agent
                      loop orchestrator, board task runner, /metrics
internal/assistant/   model boundary (Client interface + Anthropic impl:
                      chat calls with adaptive thinking, agent tool turns
                      with a serializable transcript; 2m/5m call timeouts)
internal/dbx/         lazy pgxpool, static verifiers (read-only + single
                      statement), READ ONLY execution, write execution,
                      schema introspection + bounded context
internal/chatstore/   SQLite conversation store (WAL, busy_timeout)
internal/tokenstore/  SQLite named-token store (SHA-256 at rest); may share
                      the chatstore's DB file
internal/trainingstore/ SQLite training-record store (capture toggle +
                      records; same shared DB file)
internal/approvalstore/ SQLite pending-write approvals (same shared file)
internal/taskstore/   SQLite board tasks with serialized agent transcript
                      (same shared file)
internal/textutil/    UTF-8 truncation + result-cell bounding helpers
internal/webui/       go:embed of dist/ with SPA fallback (placeholder page
                      committed; Docker copies the real UI build in)
```

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — | PostgreSQL to answer questions about. **Use a read-only role** — it is the hard security boundary. Missing → chat endpoints return `503 NOT_CONFIGURED`. |
| `ANTHROPIC_API_KEY` | — | Anthropic key; the only egress. Missing → `503 NOT_CONFIGURED`. |
| `APP_ACCESS_TOKEN` | — | Primary bearer token, and the only token that can manage named tokens. Required — the server refuses to start without it unless `AI_CHAT_DEV_NO_AUTH=1`. |
| `PORT` | `8080` | Listen port. |
| `AI_CHAT_MODEL` | `claude-opus-4-8` | Anthropic model id. |
| `AI_CHAT_DB_PATH` | `./data/ai-chat.sqlite` | SQLite file for conversations + tokens + training records + approvals + tasks. |
| `AI_CHAT_ANTHROPIC_BASE_URL` | — | Anthropic endpoint override (`ANTHROPIC_BASE_URL` also honored). |
| `AI_CHAT_FEATURES` | `chat,agent,board` | Enabled surfaces (comma list; `chat` always implied). Disabled surfaces answer `403 FEATURE_DISABLED`. |
| `AI_CHAT_WRITE_DATABASE_URL` | — | Separate connection (own pool, a role WITH write grants) used ONLY to execute human-approved writes. Unset → approvals can only be rejected (`409 WRITES_DISABLED` on approve). |
| `AI_CHAT_AGENT_MAX_STEPS` | `8` | Max tool steps per agent run before the run fails. |
| `AI_CHAT_DEV_NO_AUTH` | — | `1` disables auth. Local development only. |

## Run / test

```bash
go run ./cmd/ai-chat        # API on :8080; UI placeholder at /
go test ./...               # unit + handler tests (fake model, temp SQLite, no network)
go vet ./... && gofmt -l ./cmd ./internal
```

Health endpoints (`/healthz`, `/readyz`), `/metrics` (hand-rolled Prometheus
text), and the static UI are unauthenticated; everything under `/api/v1`
requires `Authorization: Bearer <token>`.
`GET /api/v1/status` reports `features` and `writesEnabled`.

## Agent mode, approvals, and the board

Agent mode gives the model two tools: `run_query` (one read-only statement,
verified and executed automatically; surfaced as a `step` SSE event and in
`Message.steps`) and `propose_write` (one mutating statement + rationale; it
becomes a **pending approval** and ends the turn). Nothing the model proposes
ever executes without a human decision:

- `GET /api/v1/approvals` — pending proposals (chat and board sources).
- `POST /api/v1/approvals/{id}` `{"decision": "approve"|"reject"}` —
  approving runs the EXACT stored SQL on the write connection in a single
  transaction (single-statement gate, statement timeout) and reports
  rows-affected; the outcome is appended to the source conversation or
  resumes the source task. Re-deciding → `409 ALREADY_DECIDED`; approving
  without a write connection → `409 WRITES_DISABLED`.

Board tasks (`POST /api/v1/tasks` `{title, prompt}`) run in the background,
one at a time, oldest first. Statuses: `queued → running →
(needs_approval ⇢ queued) → done | failed | canceled`. Cancel
(`POST /api/v1/tasks/{id}/cancel`) flips queued tasks immediately, stops
running tasks cooperatively between steps, and auto-rejects the pending
approval of an approval-blocked task; only finished tasks can be deleted
(`409 TASK_ACTIVE` otherwise). Tasks stuck in `running` after a crash are
requeued at startup.

## Training data

Completed exchanges are captured as training records **by default**
(`GET/PUT /api/v1/training/capture` reads/toggles the setting; the PUT is
admin-only). Each record holds the question as intent, the generated SQL, and
the bounded result summary or execution error, keyed by the assistant message
id. Agent-mode runs capture each successfully executed `run_query` step
(session `agent` for chat, `task` for board tasks, keyed
`<messageId|taskId>:<stepIndex>`). Answers can be rated explicitly with
`POST /api/v1/messages/{id}/eval {"verdict": "good"|"bad"}` — evals bypass
the capture toggle and upsert by message id (re-rating updates the verdict).

`GET /api/v1/training/export?includeRows&evaluatedOnly` streams the records
as JSONL (`application/x-ndjson`), oldest first, stopping at a 64 MiB budget
(compare line count with `GET /api/v1/training/stats` to detect truncation).
The line format matches ContextGrip's training export so dumps merge
downstream without transformation; connection identity is a non-secret hash
of `host:port/dbname` plus the database name — never credentials.
`DELETE /api/v1/training/records` (admin-only) wipes all records and returns
the count.

## Security model (layered)

1. **Read-only DB role** (operator-provided) — holds no matter what SQL the
   model produces.
2. **`READ ONLY` transaction + `SET LOCAL statement_timeout`** (30s) on every
   generated statement (`internal/dbx/readonly.go`).
3. **Static verifier** (best-effort): after trimming one trailing semicolon,
   any remaining semicolon in the RAW text is rejected regardless of quoting
   (this defeats quote-scanner desync tricks like `E'\''`-literals, at the
   documented cost of over-rejecting legitimate quoted semicolons such as
   `SELECT $$a;b$$`); the comment-stripped text must start with
   `SELECT`/`WITH`.
4. **Human-gated writes**: model-proposed mutations never execute directly —
   they become approvals, run only on explicit approve, only against the
   dedicated `AI_CHAT_WRITE_DATABASE_URL` connection, and only as a single
   statement (raw interior-semicolon gate, no read-only requirement).
5. **Bearer auth**: primary token compared via SHA-256 +
   constant-time compare; named tokens stored hashed, revocable, and unable
   to manage tokens (`403 ADMIN_REQUIRED`).

Bounds: question ≤ 4000 chars, schema context ≤ 10k chars, 6 history turns,
100-row query cap, 20-row/256-chars-per-cell result sample (bounded before it
reaches the model, the stream, or the store), 500 conversations (oldest
pruned), 200 messages per conversation (`CONVERSATION_FULL`).

## Notes

- SSE frames are `event: <name>\ndata: <single-line JSON>\n\n`, flushed per
  event, with `X-Accel-Buffering: no`; event payloads are documented in
  `openapi.yaml` under `/api/v1/messages`.
- After SSE headers are sent, assistant records (including partial answers on
  stream failure) are persisted with `context.WithoutCancel`, so a client
  disconnect cannot lose them.
- The SQLite stores set `journal_mode=WAL` and `busy_timeout=5000` on every
  pooled connection via `_pragma` DSN options.
