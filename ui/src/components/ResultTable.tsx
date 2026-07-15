import type { ResultSummary } from '../lib/types';

const MAX_SAMPLE_ROWS = 5;

function cellText(cell: unknown): { text: string; isNull: boolean } {
  if (cell === null || cell === undefined) return { text: 'NULL', isNull: true };
  if (typeof cell === 'object') return { text: JSON.stringify(cell), isNull: false };
  return { text: String(cell), isNull: false };
}

export function ResultTable({ result }: { result: ResultSummary }) {
  const rows = (result.rowSample ?? []).slice(0, MAX_SAMPLE_ROWS);
  const columns = result.columns ?? [];
  const rowWord = result.rowCount === 1 ? 'row' : 'rows';

  return (
    <div className="result-block">
      <div className="result-header">
        {result.rowCount} {rowWord} · {result.executionTimeMs} ms
        {result.truncated ? ' · truncated' : ''}
      </div>
      {columns.length > 0 && rows.length > 0 && (
        <div className="result-scroll">
          <table>
            <thead>
              <tr>
                {columns.map((c, i) => (
                  <th key={i}>{c}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row, ri) => (
                <tr key={ri}>
                  {columns.map((_, ci) => {
                    const { text, isNull } = cellText(row[ci]);
                    return (
                      <td key={ci} className={isNull ? 'cell-null' : undefined}>
                        {text}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
