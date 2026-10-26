import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';
import { createAppStore } from './lib/store';
import type { RelayStore } from './lib/store';
import { newRequest } from './lib/types';
import {
  fixtureGRPCExecution,
  fixtureProtoMethod,
  fixtureProtoSchema,
  fixtureState,
  testBackend,
} from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  act(() => {
    stores.forEach((store) => store.getState().dispose());
  });
  stores.length = 0;
});

function grpcBackend() {
  const state = fixtureState();
  state.protoSchemas = [fixtureProtoSchema()];
  const request = { ...newRequest('w1', 'c1', '', 'grpc'), id: 'g1', name: 'Unary call' };
  request.grpc = {
    ...request.grpc!,
    schemaId: 'schema1',
    method: '/example.catalog.v1.Catalog/Get',
    endpoint: 'localhost:50051',
    bodyFormat: 'json5',
    body: "{ itemId: '9223372036854775807', }",
  };
  state.requests.push(request);
  state.ui.openRequestIds = ['g1'];
  state.ui.activeRequestId = 'g1';
  return testBackend(state);
}
function mount(backend = grpcBackend()) {
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { store, backend, user: userEvent.setup() };
}

describe('gRPC unary UI', () => {
  it('calls the current JSON5 draft, shows exact canonical JSON, hex, repeated metadata and typed status details', async () => {
    const backend = grpcBackend();
    backend.api.StartGRPCUnary.mockResolvedValue(
      fixtureGRPCExecution('g1', {
        code: 3,
        codeName: 'InvalidArgument',
        statusMessage: '<img src=x onerror=unsafe()>',
        headers: [
          { name: 'x-repeat', value: 'one', enabled: true },
          { name: 'x-repeat', value: 'two', enabled: true },
          { name: 'payload-bin', value: 'AP9B', enabled: true },
        ],
        trailers: [{ name: 'x-trailer', value: 'done', enabled: true }],
        details: [
          {
            typeURL: 'type.googleapis.com/example.Detail',
            messageJSON: '{"id":"18446744073709551615"}',
            wireBase64: 'AP9B',
            error: '',
          },
          {
            typeURL: 'type.example.test/Unknown',
            messageJSON: '',
            wireBase64: 'AP9B',
            error: 'Unknown detail type; displaying Protobuf bytes',
          },
        ],
      }),
    );
    const { user } = mount(backend);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Call' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Call' }));
    expect(backend.api.StartGRPCUnary).toHaveBeenCalledWith(
      expect.objectContaining({
        grpc: expect.objectContaining({
          bodyFormat: 'json5',
          body: "{ itemId: '9223372036854775807', }",
        }),
      }),
    );
    expect(backend.api.ValidateProtoMessage).not.toHaveBeenCalled();
    expect(await screen.findByLabelText('Сообщение ответа gRPC')).toHaveTextContent(
      '9223372036854775807',
    );
    expect(screen.getByLabelText('Результат gRPC')).toHaveTextContent('3 InvalidArgument');
    await user.selectOptions(screen.getByLabelText('Вид ответа gRPC'), 'hex');
    expect(screen.getByLabelText('Сообщение ответа gRPC')).toHaveTextContent('08 ff ff');
    const response = within(screen.getByLabelText('Результат gRPC'));
    await user.click(response.getByRole('tab', { name: 'Headers (3)' }));
    expect(response.getAllByText('x-repeat')).toHaveLength(2);
    expect(response.getByText('AP9B')).toBeInTheDocument();
    expect(response.getByText(/Base64/)).toBeInTheDocument();
    await user.click(response.getByRole('tab', { name: 'Trailers (1)' }));
    expect(response.getByText('done')).toBeInTheDocument();
    await user.click(response.getByRole('tab', { name: 'Status / details (2)' }));
    expect(response.getByText('<img src=x onerror=unsafe()>')).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
    await user.click(response.getByText('type.googleapis.com/example.Detail'));
    expect(response.getByText('{"id":"18446744073709551615"}')).toBeVisible();
    await user.click(response.getByText('type.example.test/Unknown'));
    expect(response.getByText(/Unknown detail type/)).toBeVisible();
    expect(
      response.getAllByText(/00 ff 41/).some((element) => !element.closest('details:not([open])')),
    ).toBe(true);
  });

  it('supports Ctrl Enter and Escape while showing async cancellation until terminal state', async () => {
    const backend = grpcBackend();
    backend.api.StartGRPCUnary.mockResolvedValue(fixtureGRPCExecution('g1', { state: 'running' }));
    backend.api.GetGRPCExecution.mockResolvedValue(
      fixtureGRPCExecution('g1', { state: 'running' }),
    );
    const { store } = mount(backend);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Call' })).toBeEnabled());
    fireEvent.keyDown(window, { key: 'Enter', ctrlKey: true });
    await screen.findByRole('button', { name: 'Cancel' });
    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1'));
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
    backend.api.GetGRPCExecution.mockResolvedValue(
      fixtureGRPCExecution('g1', { state: 'canceled', code: 1, codeName: 'Canceled' }),
    );
    await waitFor(() => expect(store.getState().grpcRuns.g1.execution?.state).toBe('canceled'));
    expect(screen.getByRole('button', { name: 'Call' })).toBeEnabled();
    expect(within(screen.getByLabelText('Результат gRPC')).getByRole('status')).toHaveTextContent(
      'Отменён',
    );
  });

  it('edits connection, mTLS files, metadata, compression and limits without saving local file grants or literal tokens', async () => {
    const backend = grpcBackend();
    backend.api.NativeFilesAvailable.mockResolvedValue(true);
    const { user, store } = mount(backend);
    await user.click(await screen.findByRole('tab', { name: 'Соединение / TLS' }));
    expect(screen.getByLabelText('Защита соединения gRPC')).toHaveValue('tls');
    await user.click(screen.getByRole('button', { name: 'Выбрать CA для gRPC…' }));
    await user.type(screen.getByLabelText('gRPC TLS Server name'), 'api.test');
    expect(backend.api.ChooseHTTPFile).toHaveBeenCalledWith('ca');
    await user.selectOptions(screen.getByLabelText('Защита соединения gRPC'), 'plaintext');
    expect(screen.getByText(/без шифрования/)).toBeVisible();
    await user.click(screen.getByRole('tab', { name: 'Metadata (0)' }));
    await user.click(screen.getByRole('button', { name: '+ Metadata' }));
    await user.type(screen.getByLabelText('Имя gRPC metadata 1'), 'authorization');
    await user.type(screen.getByLabelText('Значение gRPC metadata 1'), 'Bearer memory-only-token');
    await user.click(screen.getByRole('tab', { name: 'Опции' }));
    await user.type(screen.getByLabelText('gRPC authority'), 'authority.test');
    await user.clear(screen.getByLabelText('gRPC deadline, мс'));
    await user.type(screen.getByLabelText('gRPC deadline, мс'), '1500');
    await user.selectOptions(screen.getByLabelText('Сжатие gRPC'), 'gzip');
    await act(async () => {
      await store.getState().saveRequest('g1');
    });
    const saved = backend.state().requests.find((request) => request.id === 'g1')!.grpc;
    expect(saved).toMatchObject({
      plaintext: true,
      authority: 'authority.test',
      compression: 'gzip',
      timeoutMs: 1500,
      tls: { caFileId: '', serverName: 'api.test' },
      metadata: [{ name: 'authorization', value: '', enabled: true }],
    });
    expect(store.getState().tabs.g1.draft.grpc?.metadata?.[0].value).toContain('memory-only-token');
  });

  it('keeps a local schema usable when reflection is disabled, then selects a successful discovery', async () => {
    const backend = grpcBackend();
    backend.api.StartGRPCReflection.mockResolvedValueOnce(
      fixtureGRPCExecution('g1', {
        kind: 'reflection',
        state: 'failed',
        error: 'Server reflection is not enabled',
        code: 12,
        codeName: 'Unimplemented',
      }),
    );
    const { user, store } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'Discover via Reflection' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('reflection is not enabled');
    expect(screen.getByText(/Выберите импортированную схему/)).toBeVisible();
    expect(screen.getByLabelText('Схема Protobuf')).toHaveValue('schema1');
    await user.click(screen.getByRole('button', { name: 'Call' }));
    expect(backend.api.StartGRPCUnary).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: 'Discover via Reflection' }));
    await waitFor(() =>
      expect(store.getState().tabs.g1.draft.grpc?.schemaId).toMatch(/^reflection/),
    );
    expect(screen.getByLabelText('Схема Protobuf')).not.toHaveValue('schema1');
    expect(screen.getByRole('button', { name: 'Call' })).toBeDisabled();
    expect(screen.getByLabelText('Результат gRPC')).toHaveTextContent('grpc.reflection.v1');
  });

  it('opens streaming methods and keeps offline Validate available', async () => {
    const backend = grpcBackend();
    const detail = fixtureProtoMethod();
    backend.api.InspectProtoMethod.mockResolvedValue({
      ...detail,
      method: { ...detail.method, serverStreaming: true },
    });
    const { user } = mount(backend);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Start stream' })).toBeEnabled());
    expect(screen.getByText(/отправляется один раз/)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Validate' }));
    expect(await screen.findByLabelText('Canonical ProtoJSON')).toBeVisible();
    expect(backend.api.StartGRPCUnary).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Start stream' }));
    expect(backend.api.StartGRPCStream).toHaveBeenCalledOnce();
  });

  it('disposes active operations when the document actually closes and preserves a cached page', async () => {
    const backend = grpcBackend();
    backend.api.StartGRPCUnary.mockResolvedValue(fixtureGRPCExecution('g1', { state: 'running' }));
    backend.api.GetGRPCExecution.mockResolvedValue(
      fixtureGRPCExecution('g1', { state: 'canceled' }),
    );
    const { user } = mount(backend);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Call' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Call' }));
    const cached = new Event('pagehide');
    Object.defineProperty(cached, 'persisted', { value: true });
    fireEvent(window, cached);
    expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
    fireEvent(window, new Event('pagehide'));
    await waitFor(() => expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1'));
    await waitFor(() => expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1'));
  });
});
