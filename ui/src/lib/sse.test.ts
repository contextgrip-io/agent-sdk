import { describe, expect, it } from 'vitest';
import { SseParser, parseSseFrame } from './sse';

describe('parseSseFrame', () => {
  it('parses an event name and JSON data', () => {
    const ev = parseSseFrame('event: sql\ndata: {"sql":"SELECT 1"}');
    expect(ev).toEqual({ event: 'sql', data: { sql: 'SELECT 1' } });
  });

  it('defaults the event name to "message" when no event field is present', () => {
    const ev = parseSseFrame('data: {"ok":true}');
    expect(ev).toEqual({ event: 'message', data: { ok: true } });
  });

  it('joins multiple data lines with "\\n" before JSON parsing', () => {
    // JSON split across data lines only survives if the join is exactly "\n".
    const ev = parseSseFrame('event: delta\ndata: {"text":\ndata: "hi"}');
    expect(ev).toEqual({ event: 'delta', data: { text: 'hi' } });
  });

  it('strips exactly one leading space after the colon', () => {
    const ev = parseSseFrame('event:delta\ndata:  "x"');
    // two spaces after "data:" -> one is stripped, one belongs to the value,
    // which is still valid JSON for a string literal
    expect(ev).toEqual({ event: 'delta', data: 'x' });
  });

  it('ignores comment lines and unknown fields', () => {
    const ev = parseSseFrame(': keep-alive\nid: 42\nretry: 100\nevent: done\ndata: {}');
    expect(ev).toEqual({ event: 'done', data: {} });
  });

  it('returns null for a frame with no data field', () => {
    expect(parseSseFrame('event: done')).toBeNull();
    expect(parseSseFrame(': just a comment')).toBeNull();
  });

  it('returns null for malformed (non-JSON) data', () => {
    expect(parseSseFrame('event: delta\ndata: not-json')).toBeNull();
  });
});

describe('SseParser', () => {
  it('splits frames on blank lines and emits each event once', () => {
    const p = new SseParser();
    const events = p.push(
      'event: meta\ndata: {"conversationId":"c1","userMessageId":"m1"}\n\n' +
        'event: sql\ndata: {"sql":"SELECT 1"}\n\n',
    );
    expect(events.map((e) => e.event)).toEqual(['meta', 'sql']);
    expect(events[0].data).toEqual({ conversationId: 'c1', userMessageId: 'm1' });
  });

  it('buffers partial frames across arbitrary chunk boundaries', () => {
    const p = new SseParser();
    const stream = 'event: delta\ndata: {"text":"he"}\n\nevent: delta\ndata: {"text":"llo"}\n\n';
    const events = [];
    for (const ch of stream) events.push(...p.push(ch)); // one char at a time
    expect(events).toEqual([
      { event: 'delta', data: { text: 'he' } },
      { event: 'delta', data: { text: 'llo' } },
    ]);
  });

  it('joins multi-line data across pushes', () => {
    const p = new SseParser();
    const a = p.push('event: delta\ndata: {"text":\n');
    const b = p.push('data: "split"}\n\n');
    expect(a).toEqual([]);
    expect(b).toEqual([{ event: 'delta', data: { text: 'split' } }]);
  });

  it('skips malformed frames without dropping the ones around them', () => {
    const p = new SseParser();
    const events = p.push(
      'event: sql\ndata: {"sql":"SELECT 1"}\n\n' +
        'event: bogus\ndata: {broken json\n\n' +
        'event: done\ndata: {"conversationId":"c1","assistantMessageId":"a1"}\n\n',
    );
    expect(events.map((e) => e.event)).toEqual(['sql', 'done']);
  });

  it('handles CRLF line endings, including a CRLF split across chunks', () => {
    const p = new SseParser();
    const events = [
      ...p.push('event: delta\r\ndata: {"text":"a"}\r'),
      ...p.push('\n\r\nevent: delta\r\ndata: {"text":"b"}\r\n\r\n'),
    ];
    expect(events).toEqual([
      { event: 'delta', data: { text: 'a' } },
      { event: 'delta', data: { text: 'b' } },
    ]);
  });

  it('flush() drains a trailing frame that had no final blank line', () => {
    const p = new SseParser();
    expect(p.push('event: error\ndata: {"message":"boom"}')).toEqual([]);
    expect(p.flush()).toEqual([{ event: 'error', data: { message: 'boom' } }]);
    expect(p.flush()).toEqual([]); // idempotent
  });

  it('flush() on comment-only or empty leftovers yields nothing', () => {
    const p = new SseParser();
    p.push(': ping\n');
    expect(p.flush()).toEqual([]);
  });
});
