// Thin client for the AI Chat API (see ../../openapi.yaml). All URLs are
// relative: same-origin in production (the UI is embedded in the Go binary),
// proxied to localhost:8080 by Vite in development.

import { SseParser, type SseEvent } from './sse';
import type {
  Conversation,
  ConversationDetail,
  ResultEvent,
  Status,
  TrainingCapture,
  TrainingStats,
  Verdict,
} from './types';

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

function authHeaders(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}` };
}

/** Errors are {"error": "...", "code": "..."} per the contract. */
async function toApiError(res: Response): Promise<ApiError> {
  let message = `Request failed (HTTP ${res.status})`;
  let code: string | undefined;
  try {
    const body: unknown = await res.json();
    if (body && typeof body === 'object') {
      const b = body as { error?: unknown; code?: unknown };
      if (typeof b.error === 'string' && b.error) message = b.error;
      if (typeof b.code === 'string') code = b.code;
    }
  } catch {
    // non-JSON body; keep the generic message
  }
  return new ApiError(message, res.status, code);
}

async function getJson<T>(token: string, path: string): Promise<T> {
  const res = await fetch(path, { headers: authHeaders(token) });
  if (!res.ok) throw await toApiError(res);
  return (await res.json()) as T;
}

export function getStatus(token: string): Promise<Status> {
  return getJson<Status>(token, '/api/v1/status');
}

export function listConversations(token: string): Promise<Conversation[]> {
  return getJson<Conversation[]>(token, '/api/v1/conversations');
}

export function getConversation(token: string, id: string): Promise<ConversationDetail> {
  return getJson<ConversationDetail>(token, `/api/v1/conversations/${encodeURIComponent(id)}`);
}

export async function deleteConversation(token: string, id: string): Promise<void> {
  const res = await fetch(`/api/v1/conversations/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: authHeaders(token),
  });
  if (!res.ok) throw await toApiError(res);
}

/** Rate an assistant answer; the server upserts by message id. */
export async function evalMessage(token: string, messageId: string, verdict: Verdict): Promise<void> {
  const res = await fetch(`/api/v1/messages/${encodeURIComponent(messageId)}/eval`, {
    method: 'POST',
    headers: { ...authHeaders(token), 'Content-Type': 'application/json' },
    body: JSON.stringify({ verdict }),
  });
  if (!res.ok) throw await toApiError(res);
}

export function getTrainingCapture(token: string): Promise<TrainingCapture> {
  return getJson<TrainingCapture>(token, '/api/v1/training/capture');
}

/** Admin-only (primary APP_ACCESS_TOKEN): 403 ADMIN_REQUIRED otherwise. */
export async function setTrainingCapture(token: string, enabled: boolean): Promise<TrainingCapture> {
  const res = await fetch('/api/v1/training/capture', {
    method: 'PUT',
    headers: { ...authHeaders(token), 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  });
  if (!res.ok) throw await toApiError(res);
  return (await res.json()) as TrainingCapture;
}

export function getTrainingStats(token: string): Promise<TrainingStats> {
  return getJson<TrainingStats>(token, '/api/v1/training/stats');
}

/** Fetch the JSONL training export as a Blob (for an anchor download). */
export async function fetchTrainingExport(token: string, evaluatedOnly: boolean): Promise<Blob> {
  const query = evaluatedOnly ? '?evaluatedOnly=true' : '';
  const res = await fetch(`/api/v1/training/export${query}`, { headers: authHeaders(token) });
  if (!res.ok) throw await toApiError(res);
  return await res.blob();
}

/** Admin-only. Deletes ALL training records; resolves to the removed count. */
export async function deleteTrainingRecords(token: string): Promise<number> {
  const res = await fetch('/api/v1/training/records', {
    method: 'DELETE',
    headers: authHeaders(token),
  });
  if (!res.ok) throw await toApiError(res);
  const body = (await res.json()) as { deleted?: number };
  return typeof body.deleted === 'number' ? body.deleted : 0;
}

export interface StreamHandlers {
  onMeta?(meta: { conversationId: string; userMessageId: string }): void;
  onSql?(sql: string): void;
  onResult?(result: ResultEvent): void;
  onDelta?(text: string): void;
  onDone?(done: { conversationId: string; assistantMessageId: string }): void;
  /** Terminal `error` event from the stream. */
  onError?(message: string): void;
}

function dispatch(ev: SseEvent, h: StreamHandlers): void {
  // Payload shapes per openapi.yaml's /api/v1/messages event documentation.
  const d = ev.data as Record<string, unknown> | null;
  switch (ev.event) {
    case 'meta':
      if (d && typeof d.conversationId === 'string' && typeof d.userMessageId === 'string') {
        h.onMeta?.({ conversationId: d.conversationId, userMessageId: d.userMessageId });
      }
      break;
    case 'sql':
      if (d && typeof d.sql === 'string') h.onSql?.(d.sql);
      break;
    case 'result':
      if (d && typeof d === 'object') h.onResult?.(d as unknown as ResultEvent);
      break;
    case 'delta':
      if (d && typeof d.text === 'string') h.onDelta?.(d.text);
      break;
    case 'done':
      if (d && typeof d.conversationId === 'string' && typeof d.assistantMessageId === 'string') {
        h.onDone?.({ conversationId: d.conversationId, assistantMessageId: d.assistantMessageId });
      }
      break;
    case 'error':
      h.onError?.(d && typeof d.message === 'string' ? d.message : 'The stream reported an unknown error.');
      break;
    default:
      // Unknown event types are ignored (forward compatibility).
      break;
  }
}

/**
 * POST /api/v1/messages and stream the SSE response.
 *
 * Pre-stream failures (validation, auth, unknown conversation, not
 * configured) reject with ApiError. Once the stream is open, events are
 * dispatched to the handlers; a terminal `error` event goes to onError and
 * does NOT reject. Resolves when the stream ends.
 */
export async function streamMessage(
  token: string,
  body: { question: string; conversationId?: string },
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch('/api/v1/messages', {
    method: 'POST',
    headers: {
      ...authHeaders(token),
      'Content-Type': 'application/json',
      Accept: 'text/event-stream',
    },
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw await toApiError(res);
  if (!res.body) throw new ApiError('The response had no body to stream.', res.status);

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  const parser = new SseParser();

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    for (const ev of parser.push(decoder.decode(value, { stream: true }))) {
      dispatch(ev, handlers);
    }
  }
  const tail = decoder.decode();
  for (const ev of tail ? parser.push(tail) : []) dispatch(ev, handlers);
  for (const ev of parser.flush()) dispatch(ev, handlers);
}
