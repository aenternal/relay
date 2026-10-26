import { useRef, useState } from 'react';
import { useRelay } from '../lib/context';
import type {
  NativeProtoSelection,
  ProtoDiagnostic,
  ProtoImportInput,
  ProtoImportResult,
  ProtoSchema,
  SelectedFile,
} from '../lib/types';
import { Modal } from './Modal';
import { ProtoDiagnostics } from './ProtoDiagnostics';

const MAX_IMPORT_BYTES = 16 * 1024 * 1024;
const MAX_SOURCE_BYTES = 2 * 1024 * 1024;
const MAX_IMPORT_FILES = 256;

function toBase64(bytes: Uint8Array) {
  let value = '';
  for (let offset = 0; offset < bytes.length; offset += 8192)
    value += String.fromCharCode(...bytes.subarray(offset, offset + 8192));
  return btoa(value);
}

export function ProtoImportDialog({
  schema,
  onClose,
  onImported,
}: {
  schema?: ProtoSchema;
  onClose: () => void;
  onImported: (result: ProtoImportResult) => void;
}) {
  const {
    workspaceId,
    nativeFilesAvailable,
    selectProtoImport,
    selectProtoIncludeDirectory,
    importProtoSchema,
  } = useRelay();
  const [name, setName] = useState(schema?.name ?? '');
  const [native, setNative] = useState<NativeProtoSelection | null>(null);
  const [input, setInput] = useState<ProtoImportInput | null>(null);
  const [includes, setIncludes] = useState<SelectedFile[]>([]);
  const [diagnostics, setDiagnostics] = useState<ProtoDiagnostic[]>([]);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const filesRef = useRef<HTMLInputElement>(null);
  const folderRef = useRef<HTMLInputElement>(null);
  const descriptorRef = useRef<HTMLInputElement>(null);
  async function choose(kind: string) {
    if (!nativeFilesAvailable) {
      ({ files: filesRef, folder: folderRef, descriptor: descriptorRef })[kind]?.current?.click();
      return;
    }
    setBusy(true);
    setError('');
    try {
      const selection = await selectProtoImport(kind);
      if (!selection.id) return;
      setNative(selection);
      setInput(null);
      if (selection.kind === 'descriptor') setIncludes([]);
      setDiagnostics([]);
      setWarnings([]);
      if (!name) setName(selection.name.replace(/\.(proto|protoset|desc|pb)$/i, ''));
    } catch {
      setError('Не удалось выбрать файлы схемы.');
    } finally {
      setBusy(false);
    }
  }
  async function readFiles(files: FileList | null, kind: string) {
    if (!files?.length) return;
    setBusy(true);
    setError('');
    setDiagnostics([]);
    setWarnings([]);
    setInput(null);
    setNative(null);
    try {
      const selected = Array.from(files).filter(
        (file) => kind === 'descriptor' || file.name.endsWith('.proto'),
      );
      if (!selected.length) throw new Error('Выберите хотя бы один файл .proto.');
      if (
        selected.length > MAX_IMPORT_FILES ||
        selected.reduce((total, file) => total + file.size, 0) > MAX_IMPORT_BYTES
      )
        throw new Error(
          'Импорт ограничен 256 файлами и 16 МиБ. Разделите большую схему на наборы.',
        );
      if (kind !== 'descriptor' && selected.some((file) => file.size > MAX_SOURCE_BYTES))
        throw new Error('Размер одного .proto ограничен 2 МиБ.');
      if (kind === 'descriptor' && selected.length !== 1)
        throw new Error('Выберите один FileDescriptorSet.');
      const sources =
        kind === 'descriptor'
          ? []
          : await Promise.all(
              selected.map(async (file) => ({
                name:
                  kind === 'folder' && file.webkitRelativePath
                    ? file.webkitRelativePath.split('/').slice(1).join('/')
                    : file.name,
                content: await file.text(),
              })),
            );
      const next: ProtoImportInput = {
        id: schema?.id ?? '',
        workspaceId,
        name: '',
        kind: kind === 'descriptor' ? 'descriptor' : 'proto',
        sources,
        rootFiles: sources.map((source) => source.name),
        descriptorSetBase64:
          kind === 'descriptor' ? toBase64(new Uint8Array(await selected[0].arrayBuffer())) : '',
      };
      setInput(next);
      setNative(null);
      if (!name)
        setName(
          (kind === 'folder'
            ? selected[0].webkitRelativePath.split('/')[0]
            : selected[0].name
          ).replace(/\.(proto|protoset|desc|pb)$/i, ''),
        );
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : 'Не удалось прочитать файлы.');
    } finally {
      setBusy(false);
    }
  }
  async function addInclude() {
    setBusy(true);
    setError('');
    try {
      const selected = await selectProtoIncludeDirectory();
      if (selected.id)
        setIncludes((items) => [...items.filter((item) => item.id !== selected.id), selected]);
    } catch {
      setError('Не удалось выбрать include-директорию.');
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    if (busy || !name.trim() || (!native && !input)) return;
    setBusy(true);
    setError('');
    setDiagnostics([]);
    setWarnings([]);
    const result = await importProtoSchema(
      native
        ? {
            selectionId: native.id,
            includeIds: includes.map((item) => item.id),
            id: schema?.id ?? '',
            workspaceId,
            name: name.trim(),
          }
        : { ...input!, id: schema?.id ?? '', workspaceId, name: name.trim() },
    );
    setBusy(false);
    if (!result) {
      setError('Не удалось сохранить схему. Проверьте файлы и повторите импорт.');
      return;
    }
    if (result.schemaId) {
      onImported(result);
      return;
    }
    setDiagnostics(result.diagnostics);
    setWarnings(result.warnings);
    if (!result.diagnostics.length) setError('Импорт не завершён. Проверьте выбранные файлы.');
  }
  const fileNames = native?.files ?? input?.sources.map((source) => source.name) ?? [];
  const kind = native?.kind ?? input?.kind;
  return (
    <Modal
      title={schema ? 'Переимпортировать схему' : 'Импорт Protobuf'}
      onClose={() => {
        if (!busy) onClose();
      }}
    >
      <div className="proto-import-form">
        <label className="form-field">
          Название схемы
          <input
            aria-label="Название схемы"
            maxLength={200}
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </label>
        <div className="proto-import-actions">
          <button className="button small" disabled={busy} onClick={() => void choose('files')}>
            Файлы .proto
          </button>
          <button className="button small" disabled={busy} onClick={() => void choose('folder')}>
            Папка .proto
          </button>
          <button
            className="button small"
            disabled={busy}
            onClick={() => void choose('descriptor')}
          >
            Descriptor set
          </button>
        </div>
        <input
          type="file"
          hidden
          ref={filesRef}
          multiple
          accept=".proto"
          aria-label="Файлы Protobuf"
          onChange={(event) => {
            void readFiles(event.target.files, 'files');
            event.target.value = '';
          }}
        />
        <input
          type="file"
          hidden
          ref={folderRef}
          multiple
          {...({ webkitdirectory: '' } as object)}
          aria-label="Папка Protobuf"
          onChange={(event) => {
            void readFiles(event.target.files, 'folder');
            event.target.value = '';
          }}
        />
        <input
          type="file"
          hidden
          ref={descriptorRef}
          accept=".protoset,.pb,.desc"
          aria-label="Descriptor set Protobuf"
          onChange={(event) => {
            void readFiles(event.target.files, 'descriptor');
            event.target.value = '';
          }}
        />
        {kind === 'descriptor' ? (
          <p className="hint">
            Выбран бинарный FileDescriptorSet. Включите в него зависимости при экспорте.
          </p>
        ) : (
          fileNames.length > 0 && (
            <>
              <ul className="proto-selected-files" aria-label="Выбранные файлы Protobuf">
                {fileNames.map((file, index) => (
                  <li key={`${file}-${index}`}>
                    {input && (
                      <input
                        type="checkbox"
                        aria-label={`Корневой файл ${file}`}
                        checked={input.rootFiles.includes(file)}
                        onChange={(event) =>
                          setInput({
                            ...input,
                            rootFiles: event.target.checked
                              ? [...input.rootFiles, file]
                              : input.rootFiles.filter((root) => root !== file),
                          })
                        }
                      />
                    )}
                    <code>{file}</code>
                  </li>
                ))}
              </ul>
              {input && (
                <p className="proto-root-note">
                  Отметьте корневые файлы. Остальные доступны для imports. Пути папки сохраняются
                  относительно выбранной директории.
                </p>
              )}
            </>
          )
        )}
        {nativeFilesAvailable && kind !== 'descriptor' && (
          <>
            <button className="button small" disabled={busy} onClick={() => void addInclude()}>
              + Include-директория
            </button>
            {includes.map((include) => (
              <div className="proto-include-row" key={include.id}>
                <code>{include.name}</code>
                <button
                  className="icon-button"
                  aria-label={`Убрать include ${include.name}`}
                  onClick={() =>
                    setIncludes((items) => items.filter((item) => item.id !== include.id))
                  }
                >
                  ×
                </button>
              </div>
            ))}
          </>
        )}
        <p className="hint">
          Импорт выполняется офлайн. Стандартные google/protobuf/*.proto доступны автоматически.
          Скомпилированный набор дескрипторов с комментариями сохраняется в проекте.
        </p>
        {schema && (
          <p className="warning-text">
            При замене будут проверены изменения сигнатур методов. Сохранённые запросы продолжат
            ссылаться на эту схему.
          </p>
        )}
        {error && (
          <p role="alert" className="warning-text">
            {error}
          </p>
        )}
        <ProtoDiagnostics diagnostics={diagnostics} warnings={warnings} />
        <div className="modal-actions">
          <button className="button" disabled={busy} onClick={onClose}>
            Отмена
          </button>
          <button
            className="button primary"
            disabled={
              busy ||
              !name.trim() ||
              (!native && !input) ||
              (input?.kind === 'proto' && !input.rootFiles.length)
            }
            onClick={() => void save()}
          >
            {busy ? 'Обработка…' : 'Импортировать'}
          </button>
        </div>
      </div>
    </Modal>
  );
}
