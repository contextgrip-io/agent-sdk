"""A real threaded HTTP stub server for exercising the client end-to-end.

Routes are registered per test as ``(method, path) -> handler``; each
handler receives the recorded request and returns a ``JsonResponse`` or an
``SseResponse``. SSE responses are written as raw byte chunks with a flush
(and a small pause) between chunks, so tests exercise chunked delivery of
frames split at arbitrary byte boundaries.
"""

from __future__ import annotations

import json
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit


@dataclass
class RecordedRequest:
    method: str
    path: str  # full request target, including any query string
    headers: dict[str, str]
    body: bytes

    @property
    def json(self) -> Any:
        return json.loads(self.body)

    @property
    def query(self) -> dict[str, list[str]]:
        return parse_qs(urlsplit(self.path).query)


@dataclass
class JsonResponse:
    status: int = 200
    payload: Any = None


@dataclass
class SseResponse:
    """A streamed body written chunk-by-chunk with a flush between chunks."""

    chunks: list[bytes] = field(default_factory=list)
    delay: float = 0.005
    content_type: str = "text/event-stream"


Handler = Callable[[RecordedRequest], "JsonResponse | SseResponse"]


class _StubHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    stub: "StubServer"


class _RequestHandler(BaseHTTPRequestHandler):
    # HTTP/1.0 (the default): SSE bodies end at connection close, so the
    # client never needs Content-Length for streams.

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
        pass

    def do_GET(self) -> None:
        self._handle("GET")

    def do_POST(self) -> None:
        self._handle("POST")

    def do_PUT(self) -> None:
        self._handle("PUT")

    def do_DELETE(self) -> None:
        self._handle("DELETE")

    def _handle(self, method: str) -> None:
        stub: StubServer = self.server.stub  # type: ignore[attr-defined]
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        request = RecordedRequest(
            method=method,
            path=self.path,
            headers={k: v for k, v in self.headers.items()},
            body=body,
        )
        stub.requests.append(request)

        # Routes are keyed on the bare path; the query string is recorded
        # on the request for assertions.
        handler = stub.routes.get((method, urlsplit(self.path).path))
        if handler is None:
            self._send_json(
                JsonResponse(404, {"error": "no stub route", "code": "NOT_FOUND"})
            )
            return
        response = handler(request)
        if isinstance(response, SseResponse):
            self._send_sse(response)
        else:
            self._send_json(response)

    def _send_json(self, response: JsonResponse) -> None:
        data = json.dumps(response.payload).encode()
        self.send_response(response.status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_sse(self, response: SseResponse) -> None:
        self.send_response(200)
        self.send_header("Content-Type", response.content_type)
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for chunk in response.chunks:
            self.wfile.write(chunk)
            self.wfile.flush()
            if response.delay:
                time.sleep(response.delay)


class StubServer:
    def __init__(self) -> None:
        self.routes: dict[tuple[str, str], Handler] = {}
        self.requests: list[RecordedRequest] = []
        self._server = _StubHTTPServer(("127.0.0.1", 0), _RequestHandler)
        self._server.stub = self
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    # -- convenience registration helpers ------------------------------------

    def on(self, method: str, path: str, handler: Handler) -> None:
        self.routes[(method, path)] = handler

    def json(self, method: str, path: str, payload: Any, status: int = 200) -> None:
        self.on(method, path, lambda _req: JsonResponse(status, payload))

    def sse(self, method: str, path: str, chunks: list[bytes]) -> None:
        self.on(method, path, lambda _req: SseResponse(chunks))

    def ndjson(self, method: str, path: str, chunks: list[bytes]) -> None:
        self.on(
            method,
            path,
            lambda _req: SseResponse(chunks, content_type="application/x-ndjson"),
        )

    def last_request(self) -> RecordedRequest:
        return self.requests[-1]
