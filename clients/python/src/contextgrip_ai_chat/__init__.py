"""Python client for the ContextGrip AI Chat API.

The API contract lives in ``openapi.yaml`` at the repository root; this
package mirrors it. See README.md for usage.
"""

from .client import Client
from .errors import AiChatError
from .models import (
    AskResponse,
    Conversation,
    ConversationDetail,
    CreatedToken,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    Message,
    MetaEvent,
    ResultEvent,
    ResultSummary,
    SqlEvent,
    Status,
    StreamEvent,
    TokenInfo,
)

__version__ = "0.1.0"

__all__ = [
    "AiChatError",
    "AskResponse",
    "Client",
    "Conversation",
    "ConversationDetail",
    "CreatedToken",
    "DeltaEvent",
    "DoneEvent",
    "ErrorEvent",
    "Message",
    "MetaEvent",
    "ResultEvent",
    "ResultSummary",
    "SqlEvent",
    "Status",
    "StreamEvent",
    "TokenInfo",
    "__version__",
]
