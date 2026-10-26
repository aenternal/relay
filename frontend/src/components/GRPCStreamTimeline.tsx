import { useState } from 'react';
import type { GRPCStreamMessage } from '../lib/types';
import { responseHex } from '../lib/response';

const windowSize = 50;

export function GRPCStreamTimeline({
  messages,
  evicted = 0,
}: {
  messages: GRPCStreamMessage[];
  evicted?: number;
}) {
  const [start, setStart] = useState<number | null>(null);
  const [selection, setSelection] = useState<number | null>(null);
  const [mode, setMode] = useState<'json' | 'hex'>('json');
  const maxStart = Math.max(0, messages.length - windowSize);
  const first = start === null ? maxStart : Math.min(start, maxStart);
  const visible = messages.slice(first, first + windowSize);
  const selected = messages.find((message) => message.sequence === selection) || messages.at(-1);
  return (
    <div className="grpc-timeline" aria-label="История gRPC stream">
      <div className="grpc-timeline-toolbar">
        <span className="hint">
          В памяти: {messages.length} · Освобождено: {evicted}
        </span>
        <span className="panel-tabs-spacer" />
        <button
          className="button small"
          disabled={first === 0}
          onClick={() => setStart(Math.max(0, first - windowSize))}
        >
          Предыдущие 50
        </button>
        <span className="hint">
          {messages.length ? first + 1 : 0}–{Math.min(messages.length, first + windowSize)} из{' '}
          {messages.length}
        </span>
        <button
          className="button small"
          disabled={first === maxStart}
          onClick={() => setStart(Math.min(maxStart, first + windowSize))}
        >
          Следующие 50
        </button>
        <button
          className="button small"
          onClick={() => {
            setStart(null);
            setSelection(null);
          }}
        >
          К последним
        </button>
      </div>
      {evicted > 0 && (
        <p className="hint response-warning">
          Ранние сообщения освобождены по лимиту истории или памяти. Счётчики потока включают их.
        </p>
      )}
      <div className="grpc-message-list" role="list" aria-label="Сообщения gRPC stream">
        {visible.map((message) => (
          <div role="listitem" key={message.sequence}>
            <button
              className={`grpc-message-row ${selected?.sequence === message.sequence ? 'selected' : ''}`}
              aria-pressed={selected?.sequence === message.sequence}
              aria-label={`${message.direction === 'sent' ? '→' : '←'} #${message.index} · ${message.direction === 'sent' ? 'отправлено' : 'получено'} · ${message.wireBytes} байт`}
              onClick={() => setSelection(message.sequence)}
            >
              <span className={message.direction === 'sent' ? 'sent' : 'received'}>
                {message.direction === 'sent' ? '→' : '←'} #{message.index}
              </span>
              <span>{message.direction === 'sent' ? 'Отправлено' : 'Получено'}</span>
              <time dateTime={message.timestamp}>{message.timestamp || '—'}</time>
              <span>{message.wireBytes.toLocaleString('ru-RU')} байт</span>
              {(message.truncated || message.warnings.length > 0) && (
                <span className="warning-text">⚠</span>
              )}
            </button>
          </div>
        ))}
        {!messages.length && (
          <p className="hint">Сообщений пока нет. Поток может завершиться без ответов.</p>
        )}
      </div>
      {selected && (
        <div className="grpc-message-preview">
          <div className="grpc-timeline-toolbar">
            <strong>
              {selected.direction === 'sent' ? '→' : '←'} #{selected.index}
            </strong>
            <span className="hint">
              sequence {selected.sequence} · {selected.wireBytes} байт
            </span>
            <span className="panel-tabs-spacer" />
            <select
              aria-label="Вид сообщения gRPC stream"
              value={mode}
              onChange={(event) => setMode(event.target.value as typeof mode)}
            >
              <option value="json">Canonical ProtoJSON</option>
              <option value="hex">Protobuf hex</option>
            </select>
          </div>
          {selected.warnings.map((warning, index) => (
            <p className="warning-text response-warning" key={index}>
              {warning}
            </p>
          ))}
          {selected.truncated && (
            <p className="warning-text response-warning">Превью сообщения ограничено.</p>
          )}
          {mode === 'json' && selected.messageJSON.length > 256 * 1024 && (
            <p className="hint response-warning">
              Текстовое превью: показаны первые 256 КиБ символов Canonical ProtoJSON.
            </p>
          )}
          <pre className="response-body" aria-label="Сообщение gRPC stream">
            {mode === 'hex'
              ? responseHex(selected.wireBase64) ||
                (selected.wireBytes
                  ? 'Бинарные данные не показаны. См. предупреждения.'
                  : '(пустое сообщение)')
              : selected.messageJSON.slice(0, 256 * 1024) || '(пустое сообщение)'}
          </pre>
        </div>
      )}
    </div>
  );
}
