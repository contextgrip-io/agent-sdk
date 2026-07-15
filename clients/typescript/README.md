# @contextgrip/ai-chat-client

TypeScript client for the [ContextGrip AI Chat](../../README.md) API — ask your
PostgreSQL database questions in plain language, with every answer showing the
SQL it ran.

- Zero runtime dependencies: uses the global `fetch` (Node >= 20 and browsers).
- ESM, fully typed. Types mirror [`openapi.yaml`](../../openapi.yaml), the
  contract this package ships with and can never drift from.
- Streaming (`meta -> sql -> result -> delta* -> done|error`) over SSE with
  typed handlers.

## Install

Until the package is published to npm, install it from this repository
subdirectory. npm cannot install a git subdirectory directly, so build a
tarball from a clone:

```bash
git clone https://github.com/contextgrip-io/agent-sdk
cd agent-sdk/clients/typescript
npm install && npm pack        # produces contextgrip-ai-chat-client-0.1.0.tgz

# then, in your project:
npm install /path/to/contextgrip-ai-chat-client-0.1.0.tgz
```

(`npm pack` runs the `prepare` script, so `dist/` is built into the tarball.
Once published: `npm install @contextgrip/ai-chat-client`.)

## One-shot ask

```typescript
import { AiChatClient, AiChatError } from '@contextgrip/ai-chat-client';

const client = new AiChatClient({
  baseUrl: 'http://localhost:8080',
  token: process.env.APP_ACCESS_TOKEN,
});

try {
  const resp = await client.ask({
    question: 'Which customers churned in June, and what were they paying?',
  });
  console.log(resp.sql);      // the generated read-only SQL (always shown)
  console.log(resp.answer);   // natural-language explanation
  if (resp.resultError) {
    // Failed query execution is NOT an HTTP error: the response carries
    // resultError and an answer explaining the failure.
    console.error('query failed:', resp.resultError);
  } else if (resp.result) {
    console.table(resp.result.rowSample);
  }

  // Continue the same conversation:
  const followUp = await client.ask({
    question: 'And how does that compare to May?',
    conversationId: resp.conversationId,
  });
} catch (err) {
  if (err instanceof AiChatError) {
    // Thrown for any non-2xx response, parsed from the {error, code} body.
    console.error(err.status, err.code, err.message); // e.g. 401 UNAUTHORIZED ...
  } else {
    throw err;
  }
}
```

## Streaming

`streamMessage` posts to `/api/v1/messages` and dispatches Server-Sent Events
as they arrive. `onSql` and `onDelta` receive the payload string directly; the
other handlers receive the full event payload.

```typescript
import { AiChatClient } from '@contextgrip/ai-chat-client';

const client = new AiChatClient({ baseUrl: 'http://localhost:8080', token });

const controller = new AbortController(); // optional: cancel mid-stream

await client.streamMessage(
  { question: 'How many orders per day this week?' },
  {
    onMeta: ({ conversationId }) => console.log('conversation', conversationId),
    onSql: (sql) => console.log('sql:', sql),
    onResult: (result) => {
      if ('error' in result) console.error('query failed:', result.error);
      else console.log(`${result.rowCount} rows in ${result.executionTimeMs}ms`);
    },
    onDelta: (text) => process.stdout.write(text),
    onDone: () => console.log('\n[done]'),
    // A terminal error event resolves the promise (it does not throw):
    onError: ({ message }) => console.error('stream error:', message),
  },
  { signal: controller.signal },
);
```

Pre-stream failures (validation, auth, unknown conversation) reject with
`AiChatError` before any handler fires. Aborting via the signal rejects with
an `AbortError`.

## Conversations and tokens

```typescript
const conversations = await client.listConversations();      // newest first
const detail = await client.getConversation(conversations[0].id);
await client.deleteConversation(conversations[0].id);

// Admin (primary APP_ACCESS_TOKEN only):
const created = await client.createToken('reporting-cron');
console.log(created.token); // raw value — shown only in this response
const tokens = await client.listTokens();                     // metadata only
await client.revokeToken(created.id);
```

## Errors

Every non-2xx response throws `AiChatError` with `status` (HTTP status) and,
when the server provided one, `code` — one of `UNAUTHORIZED`,
`ADMIN_REQUIRED`, `VALIDATION`, `NOT_FOUND`, `NOT_CONFIGURED`,
`CONVERSATION_FULL`, `STORE_ERROR`, `MODEL_ERROR`, `STREAM_UNSUPPORTED`.

## Development

```bash
npm install
npm test          # builds, then runs node:test suites against a local stub server
npm run typecheck # tsc --noEmit
npm run build     # emits dist/ with declarations
```

## License

Apache-2.0
