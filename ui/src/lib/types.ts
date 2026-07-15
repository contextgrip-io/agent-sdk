// API types, mirrored from ../../openapi.yaml (the source of truth).

export type Feature = 'chat' | 'agent' | 'board';

export interface Status {
  version: string;
  model: string;
  engine: string;
  ready: boolean;
  /** Enabled surfaces from AI_CHAT_FEATURES. */
  features: Feature[];
  /** True when AI_CHAT_WRITE_DATABASE_URL is configured — approvals can execute. */
  writesEnabled: boolean;
}

export type AskMode = 'chat' | 'agent';

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

/** One agent-mode tool step. */
export interface Step {
  index: number;
  kind: 'query' | 'schema' | 'note';
  /** One-line description of the step. */
  summary: string;
  /** The read-only SQL this step executed (kind query). */
  sql?: string;
  result?: ResultSummary;
  /** Step execution error. */
  error?: string;
}

/** A proposed write awaiting (or past) a decision. */
export interface Approval {
  id: string;
  /** The exact statement that will run if approved. */
  sql: string;
  /** The model's stated reason for the write. */
  rationale?: string;
  status: 'pending' | 'approved' | 'rejected';
  source?: {
    conversationId?: string;
    messageId?: string;
    taskId?: string;
  };
  createdAt: string;
  decidedAt?: string;
}

/** Response of POST /api/v1/approvals/{id}. */
export interface ApprovalDecision {
  approval: Approval;
  result?: ResultSummary;
  /** Execution error when the approved write failed. */
  error?: string;
}

export type TaskStatus = 'queued' | 'running' | 'needs_approval' | 'done' | 'failed' | 'canceled';

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

export interface TaskDetail {
  task: Task;
  steps: Step[];
  pendingApproval?: Approval;
}

export interface Conversation {
  id: string;
  title: string;
  mode?: 'chat' | 'agent';
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
  /** Agent-mode tool steps persisted with the message. */
  steps?: Step[];
  /** Set while this message's proposed write awaits a decision. */
  pendingApprovalId?: string;
  createdAt: string;
}

export interface ConversationDetail {
  conversation: Conversation;
  messages: ApiMessage[];
}

export type Verdict = 'good' | 'bad';

export interface TrainingCapture {
  enabled: boolean;
}

export interface TrainingStats {
  records: number;
  /** Records carrying an eval verdict. */
  evaluated: number;
  firstCapturedAt?: string;
  lastCapturedAt?: string;
}

// UI-side message model. A streamed assistant turn and a persisted assistant
// message render through the same shape.
export interface UiMessage {
  id: string;
  /**
   * Server-assigned assistant message id, adopted from the `done` event.
   * Streamed turns start with a local placeholder id; only messages with a
   * real (server) id can be rated.
   */
  serverId?: string;
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
  /** Agent-mode tool steps, in order. */
  steps?: Step[];
  /** Id of the write proposal this turn ended on. */
  pendingApprovalId?: string;
  /** The full pending approval, when known (streamed event or lookup). */
  approval?: Approval;
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
    steps: m.steps,
    pendingApprovalId: m.pendingApprovalId,
  };
}

/**
 * A conversation keeps the mode of its first message (server-enforced), but
 * the contract does not expose the mode directly — infer it from agent-only
 * artifacts (steps / pending approvals) on any assistant message.
 */
export function looksLikeAgentConversation(messages: ApiMessage[]): boolean {
  return messages.some((m) => (m.steps?.length ?? 0) > 0 || Boolean(m.pendingApprovalId));
}
