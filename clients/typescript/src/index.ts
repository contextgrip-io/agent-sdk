/**
 * @contextgrip/ai-chat-client — TypeScript client for the ContextGrip AI Chat API.
 *
 * The contract is `openapi.yaml` at the repository root. Zero runtime
 * dependencies: uses the global `fetch` (Node >= 20 and browsers).
 */

import type {
  Approval,
  ApprovalDecisionResult,
  AskRequest,
  AskResponse,
  Conversation,
  ConversationDetail,
  CreatedToken,
  Status,
  Step,
  StreamDeltaEvent,
  StreamDoneEvent,
  StreamErrorEvent,
  StreamHandlers,
  StreamMetaEvent,
  StreamResultEvent,
  StreamSqlEvent,
  Task,
  TaskDetail,
  TaskStatus,
  TokenInfo,
  TrainingCapture,
  TrainingExportLine,
  TrainingExportOptions,
  TrainingStats,
  TrainingVerdict,
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
   * CONVERSATION_FULL, STORE_ERROR, MODEL_ERROR, STREAM_UNSUPPORTED,
   * FEATURE_DISABLED, WRITES_DISABLED, ALREADY_DECIDED, TASK_ACTIVE,
   * TASK_FINISHED.
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
    case 'step':
      handlers.onStep?.(payload as Step);
      break;
    case 'approval_required':
      handlers.onApprovalRequired?.(payload as Approval);
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

/**
 * Parse one NDJSON line into an object. Returns undefined for blank lines.
 * The server enforces its export byte budget on whole lines, so a partial
 * line can never legitimately occur — a line that fails to parse indicates
 * corruption and throws rather than silently dropping a training record
 * (matching the Go and Python clients).
 */
function parseNdjsonLine(rawLine: string): Record<string, unknown> | undefined {
  const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine;
  if (line.trim() === '') return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    throw new Error(`exportTraining: malformed NDJSON line: ${line.slice(0, 80)}`);
  }
  if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
    return parsed as Record<string, unknown>;
  }
  throw new Error(`exportTraining: NDJSON line is not an object: ${line.slice(0, 80)}`);
}

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

  /** `GET /api/v1/approvals` — list pending write approvals (chat and board sources). */
  listApprovals(): Promise<Approval[]> {
    return this.#json('GET', '/api/v1/approvals');
  }

  /**
   * `POST /api/v1/approvals/{id}` — approve or reject a proposed write.
   * Approving executes the exact proposed SQL against the write connection
   * in a single transaction and returns the execution outcome. Decisions
   * are idempotent-once: deciding an already-decided approval rejects with
   * {@link AiChatError} code `ALREADY_DECIDED`; without a configured write
   * connection, approval rejects with code `WRITES_DISABLED` (both 409).
   */
  decideApproval(id: string, decision: 'approve' | 'reject'): Promise<ApprovalDecisionResult> {
    return this.#json('POST', `/api/v1/approvals/${encodeURIComponent(id)}`, { decision });
  }

  /** `POST /api/v1/tasks` — file a task for the agent (requires the `"board"` feature). */
  createTask(title: string, prompt: string): Promise<Task> {
    return this.#json('POST', '/api/v1/tasks', { title, prompt });
  }

  /** `GET /api/v1/tasks` — list board tasks, most recently updated first. */
  listTasks(status?: TaskStatus): Promise<Task[]> {
    const query = status === undefined ? '' : `?status=${encodeURIComponent(status)}`;
    return this.#json('GET', `/api/v1/tasks${query}`);
  }

  /** `GET /api/v1/tasks/{id}` — status, transcript steps, answer, pending approval. */
  getTask(id: string): Promise<TaskDetail> {
    return this.#json('GET', `/api/v1/tasks/${encodeURIComponent(id)}`);
  }

  /** `POST /api/v1/tasks/{id}/cancel` — cancel a queued, running, or approval-blocked task. */
  cancelTask(id: string): Promise<Task> {
    return this.#json('POST', `/api/v1/tasks/${encodeURIComponent(id)}/cancel`);
  }

  /**
   * `DELETE /api/v1/tasks/{id}` — delete a finished task
   * (done/failed/canceled only; a still-active task rejects with
   * {@link AiChatError} code `TASK_ACTIVE`).
   */
  async deleteTask(id: string): Promise<void> {
    await this.#json('DELETE', `/api/v1/tasks/${encodeURIComponent(id)}`);
  }

  /**
   * `POST /api/v1/messages/{id}/eval` — rate an assistant answer
   * (good/bad); writes a training record. Explicit evals bypass the
   * capture toggle. Upserts by message id: rating the same answer again
   * updates the verdict. Only assistant messages that carry SQL can be
   * rated.
   */
  async rateMessage(id: string, verdict: TrainingVerdict): Promise<void> {
    await this.#json('POST', `/api/v1/messages/${encodeURIComponent(id)}/eval`, { verdict });
  }

  /** `GET /api/v1/training/capture` — read the automatic-capture setting. */
  getTrainingCapture(): Promise<TrainingCapture> {
    return this.#json('GET', '/api/v1/training/capture');
  }

  /**
   * `PUT /api/v1/training/capture` — enable/disable automatic capture
   * (admin: primary token only). When enabled (the default), every
   * completed chat exchange is recorded as a training record; explicit
   * evals are recorded regardless of this setting.
   */
  setTrainingCapture(enabled: boolean): Promise<TrainingCapture> {
    return this.#json('PUT', '/api/v1/training/capture', { enabled });
  }

  /** `GET /api/v1/training/stats` — training-record counts and capture range. */
  trainingStats(): Promise<TrainingStats> {
    return this.#json('GET', '/api/v1/training/stats');
  }

  /**
   * `GET /api/v1/training/export` — stream training records as JSONL
   * (newline-delimited JSON), yielding one parsed {@link TrainingExportLine}
   * per line. Blank lines are skipped. The server enforces its 64 MiB
   * export byte budget on whole lines (compare the line count with
   * `trainingStats()` to detect truncation), so a line that fails to parse
   * indicates corruption and throws rather than silently dropping a record.
   *
   * The request is made lazily on first iteration; a non-2xx response
   * rejects the first `next()` with {@link AiChatError}. Breaking out of
   * the loop early cancels the underlying stream.
   */
  async *exportTraining(
    opts: TrainingExportOptions = {},
  ): AsyncGenerator<TrainingExportLine, void, void> {
    const params = new URLSearchParams();
    if (opts.includeRows !== undefined) params.set('includeRows', String(opts.includeRows));
    if (opts.evaluatedOnly !== undefined) params.set('evaluatedOnly', String(opts.evaluatedOnly));
    const query = params.toString();
    const path = `/api/v1/training/export${query === '' ? '' : `?${query}`}`;
    const res = await this.#send('GET', path, undefined, 'application/x-ndjson');
    if (res.body === null) {
      throw new AiChatError('response has no body', res.status);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let newline: number;
        while ((newline = buffer.indexOf('\n')) !== -1) {
          const line = parseNdjsonLine(buffer.slice(0, newline));
          buffer = buffer.slice(newline + 1);
          if (line !== undefined) yield line as unknown as TrainingExportLine;
        }
      }
      buffer += decoder.decode();
      // Flush a trailing line not terminated by a newline.
      const trailing = parseNdjsonLine(buffer);
      if (trailing !== undefined) yield trailing as unknown as TrainingExportLine;
    } finally {
      try {
        await reader.cancel();
      } catch {
        // The stream is already closed or errored; nothing to release.
      }
    }
  }

  /**
   * `DELETE /api/v1/training/records` — delete ALL training records
   * (admin: primary token only). Returns the number of records removed.
   */
  deleteTrainingRecords(): Promise<{ deleted: number }> {
    return this.#json('DELETE', '/api/v1/training/records');
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
