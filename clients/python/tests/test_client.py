"""End-to-end tests against a real threaded stub HTTP server."""

from __future__ import annotations

import pytest

from contextgrip_ai_chat import (
    AiChatError,
    AskResponse,
    Client,
    Conversation,
    ConversationDetail,
    CreatedToken,
    ResultSummary,
    Status,
    TokenInfo,
)

from .stub_server import StubServer

ASK_PAYLOAD = {
    "conversationId": "conv-1",
    "userMessageId": "msg-u1",
    "assistantMessageId": "msg-a1",
    "sql": "SELECT count(*) FROM orders",
    "result": {
        "columns": ["count"],
        "rowSample": [[42]],
        "rowCount": 1,
        "truncated": False,
        "executionTimeMs": 12,
    },
    "answer": "There are 42 orders.",
}


class TestAsk:
    def test_happy_path(self, server: StubServer, client: Client) -> None:
        server.json("POST", "/api/v1/ask", ASK_PAYLOAD)

        response = client.ask("How many orders?")

        assert isinstance(response, AskResponse)
        assert response.conversation_id == "conv-1"
        assert response.user_message_id == "msg-u1"
        assert response.assistant_message_id == "msg-a1"
        assert response.sql == "SELECT count(*) FROM orders"
        assert response.answer == "There are 42 orders."
        assert response.result_error is None
        assert isinstance(response.result, ResultSummary)
        assert response.result.columns == ["count"]
        assert response.result.row_sample == [[42]]
        assert response.result.row_count == 1
        assert response.result.truncated is False
        assert response.result.execution_time_ms == 12

        request = server.last_request()
        assert request.json == {"question": "How many orders?"}
        assert request.headers["Authorization"] == "Bearer test-token"
        assert request.headers["Content-Type"] == "application/json"

    def test_sends_conversation_id_in_camel_case(
        self, server: StubServer, client: Client
    ) -> None:
        server.json("POST", "/api/v1/ask", ASK_PAYLOAD)

        client.ask("Follow-up?", conversation_id="conv-1")

        assert server.last_request().json == {
            "question": "Follow-up?",
            "conversationId": "conv-1",
        }

    def test_result_error_is_not_an_http_error(
        self, server: StubServer, client: Client
    ) -> None:
        payload = dict(ASK_PAYLOAD)
        del payload["result"]
        payload["resultError"] = 'relation "orders" does not exist'
        payload["answer"] = "The query failed because the table does not exist."
        server.json("POST", "/api/v1/ask", payload)

        response = client.ask("How many orders?")

        assert response.result is None
        assert response.result_error == 'relation "orders" does not exist'
        assert "failed" in response.answer

    def test_401_maps_to_unauthorized_error(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "POST",
            "/api/v1/ask",
            {"error": "missing or invalid bearer token", "code": "UNAUTHORIZED"},
            status=401,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.ask("Who am I?")

        assert excinfo.value.status == 401
        assert excinfo.value.code == "UNAUTHORIZED"
        assert excinfo.value.message == "missing or invalid bearer token"

    def test_error_without_code(self, server: StubServer, client: Client) -> None:
        server.json("POST", "/api/v1/ask", {"error": "boom"}, status=503)

        with pytest.raises(AiChatError) as excinfo:
            client.ask("Anything?")

        assert excinfo.value.status == 503
        assert excinfo.value.code is None
        assert excinfo.value.message == "boom"


class TestStatus:
    def test_status(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/status",
            {
                "version": "0.1.0",
                "model": "claude-sonnet-4-5",
                "engine": "postgresql",
                "ready": True,
                "features": ["chat", "agent", "board"],
                "writesEnabled": True,
            },
        )

        status = client.status()

        assert isinstance(status, Status)
        assert status.version == "0.1.0"
        assert status.model == "claude-sonnet-4-5"
        assert status.engine == "postgresql"
        assert status.ready is True
        assert status.features == ["chat", "agent", "board"]
        assert status.writes_enabled is True

    def test_status_chat_only_writes_disabled(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "GET",
            "/api/v1/status",
            {
                "version": "0.1.0",
                "model": "claude-sonnet-4-5",
                "engine": "postgresql",
                "ready": True,
                "features": ["chat"],
                "writesEnabled": False,
            },
        )

        status = client.status()

        assert status.features == ["chat"]
        assert status.writes_enabled is False


class TestConversations:
    def test_list(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/conversations",
            [
                {
                    "id": "conv-2",
                    "title": "Recent one",
                    "createdAt": "2026-07-15T10:00:00Z",
                    "updatedAt": "2026-07-15T11:00:00Z",
                },
                {
                    "id": "conv-1",
                    "title": "Older one",
                    "createdAt": "2026-07-14T10:00:00Z",
                    "updatedAt": "2026-07-14T10:05:00Z",
                },
            ],
        )

        conversations = client.list_conversations()

        assert [c.id for c in conversations] == ["conv-2", "conv-1"]
        assert all(isinstance(c, Conversation) for c in conversations)
        assert conversations[0].title == "Recent one"
        assert conversations[0].created_at == "2026-07-15T10:00:00Z"
        assert conversations[0].updated_at == "2026-07-15T11:00:00Z"

    def test_get(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/conversations/conv-1",
            {
                "conversation": {
                    "id": "conv-1",
                    "title": "Orders",
                    "createdAt": "2026-07-14T10:00:00Z",
                    "updatedAt": "2026-07-14T10:05:00Z",
                },
                "messages": [
                    {
                        "id": "msg-u1",
                        "role": "user",
                        "text": "How many orders?",
                        "createdAt": "2026-07-14T10:00:00Z",
                    },
                    {
                        "id": "msg-a1",
                        "role": "assistant",
                        "text": "There are 42 orders.",
                        "sql": "SELECT count(*) FROM orders",
                        "result": {
                            "columns": ["count"],
                            "rowSample": [[42]],
                            "rowCount": 1,
                            "truncated": False,
                            "executionTimeMs": 12,
                        },
                        "createdAt": "2026-07-14T10:00:05Z",
                    },
                    {
                        "id": "msg-a2",
                        "role": "assistant",
                        "error": "statement timeout",
                        "createdAt": "2026-07-14T10:04:00Z",
                    },
                ],
            },
        )

        detail = client.get_conversation("conv-1")

        assert isinstance(detail, ConversationDetail)
        assert detail.conversation.id == "conv-1"
        assert [m.id for m in detail.messages] == ["msg-u1", "msg-a1", "msg-a2"]
        user, assistant, failed = detail.messages
        assert user.role == "user"
        assert user.sql is None and user.result is None
        assert assistant.sql == "SELECT count(*) FROM orders"
        assert assistant.result is not None
        assert assistant.result.row_count == 1
        assert failed.error == "statement timeout"

    def test_get_unknown_raises_not_found(
        self, server: StubServer, client: Client
    ) -> None:
        server.json(
            "GET",
            "/api/v1/conversations/nope",
            {"error": "conversation not found", "code": "NOT_FOUND"},
            status=404,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.get_conversation("nope")

        assert excinfo.value.status == 404
        assert excinfo.value.code == "NOT_FOUND"

    def test_delete(self, server: StubServer, client: Client) -> None:
        server.json("DELETE", "/api/v1/conversations/conv-1", {"deleted": True})

        assert client.delete_conversation("conv-1") is None

        request = server.last_request()
        assert request.method == "DELETE"
        assert request.path == "/api/v1/conversations/conv-1"


class TestTokens:
    def test_list(self, server: StubServer, client: Client) -> None:
        server.json(
            "GET",
            "/api/v1/tokens",
            [
                {
                    "id": "tok-1",
                    "label": "ci",
                    "fingerprint": "ab12cd34",
                    "createdAt": "2026-07-01T00:00:00Z",
                    "lastUsedAt": "2026-07-15T09:00:00Z",
                },
                {
                    "id": "tok-2",
                    "label": "unused",
                    "fingerprint": "ef56ab78",
                    "createdAt": "2026-07-10T00:00:00Z",
                },
            ],
        )

        tokens = client.list_tokens()

        assert all(isinstance(t, TokenInfo) for t in tokens)
        assert tokens[0].fingerprint == "ab12cd34"
        assert tokens[0].last_used_at == "2026-07-15T09:00:00Z"
        assert tokens[1].last_used_at is None

    def test_create(self, server: StubServer, client: Client) -> None:
        server.json(
            "POST",
            "/api/v1/tokens",
            {
                "id": "tok-3",
                "label": "reporting",
                "fingerprint": "0011aabb",
                "createdAt": "2026-07-15T12:00:00Z",
                "token": "pgac_raw_secret_value",
            },
            status=201,
        )

        created = client.create_token("reporting")

        assert isinstance(created, CreatedToken)
        assert created.id == "tok-3"
        assert created.label == "reporting"
        assert created.token == "pgac_raw_secret_value"
        assert created.last_used_at is None
        assert server.last_request().json == {"label": "reporting"}

    def test_create_admin_required(self, server: StubServer, client: Client) -> None:
        server.json(
            "POST",
            "/api/v1/tokens",
            {"error": "named tokens cannot manage tokens", "code": "ADMIN_REQUIRED"},
            status=403,
        )

        with pytest.raises(AiChatError) as excinfo:
            client.create_token("nope")

        assert excinfo.value.status == 403
        assert excinfo.value.code == "ADMIN_REQUIRED"

    def test_revoke(self, server: StubServer, client: Client) -> None:
        server.json("DELETE", "/api/v1/tokens/tok-1", {"deleted": True})

        assert client.revoke_token("tok-1") is None

        request = server.last_request()
        assert request.method == "DELETE"
        assert request.path == "/api/v1/tokens/tok-1"


class TestClientBehavior:
    def test_no_authorization_header_without_token(self, server: StubServer) -> None:
        server.json(
            "GET",
            "/api/v1/status",
            {
                "version": "0.1.0",
                "model": "m",
                "engine": "postgresql",
                "ready": True,
                "features": ["chat"],
                "writesEnabled": False,
            },
        )

        with Client(base_url=server.base_url, timeout=10.0) as client:
            client.status()

        assert "Authorization" not in server.last_request().headers

    def test_context_manager_closes_http_client(self, server: StubServer) -> None:
        client = Client(base_url=server.base_url, timeout=10.0)
        with client:
            pass
        assert client._http.is_closed
