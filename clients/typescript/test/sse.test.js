import test from 'node:test';
import assert from 'node:assert/strict';

import { parseSseFrame } from '../dist/sse.js';

test('single event/data frame', () => {
  assert.deepEqual(parseSseFrame('event: sql\ndata: {"sql":"SELECT 1"}'), {
    event: 'sql',
    data: '{"sql":"SELECT 1"}',
  });
});

test('multiple data lines are joined with "\\n" (spec-correct)', () => {
  assert.deepEqual(parseSseFrame('event: delta\ndata: a\ndata: b\ndata: c'), {
    event: 'delta',
    data: 'a\nb\nc',
  });
});

test('exactly one leading space after the colon is stripped', () => {
  assert.deepEqual(parseSseFrame('data:  two spaces'), {
    event: 'message',
    data: ' two spaces',
  });
  assert.deepEqual(parseSseFrame('data:no space'), {
    event: 'message',
    data: 'no space',
  });
});

test('event defaults to "message" when absent', () => {
  assert.equal(parseSseFrame('data: x').event, 'message');
});

test('comment lines and unknown fields are ignored', () => {
  assert.deepEqual(
    parseSseFrame(': heartbeat\nid: 7\nretry: 100\nevent: meta\ndata: {}'),
    { event: 'meta', data: '{}' },
  );
});

test('frames with no data lines return null', () => {
  assert.equal(parseSseFrame(': just a comment'), null);
  assert.equal(parseSseFrame('event: meta'), null);
  assert.equal(parseSseFrame(''), null);
});

test('CRLF line endings are tolerated', () => {
  assert.deepEqual(parseSseFrame('event: delta\r\ndata: a\r\ndata: b\r'), {
    event: 'delta',
    data: 'a\nb',
  });
});
