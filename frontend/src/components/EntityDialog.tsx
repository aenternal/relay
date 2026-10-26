import { useState } from 'react';
import { useRelay } from '../lib/context';
import { newRequest } from '../lib/types';
import type { Entity } from './Sidebar';
import { Modal } from './Modal';

const labels = {
  workspace: 'пространство',
  collection: 'коллекцию',
  folder: 'папку',
  request: 'запрос',
};

export function EntityDialog({ entity, onClose }: { entity: Entity; onClose: () => void }) {
  const { workspaceId, mutate, switchWorkspace, openRequest, tabs, editRequest, saveRequest } =
    useRelay();
  const [name, setName] = useState(entity.item?.name ?? '');
  const [protocol, setProtocol] = useState('http');
  const [busy, setBusy] = useState(false);
  async function submit() {
    if (!name.trim() || busy) return;
    setBusy(true);
    const previousIds = new Set<string>();
    const state = await mutate(async (api) => {
      const before = await api.LoadState();
      before.workspaces.forEach((item) => previousIds.add(item.id));
      before.requests.forEach((item) => previousIds.add(item.id));
      switch (entity.kind) {
        case 'workspace':
          return api.SaveWorkspace({
            id: '',
            activeEnvironmentId: '',
            ...entity.item,
            name: name.trim(),
          });
        case 'collection':
          return api.SaveCollection({ id: '', workspaceId, ...entity.item, name: name.trim() });
        case 'folder':
          return api.SaveFolder({
            id: '',
            workspaceId,
            collectionId: entity.collectionId,
            parentId: entity.parentId,
            ...entity.item,
            name: name.trim(),
          });
        case 'request':
          return api.SaveRequest({
            ...(entity.item ??
              newRequest(workspaceId, entity.collectionId, entity.folderId, protocol)),
            name: name.trim(),
          });
      }
    });
    if (state) {
      if (entity.kind === 'workspace' && !entity.item) {
        const created = state.workspaces.find((item) => !previousIds.has(item.id));
        if (created) switchWorkspace(created.id);
      }
      if (entity.kind === 'request') {
        const request = entity.item
          ? state.requests.find((item) => item.id === entity.item!.id)
          : state.requests.find((item) => !previousIds.has(item.id));
        if (request) {
          const tab = tabs[request.id];
          if (tab) {
            editRequest(request.id, { ...tab.draft, name: request.name });
            await saveRequest(request.id);
          }
          openRequest(request.id);
        }
      }
      onClose();
    }
    setBusy(false);
  }
  return (
    <Modal
      title={entity.item ? 'Переименовать' : `Создать ${labels[entity.kind]}`}
      onClose={onClose}
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <label className="form-field">
          Название
          <input
            value={name}
            onChange={(event) => setName(event.target.value)}
            maxLength={200}
            required
          />
        </label>
        {entity.kind === 'request' && !entity.item && (
          <label className="form-field">
            Протокол
            <select value={protocol} onChange={(event) => setProtocol(event.target.value)}>
              <option value="http">HTTP</option>
              <option value="grpc">gRPC — выполнение планируется</option>
              <option value="websocket">WebSocket — соединение планируется</option>
            </select>
          </label>
        )}
        <div className="modal-actions">
          <button type="button" className="button" onClick={onClose}>
            Отмена
          </button>
          <button className="button primary" disabled={!name.trim() || busy}>
            {busy ? 'Сохранение…' : entity.item ? 'Сохранить' : 'Создать'}
          </button>
        </div>
      </form>
    </Modal>
  );
}
