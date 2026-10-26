import { useEffect, useState } from 'react';
import { useRelay } from '../lib/context';
import { webSocketActive } from '../lib/websocket';
import { WebSocketTimeline } from './WebSocketTimeline';

export function WebSocketResponse({ id }: { id: string }) {
  const {
    websocketRuns,
    workspaceId,
    websocketSessions,
    savedWebSocketSession,
    websocketHistoryLoading,
    websocketHistoryError,
    refreshWebSocketSessions,
    saveWebSocketSession,
    openSavedWebSocketSession,
    closeSavedWebSocketSession,
    deleteSavedWebSocketSession,
  } = useRelay();
  const run = websocketRuns[id],
    execution = run?.execution;
  const [section, setSection] = useState('events'),
    [includePayloads, setIncludePayloads] = useState(false),
    [saving, setSaving] = useState(false);
  useEffect(() => {
    void refreshWebSocketSessions(workspaceId);
  }, [workspaceId, refreshWebSocketSessions]);
  return (
    <div className="response-panel websocket-response" aria-label="Результат WebSocket">
      <div className="response-heading">
        <strong>{execution?.state || (run?.starting ? 'connecting' : 'WebSocket')}</strong>
        {execution && (
          <>
            <span className="quiet-badge">HTTP {execution.handshakeStatusCode || '—'}</span>
            <span className="hint">
              ↑ {execution.sentCount} / {execution.sentBytes} байт · ↓ {execution.receivedCount} /{' '}
              {execution.receivedBytes} байт · {execution.totalMs.toFixed(1)} мс
            </span>
            {execution.subprotocol && <span className="quiet-badge">{execution.subprotocol}</span>}
          </>
        )}
      </div>
      {run?.error && (
        <p role="alert" className="error-text response-warning">
          {run.error}
        </p>
      )}
      {run?.notice && (
        <p role="status" className="hint response-warning">
          {run.notice}
        </p>
      )}
      {execution?.error && (
        <p role="alert" className="error-text response-warning">
          {execution.error}
        </p>
      )}
      {execution?.warnings.map((warning, index) => (
        <p className="warning-text response-warning" key={index}>
          {warning}
        </p>
      ))}
      {!!execution?.droppedEvents && (
        <p className="warning-text response-warning">
          Очередь Go: пропущено событий {execution.droppedEvents}.
        </p>
      )}
      {execution && (
        <>
          <div className="panel-tabs" role="tablist" aria-label="Ответ WebSocket">
            {(
              [
                ['events', 'События'],
                ['handshake', 'Handshake'],
                ['connection', 'Соединение'],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                role="tab"
                aria-selected={section === key}
                onClick={() => setSection(key)}
              >
                {label}
              </button>
            ))}
          </div>
          {section === 'events' ? (
            <WebSocketTimeline
              events={run?.events || []}
              evicted={run?.evictedEvents}
              missed={run?.missedEvents}
            />
          ) : section === 'handshake' ? (
            <div className="headers-panel">
              <p className="hint">
                {execution.url} · HTTP {execution.handshakeStatusCode}
              </p>
              <table className="response-headers">
                <tbody>
                  {execution.handshakeHeaders.map((header, index) => (
                    <tr key={index}>
                      <th>{header.name}</th>
                      <td>{header.value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <div className="headers-panel">
              <p>
                Connect: {execution.connectMs.toFixed(1)} мс · Всего: {execution.totalMs.toFixed(1)}{' '}
                мс
              </p>
              <p>
                Close code: {execution.closeCode || '—'} ·{' '}
                {execution.cleanClose ? 'clean close' : 'без подтверждённого close handshake'}
              </p>
              <p>Close reason: {execution.closeReason || '—'}</p>
              {execution.tls && (
                <>
                  <p>
                    {execution.tls.version} · {execution.tls.negotiatedProtocol || '—'} ·{' '}
                    {execution.tls.verified ? 'Сертификат проверен' : 'Сертификат не проверен'}
                  </p>
                  <p>
                    {execution.tls.subject} · {execution.tls.issuer}
                  </p>
                </>
              )}
            </div>
          )}
          <div className="websocket-history-actions">
            <label className="checkbox-label">
              <input
                type="checkbox"
                checked={includePayloads}
                onChange={(event) => setIncludePayloads(event.target.checked)}
              />
              Включить payloads в историю
            </label>
            <button
              className="button small"
              disabled={saving || webSocketActive(execution) || run?.draining}
              onClick={async () => {
                setSaving(true);
                await saveWebSocketSession(id, includePayloads);
                setSaving(false);
              }}
            >
              Сохранить сессию
            </button>
            <span className="hint">
              Сохранение после закрытия. По умолчанию только metadata; payloads — обработанные
              ограниченные preview.
            </span>
          </div>
        </>
      )}
      {!execution && !run?.starting && (
        <p className="inline-empty">
          Нажмите Connect, затем отправьте сообщение. Соединение остаётся открытым до Disconnect или
          Abort.
        </p>
      )}
      <details className="websocket-history">
        <summary>Сохранённые сессии WebSocket ({websocketSessions.length})</summary>
        <div className="websocket-history-actions">
          <button
            className="button small"
            disabled={websocketHistoryLoading}
            onClick={() => void refreshWebSocketSessions(workspaceId)}
          >
            Обновить сессии
          </button>
          {websocketHistoryLoading && <span role="status">Загрузка…</span>}
        </div>
        {websocketHistoryError && (
          <p className="error-text" role="alert">
            {websocketHistoryError}
          </p>
        )}
        {websocketSessions.map((session) => (
          <div className="websocket-session-row" key={session.id}>
            <span>
              {session.savedAt} · {session.url} · {session.state} · {session.eventCount} событий ·{' '}
              {session.includePayloads ? 'с payloads' : 'без payloads'}
            </span>
            <button
              className="button small"
              aria-label={`Открыть сессию ${session.id}`}
              onClick={() => void openSavedWebSocketSession(workspaceId, session.id)}
            >
              Открыть сессию
            </button>
            <button
              className="button small"
              aria-label={`Удалить сессию ${session.id}`}
              onClick={() => void deleteSavedWebSocketSession(workspaceId, session.id)}
            >
              Удалить сессию
            </button>
          </div>
        ))}
        {savedWebSocketSession && (
          <div className="websocket-saved-preview" aria-label="Сохранённая сессия WebSocket">
            <div className="websocket-history-actions">
              <strong>
                {savedWebSocketSession.execution.url} · {savedWebSocketSession.execution.state}
              </strong>
              <button className="button small" onClick={closeSavedWebSocketSession}>
                Закрыть просмотр сессии
              </button>
            </div>
            <p className="hint">
              {savedWebSocketSession.includePayloads ? 'С payloads' : 'Payloads не сохранены'} · Не
              вошло в историю: {savedWebSocketSession.omittedEvents} · Close:{' '}
              {savedWebSocketSession.execution.closeCode}
            </p>
            <WebSocketTimeline
              key={savedWebSocketSession.id}
              events={savedWebSocketSession.events}
              evicted={savedWebSocketSession.omittedEvents}
            />
          </div>
        )}
      </details>
    </div>
  );
}
