import { useState } from 'react';
import { useRelay } from '../lib/context';
import { responseHex } from '../lib/response';
import type { Header } from '../lib/types';
import { GRPCStreamTimeline } from './GRPCStreamTimeline';

function Metadata({ rows }: { rows: Header[] }) {
  return rows.length ? (
    <dl className="response-headers">
      {rows.map((row, index) => (
        <div key={index}>
          <dt>{row.name}</dt>
          <dd>
            {row.value}
            {row.name.endsWith('-bin') && <small> · Base64</small>}
          </dd>
        </div>
      ))}
    </dl>
  ) : (
    <p className="hint">Metadata отсутствует.</p>
  );
}

export function GRPCResponse({ id }: { id: string }) {
  const { grpcRuns } = useRelay();
  const run = grpcRuns[id];
  const execution = run?.execution;
  const [section, setSection] = useState<
    'body' | 'headers' | 'trailers' | 'status' | 'timing' | 'tls'
  >('body');
  const [mode, setMode] = useState<'json' | 'hex'>('json');
  const running = run?.starting || execution?.state === 'running';
  const state = run?.starting
    ? 'Подготовка…'
    : execution?.state === 'running'
      ? 'Выполняется…'
      : execution?.state === 'completed'
        ? 'Завершён'
        : execution?.state === 'canceled'
          ? 'Отменён'
          : execution?.state === 'failed' || run?.error
            ? 'Ошибка'
            : 'Нет выполнения';
  const error = run?.error || execution?.error;
  return (
    <section
      className="response-panel grpc-response"
      aria-label="Результат gRPC"
      aria-busy={running}
    >
      <div className="response-heading">
        <span>
          {run?.kind === 'reflection'
            ? 'Server reflection'
            : run?.kind === 'stream'
              ? 'Поток gRPC'
              : 'Ответ gRPC'}
        </span>
        <span className="quiet-badge" role="status">
          {state}
        </span>
        {execution && (
          <>
            {!!execution.codeName && (
              <strong className={execution.code ? 'danger-text' : 'response-status'}>
                {execution.code} {execution.codeName}
              </strong>
            )}
            <span className="hint">
              {execution.totalMs.toFixed(1)} мс
              {run.kind !== 'stream' && ` · ${execution.wireBytes.toLocaleString('ru-RU')} байт`}
            </span>
          </>
        )}
        {run?.kind === 'stream' && execution && (
          <>
            <span className="quiet-badge">
              {execution.rpcType || 'stream'} · {execution.streamState || 'connecting'}
            </span>
            <span className="hint">
              Отправлено: {execution.sentCount || 0} · Получено: {execution.receivedCount || 0}
            </span>
            {run.draining && execution.state !== 'running' && (
              <span className="hint">Чтение последних сообщений…</span>
            )}
          </>
        )}
      </div>
      {error && (
        <p className="response-error" role="alert">
          {error}
        </p>
      )}
      {run?.notice && <p className="hint response-warning">{run.notice}</p>}
      {run?.kind === 'reflection' && (error || execution?.state === 'failed') && (
        <p className="warning-text response-warning">
          Reflection недоступен. Выберите импортированную схему .proto или descriptor set и вызовите
          метод с ней. Текущая схема запроса сохранена.
        </p>
      )}
      {execution?.warnings.map((warning, index) => (
        <p className="warning-text response-warning" key={index}>
          {warning}
        </p>
      ))}
      {execution?.truncated && (
        <p className="warning-text response-warning">
          Превью ограничено. Часть ответа или status details не показана.
        </p>
      )}
      {execution?.kind === 'reflection' &&
        execution.state === 'completed' &&
        execution.schemaId && (
          <p className="hint response-warning">
            Схема обнаружена · {execution.reflectionProtocol}. Если настройки соединения изменились,
            выберите обнаруженную схему вручную.
          </p>
        )}
      {execution ? (
        <>
          <div className="panel-tabs response-tabs" role="tablist" aria-label="Данные ответа gRPC">
            {(
              [
                ['body', run.kind === 'stream' ? 'История' : 'Сообщение'],
                ['headers', `Headers (${execution.headers.length})`],
                ['trailers', `Trailers (${execution.trailers.length})`],
                ['status', `Status / details (${execution.details.length})`],
                ['timing', 'Время'],
                ['tls', 'TLS'],
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
            <span className="panel-tabs-spacer" />
            {section === 'body' && run.kind !== 'stream' && (
              <select
                aria-label="Вид ответа gRPC"
                value={mode}
                onChange={(event) => setMode(event.target.value as typeof mode)}
              >
                <option value="json">Canonical ProtoJSON</option>
                <option value="hex">Protobuf hex</option>
              </select>
            )}
          </div>
          <div className="response-content" role="tabpanel" aria-label="Данные ответа gRPC">
            {section === 'body' && run.kind === 'stream' ? (
              <GRPCStreamTimeline
                key={execution.id}
                messages={run.messages || []}
                evicted={run.evictedMessages}
              />
            ) : section === 'body' ? (
              <pre className="response-body" aria-label="Сообщение ответа gRPC">
                {mode === 'hex'
                  ? responseHex(execution.wireBase64) ||
                    (execution.wireBytes
                      ? 'Бинарные данные не показаны. См. предупреждения ответа.'
                      : '(пустое сообщение)')
                  : execution.messageJSON ||
                    (running
                      ? 'Ожидание сообщения…'
                      : execution.kind === 'reflection'
                        ? 'Сервисы и методы доступны в выборе схемы Protobuf.'
                        : execution.code
                          ? 'Сервер вернул gRPC status без сообщения.'
                          : '(пустое сообщение)')}
              </pre>
            ) : section === 'headers' ? (
              <Metadata rows={execution.headers} />
            ) : section === 'trailers' ? (
              <Metadata rows={execution.trailers} />
            ) : section === 'status' ? (
              <>
                <dl className="response-headers">
                  <div>
                    <dt>Code</dt>
                    <dd>
                      {execution.code} {execution.codeName || '—'}
                    </dd>
                  </div>
                  <div>
                    <dt>Message</dt>
                    <dd>{execution.statusMessage || '—'}</dd>
                  </div>
                  <div>
                    <dt>Method</dt>
                    <dd>{execution.method || '—'}</dd>
                  </div>
                  <div>
                    <dt>Endpoint</dt>
                    <dd>{execution.endpoint || '—'}</dd>
                  </div>
                </dl>
                {execution.details.map((detail, index) => (
                  <details className="grpc-status-detail" key={index}>
                    <summary>{detail.typeURL || `Detail ${index + 1}`}</summary>
                    {detail.error && <p className="hint">{detail.error}</p>}
                    <pre className="response-body">
                      {detail.messageJSON || responseHex(detail.wireBase64)}
                    </pre>
                    {!!detail.messageJSON && !!detail.wireBase64 && (
                      <details>
                        <summary>Protobuf hex</summary>
                        <pre className="response-body">{responseHex(detail.wireBase64)}</pre>
                      </details>
                    )}
                  </details>
                ))}
                {!execution.details.length && <p className="hint">Status details отсутствуют.</p>}
              </>
            ) : section === 'timing' ? (
              <dl className="response-headers">
                <div>
                  <dt>Соединение</dt>
                  <dd>{execution.connectMs.toFixed(1)} мс</dd>
                </div>
                <div>
                  <dt>Всего</dt>
                  <dd>{execution.totalMs.toFixed(1)} мс</dd>
                </div>
                <div>
                  <dt>Начало</dt>
                  <dd>{execution.startedAt || '—'}</dd>
                </div>
                <div>
                  <dt>Завершение</dt>
                  <dd>{execution.finishedAt || '—'}</dd>
                </div>
                <div>
                  <dt>Размер Protobuf</dt>
                  <dd>{execution.wireBytes} байт</dd>
                </div>
              </dl>
            ) : execution.tls ? (
              <dl className="response-headers">
                {Object.entries({
                  Версия: execution.tls.version,
                  Сервер: execution.tls.serverName,
                  ALPN: execution.tls.negotiatedProtocol,
                  Cipher: execution.tls.cipherSuite,
                  Проверен: execution.tls.verified ? 'Да' : 'Нет',
                  Сертификат: execution.tls.subject,
                  Издатель: execution.tls.issuer,
                  Истекает: execution.tls.expiresAt,
                  DNS: execution.tls.dnsNames.join(', '),
                }).map(([key, value]) => (
                  <div key={key}>
                    <dt>{key}</dt>
                    <dd>{value}</dd>
                  </div>
                ))}
              </dl>
            ) : (
              <p className="hint">TLS не использовался или соединение не установлено.</p>
            )}
          </div>
        </>
      ) : (
        !error && (
          <div className="response-empty">
            <span className="response-symbol" aria-hidden="true">
              ↗
            </span>
            <strong>{run?.starting ? 'Подготовка gRPC' : 'Готов к вызову'}</strong>
            <p>
              Выберите метод. Ctrl/⌘ Enter — вызвать, открыть поток или отправить сообщение; Escape
              — отменить.
            </p>
          </div>
        )
      )}
    </section>
  );
}
