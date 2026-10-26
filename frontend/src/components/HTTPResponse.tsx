import { useMemo, useState } from 'react';
import { useRelay } from '../lib/context';
import { prettyJSON, responseHex } from '../lib/response';
import type { Header } from '../lib/types';

function Headers({ rows }: { rows: Header[] }) {
  return rows.length ? (
    <dl className="response-headers">
      {rows.map((header, index) => (
        <div key={index}>
          <dt>{header.name}</dt>
          <dd>{header.value}</dd>
        </div>
      ))}
    </dl>
  ) : (
    <p className="hint">Нет заголовков.</p>
  );
}

export function HTTPResponse({ id }: { id: string }) {
  const { runs, nativeFilesAvailable, saveHTTPResponse } = useRelay();
  const run = runs[id];
  const execution = run?.execution;
  const [section, setSection] = useState<
    'body' | 'headers' | 'trailers' | 'timing' | 'tls' | 'redirects'
  >('body');
  const [mode, setMode] = useState<'pretty' | 'raw' | 'hex'>('pretty');
  const [saving, setSaving] = useState(false);
  const [savedExecutionId, setSavedExecutionId] = useState('');
  const body = useMemo(() => {
    if (!execution) return '';
    if (mode === 'hex' || execution.binary)
      return responseHex(execution.bodyBase64, 65536, execution.bodyText);
    return mode === 'pretty' ? prettyJSON(execution.bodyText) : execution.bodyText;
  }, [execution, mode]);
  async function save() {
    if (!execution) return;
    setSaving(true);
    setSavedExecutionId('');
    if (await saveHTTPResponse(execution.id)) setSavedExecutionId(execution.id);
    setSaving(false);
  }
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
  return (
    <div
      className="response-panel"
      aria-label="Результат запроса"
      aria-busy={run?.starting || execution?.state === 'running'}
    >
      <div className="response-heading">
        <span>Результат</span>
        <span className="quiet-badge" role="status">
          {state}
        </span>
        {execution && (
          <>
            <strong className={execution.statusCode >= 400 ? 'danger-text' : 'response-status'}>
              {execution.statusCode > 0
                ? `${execution.statusCode} ${execution.status.replace(/^\d{3}\s*/, '')}`.trim()
                : ''}
            </strong>
            <span>{execution.protocol}</span>
            <span className="hint">
              {execution.timing.totalMs.toFixed(1)} мс ·{' '}
              {execution.receivedBytes.toLocaleString('ru-RU')} байт
            </span>
          </>
        )}
        {execution?.downloadAvailable && (
          <button
            className="button small response-save"
            disabled={!nativeFilesAvailable || saving}
            title={
              nativeFilesAvailable
                ? execution.state === 'completed'
                  ? 'Сохранить полное тело ответа'
                  : 'Сохранить полученную часть ответа'
                : 'Сохранение файлов доступно в desktop-приложении'
            }
            onClick={() => void save()}
          >
            {saving
              ? 'Сохранение…'
              : savedExecutionId === execution.id
                ? 'Сохранено'
                : execution.state === 'completed'
                  ? 'Сохранить ответ…'
                  : 'Сохранить часть ответа…'}
          </button>
        )}
      </div>
      {(run?.error || execution?.error) && (
        <p className="response-error" role="alert">
          {run?.error || execution?.error}
        </p>
      )}
      {execution?.warnings?.map((warning, index) => (
        <p className="warning-text response-warning" key={index}>
          {warning}
        </p>
      ))}
      {execution &&
        (execution.truncated ||
          (execution.downloadAvailable &&
            execution.state !== 'completed' &&
            execution.state !== 'running')) && (
          <p className="warning-text response-warning">
            {execution.truncated && (
              <>
                Превью ограничено: показано {execution.previewBytes.toLocaleString('ru-RU')} из{' '}
                {execution.receivedBytes.toLocaleString('ru-RU')} байт.{' '}
              </>
            )}
            {execution.downloadAvailable
              ? execution.state === 'completed'
                ? 'Полное тело можно сохранить в desktop-приложении.'
                : 'Загружена только часть тела ответа. Можно сохранить полученную часть в desktop-приложении.'
              : 'Полный ответ недоступен для сохранения.'}
          </p>
        )}
      {execution ? (
        <>
          <div className="panel-tabs response-tabs" role="tablist" aria-label="Данные ответа">
            {(
              [
                ['body', 'Тело'],
                ['headers', `Headers (${execution.headers?.length ?? 0})`],
                ['trailers', `Trailers (${execution.trailers?.length ?? 0})`],
                ['timing', 'Время'],
                ['tls', 'TLS'],
                ['redirects', `Редиректы (${execution.redirects?.length ?? 0})`],
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
            {section === 'body' && (
              <select
                aria-label="Вид тела ответа"
                value={execution.binary ? 'hex' : mode}
                onChange={(event) => setMode(event.target.value as typeof mode)}
                disabled={execution.binary}
              >
                <option value="pretty">Pretty</option>
                <option value="raw">Raw</option>
                <option value="hex">Hex</option>
              </select>
            )}
          </div>
          <div className="response-content" role="tabpanel" aria-label="Данные ответа">
            {section === 'body' ? (
              <>
                {execution.binary && (
                  <p className="hint">Бинарный ответ. Hex ограничен первыми 64 KiB превью.</p>
                )}
                <pre className="response-body" aria-label="Тело ответа">
                  {body || (execution.state === 'running' ? 'Ожидание тела…' : 'Пустое тело')}
                </pre>
              </>
            ) : section === 'headers' ? (
              <Headers rows={execution.headers ?? []} />
            ) : section === 'trailers' ? (
              <Headers rows={execution.trailers ?? []} />
            ) : section === 'timing' ? (
              <dl className="response-headers">
                {Object.entries({
                  DNS: execution.timing.dnsMs,
                  Соединение: execution.timing.connectMs,
                  TLS: execution.timing.tlsMs,
                  'Первый байт': execution.timing.firstByteMs,
                  Загрузка: execution.timing.downloadMs,
                  Всего: execution.timing.totalMs,
                }).map(([key, value]) => (
                  <div key={key}>
                    <dt>{key}</dt>
                    <dd>{value.toFixed(1)} мс</dd>
                  </div>
                ))}
              </dl>
            ) : section === 'redirects' ? (
              execution.redirects?.length ? (
                <ol className="redirects">
                  {execution.redirects.map((redirect, index) => (
                    <li key={index}>
                      {redirect.statusCode} · {redirect.url}
                    </li>
                  ))}
                </ol>
              ) : (
                <p className="hint">Редиректов нет.</p>
              )
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
                  DNS: execution.tls.dnsNames?.join(', '),
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
        !run?.error && (
          <div className="response-empty">
            <span className="response-symbol" aria-hidden="true">
              ↗
            </span>
            <strong>{run?.starting ? 'Подготовка запроса' : 'Готов к отправке'}</strong>
            <p>Ctrl/⌘ Enter — отправить запрос. Escape — отменить.</p>
          </div>
        )
      )}
    </div>
  );
}
