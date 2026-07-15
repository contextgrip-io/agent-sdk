# ContextGrip AI Chat web UI

Single-page chat client for the AI Chat API. It is a pure API client of the Go
server in [`../server`](../server) — no server-side code lives here. The
contract it is built against is [`../openapi.yaml`](../openapi.yaml).

## How it reaches the API

All requests use **relative URLs** (`/api/v1/...`), so the UI works wherever it
is served from:

- **Production** — `npm run build` outputs `dist/`, which the repo-root
  `Dockerfile` copies into the Go server and embeds into the binary; the UI is
  then served at `/` and calls the API same-origin.
- **Development** — the Vite dev server proxies `/api`, `/healthz`, and
  `/readyz` to `http://localhost:8080` (see `vite.config.ts`), so run the Go
  server alongside it.

Authentication is a bearer token (the server's `APP_ACCESS_TOKEN` or a named
token minted via the API). The sign-in screen validates it against
`GET /api/v1/status` and stores it in `localStorage` under `ai_chat_token`;
"Sign out" clears it. Answers stream over SSE from `POST /api/v1/messages`
(`meta → sql → result → delta* → done | error`), parsed by the small
spec-correct parser in `src/lib/sse.ts`.

Training data: completed answers that carry SQL show 👍/👎 buttons
(`POST /api/v1/messages/{id}/eval`), and the header's "Training data" button
opens an inline panel over `/api/v1/training/*` — capture toggle, record
stats, a JSONL export download (`training-export.jsonl`, optionally rated
records only), and an admin-only delete-all. Admin-only calls made with a
named token surface the server's 403 as an inline note.

## Develop

```bash
# terminal 1: the API server
cd ../server && go run ./cmd/ai-chat        # :8080

# terminal 2: the UI
npm install
npm run dev                                 # :5173, proxies /api to :8080
```

## Build

```bash
npm run build     # typechecks, then emits dist/ (index.html + hashed assets)
```

## Test

```bash
npx tsc --noEmit  # typecheck
npm test          # vitest — covers the SSE parser (src/lib/sse.test.ts)
```

## Layout

```
index.html            entry document
vite.config.ts        dev proxy + build output (dist/)
src/main.tsx          React root
src/App.tsx           token gate, conversation state, streaming orchestration
src/components/       SignIn, MessageList (incl. rating), SqlBlock, ResultTable,
                      Composer, TrainingPanel
src/lib/types.ts      API types mirrored from openapi.yaml + UI message model
src/lib/api.ts        fetch client (status/conversations/eval/training + SSE streaming)
src/lib/sse.ts        incremental SSE frame parser (tested)
src/styles.css        the one stylesheet (light/dark via prefers-color-scheme)
```
