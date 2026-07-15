import type { Step } from '../lib/types';
import { ResultTable } from './ResultTable';
import { SqlBlock } from './SqlBlock';

/**
 * Compact ordered list of agent tool steps. Rows with detail (sql, result,
 * or an error) expand via a native <details>; bare notes render flat.
 * Shared by the chat thread and the board task drawer.
 */
export function StepList({ steps }: { steps: Step[] }) {
  if (steps.length === 0) return null;
  const ordered = [...steps].sort((a, b) => a.index - b.index);
  return (
    <ol className="step-list">
      {ordered.map((s) => {
        const failed = Boolean(s.error);
        const icon = (
          <span className={failed ? 'step-icon step-failed' : 'step-icon'}>
            {failed ? '✗' : '✓'}
          </span>
        );
        const hasDetail = Boolean(s.sql || s.result || s.error);
        return (
          <li key={s.index} className="step-item">
            {hasDetail ? (
              <details>
                <summary>
                  {icon} {s.summary}
                </summary>
                <div className="step-body">
                  {s.sql && <SqlBlock sql={s.sql} />}
                  {s.result && <ResultTable result={s.result} />}
                  {s.error && <div className="stream-error">{s.error}</div>}
                </div>
              </details>
            ) : (
              <div className="step-plain">
                {icon} {s.summary}
              </div>
            )}
          </li>
        );
      })}
    </ol>
  );
}
