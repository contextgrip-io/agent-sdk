"""Streaming tests: full SSE sequences over a real socket, chunked delivery."""

from __future__ import annotations

import pytest

from contextgrip_ai_chat import (
    AiChatError,
    Client,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    MetaEvent,
    ResultEvent,
    SqlEvent,
)

from .stub_server import StubServer

FULL_STREAM = (
    b'event: meta\n'
    b'data: {"conversationId":"conv-1","userMessageId":"msg-u1"}\n'
    b'\n'
    b'event: sql\n'
    b'data: {"sql":"SELECT count(*) FROM orders"}\n'
    b'\n'
    b'event: result\n'
    b'data: {"columns":["count"],"rowSample":[[42]],"rowCount":1,'
    b'"truncated":false,"executionTimeMs":12}\n'
    b'\n'
    # multi-line data frame: JSON split across two data: lines, joined with \n
    b'event: delta\n'
    b'data: {"text":\n'
    b'data:  "There are"}\n'
    b'\n'
    b'event: delta\n'
    b'data: {"text": " 42 orders."}\n'
    b'\n'
    # unknown event name: skipped
    b'event: bogus\n'
    b'data: {"whatever":1}\n'
    b'\n'
    # malformed JSON on a known event: skipped
    b'event: delta\n'
    b'data: {not json!!\n'
    b'\n'
    # SSE comment and ignored fields
    b': keep-alive comment\n'
    b'id: 7\n'
    b'retry: 1000\n'
    b'\n'
    b'event: done\n'
    b'data: {"conversationId":"conv-1","assistantMessageId":"msg-a1"}\n'
    b'\n'
)


def _chunk(raw: bytes, size: int) -> list[bytes]:
    return [raw[i : i + size] for i in range(0, len(raw), size)]


class TestStreamMessage:
    def test_full_sequence_with_chunked_delivery(
        self, server: StubServer, client: Client
    ) -> None:
        # 7-byte chunks split lines and even UTF-8 frames mid-token.
        server.sse("POST", "/api/v1/messages", _chunk(FULL_STREAM, 7))

        events = list(client.stream_message("How many orders?"))

        assert [type(e) for e in events] == [
            MetaEvent,
            SqlEvent,
            ResultEvent,
            DeltaEvent,
            DeltaEvent,
            DoneEvent,
        ]
        meta, sql, result, delta1, delta2, done = events
        assert meta == MetaEvent(conversation_id="conv-1", user_message_id="msg-u1")
        assert sql == SqlEvent(sql="SELECT count(*) FROM orders")
        assert isinstance(result, ResultEvent)
        assert result.error is None
        assert result.result is not None
        assert result.result.row_count == 1
        assert result.result.columns == ["count"]
        assert result.execution_time_ms == 12
        # multi-line data frame parsed as one JSON document
        assert delta1 == DeltaEvent(text="There are")
        assert delta2 == DeltaEvent(text=" 42 orders.")
        assert done == DoneEvent(
            conversation_id="conv-1", assistant_message_id="msg-a1"
        )

        request = server.last_request()
        assert request.json == {"question": "How many orders?"}
        assert request.headers["Authorization"] == "Bearer test-token"
        assert request.headers["Accept"] == "text/event-stream"

    def test_conversation_id_forwarded(
        self, server: StubServer, client: Client
    ) -> None:
        server.sse("POST", "/api/v1/messages", _chunk(FULL_STREAM, 64))

        list(client.stream_message("Follow-up?", conversation_id="conv-1"))

        assert server.last_request().json == {
            "question": "Follow-up?",
            "conversationId": "conv-1",
        }

    def test_result_execution_failure_frame(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            b'event: meta\n'
            b'data: {"conversationId":"c","userMessageId":"u"}\n\n'
            b'event: sql\n'
            b'data: {"sql":"SELECT * FROM missing"}\n\n'
            b'event: result\n'
            b'data: {"error":"relation \\"missing\\" does not exist",'
            b'"executionTimeMs":3}\n\n'
            b'event: delta\n'
            b'data: {"text":"That table does not exist."}\n\n'
            b'event: done\n'
            b'data: {"conversationId":"c","assistantMessageId":"a"}\n\n'
        )
        server.sse("POST", "/api/v1/messages", _chunk(raw, 11))

        events = list(client.stream_message("Query the missing table"))

        result = next(e for e in events if isinstance(e, ResultEvent))
        assert result.result is None
        assert result.error == 'relation "missing" does not exist'
        assert result.execution_time_ms == 3
        assert isinstance(events[-1], DoneEvent)

    def test_terminal_error_event_is_yielded_not_raised(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            b'event: meta\n'
            b'data: {"conversationId":"c","userMessageId":"u"}\n\n'
            b'event: error\n'
            b'data: {"message":"model overloaded"}\n\n'
        )
        server.sse("POST", "/api/v1/messages", _chunk(raw, 9))

        events = list(client.stream_message("Anything?"))

        assert [type(e) for e in events] == [MetaEvent, ErrorEvent]
        assert events[-1] == ErrorEvent(message="model overloaded")

    def test_stream_without_trailing_blank_line_still_yields_last_frame(
        self, server: StubServer, client: Client
    ) -> None:
        raw = (
            b'event: meta\n'
            b'data: {"conversationId":"c","userMessageId":"u"}\n\n'
            b'event: error\n'
            b'data: {"message":"connection torn down"}\n'  # no trailing blank line
        )
        server.sse("POST", "/api/v1/messages", [raw])

        events = list(client.stream_message("Anything?"))

        assert events[-1] == ErrorEvent(message="connection torn down")

    def test_pre_stream_401_raises(self, server: StubServer, client: Client) -> None:
        server.json(
            "POST",
            "/api/v1/messages",
            {"error": "missing or invalid bearer token", "code": "UNAUTHORIZED"},
            status=401,
        )

        with pytest.raises(AiChatError) as excinfo:
            list(client.stream_message("Who am I?"))

        assert excinfo.value.status == 401
        assert excinfo.value.code == "UNAUTHORIZED"
        assert excinfo.value.message == "missing or invalid bearer token"

    def test_pre_stream_404_unknown_conversation_raises(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/messages",
            {"error": "conversation not found", "code": "NOT_FOUND"},
            status=404,
        )

        with pytest.raises(AiChatError) as excinfo:
            list(client.stream_message("Hi", conversation_id="nope"))

        assert excinfo.value.status == 404
        assert excinfo.value.code == "NOT_FOUND"
