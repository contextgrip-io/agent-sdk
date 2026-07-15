"""Minimal Server-Sent Events parsing for the /api/v1/messages stream.

Frames are separated by a blank line. Within a frame, multiple ``data:``
lines are joined with ``"\\n"`` before JSON parsing, per the SSE spec.
Malformed frames (invalid JSON, missing required keys, unknown event names)
are skipped rather than raised.
"""

from __future__ import annotations

import json
from collections.abc import Iterable, Iterator
from typing import Any

from .models import (
    Approval,
    ApprovalRequiredEvent,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    MetaEvent,
    ResultEvent,
    ResultSummary,
    SqlEvent,
    Step,
    StepEvent,
    StreamEvent,
)


def iter_sse_frames(lines: Iterable[str]) -> Iterator[tuple[str, str]]:
    """Yield ``(event_name, data)`` per SSE frame.

    ``lines`` must be an iterable of lines with line endings already
    stripped (e.g. ``httpx.Response.iter_lines()``). Multi-line ``data:``
    fields are joined with a newline. Comment lines and unknown fields
    (``id:``, ``retry:``) are ignored. A frame with no data is skipped.
    """
    event_name = ""
    data_lines: list[str] = []
    for line in lines:
        if line == "":
            if data_lines:
                yield event_name or "message", "\n".join(data_lines)
            event_name = ""
            data_lines = []
            continue
        if line.startswith(":"):
            continue  # comment
        name, sep, value = line.partition(":")
        if sep and value.startswith(" "):
            value = value[1:]
        if name == "event":
            event_name = value
        elif name == "data":
            data_lines.append(value)
        # id, retry, anything else: ignored
    if data_lines:  # stream ended without a trailing blank line
        yield event_name or "message", "\n".join(data_lines)


def parse_stream_event(event_name: str, data: str) -> StreamEvent | None:
    """Convert one SSE frame into a typed StreamEvent.

    Returns ``None`` for frames that should be skipped: unknown event
    names, non-JSON data, non-object payloads, or payloads missing the
    fields the event requires.
    """
    try:
        payload: Any = json.loads(data)
    except json.JSONDecodeError:
        return None
    if not isinstance(payload, dict):
        return None
    try:
        if event_name == "meta":
            return MetaEvent(
                conversation_id=payload["conversationId"],
                user_message_id=payload["userMessageId"],
            )
        if event_name == "sql":
            return SqlEvent(sql=payload["sql"])
        if event_name == "result":
            if "error" in payload:
                return ResultEvent(
                    error=payload["error"],
                    execution_time_ms=payload.get("executionTimeMs"),
                )
            summary = ResultSummary.from_dict(payload)
            return ResultEvent(result=summary, execution_time_ms=summary.execution_time_ms)
        if event_name == "step":
            return StepEvent(step=Step.from_dict(payload))
        if event_name == "approval_required":
            return ApprovalRequiredEvent(approval=Approval.from_dict(payload))
        if event_name == "delta":
            return DeltaEvent(text=payload["text"])
        if event_name == "done":
            return DoneEvent(
                conversation_id=payload["conversationId"],
                assistant_message_id=payload["assistantMessageId"],
                pending_approval_id=payload.get("pendingApprovalId"),
            )
        if event_name == "error":
            return ErrorEvent(message=payload["message"])
    except (KeyError, TypeError):
        return None
    return None
