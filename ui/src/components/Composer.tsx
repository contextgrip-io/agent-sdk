import { useState, type KeyboardEvent } from 'react';

interface Props {
  disabled: boolean;
  onSend(question: string): void;
  /** Show the Agent mode toggle (status.features includes "agent"). */
  agentAvailable: boolean;
  agentMode: boolean;
  /** A conversation keeps the mode of its first message — lock once started. */
  agentLocked: boolean;
  onAgentModeChange(on: boolean): void;
}

export function Composer({
  disabled,
  onSend,
  agentAvailable,
  agentMode,
  agentLocked,
  onAgentModeChange,
}: Props) {
  const [value, setValue] = useState('');

  function submit() {
    const question = value.trim();
    if (!question || disabled) return;
    setValue('');
    onSend(question);
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  }

  return (
    <div className="composer-block">
      {agentAvailable && (
        <div className="agent-toggle-row">
          <label
            className="training-toggle"
            title={
              agentLocked
                ? 'A conversation keeps the mode of its first message.'
                : 'Let the model take multiple read-only steps; writes become approvals.'
            }
          >
            <input
              type="checkbox"
              checked={agentMode}
              disabled={agentLocked || disabled}
              onChange={(e) => onAgentModeChange(e.target.checked)}
            />
            Agent mode
            {agentLocked && <span className="agent-locked-hint">(fixed for this conversation)</span>}
          </label>
        </div>
      )}
      <div className="composer">
        <textarea
          value={value}
          rows={2}
          maxLength={4000}
          placeholder="Ask a question about your database… (Enter to send, Shift+Enter for a new line)"
          disabled={disabled}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={onKeyDown}
        />
        <button type="button" onClick={submit} disabled={disabled || !value.trim()}>
          Send
        </button>
      </div>
    </div>
  );
}
