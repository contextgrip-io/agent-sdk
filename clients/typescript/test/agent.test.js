import test from 'node:test';
import assert from 'node:assert/strict';

import { AiChatClient, AiChatError } from '../dist/index.js';
import { startStub, readBody, sendJson, sleep } from './stub.js';

const SSE_HEADERS = {
  'content-type': 'text/event-stream',
  'cache-control': 'no-cache',
  connection: 'keep-alive',
};

const STEP_1 = {
  index: 0,
  kind: 'schema',
  summary: 'Inspected the orders and customers tables',
};
const STEP_2 = {
  index: 1,
  kind: 'query',
  summary: 'Counted June churners',
  sql: "SELECT count(*) FROM churn WHERE month = '2026-06'",
  result: { columns: ['count'], rowSample: [[3]], rowCount: 1, truncated: false, executionTimeMs: 11 },
};
const APPROVAL = {
  id: 'appr-1',
  sql: "UPDATE customers SET flagged = true WHERE churned_at >= '2026-06-01'",
  rationale: 'Flag June churners for the win-back campaign',
  status: 'pending',
  source: { conversationId: 'conv-a', messageId: 'msg-a9' },
  createdAt: '2026-07-15T09:00:00Z',
};

test('agent-mode stream: mode in request; step, approval_required, and done.pendingApprovalId dispatched in order', async () => {
  let seen;
  const stub = await startStub(async (req, res) => {
    seen = { body: JSON.parse(await readBody(req)) };
    res.writeHead(200, SSE_HEADERS);
    res.write('event: meta\ndata: {"conversationId":"conv-a","userMessageId":"u-9"}\n\n');
    await sleep(10);
    res.write(`event: step\ndata: ${JSON.stringify(STEP_1)}\n\n`);
    res.write(`event: step\ndata: ${JSON.stringify(STEP_2)}\n\n`);
    await sleep(10);
    res.write(`event: approval_required\ndata: ${JSON.stringify(APPROVAL)}\n\n`);
    res.write('event: delta\ndata: {"text":"I need approval to flag 3 customers."}\n\n');
    res.write(
      'event: done\ndata: {"conversationId":"conv-a","assistantMessageId":"msg-a9","pendingApprovalId":"appr-1"}\n\n',
    );
    res.end();
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    const events = [];
    await client.streamMessage(
      { question: 'Flag June churners', mode: 'agent' },
      {
        onMeta: (meta) => events.push(['meta', meta]),
        onSql: (sql) => events.push(['sql', sql]),
        onResult: (result) => events.push(['result', result]),
        onStep: (step) => events.push(['step', step]),
        onApprovalRequired: (approval) => events.push(['approval_required', approval]),
        onDelta: (text) => events.push(['delta', text]),
        onDone: (done) => events.push(['done', done]),
        onError: (error) => events.push(['error', error]),
      },
    );
    assert.deepEqual(seen.body, { question: 'Flag June churners', mode: 'agent' });
    assert.deepEqual(events, [
      ['meta', { conversationId: 'conv-a', userMessageId: 'u-9' }],
      ['step', STEP_1],
      ['step', STEP_2],
      ['approval_required', APPROVAL],
      ['delta', 'I need approval to flag 3 customers.'],
      ['done', { conversationId: 'conv-a', assistantMessageId: 'msg-a9', pendingApprovalId: 'appr-1' }],
    ]);
  } finally {
    await stub.close();
  }
});

test('approvals: list and decide (approve with execution result, reject)', async () => {
  const requests = [];
  const stub = await startStub(async (req, res) => {
    const entry = { method: req.method, url: req.url };
    if (req.method === 'GET' && req.url === '/api/v1/approvals') {
      sendJson(res, 200, [APPROVAL]);
    } else if (req.method === 'POST' && req.url === '/api/v1/approvals/appr-1') {
      entry.body = JSON.parse(await readBody(req));
      if (entry.body.decision === 'approve') {
        sendJson(res, 200, {
          approval: { ...APPROVAL, status: 'approved', decidedAt: '2026-07-15T09:05:00Z' },
          result: { rowCount: 3, truncated: false, executionTimeMs: 21 },
        });
      } else {
        sendJson(res, 200, {
          approval: { ...APPROVAL, status: 'rejected', decidedAt: '2026-07-15T09:05:00Z' },
        });
      }
    } else {
      sendJson(res, 404, { error: 'unknown resource', code: 'NOT_FOUND' });
    }
    requests.push(entry);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });

    const pending = await client.listApprovals();
    assert.deepEqual(pending, [APPROVAL]);

    const approved = await client.decideApproval('appr-1', 'approve');
    assert.equal(approved.approval.status, 'approved');
    assert.deepEqual(approved.result, { rowCount: 3, truncated: false, executionTimeMs: 21 });
    assert.equal(approved.error, undefined);
    assert.deepEqual(requests[1].body, { decision: 'approve' });

    const rejected = await client.decideApproval('appr-1', 'reject');
    assert.equal(rejected.approval.status, 'rejected');
    assert.equal(rejected.result, undefined);

    assert.deepEqual(
      requests.map((r) => `${r.method} ${r.url}`),
      ['GET /api/v1/approvals', 'POST /api/v1/approvals/appr-1', 'POST /api/v1/approvals/appr-1'],
    );
  } finally {
    await stub.close();
  }
});

test('decideApproval maps 409 to ALREADY_DECIDED and WRITES_DISABLED', async () => {
  for (const code of ['ALREADY_DECIDED', 'WRITES_DISABLED']) {
    const stub = await startStub((req, res) => {
      sendJson(res, 409, { error: `conflict: ${code}`, code });
    });
    try {
      const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
      await assert.rejects(client.decideApproval('appr-1', 'approve'), (err) => {
        assert.ok(err instanceof AiChatError);
        assert.equal(err.status, 409);
        assert.equal(err.code, code);
        return true;
      });
    } finally {
      await stub.close();
    }
  }
});

const TASK = {
  id: 'task-1',
  title: 'Weekly churn digest',
  prompt: 'Summarize churn for the last 7 days',
  status: 'queued',
  createdAt: '2026-07-15T08:00:00Z',
  updatedAt: '2026-07-15T08:00:00Z',
};

test('tasks: create, list (with and without status filter), get, cancel, delete', async () => {
  const requests = [];
  const stub = await startStub(async (req, res) => {
    const entry = { method: req.method, url: req.url };
    if (req.method === 'POST' && req.url === '/api/v1/tasks') {
      entry.body = JSON.parse(await readBody(req));
      sendJson(res, 201, TASK);
    } else if (req.method === 'GET' && req.url === '/api/v1/tasks') {
      sendJson(res, 200, [TASK]);
    } else if (req.method === 'GET' && req.url === '/api/v1/tasks?status=needs_approval') {
      sendJson(res, 200, [{ ...TASK, status: 'needs_approval' }]);
    } else if (req.method === 'GET' && req.url === '/api/v1/tasks/task-1') {
      sendJson(res, 200, {
        task: { ...TASK, status: 'needs_approval' },
        steps: [STEP_1, STEP_2],
        pendingApproval: { ...APPROVAL, source: { taskId: 'task-1' } },
      });
    } else if (req.method === 'POST' && req.url === '/api/v1/tasks/task-1/cancel') {
      sendJson(res, 200, { ...TASK, status: 'canceled', updatedAt: '2026-07-15T08:30:00Z' });
    } else if (req.method === 'DELETE' && req.url === '/api/v1/tasks/task-1') {
      sendJson(res, 200, { deleted: true });
    } else {
      sendJson(res, 404, { error: 'unknown resource', code: 'NOT_FOUND' });
    }
    requests.push(entry);
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });

    const created = await client.createTask('Weekly churn digest', 'Summarize churn for the last 7 days');
    assert.deepEqual(created, TASK);
    assert.deepEqual(requests[0].body, {
      title: 'Weekly churn digest',
      prompt: 'Summarize churn for the last 7 days',
    });

    assert.deepEqual(await client.listTasks(), [TASK]);
    const filtered = await client.listTasks('needs_approval');
    assert.equal(filtered[0].status, 'needs_approval');

    const detail = await client.getTask('task-1');
    assert.equal(detail.task.status, 'needs_approval');
    assert.deepEqual(detail.steps, [STEP_1, STEP_2]);
    assert.deepEqual(detail.pendingApproval.source, { taskId: 'task-1' });

    const canceled = await client.cancelTask('task-1');
    assert.equal(canceled.status, 'canceled');

    assert.equal(await client.deleteTask('task-1'), undefined);

    assert.deepEqual(
      requests.map((r) => `${r.method} ${r.url}`),
      [
        'POST /api/v1/tasks',
        'GET /api/v1/tasks',
        'GET /api/v1/tasks?status=needs_approval',
        'GET /api/v1/tasks/task-1',
        'POST /api/v1/tasks/task-1/cancel',
        'DELETE /api/v1/tasks/task-1',
      ],
    );
  } finally {
    await stub.close();
  }
});

test('deleteTask maps 409 to TASK_ACTIVE; cancelTask maps 409 to TASK_FINISHED', async () => {
  const stub = await startStub((req, res) => {
    if (req.method === 'DELETE') {
      sendJson(res, 409, { error: 'task is still running', code: 'TASK_ACTIVE' });
    } else {
      sendJson(res, 409, { error: 'task already finished', code: 'TASK_FINISHED' });
    }
  });
  try {
    const client = new AiChatClient({ baseUrl: stub.url, token: 't' });
    await assert.rejects(client.deleteTask('task-1'), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 409);
      assert.equal(err.code, 'TASK_ACTIVE');
      return true;
    });
    await assert.rejects(client.cancelTask('task-1'), (err) => {
      assert.ok(err instanceof AiChatError);
      assert.equal(err.status, 409);
      assert.equal(err.code, 'TASK_FINISHED');
      return true;
    });
  } finally {
    await stub.close();
  }
});
