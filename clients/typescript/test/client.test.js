import test from 'node:test';
import assert from 'node:assert/strict';

import { AiChatClient, AiChatError } from '../dist/index.js';
import { startStub, readBody, sendJson } from './stub.js';

test('ask() posts the question with auth + JSON headers and returns the answer', async () => {
  let seen;
  const stub = await startStub(async (req, res) => {
    seen = {
      method: req.method,
      url: req.url,
      authorization: req.headers.authorization,
      contentType: req.headers['content-type'],
      body: await readBody(req),
    };
    sendJson(res, 200, {
      conversationId: 'conv-1',
      userMessageId: 'msg-u1',
      assistantMessageId: 'msg-a1',
      sql: 'SELECT count(*) FROM orders',
      result: {
        columns: ['count'],
        rowSample: [[42]],
        rowCount: 1,
        truncated: false,
        executionTimeMs: 7,
      },
      answer: 'There are 42 orders.',
    });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'tok-abc' });
    const resp = await client.ask({ question: 'How many orders?' });

    assert.equal(seen.method, 'POST');
    assert.equal(seen.url, '/api/v1/ask');
    assert.equal(seen.authorization, 'Bearer tok-abc');
    assert.match(seen.contentType, /^application\/json/);
    assert.deepEqual(JSON.parse(seen.body), { question: 'How many orders?' });

    assert.equal(resp.conversationId, 'conv-1');
    assert.equal(resp.userMessageId, 'msg-u1');
    assert.equal(resp.assistantMessageId, 'msg-a1');
    assert.equal(resp.sql, 'SELECT count(*) FROM orders');
    assert.deepEqual(resp.result.rowSample, [[42]]);
    assert.equal(resp.answer, 'There are 42 orders.');
  } finally {
    await stub.close();
  }
});

test('ask() passes conversationId through and surfaces resultError on HTTP 200', async () => {
  let body;
  const stub = await startStub(async (req, res) => {
    body = JSON.parse(await readBody(req));
    sendJson(res, 200, {
      conversationId: 'conv-2',
      userMessageId: 'u',
      assistantMessageId: 'a',
      sql: 'SELECT * FROM missing_table',
      resultError: 'relation "missing_table" does not exist',
      answer: 'That table does not exist.',
    });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const resp = await client.ask({ question: 'q', conversationId: 'conv-2' });
    assert.deepEqual(body, { question: 'q', conversationId: 'conv-2' });
    assert.equal(resp.result, undefined);
    assert.equal(resp.resultError, 'relation "missing_table" does not exist');
  } finally {
    await stub.close();
  }
});

test('401 maps to AiChatError with status and code UNAUTHORIZED', async () => {
  const stub = await startStub((req, res) => {
    sendJson(res, 401, { error: 'missing or invalid bearer token', code: 'UNAUTHORIZED' });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'bad' });
    await assert.rejects(client.status(), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.name, 'AiChatError');
      assert.equal(err.status, 401);
      assert.equal(err.code, 'UNAUTHORIZED');
      assert.equal(err.message, 'missing or invalid bearer token');
      return true;
    });
  } finally {
    await stub.close();
  }
});

test('non-JSON error bodies still produce an AiChatError with the status', async () => {
  const stub = await startStub((req, res) => {
    res.writeHead(502, { 'content-type': 'text/html' });
    res.end('<html>Bad Gateway</html>');
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    await assert.rejects(client.status(), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 502);
      assert.equal(err.code, undefined);
      return true;
    });
  } finally {
    await stub.close();
  }
});

test('status() hits GET /api/v1/status; no auth header without a token', async () => {
  let seen;
  const stub = await startStub((req, res) => {
    seen = { method: req.method, url: req.url, authorization: req.headers.authorization };
    sendJson(res, 200, {
      version: '0.1.0',
      model: 'claude-opus-4-8',
      engine: 'postgresql',
      ready: true,
      features: ['chat', 'agent', 'board'],
      writesEnabled: false,
    });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url });
    const status = await client.status();
    assert.equal(seen.method, 'GET');
    assert.equal(seen.url, '/api/v1/status');
    assert.equal(seen.authorization, undefined);
    assert.deepEqual(status, {
      version: '0.1.0',
      model: 'claude-opus-4-8',
      engine: 'postgresql',
      ready: true,
      features: ['chat', 'agent', 'board'],
      writesEnabled: false,
    });
  } finally {
    await stub.close();
  }
});

test('trailing slash on baseUrl does not double the path separator', async () => {
  let url;
  const stub = await startStub((req, res) => {
    url = req.url;
    sendJson(res, 200, []);
  });
  try {
    const client = new AiChatClient({ baseUrl: `${stub.url}/`, token: 't' });
    await client.listConversations();
    assert.equal(url, '/api/v1/conversations');
  } finally {
    await stub.close();
  }
});

test('conversations CRUD: list, get, delete', async () => {
  const conversation = {
    id: 'conv-9',
    title: 'Churned customers in June',
    createdAt: '2026-07-01T10:00:00Z',
    updatedAt: '2026-07-01T10:05:00Z',
  };
  const requests = [];
  const stub = await startStub((req, res) => {
    requests.push({ method: req.method, url: req.url, authorization: req.headers.authorization });
    if (req.method === 'GET' && req.url === '/api/v1/conversations') {
      sendJson(res, 200, [conversation]);
    } else if (req.method === 'GET' && req.url === '/api/v1/conversations/conv-9') {
      sendJson(res, 200, {
        conversation,
        messages: [
          { id: 'm1', role: 'user', text: 'Which customers churned in June?', createdAt: '2026-07-01T10:00:00Z' },
          {
            id: 'm2',
            role: 'assistant',
            text: 'Three customers churned.',
            sql: 'SELECT ...',
            result: { columns: ['name'], rowSample: [['acme']], rowCount: 3, truncated: false, executionTimeMs: 12 },
            createdAt: '2026-07-01T10:00:05Z',
          },
        ],
      });
    } else if (req.method === 'DELETE' && req.url === '/api/v1/conversations/conv-9') {
      sendJson(res, 200, { deleted: true });
    } else {
      sendJson(res, 404, { error: 'unknown resource', code: 'NOT_FOUND' });
    }
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'tok' });

    const list = await client.listConversations();
    assert.deepEqual(list, [conversation]);

    const detail = await client.getConversation('conv-9');
    assert.deepEqual(detail.conversation, conversation);
    assert.equal(detail.messages.length, 2);
    assert.equal(detail.messages[0].role, 'user');
    assert.equal(detail.messages[1].sql, 'SELECT ...');
    assert.equal(detail.messages[1].result.rowCount, 3);

    const deleted = await client.deleteConversation('conv-9');
    assert.equal(deleted, undefined);

    assert.deepEqual(
      requests.map((r) => `${r.method} ${r.url}`),
      [
        'GET /api/v1/conversations',
        'GET /api/v1/conversations/conv-9',
        'DELETE /api/v1/conversations/conv-9',
      ],
    );
    assert.ok(requests.every((r) => r.authorization === 'Bearer tok'));
  } finally {
    await stub.close();
  }
});

test('getConversation() URL-encodes the id and maps 404 to NOT_FOUND', async () => {
  let url;
  const stub = await startStub((req, res) => {
    url = req.url;
    sendJson(res, 404, { error: 'unknown conversation', code: 'NOT_FOUND' });
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    await assert.rejects(client.getConversation('a/b c'), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 404);
      assert.equal(err.code, 'NOT_FOUND');
      return true;
    });
    assert.equal(url, '/api/v1/conversations/a%2Fb%20c');
  } finally {
    await stub.close();
  }
});

test('token admin endpoints: list, create, revoke, and ADMIN_REQUIRED mapping', async () => {
  const info = {
    id: 'tok-1',
    label: 'reporting-cron',
    fingerprint: 'ab12cd34',
    createdAt: '2026-07-01T09:00:00Z',
  };
  const requests = [];
  const stub = await startStub(async (req, res) => {
    const entry = { method: req.method, url: req.url };
    if (req.method === 'GET' && req.url === '/api/v1/tokens') {
      sendJson(res, 200, [{ ...info, lastUsedAt: '2026-07-02T09:00:00Z' }]);
    } else if (req.method === 'POST' && req.url === '/api/v1/tokens') {
      entry.body = JSON.parse(await readBody(req));
      sendJson(res, 201, { ...info, token: 'raw-secret-token' });
    } else if (req.method === 'DELETE' && req.url === '/api/v1/tokens/tok-1') {
      sendJson(res, 200, { deleted: true });
    } else {
      sendJson(res, 404, { error: 'unknown resource', code: 'NOT_FOUND' });
    }
    requests.push(entry);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 'primary' });

    const tokens = await client.listTokens();
    assert.equal(tokens.length, 1);
    assert.equal(tokens[0].fingerprint, 'ab12cd34');
    assert.equal(tokens[0].lastUsedAt, '2026-07-02T09:00:00Z');

    const created = await client.createToken('reporting-cron');
    assert.equal(created.token, 'raw-secret-token');
    assert.equal(created.label, 'reporting-cron');
    assert.deepEqual(requests[1].body, { label: 'reporting-cron' });

    assert.equal(await client.revokeToken('tok-1'), undefined);
    assert.deepEqual(
      requests.map((r) => `${r.method} ${r.url}`),
      ['GET /api/v1/tokens', 'POST /api/v1/tokens', 'DELETE /api/v1/tokens/tok-1'],
    );
  } finally {
    await stub.close();
  }

  const adminStub = await startStub((req, res) => {
    sendJson(res, 403, { error: 'named tokens cannot manage tokens', code: 'ADMIN_REQUIRED' });
  });
  try {
    const named = new AiChatClient({ baseUrl: adminStub.url, token: 'named-token' });
    await assert.rejects(named.createToken('nope'), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 403);
      assert.equal(err.code, 'ADMIN_REQUIRED');
      return true;
    });
  } finally {
    await adminStub.close();
  }
});
