"""Synchronous client for the ContextGrip AI Chat API."""

from __future__ import annotations

import json
from collections.abc import Iterator
from types import TracebackType
from typing import Any, Literal
from urllib.parse import quote

import httpx

from ._sse import iter_sse_frames, parse_stream_event
from .errors import AiChatError
from .models import (
    Approval,
    AskResponse,
    Conversation,
    ConversationDetail,
    CreatedToken,
    DecideApprovalResult,
    Status,
    StreamEvent,
    Task,
    TaskDetail,
    TokenInfo,
    TrainingExportLine,
    TrainingStats,
)

Mode = Literal["chat", "agent"]


class Client:
    """Client for a ContextGrip AI Chat server.

    Usage::

        from contextgrip_ai_chat import Client

        with Client(base_url="http://localhost:8080", token="...") as client:
            response = client.ask("How many orders shipped last week?")
            print(response.sql)
            print(response.answer)

    The client is a context manager; ``close()`` releases the underlying
    ``httpx.Client``. When ``token`` is set, every request carries
    ``Authorization: Bearer <token>``.
    """

    def __init__(
        self,
        base_url: str,
        token: str | None = None,
        timeout: float = 600.0,
    ) -> None:
        headers: dict[str, str] = {}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self._http = httpx.Client(base_url=base_url, timeout=timeout, headers=headers)

    # --- lifecycle ----------------------------------------------------------

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()

    # --- system -------------------------------------------------------------

    def status(self) -> Status:
        """Service status for authenticated callers."""
        return Status.from_dict(self._request_json("GET", "/api/v1/status"))

    # --- chat ---------------------------------------------------------------

    def ask(
        self,
        question: str,
        conversation_id: str | None = None,
        mode: Mode = "chat",
    ) -> AskResponse:
        """One-shot question; blocks until the full answer is ready.

        A failed query execution is not an HTTP error: the returned
        ``AskResponse`` carries ``result_error`` and an ``answer``
        explaining the failure.

        ``mode="agent"`` (requires the "agent" feature) lets the model take
        multiple tool steps: the response may carry ``steps`` instead of the
        sql/result pair, and ``pending_approval_id`` when a proposed write
        awaits a decision. A conversation keeps the mode of its first
        message.
        """
        payload = self._request_json(
            "POST",
            "/api/v1/ask",
            json_body=_ask_body(question, conversation_id, mode),
        )
        return AskResponse.from_dict(payload)

    def stream_message(
        self,
        question: str,
        conversation_id: str | None = None,
        mode: Mode = "chat",
    ) -> Iterator[StreamEvent]:
        """Ask a question and stream the answer as typed SSE events.

        Chat mode yields ``MetaEvent -> SqlEvent -> ResultEvent ->
        DeltaEvent* -> DoneEvent``, or a terminal ``ErrorEvent`` at any
        point after the stream has started (yielded, not raised). In agent
        mode ``StepEvent`` and ``ApprovalRequiredEvent`` are interleaved
        before the delta stream, the sql/result pair may be absent, and
        ``DoneEvent.pending_approval_id`` is set when the turn ended on a
        proposed write. Pre-stream failures (validation, auth, unknown
        conversation) raise :class:`AiChatError`. Malformed or unknown
        frames are skipped.
        """
        with self._http.stream(
            "POST",
            "/api/v1/messages",
            json=_ask_body(question, conversation_id, mode),
            headers={"Accept": "text/event-stream"},
        ) as response:
            if not response.is_success:
                response.read()
                raise self._error(response)
            for event_name, data in iter_sse_frames(response.iter_lines()):
                event = parse_stream_event(event_name, data)
                if event is not None:
                    yield event

    # --- conversations ------------------------------------------------------

    def list_conversations(self) -> list[Conversation]:
        """List conversations, most recently updated first."""
        payload = self._request_json("GET", "/api/v1/conversations")
        return [Conversation.from_dict(item) for item in payload]

    def get_conversation(self, conversation_id: str) -> ConversationDetail:
        """Fetch one conversation with its messages in order."""
        payload = self._request_json(
            "GET", f"/api/v1/conversations/{quote(conversation_id, safe='')}"
        )
        return ConversationDetail.from_dict(payload)

    def delete_conversation(self, conversation_id: str) -> None:
        """Delete a conversation and its messages."""
        self._request_json(
            "DELETE", f"/api/v1/conversations/{quote(conversation_id, safe='')}"
        )

    # --- approvals ------------------------------------------------------------

    def list_approvals(self) -> list[Approval]:
        """List pending write approvals (chat and board sources)."""
        payload = self._request_json("GET", "/api/v1/approvals")
        return [Approval.from_dict(item) for item in payload]

    def decide_approval(
        self, approval_id: str, decision: Literal["approve", "reject"]
    ) -> DecideApprovalResult:
        """Approve or reject a proposed write.

        Approving executes the exact proposed SQL against the write
        connection and the returned ``result``/``error`` carry the
        execution outcome. Deciding an already-decided approval raises
        :class:`AiChatError` with code ``ALREADY_DECIDED``; approving
        without a configured write connection raises ``WRITES_DISABLED``
        (both 409).
        """
        payload = self._request_json(
            "POST",
            f"/api/v1/approvals/{quote(approval_id, safe='')}",
            json_body={"decision": decision},
        )
        return DecideApprovalResult.from_dict(payload)

    # --- tasks ------------------------------------------------------------------

    def create_task(self, title: str, prompt: str) -> Task:
        """File a board task for the agent (requires the "board" feature)."""
        payload = self._request_json(
            "POST", "/api/v1/tasks", json_body={"title": title, "prompt": prompt}
        )
        return Task.from_dict(payload)

    def list_tasks(self, status: str | None = None) -> list[Task]:
        """List board tasks, most recently updated first.

        ``status`` filters to one of: queued, running, needs_approval,
        done, failed, canceled.
        """
        params = {"status": status} if status is not None else None
        payload = self._request_json("GET", "/api/v1/tasks", params=params)
        return [Task.from_dict(item) for item in payload]

    def get_task(self, task_id: str) -> TaskDetail:
        """Task detail: status, transcript steps, answer, pending approval."""
        payload = self._request_json(
            "GET", f"/api/v1/tasks/{quote(task_id, safe='')}"
        )
        return TaskDetail.from_dict(payload)

    def cancel_task(self, task_id: str) -> Task:
        """Cancel a queued, running, or approval-blocked task.

        Canceling a finished task raises :class:`AiChatError` with code
        ``TASK_FINISHED`` (409).
        """
        payload = self._request_json(
            "POST", f"/api/v1/tasks/{quote(task_id, safe='')}/cancel"
        )
        return Task.from_dict(payload)

    def delete_task(self, task_id: str) -> None:
        """Delete a finished task (done/failed/canceled only).

        Deleting an active task raises :class:`AiChatError` with code
        ``TASK_ACTIVE`` (409).
        """
        self._request_json("DELETE", f"/api/v1/tasks/{quote(task_id, safe='')}")

    # --- training data --------------------------------------------------------

    def rate_message(
        self, message_id: str, verdict: Literal["good", "bad"]
    ) -> None:
        """Rate an assistant answer; writes a training record.

        Explicit evals bypass the capture toggle. Upserts by message id:
        rating the same answer again updates the verdict. Only assistant
        messages that carry SQL can be rated.
        """
        self._request_json(
            "POST",
            f"/api/v1/messages/{quote(message_id, safe='')}/eval",
            json_body={"verdict": verdict},
        )

    def get_training_capture(self) -> bool:
        """Read the automatic-capture setting."""
        payload = self._request_json("GET", "/api/v1/training/capture")
        return bool(payload["enabled"])

    def set_training_capture(self, enabled: bool) -> bool:
        """Enable/disable automatic capture (admin). Returns the new setting."""
        payload = self._request_json(
            "PUT", "/api/v1/training/capture", json_body={"enabled": enabled}
        )
        return bool(payload["enabled"])

    def training_stats(self) -> TrainingStats:
        """Training-record counts and capture range."""
        return TrainingStats.from_dict(
            self._request_json("GET", "/api/v1/training/stats")
        )

    def iter_training_export(
        self, include_rows: bool = True, evaluated_only: bool = False
    ) -> Iterator[TrainingExportLine]:
        """Stream the JSONL training-data export, one parsed line per record.

        The server stops the stream at a 64 MiB byte budget; compare the
        number of yielded lines with :meth:`training_stats` to detect
        truncation. Blank lines are skipped.
        """
        params = {
            "includeRows": include_rows,
            "evaluatedOnly": evaluated_only,
        }
        with self._http.stream(
            "GET", "/api/v1/training/export", params=params
        ) as response:
            if not response.is_success:
                response.read()
                raise self._error(response)
            for line in response.iter_lines():
                if not line.strip():
                    continue
                yield TrainingExportLine.from_dict(json.loads(line))

    def delete_training_records(self) -> int:
        """Delete ALL training records (admin). Returns the number removed."""
        payload = self._request_json("DELETE", "/api/v1/training/records")
        return int(payload.get("deleted", 0))

    # --- tokens (admin: primary APP_ACCESS_TOKEN only) -----------------------

    def list_tokens(self) -> list[TokenInfo]:
        """List named API tokens (admin)."""
        payload = self._request_json("GET", "/api/v1/tokens")
        return [TokenInfo.from_dict(item) for item in payload]

    def create_token(self, label: str) -> CreatedToken:
        """Mint a named API token (admin). The raw value is shown only once."""
        payload = self._request_json(
            "POST", "/api/v1/tokens", json_body={"label": label}
        )
        return CreatedToken.from_dict(payload)

    def revoke_token(self, token_id: str) -> None:
        """Revoke a named API token (admin)."""
        self._request_json("DELETE", f"/api/v1/tokens/{quote(token_id, safe='')}")

    # --- internals ------------------------------------------------------------

    def _request_json(
        self,
        method: str,
        path: str,
        json_body: dict[str, Any] | None = None,
        params: dict[str, Any] | None = None,
    ) -> Any:
        response = self._http.request(method, path, json=json_body, params=params)
        if not response.is_success:
            raise self._error(response)
        return response.json()

    @staticmethod
    def _error(response: httpx.Response) -> AiChatError:
        code: str | None = None
        message = ""
        try:
            payload = response.json()
        except (json.JSONDecodeError, ValueError):
            payload = None
        if isinstance(payload, dict):
            error_value = payload.get("error")
            if isinstance(error_value, str):
                message = error_value
            code_value = payload.get("code")
            if isinstance(code_value, str):
                code = code_value
        if not message:
            message = response.text.strip() or f"HTTP {response.status_code}"
        return AiChatError(response.status_code, code, message)


def _ask_body(
    question: str, conversation_id: str | None, mode: Mode = "chat"
) -> dict[str, Any]:
    body: dict[str, Any] = {"question": question}
    if conversation_id is not None:
        body["conversationId"] = conversation_id
    if mode != "chat":  # chat is the server default; keep the wire minimal
        body["mode"] = mode
    return body
