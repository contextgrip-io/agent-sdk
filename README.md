# ContextGrip AI Chat

Ask your PostgreSQL database questions in plain English — self-hosted, on your
own compute, with your own Anthropic API key. Every answer shows the SQL it
ran, and generated SQL executes inside a `READ ONLY` transaction with a
statement timeout: it cannot modify your data.

This repo is **API-first**: the server is a headless HTTP API
([`openapi.yaml`](openapi.yaml) is the source of truth), the bundled web chat
UI is just one client of it, and official client libraries for
[Python](clients/python), [TypeScript](clients/typescript), and
[Go](clients/go) live alongside the server so they can never drift from the
contract they ship with.

```
┌─────────────────────────── your machine ───────────────────────────┐
│  ai-chat container                                                  │
│  ┌────────────┐   /api/v1/*  ┌──────────────────────────────┐      │
│  │ web UI     │─────────────▶│ Go API server                │      │
│  │ (embedded) │              │  bearer-token auth           │      │
│  └────────────┘              │  NL→SQL loop (Anthropic)     │◀── clients/{python,ts,go}
│                              │  READ ONLY tx + stmt timeout │      │
│                              │  SQLite conversation store   │      │
│                              └───────┬──────────────────────┘      │
│                                      │ DATABASE_URL (read-only role)│
│                              ┌───────▼───────┐                     │
│                              │  PostgreSQL   │                     │
│                              └───────────────┘                     │
└─────────────────────────────────────────────────────────────────────┘
        only egress: api.anthropic.com, authenticated with YOUR key
```

## Quickstart

```bash
git clone https://github.com/contextgrip-io/agent-sdk
cd agent-sdk && cp .env.example .env
# edit .env: DATABASE_URL, ANTHROPIC_API_KEY, APP_ACCESS_TOKEN
docker compose up -d
# open http://localhost:8080 and sign in with your APP_ACCESS_TOKEN
```

Or without cloning, once an image is published:

```bash
docker run -d --env-file .env -p 8080:8080 -v ai-chat-data:/data \
  ghcr.io/contextgrip-io/agent-sdk
```

Or (planned) one-click from the ContextGrip console: **Servers → Add Service →
AI Chat** — the deploy plane clones this repo onto your worker, builds the
Dockerfile, and manages the env for you.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — (required) | PostgreSQL connection string. **Use a read-only role** — that role is the hard security boundary. |
| `ANTHROPIC_API_KEY` | — (required) | Your Anthropic key. Used only from this process; the only egress. |
| `APP_ACCESS_TOKEN` | — (required) | Primary bearer token for the UI and API, and the only token that can manage named tokens. The server refuses to start without it unless `AI_CHAT_DEV_NO_AUTH=1`. |
| `PORT` | `8080` | Listen port (deploy platforms inject this). |
| `AI_CHAT_MODEL` | `claude-opus-4-8` | Anthropic model id. |
| `AI_CHAT_DB_PATH` | `./data/ai-chat.sqlite` | Conversation store (SQLite, WAL). Mount a volume at `/data` in Docker. |
| `AI_CHAT_ANTHROPIC_BASE_URL` | — | Override the Anthropic endpoint (testing/stubs). `ANTHROPIC_BASE_URL` also honored. |
| `AI_CHAT_DEV_NO_AUTH` | — | Set to `1` to run without auth **for local development only**. |

## API

Full contract in [`openapi.yaml`](openapi.yaml). Conventions: `/api/v1/*`
resource routes, `Authorization: Bearer <token>`, errors are
`{"error": "...", "code": "..."}`, plus unauthenticated `/healthz`, `/readyz`,
and `/metrics`.

One-shot question (JSON in, JSON out):

```bash
curl -s http://localhost:8080/api/v1/ask \
  -H "Authorization: Bearer $APP_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"question": "Which customers churned in June, and what were they paying?"}'
```

Streaming (SSE — `meta → sql → result → delta* → done|error`):

```bash
curl -sN http://localhost:8080/api/v1/messages \
  -H "Authorization: Bearer $APP_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"question": "How many orders per day this week?"}'
```

Named API tokens (revocable per integration; raw value shown once, stored as
SHA-256):

```bash
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $APP_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -d '{"label": "reporting-cron"}'
```

## Clients

| Language | Package | Install |
|---|---|---|
| Python | `contextgrip-ai-chat` | `pip install "contextgrip-ai-chat @ git+https://github.com/contextgrip-io/agent-sdk#subdirectory=clients/python"` |
| TypeScript | `@contextgrip/ai-chat-client` | see [clients/typescript](clients/typescript) |
| Go | `github.com/contextgrip-io/agent-sdk/clients/go` | `go get` |

```python
from contextgrip_ai_chat import Client

client = Client(base_url="http://localhost:8080", token=TOKEN)
answer = client.ask("Which customers churned in June?")
print(answer.sql, answer.answer)
```

```typescript
import { AiChatClient } from '@contextgrip/ai-chat-client';

const client = new AiChatClient({ baseUrl: 'http://localhost:8080', token });
await client.streamMessage({ question: 'How many orders per day this week?' }, {
  onSql: (sql) => console.log(sql),
  onDelta: (text) => process.stdout.write(text),
});
```

```go
client := aichat.New("http://localhost:8080", token)
resp, err := client.Ask(ctx, aichat.AskRequest{Question: "Which customers churned in June?"})
```

## Security model

Layered, with the strongest guarantee at the bottom:

1. **Database role** — run with a read-only role; this is the boundary that
   holds no matter what SQL the model produces. A `READ ONLY` transaction
   alone does not block volatile admin functions (e.g.
   `pg_terminate_backend`), a restricted role does.
2. **READ ONLY transaction + statement timeout** — every generated statement
   executes inside `BEGIN ... READ ONLY` with `SET LOCAL statement_timeout`.
3. **Static verification (best-effort)** — generated SQL is rejected before it
   reaches the client unless it is a single `SELECT`/`WITH` statement; any
   interior semicolon in the raw text is rejected outright, so quoting tricks
   cannot smuggle a second statement.
4. **App auth** — bearer tokens; the primary token from env plus named
   revocable tokens hashed at rest.

**Data egress:** your question, a bounded schema summary (table/column names
and types), and a bounded sample of query-result rows are sent to the
Anthropic API to generate and explain SQL. Nothing else leaves the machine.

## Repository layout

```
openapi.yaml          # the contract — server, UI, and clients build against this
server/               # Go API server (embeds the built UI)
ui/                   # React chat UI (a client of the API like any other)
clients/python/       # contextgrip-ai-chat
clients/typescript/   # @contextgrip/ai-chat-client
clients/go/           # github.com/contextgrip-io/agent-sdk/clients/go
```

## Development

```bash
cd server && go test ./... && go run ./cmd/ai-chat   # API on :8080
cd ui && npm install && npm run dev                  # UI on :5173, proxies /api
```

CI builds the UI, embeds it, runs the Go suites, typechecks the UI and the
TypeScript client, runs the Python client tests, and builds the Docker image.

## The three surfaces — ready-to-run presets

One binary and one image carry all three AI UIs; `AI_CHAT_FEATURES` picks the
preset. All three are on by default.

| Preset | Env | What you get |
|---|---|---|
| **AI Chat** | `AI_CHAT_FEATURES=chat` | Plain NL→SQL Q&A, strictly read-only. |
| **AI Agentic Chat** | `AI_CHAT_FEATURES=chat,agent` | The chat plus agent mode: multi-step tool loops (read queries run automatically); proposed writes become approval cards. |
| **AI Agent Board** | `AI_CHAT_FEATURES=chat,agent,board` (default) | Everything plus the task board: file work for the agent, watch it move Queued → Running → Needs approval → Done, automate via `/api/v1/tasks`. |

Writes never execute from a feature flag alone: agent mode only *proposes*
SQL, and an approval can execute it only when a separate write-capable
connection is configured via `AI_CHAT_WRITE_DATABASE_URL`. Without it the app
is read-only regardless of preset (`writesEnabled: false` in
`/api/v1/status`).

Additional env for these surfaces:

| Variable | Default | Purpose |
|---|---|---|
| `AI_CHAT_FEATURES` | `chat,agent,board` | Enabled surfaces (comma-separated; `chat` is always implied). |
| `AI_CHAT_WRITE_DATABASE_URL` | — | Optional write-capable connection used ONLY to execute approved writes. |
| `AI_CHAT_AGENT_MAX_STEPS` | `8` | Tool-step budget per agent turn/task. |

## Roadmap

- Schedules for board tasks (recurring agent work), aligned with the sibling
  [PostGrip agent SDK protocol](https://github.com/postgrip-io/agent-sdk-protocol).
- Encrypted-at-rest conversation store.

## License

Apache-2.0
