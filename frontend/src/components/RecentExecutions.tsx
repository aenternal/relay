import { useState } from 'react';
import { useRelay } from '../lib/context';
import { Modal } from './Modal';

export function RecentExecutions({ onClose }: { onClose: () => void }) {
  const { data, workspaceId, runs, grpcRuns, websocketRuns, openRequest } = useRelay();
  const [search, setSearch] = useState('');
  const [protocol, setProtocol] = useState('all');
  const [state, setState] = useState('all');
  const [order, setOrder] = useState('newest');
  const requests = data.requests.filter((request) => request.workspaceId === workspaceId);
  const rows = requests.flatMap((request) => {
    const execution =
      request.protocol === 'http'
        ? runs[request.id]?.execution
        : request.protocol === 'grpc'
          ? grpcRuns[request.id]?.execution
          : websocketRuns[request.id]?.execution;
    if (!execution) return [];
    const endpoint =
      request.protocol === 'grpc'
        ? grpcRuns[request.id]?.execution?.endpoint || ''
        : request.protocol === 'http'
          ? runs[request.id]?.execution?.url || ''
          : websocketRuns[request.id]?.execution?.url || '';
    const status =
      request.protocol === 'http'
        ? runs[request.id]?.execution?.status || ''
        : request.protocol === 'grpc'
          ? grpcRuns[request.id]?.execution?.codeName || ''
          : String(websocketRuns[request.id]?.execution?.closeCode || '');
    return [{ request, execution, endpoint, status }];
  });
  const query = search.trim().toLocaleLowerCase();
  const visible = rows
    .filter(
      (row) =>
        (protocol === 'all' || row.request.protocol === protocol) &&
        (state === 'all' ||
          (state === 'active'
            ? ['running', 'connecting', 'open', 'closing'].includes(row.execution.state)
            : state === 'failed'
              ? row.execution.state === 'failed' || !!row.execution.error
              : !['running', 'connecting', 'open', 'closing'].includes(row.execution.state) &&
                row.execution.state !== 'failed' &&
                !row.execution.error)) &&
        (!query ||
          [row.request.name, row.endpoint, row.status].some((value) =>
            value.toLocaleLowerCase().includes(query),
          )),
    )
    .sort((left, right) =>
      order === 'name'
        ? left.request.name.localeCompare(right.request.name)
        : order === 'oldest'
          ? left.execution.startedAt.localeCompare(right.execution.startedAt)
          : right.execution.startedAt.localeCompare(left.execution.startedAt),
    );
  return (
    <Modal title="История запросов" onClose={onClose}>
      <div className="recent-executions">
        <p className="hint">
          Последний доступный результат каждого запроса в текущей сессии. Сохранённые
          WebSocket-сессии находятся в панели ответа WebSocket.
        </p>
        <input
          type="search"
          aria-label="Поиск по истории"
          placeholder="Имя, адрес или статус…"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
        />
        <div className="history-filters">
          <select
            aria-label="Протокол истории"
            value={protocol}
            onChange={(event) => setProtocol(event.target.value)}
          >
            <option value="all">Все протоколы</option>
            <option value="http">HTTP</option>
            <option value="grpc">gRPC</option>
            <option value="websocket">WebSocket</option>
          </select>
          <select
            aria-label="Состояние истории"
            value={state}
            onChange={(event) => setState(event.target.value)}
          >
            <option value="all">Все состояния</option>
            <option value="active">Активные</option>
            <option value="complete">Завершённые</option>
            <option value="failed">С ошибкой</option>
          </select>
          <select
            aria-label="Сортировка истории"
            value={order}
            onChange={(event) => setOrder(event.target.value)}
          >
            <option value="newest">Сначала новые</option>
            <option value="oldest">Сначала старые</option>
            <option value="name">По имени</option>
          </select>
        </div>
        <p className="hint" role="status">
          Найдено: {visible.length}
        </p>
        <div className="recent-list">
          {visible.slice(0, 100).map((row) => (
            <button
              key={row.execution.id}
              className="recent-row"
              onClick={() => {
                openRequest(row.request.id);
                onClose();
              }}
            >
              <span className={`protocol-label ${row.request.protocol}`}>
                {row.request.protocol === 'websocket' ? 'WS' : row.request.protocol.toUpperCase()}
              </span>
              <strong>{row.request.name}</strong>
              <span className="quiet-badge">
                {row.execution.state} {row.status}
              </span>
              <small>{row.endpoint}</small>
              <time>{row.execution.startedAt}</time>
              {row.execution.error && <span className="error-text">{row.execution.error}</span>}
            </button>
          ))}
          {!visible.length && (
            <p className="inline-empty">Выполнения по выбранным условиям не найдены.</p>
          )}
          {visible.length > 100 && (
            <p className="hint">Показаны первые 100 результатов. Уточните фильтр.</p>
          )}
        </div>
      </div>
    </Modal>
  );
}
