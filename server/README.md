# server/ — ContextGrip AI Chat API server

Go implementation of the contract in [`../openapi.yaml`](../openapi.yaml):
POST a question → generate ONE read-only SQL statement via Anthropic → verify
it statically → execute in a `READ ONLY` transaction with a statement timeout
→ stream (SSE) or return (JSON) a natural-language explanation. Single
PostgreSQL database per deployment; conversations are instance-global.

## Layout

```
cmd/ai-chat/          entrypoint: env parsing, wiring, graceful shutdown
internal/api/         router (chi), handlers, auth middleware, SSE, /metrics
internal/assistant/   model boundary (Client interface + Anthropic impl,
                      adaptive thinking, 2m/5m call timeouts)
internal/dbx/         lazy pgxpool, static read-only verifier, READ ONLY
                      execution, schema introspection + bounded context
internal/chatstore/   SQLite conversation store (WAL, busy_timeout)
internal/tokenstore/  SQLite named-token store (SHA-256 at rest); may share
                      the chatstore's DB file
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
| `AI_CHAT_DB_PATH` | `./data/ai-chat.sqlite` | SQLite file for conversations + tokens. |
| `AI_CHAT_ANTHROPIC_BASE_URL` | — | Anthropic endpoint override (`ANTHROPIC_BASE_URL` also honored). |
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
4. **Bearer auth**: primary token compared via SHA-256 +
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
