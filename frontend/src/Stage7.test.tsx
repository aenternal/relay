import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { createAppStore } from './lib/store';
import type { RelayStore } from './lib/store';
import { newRequest } from './lib/types';
import type { ProjectPreview, ProjectStats } from './lib/types';
import { PROJECT_MAX_BYTES } from './lib/transfer';
import {
  fixtureExecution,
  fixtureGRPCExecution,
  fixtureState,
  fixtureWebSocketExecution,
  testBackend,
} from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  act(() => stores.forEach((store) => store.getState().dispose()));
  stores.length = 0;
  vi.restoreAllMocks();
});
const stats: ProjectStats = {
  collections: 1,
  folders: 2,
  requests: 3,
  environments: 1,
  schemas: 1,
  secretVariables: 2,
};
const preview: ProjectPreview = {
  format: 'native',
  workspaceName: 'Перенесённый проект',
  stats,
  warnings: [{ code: 'scripts_omitted', path: 'items[0]', message: '<script>ignored</script>' }],
};

function mount(backend = testBackend()) {
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { user: userEvent.setup(), store, backend };
}

async function transfer(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Импорт / экспорт' }));
  return within(screen.getByRole('dialog', { name: 'Импорт / экспорт проекта' }));
}

describe('project transfer UI', () => {
  it('keeps workspace shortcuts and Escape from affecting a request behind the transfer dialog', async () => {
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(fixtureExecution('r1', { state: 'running' }));
    backend.api.GetHTTPExecution.mockResolvedValue(fixtureExecution('r1', { state: 'running' }));
    const { user, store } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Cancel' });
    act(() =>
      store
        .getState()
        .editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Unsaved draft' }),
    );
    const dialog = await transfer(user);
    await user.click(dialog.getByRole('textbox', { name: 'JSON проекта' }));
    await user.keyboard('{Control>}s{/Control}{Control>}w{/Control}');
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    expect(store.getState().closingId).toBe('');
    expect(store.getState().tabs.r1.draft.name).toBe('Unsaved draft');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(backend.api.CancelHTTPExecution).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Имя запроса' })).toHaveValue('Unsaved draft');
  });

  it.each([false, true])(
    'requires preview and imports bodies only with explicit choice: %s',
    async (includeBodies) => {
      const backend = testBackend();
      backend.api.PreviewProjectImport.mockResolvedValue(preview);
      const imported = fixtureState();
      imported.workspaces.push({ id: 'w2', name: preview.workspaceName, activeEnvironmentId: '' });
      imported.collections.push({ id: 'c2', workspaceId: 'w2', name: 'Imported' });
      imported.requests.push({ ...newRequest('w2', 'c2'), id: 'r2', name: 'Imported request' });
      backend.api.ImportProject.mockResolvedValue({
        state: imported,
        workspaceId: 'w2',
        stats,
        warnings: preview.warnings,
      });
      const { user, store } = mount(backend),
        dialog = await transfer(user);
      const apply = dialog.getByRole('button', { name: 'Импортировать в новое пространство' });
      expect(apply).toBeDisabled();
      expect(
        dialog.getByRole('checkbox', { name: 'Сохранить тела запросов из файла' }),
      ).not.toBeChecked();
      if (includeBodies)
        await user.click(
          dialog.getByRole('checkbox', { name: 'Сохранить тела запросов из файла' }),
        );
      fireEvent.change(dialog.getByRole('textbox', { name: 'JSON проекта' }), {
        target: { value: '{"format":"relay"}' },
      });
      await user.click(dialog.getByRole('button', { name: 'Проверить импорт' }));
      await dialog.findByLabelText('Предпросмотр импорта');
      expect(dialog.getByText('<script>ignored</script>')).toBeInTheDocument();
      expect(screen.getByRole('dialog').querySelector('script')).toBeNull();
      expect(backend.api.ImportProject).not.toHaveBeenCalled();
      await user.click(apply);
      await dialog.findByText('Проект импортирован в новое рабочее пространство.');
      expect(backend.api.ImportProject).toHaveBeenCalledWith(
        'native',
        '{"format":"relay"}',
        includeBodies,
      );
      expect(store.getState().workspaceId).toBe('w2');
    },
  );

  it('invalidates a preview when JSON or format changes and accepts bodies only by explicit choice', async () => {
    const backend = testBackend();
    backend.api.PreviewProjectImport.mockResolvedValue(preview);
    const { user } = mount(backend),
      dialog = await transfer(user);
    const input = dialog.getByRole('textbox', { name: 'JSON проекта' });
    fireEvent.change(input, { target: { value: '{}' } });
    await user.click(dialog.getByRole('button', { name: 'Проверить импорт' }));
    await dialog.findByLabelText('Предпросмотр импорта');
    fireEvent.change(input, { target: { value: '{"changed":true}' } });
    expect(
      dialog.getByRole('button', { name: 'Импортировать в новое пространство' }),
    ).toBeDisabled();
    expect(dialog.queryByLabelText('Предпросмотр импорта')).not.toBeInTheDocument();
    await user.selectOptions(
      dialog.getByRole('combobox', { name: 'Формат импорта проекта' }),
      'postman',
    );
    await user.click(dialog.getByRole('checkbox', { name: 'Сохранить тела запросов из файла' }));
    await user.click(dialog.getByRole('button', { name: 'Проверить импорт' }));
    await waitFor(() =>
      expect(backend.api.PreviewProjectImport).toHaveBeenLastCalledWith(
        'postman',
        '{"changed":true}',
      ),
    );
    await user.selectOptions(
      dialog.getByRole('combobox', { name: 'Формат импорта проекта' }),
      'insomnia',
    );
    expect(
      dialog.getByRole('button', { name: 'Импортировать в новое пространство' }),
    ).toBeDisabled();
    expect(backend.api.ImportProject).not.toHaveBeenCalled();
  });

  it('displays a bounded parser error and ignores a preview that finishes after closing', async () => {
    const backend = testBackend();
    backend.api.PreviewProjectImport.mockRejectedValueOnce(
      new Error('Insomnia export version is unsupported'),
    );
    const { user } = mount(backend);
    let dialog = await transfer(user);
    fireEvent.change(dialog.getByRole('textbox', { name: 'JSON проекта' }), {
      target: { value: '{}' },
    });
    await user.click(dialog.getByRole('button', { name: 'Проверить импорт' }));
    expect(await dialog.findByRole('alert')).toHaveTextContent('version is unsupported');
    let finish!: (value: ProjectPreview) => void;
    backend.api.PreviewProjectImport.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    await user.click(dialog.getByRole('button', { name: 'Проверить импорт' }));
    await user.click(dialog.getByRole('button', { name: 'Закрыть диалог' }));
    dialog = await transfer(user);
    await act(async () => {
      finish(preview);
    });
    expect(dialog.queryByLabelText('Предпросмотр импорта')).not.toBeInTheDocument();
    expect(dialog.getByRole('textbox', { name: 'JSON проекта' })).toHaveValue('');
  });

  it('rejects an oversized file before reading it and keeps apply disabled', async () => {
    const { user, backend } = mount(),
      dialog = await transfer(user);
    const file = new File(['{}'], 'oversized.json', { type: 'application/json' });
    Object.defineProperty(file, 'size', { value: PROJECT_MAX_BYTES + 1 });
    const read = vi.fn();
    Object.defineProperty(file, 'text', { value: read });
    await user.upload(dialog.getByLabelText('Файл проекта'), file);
    expect(await dialog.findByRole('alert')).toHaveTextContent('превышает 32 МиБ');
    expect(read).not.toHaveBeenCalled();
    expect(backend.api.PreviewProjectImport).not.toHaveBeenCalled();
    expect(
      dialog.getByRole('button', { name: 'Импортировать в новое пространство' }),
    ).toBeDisabled();
  });

  it('uses native file dialogs, handles cancellation, and saves only after export review', async () => {
    const backend = testBackend();
    backend.api.NativeFilesAvailable.mockResolvedValue(true);
    backend.api.SelectProjectFile.mockResolvedValueOnce({
      name: '',
      data: '',
    }).mockResolvedValueOnce({ name: 'project.json', data: '{}' });
    backend.api.PreviewProjectImport.mockResolvedValue(preview);
    backend.api.ExportProject.mockResolvedValue({
      filename: 'project.json',
      data: '{}',
      stats,
      warnings: [],
    });
    backend.api.SaveProjectFile.mockResolvedValue({ saved: true, stats, warnings: [] });
    const { user } = mount(backend),
      dialog = await transfer(user);
    await dialog.findByRole('button', { name: 'Выбрать файл проекта' });
    await user.click(dialog.getByRole('button', { name: 'Выбрать файл проекта' }));
    expect(dialog.queryByRole('alert')).not.toBeInTheDocument();
    await user.click(dialog.getByRole('button', { name: 'Выбрать файл проекта' }));
    await waitFor(() =>
      expect(dialog.getByRole('textbox', { name: 'JSON проекта' })).toHaveValue('{}'),
    );
    await user.click(dialog.getByRole('tab', { name: 'Экспорт' }));
    const save = dialog.getByRole('button', { name: 'Сохранить JSON…' });
    expect(save).toBeDisabled();
    expect(
      dialog.getByRole('checkbox', { name: 'Включить тела запросов в экспорт' }),
    ).not.toBeChecked();
    await user.click(dialog.getByRole('button', { name: 'Подготовить экспорт' }));
    await waitFor(() => expect(save).toBeEnabled());
    expect(backend.api.ExportProject).toHaveBeenCalledWith('w1', false);
    expect(backend.api.SaveProjectFile).not.toHaveBeenCalled();
    await user.click(dialog.getByRole('checkbox', { name: 'Включить тела запросов в экспорт' }));
    expect(save).toBeDisabled();
    await user.click(dialog.getByRole('button', { name: 'Подготовить экспорт' }));
    await waitFor(() => expect(save).toBeEnabled());
    await user.click(save);
    await dialog.findByText('Проект сохранён в JSON-файл.');
    expect(backend.api.SaveProjectFile).toHaveBeenCalledWith('w1', true);
  });
});

describe('workspace navigation and recent results', () => {
  it('searches addresses, methods and folder paths while preserving matching ancestors and workspace scope', async () => {
    const state = fixtureState();
    state.folders = [
      { id: 'f1', workspaceId: 'w1', collectionId: 'c1', parentId: '', name: 'Catalog v2' },
      { id: 'f2', workspaceId: 'w1', collectionId: 'c1', parentId: 'f1', name: 'Users' },
    ];
    const grpc = { ...newRequest('w1', 'c1', 'f2', 'grpc'), id: 'g1', name: 'List users' };
    grpc.grpc!.endpoint = 'catalog.grpc.test:443';
    grpc.grpc!.method = '/api.Catalog/List';
    state.requests.push(grpc, {
      ...newRequest('w1', 'c1', '', 'websocket'),
      id: 'ws1',
      name: 'Events',
    });
    state.workspaces.push({ id: 'w2', name: 'Other', activeEnvironmentId: '' });
    state.collections.push({ id: 'c2', workspaceId: 'w2', name: 'Other' });
    state.requests.push({
      ...newRequest('w2', 'c2'),
      id: 'other',
      name: 'catalog.grpc.test other',
    });
    const { user } = mount(testBackend(state));
    await user.click(await screen.findByRole('button', { name: /Проверки API/ }));
    const search = screen.getByRole('searchbox', { name: 'Поиск запросов' });
    await user.type(search, 'catalog.grpc.test');
    expect(screen.getByRole('button', { name: 'RPC List users' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Catalog v2/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Users/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /other/i })).not.toBeInTheDocument();
    await user.clear(search);
    await user.type(search, 'v2 List');
    expect(screen.getByRole('button', { name: 'RPC List users' })).toBeInTheDocument();
    await user.clear(search);
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Протокол запросов' }),
      'websocket',
    );
    expect(screen.getByRole('button', { name: 'WS Events' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'RPC List users' })).not.toBeInTheDocument();
    expect(screen.getByText('Найдено запросов: 1 / 3')).toBeInTheDocument();
  });

  it('sorts request rows and supports keyboard resizing without losing the edited draft', async () => {
    const state = fixtureState();
    state.requests[0].name = 'Zeta';
    state.requests.push({ ...newRequest('w1', 'c1'), id: 'r2', name: 'Alpha' });
    const { user } = mount(testBackend(state));
    await screen.findByRole('button', { name: 'GET Zeta' });
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Сортировка запросов' }),
      'name-asc',
    );
    expect(
      within(screen.getByLabelText('Дерево коллекций'))
        .getAllByRole('button', { name: /^GET (Alpha|Zeta)$/ })
        .map((button) => button.textContent),
    ).toEqual(['GETAlpha', 'GETZeta']);
    const sidebar = screen.getByRole('separator', { name: 'Ширина списка коллекций' });
    sidebar.focus();
    await user.keyboard('{ArrowRight}');
    expect(sidebar).toHaveAttribute('aria-valuenow', '280');
    await user.keyboard('{Home}');
    expect(sidebar).toHaveAttribute('aria-valuenow', '220');
    await user.click(screen.getByRole('button', { name: 'GET Alpha' }));
    await user.type(screen.getByRole('textbox', { name: 'Имя запроса' }), ' edited');
    const panel = screen.getByRole('separator', { name: 'Размер панели запроса' });
    panel.focus();
    await user.keyboard('{ArrowDown}');
    expect(panel).toHaveAttribute('aria-valuenow', '340');
    expect(screen.getByRole('textbox', { name: 'Имя запроса' })).toHaveValue('Alpha edited');
    await user.keyboard('{Control>}k{/Control}');
    expect(screen.getByRole('searchbox', { name: 'Поиск запросов' })).toHaveFocus();
  });

  it('filters the latest results across protocols without exposing payloads and reopens the selected request', async () => {
    const state = fixtureState();
    state.requests.push(
      { ...newRequest('w1', 'c1', '', 'grpc'), id: 'g1', name: 'Catalog RPC' },
      { ...newRequest('w1', 'c1', '', 'websocket'), id: 'ws1', name: 'Events' },
    );
    const { user, store } = mount(testBackend(state));
    await screen.findByRole('button', { name: 'GET Проверка сервиса' });
    act(() =>
      store.setState({
        runs: {
          r1: {
            starting: false,
            error: '',
            execution: fixtureExecution('r1', {
              startedAt: '2026-10-25T12:00:00Z',
              bodyText: 'private-body-canary',
            }),
          },
        },
        grpcRuns: {
          g1: {
            starting: false,
            kind: 'unary',
            error: '',
            execution: fixtureGRPCExecution('g1', {
              state: 'failed',
              error: 'Unavailable',
              codeName: 'Unavailable',
            }),
          },
        },
        websocketRuns: {
          ws1: {
            starting: false,
            execution: fixtureWebSocketExecution('ws1', { startedAt: '2026-10-25T13:00:00Z' }),
            error: '',
            events: [],
            evictedEvents: 0,
            missedEvents: 0,
            lastSequence: 0,
            draining: false,
            sending: false,
            pinging: false,
            disconnecting: false,
            notice: '',
          },
        },
      }),
    );
    await user.click(screen.getByRole('button', { name: 'История запросов' }));
    const dialog = within(screen.getByRole('dialog', { name: 'История запросов' }));
    expect(dialog.getByText('Найдено: 3')).toBeInTheDocument();
    expect(dialog.queryByText('private-body-canary')).not.toBeInTheDocument();
    await user.selectOptions(dialog.getByRole('combobox', { name: 'Состояние истории' }), 'failed');
    expect(dialog.getByText('Найдено: 1')).toBeInTheDocument();
    await user.selectOptions(dialog.getByRole('combobox', { name: 'Состояние истории' }), 'all');
    await user.selectOptions(
      dialog.getByRole('combobox', { name: 'Протокол истории' }),
      'websocket',
    );
    expect(dialog.queryByRole('button', { name: /Catalog RPC/ })).not.toBeInTheDocument();
    await user.click(dialog.getByRole('button', { name: /Events/ }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Имя запроса' })).toHaveValue('Events');
  });
});
