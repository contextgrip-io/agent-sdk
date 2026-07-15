import { useEffect, useRef, useState } from 'react';

export function SqlBlock({ sql }: { sql: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(sql);
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard unavailable (permissions / insecure context) — no-op.
    }
  }

  return (
    <div className="sql-block">
      <div className="sql-header">
        <span>SQL</span>
        <button type="button" className="copy-btn" onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre>{sql}</pre>
    </div>
  );
}
