"""Training-data endpoint tests: eval, capture toggle, stats, export, delete."""

from __future__ import annotations

import json

import pytest

from contextgrip_ai_chat import (
    AiChatError,
    Client,
    ExportEval,
    TrainingExportLine,
    TrainingStats,
)

from .stub_server import StubServer

EXPORT_LINE_FULL = {
    "id": "rec-1",
    "capturedAt": "2026-07-15T10:00:00Z",
    "connection": {"id": "a1b2c3d4", "name": "orders_db", "engine": "postgresql"},
    "context": {"session": "chat", "sourceMessageId": "msg-a1"},
    "query": {
        "sql": "SELECT count(*) FROM orders",
        "intent": "How many orders?",
    },
    "response": {
        "columns": ["count"],
        "rowCount": 1,
        "truncated": False,
        "executionTimeMs": 12,
        "rowSample": [[42]],
    },
    "eval": {"verdict": "good"},
}

EXPORT_LINE_MINIMAL = {
    "id": "rec-2",
    "capturedAt": "2026-07-15T11:00:00Z",
    "connection": {"id": "a1b2c3d4", "name": "orders_db", "engine": "postgresql"},
    "context": {},
    "query": {"sql": "SELECT * FROM missing"},
    "response": {
        "rowCount": 0,
        "truncated": False,
        "executionTimeMs": 3,
        "error": 'relation "missing" does not exist',
    },
}


def _chunk(raw: bytes, size: int) -> list[bytes]:
    return [raw[i : i + size] for i in range(0, len(raw), size)]


class TestRateMessage:
    def test_rate_good(self, server: StubServer, client: Client) -> None:
        server.json("POST", "/api/v1/messages/msg-a1/eval", {"recorded": True})

        assert client.rate_message("msg-a1", "good") is None

        request = server.last_request()
        assert request.method == "POST"
        assert request.path == "/api/v1/messages/msg-a1/eval"
        assert request.json == {"verdict": "good"}
        assert request.headers["Authorization"] == "Bearer test-token"

    def test_rate_bad(self, server: StubServer, client: Client) -> None:
        server.json("POST", "/api/v1/messages/msg-a2/eval", {"recorded": True})

        client.rate_message("msg-a2", "bad")

        assert server.last_request().json == {"verdict": "bad"}

    def test_unknown_message_raises_not_found(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/messages/nope/eval",
            {"error": "message not found", "code": "NOT_FOUND"},
            status=404,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.rate_message("nope", "good")

        assert excinfo.value.status == 404
        assert excinfo.value.code == "NOT_FOUND"


class TestTrainingCapture:
    def test_get(self, server: StubServer, client: Client) -> None:
        server.json("GET", "/api/v1/training/capture", {"enabled": False})

        assert client.get_training_capture() is False
        assert server.last_request().method == "GET"

    def test_set(self, server: StubServer, client: Client) -> None:
        server.json("PUT", "/api/v1/training/capture", {"enabled": True})

        assert client.set_training_capture(True) is True

        request = server.last_request()
        assert request.method == "PUT"
        assert request.json == {"enabled": True}

    def test_set_requires_admin(self, server: StubServer, client: Client) -> None:
        server.json(
            "PUT",
            "/api/v1/training/capture",
            {"error": "named tokens cannot change capture", "code": "ADMIN_REQUIRED"},
            status=403,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.set_training_capture(False)

        assert excinfo.value.status == 403
        assert excinfo.value.code == "ADMIN_REQUIRED"


class TestTrainingStats:
    def test_full(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/training/stats",
            {
                "records": 120,
                "evaluated": 17,
                "firstCapturedAt": "2026-07-01T00:00:00Z",
                "lastCapturedAt": "2026-07-15T11:00:00Z",
            },
        )

        stats = client.training_stats()

        assert isinstance(stats, TrainingStats)
        assert stats.records == 120
        assert stats.evaluated == 17
        assert stats.first_captured_at == "2026-07-01T00:00:00Z"
        assert stats.last_captured_at == "2026-07-15T11:00:00Z"

    def test_empty_store_has_no_capture_range(
        self, server: StubServer, client: Client
    ) -> None:
        server.json("GET", "/api/v1/training/stats", {"records": 0, "evaluated": 0})

        stats = client.training_stats()

        assert stats == TrainingStats(records=0, evaluated=0)


class TestTrainingExport:
    def test_iterates_chunked_ndjson_with_mid_json_split(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            json.dumps(EXPORT_LINE_FULL).encode()
            + b"\n"
            + b"\n"  # blank line: skipped
            + json.dumps(EXPORT_LINE_MINIMAL).encode()
            + b"\n"
        )
        # 13-byte chunks guarantee boundaries land mid-JSON-token.
        server.ndjson("GET", "/api/v1/training/export", _chunk(raw, 13))

        lines = list(client.iter_training_export())

        assert len(lines) == 2
        assert all(isinstance(line, TrainingExportLine) for line in lines)

        full, minimal = lines
        assert full.id == "rec-1"
        assert full.captured_at == "2026-07-15T10:00:00Z"
        assert full.connection.name == "orders_db"
        assert full.connection.engine == "postgresql"
        assert full.context.session == "chat"
        assert full.context.source_message_id == "msg-a1"
        assert full.query.sql == "SELECT count(*) FROM orders"
        assert full.query.intent == "How many orders?"
        assert full.response.row_count == 1
        assert full.response.row_sample == [[42]]
        assert full.response.error is None
        assert full.eval == ExportEval(verdict="good")

        assert minimal.id == "rec-2"
        assert minimal.context.session is None
        assert minimal.query.intent is None
        assert minimal.response.error == 'relation "missing" does not exist'
        assert minimal.response.row_sample is None
        assert minimal.eval is None

        # default query params
        assert server.last_request().query == {
            "includeRows": ["true"],
            "evaluatedOnly": ["false"],
        }

    def test_query_params_forwarded(self, server: StubServer, client: Client) -> None:
        raw = json.dumps(EXPORT_LINE_FULL).encode() + b"\n"
        server.ndjson("GET", "/api/v1/training/export", [raw])

        lines = list(
            client.iter_training_export(include_rows=False, evaluated_only=True)
        )

        assert len(lines) == 1
        assert server.last_request().query == {
            "includeRows": ["false"],
            "evaluatedOnly": ["true"],
        }

    def test_empty_export(self, server: StubServer, client: Client) -> None:
        server.ndjson("GET", "/api/v1/training/export", [b"\n"])

        assert list(client.iter_training_export()) == []

    def test_final_line_without_trailing_newline(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            json.dumps(EXPORT_LINE_FULL).encode()
            + b"\n"
            + json.dumps(EXPORT_LINE_MINIMAL).encode()  # no trailing \n
        )
        server.ndjson("GET", "/api/v1/training/export", _chunk(raw, 17))

        lines = list(client.iter_training_export())

        assert [line.id for line in lines] == ["rec-1", "rec-2"]

    def test_pre_stream_401_raises(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/training/export",
            {"error": "missing or invalid bearer token", "code": "UNAUTHORIZED"},
            status=401,
        )

        with pytest.raises(AiChatError) as excinfo:
            list(client.iter_training_export())

        assert excinfo.value.status == 401
        assert excinfo.value.code == "UNAUTHORIZED"


class TestDeleteTrainingRecords:
    def test_delete_returns_count(self, server: StubServer, client: Client) -> None:
        server.json("DELETE", "/api/v1/training/records", {"deleted": 120})

        assert client.delete_training_records() == 120

        request = server.last_request()
        assert request.method == "DELETE"
        assert request.path == "/api/v1/training/records"

    def test_delete_requires_admin(self, server: StubServer, client: Client) -> None:
        server.json(
            "DELETE",
            "/api/v1/training/records",
            {"error": "named tokens cannot manage training data", "code": "ADMIN_REQUIRED"},
            status=403,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.delete_training_records()

        assert excinfo.value.status == 403
        assert excinfo.value.code == "ADMIN_REQUIRED"
