"""Unit tests for the internal SSE frame parser and event typing."""

from __future__ import annotations

from contextgrip_ai_chat import DeltaEvent, ErrorEvent, MetaEvent, ResultEvent
from contextgrip_ai_chat._sse import iter_sse_frames, parse_stream_event


class TestIterSseFrames:
    def test_basic_frame(self) -> None:
        lines = ['event: delta', 'data: {"text":"hi"}', '']
        assert list(iter_sse_frames(lines)) == [('delta', '{"text":"hi"}')]

    def test_multiline_data_joined_with_newline(self) -> None:
        lines = ['event: delta', 'data: {"text":', 'data:  "hi"}', '']
        assert list(iter_sse_frames(lines)) == [('delta', '{"text":\n "hi"}')]

    def test_data_without_space_after_colon(self) -> None:
        lines = ['event:delta', 'data:{"text":"hi"}', '']
        assert list(iter_sse_frames(lines)) == [('delta', '{"text":"hi"}')]

    def test_only_one_leading_space_stripped(self) -> None:
        lines = ['event: delta', 'data:   spaced', '']
        assert list(iter_sse_frames(lines)) == [('delta', '  spaced')]

    def test_comments_id_and_retry_ignored(self) -> None:
        lines = [
            ': comment',
            'id: 3',
            'retry: 100',
            'event: delta',
            'data: {}',
            '',
        ]
        assert list(iter_sse_frames(lines)) == [('delta', '{}')]

    def test_frame_without_data_is_skipped(self) -> None:
        lines = ['event: delta', '', 'event: done', 'data: {}', '']
        assert list(iter_sse_frames(lines)) == [('done', '{}')]

    def test_event_name_resets_between_frames(self) -> None:
        lines = ['event: sql', 'data: {}', '', 'data: {}', '']
        assert list(iter_sse_frames(lines)) == [('sql', '{}'), ('message', '{}')]

    def test_final_frame_without_trailing_blank_line(self) -> None:
        lines = ['event: done', 'data: {}']
        assert list(iter_sse_frames(lines)) == [('done', '{}')]


class TestParseStreamEvent:
    def test_meta(self) -> None:
        event = parse_stream_event(
            'meta', '{"conversationId":"c","userMessageId":"u"}'
        )
        assert event == MetaEvent(conversation_id='c', user_message_id='u')

    def test_result_success_carries_execution_time(self) -> None:
        event = parse_stream_event(
            'result',
            '{"rowCount":2,"truncated":true,"executionTimeMs":9,"columns":["a"]}',
        )
        assert isinstance(event, ResultEvent)
        assert event.result is not None
        assert event.result.truncated is True
        assert event.execution_time_ms == 9
        assert event.error is None

    def test_result_error_variant(self) -> None:
        event = parse_stream_event(
            'result', '{"error":"boom","executionTimeMs":4}'
        )
        assert event == ResultEvent(result=None, error='boom', execution_time_ms=4)

    def test_error_event(self) -> None:
        assert parse_stream_event('error', '{"message":"m"}') == ErrorEvent(
            message='m'
        )

    def test_malformed_json_skipped(self) -> None:
        assert parse_stream_event('delta', '{oops') is None

    def test_non_object_payload_skipped(self) -> None:
        assert parse_stream_event('delta', '["not","an","object"]') is None

    def test_missing_required_key_skipped(self) -> None:
        assert parse_stream_event('meta', '{"conversationId":"c"}') is None
        assert parse_stream_event('result', '{"rowCount":1}') is None

    def test_unknown_event_skipped(self) -> None:
        assert parse_stream_event('bogus', '{"a":1}') is None

    def test_valid_delta(self) -> None:
        assert parse_stream_event('delta', '{"text":"hi"}') == DeltaEvent(text='hi')
