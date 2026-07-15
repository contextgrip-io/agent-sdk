// Shared node:http stub-server helper for the client tests.
import { createServer } from 'node:http';

/**
 * Start a stub HTTP server on an ephemeral port.
 * @param {(req: import('node:http').IncomingMessage, res: import('node:http').ServerResponse) => void} handler
 * @returns {Promise<{ url: string, close: () => Promise<void> }>}
 */
export async function startStub(handler) {
  const server = createServer(handler);
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

/** Read a request body to a UTF-8 string. */
export function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

/** JSON response helper. */
export function sendJson(res, status, body) {
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(JSON.stringify(body));
}

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
