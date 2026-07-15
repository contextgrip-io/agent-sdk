/**
 * Types for the ContextGrip AI Chat API.
 *
 * Field names mirror `openapi.yaml` at the repository root exactly — that
 * file is the source of truth for this contract.
 */

/** Response of `GET /api/v1/status`. */
export interface Status {
  version: string;
  /** Anthropic model id in use. */
  model: string;
  /** Query engine; currently always `"postgresql"`. */
  engine: string;
  ready: boolean;
}

/** Request body for `POST /api/v1/ask` and `POST /api/v1/messages`. */
export interface AskRequest {
  /** Plain-language question (server enforces a 4000-char maximum). */
  question: string;
  /** Continue an existing conversation; omit to start a new one. */
  conversationId?: string;
}

/** Summary of an executed query's result. */
export interface ResultSummary {
  columns?: string[];
  /** Up to 20 rows; string cells bounded to 256 chars. */
  rowSample?: unknown[][];
  /** Rows returned (capped at 100; see `truncated`). */
  rowCount: number;
  truncated: boolean;
  executionTimeMs: number;
}

/** Response of `POST /api/v1/ask`. */
export interface AskResponse {
  conversationId: string;
  userMessageId: string;
  assistantMessageId: string;
  /** The generated read-only SQL (always shown). */
  sql: string;
  result?: ResultSummary;
  /** Set instead of `result` when query execution failed. */
  resultError?: string;
  /** Natural-language explanation of the outcome. */
  answer: string;
}

/** One conversation in `GET /api/v1/conversations`. */
export interface Conversation {
  id: string;
  /** Derived from the first question, max 80 chars. */
  title: string;
  createdAt: string;
  updatedAt: string;
}

/** One message inside a conversation. */
export interface Message {
  id: string;
  role: 'user' | 'assistant';
  text?: string;
  sql?: string;
  result?: ResultSummary;
  error?: string;
  createdAt: string;
}

/** Response of `GET /api/v1/conversations/{id}`. */
export interface ConversationDetail {
  conversation: Conversation;
  messages: Message[];
}

/** A named API token (raw value is never returned here). */
export interface TokenInfo {
  id: string;
  label: string;
  /** First 8 hex chars of SHA-256(token). */
  fingerprint: string;
  createdAt: string;
  lastUsedAt?: string;
}

/** Response of `POST /api/v1/tokens`. */
export interface CreatedToken extends TokenInfo {
  /** Raw token value — shown only in this response. */
  token: string;
}

/** Eval verdict for an assistant answer. */
export type TrainingVerdict = 'good' | 'bad';

/** Body and response of `GET`/`PUT /api/v1/training/capture`. */
export interface TrainingCapture {
  enabled: boolean;
}

/** Response of `GET /api/v1/training/stats`. */
export interface TrainingStats {
  records: number;
  /** Records carrying an eval verdict. */
  evaluated: number;
  firstCapturedAt?: string;
  lastCapturedAt?: string;
}

/**
 * One JSONL line of `GET /api/v1/training/export`. The field layout matches
 * ContextGrip's training-data export, so dumps from both sources merge
 * downstream without transformation.
 */
export interface TrainingExportLine {
  id: string;
  capturedAt: string;
  connection: {
    /** Stable non-secret hash of host:port/dbname. */
    id: string;
    /** Database name from DATABASE_URL. */
    name: string;
    /** Currently always `"postgresql"`. */
    engine: string;
  };
  context: {
    /** `"chat"` (SSE) or `"ask"` (one-shot). */
    session?: string;
    /** Assistant message id, for dedupe. */
    sourceMessageId?: string;
  };
  query: {
    sql: string;
    /** The natural-language question. */
    intent?: string;
  };
  response: {
    columns?: string[];
    rowCount: number;
    truncated: boolean;
    executionTimeMs: number;
    error?: string;
    rowSample?: unknown[][];
  };
  eval?: {
    verdict: TrainingVerdict;
  };
}

/** Options for {@link AiChatClient.exportTraining}. */
export interface TrainingExportOptions {
  /** Include bounded result row samples (server default: true). */
  includeRows?: boolean;
  /** Restrict to records with an eval verdict (server default: false). */
  evaluatedOnly?: boolean;
}

/*
 * SSE event payloads for `POST /api/v1/messages`.
 *
 * Event sequence: meta -> sql -> result -> delta* -> done, or a terminal
 * `error` event at any point after headers are sent.
 */

/** Payload of the `meta` event. */
export interface StreamMetaEvent {
  conversationId: string;
  userMessageId: string;
}

/** Payload of the `sql` event. */
export interface StreamSqlEvent {
  sql: string;
}

/** Payload of the `result` event when query execution failed. */
export interface StreamResultErrorEvent {
  error: string;
  executionTimeMs: number;
}

/**
 * Payload of the `result` event: a {@link ResultSummary} on success, or
 * `{ error, executionTimeMs }` when execution failed. Discriminate with
 * `'error' in result`.
 */
export type StreamResultEvent = ResultSummary | StreamResultErrorEvent;

/** Payload of the `delta` event. */
export interface StreamDeltaEvent {
  text: string;
}

/** Payload of the `done` event. */
export interface StreamDoneEvent {
  conversationId: string;
  assistantMessageId: string;
}

/** Payload of the terminal `error` event. */
export interface StreamErrorEvent {
  message: string;
}

/**
 * Callbacks for {@link AiChatClient.streamMessage}. All handlers are
 * optional; unknown event types are ignored for forward compatibility.
 *
 * `onSql` and `onDelta` receive the payload's string directly (`sql` and
 * `text` respectively); the other handlers receive the full event payload.
 */
export interface StreamHandlers {
  onMeta?: (meta: StreamMetaEvent) => void;
  onSql?: (sql: string) => void;
  onResult?: (result: StreamResultEvent) => void;
  onDelta?: (text: string) => void;
  onDone?: (done: StreamDoneEvent) => void;
  /**
   * Called for a terminal `error` event on the stream. Receiving one
   * resolves the `streamMessage` promise; it does not reject.
   */
  onError?: (error: StreamErrorEvent) => void;
}
