import { useState } from 'react';
import type { WebSocketEvent } from '../lib/types';
import { responseHex } from '../lib/response';

export function WebSocketTimeline({
  events,
  evicted = 0,
  missed = 0,
}: {
  events: WebSocketEvent[];
  evicted?: number;
  missed?: number;
}) {
  const [start, setStart] = useState<number | null>(null);
  const [selection, setSelection] = useState<number | null>(null);
  const [mode, setMode] = useState('text');
  const maximum = Math.max(0, events.length - 50),
    first = start === null ? maximum : Math.min(start, maximum);
  const selected = events.find((event) => event.sequence === selection) || events.at(-1);
  return (
    <div className="grpc-timeline" aria-label="История WebSocket">
      <div className="grpc-timeline-toolbar">
        <span className="hint">
          В памяти: {events.length} · Освобождено: {evicted} · Пропущено: {missed}
        </span>
        <span className="panel-tabs-spacer" />
        <button
          className="button small"
          disabled={first === 0}
          onClick={() => setStart(Math.max(0, first - 50))}
        >
          Предыдущие 50
        </button>
        <span className="hint">
          {events.length ? first + 1 : 0}–{Math.min(events.length, first + 50)} из {events.length}
        </span>
        <button
          className="button small"
          disabled={first === maximum}
          onClick={() => setStart(Math.min(maximum, first + 50))}
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
      {(evicted > 0 || missed > 0) && (
        <p className="warning-text response-warning">
          Часть preview истории освобождена по лимиту памяти или пропущена в очереди. Счётчики
          соединения включают эти события.
        </p>
      )}
      <div className="grpc-message-list" role="list" aria-label="События WebSocket">
        {events.slice(first, first + 50).map((event) => (
          <div role="listitem" key={event.sequence}>
            <button
              className={`grpc-message-row ${selected?.sequence === event.sequence ? 'selected' : ''}`}
              aria-pressed={selected?.sequence === event.sequence}
              aria-label={`${event.direction === 'in' ? '←' : event.direction === 'out' ? '→' : '·'} #${event.sequence} ${event.type} ${event.messageType} ${event.bytes} байт`}
              onClick={() => setSelection(event.sequence)}
            >
              <span className={event.direction === 'in' ? 'received' : 'sent'}>
                {event.direction === 'in' ? '←' : event.direction === 'out' ? '→' : '·'} #
                {event.sequence}
              </span>
              <span>
                {event.type} {event.messageType}
              </span>
              <time dateTime={event.timestamp}>{event.timestamp || '—'}</time>
              <span>
                {event.bytes.toLocaleString('ru-RU')} байт
                {event.truncated || event.warnings.length ? ' ⚠' : ''}
              </span>
            </button>
          </div>
        ))}
        {!events.length && (
          <p className="hint">Событий пока нет. Соединение может завершиться без сообщений.</p>
        )}
      </div>
      {selected && (
        <div className="grpc-message-preview">
          <div className="grpc-timeline-toolbar">
            <strong>
              #{selected.sequence} {selected.type} {selected.messageType}
            </strong>
            <span className="hint">{selected.bytes} байт</span>
            <span className="panel-tabs-spacer" />
            <select
              aria-label="Вид сообщения WebSocket"
              value={mode}
              onChange={(event) => setMode(event.target.value)}
            >
              <option value="text">Text</option>
              <option value="hex">Hex</option>
              <option value="base64">Base64</option>
            </select>
          </div>
          {selected.truncated && (
            <p className="warning-text response-warning">Превью сообщения ограничено.</p>
          )}
          {(selected.text.length > 65536 || selected.dataBase64.length > 87384) && (
            <p className="hint response-warning">Показаны первые 64 КиБ превью сообщения.</p>
          )}
          {selected.warnings.map((warning, index) => (
            <p className="warning-text response-warning" key={index}>
              {warning}
            </p>
          ))}
          <pre className="response-body" aria-label="Сообщение WebSocket">
            {mode === 'hex'
              ? responseHex(selected.dataBase64, 65536, selected.text) || '(нет payload)'
              : mode === 'base64'
                ? selected.dataBase64.slice(0, 87384) || '(бинарное превью отсутствует)'
                : selected.text.slice(0, 65536) ||
                  (selected.messageType === 'binary'
                    ? '(бинарное сообщение — выберите Hex или Base64)'
                    : '(пустое сообщение / payload не сохранён)')}
          </pre>
        </div>
      )}
    </div>
  );
}
