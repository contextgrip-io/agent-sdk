import test from 'node:test';
import assert from 'node:assert/strict';

import { AiChatClient, AiChatError } from '../dist/index.js';
import { startStub, readBody, sendJson, sleep } from './stub.js';

const SSE_HEADERS = {
  'content-type': 'text/event-stream',
  'cache-control': 'no-cache',
  connection: 'keep-alive',
};

test('streamMessage() dispatches the full event sequence, including a multi-line data frame and a frame split across two TCP chunks', async () => {
  let seen;
  const stub = await startStub(async (req, res) => {
    seen = {
      method: req.method,
      url: req.url,
      authorization: req.headers.authorization,
      contentType: req.headers['content-type'],
      body: JSON.parse(await readBody(req)),
    };
    res.writeHead(200, SSE_HEADERS);

    // 1. meta
    res.write('event: meta\ndata: {"conversationId":"conv-5","userMessageId":"u-1"}\n\n');
    await sleep(15);

    // 2. sql — the frame is split across two TCP chunks, mid-token.
    res.write('event: sql\ndata: {"sq');
    await sleep(25);
    res.write('l":"SELECT count(*) FROM orders"}\n\n');
    await sleep(15);

    // 3. result
    res.write(
      'event: result\ndata: {"columns":["count"],"rowSample":[[42]],"rowCount":1,"truncated":false,"executionTimeMs":9}\n\n',
    );
    await sleep(15);

    // 4. delta with a MULTI-LINE data payload: the two data lines must be
    // joined with "\n" to form valid JSON.
    res.write('event: delta\ndata: {"text":\ndata:  "There are "}\n\n');
    await sleep(15);

    // 5. a malformed frame (invalid JSON) — must be skipped silently.
    res.write('event: delta\ndata: {not json at all\n\n');

    // 6. an SSE comment/heartbeat — no data, must be ignored.
    res.write(': heartbeat\n\n');

    // 7. an unknown event type — ignored for forward compatibility.
    res.write('event: totally-new\ndata: {"x":1}\n\n');

    // 8. second delta and done.
    res.write('event: delta\ndata: {"text":"42 orders."}\n\n');
    res.write('event: done\ndata: {"conversationId":"conv-5","assistantMessageId":"a-1"}\n\n');
    res.end();
  });

  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'tok-s' });
    const events = [];
    await client.streamMessage(
      { question: 'How many orders?' },
      {
        onMeta: (meta) => events.push(['meta', meta]),
        onSql: (sql) => events.push(['sql', sql]),
        onResult: (result) => events.push(['result', result]),
        onDelta: (text) => events.push(['delta', text]),
        onDone: (done) => events.push(['done', done]),
        onError: (error) => events.push(['error', error]),
      },
    );

    assert.equal(seen.method, 'POST');
    assert.equal(seen.url, '/api/v1/messages');
    assert.equal(seen.authorization, 'Bearer tok-s');
    assert.match(seen.contentType, /^application\/json/);
    assert.deepEqual(seen.body, { question: 'How many orders?' });

    assert.deepEqual(events, [
      ['meta', { conversationId: 'conv-5', userMessageId: 'u-1' }],
      ['sql', 'SELECT count(*) FROM orders'],
      ['result', { columns: ['count'], rowSample: [[42]], rowCount: 1, truncated: false, executionTimeMs: 9 }],
      ['delta', 'There are '],
      ['delta', '42 orders.'],
      ['done', { conversationId: 'conv-5', assistantMessageId: 'a-1' }],
    ]);
  } finally {
    await stub.close();
  }
});

test('a terminal error event calls onError and RESOLVES (does not throw)', async () => {
  const stub = await startStub(async (req, res) => {
    await readBody(req);
    res.writeHead(200, SSE_HEADERS);
    res.write('event: meta\ndata: {"conversationId":"conv-6","userMessageId":"u-2"}\n\n');
    res.write('event: error\ndata: {"message":"model request failed"}\n\n');
    res.end();
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const events = [];
    await client.streamMessage(
      { question: 'q' },
      {
        onMeta: (meta) => events.push(['meta', meta]),
        onDone: () => events.push(['done']),
        onError: (error) => events.push(['error', error]),
      },
    );
    assert.deepEqual(events, [
      ['meta', { conversationId: 'conv-6', userMessageId: 'u-2' }],
      ['error', { message: 'model request failed' }],
    ]);
  } finally {
    await stub.close();
  }
});

test('a result event carrying an execution failure is delivered as-is', async () => {
  const stub = await startStub(async (req, res) => {
    await readBody(req);
    res.writeHead(200, SSE_HEADERS);
    res.write('event: result\ndata: {"error":"statement timeout","executionTimeMs":5000}\n\n');
    res.write('event: done\ndata: {"conversationId":"c","assistantMessageId":"a"}\n\n');
    res.end();
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const results = [];
    await client.streamMessage({ question: 'q' }, { onResult: (r) => results.push(r) });
    assert.deepEqual(results, [{ error: 'statement timeout', executionTimeMs: 5000 }]);
  } finally {
    await stub.close();
  }
});

test('pre-stream non-2xx rejects with AiChatError before any handler fires', async () => {
  const stub = await startStub((req, res) => {
    sendJson(res, 401, { error: 'missing or invalid bearer token', code: 'UNAUTHORIZED' });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url });
    let handlerFired = false;
    await assert.rejects(
      client.streamMessage(
        { question: 'q' },
        {
          onMeta: () => { handlerFired = true; },
          onError: () => { handlerFired = true; },
        },
      ),
      (err) => {
        assert.ok(err instanceof AiChatError);
        assert.equal(err.status, 401);
        assert.equal(err.code, 'UNAUTHORIZED');
        return true;
      },
    );
    assert.equal(handlerFired, false);
  } finally {
    await stub.close();
  }
});

test('a trailing complete frame without a final blank line is flushed at stream end', async () => {
  const stub = await startStub(async (req, res) => {
    await readBody(req);
    res.writeHead(200, SSE_HEADERS);
    res.write('event: meta\ndata: {"conversationId":"conv-7","userMessageId":"u-3"}\n\n');
    // The server closes right after the done frame, WITHOUT the trailing
    // blank line. The client must still deliver it.
    res.write('event: done\ndata: {"conversationId":"conv-7","assistantMessageId":"a-3"}\n');
    res.end();
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const events = [];
    await client.streamMessage(
      { question: 'q' },
      {
        onMeta: () => events.push('meta'),
        onDone: (done) => events.push(['done', done]),
      },
    );
    assert.deepEqual(events, [
      'meta',
      ['done', { conversationId: 'conv-7', assistantMessageId: 'a-3' }],
    ]);
  } finally {
    await stub.close();
  }
});

test('aborting via opts.signal rejects mid-stream', async () => {
  const stub = await startStub(async (req, res) => {
    await readBody(req);
    res.writeHead(200, SSE_HEADERS);
    res.write('event: meta\ndata: {"conversationId":"conv-8","userMessageId":"u-4"}\n\n');
    // Keep the connection open; the client aborts.
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const controller = new AbortController();
    const gotMeta = [];
    await assert.rejects(
      client.streamMessage(
        { question: 'q' },
        {
          onMeta: (meta) => {
            gotMeta.push(meta);
            controller.abort();
          },
        },
        { signal: controller.signal },
      ),
      (err) => err.name === 'AbortError',
    );
    assert.equal(gotMeta.length, 1);
  } finally {
    await stub.close();
  }
});
