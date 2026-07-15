from __future__ import annotations

from collections.abc import Iterator

import pytest

from contextgrip_ai_chat import Client

from .stub_server import StubServer


@pytest.fixture()
def server() -> Iterator[StubServer]:
    stub = StubServer()
    stub.start()
    yield stub
    stub.stop()


@pytest.fixture()
def client(server: StubServer) -> Iterator[Client]:
    with Client(base_url=server.base_url, token="test-token", timeout=10.0) as c:
        yield c
