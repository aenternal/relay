import { useEffect, useMemo, useRef, useState } from 'react';
import { useRelay } from '../lib/context';
import type {
  GRPCDefinition,
  ProtoDiagnostic,
  ProtoMethodDetail,
  ProtoSchemaDetail,
  ProtoValidationResult,
} from '../lib/types';
import { BodyEditor } from './BodyEditor';
import { ProtoDiagnostics } from './ProtoDiagnostics';
import { ProtoMessageFields } from './ProtoSchemaDetails';
import { protoErrorMessage, rpcKind } from '../lib/types';

function hexPreview(value: string) {
  const binary = atob(value);
  const limit = Math.min(binary.length, 64 * 1024);
  const lines: string[] = [];
  for (let index = 0; index < limit; index += 16) {
    const part = binary.slice(index, index + 16);
    lines.push(
      `${index.toString(16).padStart(8, '0')}  ${Array.from(part, (byte) => byte.charCodeAt(0).toString(16).padStart(2, '0')).join(' ')}`,
    );
  }
  if (binary.length > limit) lines.push('… Показаны первые 64 КиБ.');
  return lines.join('\n') || '(пустое сообщение)';
}

export function ProtoMessageEditor({
  id,
  onMethodState,
}: {
  id: string;
  onMethodState?: (state: 'none' | 'unary' | 'server' | 'client' | 'bidi') => void;
}) {
  const { data, tabs, editRequest, inspectProtoSchema, inspectProtoMethod, validateProtoMessage } =
    useRelay();
  const request = tabs[id]?.draft;
  const grpc = request?.grpc;
  const [registry, setRegistry] = useState<ProtoSchemaDetail | null>(null);
  const [methodDetail, setMethodDetail] = useState<ProtoMethodDetail | null>(null);
  const [error, setError] = useState('');
  const [schemaError, setSchemaError] = useState('');
  const [methodError, setMethodError] = useState('');
  const [reload, setReload] = useState(0);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ key: string; value: ProtoValidationResult } | null>(null);
  const [preview, setPreview] = useState<'json' | 'hex'>('json');
  const [methodSearch, setMethodSearch] = useState('');
  const currentKey = JSON.stringify([
    request?.workspaceId,
    grpc?.schemaId,
    grpc?.method,
    grpc?.bodyFormat || 'json',
    grpc?.body,
    data.protoSchemas?.find((entry) => entry.id === grpc?.schemaId)?.checksum,
  ]);
  const latestKey = useRef(currentKey);
  latestKey.current = currentKey;
  const latestRequest = useRef(request);
  latestRequest.current = request;
  const schemas = (data.protoSchemas ?? []).filter(
    (schema) => schema.workspaceId === request?.workspaceId,
  );
  const schema = schemas.find((entry) => entry.id === grpc?.schemaId);
  const workspaceId = request?.workspaceId ?? '';
  const schemaId = schema?.id ?? '';
  const checksum = schema?.checksum ?? '';
  const method = grpc?.method ?? '';
  const methods = useMemo(
    () => registry?.services.flatMap((service) => service.methods) ?? [],
    [registry],
  );
  useEffect(() => {
    let active = true;
    setRegistry(null);
    setError('');
    setSchemaError('');
    if (!schemaId) return;
    void inspectProtoSchema(workspaceId, schemaId)
      .then((detail) => {
        if (active) setRegistry(detail);
      })
      .catch((cause) => {
        if (active)
          setSchemaError(
            protoErrorMessage(cause, 'Не удалось прочитать выбранную схему. Повторите чтение.'),
          );
      });
    return () => {
      active = false;
    };
  }, [workspaceId, schemaId, checksum, inspectProtoSchema, reload]);
  useEffect(() => {
    let active = true;
    setMethodDetail(null);
    onMethodState?.('none');
    setMethodError('');
    if (!schemaId || !method) return;
    void inspectProtoMethod(workspaceId, schemaId, method)
      .then((detail) => {
        if (active) {
          setMethodDetail(detail);
          onMethodState?.(
            detail.method.clientStreaming
              ? detail.method.serverStreaming
                ? 'bidi'
                : 'client'
              : detail.method.serverStreaming
                ? 'server'
                : 'unary',
          );
        }
      })
      .catch((cause) => {
        if (active)
          setMethodError(
            protoErrorMessage(cause, 'Не удалось прочитать выбранный метод. Повторите чтение.'),
          );
      });
    return () => {
      active = false;
    };
  }, [workspaceId, schemaId, checksum, method, inspectProtoMethod, reload, onMethodState]);
  if (!grpc || !request) return null;
  function update(patch: Partial<GRPCDefinition>) {
    const value = latestRequest.current;
    if (value?.grpc) editRequest(id, { ...value, grpc: { ...value.grpc, ...patch } });
  }
  async function validate(format = false) {
    if (!grpc || !request || busy || !schema || !methodDetail) return;
    const key = currentKey;
    setBusy(true);
    setError('');
    try {
      const value = await validateProtoMessage({
        workspaceId,
        schemaId,
        method: grpc.method,
        body: grpc.body,
        format: grpc.bodyFormat || 'json',
      });
      if (latestKey.current !== key) return;
      if (format && value.valid) {
        update({ body: value.canonicalJSON, bodyFormat: 'json' });
        setResult({
          key: JSON.stringify([
            workspaceId,
            schemaId,
            grpc.method,
            'json',
            value.canonicalJSON,
            checksum,
          ]),
          value,
        });
      } else setResult({ key, value });
    } catch (cause) {
      if (latestKey.current === key)
        setError(
          protoErrorMessage(cause, 'Не удалось проверить сообщение Protobuf. Повторите проверку.'),
        );
    } finally {
      setBusy(false);
    }
  }
  const matchingMethods = methods.filter((entry) =>
    entry.fullMethod.toLocaleLowerCase().includes(methodSearch.toLocaleLowerCase()),
  );
  const visibleMethods = matchingMethods.slice(0, 500);
  const stale = !!result && result.key !== currentKey;
  const diagnostics: ProtoDiagnostic[] = result && !stale ? result.value.diagnostics : [];
  return (
    <>
      <div className="proto-method-selectors">
        <label className="form-field">
          Схема Protobuf
          <select
            aria-label="Схема Protobuf"
            value={grpc.schemaId ?? ''}
            onChange={(event) => {
              setError('');
              update({ schemaId: event.target.value, method: '' });
            }}
          >
            <option value="">Выберите схему…</option>
            {grpc.schemaId && !schema && <option value={grpc.schemaId}>Схема недоступна</option>}
            {schemas.map((entry) => (
              <option value={entry.id} key={entry.id}>
                {entry.name}
              </option>
            ))}
          </select>
        </label>
        <label className="form-field">
          Метод RPC
          <select
            aria-label="Метод RPC"
            disabled={!registry}
            value={grpc.method}
            onChange={(event) => {
              setError('');
              update({ method: event.target.value });
            }}
          >
            <option value="">Выберите метод…</option>
            {grpc.method && !visibleMethods.some((entry) => entry.fullMethod === grpc.method) && (
              <option value={grpc.method}>
                {grpc.method}
                {methods.some((entry) => entry.fullMethod === grpc.method) ? '' : ' · недоступен'}
              </option>
            )}
            {visibleMethods.map((entry) => (
              <option value={entry.fullMethod} key={entry.fullMethod}>
                {entry.fullMethod} · {rpcKind(entry)}
              </option>
            ))}
          </select>
        </label>
      </div>
      {methods.length > 500 && (
        <label className="form-field">
          Поиск методов RPC
          <input
            aria-label="Поиск методов RPC"
            value={methodSearch}
            onChange={(event) => setMethodSearch(event.target.value)}
            placeholder="Уточните полное имя метода…"
          />
          <span className="hint">Показано до 500 совпадений из {matchingMethods.length}.</span>
        </label>
      )}
      {!schemas.length && (
        <p className="hint">
          Добавьте .proto или descriptor set в разделе Protobuf, затем выберите схему и метод.
        </p>
      )}
      {methodDetail && (
        <div className="proto-method-summary">
          <span className="active-badge">{rpcKind(methodDetail.method)}</span> ·{' '}
          <code>{methodDetail.method.input}</code> → <code>{methodDetail.method.output}</code>
          {methodDetail.method.comment && (
            <p className="proto-comment">{methodDetail.method.comment}</p>
          )}
        </div>
      )}
      {methodDetail &&
        (methodDetail.method.clientStreaming || methodDetail.method.serverStreaming) && (
          <p className="hint">
            {methodDetail.method.clientStreaming
              ? 'Откройте поток, затем отправляйте сообщения из редактора. Half-close завершает отправку; ответы продолжают поступать.'
              : 'При открытии потока сообщение из редактора отправляется один раз. Ответы поступают в историю до завершения сервера.'}
          </p>
        )}
      <div className="editor-panel proto-message-editor">
        <div className="body-toolbar">
          <select
            aria-label="Формат сообщения Protobuf"
            value={grpc.bodyFormat || 'json'}
            onChange={(event) => update({ bodyFormat: event.target.value })}
          >
            <option value="json">Strict ProtoJSON</option>
            <option value="json5">JSON5-like</option>
          </select>
          <button
            className="button small"
            disabled={busy || !methodDetail}
            onClick={() => void validate()}
          >
            Validate
          </button>
          <button
            className="button small"
            disabled={busy || !methodDetail}
            title="Проверить и заменить текст каноническим Strict ProtoJSON"
            onClick={() => void validate(true)}
          >
            Format
          </button>
          <button
            className="button small"
            disabled={busy || !methodDetail}
            onClick={() => {
              if (methodDetail) update({ body: methodDetail.template, bodyFormat: 'json' });
            }}
          >
            Generate Example
          </button>
          <label className="checkbox-label">
            <input
              type="checkbox"
              checked={grpc.persistBody}
              onChange={(event) => update({ persistBody: event.target.checked })}
            />
            Сохранять тело на диске
          </label>
        </div>
        <BodyEditor value={grpc.body} onChange={(body) => update({ body })} />
      </div>
      <p className="hint">
        Проверка использует текст редактора без подстановки переменных окружения. Для int64/uint64
        используйте строки, например «9223372036854775807».
      </p>
      {error && (
        <p className="warning-text" role="alert">
          {error}
        </p>
      )}
      {(schemaError || methodError) && (
        <div className="proto-read-error">
          {schemaError && (
            <p className="warning-text" role="alert">
              {schemaError}
            </p>
          )}
          {methodError && (
            <p className="warning-text" role="alert">
              {methodError}
            </p>
          )}
          <button className="button small" onClick={() => setReload((value) => value + 1)}>
            Повторить чтение
          </button>
        </div>
      )}
      <ProtoDiagnostics
        diagnostics={diagnostics}
        warnings={result && !stale ? result.value.warnings : []}
      />
      {methodDetail && (
        <details className="proto-files">
          <summary>Поля input и output</summary>
          <ProtoMessageFields message={methodDetail.input} />
          <ProtoMessageFields message={methodDetail.output} />
        </details>
      )}
      <section className="proto-message-preview" aria-label="Предпросмотр Protobuf">
        <div className="proto-preview-heading">
          <span>{busy ? 'Проверка…' : 'Предпросмотр сообщения'}</span>
          {result && !stale && result.value.valid && (
            <strong role="status">Валидный Protobuf · {result.value.wireBytes} байт</strong>
          )}
          <small>Проверка без отправки</small>
        </div>
        {stale && (
          <p className="proto-preview-stale">
            Текст или метод изменены. Нажмите Validate, чтобы обновить превью.
          </p>
        )}
        {result?.value.valid && !stale ? (
          <>
            <div
              className="panel-tabs response-tabs"
              role="tablist"
              aria-label="Форматы предпросмотра Protobuf"
            >
              <button
                role="tab"
                aria-selected={preview === 'json'}
                onClick={() => setPreview('json')}
              >
                Canonical ProtoJSON
              </button>
              <button
                role="tab"
                aria-selected={preview === 'hex'}
                onClick={() => setPreview('hex')}
              >
                Protobuf hex
              </button>
            </div>
            <pre
              className="proto-preview-body"
              aria-label={preview === 'json' ? 'Canonical ProtoJSON' : 'Protobuf hex'}
            >
              {preview === 'json'
                ? result.value.canonicalJSON
                : hexPreview(result.value.wireBase64)}
            </pre>
          </>
        ) : (
          <p className="hint">
            Выберите метод и проверьте JSON / JSON5-like тело. Сериализация и канонизация
            выполняются без потери точности 64-битных чисел.
          </p>
        )}
      </section>
    </>
  );
}
