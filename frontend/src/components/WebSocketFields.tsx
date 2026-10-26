import type { WebSocketDefinition } from '../lib/types';
import { normalizeHTTP, normalizeWebSocket } from '../lib/types';
import { BodyEditor } from './BodyEditor';
import { FilePicker, HTTPFields, ParameterRows } from './HTTPFields';

export type WebSocketSection = 'message' | 'params' | 'headers' | 'tls' | 'options';
export function WebSocketFields({
  definition,
  section,
  update,
}: {
  definition: WebSocketDefinition;
  section: WebSocketSection;
  update: (patch: Partial<WebSocketDefinition>) => void;
}) {
  const ws = normalizeWebSocket(definition);
  if (section === 'tls')
    return (
      <HTTPFields
        http={normalizeHTTP({ tls: ws.tls })}
        section="tls"
        update={(value) => {
          if (value.tls) update({ tls: value.tls });
        }}
      />
    );
  if (section === 'headers' || section === 'params')
    return (
      <div
        className="headers-panel"
        role="tabpanel"
        aria-label={section === 'headers' ? 'Заголовки WebSocket' : 'Параметры WebSocket'}
      >
        <p className="hint">
          Повторяющиеся имена сохраняются. Для Authorization, Cookie и секретных query используйте{' '}
          {'{{variable}}'}. Введённые секреты остаются в памяти вкладки.
        </p>
        <ParameterRows
          rows={section === 'headers' ? ws.headers : ws.query}
          label={section === 'headers' ? 'заголовок WebSocket' : 'query WebSocket'}
          addLabel={section === 'headers' ? '+ Заголовок WebSocket' : '+ Query WebSocket'}
          onChange={(rows) => update(section === 'headers' ? { headers: rows } : { query: rows })}
        />
      </div>
    );
  if (section === 'options')
    return (
      <div className="http-settings options-grid" role="tabpanel" aria-label="Настройки WebSocket">
        <label>
          Subprotocols
          <input
            aria-label="WebSocket subprotocols"
            placeholder="chat, example.v1"
            value={ws.subprotocols.join(', ')}
            onChange={(event) =>
              update({
                subprotocols: event.target.value
                  .split(',')
                  .map((value) => value.trim())
                  .filter(Boolean),
              })
            }
          />
        </label>
        <label>
          Прокси WebSocket
          <select
            value={ws.proxy.mode}
            onChange={(event) => update({ proxy: { ...ws.proxy, mode: event.target.value } })}
          >
            <option value="none">Без прокси</option>
            <option value="environment">Из окружения системы</option>
            <option value="custom">Указать URL</option>
          </select>
        </label>
        {ws.proxy.mode === 'custom' && (
          <label>
            URL прокси WebSocket
            <input
              value={ws.proxy.url}
              placeholder="http://proxy:8080"
              onChange={(event) => update({ proxy: { ...ws.proxy, url: event.target.value } })}
            />
          </label>
        )}
        {(
          [
            ['connectTimeoutMs', 'WebSocket соединение, мс'],
            ['writeTimeoutMs', 'WebSocket отправка, мс'],
            ['readTimeoutMs', 'WebSocket ожидание сообщения, мс'],
            ['maxMessageBytes', 'WebSocket лимит сообщения, байт'],
          ] as const
        ).map(([key, label]) => (
          <label key={key}>
            {label}
            <input
              type="number"
              min={key === 'readTimeoutMs' ? 0 : 1}
              max={key === 'maxMessageBytes' ? 4 * 1024 * 1024 : 86400000}
              value={ws[key]}
              onChange={(event) => update({ [key]: Number(event.target.value) })}
            />
          </label>
        ))}
        <p className="hint">
          Ожидание сообщения 0 — без idle timeout. Лимит сообщения по умолчанию 1 МиБ, максимум 4
          МиБ. Ручной Ping доступен раз в секунду.
        </p>
      </div>
    );
  return (
    <div className="body-panel" role="tabpanel" aria-label="Редактор WebSocket">
      <div className="body-toolbar">
        <select
          aria-label="Тип сообщения WebSocket"
          value={ws.bodyKind}
          onChange={(event) => update({ bodyKind: event.target.value })}
        >
          <option value="text">Text</option>
          <option value="binary">Binary</option>
        </select>
        {ws.bodyKind === 'binary' && (
          <select
            aria-label="Кодировка WebSocket"
            value={ws.bodyEncoding}
            onChange={(event) => update({ bodyEncoding: event.target.value })}
          >
            <option value="hex">Hex</option>
            <option value="base64">Base64</option>
            <option value="file">Файл</option>
          </select>
        )}
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={ws.persistBody}
            onChange={(event) => update({ persistBody: event.target.checked })}
          />
          Сохранять тело на диске
        </label>
        <span className="hint">
          {ws.persistBody
            ? 'Текст редактора сохраняется в SQLite'
            : 'Сообщение только в памяти вкладки'}
        </span>
      </div>
      {ws.bodyKind === 'binary' && ws.bodyEncoding === 'file' ? (
        <div className="headers-panel">
          <FilePicker
            kind="body"
            label="Файл сообщения WebSocket"
            fileId={ws.fileId}
            onSelect={(fileId) => update({ fileId })}
          />
        </div>
      ) : (
        <BodyEditor
          label="Тело сообщения WebSocket"
          value={ws.body}
          onChange={(body) => update({ body })}
        />
      )}
    </div>
  );
}
