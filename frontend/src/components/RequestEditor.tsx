import { useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import { useRelay } from '../lib/context';
import { hasVolatileCredentials, isDirty } from '../lib/store';
import type { GRPCDefinition, HTTPDefinition, WebSocketDefinition } from '../lib/types';
import { HTTPFields } from './HTTPFields';
import type { HTTPSection } from './HTTPFields';
import { HTTPResponse } from './HTTPResponse';
import { ProtoMessageEditor } from './ProtoMessageEditor';
import { GRPCFields } from './GRPCFields';
import type { GRPCSection } from './GRPCFields';
import { GRPCResponse } from './GRPCResponse';
import { WebSocketFields } from './WebSocketFields';
import type { WebSocketSection } from './WebSocketFields';
import { WebSocketResponse } from './WebSocketResponse';
import { webSocketActive } from '../lib/websocket';
import { PanelSeparator } from './PanelSeparator';

export function RequestEditor({ id }: { id: string }) {
  const {
    tabs,
    runs,
    grpcRuns,
    websocketRuns,
    connectWebSocket,
    disconnectWebSocket,
    cancelWebSocket,
    sendWebSocketMessage,
    pingWebSocket,
    editRequest,
    saveRequest,
    sendHTTPRequest,
    cancelHTTPRequest,
    sendGRPCRequest,
    sendGRPCStreamMessage,
    halfCloseGRPCStream,
    cancelGRPCExecution,
    discoverGRPCSchema,
  } = useRelay();
  const [section, setSection] = useState<HTTPSection>('body');
  const [grpcSection, setGRPCSection] = useState<GRPCSection>('message');
  const [wsSection, setWSSection] = useState<WebSocketSection>('message');
  const [closeCode, setCloseCode] = useState(1000);
  const [closeReason, setCloseReason] = useState('');
  const [rpcMode, setRPCMode] = useState<'none' | 'unary' | 'server' | 'client' | 'bidi'>('none');
  const [panelHeight, setPanelHeight] = useState(320);
  const tab = tabs[id];
  const latest = useRef(tab?.draft);
  latest.current = tab?.draft;
  if (!tab) return null;
  const draft = tab.draft;
  const http = draft.http;
  const grpc = draft.grpc;
  const websocket = draft.websocket;
  const updateHTTP = (patch: Partial<HTTPDefinition>) => {
    const current = latest.current;
    if (current?.http) editRequest(id, { ...current, http: { ...current.http, ...patch } });
  };
  const updateGRPC = (patch: Partial<GRPCDefinition>) => {
    const current = latest.current;
    if (current?.grpc) editRequest(id, { ...current, grpc: { ...current.grpc, ...patch } });
  };
  const updateWebSocket = (patch: Partial<WebSocketDefinition>) => {
    const current = latest.current;
    if (current?.websocket)
      editRequest(id, { ...current, websocket: { ...current.websocket, ...patch } });
  };
  const wsRun = websocketRuns[id];
  const wsActive = wsRun?.starting || webSocketActive(wsRun?.execution);
  const run = grpc ? grpcRuns[id] : runs[id];
  const running = run?.starting || run?.execution?.state === 'running';
  const grpcRun = grpcRuns[id];
  const activeStream = grpcRun?.kind === 'stream' && grpcRun.execution?.state === 'running';
  const canSend =
    activeStream &&
    grpcRun.execution?.streamState === 'open' &&
    ['client', 'bidi'].includes(grpcRun.execution.rpcType || '') &&
    !grpcRun.sending &&
    !grpcRun.halfClosing;
  return (
    <div
      className="request-editor"
      style={{ '--request-panel-height': `${panelHeight}px` } as CSSProperties}
    >
      <div className="request-heading">
        <div>
          <div className="eyebrow">{draft.protocol.toUpperCase()} / ОПРЕДЕЛЕНИЕ ЗАПРОСА</div>
          <input
            className="request-title"
            aria-label="Имя запроса"
            value={draft.name}
            onChange={(event) => editRequest(id, { ...draft, name: event.target.value })}
          />
        </div>
        <div className="save-status" role="status">
          <span className={isDirty(tab) ? 'status-dot pending' : 'status-dot'} />
          {tab.saving ? 'Сохранение…' : isDirty(tab) ? 'Есть изменения' : 'Сохранено'}
          <button className="button small" onClick={() => void saveRequest(id)}>
            Сохранить <kbd>⌘/Ctrl S</kbd>
          </button>
        </div>
      </div>
      <div className="url-bar">
        {http && (
          <>
            <input
              className="method-input"
              aria-label="HTTP метод"
              list="http-methods"
              value={http.method}
              onChange={(event) => updateHTTP({ method: event.target.value })}
            />
            <datalist id="http-methods">
              {['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'].map((method) => (
                <option key={method}>{method}</option>
              ))}
            </datalist>
            <input
              className="endpoint"
              aria-label="URL запроса"
              placeholder="{{baseUrl}}/v1/resource"
              value={http.url}
              spellCheck={false}
              onChange={(event) => updateHTTP({ url: event.target.value })}
            />
            <select
              aria-label="HTTP версия"
              value={http.version}
              onChange={(event) => updateHTTP({ version: event.target.value })}
            >
              {['auto', '1.0', '1.1', '2', 'h2c', '3'].map((version) => (
                <option key={version} value={version}>
                  {version === 'auto' ? 'Auto' : `HTTP/${version}`}
                </option>
              ))}
            </select>
          </>
        )}
        {grpc && (
          <input
            className="endpoint"
            aria-label="gRPC endpoint"
            placeholder="localhost:50051"
            value={grpc.endpoint}
            onChange={(event) => updateGRPC({ endpoint: event.target.value })}
          />
        )}
        {grpc && (
          <button
            className="button reflection-button"
            disabled={running || grpcRun?.draining || !grpc.endpoint.trim()}
            title="Получить сервисы и методы сервера с текущими TLS, metadata и deadline"
            onClick={() => void discoverGRPCSchema(id)}
          >
            Discover via Reflection
          </button>
        )}
        {draft.websocket && (
          <input
            className="endpoint"
            aria-label="WebSocket URL"
            placeholder="wss://example.com/events"
            value={draft.websocket.url}
            onChange={(event) => updateWebSocket({ url: event.target.value })}
          />
        )}
        {websocket ? (
          <>
            <button
              className={`button ${wsActive ? '' : 'primary'} send-button`}
              disabled={
                wsRun?.execution?.state === 'closing' ||
                wsRun?.disconnecting ||
                (!wsActive && (!!wsRun?.draining || !websocket.url.trim()))
              }
              onClick={() =>
                void (wsActive
                  ? disconnectWebSocket(id, closeCode, closeReason)
                  : connectWebSocket(id))
              }
            >
              {wsActive ? 'Disconnect' : 'Connect'}
            </button>
            {wsActive && (
              <button className="button" onClick={() => void cancelWebSocket(id)}>
                Abort
              </button>
            )}
          </>
        ) : (
          <button
            className={`button ${running ? '' : 'primary'} send-button`}
            disabled={!http && (!grpc || (!running && (rpcMode === 'none' || !!grpcRun?.draining)))}
            title={
              running
                ? 'Отменить · Escape'
                : http
                  ? 'Отправить · Ctrl/⌘ Enter'
                  : grpc
                    ? rpcMode === 'unary'
                      ? 'Вызвать · Ctrl/⌘ Enter'
                      : rpcMode !== 'none'
                        ? 'Открыть поток · Ctrl/⌘ Enter'
                        : 'Выберите метод RPC'
                    : 'Обмен сообщениями WebSocket планируется'
            }
            onClick={() =>
              void (grpc
                ? running
                  ? cancelGRPCExecution(id)
                  : sendGRPCRequest(id)
                : running
                  ? cancelHTTPRequest(id)
                  : sendHTTPRequest(id))
            }
          >
            {http
              ? running
                ? 'Cancel'
                : 'Send'
              : grpc
                ? running
                  ? 'Cancel'
                  : rpcMode !== 'unary' && rpcMode !== 'none'
                    ? 'Start stream'
                    : 'Call'
                : 'Connect · планируется'}
          </button>
        )}
      </div>
      {websocket && hasVolatileCredentials(tab) && (
        <p className="warning-text response-warning">
          Секретные заголовки и query остаются в памяти вкладки. Определение сохраняется без
          введённых значений; для сохранения используйте {'{{variable}}'}.
        </p>
      )}
      {websocket && (
        <div className="grpc-stream-controls websocket-controls" aria-label="Управление WebSocket">
          <span className="quiet-badge">
            {wsRun?.execution?.state || (wsRun?.starting ? 'connecting' : 'disconnected')}
          </span>
          {wsActive && (
            <span className="hint websocket-connection-url" title={wsRun?.execution?.url}>
              Настройки соединения зафиксированы при Connect.
            </span>
          )}
          <button
            className="button primary small"
            disabled={wsRun?.execution?.state !== 'open' || wsRun.sending || wsRun.disconnecting}
            title="Отправить текущий редактор · Ctrl/⌘ Enter"
            onClick={() => void sendWebSocketMessage(id)}
          >
            {wsRun?.sending ? 'Отправка…' : 'Send message'}
          </button>
          <button
            className="button small"
            disabled={wsRun?.execution?.state !== 'open' || wsRun.pinging || wsRun.disconnecting}
            onClick={() => void pingWebSocket(id)}
          >
            Ping
          </button>
          <label>
            Close code{' '}
            <input
              aria-label="WebSocket close code"
              type="number"
              min="1000"
              max="4999"
              value={closeCode}
              onChange={(event) => setCloseCode(Number(event.target.value))}
            />
          </label>
          <label>
            Close reason{' '}
            <input
              aria-label="WebSocket close reason"
              value={closeReason}
              onChange={(event) => setCloseReason(event.target.value)}
            />
          </label>
        </div>
      )}
      {activeStream && (
        <div className="grpc-stream-controls" aria-label="Управление gRPC stream">
          <span className="active-badge">{grpcRun.execution?.rpcType || 'stream'} stream</span>
          <span className="quiet-badge">{grpcRun.execution?.streamState || 'connecting'}</span>
          <span className="hint">{grpcRun.execution?.method}</span>
          <span className="panel-tabs-spacer" />
          {['client', 'bidi'].includes(grpcRun.execution?.rpcType || '') && (
            <>
              <button
                className="button primary small"
                disabled={!canSend}
                title="Отправить текущий текст редактора в открытый поток · Ctrl/⌘ Enter"
                onClick={() => void sendGRPCStreamMessage(id)}
              >
                {grpcRun.sending ? 'Отправка…' : 'Send message'}
              </button>
              <button
                className="button small"
                disabled={!canSend}
                title="Завершить отправку и продолжить получать ответы"
                onClick={() => void halfCloseGRPCStream(id)}
              >
                Half-close
              </button>
            </>
          )}
        </div>
      )}
      {grpc && (
        <>
          <div className="panel-tabs grpc-request-tabs" role="tablist" aria-label="Параметры gRPC">
            {(
              [
                ['message', 'Сообщение'],
                ['metadata', `Metadata (${grpc.metadata?.length ?? 0})`],
                ['tls', 'Соединение / TLS'],
                ['options', 'Опции'],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                role="tab"
                aria-selected={grpcSection === key}
                onClick={() => setGRPCSection(key)}
              >
                {label}
              </button>
            ))}
          </div>
          <div hidden={grpcSection !== 'message'}>
            <ProtoMessageEditor id={id} onMethodState={setRPCMode} />
          </div>
          {grpcSection !== 'message' && (
            <GRPCFields grpc={grpc} section={grpcSection} update={updateGRPC} />
          )}
        </>
      )}
      {websocket && (
        <div className="editor-panel">
          <div className="panel-tabs" role="tablist" aria-label="Параметры WebSocket">
            {(
              [
                ['message', 'Сообщение'],
                ['params', 'Параметры'],
                ['headers', 'Заголовки'],
                ['tls', 'TLS'],
                ['options', 'Опции'],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                role="tab"
                aria-selected={wsSection === key}
                onClick={() => setWSSection(key)}
              >
                {label}
              </button>
            ))}
            <span className="panel-tabs-spacer" />
            <span className="panel-hint">⌘/Ctrl Enter — Connect / Send</span>
          </div>
          <WebSocketFields definition={websocket} section={wsSection} update={updateWebSocket} />
        </div>
      )}
      {!grpc && !websocket && (
        <div className="editor-panel">
          <div className="panel-tabs" role="tablist" aria-label="Параметры запроса">
            {(http
              ? ([
                  ['params', 'Params'],
                  ['headers', 'Заголовки'],
                  ['body', 'Тело запроса'],
                  ['auth', 'Auth'],
                  ['tls', 'TLS'],
                  ['options', 'Опции'],
                ] as const)
              : ([['body', 'Тело запроса']] as const)
            ).map(([key, label]) => (
              <button
                key={key}
                role="tab"
                aria-selected={section === key}
                onClick={() => setSection(key)}
              >
                {label}
                {key === 'headers' && http && (
                  <span className="counter">{http.headers.length}</span>
                )}
              </button>
            ))}
            <span className="panel-tabs-spacer" />
            <span className="panel-hint">{http ? '⌘/Ctrl Enter' : 'Выполнение планируется'}</span>
          </div>
          {http ? (
            <HTTPFields http={http} section={section} update={updateHTTP} />
          ) : (
            <div className="inline-empty">Обмен сообщениями WebSocket планируется.</div>
          )}
        </div>
      )}
      <PanelSeparator
        label="Размер панели запроса"
        orientation="horizontal"
        value={panelHeight}
        min={220}
        max={900}
        onChange={setPanelHeight}
      />
      {http ? (
        <HTTPResponse id={id} />
      ) : grpc ? (
        <GRPCResponse id={id} />
      ) : websocket ? (
        <WebSocketResponse id={id} />
      ) : (
        <div className="response-panel" aria-label="Результат запроса">
          <div className="response-heading">Выполнение WebSocket планируется.</div>
        </div>
      )}
    </div>
  );
}
