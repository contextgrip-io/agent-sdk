import { useCallback, useEffect, useState } from 'react';
import { cancelTask, createTask, deleteTask, getTask, listTasks } from '../lib/api';
import type { Task, TaskDetail, TaskStatus } from '../lib/types';
import { ApprovalCard } from './ApprovalCard';
import { StepList } from './StepList';

const POLL_MS = 4000;

const COLUMNS: { title: string; statuses: TaskStatus[] }[] = [
  { title: 'Queued', statuses: ['queued'] },
  { title: 'Running', statuses: ['running'] },
  { title: 'Needs approval', statuses: ['needs_approval'] },
  { title: 'Done', statuses: ['done', 'failed', 'canceled'] },
];

const STATUS_LABEL: Record<TaskStatus, string> = {
  queued: 'queued',
  running: 'running',
  needs_approval: 'needs approval',
  done: 'done',
  failed: 'failed',
  canceled: 'canceled',
};

const ACTIVE_STATUSES: TaskStatus[] = ['queued', 'running', 'needs_approval'];

function StatusPill({ status }: { status: TaskStatus }) {
  return <span className={`pill pill-${status}`}>{STATUS_LABEL[status]}</span>;
}

interface Props {
  token: string;
  writesEnabled: boolean;
  /** Reports the failure (signing out on 401) and returns a display message. */
  onApiError(err: unknown): string;
}

export function BoardPage({ token, writesEnabled, onApiError }: Props) {
  const [tasks, setTasks] = useState<Task[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [title, setTitle] = useState('');
  const [prompt, setPrompt] = useState('');
  const [creating, setCreating] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState(false);

  const refresh = useCallback(async () => {
    try {
      setTasks(await listTasks(token));
      setListError(null);
    } catch (err) {
      setListError(onApiError(err));
    }
  }, [token, onApiError]);

  const loadDetail = useCallback(
    async (id: string) => {
      try {
        setDetail(await getTask(token, id));
        setDetailError(null);
      } catch (err) {
        setDetailError(onApiError(err));
      }
    },
    [token, onApiError],
  );

  // Poll the task list every 4s while the board is visible (mounted).
  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), POLL_MS);
    return () => window.clearInterval(timer);
  }, [refresh]);

  // Keep the open drawer live too (tasks progress in the background).
  useEffect(() => {
    if (!selectedId) {
      setDetail(null);
      setDetailError(null);
      return;
    }
    void loadDetail(selectedId);
    const timer = window.setInterval(() => void loadDetail(selectedId), POLL_MS);
    return () => window.clearInterval(timer);
  }, [selectedId, loadDetail]);

  async function submitTask() {
    const t = title.trim();
    const p = prompt.trim();
    if (!t || !p || creating) return;
    setCreating(true);
    try {
      await createTask(token, { title: t, prompt: p });
      setTitle('');
      setPrompt('');
      setShowForm(false);
      await refresh();
    } catch (err) {
      setListError(onApiError(err));
    } finally {
      setCreating(false);
    }
  }

  async function cancelSelected(task: Task) {
    if (actionBusy) return;
    setActionBusy(true);
    try {
      await cancelTask(token, task.id);
      await Promise.all([refresh(), loadDetail(task.id)]);
    } catch (err) {
      setDetailError(onApiError(err));
    } finally {
      setActionBusy(false);
    }
  }

  async function deleteSelected(task: Task) {
    if (actionBusy) return;
    if (!window.confirm(`Delete task “${task.title}”?`)) return;
    setActionBusy(true);
    try {
      await deleteTask(token, task.id);
      setSelectedId(null);
      await refresh();
    } catch (err) {
      setDetailError(onApiError(err));
    } finally {
      setActionBusy(false);
    }
  }

  return (
    <div className="board">
      <div className="board-bar">
        <button type="button" className="ghost-btn" onClick={() => setShowForm((v) => !v)}>
          {showForm ? 'Close' : 'New task'}
        </button>
        {listError && <span className="error-inline">{listError}</span>}
      </div>

      {showForm && (
        <div className="task-form">
          <input
            type="text"
            maxLength={200}
            placeholder="Title"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
          />
          <textarea
            rows={3}
            maxLength={4000}
            placeholder="What should the agent do? Proposed writes will wait for your approval."
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
          />
          <div className="task-form-actions">
            <button
              type="button"
              className="primary-btn"
              disabled={creating || !title.trim() || !prompt.trim()}
              onClick={() => void submitTask()}
            >
              {creating ? 'Filing…' : 'File task'}
            </button>
          </div>
        </div>
      )}

      <div className="board-columns">
        {COLUMNS.map((col) => {
          const colTasks = (tasks ?? []).filter((t) => col.statuses.includes(t.status));
          return (
            <section key={col.title} className="board-column">
              <h2>
                {col.title} <span className="column-count">{colTasks.length}</span>
              </h2>
              {tasks === null ? (
                <div className="column-empty">Loading…</div>
              ) : colTasks.length === 0 ? (
                <div className="column-empty">—</div>
              ) : (
                colTasks.map((t) => (
                  <button
                    key={t.id}
                    type="button"
                    className="task-card"
                    onClick={() => setSelectedId(t.id)}
                  >
                    <span className="task-card-title">{t.title}</span>
                    <StatusPill status={t.status} />
                  </button>
                ))
              )}
            </section>
          );
        })}
      </div>

      {selectedId && (
        <div className="drawer-backdrop" onClick={() => setSelectedId(null)}>
          <aside className="drawer" onClick={(e) => e.stopPropagation()}>
            {detail === null ? (
              <div className="thread-empty">{detailError ?? 'Loading task…'}</div>
            ) : (
              <>
                <div className="drawer-head">
                  <h2>{detail.task.title}</h2>
                  <button type="button" className="ghost-btn" onClick={() => setSelectedId(null)}>
                    Close
                  </button>
                </div>
                <div className="drawer-meta">
                  <StatusPill status={detail.task.status} />
                </div>
                <div className="task-prompt">{detail.task.prompt}</div>
                {detail.steps.length > 0 && <StepList steps={detail.steps} />}
                {detail.task.answer && <div className="answer-text">{detail.task.answer}</div>}
                {detail.task.error && <div className="stream-error">{detail.task.error}</div>}
                {detail.pendingApproval && (
                  <ApprovalCard
                    key={detail.pendingApproval.id}
                    token={token}
                    approval={detail.pendingApproval}
                    writesEnabled={writesEnabled}
                    onApiError={onApiError}
                    onDecided={() => {
                      void refresh();
                      void loadDetail(detail.task.id);
                    }}
                  />
                )}
                {detailError && <div className="error-text">{detailError}</div>}
                <div className="drawer-actions">
                  {ACTIVE_STATUSES.includes(detail.task.status) ? (
                    <button
                      type="button"
                      className="ghost-btn danger"
                      disabled={actionBusy}
                      onClick={() => void cancelSelected(detail.task)}
                    >
                      Cancel task
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="ghost-btn danger"
                      disabled={actionBusy}
                      onClick={() => void deleteSelected(detail.task)}
                    >
                      Delete task
                    </button>
                  )}
                </div>
              </>
            )}
          </aside>
        </div>
      )}
    </div>
  );
}
