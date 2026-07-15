"""Error types for the ContextGrip AI Chat client."""

from __future__ import annotations


class AiChatError(Exception):
    """Raised when the API returns a non-2xx response.

    Attributes mirror the wire error shape ``{"error": ..., "code": ...}``:

    - ``status``: HTTP status code of the response.
    - ``code``: stable machine slug (e.g. ``UNAUTHORIZED``, ``NOT_FOUND``,
      ``ADMIN_REQUIRED``, ``NOT_CONFIGURED``) or ``None`` when the server
      did not send one.
    - ``message``: human-readable message from the ``error`` field.
    """

    def __init__(self, status: int, code: str | None, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message

    def __repr__(self) -> str:
        return (
            f"AiChatError(status={self.status!r}, code={self.code!r}, "
            f"message={self.message!r})"
        )

    def __str__(self) -> str:
        if self.code:
            return f"[{self.status} {self.code}] {self.message}"
        return f"[{self.status}] {self.message}"
