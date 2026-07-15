import { useEffect, useRef, useState } from 'react';
import { evalMessage } from '../lib/api';
import type { UiMessage, Verdict } from '../lib/types';
import { ResultTable } from './ResultTable';
import { SqlBlock } from './SqlBlock';

function EvalButtons({ token, messageId }: { token: string; messageId: string }) {
  const [state, setState] = useState<'idle' | 'busy' | 'saved'>('idle');
  const [note, setNote] = useState<string | null>(null);

  async function rate(verdict: Verdict) {
    if (state !== 'idle') return;
    setState('busy');
    setNote(null);
    try {
      await evalMessage(token, messageId, verdict);
      setState('saved');
    } catch (err) {
      setState('idle');
      setNote(err instanceof Error ? err.message : 'Rating failed.');
    }
  }

  if (state === 'saved') {
    return <div className="eval-saved">✓ saved to training data</div>;
  }
  return (
    <div className="eval-row">
      <button
        type="button"
        className="eval-btn"
        disabled={state === 'busy'}
        onClick={() => void rate('good')}
      >
        👍 Good
      </button>
      <button
        type="button"
        className="eval-btn"
        disabled={state === 'busy'}
        onClick={() => void rate('bad')}
      >
        👎 Bad
      </button>
      {note && <span className="eval-note">{note}</span>}
    </div>
  );
}

function AssistantMessage({ m, token }: { m: UiMessage; token: string }) {
  const thinking = m.streaming && !m.text;
  // Only completed answers that carry SQL and a real (server) id can be
  // rated. Streamed turns get their server id from the done event; a turn
  // that ended in a terminal error never gets one.
  const ratableId =
    !m.streaming && m.sql !== undefined
      ? (m.serverId ?? (m.id.startsWith('local-') ? undefined : m.id))
      : undefined;
  return (
    <div className="message assistant">
      {m.sql !== undefined && <SqlBlock sql={m.sql} />}
      {m.result && <ResultTable result={m.result} />}
      {m.resultError && (
        <div className="query-failed">
          Query failed: {m.resultError}
          {m.resultErrorTimeMs !== undefined && (
            <span className="query-failed-ms"> · {m.resultErrorTimeMs} ms</span>
          )}
        </div>
      )}
      {m.text && <div className="answer-text">{m.text}</div>}
      {m.error && <div className="stream-error">{m.error}</div>}
      {thinking && <div className="thinking">Thinking…</div>}
      {ratableId && <EvalButtons key={ratableId} token={token} messageId={ratableId} />}
    </div>
  );
}

interface Props {
  messages: UiMessage[];
  loading: boolean;
  token: string;
}

export function MessageList({ messages, loading, token }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);

  // Keep the newest content in view as messages append and deltas stream in.
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages]);

  return (
    <div className="thread" ref={scrollRef}>
      {loading ? (
        <div className="thread-empty">Loading conversation…</div>
      ) : messages.length === 0 ? (
        <div className="thread-empty">
          Ask a question about your database. The generated SQL is always shown and runs
          read-only.
        </div>
      ) : (
        messages.map((m) =>
          m.role === 'user' ? (
            <div key={m.id} className="message user">
              <div className="user-bubble">{m.text}</div>
            </div>
          ) : (
            <AssistantMessage key={m.id} m={m} token={token} />
          ),
        )
      )}
    </div>
  );
}
