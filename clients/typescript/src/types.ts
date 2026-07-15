/**
 * Types for the ContextGrip AI Chat API.
 *
 * Field names mirror `openapi.yaml` at the repository root exactly — that
 * file is the source of truth for this contract.
 */

/** An enabled surface from `AI_CHAT_FEATURES`. */
export type Feature = 'chat' | 'agent' | 'board';

/** Response of `GET /api/v1/status`. */
export interface Status {
  version: string;
  /** Anthropic model id in use. */
  model: string;
  /** Query engine; currently always `"postgresql"`. */
  engine: string;
  ready: boolean;
  /** Enabled surfaces from `AI_CHAT_FEATURES`. */
  features: Feature[];
  /** True when `AI_CHAT_WRITE_DATABASE_URL` is configured — approvals can execute. */
  writesEnabled: boolean;
}

/** Request body for `POST /api/v1/ask` and `POST /api/v1/messages`. */
export interface AskRequest {
  /** Plain-language question (server enforces a 4000-char maximum). */
  question: string;
  /** Continue an existing conversation; omit to start a new one. */
  conversationId?: string;
  /**
   * `agent` lets the model take multiple tool steps (read-only queries run
   * automatically; writes become approvals). Requires the `"agent"`
   * feature; a conversation keeps the mode of its first message.
   * Server default: `chat`.
   */
  mode?: 'chat' | 'agent';
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

/** One completed agent tool step. */
export interface Step {
  index: number;
  kind: 'query' | 'schema' | 'note';
  /** One-line description of the step. */
  summary: string;
  /** The read-only SQL this step executed (kind `query`). */
  sql?: string;
  result?: ResultSummary;
  /** Step execution error. */
  error?: string;
}

/** A proposed write awaiting (or carrying) a decision. */
export interface Approval {
  id: string;
  /** The exact statement that will run if approved. */
  sql: string;
  /** The model's stated reason for the write. */
  rationale?: string;
  status: 'pending' | 'approved' | 'rejected';
  /** Where the proposal came from (exactly one of the ids is set). */
  source: {
    conversationId?: string;
    messageId?: string;
    taskId?: string;
  };
  createdAt: string;
  decidedAt?: string;
}

/** Response of `POST /api/v1/approvals/{id}`. */
export interface ApprovalDecisionResult {
  approval: Approval;
  /** Execution outcome when the approved write succeeded. */
  result?: ResultSummary;
  /** Execution error when the approved write failed. */
  error?: string;
}

/** Lifecycle state of a board task. */
export type TaskStatus = 'queued' | 'running' | 'needs_approval' | 'done' | 'failed' | 'canceled';

/** One board task. */
export interface Task {
  id: string;
  title: string;
  prompt: string;
  status: TaskStatus;
  createdAt: string;
  updatedAt: string;
  /** Final answer when done. */
  answer?: string;
  /** Failure reason when failed. */
  error?: string;
}

/** Response of `GET /api/v1/tasks/{id}`. */
export interface TaskDetail {
  task: Task;
  steps: Step[];
  pendingApproval?: Approval;
}

/** Response of `POST /api/v1/ask`. */
export interface AskResponse {
  conversationId: string;
  userMessageId: string;
  assistantMessageId: string;
  /** The generated read-only SQL (chat mode; agent mode may carry `steps` instead). */
  sql?: string;
  result?: ResultSummary;
  /** Set instead of `result` when query execution failed. */
  resultError?: string;
  /** Natural-language explanation of the outcome. */
  answer: string;
  /** Agent-mode tool steps, in execution order. */
  steps?: Step[];
  /** Set when the turn ended awaiting a write approval. */
  pendingApprovalId?: string;
}

/** One conversation in `GET /api/v1/conversations`. */
export interface Conversation {
  id: string;
  /** Derived from the first question, max 80 chars. */
  title: string;
  /** Fixed by the conversation's first message. */
  mode: 'chat' | 'agent';
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
  /** Agent-mode tool steps persisted with the message. */
  steps?: Step[];
  /** Set while this message's proposed write awaits a decision. */
  pendingApprovalId?: string;
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
 * `error` event at any point after headers are sent. In agent mode,
 * `step` and `approval_required` events are interleaved before the delta
 * stream, and the sql/result pair may be absent (steps carry the queries
 * instead).
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
  /** Set when the turn ended awaiting a write approval (agent mode). */
  pendingApprovalId?: string;
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
  /** Agent mode: one call per completed tool step, in order. */
  onStep?: (step: Step) => void;
  /**
   * Agent mode: the model proposed a write. The turn ends after this —
   * `done` carries `pendingApprovalId` — and the write executes only via
   * {@link AiChatClient.decideApproval}.
   */
  onApprovalRequired?: (approval: Approval) => void;
  onDelta?: (text: string) => void;
  onDone?: (done: StreamDoneEvent) => void;
  /**
   * Called for a terminal `error` event on the stream. Receiving one
   * resolves the `streamMessage` promise; it does not reject.
   */
  onError?: (error: StreamErrorEvent) => void;
}
