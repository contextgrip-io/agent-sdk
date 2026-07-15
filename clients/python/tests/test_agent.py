"""Agent-mode, approvals, and board-task tests against the threaded stub."""

from __future__ import annotations

import pytest

from contextgrip_ai_chat import (
    AiChatError,
    ApprovalRequiredEvent,
    ApprovalSource,
    AskResponse,
    Client,
    DecideApprovalResult,
    DeltaEvent,
    DoneEvent,
    MetaEvent,
    StepEvent,
    Task,
    TaskDetail,
)

from .stub_server import StubServer

APPROVAL_PENDING = {
    "id": "appr-1",
    "sql": "UPDATE orders SET status = 'shipped' WHERE id = 7",
    "rationale": "Mark order 7 shipped as asked.",
    "status": "pending",
    "source": {"conversationId": "conv-1", "messageId": "msg-a1"},
    "createdAt": "2026-07-15T12:00:00Z",
}

TASK_QUEUED = {
    "id": "task-1",
    "title": "Weekly cleanup",
    "prompt": "Archive orders older than a year",
    "status": "queued",
    "createdAt": "2026-07-15T12:00:00Z",
    "updatedAt": "2026-07-15T12:00:00Z",
}

AGENT_STREAM = (
    b'event: meta\n'
    b'data: {"conversationId":"conv-1","userMessageId":"msg-u1"}\n'
    b'\n'
    b'event: step\n'
    b'data: {"index":0,"kind":"schema","summary":"Inspected orders schema"}\n'
    b'\n'
    b'event: step\n'
    b'data: {"index":1,"kind":"query","summary":"Checked order 7",'
    b'"sql":"SELECT status FROM orders WHERE id = 7",'
    b'"result":{"columns":["status"],"rowSample":[["pending"]],'
    b'"rowCount":1,"truncated":false,"executionTimeMs":4}}\n'
    b'\n'
    b'event: approval_required\n'
    b'data: {"id":"appr-1","sql":"UPDATE orders SET status = \'shipped\' WHERE id = 7",'
    b'"rationale":"Mark order 7 shipped as asked.","status":"pending",'
    b'"source":{"conversationId":"conv-1","messageId":"msg-a1"},'
    b'"createdAt":"2026-07-15T12:00:00Z"}\n'
    b'\n'
    b'event: delta\n'
    b'data: {"text":"I need approval to run this write."}\n'
    b'\n'
    b'event: done\n'
    b'data: {"conversationId":"conv-1","assistantMessageId":"msg-a1",'
    b'"pendingApprovalId":"appr-1"}\n'
    b'\n'
)


def _chunk(raw: bytes, size: int) -> list[bytes]:
    return [raw[i : i + size] for i in range(0, len(raw), size)]


class TestAgentStream:
    def test_agent_mode_sequence_with_steps_and_approval(
        self, server: StubServer, client: Client
    ) -> None:
        server.sse("POST", "/api/v1/messages", _chunk(AGENT_STREAM, 7))

        events = list(client.stream_message("Ship order 7", mode="agent"))

        assert [type(e) for e in events] == [
            MetaEvent,
            StepEvent,
            StepEvent,
            ApprovalRequiredEvent,
            DeltaEvent,
            DoneEvent,
        ]
        _, schema_step, query_step, approval_event, _, done = events

        assert isinstance(schema_step, StepEvent)
        assert schema_step.step.index == 0
        assert schema_step.step.kind == "schema"
        assert schema_step.step.summary == "Inspected orders schema"
        assert schema_step.step.sql is None
        assert schema_step.step.result is None

        assert isinstance(query_step, StepEvent)
        assert query_step.step.index == 1
        assert query_step.step.kind == "query"
        assert query_step.step.sql == "SELECT status FROM orders WHERE id = 7"
        assert query_step.step.result is not None
        assert query_step.step.result.row_sample == [["pending"]]
        assert query_step.step.error is None

        assert isinstance(approval_event, ApprovalRequiredEvent)
        approval = approval_event.approval
        assert approval.id == "appr-1"
        assert approval.status == "pending"
        assert approval.sql == "UPDATE orders SET status = 'shipped' WHERE id = 7"
        assert approval.rationale == "Mark order 7 shipped as asked."
        assert approval.source == ApprovalSource(
            conversation_id="conv-1", message_id="msg-a1"
        )
        assert approval.decided_at is None

        assert isinstance(done, DoneEvent)
        assert done.pending_approval_id == "appr-1"

        # mode travels on the wire
        assert server.last_request().json == {
            "question": "Ship order 7",
            "mode": "agent",
        }

    def test_chat_mode_omits_mode_and_done_has_no_pending_approval(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            b'event: meta\n'
            b'data: {"conversationId":"c","userMessageId":"u"}\n\n'
            b'event: done\n'
            b'data: {"conversationId":"c","assistantMessageId":"a"}\n\n'
        )
        server.sse("POST", "/api/v1/messages", [raw])

        events = list(client.stream_message("How many orders?"))

        done = events[-1]
        assert isinstance(done, DoneEvent)
        assert done.pending_approval_id is None
        assert server.last_request().json == {"question": "How many orders?"}


class TestAgentAsk:
    def test_agent_mode_response_with_steps_and_pending_approval(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/ask",
            {
                "conversationId": "conv-1",
                "userMessageId": "msg-u1",
                "assistantMessageId": "msg-a1",
                "answer": "I need approval to run this write.",
                "steps": [
                    {"index": 0, "kind": "note", "summary": "Planned the change"},
                    {
                        "index": 1,
                        "kind": "query",
                        "summary": "Checked order 7",
                        "sql": "SELECT status FROM orders WHERE id = 7",
                        "error": "statement timeout",
                    },
                ],
                "pendingApprovalId": "appr-1",
            },
        )

        response = client.ask("Ship order 7", mode="agent")

        assert isinstance(response, AskResponse)
        # agent mode: no top-level sql/result pair
        assert response.sql is None
        assert response.result is None
        assert response.result_error is None
        assert response.steps is not None
        assert [s.kind for s in response.steps] == ["note", "query"]
        assert response.steps[1].error == "statement timeout"
        assert response.pending_approval_id == "appr-1"

        assert server.last_request().json == {
            "question": "Ship order 7",
            "mode": "agent",
        }


class TestApprovals:
    def test_list(self, server: StubServer, client: Client) -> None:
        board_approval = {
            "id": "appr-2",
            "sql": "DELETE FROM stale_rows",
            "status": "pending",
            "source": {"taskId": "task-1"},
            "createdAt": "2026-07-15T13:00:00Z",
        }
        server.json("GET", "/api/v1/approvals", [APPROVAL_PENDING, board_approval])

        approvals = client.list_approvals()

        assert [a.id for a in approvals] == ["appr-1", "appr-2"]
        assert approvals[0].source.conversation_id == "conv-1"
        assert approvals[0].source.task_id is None
        assert approvals[1].source == ApprovalSource(task_id="task-1")
        assert approvals[1].rationale is None

    def test_approve_executes_and_returns_result(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/approvals/appr-1",
            {
                "approval": {
                    **APPROVAL_PENDING,
                    "status": "approved",
                    "decidedAt": "2026-07-15T12:05:00Z",
                },
                "result": {
                    "rowCount": 1,
                    "truncated": False,
                    "executionTimeMs": 8,
                },
            },
        )

        outcome = client.decide_approval("appr-1", "approve")

        assert isinstance(outcome, DecideApprovalResult)
        assert outcome.approval.status == "approved"
        assert outcome.approval.decided_at == "2026-07-15T12:05:00Z"
        assert outcome.result is not None
        assert outcome.result.row_count == 1
        assert outcome.error is None

        request = server.last_request()
        assert request.method == "POST"
        assert request.json == {"decision": "approve"}

    def test_approved_write_failure_carries_error(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/approvals/appr-1",
            {
                "approval": {**APPROVAL_PENDING, "status": "approved"},
                "error": "permission denied for table orders",
            },
        )

        outcome = client.decide_approval("appr-1", "approve")

        assert outcome.result is None
        assert outcome.error == "permission denied for table orders"

    def test_reject(self, server: StubServer, client: Client) -> None:
        server.json(
            "POST",
            "/api/v1/approvals/appr-1",
            {"approval": {**APPROVAL_PENDING, "status": "rejected"}},
        )

        outcome = client.decide_approval("appr-1", "reject")

        assert outcome.approval.status == "rejected"
        assert outcome.result is None and outcome.error is None
        assert server.last_request().json == {"decision": "reject"}

    def test_already_decided_maps_to_409(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/approvals/appr-1",
            {"error": "approval already decided", "code": "ALREADY_DECIDED"},
            status=409,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.decide_approval("appr-1", "approve")

        assert excinfo.value.status == 409
        assert excinfo.value.code == "ALREADY_DECIDED"

    def test_writes_disabled_maps_to_409(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/approvals/appr-1",
            {"error": "no write connection configured", "code": "WRITES_DISABLED"},
            status=409,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.decide_approval("appr-1", "approve")

        assert excinfo.value.status == 409
        assert excinfo.value.code == "WRITES_DISABLED"


class TestTasks:
    def test_create(self, server: StubServer, client: Client) -> None:
        server.json("POST", "/api/v1/tasks", TASK_QUEUED, status=201)

        task = client.create_task("Weekly cleanup", "Archive orders older than a year")

        assert isinstance(task, Task)
        assert task.id == "task-1"
        assert task.status == "queued"
        assert task.answer is None and task.error is None
        assert server.last_request().json == {
            "title": "Weekly cleanup",
            "prompt": "Archive orders older than a year",
        }

    def test_list_with_status_filter(self, server: StubServer, client: Client) -> None:
        done_task = {
            **TASK_QUEUED,
            "id": "task-2",
            "status": "done",
            "answer": "Archived 12 orders.",
        }
        server.json("GET", "/api/v1/tasks", [done_task])

        tasks = client.list_tasks(status="done")

        assert [t.id for t in tasks] == ["task-2"]
        assert tasks[0].answer == "Archived 12 orders."
        assert server.last_request().query == {"status": ["done"]}

    def test_list_without_filter_sends_no_params(
        self, server: StubServer, client: Client
    ) -> None:
        server.json("GET", "/api/v1/tasks", [])

        assert client.list_tasks() == []
        assert server.last_request().query == {}

    def test_get_detail(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/tasks/task-1",
            {
                "task": {**TASK_QUEUED, "status": "needs_approval"},
                "steps": [
                    {
                        "index": 0,
                        "kind": "query",
                        "summary": "Counted stale orders",
                        "sql": "SELECT count(*) FROM orders WHERE created_at < now() - interval '1 year'",
                        "result": {
                            "rowCount": 1,
                            "truncated": False,
                            "executionTimeMs": 6,
                        },
                    }
                ],
                "pendingApproval": {**APPROVAL_PENDING, "source": {"taskId": "task-1"}},
            },
        )

        detail = client.get_task("task-1")

        assert isinstance(detail, TaskDetail)
        assert detail.task.status == "needs_approval"
        assert len(detail.steps) == 1
        assert detail.steps[0].kind == "query"
        assert detail.pending_approval is not None
        assert detail.pending_approval.source.task_id == "task-1"

    def test_cancel(self, server: StubServer, client: Client) -> None:
        server.json(
            "POST", "/api/v1/tasks/task-1/cancel", {**TASK_QUEUED, "status": "canceled"}
        )

        task = client.cancel_task("task-1")

        assert task.status == "canceled"
        request = server.last_request()
        assert request.method == "POST"
        assert request.path == "/api/v1/tasks/task-1/cancel"

    def test_cancel_finished_maps_to_409(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/tasks/task-1/cancel",
            {"error": "task already finished", "code": "TASK_FINISHED"},
            status=409,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.cancel_task("task-1")

        assert excinfo.value.status == 409
        assert excinfo.value.code == "TASK_FINISHED"

    def test_delete(self, server: StubServer, client: Client) -> None:
        server.json("DELETE", "/api/v1/tasks/task-1", {"deleted": True})

        assert client.delete_task("task-1") is None

        request = server.last_request()
        assert request.method == "DELETE"
        assert request.path == "/api/v1/tasks/task-1"

    def test_delete_active_maps_to_409_task_active(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "DELETE",
            "/api/v1/tasks/task-1",
            {"error": "task is still running", "code": "TASK_ACTIVE"},
            status=409,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.delete_task("task-1")

        assert excinfo.value.status == 409
        assert excinfo.value.code == "TASK_ACTIVE"
