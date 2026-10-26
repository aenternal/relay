import { useState } from 'react';
import { useRelay } from '../lib/context';
import type { Environment, Variable } from '../lib/types';
import { Modal } from './Modal';

type EditVariable = Variable & { secretInput: string; removeSecret: boolean; key: string };

function EnvironmentForm({
  environment,
  onClose,
}: {
  environment: Environment;
  onClose: () => void;
}) {
  const { mutate, capabilities } = useRelay();
  const [name, setName] = useState(environment.name);
  const [environmentId, setEnvironmentId] = useState(environment.id);
  const [variables, setVariables] = useState<EditVariable[]>(
    (environment.variables ?? []).map((item) => ({
      ...item,
      value: item.secret ? '' : item.value,
      secretInput: '',
      removeSecret: false,
      key: item.id,
    })),
  );
  const [busy, setBusy] = useState(false);
  const [validation, setValidation] = useState('');
  function update(index: number, patch: Partial<EditVariable>) {
    setVariables((current) =>
      current.map((item, i) => (i === index ? { ...item, ...patch } : item)),
    );
  }
  async function save() {
    if (!name.trim() || busy) return;
    if (variables.some((item) => !/^[A-Za-z_][A-Za-z0-9_]*$/.test(item.name))) {
      setValidation('Имена переменных: латинские буквы, цифры и _, без цифры в начале.');
      return;
    }
    if (new Set(variables.map((item) => item.name)).size !== variables.length) {
      setValidation('Имена переменных должны быть уникальными в окружении.');
      return;
    }
    setBusy(true);
    setValidation('');
    const snapshot = variables.map((item) => ({ ...item }));
    const state = await mutate(async (api) => {
      return api.SaveEnvironment({
        ...environment,
        id: environmentId,
        name: name.trim(),
        variables: snapshot.map(
          ({ id, name: variableName, value, secret, secretRef, removeSecret }) => ({
            id,
            name: variableName,
            value: secret ? '' : value,
            secret,
            secretRef: removeSecret || !secret ? '' : secretRef,
          }),
        ),
      });
    });
    if (!state) {
      setBusy(false);
      return;
    }
    const saved = state.environments.find(
      (item) =>
        item.id === environmentId ||
        (!environmentId &&
          item.workspaceId === environment.workspaceId &&
          item.name === name.trim()),
    );
    let success = true;
    if (saved) {
      setEnvironmentId(saved.id);
      setVariables((current) =>
        current.map((item) => ({
          ...item,
          id: saved.variables.find((entry) => entry.name === item.name)?.id ?? item.id,
          secretRef: saved.variables.find((entry) => entry.name === item.name)?.secretRef ?? '',
          removeSecret: false,
        })),
      );
      for (const item of snapshot) {
        if (!item.secret || !item.secretInput) continue;
        const variable = saved.variables.find((entry) => entry.name === item.name);
        if (variable) {
          const result = await mutate(
            (api) => api.SetSecret(saved.id, variable.id, item.secretInput),
            'Не удалось сохранить секрет в системное хранилище. Проверьте доступность keychain.',
          );
          if (!result) success = false;
          else {
            const persisted = result.environments.find((entry) => entry.id === saved.id);
            setVariables((current) =>
              current.map((entry) => ({
                ...entry,
                secretRef:
                  persisted?.variables.find((variable) => variable.id === entry.id)?.secretRef ??
                  entry.secretRef,
              })),
            );
          }
        }
      }
    }
    // Remove plaintext from form state after transmission, including failures.
    setVariables((current) => current.map((item) => ({ ...item, secretInput: '' })));
    setBusy(false);
    if (success) onClose();
    else
      setValidation(
        'Окружение сохранено. Не удалось сохранить некоторые секреты — введите их повторно.',
      );
  }
  return (
    <div className="environment-form">
      <label className="form-field">
        Название окружения
        <input value={name} maxLength={200} onChange={(event) => setName(event.target.value)} />
      </label>
      <div className={`vault-notice ${capabilities.secretAvailable ? '' : 'warning'}`}>
        <span aria-hidden="true">◇</span>
        <div>
          <strong>
            {capabilities.secretAvailable
              ? `Секреты: ${capabilities.secretBackend}`
              : 'Системное хранилище секретов недоступно'}
          </strong>
          <p>
            {capabilities.message ||
              'Значения секретов хранятся в keychain ОС и не возвращаются в интерфейс. Публичные значения записываются в SQLite.'}
          </p>
        </div>
      </div>
      <div className="variable-table">
        <div className="variable-table-heading">
          <span>ПЕРЕМЕННАЯ</span>
          <span>ЗНАЧЕНИЕ</span>
          <span>ТИП</span>
          <span />
        </div>
        {variables.map((variable, index) => (
          <div className="variable-row" key={variable.key}>
            <input
              aria-label={`Имя переменной ${index + 1}`}
              value={variable.name}
              placeholder="baseUrl"
              spellCheck={false}
              autoComplete="off"
              onChange={(event) => update(index, { name: event.target.value })}
            />
            <div className="variable-value">
              <input
                aria-label={`Значение переменной ${index + 1}`}
                type={variable.secret ? 'password' : 'text'}
                autoComplete="off"
                value={variable.secret ? variable.secretInput : variable.value}
                disabled={variable.secret && !capabilities.secretAvailable}
                placeholder={
                  variable.secret
                    ? variable.secretRef && !variable.removeSecret
                      ? '•••••••• — сохранён, введите для замены'
                      : 'Введите секрет'
                    : 'https://api.example.com'
                }
                onChange={(event) =>
                  update(
                    index,
                    variable.secret
                      ? { secretInput: event.target.value, removeSecret: false }
                      : { value: event.target.value },
                  )
                }
              />
              {variable.secret && variable.secretRef && (
                <button
                  className="text-button danger-text"
                  onClick={() =>
                    update(index, { removeSecret: !variable.removeSecret, secretInput: '' })
                  }
                >
                  {variable.removeSecret
                    ? 'Отменить удаление секрета'
                    : 'Удалить сохранённый секрет'}
                </button>
              )}
            </div>
            <select
              aria-label={`Тип переменной ${index + 1}`}
              value={variable.secret ? 'secret' : 'public'}
              onChange={(event) =>
                update(index, {
                  secret: event.target.value === 'secret',
                  value: '',
                  secretInput: '',
                })
              }
            >
              <option value="public">Публичная</option>
              <option value="secret">Секрет</option>
            </select>
            <button
              className="icon-button danger-text"
              aria-label={`Удалить переменную ${index + 1}`}
              onClick={() => setVariables((current) => current.filter((_, i) => i !== index))}
            >
              ×
            </button>
          </div>
        ))}
      </div>
      {!variables.length && (
        <div className="inline-empty">Добавьте baseUrl, token или другие переменные запроса.</div>
      )}
      <button
        className="button small"
        onClick={() =>
          setVariables((current) => [
            ...current,
            {
              id: '',
              name: '',
              value: '',
              secret: false,
              secretRef: '',
              secretInput: '',
              removeSecret: false,
              key: crypto.randomUUID(),
            },
          ])
        }
      >
        + Переменная
      </button>
      {validation && (
        <p className="form-error" role="alert">
          {validation}
        </p>
      )}
      <div className="form-footer">
        <p className="hint">
          В запросе используйте <code>{'{{baseUrl}}'}</code> или <code>{'{{token}}'}</code>.<br />
          Подстановка переменных подключается вместе с выполнением запросов.
        </p>
        <button className="button" onClick={onClose}>
          Отмена
        </button>
        <button
          className="button primary"
          disabled={!name.trim() || busy}
          onClick={() => void save()}
        >
          {busy ? 'Сохранение…' : 'Сохранить окружение'}
        </button>
      </div>
    </div>
  );
}

export function Environments() {
  const { data, workspaceId, mutate } = useRelay();
  const [editing, setEditing] = useState<Environment | null>(null);
  const [deleting, setDeleting] = useState<Environment | null>(null);
  const [busy, setBusy] = useState(false);
  const environments = data.environments.filter((item) => item.workspaceId === workspaceId);
  const workspace = data.workspaces.find((item) => item.id === workspaceId);
  return (
    <section className="page environments-page">
      <div className="page-heading">
        <div>
          <div className="eyebrow">ПЕРЕМЕННЫЕ ПРОЕКТА</div>
          <h1>Окружения</h1>
          <p>Один запрос для локального стенда, staging и production.</p>
        </div>
        <button
          className="button primary"
          disabled={!workspaceId}
          onClick={() => setEditing({ id: '', workspaceId, name: '', variables: [] })}
        >
          + Окружение
        </button>
      </div>
      <div className="environment-cards">
        {environments.map((environment) => (
          <article className="environment-card" key={environment.id}>
            <div className="environment-card-heading">
              <span className="environment-symbol">◇</span>
              <div>
                <h2>{environment.name}</h2>
                <span className="hint">
                  {environment.variables?.length ?? 0} переменных ·{' '}
                  {(environment.variables ?? []).filter((item) => item.secret).length} секретов
                </span>
              </div>
              {workspace?.activeEnvironmentId === environment.id && (
                <span className="active-badge">Активно</span>
              )}
            </div>
            <div className="environment-preview">
              {(environment.variables ?? []).slice(0, 3).map((variable) => (
                <div key={variable.id}>
                  <code>{variable.name}</code>
                  <span>
                    {variable.secret
                      ? variable.secretRef
                        ? '••••••••'
                        : 'Не задан'
                      : variable.value}
                  </span>
                </div>
              ))}
            </div>
            <div className="card-actions">
              <button className="button small" onClick={() => setEditing(environment)}>
                Редактировать
              </button>
              {workspace && workspace.activeEnvironmentId !== environment.id && (
                <button
                  className="text-button"
                  onClick={() =>
                    void mutate((api) =>
                      api.SaveWorkspace({ ...workspace, activeEnvironmentId: environment.id }),
                    )
                  }
                >
                  Активировать
                </button>
              )}
              <button
                className="icon-button danger-text"
                aria-label={`Удалить окружение ${environment.name}`}
                onClick={() => setDeleting(environment)}
              >
                ×
              </button>
            </div>
          </article>
        ))}
      </div>
      {!environments.length && (
        <div className="large-empty">
          <span className="empty-icon">◇</span>
          <h2>У каждого API свой контекст</h2>
          <p>
            Создайте окружение с адресом сервера и переменными.
            <br />
            Токены сохраняются отдельно в системном keychain.
          </p>
          <button
            className="button"
            disabled={!workspaceId}
            onClick={() => setEditing({ id: '', workspaceId, name: '', variables: [] })}
          >
            Создать окружение
          </button>
        </div>
      )}
      {editing && (
        <Modal
          title={editing.id ? 'Редактировать окружение' : 'Новое окружение'}
          onClose={() => setEditing(null)}
        >
          <EnvironmentForm
            key={editing.id || 'new'}
            environment={editing}
            onClose={() => setEditing(null)}
          />
        </Modal>
      )}
      {deleting && (
        <Modal title="Удалить окружение?" onClose={() => setDeleting(null)}>
          <p>Окружение «{deleting.name}» и его переменные будут удалены.</p>
          <div className="modal-actions">
            <button className="button" onClick={() => setDeleting(null)}>
              Отмена
            </button>
            <button
              className="button danger"
              disabled={busy}
              onClick={async () => {
                setBusy(true);
                const result = await mutate((api) => api.DeleteEnvironment(deleting.id));
                setBusy(false);
                if (result) setDeleting(null);
              }}
            >
              Удалить
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}
