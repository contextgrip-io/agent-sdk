/**
 * Minimal Server-Sent Events frame parsing.
 *
 * A frame is the text between two blank lines of an SSE stream. Within a
 * frame, `event:` names the event type and one or more `data:` lines carry
 * the payload; multiple data lines are joined with "\n" per the SSE spec.
 */

/** A parsed SSE frame: event type plus the joined data payload. */
export interface SseFrame {
  event: string;
  data: string;
}

/**
 * Parse one SSE frame. Returns `null` when the frame carries no data
 * (e.g. comments or heartbeats), so callers can skip it.
 *
 * - Lines are split on "\n"; a trailing "\r" is tolerated (CRLF streams).
 * - A single leading space after the field colon is stripped, per spec.
 * - Multiple `data:` lines are joined with "\n".
 * - Comment lines (starting with ":") and unknown fields (`id:`, `retry:`,
 *   ...) are ignored.
 */
export function parseSseFrame(frame: string): SseFrame | null {
  let event = 'message';
  const data: string[] = [];
  for (const raw of frame.split('\n')) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (line === '' || line.startsWith(':')) continue;
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') {
      event = value;
    } else if (field === 'data') {
      data.push(value);
    }
  }
  if (data.length === 0) return null;
  return { event, data: data.join('\n') };
}
