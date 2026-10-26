import type { GRPCDefinition } from '../lib/types';
import { normalizeGRPC } from '../lib/types';
import { FilePicker, ParameterRows } from './HTTPFields';

export type GRPCSection = 'message' | 'metadata' | 'tls' | 'options';

export function GRPCFields({
  grpc: definition,
  section,
  update,
}: {
  grpc: GRPCDefinition;
  section: Exclude<GRPCSection, 'message'>;
  update: (patch: Partial<GRPCDefinition>) => void;
}) {
  const grpc = normalizeGRPC(definition);
  const tls = grpc.tls!;
  if (section === 'metadata')
    return (
      <div className="http-fields grpc-fields">
        <ParameterRows
          rows={grpc.metadata!}
          label="gRPC metadata"
          addLabel="+ Metadata"
          onChange={(metadata) => update({ metadata })}
          suggestions={['authorization', 'x-api-key', 'x-request-id']}
        />
        <p className="hint">
          Повторяющиеся имена передаются отдельными значениями. Для имён с суффиксом -bin введите
          Base64. Пароли и токены сохраняются только как ссылки {'{{secret}}'}; прямые значения
          остаются в памяти вкладки.
        </p>
      </div>
    );
  if (section === 'tls')
    return (
      <div className="http-fields grpc-fields">
        <label className="form-field">
          Защита соединения gRPC
          <select
            aria-label="Защита соединения gRPC"
            value={grpc.plaintext ? 'plaintext' : 'tls'}
            onChange={(event) => update({ plaintext: event.target.value === 'plaintext' })}
          >
            <option value="tls">TLS / mTLS</option>
            <option value="plaintext">Plaintext (h2c)</option>
          </select>
        </label>
        {grpc.plaintext ? (
          <p className="warning-text">Соединение передаёт тело и metadata без шифрования.</p>
        ) : (
          <>
            <FilePicker
              kind="ca"
              fileId={tls.caFileId}
              label="Выбрать CA для gRPC…"
              onSelect={(caFileId) => update({ tls: { ...tls, caFileId } })}
            />
            <FilePicker
              kind="certificate"
              fileId={tls.clientCertFileId}
              label="Выбрать сертификат gRPC…"
              onSelect={(clientCertFileId) => update({ tls: { ...tls, clientCertFileId } })}
            />
            <FilePicker
              kind="private-key"
              fileId={tls.clientKeyFileId}
              label="Выбрать приватный ключ gRPC…"
              onSelect={(clientKeyFileId) => update({ tls: { ...tls, clientKeyFileId } })}
            />
            <label className="form-field">
              TLS Server name / SNI
              <input
                aria-label="gRPC TLS Server name"
                value={tls.serverName}
                placeholder="Имя из сертификата сервера"
                onChange={(event) => update({ tls: { ...tls, serverName: event.target.value } })}
              />
            </label>
            <label className="checkbox-label">
              <input
                type="checkbox"
                checked={tls.insecure}
                onChange={(event) => update({ tls: { ...tls, insecure: event.target.checked } })}
              />
              Отключить проверку сертификата gRPC (insecure TLS)
            </label>
            {tls.insecure && <p className="warning-text">Подлинность сервера не проверяется.</p>}
          </>
        )}
      </div>
    );
  return (
    <div className="http-fields grpc-fields options-grid">
      <label className="form-field">
        gRPC authority
        <input
          aria-label="gRPC authority"
          value={grpc.authority}
          placeholder="По умолчанию — endpoint"
          onChange={(event) => update({ authority: event.target.value })}
        />
      </label>
      <label className="form-field">
        Deadline, мс
        <input
          aria-label="gRPC deadline, мс"
          type="number"
          min="1"
          max="86400000"
          value={definition.timeoutMs ?? grpc.timeoutMs}
          onChange={(event) => update({ timeoutMs: Number(event.target.value) })}
        />
      </label>
      <label className="form-field">
        Таймаут соединения, мс
        <input
          aria-label="gRPC таймаут соединения, мс"
          type="number"
          min="1"
          max="86400000"
          value={definition.connectTimeoutMs ?? grpc.connectTimeoutMs}
          onChange={(event) => update({ connectTimeoutMs: Number(event.target.value) })}
        />
      </label>
      {(
        [
          ['maxSendBytes', 'Лимит исходящего сообщения, байт'],
          ['maxReceiveBytes', 'Лимит входящего сообщения, байт'],
        ] as const
      ).map(([key, label]) => (
        <label className="form-field" key={key}>
          {label}
          <input
            aria-label={`gRPC ${label.toLocaleLowerCase()}`}
            type="number"
            min="1"
            max={4 * 1024 * 1024}
            value={definition[key] ?? grpc[key]}
            onChange={(event) => update({ [key]: Number(event.target.value) })}
          />
        </label>
      ))}
      <label className="form-field">
        Сжатие gRPC
        <select
          aria-label="Сжатие gRPC"
          value={grpc.compression}
          onChange={(event) => update({ compression: event.target.value })}
        >
          <option value="none">Без сжатия</option>
          <option value="gzip">gzip</option>
        </select>
      </label>
      <p className="hint">
        Лимиты сообщений — до 4 МиБ. Эти настройки также используются для server reflection.
      </p>
    </div>
  );
}
