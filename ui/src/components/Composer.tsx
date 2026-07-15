import { useState, type KeyboardEvent } from 'react';

interface Props {
  disabled: boolean;
  onSend(question: string): void;
}

export function Composer({ disabled, onSend }: Props) {
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
  );
}
