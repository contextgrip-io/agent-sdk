"""Python client for the ContextGrip AI Chat API.

The API contract lives in ``openapi.yaml`` at the repository root; this
package mirrors it. See README.md for usage.
"""

from .client import Client
from .errors import AiChatError
from .models import (
    Approval,
    ApprovalRequiredEvent,
    ApprovalSource,
    AskResponse,
    Conversation,
    ConversationDetail,
    CreatedToken,
    DecideApprovalResult,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    ExportConnection,
    ExportContext,
    ExportEval,
    ExportQuery,
    ExportResponse,
    Message,
    MetaEvent,
    ResultEvent,
    ResultSummary,
    SqlEvent,
    Status,
    Step,
    StepEvent,
    StreamEvent,
    Task,
    TaskDetail,
    TokenInfo,
    TrainingExportLine,
    TrainingStats,
)

__version__ = "0.1.0"

__all__ = [
    "AiChatError",
    "Approval",
    "ApprovalRequiredEvent",
    "ApprovalSource",
    "AskResponse",
    "Client",
    "Conversation",
    "ConversationDetail",
    "CreatedToken",
    "DecideApprovalResult",
    "DeltaEvent",
    "DoneEvent",
    "ErrorEvent",
    "ExportConnection",
    "ExportContext",
    "ExportEval",
    "ExportQuery",
    "ExportResponse",
    "Message",
    "MetaEvent",
    "ResultEvent",
    "ResultSummary",
    "SqlEvent",
    "Status",
    "Step",
    "StepEvent",
    "StreamEvent",
    "Task",
    "TaskDetail",
    "TokenInfo",
    "TrainingExportLine",
    "TrainingStats",
    "__version__",
]
