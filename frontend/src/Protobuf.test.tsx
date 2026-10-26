import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';
import { createAppStore } from './lib/store';
import type { RelayStore } from './lib/store';
import { emptyState, newRequest } from './lib/types';
import type { ProtoValidationResult } from './lib/types';
import { fixtureProtoSchema, fixtureState, testBackend } from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  stores.forEach((store) => store.getState().dispose());
  stores.length = 0;
});

function schemaBackend(withRequest = false) {
  const state = fixtureState();
  state.protoSchemas = [fixtureProtoSchema()];
  if (withRequest) {
    const request = { ...newRequest('w1', 'c1', '', 'grpc'), id: 'g1', name: 'Get item' };
    request.grpc = {
      ...request.grpc!,
      schemaId: 'schema1',
      method: '/example.catalog.v1.Catalog/Get',
      bodyFormat: 'json5',
      body: "{ itemId: '9223372036854775807', }",
    };
    state.requests.push(request);
    state.ui.openRequestIds = ['g1'];
    state.ui.activeRequestId = 'g1';
  }
  return testBackend(state);
}

function mount(backend = schemaBackend()) {
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { backend, store, user: userEvent.setup() };
}

describe('Protobuf workspace', () => {
  it('shows safe busy errors and retries failed schema reads without requiring a new import', async () => {
    const backend = schemaBackend(true);
    const busy = new Error('Application call failed', {
      cause: { message: 'Protobuf tools are busy; wait and try again' },
    });
    backend.api.InspectProtoSchema.mockRejectedValueOnce(busy);
    const { user } = mount(backend);
    expect(await screen.findByRole('alert')).toHaveTextContent('Protobuf tools are busy');
    await user.click(screen.getByRole('button', { name: 'Повторить чтение' }));
    await waitFor(() => expect(screen.getByLabelText('Метод RPC')).toBeEnabled());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(backend.api.InspectProtoSchema).toHaveBeenCalledTimes(2);
    backend.api.ValidateProtoMessage.mockRejectedValueOnce(busy);
    await user.click(screen.getByRole('button', { name: 'Validate' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Protobuf tools are busy');
    await user.click(screen.getByRole('button', { name: 'Validate' }));
    await screen.findByLabelText('Canonical ProtoJSON');
    backend.api.InspectProtoSchema.mockRejectedValueOnce(new Error('context deadline exceeded'));
    await user.click(screen.getByRole('button', { name: 'Protobuf' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('context deadline exceeded');
    await user.click(screen.getByRole('button', { name: 'Повторить чтение' }));
    await screen.findByLabelText('Дерево сервисов Protobuf');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(backend.api.InspectProtoSchema).toHaveBeenCalledTimes(4);
  });

  it('keeps failed native imports open with include paths and concrete diagnostics, then reports compatibility warnings', async () => {
    const backend = testBackend();
    backend.api.NativeFilesAvailable.mockResolvedValue(true);
    const original = backend.api.ImportNativeProtoSchema.getMockImplementation()!;
    backend.api.ImportNativeProtoSchema.mockResolvedValueOnce({
      state: emptyState(),
      schemaId: '',
      diagnostics: [
        {
          file: 'catalog.proto',
          path: 'types/missing.proto',
          line: 3,
          column: 8,
          message: 'Import not found: types/missing.proto',
        },
      ],
      warnings: [],
    });
    backend.api.ImportNativeProtoSchema.mockImplementationOnce(async (input) => ({
      ...(await original(input)),
      warnings: ['Method output changed: /example.catalog.v1.Catalog/Get'],
    }));
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'Protobuf' }));
    await user.click(screen.getByRole('button', { name: '+ Импорт схемы' }));
    await user.click(screen.getByRole('button', { name: 'Файлы .proto' }));
    await screen.findByText('catalog.proto', { selector: 'code' });
    await user.click(screen.getByRole('button', { name: '+ Include-директория' }));
    await user.click(screen.getByRole('button', { name: 'Импортировать' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'catalog.proto · 3:8 · types/missing.proto',
    );
    expect(backend.state().protoSchemas).toHaveLength(0);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Импортировать' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(backend.api.ImportNativeProtoSchema).toHaveBeenLastCalledWith(
      expect.objectContaining({
        selectionId: 'selection1',
        includeIds: ['include1'],
        workspaceId: 'w1',
        name: 'catalog',
      }),
    );
    expect(screen.getByLabelText('Диагностика Protobuf')).toHaveTextContent(
      'Method output changed',
    );
  });

  it('uploads multiple source files without parsing or rewriting their content in JavaScript', async () => {
    const { user, backend } = mount(testBackend());
    await user.click(await screen.findByRole('button', { name: 'Protobuf' }));
    await user.click(screen.getByRole('button', { name: '+ Импорт схемы' }));
    const source = 'syntax = "proto3"; import "types.proto"; // source kept verbatim\n';
    const first = new File([source], 'catalog.proto', { type: 'text/plain' });
    const second = new File(['syntax = "proto3";'], 'types.proto', { type: 'text/plain' });
    Object.defineProperty(first, 'text', { value: async () => source });
    Object.defineProperty(second, 'text', { value: async () => 'syntax = "proto3";' });
    await user.upload(screen.getByLabelText('Файлы Protobuf'), [first, second]);
    await user.click(screen.getByLabelText('Корневой файл types.proto'));
    await user.click(screen.getByRole('button', { name: 'Импортировать' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(backend.api.ImportProtoSchema).toHaveBeenCalledWith(
      expect.objectContaining({
        kind: 'proto',
        rootFiles: ['catalog.proto'],
        sources: [
          { name: 'catalog.proto', content: source },
          { name: 'types.proto', content: 'syntax = "proto3";' },
        ],
      }),
    );
  });

  it('preserves relative import paths when uploading a folder', async () => {
    const { user, backend } = mount(testBackend());
    await user.click(await screen.findByRole('button', { name: 'Protobuf' }));
    await user.click(screen.getByRole('button', { name: '+ Импорт схемы' }));
    const root = new File(['root'], 'catalog.proto');
    const nested = new File(['nested'], 'types.proto');
    Object.defineProperties(root, {
      webkitRelativePath: { value: 'api/catalog.proto' },
      text: { value: async () => 'root' },
    });
    Object.defineProperties(nested, {
      webkitRelativePath: { value: 'api/models/types.proto' },
      text: { value: async () => 'nested' },
    });
    await user.upload(screen.getByLabelText('Папка Protobuf'), [root, nested]);
    await screen.findByLabelText('Корневой файл models/types.proto');
    await user.click(screen.getByRole('button', { name: 'Импортировать' }));
    await waitFor(() => expect(backend.api.ImportProtoSchema).toHaveBeenCalled());
    expect(backend.api.ImportProtoSchema).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'api',
        sources: [
          { name: 'catalog.proto', content: 'root' },
          { name: 'models/types.proto', content: 'nested' },
        ],
      }),
    );
  });

  it('transfers descriptor-set bytes unchanged and enforces the per-source size bound', async () => {
    const { user, backend } = mount(testBackend());
    await user.click(await screen.findByRole('button', { name: 'Protobuf' }));
    await user.click(screen.getByRole('button', { name: '+ Импорт схемы' }));
    const oversized = new File(['x'], 'large.proto');
    Object.defineProperty(oversized, 'size', { value: 2 * 1024 * 1024 + 1 });
    await user.upload(screen.getByLabelText('Файлы Protobuf'), oversized);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Размер одного .proto ограничен 2 МиБ',
    );
    expect(screen.getByRole('button', { name: 'Импортировать' })).toBeDisabled();
    const file = new File([''], 'catalog.protoset');
    Object.defineProperty(file, 'arrayBuffer', {
      value: async () => new Uint8Array([0, 255, 128, 1]).buffer,
    });
    await user.upload(screen.getByLabelText('Descriptor set Protobuf'), file);
    await user.click(screen.getByRole('button', { name: 'Импортировать' }));
    await waitFor(() => expect(backend.api.ImportProtoSchema).toHaveBeenCalled());
    expect(backend.api.ImportProtoSchema).toHaveBeenCalledWith(
      expect.objectContaining({
        kind: 'descriptor',
        descriptorSetBase64: 'AP+AAQ==',
        sources: [],
        rootFiles: [],
      }),
    );
    expect(backend.api.SelectProtoImport).not.toHaveBeenCalled();
  });

  it('shows package/service/method trees, stream flags and documented fields, then creates a typed gRPC definition', async () => {
    const { user, backend } = mount();
    await user.click(await screen.findByRole('button', { name: 'Protobuf' }));
    const tree = await screen.findByLabelText('Дерево сервисов Protobuf');
    expect(tree).toHaveTextContent('example.catalog.v1');
    expect(tree).toHaveTextContent('Server streaming');
    await user.click(
      within(tree).getByRole('button', { name: 'Метод /example.catalog.v1.Catalog/Get' }),
    );
    expect(screen.getByText('Keep the exact identifier.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Создать gRPC запрос' }));
    await waitFor(() => expect(screen.getByLabelText('Схема Protobuf')).toHaveValue('schema1'));
    expect(screen.getByLabelText('Метод RPC')).toHaveValue('/example.catalog.v1.Catalog/Get');
    expect(
      backend.state().requests.find((request) => request.protocol === 'grpc')?.grpc,
    ).toMatchObject({
      schemaId: 'schema1',
      method: '/example.catalog.v1.Catalog/Get',
      persistBody: false,
    });
  });

  it('sends raw JSON5-like text to Go, shows exact int64 canonical JSON and binary hex, and never saves the body implicitly', async () => {
    const { user, backend } = mount(schemaBackend(true));
    const validate = await screen.findByRole('button', { name: 'Validate' });
    await waitFor(() => expect(validate).toBeEnabled());
    await user.click(validate);
    expect(await screen.findByLabelText('Canonical ProtoJSON')).toHaveTextContent(
      '9223372036854775807',
    );
    expect(backend.api.ValidateProtoMessage).toHaveBeenCalledWith(
      expect.objectContaining({
        body: "{ itemId: '9223372036854775807', }",
        format: 'json5',
        schemaId: 'schema1',
      }),
    );
    await user.click(screen.getByRole('tab', { name: 'Protobuf hex' }));
    expect(screen.getByLabelText('Protobuf hex')).toHaveTextContent('00000000 08 ff');
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    expect(screen.getByText(/без подстановки переменных окружения/)).toBeInTheDocument();
  });

  it('renders typed diagnostics and excludes invalid canonical previews', async () => {
    const backend = schemaBackend(true);
    backend.api.ValidateProtoMessage.mockResolvedValueOnce({
      valid: false,
      canonicalJSON: '',
      wireBase64: '',
      wireBytes: 0,
      diagnostics: [
        { file: '', path: '$.choice', line: 2, column: 5, message: 'oneof conflict: choice' },
      ],
      warnings: [],
    });
    const { user } = mount(backend);
    const validate = await screen.findByRole('button', { name: 'Validate' });
    await waitFor(() => expect(validate).toBeEnabled());
    await user.click(validate);
    expect(await screen.findByRole('alert')).toHaveTextContent('2:5 · $.choice');
    expect(screen.queryByLabelText('Canonical ProtoJSON')).not.toBeInTheDocument();
  });

  it('discards validation results when the edited input changes while Go is validating', async () => {
    const backend = schemaBackend(true);
    let resolve!: (value: ProtoValidationResult) => void;
    backend.api.ValidateProtoMessage.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }),
    );
    const { user, store } = mount(backend);
    const validate = await screen.findByRole('button', { name: 'Validate' });
    await waitFor(() => expect(validate).toBeEnabled());
    await user.click(validate);
    act(() => {
      const request = store.getState().tabs.g1.draft;
      store.getState().editRequest('g1', { ...request, grpc: { ...request.grpc!, body: '{}' } });
    });
    await act(async () =>
      resolve({
        valid: true,
        canonicalJSON: '{"stale":true}',
        wireBase64: '',
        wireBytes: 0,
        diagnostics: [],
        warnings: [],
      }),
    );
    expect(screen.queryByLabelText('Canonical ProtoJSON')).not.toBeInTheDocument();
    await user.click(validate);
    await screen.findByLabelText('Canonical ProtoJSON');
    act(() => {
      const request = store.getState().tabs.g1.draft;
      store
        .getState()
        .editRequest('g1', { ...request, grpc: { ...request.grpc!, body: '{"itemId":"1"}' } });
    });
    expect(screen.getByText(/Нажмите Validate, чтобы обновить превью/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Canonical ProtoJSON')).not.toBeInTheDocument();
  });

  it('formats through canonical Go output and generates a descriptor-based example explicitly', async () => {
    const { user, store, backend } = mount(schemaBackend(true));
    const format = await screen.findByRole('button', { name: 'Format' });
    await waitFor(() => expect(format).toBeEnabled());
    await user.click(format);
    await waitFor(() => expect(store.getState().tabs.g1.draft.grpc?.bodyFormat).toBe('json'));
    expect(store.getState().tabs.g1.draft.grpc?.body).toContain('"9223372036854775807"');
    expect(screen.getByLabelText('Canonical ProtoJSON')).toHaveTextContent('9223372036854775807');
    await user.click(screen.getByRole('button', { name: 'Generate Example' }));
    expect(store.getState().tabs.g1.draft.grpc?.body).toBe('{\n  "itemId": "0"\n}');
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
  });

  it('clears a deleted schema from open drafts and queued autosaves while preserving other edits', async () => {
    const backend = schemaBackend(true);
    const store = createAppStore(backend.api, 60000);
    stores.push(store);
    await store.getState().initialize();
    const draft = store.getState().tabs.g1.draft;
    store.getState().editRequest('g1', {
      ...draft,
      name: 'My unsaved request',
      grpc: { ...draft.grpc!, body: '{ itemId: "42" }' },
    });
    let unblock!: () => void;
    const gate = new Promise<void>((resolve) => {
      unblock = resolve;
    });
    const blocked = store.getState().mutate(async () => {
      await gate;
      return backend.state();
    });
    const deletion = store.getState().mutate((api) => api.DeleteProtoSchema('w1', 'schema1'));
    const save = store.getState().saveRequest('g1');
    unblock();
    await Promise.all([blocked, deletion, save]);
    expect(store.getState().tabs.g1.draft).toMatchObject({
      name: 'My unsaved request',
      grpc: { schemaId: '', method: '/example.catalog.v1.Catalog/Get', body: '{ itemId: "42" }' },
    });
    expect(backend.api.SaveRequest).toHaveBeenLastCalledWith(
      expect.objectContaining({
        name: 'My unsaved request',
        grpc: expect.objectContaining({
          schemaId: '',
          method: '/example.catalog.v1.Catalog/Get',
          body: '',
        }),
      }),
    );
    expect(store.getState().tabs.g1.saved.grpc?.schemaId).toBe('');
    expect(store.getState().error).toBe('');
  });
});
