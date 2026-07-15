// API types, mirrored from ../../openapi.yaml (the source of truth).

export interface Status {
  version: string;
  model: string;
  engine: string;
  ready: boolean;
}

export interface ResultSummary {
  columns?: string[];
  /** Up to 20 rows; string cells bounded to 256 chars. */
  rowSample?: unknown[][];
  /** Rows returned (capped at 100; see truncated). */
  rowCount: number;
  truncated: boolean;
  executionTimeMs: number;
}

/** Payload of the `result` SSE event when query execution failed. */
export interface ResultError {
  error: string;
  executionTimeMs: number;
}

export type ResultEvent = ResultSummary | ResultError;

export function isResultError(r: ResultEvent): r is ResultError {
  return typeof (r as ResultError).error === 'string';
}

export interface Conversation {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
}

export interface ApiMessage {
  id: string;
  role: 'user' | 'assistant';
  text?: string;
  sql?: string;
  result?: ResultSummary;
  error?: string;
  createdAt: string;
}

export interface ConversationDetail {
  conversation: Conversation;
  messages: ApiMessage[];
}

// UI-side message model. A streamed assistant turn and a persisted assistant
// message render through the same shape.
export interface UiMessage {
  id: string;
  role: 'user' | 'assistant';
  /** User question, or the streamed/persisted assistant answer text. */
  text?: string;
  sql?: string;
  result?: ResultSummary;
  /** Query execution failed (amber box). */
  resultError?: string;
  resultErrorTimeMs?: number;
  /** Terminal error (red box). */
  error?: string;
  /** True while this assistant turn is still streaming. */
  streaming?: boolean;
}

/**
 * Convert a persisted message into the UI shape. A persisted assistant
 * message that carries both sql and error is a query-execution failure and
 * renders as the amber box; an error without sql is a terminal error.
 */
export function uiMessageFromApi(m: ApiMessage): UiMessage {
  if (m.role === 'user') {
    return { id: m.id, role: 'user', text: m.text ?? '' };
  }
  const isQueryFailure = Boolean(m.sql && m.error);
  return {
    id: m.id,
    role: 'assistant',
    text: m.text,
    sql: m.sql,
    result: m.result,
    resultError: isQueryFailure ? m.error : undefined,
    error: isQueryFailure ? undefined : m.error,
  };
}
