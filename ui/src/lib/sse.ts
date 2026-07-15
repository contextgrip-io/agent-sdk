// Minimal Server-Sent Events parser for the /api/v1/messages stream.
//
// Frames are separated by blank lines. Within a frame we honor `event:` and
// `data:` fields; multiple data lines are joined with "\n" (spec-correct),
// then the joined payload is parsed as JSON — every event on this API carries
// a single JSON object. Comment lines (leading ":") and unknown fields (id,
// retry, ...) are ignored. Frames without valid JSON data are skipped.

export interface SseEvent {
  event: string;
  data: unknown;
}

/**
 * Parse one raw SSE frame (the text between blank-line separators).
 * Returns null for frames that carry no data or whose data is not valid
 * JSON — callers should skip those.
 */
export function parseSseFrame(frame: string): SseEvent | null {
  let event = 'message';
  const dataLines: string[] = [];
  let sawData = false;

  for (const line of frame.split('\n')) {
    if (line === '') continue;
    if (line.startsWith(':')) continue; // comment / keep-alive

    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1); // strip one leading space

    if (field === 'event') {
      event = value;
    } else if (field === 'data') {
      dataLines.push(value);
      sawData = true;
    }
    // other fields (id, retry, ...) intentionally ignored
  }

  if (!sawData) return null;
  try {
    return { event, data: JSON.parse(dataLines.join('\n')) };
  } catch {
    return null; // malformed frame: skip
  }
}

/**
 * Incremental SSE parser. Feed it decoded text chunks as they arrive from a
 * ReadableStream; it buffers partial frames across chunk boundaries and
 * returns each completed event exactly once. Call flush() after the stream
 * ends to drain a final frame that lacked a trailing blank line.
 */
export class SseParser {
  private buffer = '';
  private pendingCR = false;

  push(chunk: string): SseEvent[] {
    // Normalize line endings to \n. A trailing lone CR is held back because
    // it may be the first half of a CRLF split across chunks.
    let text = (this.pendingCR ? '\r' : '') + chunk;
    this.pendingCR = false;
    if (text.endsWith('\r')) {
      this.pendingCR = true;
      text = text.slice(0, -1);
    }
    this.buffer += text.replace(/\r\n/g, '\n').replace(/\r/g, '\n');

    const events: SseEvent[] = [];
    let sep: number;
    while ((sep = this.buffer.indexOf('\n\n')) !== -1) {
      const frame = this.buffer.slice(0, sep);
      this.buffer = this.buffer.slice(sep + 2);
      const ev = parseSseFrame(frame);
      if (ev) events.push(ev);
    }
    return events;
  }

  /** Drain any buffered trailing frame(s) once the stream has ended. */
  flush(): SseEvent[] {
    let text = this.buffer + (this.pendingCR ? '\n' : '');
    this.buffer = '';
    this.pendingCR = false;
    text = text.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
    if (text.trim() === '') return [];

    const events: SseEvent[] = [];
    for (const frame of text.split('\n\n')) {
      const ev = parseSseFrame(frame);
      if (ev) events.push(ev);
    }
    return events;
  }
}
