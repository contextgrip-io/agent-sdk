import { useCallback, useEffect, useRef, useState } from 'react';
import {
  ApiError,
  deleteConversation,
  getConversation,
  getStatus,
  listApprovals,
  listConversations,
  streamMessage,
} from './lib/api';
import {
  isResultError,
  looksLikeAgentConversation,
  uiMessageFromApi,
  type Conversation,
  type Status,
  type UiMessage,
} from './lib/types';
import { BoardPage } from './components/BoardPage';
import { Composer } from './components/Composer';
import { MessageList } from './components/MessageList';
import { SignIn } from './components/SignIn';
import { TrainingPanel } from './components/TrainingPanel';

const TOKEN_KEY = 'ai_chat_token';

type Auth =
  | { phase: 'checking' }
  | { phase: 'signedOut'; notice?: string }
  | { phase: 'signedIn'; token: string; status: Status };

let localIdCounter = 0;
function localId(prefix: string): string {
  return `local-${prefix}-${++localIdCounter}`;
}

export default function App() {
  const [auth, setAuth] = useState<Auth>({ phase: 'checking' });
  const [view, setView] = useState<'chat' | 'board'>('chat');
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [messages, setMessages] = useState<UiMessage[]>([]);
  const [loadingThread, setLoadingThread] = useState(false);
  const [streaming, setStreaming] = useState(false);
  const [showTraining, setShowTraining] = useState(false);
  // The Agent mode toggle. A conversation keeps the mode of its first
  // message (server-enforced), so this is only editable pre-conversation.
  const [agentMode, setAgentMode] = useState(false);
  // The id the streaming request was started under can go stale if the user
  // switches conversations mid-stream; track the active thread with a ref.
  const activeThreadRef = useRef<string | null>(null);

  const signOut = useCallback((notice?: string) => {
    localStorage.removeItem(TOKEN_KEY);
    setAuth({ phase: 'signedOut', notice });
    setConversations([]);
    setConversationId(null);
    setMessages([]);
    setStreaming(false);
    setShowTraining(false);
    setAgentMode(false);
    setView('chat');
  }, []);

  /** Sign out on expired/revoked tokens; otherwise report and continue. */
  const handleApiFailure = useCallback(
    (err: unknown): string => {
      if (err instanceof ApiError && err.status === 401) {
        signOut('Your session is no longer valid. Sign in again.');
        return err.message;
      }
      return err instanceof Error ? err.message : 'Unexpected error.';
    },
    [signOut],
  );

  // Token gate: validate a stored token against /api/v1/status on load.
  useEffect(() => {
    const stored = localStorage.getItem(TOKEN_KEY);
    if (!stored) {
      setAuth({ phase: 'signedOut' });
      return;
    }
    let cancelled = false;
    getStatus(stored)
      .then((status) => {
        if (!cancelled) setAuth({ phase: 'signedIn', token: stored, status });
      })
      .catch((err) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          localStorage.removeItem(TOKEN_KEY);
          setAuth({ phase: 'signedOut' });
        } else {
          setAuth({
            phase: 'signedOut',
            notice: 'Could not reach the API to validate the saved token. Sign in to retry.',
          });
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const refreshConversations = useCallback(
    async (token: string) => {
      try {
        setConversations(await listConversations(token));
      } catch (err) {
        handleApiFailure(err);
      }
    },
    [handleApiFailure],
  );

  // Load the conversation list once signed in.
  useEffect(() => {
    if (auth.phase === 'signedIn') void refreshConversations(auth.token);
  }, [auth, refreshConversations]);

  function onSignedIn(token: string, status: Status) {
    localStorage.setItem(TOKEN_KEY, token);
    setAuth({ phase: 'signedIn', token, status });
  }

  async function selectConversation(id: string | null) {
    setConversationId(id);
    activeThreadRef.current = id;
    if (id === null) {
      setMessages([]);
      return;
    }
    if (auth.phase !== 'signedIn') return;
    setLoadingThread(true);
    try {
      const detail = await getConversation(auth.token, id);
      // Reflect the conversation's (server-sticky) mode in the locked toggle.
      setAgentMode(detail.conversation.mode === 'agent' || looksLikeAgentConversation(detail.messages));
      let uiMessages = detail.messages.map(uiMessageFromApi);
      // A reloaded pending approval only carries its id — look the full
      // proposal up in the pending list so the card can render.
      if (uiMessages.some((m) => m.pendingApprovalId)) {
        try {
          const approvals = await listApprovals(auth.token);
          const byId = new Map(approvals.map((a) => [a.id, a]));
          uiMessages = uiMessages.map((m) =>
            m.pendingApprovalId && byId.has(m.pendingApprovalId)
              ? { ...m, approval: byId.get(m.pendingApprovalId) }
              : m,
          );
        } catch (err) {
          handleApiFailure(err);
        }
      }
      // Ignore the result if the user has moved on meanwhile.
      if (activeThreadRef.current === id) {
        setMessages(uiMessages);
      }
    } catch (err) {
      const message = handleApiFailure(err);
      if (activeThreadRef.current === id) {
        setMessages([
          { id: localId('err'), role: 'assistant', error: `Could not load conversation: ${message}` },
        ]);
      }
    } finally {
      setLoadingThread(false);
    }
  }

  async function removeCurrentConversation() {
    if (auth.phase !== 'signedIn' || conversationId === null) return;
    const current = conversations.find((c) => c.id === conversationId);
    const title = current ? `“${current.title}”` : 'this conversation';
    if (!window.confirm(`Delete ${title} and all its messages?`)) return;
    try {
      await deleteConversation(auth.token, conversationId);
      setConversationId(null);
      activeThreadRef.current = null;
      setMessages([]);
      await refreshConversations(auth.token);
    } catch (err) {
      window.alert(`Delete failed: ${handleApiFailure(err)}`);
    }
  }

  /** The server appends an outcome message — reload the thread and list. */
  function onApprovalDecided() {
    if (auth.phase !== 'signedIn') return;
    if (conversationId) void selectConversation(conversationId);
    void refreshConversations(auth.token);
  }

  async function send(question: string) {
    if (auth.phase !== 'signedIn' || streaming) return;
    const token = auth.token;
    const features = auth.status.features ?? [];
    const startedIn = conversationId;
    const assistantId = localId('assistant');
    setMessages((prev) => [
      ...prev,
      { id: localId('user'), role: 'user', text: question },
      { id: assistantId, role: 'assistant', streaming: true },
    ]);
    setStreaming(true);

    const patchAssistant = (fn: (m: UiMessage) => UiMessage) => {
      setMessages((prev) => prev.map((m) => (m.id === assistantId ? fn(m) : m)));
    };

    // If done carries pendingApprovalId but the approval_required event never
    // reached us, look the proposal up afterwards so the card still renders.
    let pendingId: string | undefined;
    let approvalSeen = false;

    try {
      await streamMessage(
        token,
        {
          question,
          ...(startedIn ? { conversationId: startedIn } : {}),
          ...(features.includes('agent')
            ? { mode: agentMode ? ('agent' as const) : ('chat' as const) }
            : {}),
        },
        {
          onMeta: ({ conversationId: cid }) => {
            // Adopt the (possibly new) conversation id without reloading.
            setConversationId(cid);
            activeThreadRef.current = cid;
          },
          onSql: (sql) => patchAssistant((m) => ({ ...m, sql })),
          onResult: (result) =>
            isResultError(result)
              ? patchAssistant((m) => ({
                  ...m,
                  resultError: result.error,
                  resultErrorTimeMs: result.executionTimeMs,
                }))
              : patchAssistant((m) => ({ ...m, result })),
          onStep: (step) => patchAssistant((m) => ({ ...m, steps: [...(m.steps ?? []), step] })),
          onApprovalRequired: (approval) => {
            approvalSeen = true;
            patchAssistant((m) => ({ ...m, approval, pendingApprovalId: approval.id }));
          },
          onDelta: (text) => patchAssistant((m) => ({ ...m, text: (m.text ?? '') + text })),
          onDone: ({ assistantMessageId, pendingApprovalId }) => {
            if (pendingApprovalId) pendingId = pendingApprovalId;
            // Adopt the real message id so the answer becomes ratable.
            patchAssistant((m) => ({
              ...m,
              serverId: assistantMessageId,
              ...(pendingApprovalId ? { pendingApprovalId } : {}),
            }));
          },
          onError: (message) => patchAssistant((m) => ({ ...m, error: message })),
        },
      );
      if (pendingId && !approvalSeen) {
        const approval = (await listApprovals(token)).find((a) => a.id === pendingId);
        if (approval) patchAssistant((m) => ({ ...m, approval }));
      }
    } catch (err) {
      // Pre-stream {error} JSON (validation/auth/not-configured) or a
      // network failure mid-stream.
      patchAssistant((m) => ({ ...m, error: handleApiFailure(err) }));
    } finally {
      patchAssistant((m) => ({ ...m, streaming: false }));
      setStreaming(false);
      void refreshConversations(token);
    }
  }

  if (auth.phase === 'checking') {
    return <div className="signin-wrap">Loading…</div>;
  }
  if (auth.phase === 'signedOut') {
    return <SignIn notice={auth.notice} onSignedIn={onSignedIn} />;
  }

  const { status } = auth;
  const features = status.features ?? [];
  const writesEnabled = status.writesEnabled ?? false;
  return (
    <div className={view === 'board' ? 'app app-wide' : 'app'}>
      <header className="header">
        <div className="header-title">
          <h1>ContextGrip AI Chat</h1>
          <span className="model-chip">{status.model}</span>
        </div>
        {features.includes('board') && (
          <nav className="view-nav">
            <button
              type="button"
              className={view === 'chat' ? 'nav-btn active' : 'nav-btn'}
              onClick={() => setView('chat')}
            >
              Chat
            </button>
            <button
              type="button"
              className={view === 'board' ? 'nav-btn active' : 'nav-btn'}
              onClick={() => setView('board')}
            >
              Board
            </button>
          </nav>
        )}
        <div className="header-actions">
          <button type="button" className="ghost-btn" onClick={() => setShowTraining((v) => !v)}>
            Training data
          </button>
          <button type="button" className="ghost-btn" onClick={() => signOut()}>
            Sign out
          </button>
        </div>
      </header>

      {!status.ready && (
        <div className="banner">
          The server is not fully configured: set <code>ANTHROPIC_API_KEY</code> and{' '}
          <code>DATABASE_URL</code> (and make sure the database is reachable) before new
          questions can be answered. You can still browse existing conversations.
        </div>
      )}

      {showTraining && <TrainingPanel token={auth.token} onApiError={handleApiFailure} />}

      {view === 'board' ? (
        <BoardPage token={auth.token} writesEnabled={writesEnabled} onApiError={handleApiFailure} />
      ) : (
        <>
          <div className="toolbar">
            <select
              value={conversationId ?? ''}
              disabled={streaming}
              onChange={(e) => void selectConversation(e.target.value === '' ? null : e.target.value)}
            >
              <option value="">New conversation</option>
              {conversations.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.title}
                </option>
              ))}
            </select>
            {conversationId !== null && (
              <button
                type="button"
                className="ghost-btn danger"
                disabled={streaming}
                onClick={() => void removeCurrentConversation()}
              >
                Delete
              </button>
            )}
          </div>

          <MessageList
            messages={messages}
            loading={loadingThread}
            token={auth.token}
            writesEnabled={writesEnabled}
            onApiError={handleApiFailure}
            onApprovalDecided={onApprovalDecided}
          />

          <Composer
            disabled={streaming}
            onSend={(q) => void send(q)}
            agentAvailable={features.includes('agent')}
            agentMode={agentMode}
            agentLocked={conversationId !== null}
            onAgentModeChange={setAgentMode}
          />
        </>
      )}

      <footer className="footer">
        runs on your compute · read-only SQL · {status.model}
      </footer>
    </div>
  );
}
