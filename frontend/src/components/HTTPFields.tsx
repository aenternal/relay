import { useEffect, useId, useRef, useState } from 'react';
import { useRelay } from '../lib/context';
import type { Header, HTTPAuth, HTTPDefinition } from '../lib/types';
import { BodyEditor } from './BodyEditor';

const HTTP_HEADER_NAMES = [
  'Accept',
  'Accept-Encoding',
  'Accept-Language',
  'Authorization',
  'Cache-Control',
  'Content-Type',
  'Cookie',
  'If-Match',
  'If-Modified-Since',
  'If-None-Match',
  'Origin',
  'Referer',
  'User-Agent',
  'X-API-Key',
  'X-Request-ID',
];

export type HTTPSection = 'body' | 'params' | 'headers' | 'auth' | 'tls' | 'options';

export function ParameterRows({
  rows,
  onChange,
  label,
  addLabel,
  suggestions,
}: {
  rows: Header[];
  onChange: (rows: Header[]) => void;
  label: string;
  addLabel: string;
  suggestions?: readonly string[];
}) {
  const listId = useId();
  const update = (index: number, patch: Partial<Header>) =>
    onChange(rows.map((item, i) => (i === index ? { ...item, ...patch } : item)));
  return (
    <div className="parameter-rows">
      {suggestions && (
        <datalist id={listId}>
          {suggestions.map((name) => (
            <option key={name} value={name} />
          ))}
        </datalist>
      )}
      {rows.map((row, index) => (
        <div className="header-row" key={index}>
          <input
            type="checkbox"
            aria-label={`Включить ${label} ${index + 1}`}
            checked={row.enabled}
            onChange={(event) => update(index, { enabled: event.target.checked })}
          />
          <input
            aria-label={`Имя ${label} ${index + 1}`}
            list={suggestions?.length ? listId : undefined}
            placeholder="Имя"
            value={row.name}
            onChange={(event) => update(index, { name: event.target.value })}
          />
          <input
            aria-label={`Значение ${label} ${index + 1}`}
            placeholder="Значение или {{variable}}"
            value={row.value}
            onChange={(event) => update(index, { value: event.target.value })}
          />
          <button
            className="icon-button"
            aria-label={`Удалить ${label} ${index + 1}`}
            onClick={() => onChange(rows.filter((_, i) => i !== index))}
          >
            ×
          </button>
        </div>
      ))}
      <button
        className="button small"
        onClick={() => onChange([...rows, { name: '', value: '', enabled: true }])}
      >
        {addLabel}
      </button>
    </div>
  );
}

export function FilePicker({
  kind,
  fileId,
  onSelect,
  label,
}: {
  kind: string;
  fileId: string;
  onSelect: (id: string, name: string) => void;
  label: string;
}) {
  const { nativeFilesAvailable, chooseHTTPFile } = useRelay();
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const selection = useRef(0);
  useEffect(
    () => () => {
      selection.current++;
    },
    [],
  );
  async function select() {
    const generation = ++selection.current;
    setBusy(true);
    const file = await chooseHTTPFile(kind);
    if (generation !== selection.current) return;
    setBusy(false);
    if (file?.id) {
      setName(file.name);
      onSelect(file.id, file.name);
    }
  }
  return (
    <div className="file-picker">
      <button
        className="button small"
        disabled={!nativeFilesAvailable || busy}
        onClick={() => void select()}
      >
        {busy ? 'Открытие…' : label}
      </button>
      <span className="hint">
        {!nativeFilesAvailable
          ? 'Выбор файлов доступен в desktop-приложении.'
          : fileId
            ? name || 'Файл выбран для текущего запуска'
            : 'Файл не выбран. После перезапуска выберите его заново.'}
      </span>
      {fileId && (
        <button
          className="icon-button"
          aria-label={`Убрать файл: ${label}`}
          onClick={() => {
            selection.current++;
            setBusy(false);
            setName('');
            onSelect('', '');
          }}
        >
          ×
        </button>
      )}
    </div>
  );
}

export function HTTPAuthFields({
  auth,
  onChange,
}: {
  auth: HTTPAuth;
  onChange: (auth: HTTPAuth) => void;
}) {
  const field = (
    key: 'username' | 'password' | 'token' | 'keyName' | 'keyValue',
    label: string,
    secret = false,
  ) => (
    <label>
      {label}
      <input
        type={secret ? 'password' : 'text'}
        autoComplete="off"
        spellCheck={false}
        value={auth[key]}
        placeholder={secret ? '{{secret}}' : ''}
        onChange={(event) => onChange({ ...auth, [key]: event.target.value })}
      />
    </label>
  );
  return (
    <>
      <label>
        Тип авторизации
        <select
          value={auth.type}
          onChange={(event) => onChange({ ...auth, type: event.target.value })}
        >
          <option value="none">Без авторизации</option>
          <option value="inherit">Наследовать от папки / коллекции</option>
          <option value="basic">Basic</option>
          <option value="bearer">Bearer token</option>
          <option value="api-key">API key</option>
        </select>
      </label>
      <p className="hint">
        Секреты берутся из активного окружения через <code>{'{{variable}}'}</code>. Пароль, токен и
        API key, введённые напрямую, остаются в памяти вкладки.
      </p>
      {auth.type === 'inherit' && (
        <p className="hint">
          Используется авторизация ближайшей папки, затем коллекции. Если авторизация не настроена,
          запрос отправляется без неё.
        </p>
      )}
      {auth.type === 'basic' && (
        <>
          {field('username', 'Имя пользователя')}
          {field('password', 'Пароль', true)}
        </>
      )}
      {auth.type === 'bearer' && field('token', 'Bearer token', true)}
      {auth.type === 'api-key' && (
        <>
          {field('keyName', 'Имя API key')}
          {field('keyValue', 'Значение API key', true)}
          <label>
            Добавить API key в
            <select
              value={auth.in}
              onChange={(event) => onChange({ ...auth, in: event.target.value })}
            >
              <option value="header">Header</option>
              <option value="query">Query</option>
            </select>
          </label>
        </>
      )}
    </>
  );
}

export function HTTPFields({
  http,
  section,
  update,
}: {
  http: HTTPDefinition;
  section: HTTPSection;
  update: (value: Partial<HTTPDefinition>) => void;
}) {
  if (section === 'params')
    return (
      <div className="headers-panel" role="tabpanel" aria-label="Параметры">
        <h3>Query параметры</h3>
        <p className="hint">
          Добавляются к URL в указанном порядке. Повторяющиеся имена сохраняются.
        </p>
        <ParameterRows
          rows={http.query}
          label="query параметр"
          addLabel="+ Query параметр"
          onChange={(query) => update({ query })}
        />
        <h3>Параметры пути</h3>
        <p className="hint">
          В URL используйте <code>{'{id}'}</code> или <code>:id</code>. Значения кодируются как
          сегменты пути.
        </p>
        <ParameterRows
          rows={http.pathParams}
          label="параметр пути"
          addLabel="+ Параметр пути"
          onChange={(pathParams) => update({ pathParams })}
        />
      </div>
    );
  if (section === 'headers')
    return (
      <div className="headers-panel" role="tabpanel" aria-label="Заголовки">
        <p className="hint">
          Для Authorization, Cookie и API keys используйте ссылки на секреты:{' '}
          <code>{'{{token}}'}</code>.
        </p>
        <ParameterRows
          rows={http.headers}
          suggestions={HTTP_HEADER_NAMES}
          label="заголовок"
          addLabel="+ Заголовок"
          onChange={(headers) => update({ headers })}
        />
      </div>
    );
  if (section === 'auth')
    return (
      <div className="http-settings" role="tabpanel" aria-label="Авторизация">
        <HTTPAuthFields auth={http.auth} onChange={(auth) => update({ auth })} />
      </div>
    );
  if (section === 'tls')
    return (
      <div className="http-settings" role="tabpanel" aria-label="TLS">
        <label>
          Server name (SNI)
          <input
            value={http.tls.serverName}
            placeholder="Имя из URL по умолчанию"
            onChange={(event) => update({ tls: { ...http.tls, serverName: event.target.value } })}
          />
        </label>
        <FilePicker
          kind="ca"
          label="Корневой CA (PEM)"
          fileId={http.tls.caFileId}
          onSelect={(caFileId) => update({ tls: { ...http.tls, caFileId } })}
        />
        <FilePicker
          kind="certificate"
          label="Клиентский сертификат (PEM)"
          fileId={http.tls.clientCertFileId}
          onSelect={(clientCertFileId) => update({ tls: { ...http.tls, clientCertFileId } })}
        />
        <FilePicker
          kind="private-key"
          label="Приватный ключ (PEM)"
          fileId={http.tls.clientKeyFileId}
          onSelect={(clientKeyFileId) => update({ tls: { ...http.tls, clientKeyFileId } })}
        />
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={http.tls.insecure}
            onChange={(event) => update({ tls: { ...http.tls, insecure: event.target.checked } })}
          />
          Отключить проверку TLS-сертификата (insecure)
        </label>
        {http.tls.insecure && (
          <p className="warning-text" role="alert">
            Проверка сертификата отключена. Соединение уязвимо для подмены сервера.
          </p>
        )}
      </div>
    );
  if (section === 'options')
    return (
      <div className="http-settings options-grid" role="tabpanel" aria-label="Настройки HTTP">
        <label>
          Прокси
          <select
            value={http.proxy.mode}
            onChange={(event) => update({ proxy: { ...http.proxy, mode: event.target.value } })}
          >
            <option value="none">Без прокси</option>
            <option value="environment">Из окружения системы</option>
            <option value="custom">Указать URL</option>
          </select>
        </label>
        {http.proxy.mode === 'custom' && (
          <label>
            URL прокси
            <input
              value={http.proxy.url}
              placeholder="http://, https:// или socks5://"
              onChange={(event) => update({ proxy: { ...http.proxy, url: event.target.value } })}
            />
          </label>
        )}
        {(
          [
            'timeoutMs',
            'connectTimeoutMs',
            'tlsHandshakeTimeoutMs',
            'responseHeaderTimeoutMs',
            'maxRedirects',
            'maxDownloadBytes',
          ] as const
        ).map((key) => (
          <label key={key}>
            {
              {
                timeoutMs: 'Общий таймаут, мс',
                connectTimeoutMs: 'Соединение, мс',
                tlsHandshakeTimeoutMs: 'TLS handshake, мс',
                responseHeaderTimeoutMs: 'Ожидание заголовков, мс',
                maxRedirects: 'Максимум редиректов',
                maxDownloadBytes: 'Лимит загрузки, байт',
              }[key]
            }
            <input
              type="number"
              min={0}
              step={1}
              value={http[key]}
              onChange={(event) => update({ [key]: Math.max(0, Number(event.target.value) || 0) })}
            />
          </label>
        ))}
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={http.followRedirects}
            onChange={(event) => update({ followRedirects: event.target.checked })}
          />
          Следовать редиректам
        </label>
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={http.decompress}
            onChange={(event) => update({ decompress: event.target.checked })}
          />
          Распаковывать gzip / deflate
        </label>
        <p className="hint">
          HTTP/3 использует QUIC/UDP. Ограничения выбранного транспорта показываются в результате
          запроса.
        </p>
      </div>
    );
  return (
    <div className="body-panel" role="tabpanel" aria-label="Редактор тела">
      <div className="body-toolbar">
        <select
          aria-label="Тип тела запроса"
          value={http.bodyKind}
          onChange={(event) => update({ bodyKind: event.target.value })}
        >
          <option value="none">Без тела</option>
          <option value="raw">Raw text</option>
          <option value="json">JSON</option>
          <option value="json5">JSON5</option>
          <option value="form">Form URL encoded</option>
          <option value="multipart">Multipart</option>
          <option value="file">Бинарный файл</option>
        </select>
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={http.persistBody}
            onChange={(event) => update({ persistBody: event.target.checked })}
          />
          Сохранять тело на диске
        </label>
        <span className="hint">
          {http.persistBody ? 'Текст тела сохраняется в SQLite' : 'Тело только в памяти вкладки'}
        </span>
      </div>
      {http.bodyKind === 'none' ? (
        <div className="inline-empty">Запрос отправляется без тела.</div>
      ) : http.bodyKind === 'form' ? (
        <div className="headers-panel">
          <ParameterRows
            rows={http.form}
            label="поле формы"
            addLabel="+ Поле формы"
            onChange={(form) => update({ form })}
          />
        </div>
      ) : http.bodyKind === 'multipart' ? (
        <div className="headers-panel">
          {http.multipart.map((part, index) => (
            <div className="multipart-part" key={index}>
              <ParameterRows
                rows={[part]}
                label={`часть ${index + 1}`}
                addLabel="+ Часть"
                onChange={(rows) => {
                  if (rows.length > 1) return;
                  update({
                    multipart: rows.length
                      ? http.multipart.map((item, i) =>
                          i === index ? { ...item, ...rows[0] } : item,
                        )
                      : http.multipart.filter((_, i) => i !== index),
                  });
                }}
              />
              <FilePicker
                kind="body"
                label={`Файл части ${index + 1}`}
                fileId={part.fileId}
                onSelect={(fileId, fileName) =>
                  update({
                    multipart: http.multipart.map((item, i) =>
                      i === index ? { ...item, fileId, fileName } : item,
                    ),
                  })
                }
              />
              <label className="multipart-type">
                Content-Type
                <input
                  value={part.contentType}
                  placeholder="Автоматически"
                  onChange={(event) =>
                    update({
                      multipart: http.multipart.map((item, i) =>
                        i === index ? { ...item, contentType: event.target.value } : item,
                      ),
                    })
                  }
                />
              </label>
            </div>
          ))}
          <button
            className="button small"
            onClick={() =>
              update({
                multipart: [
                  ...http.multipart,
                  { name: '', value: '', enabled: true, fileId: '', fileName: '', contentType: '' },
                ],
              })
            }
          >
            + Часть multipart
          </button>
        </div>
      ) : http.bodyKind === 'file' || http.bodyKind === 'binary' ? (
        <div className="headers-panel">
          <FilePicker
            kind="body"
            label="Выбрать файл тела"
            fileId={http.fileId}
            onSelect={(fileId) => update({ fileId })}
          />
        </div>
      ) : (
        <BodyEditor value={http.body} onChange={(body) => update({ body })} />
      )}
    </div>
  );
}
