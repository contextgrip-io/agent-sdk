/**
 * @contextgrip/ai-chat-client — TypeScript client for the ContextGrip AI Chat API.
 *
 * The contract is `openapi.yaml` at the repository root. Zero runtime
 * dependencies: uses the global `fetch` (Node >= 20 and browsers).
 */

import type {
  AskRequest,
  AskResponse,
  Conversation,
  ConversationDetail,
  CreatedToken,
  Status,
  StreamDeltaEvent,
  StreamDoneEvent,
  StreamErrorEvent,
  StreamHandlers,
  StreamMetaEvent,
  StreamResultEvent,
  StreamSqlEvent,
  TokenInfo,
} from './types.js';
import { parseSseFrame } from './sse.js';

export type * from './types.js';

/** Options for {@link AiChatClient}. */
export interface AiChatClientOptions {
  /** Origin of the AI Chat server, e.g. `"http://localhost:8080"`. */
  baseUrl: string;
  /**
   * Bearer token: the primary `APP_ACCESS_TOKEN` or a named token minted
   * via `createToken`. Omit only when the server runs with
   * `AI_CHAT_DEV_NO_AUTH=1`.
   */
  token?: string;
  /** Custom fetch implementation; defaults to the global `fetch`. */
  fetch?: typeof fetch;
}

/**
 * Error thrown for any non-2xx HTTP response, carrying the HTTP status and
 * the server's `{error, code}` body when present.
 */
export class AiChatError extends Error {
  /** HTTP status code of the failed response. */
  readonly status: number;
  /**
   * Stable machine slug from the error body. Known values: UNAUTHORIZED,
   * ADMIN_REQUIRED, VALIDATION, NOT_FOUND, NOT_CONFIGURED,
   * CONVERSATION_FULL, STORE_ERROR, MODEL_ERROR, STREAM_UNSUPPORTED.
   */
  readonly code?: string;

  constructor(message: string, status: number, code?: string) {
    super(message);
    this.name = 'AiChatError';
    this.status = status;
    this.code = code;
  }
}

async function errorFromResponse(res: Response): Promise<AiChatError> {
  let message = `request failed with status ${res.status}`;
  let code: string | undefined;
  let text = '';
  try {
    text = await res.text();
  } catch {
    // Body unavailable; fall back to the status message.
  }
  if (text !== '') {
    try {
      const parsed: unknown = JSON.parse(text);
      if (parsed !== null && typeof parsed === 'object') {
        const body = parsed as { error?: unknown; code?: unknown };
        if (typeof body.error === 'string' && body.error !== '') {
          message = body.error;
        }
        if (typeof body.code === 'string') {
          code = body.code;
        }
      }
    } catch {
      // Non-JSON error body (e.g. a proxy page): use a bounded excerpt.
      message = text.length > 300 ? `${text.slice(0, 300)}...` : text;
    }
  }
  return new AiChatError(message, res.status, code);
}

function dispatchFrame(frameText: string, handlers: StreamHandlers): void {
  const frame = parseSseFrame(frameText);
  if (frame === null) return;
  let payload: unknown;
  try {
    payload = JSON.parse(frame.data);
  } catch {
    return; // Malformed frame: skip it without killing the stream.
  }
  if (payload === null || typeof payload !== 'object') return;
  switch (frame.event) {
    case 'meta':
      handlers.onMeta?.(payload as StreamMetaEvent);
      break;
    case 'sql': {
      const { sql } = payload as StreamSqlEvent;
      if (typeof sql === 'string') handlers.onSql?.(sql);
      break;
    }
    case 'result':
      handlers.onResult?.(payload as StreamResultEvent);
      break;
    case 'delta': {
      const { text } = payload as StreamDeltaEvent;
      if (typeof text === 'string') handlers.onDelta?.(text);
      break;
    }
    case 'done':
      handlers.onDone?.(payload as StreamDoneEvent);
      break;
    case 'error':
      handlers.onError?.(payload as StreamErrorEvent);
      break;
    default:
      // Unknown event types are ignored for forward compatibility.
      break;
  }
}

/** Frames are separated by a blank line; tolerate CRLF streams. */
const FRAME_BOUNDARY = /\r?\n\r?\n/;

/** Client for the ContextGrip AI Chat API. */
export class AiChatClient {
  readonly #baseUrl: string;
  readonly #token: string | undefined;
  readonly #fetch: typeof fetch;

  constructor(opts: AiChatClientOptions) {
    this.#baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.#token = opts.token;
    // Wrap the global fetch so it is never called with a foreign `this`
    // (browsers throw "Illegal invocation" otherwise).
    this.#fetch = opts.fetch ?? ((input, init) => fetch(input, init));
  }

  /** `GET /api/v1/status` — service status for authenticated callers. */
  status(): Promise<Status> {
    return this.#json('GET', '/api/v1/status');
  }

  /**
   * `POST /api/v1/ask` — one-shot question; blocks until the full answer
   * is ready. Failed query execution is NOT an HTTP error: the response
   * carries `resultError` and an `answer` explaining the failure.
   */
  ask(req: AskRequest): Promise<AskResponse> {
    return this.#json('POST', '/api/v1/ask', req);
  }

  /**
   * `POST /api/v1/messages` — ask a question and stream the answer over
   * SSE. Event sequence: meta -> sql -> result -> delta* -> done, or a
   * terminal `error` event at any point after headers are sent.
   *
   * A terminal `error` event calls `handlers.onError` and resolves the
   * promise; pre-stream failures (validation, auth, unknown conversation)
   * reject with {@link AiChatError}. Aborting via `opts.signal` rejects
   * with the abort reason (an `AbortError` by default).
   */
  async streamMessage(
    req: AskRequest,
    handlers: StreamHandlers,
    opts: { signal?: AbortSignal } = {},
  ): Promise<void> {
    const res = await this.#send(
      'POST',
      '/api/v1/messages',
      req,
      'text/event-stream',
      opts.signal,
    );
    if (res.body === null) {
      throw new AiChatError('response has no body', res.status);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    const drain = (): void => {
      for (;;) {
        const boundary = FRAME_BOUNDARY.exec(buffer);
        if (boundary === null) break;
        const frame = buffer.slice(0, boundary.index);
        buffer = buffer.slice(boundary.index + boundary[0].length);
        dispatchFrame(frame, handlers);
      }
    };
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        drain();
      }
      buffer += decoder.decode();
      drain();
      // Flush a trailing complete frame not terminated by a blank line.
      if (buffer.trim() !== '') {
        dispatchFrame(buffer, handlers);
      }
    } finally {
      try {
        await reader.cancel();
      } catch {
        // The stream is already closed or errored; nothing to release.
      }
    }
  }

  /** `GET /api/v1/conversations` — most recently updated first. */
  listConversations(): Promise<Conversation[]> {
    return this.#json('GET', '/api/v1/conversations');
  }

  /** `GET /api/v1/conversations/{id}` — one conversation with its messages in order. */
  getConversation(id: string): Promise<ConversationDetail> {
    return this.#json('GET', `/api/v1/conversations/${encodeURIComponent(id)}`);
  }

  /** `DELETE /api/v1/conversations/{id}` — delete a conversation and its messages. */
  async deleteConversation(id: string): Promise<void> {
    await this.#json('DELETE', `/api/v1/conversations/${encodeURIComponent(id)}`);
  }

  /** `GET /api/v1/tokens` — list named API tokens (admin: primary token only). */
  listTokens(): Promise<TokenInfo[]> {
    return this.#json('GET', '/api/v1/tokens');
  }

  /**
   * `POST /api/v1/tokens` — mint a named API token (admin: primary token
   * only). The raw token value is returned once in this response.
   */
  createToken(label: string): Promise<CreatedToken> {
    return this.#json('POST', '/api/v1/tokens', { label });
  }

  /** `DELETE /api/v1/tokens/{id}` — revoke a named API token (admin: primary token only). */
  async revokeToken(id: string): Promise<void> {
    await this.#json('DELETE', `/api/v1/tokens/${encodeURIComponent(id)}`);
  }

  async #json<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.#send(method, path, body, 'application/json');
    return (await res.json()) as T;
  }

  async #send(
    method: string,
    path: string,
    body: unknown,
    accept: string,
    signal?: AbortSignal,
  ): Promise<Response> {
    const headers: Record<string, string> = { accept };
    if (this.#token !== undefined) {
      headers.authorization = `Bearer ${this.#token}`;
    }
    let payload: string | undefined;
    if (body !== undefined) {
      headers['content-type'] = 'application/json';
      payload = JSON.stringify(body);
    }
    const res = await this.#fetch(this.#baseUrl + path, {
      method,
      headers,
      body: payload,
      signal,
    });
    if (!res.ok) {
      throw await errorFromResponse(res);
    }
    return res;
  }
}
