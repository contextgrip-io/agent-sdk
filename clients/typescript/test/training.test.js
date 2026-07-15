import test from 'node:test';
import assert from 'node:assert/strict';

import { AiChatClient, AiChatError } from '../dist/index.js';
import { startStub, readBody, sendJson, sleep } from './stub.js';

test('rateMessage() posts the verdict to /api/v1/messages/{id}/eval', async () => {
  let seen;
  const stub = await startStub(async (req, res) => {
    seen = {
      method: req.method,
      url: req.url,
      authorization: req.headers.authorization,
      contentType: req.headers['content-type'],
      body: JSON.parse(await readBody(req)),
    };
    sendJson(res, 200, { recorded: true });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'tok-r' });
    const out = await client.rateMessage('msg a/1', 'good');
    assert.equal(out, undefined);
    assert.equal(seen.method, 'POST');
    assert.equal(seen.url, '/api/v1/messages/msg%20a%2F1/eval');
    assert.equal(seen.authorization, 'Bearer tok-r');
    assert.match(seen.contentType, /^application\/json/);
    assert.deepEqual(seen.body, { verdict: 'good' });
  } finally {
    await stub.close();
  }
});

test('rateMessage() maps 404 (unknown message) to NOT_FOUND', async () => {
  const stub = await startStub((req, res) => {
    sendJson(res, 404, { error: 'unknown message', code: 'NOT_FOUND' });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    await assert.rejects(client.rateMessage('nope', 'bad'), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 404);
      assert.equal(err.code, 'NOT_FOUND');
      return true;
    });
  } finally {
    await stub.close();
  }
});

test('training capture: GET reads, PUT updates, 403 maps to ADMIN_REQUIRED', async () => {
  const requests = [];
  const stub = await startStub(async (req, res) => {
    const entry = { method: req.method, url: req.url };
    if (req.method === 'GET' && req.url === '/api/v1/training/capture') {
      sendJson(res, 200, { enabled: true });
    } else if (req.method === 'PUT' && req.url === '/api/v1/training/capture') {
      entry.body = JSON.parse(await readBody(req));
      sendJson(res, 200, entry.body);
    } else {
      sendJson(res, 404, { error: 'unknown resource', code: 'NOT_FOUND' });
    }
    requests.push(entry);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'primary' });

    assert.deepEqual(await client.getTrainingCapture(), { enabled: true });

    const updated = await client.setTrainingCapture(false);
    assert.deepEqual(updated, { enabled: false });
    assert.deepEqual(requests[1].body, { enabled: false });

    assert.deepEqual(
      requests.map((r) => `${r.method} ${r.url}`),
      ['GET /api/v1/training/capture', 'PUT /api/v1/training/capture'],
    );
  } finally {
    await stub.close();
  }

  const adminStub = await startStub((req, res) => {
    sendJson(res, 403, { error: 'named tokens cannot manage capture', code: 'ADMIN_REQUIRED' });
  });
  try {
    const named = new AiChatClient({ baseUrl: adminStub.url, token: 'named-token' });
    await assert.rejects(named.setTrainingCapture(true), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 403);
      assert.equal(err.code, 'ADMIN_REQUIRED');
      return true;
    });
  } finally {
    await adminStub.close();
  }
});

test('trainingStats() returns the stats object', async () => {
  let seen;
  const stub = await startStub((req, res) => {
    seen = { method: req.method, url: req.url };
    sendJson(res, 200, {
      records: 128,
      evaluated: 17,
      firstCapturedAt: '2026-07-01T00:00:00Z',
      lastCapturedAt: '2026-07-15T12:00:00Z',
    });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const stats = await client.trainingStats();
    assert.equal(seen.method, 'GET');
    assert.equal(seen.url, '/api/v1/training/stats');
    assert.deepEqual(stats, {
      records: 128,
      evaluated: 17,
      firstCapturedAt: '2026-07-01T00:00:00Z',
      lastCapturedAt: '2026-07-15T12:00:00Z',
    });
  } finally {
    await stub.close();
  }
});

const LINE_1 = {
  id: 'rec-1',
  capturedAt: '2026-07-14T10:00:00Z',
  connection: { id: 'ab12cd34', name: 'shop', engine: 'postgresql' },
  context: { session: 'chat', sourceMessageId: 'msg-a1' },
  query: { sql: 'SELECT count(*) FROM orders', intent: 'How many orders?' },
  response: { columns: ['count'], rowCount: 1, truncated: false, executionTimeMs: 9, rowSample: [[42]] },
  eval: { verdict: 'good' },
};
const LINE_2 = {
  id: 'rec-2',
  capturedAt: '2026-07-14T11:00:00Z',
  connection: { id: 'ab12cd34', name: 'shop', engine: 'postgresql' },
  context: { session: 'ask' },
  query: { sql: 'SELECT 1' },
  response: { rowCount: 0, truncated: false, executionTimeMs: 2, error: 'statement timeout' },
};
const LINE_3 = {
  id: 'rec-3',
  capturedAt: '2026-07-14T12:00:00Z',
  connection: { id: 'ab12cd34', name: 'shop', engine: 'postgresql' },
  context: {},
  query: { sql: 'SELECT 2' },
  response: { rowCount: 1, truncated: true, executionTimeMs: 4 },
};

test('exportTraining() yields parsed lines from a chunked NDJSON response, with a line split mid-JSON across chunks', async () => {
  let seen;
  const stub = await startStub(async (req, res) => {
    seen = { method: req.method, url: req.url, authorization: req.headers.authorization };
    res.writeHead(200, { 'content-type': 'application/x-ndjson' });

    const line2 = JSON.stringify(LINE_2);
    // Chunk 1: all of line 1, a blank line, and the FIRST HALF of line 2
    // (split mid-JSON, inside a string token).
    res.write(`${JSON.stringify(LINE_1)}\n\n${line2.slice(0, 25)}`);
    await sleep(25);
    // Chunk 2: the rest of line 2, then line 3 WITHOUT a trailing newline.
    res.write(`${line2.slice(25)}\n${JSON.stringify(LINE_3)}`);
    res.end();
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'tok-x' });
    const lines = [];
    for await (const line of client.exportTraining({ includeRows: false, evaluatedOnly: true })) {
      lines.push(line);
    }
    assert.equal(seen.method, 'GET');
    assert.equal(seen.url, '/api/v1/training/export?includeRows=false&evaluatedOnly=true');
    assert.equal(seen.authorization, 'Bearer tok-x');
    assert.deepEqual(lines, [LINE_1, LINE_2, LINE_3]);
  } finally {
    await stub.close();
  }
});

test('exportTraining() omits the query string when no options are given', async () => {
  let url;
  const stub = await startStub((req, res) => {
    url = req.url;
    res.writeHead(200, { 'content-type': 'application/x-ndjson' });
    res.end(`${JSON.stringify(LINE_1)}\n`);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const lines = [];
    for await (const line of client.exportTraining()) lines.push(line);
    assert.equal(url, '/api/v1/training/export');
    assert.deepEqual(lines, [LINE_1]);
  } finally {
    await stub.close();
  }
});

test('exportTraining() throws on a malformed line (server writes whole lines only)', async () => {
  const stub = await startStub((req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson' });
    const cut = JSON.stringify(LINE_2).slice(0, 40);
    res.end(`${JSON.stringify(LINE_1)}\n${cut}`);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const lines = [];
    await assert.rejects(async () => {
      for await (const line of client.exportTraining()) lines.push(line);
    }, /malformed NDJSON line/);
    assert.deepEqual(lines, [LINE_1]);
  } finally {
    await stub.close();
  }
});

test('exportTraining() rejects the first iteration with AiChatError on non-2xx', async () => {
  const stub = await startStub((req, res) => {
    sendJson(res, 401, { error: 'missing or invalid bearer token', code: 'UNAUTHORIZED' });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url });
    const iterator = client.exportTraining();
    await assert.rejects(iterator.next(), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 401);
      assert.equal(err.code, 'UNAUTHORIZED');
      return true;
    });
  } finally {
    await stub.close();
  }
});

test('breaking out of exportTraining() early closes the stream cleanly', async () => {
  const stub = await startStub(async (req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson' });
    res.write(`${JSON.stringify(LINE_1)}\n`);
    // Keep the connection open; the client stops iterating.
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const lines = [];
    for await (const line of client.exportTraining()) {
      lines.push(line);
      break;
    }
    assert.deepEqual(lines, [LINE_1]);
  } finally {
    await stub.close();
  }
});

test('deleteTrainingRecords() returns the deleted count; 403 maps to ADMIN_REQUIRED', async () => {
  let seen;
  const stub = await startStub((req, res) => {
    seen = { method: req.method, url: req.url };
    sendJson(res, 200, { deleted: 12 });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'primary' });
    assert.deepEqual(await client.deleteTrainingRecords(), { deleted: 12 });
    assert.equal(seen.method, 'DELETE');
    assert.equal(seen.url, '/api/v1/training/records');
  } finally {
    await stub.close();
  }

  const adminStub = await startStub((req, res) => {
    sendJson(res, 403, { error: 'named tokens cannot delete records', code: 'ADMIN_REQUIRED' });
  });
  try {
    const named = new AiChatClient({ baseUrl: adminStub.url, token: 'named-token' });
    await assert.rejects(named.deleteTrainingRecords(), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 403);
      assert.equal(err.code, 'ADMIN_REQUIRED');
      return true;
    });
  } finally {
    await adminStub.close();
  }
});
