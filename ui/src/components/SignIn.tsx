import { useState, type FormEvent } from 'react';
import { ApiError, getStatus } from '../lib/api';
import type { Status } from '../lib/types';

interface Props {
  /** Shown above the form, e.g. after an expired session. */
  notice?: string;
  onSignedIn(token: string, status: Status): void;
}

export function SignIn({ notice, onSignedIn }: Props) {
  const [token, setToken] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const candidate = token.trim();
    if (!candidate || busy) return;
    setBusy(true);
    setError(null);
    try {
      const status = await getStatus(candidate);
      onSignedIn(candidate, status);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError('That access token was not accepted. Check APP_ACCESS_TOKEN and try again.');
      } else {
        setError(err instanceof Error ? err.message : 'Could not reach the API.');
      }
      setBusy(false);
    }
  }

  return (
    <div className="signin-wrap">
      <form className="signin-card" onSubmit={submit}>
        <h1>ContextGrip AI Chat</h1>
        <p className="signin-sub">
          Ask your PostgreSQL database questions in plain language. Sign in with the
          service&rsquo;s access token to continue.
        </p>
        {notice && <div className="notice">{notice}</div>}
        <label className="field-label" htmlFor="token">
          Access token
        </label>
        <input
          id="token"
          type="password"
          autoComplete="current-password"
          autoFocus
          placeholder="APP_ACCESS_TOKEN"
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
        {error && <div className="error-text">{error}</div>}
        <button type="submit" disabled={busy || !token.trim()}>
          {busy ? 'Checking…' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}
