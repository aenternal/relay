import { useEffect, useState } from 'react';
import { useRelay } from '../lib/context';
import type { Collection, Folder, RequestDefinition, Workspace } from '../lib/types';
import { normalizeHTTP } from '../lib/types';
import { HTTPAuthFields } from './HTTPFields';
import { Modal } from './Modal';
import { PanelSeparator } from './PanelSeparator';
import { ProjectTransferDialog } from './ProjectTransferDialog';
import { RecentExecutions } from './RecentExecutions';

export type Entity =
  | { kind: 'workspace'; item?: Workspace }
  | { kind: 'collection'; item?: Collection }
  | { kind: 'folder'; item?: Folder; collectionId: string; parentId: string }
  | { kind: 'request'; item?: RequestDefinition; collectionId: string; folderId: string };

export function Sidebar({
  view,
  setView,
  edit,
  remove,
}: {
  view: string;
  setView: (view: 'collections' | 'environments' | 'schemas' | 'settings') => void;
  edit: (entity: Entity) => void;
  remove: (entity: Entity) => void;
}) {
  const { data, workspaceId, switchWorkspace, openRequest, activeId, mutate } = useRelay();
  const [search, setSearch] = useState('');
  const [protocol, setProtocol] = useState('all');
  const [sort, setSort] = useState('manual');
  const [width, setWidth] = useState(260);
  const [transferOpen, setTransferOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [authEntity, setAuthEntity] = useState<
    { kind: 'collection'; item: Collection } | { kind: 'folder'; item: Folder } | null
  >(null);
  const [auth, setAuth] = useState(normalizeHTTP({}).auth);
  const [savingAuth, setSavingAuth] = useState(false);
  function openAuth(entity: NonNullable<typeof authEntity>) {
    setAuth({ ...normalizeHTTP({}).auth, type: 'inherit', ...entity.item.auth });
    setAuthEntity(entity);
  }
  const invalidAuth = [auth.password, auth.token, auth.keyValue].some(
    (value) => value && !/^\s*\{\{[^{}]+\}\}\s*$/.test(value),
  );
  async function saveAuth() {
    if (!authEntity || invalidAuth) return;
    setSavingAuth(true);
    const entity = authEntity;
    const result = await mutate((api) =>
      entity.kind === 'folder'
        ? api.SaveFolder({ ...entity.item, auth })
        : api.SaveCollection({ ...entity.item, auth }),
    );
    setSavingAuth(false);
    if (result) setAuthEntity(null);
  }
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  useEffect(() => {
    const close = (event: PointerEvent | KeyboardEvent) => {
      if (event instanceof KeyboardEvent && event.key !== 'Escape') return;
      document.querySelectorAll<HTMLDetailsElement>('.row-menu[open]').forEach((menu) => {
        if (event instanceof KeyboardEvent || !menu.contains(event.target as Node))
          menu.open = false;
      });
    };
    document.addEventListener('pointerdown', close);
    document.addEventListener('keydown', close);
    return () => {
      document.removeEventListener('pointerdown', close);
      document.removeEventListener('keydown', close);
    };
  }, []);
  const workspace = data.workspaces.find((item) => item.id === workspaceId);
  const workspaceCollections = data.collections.filter((item) => item.workspaceId === workspaceId);
  const workspaceRequests = data.requests.filter((item) => item.workspaceId === workspaceId);
  const folders = data.folders.filter((item) => item.workspaceId === workspaceId);
  const folderById = new Map(folders.map((folder) => [folder.id, folder]));
  const query = search.trim().toLocaleLowerCase();
  const filtering = !!query || protocol !== 'all';
  function sorted<T extends { name: string }>(items: T[]) {
    return sort === 'manual'
      ? items
      : [...items].sort(
          (left, right) => (sort === 'name-desc' ? -1 : 1) * left.name.localeCompare(right.name),
        );
  }
  const requests = workspaceRequests.filter((request) => {
    if (protocol !== 'all' && request.protocol !== protocol) return false;
    if (!query) return true;
    const location = [
      workspaceCollections.find((item) => item.id === request.collectionId)?.name || '',
    ];
    const seen = new Set<string>();
    let folder = folderById.get(request.folderId);
    while (folder && !seen.has(folder.id)) {
      seen.add(folder.id);
      location.push(folder.name);
      folder = folderById.get(folder.parentId);
    }
    const text = [
      request.name,
      request.protocol,
      request.http?.method,
      request.http?.url,
      request.grpc?.endpoint,
      request.grpc?.method,
      request.websocket?.url,
      ...location,
    ]
      .join(' ')
      .toLocaleLowerCase();
    return query.split(/\s+/).every((word) => text.includes(word));
  });
  const visibleFolders = new Set<string>();
  const showFolder = (id: string) => {
    let folder = folderById.get(id);
    while (folder && !visibleFolders.has(folder.id)) {
      visibleFolders.add(folder.id);
      folder = folderById.get(folder.parentId);
    }
  };
  requests.forEach((request) => showFolder(request.folderId));
  if (protocol === 'all')
    folders
      .filter((folder) => !query || folder.name.toLocaleLowerCase().includes(query))
      .forEach((folder) => showFolder(folder.id));
  const collections = sorted(
    workspaceCollections.filter(
      (collection) =>
        !filtering ||
        requests.some((request) => request.collectionId === collection.id) ||
        folders.some(
          (folder) => folder.collectionId === collection.id && visibleFolders.has(folder.id),
        ) ||
        (protocol === 'all' && collection.name.toLocaleLowerCase().includes(query)),
    ),
  );
  function toggle(id: string) {
    setCollapsed((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }
  function requestRow(request: RequestDefinition) {
    return (
      <div
        className={`tree-row request-row ${activeId === request.id && view === 'collections' ? 'active' : ''}`}
        key={request.id}
      >
        <button
          className="tree-name"
          onClick={() => {
            openRequest(request.id);
            setView('collections');
          }}
        >
          <span className={`protocol-label ${request.protocol}`}>
            {request.http?.method || (request.protocol === 'grpc' ? 'RPC' : 'WS')}
          </span>
          <span>{request.name}</span>
        </button>
        <details className="row-menu">
          <summary aria-label={`Действия запроса ${request.name}`}>⋯</summary>
          <div>
            <button
              onClick={() =>
                edit({
                  kind: 'request',
                  item: request,
                  collectionId: request.collectionId,
                  folderId: request.folderId,
                })
              }
            >
              Переименовать
            </button>
            <button
              className="danger-text"
              onClick={() =>
                remove({
                  kind: 'request',
                  item: request,
                  collectionId: request.collectionId,
                  folderId: request.folderId,
                })
              }
            >
              Удалить
            </button>
          </div>
        </details>
      </div>
    );
  }
  function folderRows(collectionId: string, parentId: string, depth = 0): React.ReactNode {
    if (depth > 50) return null;
    return sorted(
      folders.filter(
        (folder) =>
          folder.collectionId === collectionId &&
          folder.parentId === parentId &&
          (!filtering || visibleFolders.has(folder.id)),
      ),
    ).map((folder) => (
      <div className="tree-folder" key={folder.id}>
        <div className="tree-row">
          <button
            className="tree-name"
            aria-expanded={!collapsed.has(folder.id)}
            onClick={() => toggle(folder.id)}
          >
            <span className="chevron">{collapsed.has(folder.id) ? '›' : '⌄'}</span>
            <span className="folder-icon" aria-hidden="true">
              ▱
            </span>
            <span>{folder.name}</span>
          </button>
          <details className="row-menu">
            <summary aria-label={`Действия папки ${folder.name}`}>⋯</summary>
            <div>
              <button onClick={() => edit({ kind: 'request', collectionId, folderId: folder.id })}>
                Новый запрос
              </button>
              <button onClick={() => edit({ kind: 'folder', collectionId, parentId: folder.id })}>
                Вложенная папка
              </button>
              <button
                onClick={() =>
                  edit({ kind: 'folder', item: folder, collectionId, parentId: folder.parentId })
                }
              >
                Переименовать
              </button>
              <button onClick={() => openAuth({ kind: 'folder', item: folder })}>
                Авторизация
              </button>
              <button
                className="danger-text"
                onClick={() =>
                  remove({
                    kind: 'folder',
                    item: folder,
                    collectionId,
                    parentId: folder.parentId,
                  })
                }
              >
                Удалить
              </button>
            </div>
          </details>
        </div>
        {(!collapsed.has(folder.id) || filtering) && (
          <div className="tree-children">
            {folderRows(collectionId, folder.id, depth + 1)}
            {sorted(requests.filter((item) => item.folderId === folder.id)).map(requestRow)}
          </div>
        )}
      </div>
    ));
  }
  return (
    <aside
      className="sidebar"
      style={{ width }}
      onClick={(event) => {
        const target = event.target as HTMLElement;
        if (target.closest('button')) {
          const menu = target.closest<HTMLDetailsElement>('details.row-menu');
          if (menu) menu.open = false;
        }
      }}
    >
      <div className="brand">
        <span className="brand-mark">
          r<span>·</span>
        </span>
        <strong>relay</strong>
        <span className="stage-badge">LOCAL</span>
      </div>
      <div className="workspace-block">
        <label className="eyebrow" htmlFor="workspace">
          РАБОЧЕЕ ПРОСТРАНСТВО
        </label>
        <div className="workspace-picker">
          <select
            id="workspace"
            value={workspaceId}
            onChange={(event) => switchWorkspace(event.target.value)}
            aria-label="Рабочее пространство"
          >
            {data.workspaces.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
          <details className="row-menu">
            <summary aria-label="Действия рабочего пространства">⋯</summary>
            <div>
              <button onClick={() => edit({ kind: 'workspace' })}>Создать пространство</button>
              {workspace && (
                <>
                  <button onClick={() => edit({ kind: 'workspace', item: workspace })}>
                    Переименовать
                  </button>
                  <button
                    className="danger-text"
                    onClick={() => remove({ kind: 'workspace', item: workspace })}
                  >
                    Удалить пространство
                  </button>
                </>
              )}
            </div>
          </details>
        </div>
      </div>
      <nav className="sidebar-nav" aria-label="Разделы">
        <button
          className={view === 'collections' ? 'selected' : ''}
          onClick={() => setView('collections')}
        >
          <span aria-hidden="true">▧</span>Коллекции
          <span className="nav-count">{workspaceCollections.length}</span>
        </button>
        <button
          className={view === 'environments' ? 'selected' : ''}
          onClick={() => setView('environments')}
        >
          <span aria-hidden="true">◇</span>Окружения
        </button>
        <button className={view === 'schemas' ? 'selected' : ''} onClick={() => setView('schemas')}>
          <span aria-hidden="true">⌘</span>Protobuf
        </button>
        <button onClick={() => setHistoryOpen(true)}>
          <span aria-hidden="true">↺</span>История запросов
        </button>
      </nav>
      <div className="collection-tools">
        <span className="eyebrow">КОЛЛЕКЦИИ</span>
        <button
          className="icon-button"
          aria-label="Создать коллекцию"
          disabled={!workspaceId}
          onClick={() => edit({ kind: 'collection' })}
        >
          +
        </button>
      </div>
      <div className="search-box">
        <span aria-hidden="true">⌕</span>
        <input
          aria-label="Поиск запросов"
          placeholder="Имя, адрес, метод…"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
        />
      </div>
      <div className="collection-filters">
        <select
          aria-label="Протокол запросов"
          value={protocol}
          onChange={(event) => setProtocol(event.target.value)}
        >
          <option value="all">Все протоколы</option>
          <option value="http">HTTP</option>
          <option value="grpc">gRPC</option>
          <option value="websocket">WebSocket</option>
        </select>
        <select
          aria-label="Сортировка запросов"
          value={sort}
          onChange={(event) => setSort(event.target.value)}
        >
          <option value="manual">Исходный порядок</option>
          <option value="name-asc">Имя А → Я</option>
          <option value="name-desc">Имя Я → А</option>
        </select>
      </div>
      {filtering && (
        <p className="search-count" role="status">
          Найдено запросов: {requests.length} / {workspaceRequests.length}
        </p>
      )}
      <div className="collection-tree" aria-label="Дерево коллекций">
        {collections.map((collection) => (
          <div className="collection" key={collection.id}>
            <div className="tree-row collection-row">
              <button
                className="tree-name"
                aria-expanded={!collapsed.has(collection.id)}
                onClick={() => toggle(collection.id)}
              >
                <span className="chevron">{collapsed.has(collection.id) ? '›' : '⌄'}</span>
                <span>{collection.name}</span>
              </button>
              <details className="row-menu">
                <summary aria-label={`Действия коллекции ${collection.name}`}>⋯</summary>
                <div>
                  <button
                    onClick={() =>
                      edit({ kind: 'request', collectionId: collection.id, folderId: '' })
                    }
                  >
                    Новый запрос
                  </button>
                  <button
                    onClick={() =>
                      edit({ kind: 'folder', collectionId: collection.id, parentId: '' })
                    }
                  >
                    Новая папка
                  </button>
                  <button onClick={() => edit({ kind: 'collection', item: collection })}>
                    Переименовать
                  </button>
                  <button onClick={() => openAuth({ kind: 'collection', item: collection })}>
                    Авторизация
                  </button>
                  <button
                    className="danger-text"
                    onClick={() => remove({ kind: 'collection', item: collection })}
                  >
                    Удалить
                  </button>
                </div>
              </details>
            </div>
            {(!collapsed.has(collection.id) || filtering) && (
              <div className="tree-children">
                {folderRows(collection.id, '')}
                {sorted(
                  requests.filter((item) => item.collectionId === collection.id && !item.folderId),
                ).map(requestRow)}
                <button
                  className="tree-add"
                  onClick={() =>
                    edit({ kind: 'request', collectionId: collection.id, folderId: '' })
                  }
                >
                  + Новый запрос
                </button>
              </div>
            )}
          </div>
        ))}
        {!collections.length && filtering && (
          <p className="sidebar-empty">Запросы не найдены. Измените поиск или протокол.</p>
        )}
        {!workspaceCollections.length && !filtering && (
          <div className="sidebar-empty">
            Соберите запросы
            <br />в свою первую коллекцию.
            <button
              className="text-button"
              onClick={() => edit({ kind: 'collection' })}
              disabled={!workspaceId}
            >
              Создать коллекцию
            </button>
          </div>
        )}
      </div>
      <div className="sidebar-bottom">
        <button onClick={() => setTransferOpen(true)}>
          <span aria-hidden="true">⇄</span>Импорт / экспорт
        </button>
        <button
          className={view === 'settings' ? 'selected' : ''}
          onClick={() => setView('settings')}
        >
          <span aria-hidden="true">⚙</span>Настройки
        </button>
        <div className="local-label">
          <span className="status-dot" />
          Локальное хранилище
        </div>
      </div>
      <PanelSeparator
        label="Ширина списка коллекций"
        orientation="vertical"
        value={width}
        min={220}
        max={480}
        onChange={setWidth}
      />
      {transferOpen && <ProjectTransferDialog onClose={() => setTransferOpen(false)} />}
      {historyOpen && (
        <RecentExecutions
          onClose={() => {
            setHistoryOpen(false);
            setView('collections');
          }}
        />
      )}
      {authEntity && (
        <Modal title={`Авторизация: ${authEntity.item.name}`} onClose={() => setAuthEntity(null)}>
          <div className="http-settings">
            <HTTPAuthFields auth={auth} onChange={setAuth} />
          </div>
          <p className={invalidAuth ? 'warning-text' : 'hint'}>
            В папках и коллекциях пароль, токен и API key сохраняются только как ссылки{' '}
            {'{{variable}}'}.
          </p>
          <div className="modal-actions">
            <button className="button" onClick={() => setAuthEntity(null)}>
              Отмена
            </button>
            <button
              className="button primary"
              disabled={invalidAuth || savingAuth}
              onClick={() => void saveAuth()}
            >
              {savingAuth ? 'Сохранение…' : 'Сохранить авторизацию'}
            </button>
          </div>
        </Modal>
      )}
    </aside>
  );
}
