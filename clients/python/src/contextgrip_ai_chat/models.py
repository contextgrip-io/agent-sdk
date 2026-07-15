"""Typed models mirroring openapi.yaml schemas.

Wire names are camelCase; attributes are snake_case. Timestamps are kept as
the ISO-8601 strings the server sends (``created_at``, ``updated_at``,
``last_used_at``).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Union


@dataclass
class Status:
    """GET /api/v1/status response."""

    version: str
    model: str
    engine: str
    ready: bool
    features: list[str]
    writes_enabled: bool

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Status":
        return cls(
            version=data["version"],
            model=data["model"],
            engine=data["engine"],
            ready=data["ready"],
            features=data["features"],
            writes_enabled=data["writesEnabled"],
        )


@dataclass
class ResultSummary:
    """Summary of a successfully executed query."""

    row_count: int
    truncated: bool
    execution_time_ms: int
    columns: list[str] | None = None
    row_sample: list[list[Any]] | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ResultSummary":
        return cls(
            row_count=data["rowCount"],
            truncated=data["truncated"],
            execution_time_ms=data["executionTimeMs"],
            columns=data.get("columns"),
            row_sample=data.get("rowSample"),
        )


@dataclass
class Step:
    """One agent-mode tool step, in execution order."""

    index: int
    kind: str  # query | schema | note
    summary: str
    sql: str | None = None
    result: ResultSummary | None = None
    error: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Step":
        result = data.get("result")
        return cls(
            index=data["index"],
            kind=data["kind"],
            summary=data["summary"],
            sql=data.get("sql"),
            result=ResultSummary.from_dict(result) if result is not None else None,
            error=data.get("error"),
        )


@dataclass
class AskResponse:
    """POST /api/v1/ask response.

    In chat mode ``sql`` is set and exactly one of ``result`` /
    ``result_error`` accompanies it (``result_error`` means query execution
    failed, which is not an HTTP error — ``answer`` then explains the
    failure). In agent mode the sql/result pair may be absent: ``steps``
    carries the queries instead, and ``pending_approval_id`` is set when
    the turn ended awaiting a write approval.
    """

    conversation_id: str
    user_message_id: str
    assistant_message_id: str
    answer: str
    sql: str | None = None
    result: ResultSummary | None = None
    result_error: str | None = None
    steps: list[Step] | None = None
    pending_approval_id: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AskResponse":
        result = data.get("result")
        steps = data.get("steps")
        return cls(
            conversation_id=data["conversationId"],
            user_message_id=data["userMessageId"],
            assistant_message_id=data["assistantMessageId"],
            answer=data["answer"],
            sql=data.get("sql"),
            result=ResultSummary.from_dict(result) if result is not None else None,
            result_error=data.get("resultError"),
            steps=[Step.from_dict(s) for s in steps] if steps is not None else None,
            pending_approval_id=data.get("pendingApprovalId"),
        )


@dataclass
class Conversation:
    id: str
    title: str
    created_at: str
    updated_at: str
    mode: str = "chat"  # fixed by the conversation's first message

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Conversation":
        return cls(
            id=data["id"],
            title=data["title"],
            created_at=data["createdAt"],
            updated_at=data["updatedAt"],
            mode=data.get("mode", "chat"),
        )


@dataclass
class Message:
    id: str
    role: str
    created_at: str
    text: str | None = None
    sql: str | None = None
    result: ResultSummary | None = None
    error: str | None = None
    steps: list[Step] | None = None
    pending_approval_id: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Message":
        result = data.get("result")
        steps = data.get("steps")
        return cls(
            id=data["id"],
            role=data["role"],
            created_at=data["createdAt"],
            text=data.get("text"),
            sql=data.get("sql"),
            result=ResultSummary.from_dict(result) if result is not None else None,
            error=data.get("error"),
            steps=[Step.from_dict(s) for s in steps] if steps is not None else None,
            pending_approval_id=data.get("pendingApprovalId"),
        )


@dataclass
class ConversationDetail:
    conversation: Conversation
    messages: list[Message] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ConversationDetail":
        return cls(
            conversation=Conversation.from_dict(data["conversation"]),
            messages=[Message.from_dict(m) for m in data.get("messages") or []],
        )


@dataclass
class TokenInfo:
    """A named API token. Raw token values are never returned here."""

    id: str
    label: str
    fingerprint: str
    created_at: str
    last_used_at: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TokenInfo":
        return cls(
            id=data["id"],
            label=data["label"],
            fingerprint=data["fingerprint"],
            created_at=data["createdAt"],
            last_used_at=data.get("lastUsedAt"),
        )


@dataclass
class CreatedToken:
    """POST /api/v1/tokens response — ``token`` is shown only once."""

    id: str
    label: str
    fingerprint: str
    created_at: str
    token: str
    last_used_at: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "CreatedToken":
        return cls(
            id=data["id"],
            label=data["label"],
            fingerprint=data["fingerprint"],
            created_at=data["createdAt"],
            token=data["token"],
            last_used_at=data.get("lastUsedAt"),
        )


# --- approvals & tasks --------------------------------------------------------


@dataclass
class ApprovalSource:
    """Where a write proposal came from (exactly one of the ids is set)."""

    conversation_id: str | None = None
    message_id: str | None = None
    task_id: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ApprovalSource":
        return cls(
            conversation_id=data.get("conversationId"),
            message_id=data.get("messageId"),
            task_id=data.get("taskId"),
        )


@dataclass
class Approval:
    """A proposed write awaiting (or carrying) a decision."""

    id: str
    sql: str
    status: str  # pending | approved | rejected
    source: ApprovalSource
    created_at: str
    rationale: str | None = None
    decided_at: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Approval":
        return cls(
            id=data["id"],
            sql=data["sql"],
            status=data["status"],
            source=ApprovalSource.from_dict(data.get("source") or {}),
            created_at=data["createdAt"],
            rationale=data.get("rationale"),
            decided_at=data.get("decidedAt"),
        )


@dataclass
class DecideApprovalResult:
    """POST /api/v1/approvals/{id} response.

    ``result`` carries the execution outcome of an approved write;
    ``error`` is set instead when the approved write failed.
    """

    approval: Approval
    result: ResultSummary | None = None
    error: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "DecideApprovalResult":
        result = data.get("result")
        return cls(
            approval=Approval.from_dict(data["approval"]),
            result=ResultSummary.from_dict(result) if result is not None else None,
            error=data.get("error"),
        )


@dataclass
class Task:
    """A board task run through the agent loop in the background."""

    id: str
    title: str
    prompt: str
    status: str  # queued | running | needs_approval | done | failed | canceled
    created_at: str
    updated_at: str
    answer: str | None = None
    error: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Task":
        return cls(
            id=data["id"],
            title=data["title"],
            prompt=data["prompt"],
            status=data["status"],
            created_at=data["createdAt"],
            updated_at=data["updatedAt"],
            answer=data.get("answer"),
            error=data.get("error"),
        )


@dataclass
class TaskDetail:
    """Task detail: status, transcript steps, answer, pending approval."""

    task: Task
    steps: list[Step] = field(default_factory=list)
    pending_approval: Approval | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TaskDetail":
        pending = data.get("pendingApproval")
        return cls(
            task=Task.from_dict(data["task"]),
            steps=[Step.from_dict(s) for s in data.get("steps") or []],
            pending_approval=Approval.from_dict(pending) if pending is not None else None,
        )


# --- training data ----------------------------------------------------------


@dataclass
class TrainingStats:
    """GET /api/v1/training/stats response."""

    records: int
    evaluated: int
    first_captured_at: str | None = None
    last_captured_at: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TrainingStats":
        return cls(
            records=data["records"],
            evaluated=data["evaluated"],
            first_captured_at=data.get("firstCapturedAt"),
            last_captured_at=data.get("lastCapturedAt"),
        )


@dataclass
class ExportConnection:
    """Non-secret identity of the database a record was captured against."""

    id: str
    name: str
    engine: str

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExportConnection":
        return cls(id=data["id"], name=data["name"], engine=data["engine"])


@dataclass
class ExportContext:
    """Capture context: session kind and source message id (for dedupe)."""

    session: str | None = None
    source_message_id: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExportContext":
        return cls(
            session=data.get("session"),
            source_message_id=data.get("sourceMessageId"),
        )


@dataclass
class ExportQuery:
    """The generated SQL and the natural-language intent behind it."""

    sql: str
    intent: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExportQuery":
        return cls(sql=data["sql"], intent=data.get("intent"))


@dataclass
class ExportResponse:
    """Execution outcome for an exported record.

    ``error`` is set when execution failed; ``row_sample`` is present only
    when the export was requested with ``includeRows``.
    """

    row_count: int
    truncated: bool
    execution_time_ms: int
    columns: list[str] | None = None
    error: str | None = None
    row_sample: list[list[Any]] | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExportResponse":
        return cls(
            row_count=data["rowCount"],
            truncated=data["truncated"],
            execution_time_ms=data["executionTimeMs"],
            columns=data.get("columns"),
            error=data.get("error"),
            row_sample=data.get("rowSample"),
        )


@dataclass
class ExportEval:
    """Explicit good/bad verdict attached to a record, when rated."""

    verdict: str

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExportEval":
        return cls(verdict=data["verdict"])


@dataclass
class TrainingExportLine:
    """One JSONL line from GET /api/v1/training/export.

    Field layout matches ContextGrip's training export, so dumps from both
    sources merge downstream without transformation.
    """

    id: str
    captured_at: str
    connection: ExportConnection
    context: ExportContext
    query: ExportQuery
    response: ExportResponse
    eval: ExportEval | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TrainingExportLine":
        eval_data = data.get("eval")
        return cls(
            id=data["id"],
            captured_at=data["capturedAt"],
            connection=ExportConnection.from_dict(data["connection"]),
            context=ExportContext.from_dict(data.get("context") or {}),
            query=ExportQuery.from_dict(data["query"]),
            response=ExportResponse.from_dict(data["response"]),
            eval=ExportEval.from_dict(eval_data) if eval_data is not None else None,
        )


# --- SSE stream events (POST /api/v1/messages) -----------------------------


@dataclass
class MetaEvent:
    """First event of a stream: ids for the conversation and user message."""

    conversation_id: str
    user_message_id: str


@dataclass
class SqlEvent:
    """The generated read-only SQL."""

    sql: str


@dataclass
class ResultEvent:
    """Query execution outcome.

    On success ``result`` is set; on execution failure ``error`` is set
    instead. ``execution_time_ms`` is populated in both cases when the
    server sent it.
    """

    result: ResultSummary | None = None
    error: str | None = None
    execution_time_ms: int | None = None


@dataclass
class DeltaEvent:
    """A chunk of the streamed natural-language answer."""

    text: str


@dataclass
class StepEvent:
    """Agent mode: one completed tool step, in order."""

    step: Step


@dataclass
class ApprovalRequiredEvent:
    """Agent mode: a proposed write; the turn ends after this event.

    The write executes only via ``decide_approval`` — the following
    ``DoneEvent`` carries the ``pending_approval_id``.
    """

    approval: Approval


@dataclass
class DoneEvent:
    """Terminal success event.

    ``pending_approval_id`` is set when the turn ended awaiting a write
    approval (agent mode).
    """

    conversation_id: str
    assistant_message_id: str
    pending_approval_id: str | None = None


@dataclass
class ErrorEvent:
    """Terminal error event (yielded, never raised, once streaming began)."""

    message: str


StreamEvent = Union[
    MetaEvent,
    SqlEvent,
    ResultEvent,
    StepEvent,
    ApprovalRequiredEvent,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
]
