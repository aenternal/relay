import { useEffect, useRef, useState } from 'react';
import { useRelay } from '../lib/context';
import type {
  ProjectExport,
  ProjectFormat,
  ProjectPreview,
  ProjectStats,
  ProjectWarning,
} from '../lib/types';
import { protoErrorMessage } from '../lib/types';
import { downloadProject, PROJECT_MAX_BYTES, projectInputError } from '../lib/transfer';
import { Modal } from './Modal';

function Report({ stats, warnings }: { stats: ProjectStats; warnings: ProjectWarning[] }) {
  return (
    <div className="project-report">
      <dl className="project-stats">
        {(
          [
            ['Коллекции', stats.collections],
            ['Папки', stats.folders],
            ['Запросы', stats.requests],
            ['Окружения', stats.environments],
            ['Protobuf схемы', stats.schemas],
            ['Секретные переменные', stats.secretVariables],
          ] as const
        ).map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      {!!warnings.length && (
        <details className="project-warnings" open>
          <summary>Предупреждения ({warnings.length})</summary>
          <ul>
            {warnings.map((warning, index) => (
              <li key={index}>
                <strong>{warning.code}</strong>
                {warning.path && (
                  <>
                    {' '}
                    · <code>{warning.path}</code>
                  </>
                )}
                <p>{warning.message}</p>
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

export function ProjectTransferDialog({ onClose }: { onClose: () => void }) {
  const {
    workspaceId,
    data: state,
    nativeFilesAvailable,
    exportProject,
    previewProjectImport,
    importProject,
    selectProjectFile,
    saveProjectFile,
  } = useRelay();
  const [mode, setMode] = useState<'import' | 'export'>('import');
  const [format, setFormat] = useState<ProjectFormat>('native');
  const [data, setData] = useState('');
  const [filename, setFilename] = useState('');
  const [includeBodies, setIncludeBodies] = useState(false);
  const [preview, setPreview] = useState<ProjectPreview | null>(null);
  const [output, setOutput] = useState<ProjectExport | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [complete, setComplete] = useState(false);
  const [saved, setSaved] = useState(false);
  const generation = useRef(0);
  useEffect(
    () => () => {
      generation.current += 1;
    },
    [],
  );
  const invalidate = () => {
    generation.current += 1;
    setPreview(null);
    setOutput(null);
    setError('');
    setComplete(false);
    setSaved(false);
    setBusy(false);
  };
  const run = async (action: () => Promise<void>) => {
    const token = ++generation.current;
    setBusy(true);
    setError('');
    try {
      await action();
    } catch (error) {
      if (token === generation.current)
        setError(
          protoErrorMessage(
            error,
            'Не удалось обработать проект. Проверьте формат и ограничения файла.',
          ),
        );
    } finally {
      if (token === generation.current) setBusy(false);
    }
  };
  const workspace = state.workspaces.find((item) => item.id === workspaceId);
  return (
    <Modal title="Импорт / экспорт проекта" onClose={onClose}>
      <div className="project-transfer">
        <div className="panel-tabs" role="tablist" aria-label="Перенос проекта">
          {(['import', 'export'] as const).map((item) => (
            <button
              key={item}
              role="tab"
              aria-selected={mode === item}
              disabled={busy}
              onClick={() => {
                invalidate();
                setMode(item);
                setIncludeBodies(false);
              }}
            >
              {item === 'import' ? 'Импорт' : 'Экспорт'}
            </button>
          ))}
        </div>
        <p className="hint">
          Секретные значения, история ответов и выбранные файлы не переносятся. После импорта заново
          задайте секреты и TLS-файлы. Новый проект создаётся отдельным пространством.
        </p>
        {mode === 'import' ? (
          <>
            <label>
              Формат импорта
              <select
                aria-label="Формат импорта проекта"
                disabled={busy}
                value={format}
                onChange={(event) => {
                  invalidate();
                  setFormat(event.target.value as ProjectFormat);
                }}
              >
                <option value="native">Relay project</option>
                <option value="postman">Postman Collection v2.1</option>
                <option value="insomnia">Insomnia export</option>
              </select>
            </label>
            {nativeFilesAvailable ? (
              <button
                className="button"
                disabled={busy}
                onClick={() =>
                  void run(async () => {
                    const token = generation.current;
                    const file = await selectProjectFile();
                    if (token !== generation.current || (!file.name && !file.data)) return;
                    setPreview(null);
                    setComplete(false);
                    setFilename(file.name);
                    const problem = projectInputError(file.data);
                    if (problem) {
                      setData('');
                      setError(problem);
                    } else setData(file.data);
                  })
                }
              >
                Выбрать файл проекта
              </button>
            ) : (
              <label className="project-file">
                Файл JSON (до 32 МиБ)
                <input
                  aria-label="Файл проекта"
                  type="file"
                  accept=".json,application/json"
                  disabled={busy}
                  onChange={async (event) => {
                    const file = event.target.files?.[0];
                    event.target.value = '';
                    if (!file) return;
                    invalidate();
                    setFilename(file.name);
                    setData('');
                    if (file.size > PROJECT_MAX_BYTES) {
                      setError('Размер проекта превышает 32 МиБ.');
                      return;
                    }
                    const token = ++generation.current;
                    setBusy(true);
                    try {
                      const text = await file.text();
                      if (token === generation.current) {
                        const problem = projectInputError(text);
                        if (problem) setError(problem);
                        else setData(text);
                      }
                    } catch {
                      if (token === generation.current)
                        setError('Не удалось прочитать файл проекта.');
                    } finally {
                      if (token === generation.current) setBusy(false);
                    }
                  }}
                />
              </label>
            )}
            {filename && <span className="hint">{filename}</span>}
            <label>
              JSON проекта
              <textarea
                aria-label="JSON проекта"
                spellCheck={false}
                value={data}
                maxLength={PROJECT_MAX_BYTES}
                disabled={busy}
                onChange={(event) => {
                  invalidate();
                  setFilename('');
                  setData(event.target.value);
                }}
              />
            </label>
            <label className="checkbox-label">
              <input
                type="checkbox"
                checked={includeBodies}
                disabled={busy}
                onChange={(event) => setIncludeBodies(event.target.checked)}
              />
              Сохранить тела запросов из файла
            </label>
            <p className="hint">
              Без этой опции определения импортируются с пустыми телами. Скрипты и flows не
              выполняются; неподдерживаемые поля указаны в отчёте.
            </p>
            {preview && (
              <div aria-label="Предпросмотр импорта">
                <h3>{preview.workspaceName}</h3>
                <Report stats={preview.stats} warnings={preview.warnings} />
              </div>
            )}
            {complete && <p role="status">Проект импортирован в новое рабочее пространство.</p>}
            <div className="modal-actions">
              <button
                className="button"
                disabled={busy || !data || complete}
                onClick={() =>
                  void run(async () => {
                    const problem = projectInputError(data);
                    if (problem) {
                      setError(problem);
                      return;
                    }
                    const token = generation.current;
                    const result = await previewProjectImport(format, data);
                    if (token === generation.current) setPreview(result);
                  })
                }
              >
                Проверить импорт
              </button>
              <button
                className="button primary"
                disabled={busy || !preview || complete}
                onClick={() =>
                  void run(async () => {
                    const token = generation.current;
                    const result = await importProject(format, data, includeBodies);
                    if (result && token === generation.current) {
                      setComplete(true);
                      setPreview({
                        format,
                        workspaceName: preview!.workspaceName,
                        stats: result.stats,
                        warnings: result.warnings,
                      });
                    } else if (!result && token === generation.current)
                      setError('Проект не импортирован. Изменения не применены.');
                  })
                }
              >
                {busy ? 'Обработка…' : 'Импортировать в новое пространство'}
              </button>
            </div>
          </>
        ) : (
          <>
            <p>
              Рабочее пространство: <strong>{workspace?.name || 'не выбрано'}</strong>
            </p>
            <label className="checkbox-label">
              <input
                type="checkbox"
                checked={includeBodies}
                disabled={busy}
                onChange={(event) => {
                  invalidate();
                  setIncludeBodies(event.target.checked);
                }}
              />
              Включить тела запросов в экспорт
            </label>
            <p className="hint">
              Экспортируются сохранённые определения. Изменения открытых вкладок сначала сохраните.
              Тела могут содержать личные данные; по умолчанию они не включаются.
            </p>
            {output && (
              <div aria-label="Предпросмотр экспорта">
                <p>{output.filename}</p>
                <Report stats={output.stats} warnings={output.warnings} />
              </div>
            )}
            <div className="modal-actions">
              <button
                className="button"
                disabled={busy || !workspaceId}
                onClick={() =>
                  void run(async () => {
                    const token = generation.current;
                    const result = await exportProject(workspaceId, includeBodies);
                    if (token === generation.current) {
                      setOutput(result);
                      setSaved(false);
                    }
                  })
                }
              >
                {busy ? 'Подготовка…' : 'Подготовить экспорт'}
              </button>
              <button
                className="button primary"
                disabled={!output || busy}
                onClick={() => {
                  if (!output) return;
                  if (nativeFilesAvailable)
                    void run(async () => {
                      const token = generation.current;
                      const result = await saveProjectFile(workspaceId, includeBodies);
                      if (token === generation.current && result.saved) {
                        setSaved(true);
                        setOutput({ ...output, warnings: result.warnings, stats: result.stats });
                      }
                    });
                  else
                    try {
                      downloadProject(output.filename, output.data);
                    } catch {
                      setError('Не удалось сохранить JSON-файл.');
                    }
                }}
              >
                {nativeFilesAvailable ? 'Сохранить JSON…' : 'Скачать JSON'}
              </button>
            </div>
            {saved && <p role="status">Проект сохранён в JSON-файл.</p>}
          </>
        )}
        {error && (
          <p className="error-text" role="alert">
            {error}
          </p>
        )}
      </div>
    </Modal>
  );
}
