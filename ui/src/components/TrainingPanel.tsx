import { useEffect, useState } from 'react';
import {
  ApiError,
  deleteTrainingRecords,
  fetchTrainingExport,
  getTrainingCapture,
  getTrainingStats,
  setTrainingCapture,
} from '../lib/api';
import type { TrainingStats } from '../lib/types';

interface Props {
  token: string;
  /** Reports the failure (signing out on 401) and returns a display message. */
  onApiError(err: unknown): string;
}

function isAdminRequired(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403;
}

export function TrainingPanel({ token, onApiError }: Props) {
  const [capture, setCapture] = useState<boolean | null>(null);
  const [stats, setStats] = useState<TrainingStats | null>(null);
  const [adminNote, setAdminNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [ratedOnly, setRatedOnly] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [deleteArmed, setDeleteArmed] = useState(false);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    let cancelled = false;
    Promise.all([getTrainingCapture(token), getTrainingStats(token)])
      .then(([cap, st]) => {
        if (cancelled) return;
        setCapture(cap.enabled);
        setStats(st);
      })
      .catch((err) => {
        if (!cancelled) setError(onApiError(err));
      });
    return () => {
      cancelled = true;
    };
    // Loads once per open: the panel is mounted fresh each time it is shown.
  }, [token, onApiError]);

  async function toggleCapture() {
    if (capture === null) return;
    const next = !capture;
    setCapture(next); // optimistic
    setAdminNote(null);
    setError(null);
    try {
      const updated = await setTrainingCapture(token, next);
      setCapture(updated.enabled);
    } catch (err) {
      setCapture(!next); // revert
      if (isAdminRequired(err)) {
        setAdminNote('Changing capture requires the primary access token.');
      } else {
        setError(onApiError(err));
      }
    }
  }

  async function download() {
    setDownloading(true);
    setError(null);
    try {
      const blob = await fetchTrainingExport(token, ratedOnly);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = 'training-export.jsonl';
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      setError(onApiError(err));
    } finally {
      setDownloading(false);
    }
  }

  async function onDeleteClick() {
    if (!deleteArmed) {
      setDeleteArmed(true);
      return;
    }
    setDeleting(true);
    setAdminNote(null);
    setError(null);
    try {
      await deleteTrainingRecords(token);
      setStats(await getTrainingStats(token));
    } catch (err) {
      if (isAdminRequired(err)) {
        setAdminNote('Deleting records requires the primary access token.');
      } else {
        setError(onApiError(err));
      }
    } finally {
      setDeleting(false);
      setDeleteArmed(false);
    }
  }

  return (
    <section className="training-panel">
      <div className="training-row">
        <label className="training-toggle">
          <input
            type="checkbox"
            checked={capture ?? false}
            disabled={capture === null}
            onChange={() => void toggleCapture()}
          />
          Capture every exchange automatically
        </label>
        <span className="training-stats">
          {stats ? `${stats.records} records · ${stats.evaluated} rated` : 'Loading stats…'}
        </span>
      </div>
      <div className="training-row">
        <button type="button" className="ghost-btn" disabled={downloading} onClick={() => void download()}>
          {downloading ? 'Downloading…' : 'Download JSONL'}
        </button>
        <label className="training-toggle">
          <input type="checkbox" checked={ratedOnly} onChange={(e) => setRatedOnly(e.target.checked)} />
          rated only
        </label>
        <span className="training-spacer" />
        <button
          type="button"
          className="ghost-btn danger"
          disabled={deleting}
          onClick={() => void onDeleteClick()}
        >
          {deleting
            ? 'Deleting…'
            : deleteArmed
              ? `Really delete ${stats ? stats.records : 'all'} records?`
              : 'Delete all'}
        </button>
      </div>
      {adminNote && <div className="admin-note">{adminNote}</div>}
      {error && <div className="error-text">{error}</div>}
    </section>
  );
}
