# contextgrip-ai-chat

Python client for the [ContextGrip AI Chat](https://github.com/contextgrip-io/ai-chat)
API — ask your PostgreSQL database questions in plain language against your own
self-hosted server. The contract this client mirrors is
[`openapi.yaml`](../../openapi.yaml) at the repository root.

- Python ≥ 3.11, single runtime dependency ([httpx](https://www.python-httpx.org/)).
- Fully typed (`py.typed` included); models are plain dataclasses with
  snake_case attributes mapped to the API's camelCase wire names.

## Install

```bash
pip install "contextgrip-ai-chat @ git+https://github.com/contextgrip-io/ai-chat#subdirectory=clients/python"
```

## Ask a question

```python
from contextgrip_ai_chat import Client

with Client(base_url="http://localhost:8080", token="your-app-access-token") as client:
    response = client.ask("How many orders shipped last week?")
    print(response.sql)      # the generated read-only SQL (always shown)
    print(response.answer)   # natural-language explanation

    if response.result is not None:
        print(response.result.columns, response.result.row_sample)
    else:
        # failed query execution is not an HTTP error;
        # the answer explains the failure
        print("query failed:", response.result_error)

    # continue the same conversation
    follow_up = client.ask("Break that down by region", conversation_id=response.conversation_id)
```

`token` is either the server's primary `APP_ACCESS_TOKEN` or a named token
minted via the token admin endpoints. `timeout` defaults to 600 seconds —
NL→SQL answers can take a while.

## Stream the answer

`stream_message()` yields typed events in order:
`MetaEvent → SqlEvent → ResultEvent → DeltaEvent* → DoneEvent`, or a terminal
`ErrorEvent` at any point once the stream has started (yielded, not raised).
Pre-stream failures (validation, auth, unknown conversation) raise
`AiChatError`.

```python
from contextgrip_ai_chat import (
    Client, DeltaEvent, DoneEvent, ErrorEvent, MetaEvent, ResultEvent, SqlEvent,
)

with Client(base_url="http://localhost:8080", token="...") as client:
    for event in client.stream_message("Which products sell best on weekends?"):
        match event:
            case MetaEvent():
                print("conversation:", event.conversation_id)
            case SqlEvent():
                print("sql:", event.sql)
            case ResultEvent() if event.error is not None:
                print("query failed:", event.error)
            case ResultEvent():
                print(f"{event.result.row_count} rows in {event.execution_time_ms}ms")
            case DeltaEvent():
                print(event.text, end="", flush=True)
            case DoneEvent():
                print()
            case ErrorEvent():
                print("stream error:", event.message)
```

## Agent mode and write approvals

With the `agent` feature enabled (`client.status().features`), pass
`mode="agent"` to let the model take multiple tool steps: read-only queries
run automatically as `StepEvent`s, and proposed writes never execute
directly — the turn ends with an `ApprovalRequiredEvent`, the `DoneEvent`
carries `pending_approval_id`, and the write only runs when you approve it.

```python
from contextgrip_ai_chat import (
    ApprovalRequiredEvent, Client, DeltaEvent, DoneEvent, StepEvent,
)

with Client(base_url="http://localhost:8080", token="...") as client:
    for event in client.stream_message("Mark order 7 as shipped", mode="agent"):
        match event:
            case StepEvent():
                print(f"step {event.step.index} [{event.step.kind}]: {event.step.summary}")
            case ApprovalRequiredEvent():
                print("proposed write:", event.approval.sql)
                print("rationale:", event.approval.rationale)
            case DeltaEvent():
                print(event.text, end="", flush=True)
            case DoneEvent() if event.pending_approval_id:
                print("\nawaiting approval:", event.pending_approval_id)

    # decide the pending write (executes only on approve; requires
    # AI_CHAT_WRITE_DATABASE_URL on the server — writes_enabled in status)
    for approval in client.list_approvals():
        outcome = client.decide_approval(approval.id, "approve")  # or "reject"
        if outcome.error is not None:
            print("write failed:", outcome.error)
        elif outcome.result is not None:
            print(f"write ran in {outcome.result.execution_time_ms}ms")
```

`ask(..., mode="agent")` works too: the response may carry `steps` instead of
the sql/result pair, plus `pending_approval_id` when a write awaits a
decision. A conversation keeps the mode of its first message.

## Board tasks

With the `board` feature, file background tasks that run through the same
agent loop, one at a time. Proposed writes pause a task in `needs_approval`;
the approval decision resumes it.

```python
task = client.create_task("Weekly cleanup", "Archive orders older than a year")

for t in client.list_tasks(status="needs_approval"):
    detail = client.get_task(t.id)
    for step in detail.steps:
        print(step.index, step.kind, step.summary)
    if detail.pending_approval is not None:
        client.decide_approval(detail.pending_approval.id, "approve")

client.cancel_task(task.id)   # queued/running/needs_approval only (409 TASK_FINISHED otherwise)
client.delete_task(task.id)   # done/failed/canceled only (409 TASK_ACTIVE otherwise)
```

## Conversations

```python
for conversation in client.list_conversations():
    print(conversation.id, conversation.title, conversation.updated_at)

detail = client.get_conversation(conversation_id)
for message in detail.messages:
    print(message.role, message.text)

client.delete_conversation(conversation_id)
```

## Training data

Rate answers, control automatic capture, and stream the JSONL export. The
export line format matches ContextGrip's training-data export, so dumps from
both sources merge downstream without transformation.

```python
# rate an assistant answer (writes a training record, bypasses the capture toggle)
client.rate_message(response.assistant_message_id, "good")

# automatic capture of every completed exchange (set is admin-only)
if not client.get_training_capture():
    client.set_training_capture(True)

stats = client.training_stats()
print(f"{stats.records} records, {stats.evaluated} evaluated")

# stream the export as parsed TrainingExportLine objects (NDJSON under the hood)
exported = 0
for line in client.iter_training_export(include_rows=True, evaluated_only=False):
    verdict = line.eval.verdict if line.eval else None
    print(line.id, line.query.intent, "->", line.query.sql, verdict)
    exported += 1

# the server stops the stream at a 64 MiB budget — compare with stats
if exported < stats.records:
    print("export truncated")

# wipe all training records (admin-only)
deleted = client.delete_training_records()
```

## Token admin (primary `APP_ACCESS_TOKEN` only)

```python
created = client.create_token("reporting")
print(created.token)  # raw value — shown only once

for token in client.list_tokens():
    print(token.id, token.label, token.fingerprint, token.last_used_at)

client.revoke_token(created.id)
```

## Errors

Non-2xx API responses raise `AiChatError` with `status`, `code`, and
`message` parsed from the wire error shape `{"error": ..., "code": ...}`:

```python
from contextgrip_ai_chat import AiChatError

try:
    client.status()
except AiChatError as exc:
    print(exc.status, exc.code, exc.message)  # e.g. 401 UNAUTHORIZED ...
```

Known codes: `UNAUTHORIZED`, `ADMIN_REQUIRED`, `VALIDATION`, `NOT_FOUND`,
`NOT_CONFIGURED`, `CONVERSATION_FULL`, `STORE_ERROR`, `MODEL_ERROR`,
`STREAM_UNSUPPORTED`, `FEATURE_DISABLED`, `WRITES_DISABLED`,
`ALREADY_DECIDED`, `TASK_ACTIVE`, `TASK_FINISHED`.

## Development

```bash
cd clients/python
python3 -m venv .venv && . .venv/bin/activate
pip install -e ".[dev]"
python -m pytest
```

Tests run against a real threaded stub HTTP server (including chunked SSE
delivery) — no network access or live server required.
