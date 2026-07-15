import { useState } from 'react';
import { ApiError, decideApproval } from '../lib/api';
import type { Approval, ResultSummary } from '../lib/types';
import { SqlBlock } from './SqlBlock';

interface Outcome {
  decision: 'approve' | 'reject';
  result?: ResultSummary;
  error?: string;
}

interface Props {
  token: string;
  approval: Approval;
  /** From status.writesEnabled — approving needs AI_CHAT_WRITE_DATABASE_URL. */
  writesEnabled: boolean;
  /** Reports the failure (signing out on 401) and returns a display message. */
  onApiError(err: unknown): string;
  /** Called after any decision lands (including ALREADY_DECIDED races). */
  onDecided(): void;
}

/**
 * Inline amber card for a proposed write: the exact SQL, the model's
 * rationale, and Approve & run / Reject. Shared by chat and the board.
 */
export function ApprovalCard({ token, approval, writesEnabled, onApiError, onDecided }: Props) {
  const [busy, setBusy] = useState<'approve' | 'reject' | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [note, setNote] = useState<string | null>(null);

  const alreadyDecided = approval.status !== 'pending';

  async function decide(decision: 'approve' | 'reject') {
    if (busy || outcome || alreadyDecided) return;
    setBusy(decision);
    setNote(null);
    try {
      const res = await decideApproval(token, approval.id, decision);
      setOutcome({ decision, result: res.result, error: res.error });
      onDecided();
    } catch (err) {
      if (err instanceof ApiError && err.code === 'ALREADY_DECIDED') {
        setNote('This proposal was already decided elsewhere.');
        onDecided();
      } else if (err instanceof ApiError && err.code === 'WRITES_DISABLED') {
        setNote('Writes are not configured on this server, so the approval could not execute.');
      } else {
        setNote(onApiError(err));
      }
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="approval-card">
      <div className="approval-title">Write approval required</div>
      <SqlBlock sql={approval.sql} />
      {approval.rationale && <div className="approval-rationale">{approval.rationale}</div>}
      {!writesEnabled && !outcome && !alreadyDecided && (
        <div className="approval-hint">
          Writes are disabled on this server: set <code>AI_CHAT_WRITE_DATABASE_URL</code> so
          approvals can execute. Rejecting still works.
        </div>
      )}
      {outcome ? (
        outcome.decision === 'reject' ? (
          <div className="approval-outcome">Rejected — the write will not run.</div>
        ) : outcome.error ? (
          <div className="stream-error">Approved, but the write failed: {outcome.error}</div>
        ) : (
          <div className="approval-outcome">
            Approved &amp; executed
            {outcome.result
              ? ` — ${outcome.result.rowCount} ${outcome.result.rowCount === 1 ? 'row' : 'rows'} affected · ${outcome.result.executionTimeMs} ms`
              : ''}
            .
          </div>
        )
      ) : alreadyDecided ? (
        <div className="approval-outcome">
          {approval.status === 'approved' ? 'Approved.' : 'Rejected.'}
        </div>
      ) : (
        <div className="approval-actions">
          <button
            type="button"
            className="approve-btn"
            disabled={!writesEnabled || busy !== null}
            onClick={() => void decide('approve')}
          >
            {busy === 'approve' ? 'Running…' : 'Approve & run'}
          </button>
          <button
            type="button"
            className="ghost-btn"
            disabled={busy !== null}
            onClick={() => void decide('reject')}
          >
            {busy === 'reject' ? 'Rejecting…' : 'Reject'}
          </button>
        </div>
      )}
      {note && <div className="approval-note">{note}</div>}
    </div>
  );
}
