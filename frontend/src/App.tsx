import { useEffect, useState } from 'react';
import { useStore } from 'zustand';
import { StoreContext, useRelay } from './lib/context';
import type { RelayStore } from './lib/store';
import {
  hasVolatileBody,
  hasVolatileCredentials,
  hasVolatileFiles,
  hasVolatileData,
  isDirty,
} from './lib/store';
import { Sidebar } from './components/Sidebar';
import type { Entity } from './components/Sidebar';
import { EntityDialog } from './components/EntityDialog';
import { Modal } from './components/Modal';
import { RequestEditor } from './components/RequestEditor';
import { Environments } from './components/Environments';
import { Settings } from './components/Settings';
import { SchemaManager } from './components/SchemaManager';

function Shell() {
  const {
    data,
    buildInfo,
    loaded,
    error,
    clearError,
    tabs,
    openIds,
    activeId,
    workspaceId,
    openRequest,
    requestClose,
    closingId,
    resolveClose,
    mutate,
    saveRequest,
    sendHTTPRequest,
    cancelHTTPRequest,
    sendGRPCRequest,
    cancelGRPCExecution,
    webSocketPrimary,
    disconnectWebSocket,
  } = useRelay();
  const [view, setView] = useState<'collections' | 'environments' | 'schemas' | 'settings'>(
    'collections',
  );
  const [editing, setEditing] = useState<Entity | null>(null);
  const [deleting, setDeleting] = useState<Entity | null>(null);
  const [busy, setBusy] = useState(false);
  const workspace = data.workspaces.find((item) => item.id === workspaceId);
  const collections = data.collections.filter((item) => item.workspaceId === workspaceId);
  const visibleIds = openIds.filter((id) => tabs[id]?.draft.workspaceId === workspaceId);
  useEffect(() => {
    function keydown(event: KeyboardEvent) {
      if (document.querySelector('[role=dialog]')) return;
      if ((event.ctrlKey || event.metaKey) && !document.querySelector('[role=dialog]')) {
        if (event.key.toLowerCase() === 'k') {
          event.preventDefault();
          const search = document.querySelector<HTMLInputElement>(
            'input[aria-label="Поиск запросов"]',
          );
          search?.focus();
          search?.select();
          return;
        }
        if (event.key === 'Tab' && view === 'collections' && visibleIds.length > 0) {
          event.preventDefault();
          const current = visibleIds.indexOf(activeId);
          const step = event.shiftKey ? -1 : 1;
          const next = (current + step + visibleIds.length) % visibleIds.length;
          openRequest(visibleIds[next]);
          return;
        }
        if (event.key === ',') {
          event.preventDefault();
          setView('settings');
          return;
        }
      }
      if (view === 'collections' && activeId && !document.querySelector('[role=dialog]')) {
        if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') {
          event.preventDefault();
          void (tabs[activeId]?.draft.websocket
            ? webSocketPrimary(activeId)
            : tabs[activeId]?.draft.grpc
              ? sendGRPCRequest(activeId)
              : sendHTTPRequest(activeId));
          return;
        }
        if (event.key === 'Escape') {
          void (tabs[activeId]?.draft.websocket
            ? disconnectWebSocket(activeId)
            : tabs[activeId]?.draft.grpc
              ? cancelGRPCExecution(activeId)
              : cancelHTTPRequest(activeId));
          return;
        }
      }
      if (!(event.ctrlKey || event.metaKey)) return;
      if (event.key.toLowerCase() === 's') {
        event.preventDefault();
        if (activeId) void saveRequest(activeId);
      }
      if (event.key.toLowerCase() === 'w' && activeId && view === 'collections') {
        event.preventDefault();
        requestClose(activeId);
      }
    }
    window.addEventListener('keydown', keydown);
    return () => window.removeEventListener('keydown', keydown);
  }, [
    activeId,
    saveRequest,
    requestClose,
    sendHTTPRequest,
    cancelHTTPRequest,
    sendGRPCRequest,
    cancelGRPCExecution,
    webSocketPrimary,
    disconnectWebSocket,
    tabs,
    view,
    openRequest,
    visibleIds,
  ]);
  useEffect(() => {
    function beforeUnload(event: BeforeUnloadEvent) {
      if (Object.values(tabs).some((tab) => isDirty(tab) || hasVolatileData(tab))) {
        event.preventDefault();
        event.returnValue = '';
      }
    }
    window.addEventListener('beforeunload', beforeUnload);
    return () => window.removeEventListener('beforeunload', beforeUnload);
  }, [tabs]);
  const deletingDirty =
    deleting?.item &&
    Object.values(tabs).some((tab) => {
      const request = tab.draft;
      if (!(isDirty(tab) || hasVolatileData(tab))) return false;
      switch (deleting.kind) {
        case 'workspace':
          return request.workspaceId === deleting.item?.id;
        case 'collection':
          return request.collectionId === deleting.item?.id;
        case 'request':
          return request.id === deleting.item?.id;
        case 'folder':
          return (
            request.folderId === deleting.item?.id ||
            request.collectionId === (deleting.item as { collectionId: string }).collectionId
          );
      }
    });
  async function remove() {
    if (!deleting?.item || busy) return;
    setBusy(true);
    const result = await mutate((api) => {
      const id = deleting.item!.id;
      switch (deleting.kind) {
        case 'workspace':
          return api.DeleteWorkspace(id);
        case 'collection':
          return api.DeleteCollection(id);
        case 'folder':
          return api.DeleteFolder(id);
        case 'request':
          return api.DeleteRequest(id);
      }
    });
    setBusy(false);
    if (result) setDeleting(null);
  }
  return (
    <div className="app-shell">
      <Sidebar view={view} setView={setView} edit={setEditing} remove={setDeleting} />
      <main className="main-area">
        <header className="topbar">
          <div className="breadcrumb">
            {workspace?.name || 'Relay'}
            <span>/</span>
            <strong>
              {view === 'collections'
                ? 'Коллекции'
                : view === 'settings'
                  ? 'Настройки'
                  : view === 'schemas'
                    ? 'Protobuf'
                    : 'Окружения'}
            </strong>
          </div>
          <div className="topbar-right">
            <span className="local-only">
              <span aria-hidden="true">◇</span> Локальный workspace
            </span>
            {workspace && (
              <select
                aria-label="Активное окружение"
                value={workspace.activeEnvironmentId}
                onChange={(event) =>
                  void mutate((api) =>
                    api.SaveWorkspace({ ...workspace, activeEnvironmentId: event.target.value }),
                  )
                }
              >
                <option value="">Без окружения</option>
                {data.environments
                  .filter((item) => item.workspaceId === workspaceId)
                  .map((item) => (
                    <option key={item.id} value={item.id}>
                      {item.name}
                    </option>
                  ))}
              </select>
            )}
          </div>
        </header>
        {error && (
          <div className="error-banner" role="alert">
            <span>Изменения не сохранены: {error}</span>
            <button className="icon-button" aria-label="Закрыть ошибку" onClick={clearError}>
              ×
            </button>
          </div>
        )}
        {!loaded ? (
          <div className="large-empty" role="status">
            Открываем workspace…
          </div>
        ) : view === 'environments' ? (
          <Environments key={workspaceId} />
        ) : view === 'settings' ? (
          <Settings />
        ) : view === 'schemas' ? (
          <SchemaManager key={workspaceId} onRequestCreated={() => setView('collections')} />
        ) : (
          <>
            <div className="request-tabs" role="tablist" aria-label="Открытые запросы">
              {visibleIds.map((id) => {
                const tab = tabs[id];
                return (
                  <div className={`request-tab ${id === activeId ? 'active' : ''}`} key={id}>
                    <button
                      role="tab"
                      aria-selected={id === activeId}
                      aria-controls="request-workspace"
                      onClick={() => openRequest(id)}
                    >
                      <span className={`tab-protocol ${tab.draft.protocol}`}>
                        {tab.draft.http?.method || (tab.draft.protocol === 'grpc' ? 'RPC' : 'WS')}
                      </span>
                      <span>{tab.draft.name}</span>
                      {(isDirty(tab) || hasVolatileData(tab)) && (
                        <span className="dirty-dot" aria-label="Несохранённые изменения" />
                      )}
                    </button>
                    <button
                      className="close-tab"
                      aria-label={`Закрыть вкладку ${tab.draft.name}`}
                      onClick={() => requestClose(id)}
                    >
                      ×
                    </button>
                  </div>
                );
              })}
              <button
                className="new-tab"
                aria-label="Новый запрос"
                onClick={() =>
                  setEditing(
                    collections[0]
                      ? { kind: 'request', collectionId: collections[0].id, folderId: '' }
                      : { kind: 'collection' },
                  )
                }
                disabled={!workspaceId}
              >
                +
              </button>
            </div>
            <div className="request-workspace" id="request-workspace">
              {activeId && tabs[activeId]?.draft.workspaceId === workspaceId ? (
                <RequestEditor key={activeId} id={activeId} />
              ) : (
                <div className="welcome">
                  <div className="welcome-art" aria-hidden="true">
                    <span>↗</span>
                    <span className="welcome-orbit" />
                  </div>
                  <div className="eyebrow">API WORKSPACE</div>
                  <h1>От идеи до первого ответа.</h1>
                  <p>
                    Соберите запросы, настройте окружения
                    <br />и держите весь контекст API под рукой.
                  </p>
                  <button
                    className="button primary"
                    onClick={() =>
                      setEditing(
                        workspaceId
                          ? collections[0]
                            ? { kind: 'request', collectionId: collections[0].id, folderId: '' }
                            : { kind: 'collection' }
                          : { kind: 'workspace' },
                      )
                    }
                  >
                    +{' '}
                    {workspaceId
                      ? collections.length
                        ? 'Новый запрос'
                        : 'Создать коллекцию'
                      : 'Создать пространство'}
                  </button>
                  <div className="welcome-features">
                    <div>
                      <span>HTTP</span>
                      <p>Отправка и ответы</p>
                    </div>
                    <div>
                      <span>gRPC</span>
                      <p>Unary и streams</p>
                    </div>
                    <div>
                      <span>WebSocket</span>
                      <p>Сессии и сообщения</p>
                    </div>
                  </div>
                  <p className="welcome-stage">
                    Локальные коллекции и хранилище
                    <br />
                    HTTP/1.0 · 1.1 · 2 · h2c · 3
                  </p>
                </div>
              )}
            </div>
          </>
        )}
        <footer className="statusbar">
          <span>
            <span className="status-dot" />
            {loaded ? 'Workspace готов' : 'Загрузка…'}
          </span>
          <span>Relay {buildInfo?.version || 'dev'}</span>
          <span>⌘/Ctrl Enter — отправить · ⌘/Ctrl S — сохранить</span>
        </footer>
      </main>
      {editing && <EntityDialog entity={editing} onClose={() => setEditing(null)} />}
      {deleting && (
        <Modal title="Удалить безвозвратно?" onClose={() => setDeleting(null)}>
          <p>
            «{deleting.item?.name}» будет удалено
            {deleting.kind === 'request' ? '.' : ' вместе со всем содержимым.'}
          </p>
          {deletingDirty && (
            <p className="warning-text">
              В открытых вкладках есть изменения или данные только в памяти. При удалении они будут
              потеряны.
            </p>
          )}
          <div className="modal-actions">
            <button className="button" onClick={() => setDeleting(null)}>
              Отмена
            </button>
            <button className="button danger" disabled={busy} onClick={() => void remove()}>
              {busy ? 'Удаление…' : 'Удалить'}
            </button>
          </div>
        </Modal>
      )}
      {closingId && tabs[closingId] && (
        <Modal title="Сохранить изменения?" onClose={() => void resolveClose('cancel')}>
          <p>
            Вкладка «{tabs[closingId].draft.name}» содержит изменения или данные только в памяти.
          </p>
          {hasVolatileCredentials(tabs[closingId]) && (
            <p className="warning-text">
              Пароль, токен, API key или секретные metadata находятся только в памяти вкладки и
              будут потеряны при закрытии. Для сохранения используйте секретные переменные
              окружения. Кнопка сохранения записывает определение без этих значений.
            </p>
          )}
          {hasVolatileFiles(tabs[closingId]) && (
            <p className="warning-text">
              Выбранные файлы не сохраняются в определении запроса. После закрытия вкладки их
              потребуется выбрать заново.
            </p>
          )}
          {hasVolatileBody(tabs[closingId]) && (
            <p className="warning-text">
              Тело запроса находится только в памяти. Кнопка «Сохранить включая тело» разрешает
              записать его в SQLite. Чувствительные данные лучше перенести в секретные переменные.
            </p>
          )}
          <div className="modal-actions">
            <button className="button" onClick={() => void resolveClose('cancel')}>
              Отмена
            </button>
            <button className="button" onClick={() => void resolveClose('discard')}>
              Не сохранять
            </button>
            <button className="button primary" onClick={() => void resolveClose('save')}>
              {hasVolatileBody(tabs[closingId])
                ? 'Сохранить включая тело'
                : hasVolatileCredentials(tabs[closingId]) || hasVolatileFiles(tabs[closingId])
                  ? 'Сохранить определение и закрыть'
                  : 'Сохранить'}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}

export function App({ store }: { store: RelayStore }) {
  const theme = useStore(store, (state) => state.data.settings.theme);
  useEffect(() => {
    void store.getState().initialize();
    const pagehide = (event: PageTransitionEvent) => {
      if (!event.persisted) store.getState().dispose();
    };
    window.addEventListener('pagehide', pagehide);
    return () => window.removeEventListener('pagehide', pagehide);
  }, [store]);
  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const update = () => {
      document.documentElement.dataset.theme =
        theme === 'system' ? (media.matches ? 'dark' : 'light') : theme;
    };
    update();
    media.addEventListener('change', update);
    return () => media.removeEventListener('change', update);
  }, [theme]);
  return (
    <StoreContext.Provider value={store}>
      <Shell />
    </StoreContext.Provider>
  );
}
