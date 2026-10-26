import { useEffect, useRef, useState } from 'react';
import { useRelay } from '../lib/context';
import { newRequest, protoErrorMessage } from '../lib/types';
import type { ProtoImportResult, ProtoMethod, ProtoSchema, ProtoSchemaDetail } from '../lib/types';
import { Modal } from './Modal';
import { ProtoImportDialog } from './ProtoImportDialog';
import { ProtoDiagnostics } from './ProtoDiagnostics';
import { ProtoSchemaDetails } from './ProtoSchemaDetails';

export function SchemaManager({ onRequestCreated }: { onRequestCreated: () => void }) {
  const { data, workspaceId, inspectProtoSchema, mutate, openRequest } = useRelay();
  const [selectedId, setSelectedId] = useState('');
  const [importing, setImporting] = useState<ProtoSchema | 'new' | null>(null);
  const [deleting, setDeleting] = useState<ProtoSchema | null>(null);
  const [detail, setDetail] = useState<ProtoSchemaDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const schemas = (data.protoSchemas ?? []).filter((schema) => schema.workspaceId === workspaceId);
  const selected = schemas.find((schema) => schema.id === selectedId) ?? schemas[0];
  const schemaId = selected?.id ?? '';
  const checksum = selected?.checksum ?? '';
  const collection = data.collections.find((item) => item.workspaceId === workspaceId);
  useEffect(() => {
    let active = true;
    setDetail(null);
    setError('');
    setLoading(false);
    if (!schemaId) return;
    setLoading(true);
    void inspectProtoSchema(workspaceId, schemaId)
      .then((value) => {
        if (active) setDetail(value);
      })
      .catch((cause) => {
        if (active)
          setError(protoErrorMessage(cause, 'Не удалось прочитать схему. Повторите чтение.'));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [workspaceId, schemaId, checksum, inspectProtoSchema, reload]);
  function imported(result: ProtoImportResult) {
    setSelectedId(result.schemaId);
    setWarnings(result.warnings);
    setImporting(null);
  }
  async function remove() {
    if (!deleting || busy) return;
    setBusy(true);
    const state = await mutate(
      (api) => api.DeleteProtoSchema(workspaceId, deleting.id),
      'Не удалось удалить схему Protobuf.',
    );
    setBusy(false);
    if (state) {
      setDeleting(null);
      setWarnings([]);
    }
  }
  async function createRequest(method: ProtoMethod) {
    if (!selected || !collection || busy) return;
    setBusy(true);
    const previousIds = new Set<string>();
    const request = newRequest(workspaceId, collection.id, '', 'grpc');
    request.name = method.name;
    request.grpc = { ...request.grpc!, schemaId: selected.id, method: method.fullMethod };
    const state = await mutate(async (api) => {
      const before = await api.LoadState();
      before.requests.forEach((entry) => previousIds.add(entry.id));
      return api.SaveRequest(request);
    });
    if (!mounted.current) return;
    setBusy(false);
    const created = state?.requests.find((entry) => !previousIds.has(entry.id));
    if (created) {
      openRequest(created.id);
      onRequestCreated();
    }
  }
  return (
    <section className="page schema-page">
      <div className="page-heading">
        <div>
          <div className="eyebrow">SCHEMA REGISTRY</div>
          <h1>Protobuf</h1>
          <p>Импортируйте схемы, изучайте методы и проверяйте сообщения.</p>
        </div>
        <div className="schema-heading-actions">
          <button
            className="button primary"
            disabled={!workspaceId}
            onClick={() => setImporting('new')}
          >
            + Импорт схемы
          </button>
        </div>
      </div>
      <ProtoDiagnostics diagnostics={[]} warnings={warnings} />
      {!schemas.length ? (
        <div className="large-empty">
          <h2>Схемы этого пространства</h2>
          <p className="hint">
            Добавьте .proto с imports или бинарный FileDescriptorSet.
            <br />
            Генерация кода и внешний protoc не требуются.
          </p>
          <button className="button" disabled={!workspaceId} onClick={() => setImporting('new')}>
            Импортировать Protobuf
          </button>
        </div>
      ) : (
        <div className="schema-layout">
          <nav className="schema-list" aria-label="Импортированные схемы">
            {schemas.map((schema) => (
              <button
                className={`schema-list-item ${schema.id === schemaId ? 'selected' : ''}`}
                key={schema.id}
                onClick={() => {
                  setSelectedId(schema.id);
                  setWarnings([]);
                }}
              >
                <strong>{schema.name}</strong>
                <small>
                  {schema.kind === 'descriptor' ? 'Descriptor set' : '.proto'} ·{' '}
                  {schema.files.length} файлов
                </small>
              </button>
            ))}
          </nav>
          <div className="schema-detail">
            {selected && (
              <div className="schema-detail-heading">
                <div>
                  <h2>{selected.name}</h2>
                  <code className="schema-checksum" title="SHA-256">
                    SHA-256 · {selected.checksum}
                  </code>
                  <div className="schema-meta">
                    Импортировано:{' '}
                    {selected.importedAt ? new Date(selected.importedAt).toLocaleString() : '—'}
                  </div>
                </div>
                <button className="button small" onClick={() => setImporting(selected)}>
                  Переимпорт
                </button>
                <button className="button small danger-text" onClick={() => setDeleting(selected)}>
                  Удалить
                </button>
              </div>
            )}
            {loading && (
              <p className="hint" role="status">
                Чтение дескрипторов…
              </p>
            )}
            {error && (
              <div className="proto-read-error">
                <p role="alert" className="warning-text">
                  {error}
                </p>
                <button
                  className="button small"
                  disabled={loading}
                  onClick={() => setReload((value) => value + 1)}
                >
                  Повторить чтение
                </button>
              </div>
            )}
            {detail && (
              <>
                <ProtoSchemaDetails
                  key={`${schemaId}-${checksum}`}
                  detail={detail}
                  selectMethod={
                    collection && !busy ? (method) => void createRequest(method) : undefined
                  }
                />
                {!collection && (
                  <p className="hint">
                    Создайте коллекцию, чтобы добавлять gRPC запросы из дерева методов.
                  </p>
                )}
              </>
            )}
          </div>
        </div>
      )}
      {importing && (
        <ProtoImportDialog
          schema={importing === 'new' ? undefined : importing}
          onClose={() => setImporting(null)}
          onImported={imported}
        />
      )}
      {deleting && (
        <Modal
          title="Удалить схему Protobuf?"
          onClose={() => {
            if (!busy) setDeleting(null);
          }}
        >
          <p>Схема «{deleting.name}» будет удалена из пространства.</p>
          <p className="warning-text">
            Выбор схемы в запросах будет сброшен. Имена методов, текст запросов и изменения в
            открытых вкладках сохранятся.
          </p>
          <div className="modal-actions">
            <button className="button" disabled={busy} onClick={() => setDeleting(null)}>
              Отмена
            </button>
            <button className="button danger" disabled={busy} onClick={() => void remove()}>
              {busy ? 'Удаление…' : 'Удалить схему'}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}
