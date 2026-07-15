import { useEffect, useRef } from 'react';
import type { UiMessage } from '../lib/types';
import { ResultTable } from './ResultTable';
import { SqlBlock } from './SqlBlock';

function AssistantMessage({ m }: { m: UiMessage }) {
  const thinking = m.streaming && !m.text;
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
    </div>
  );
}

interface Props {
  messages: UiMessage[];
  loading: boolean;
}

export function MessageList({ messages, loading }: Props) {
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
            <AssistantMessage key={m.id} m={m} />
          ),
        )
      )}
    </div>
  );
}
