import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';
import { GRPCStreamTimeline } from './components/GRPCStreamTimeline';
import { createAppStore } from './lib/store';
import type { RelayStore } from './lib/store';
import { newRequest } from './lib/types';
import {
  fixtureGRPCExecution,
  fixtureGRPCStreamMessage,
  fixtureProtoMethod,
  fixtureProtoSchema,
  fixtureState,
  testBackend,
} from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  act(() => stores.forEach((store) => store.getState().dispose()));
  stores.length = 0;
});
function backendForStream() {
  const data = fixtureState();
  data.protoSchemas = [fixtureProtoSchema()];
  const request = { ...newRequest('w1', 'c1', '', 'grpc'), id: 'g1', name: 'Bidirectional' };
  request.grpc = {
    ...request.grpc!,
    endpoint: 'localhost:50051',
    schemaId: 'schema1',
    method: '/example.catalog.v1.Catalog/Watch',
  };
  data.requests.push(request);
  data.ui.openRequestIds = ['g1'];
  data.ui.activeRequestId = 'g1';
  const backend = testBackend(data);
  const detail = fixtureProtoMethod();
  backend.api.InspectProtoMethod.mockResolvedValue({
    ...detail,
    method: { ...detail.method, clientStreaming: true, serverStreaming: true },
  });
  return backend;
}
function mount(backend = backendForStream()) {
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { store, backend, user: userEvent.setup() };
}

describe('gRPC stream UI', () => {
  it('opens bidi without sending, sends a second message after the first response, and half-closes without Cancel', async () => {
    const { store, backend, user } = mount();
    await user.click(await screen.findByRole('button', { name: 'Start stream' }));
    await screen.findByRole('button', { name: 'Send message' });
    expect(backend.api.SendGRPCStreamMessage).not.toHaveBeenCalled();
    const update = async (text: string) =>
      act(() => {
        const request = store.getState().tabs.g1.draft;
        store.getState().editRequest('g1', {
          ...request,
          grpc: { ...request.grpc!, body: `{"text":"${text}"}` },
        });
      });
    await update('первое');
    await user.click(screen.getByRole('button', { name: 'Send message' }));
    await waitFor(() =>
      expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('первое'),
    );
    expect(screen.getByText('Отправлено: 1 · Получено: 1')).toBeVisible();
    await update('второе');
    fireEvent.keyDown(window, { key: 'Enter', ctrlKey: true });
    await waitFor(() =>
      expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('второе'),
    );
    expect(backend.api.StartGRPCStream).toHaveBeenCalledOnce();
    expect(screen.getByText('Отправлено: 2 · Получено: 2')).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Half-close' }));
    await waitFor(() => expect(store.getState().grpcRuns.g1.execution?.state).toBe('completed'));
    expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
    expect(screen.getByLabelText('История gRPC stream')).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Send message' })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Start stream' })).toBeEnabled());
  });

  it('keeps midstream responses inspectable beside the final error, trailers and safe JSON/hex', async () => {
    const backend = backendForStream();
    const terminal = fixtureGRPCExecution('g1', {
      kind: 'stream',
      rpcType: 'bidi',
      state: 'failed',
      streamState: 'closed',
      code: 7,
      codeName: 'PermissionDenied',
      receivedCount: 2,
      statusMessage: '<script>unsafe()</script>',
      trailers: [{ name: 'reason', value: 'policy', enabled: true }],
    });
    backend.api.PollGRPCStream.mockResolvedValueOnce({
      execution: terminal,
      messages: [
        fixtureGRPCStreamMessage(1, { messageJSON: '{"text":"<img src=x>"}' }),
        fixtureGRPCStreamMessage(2),
      ],
      nextSequence: 2,
    }).mockResolvedValue({ execution: terminal, messages: [], nextSequence: 2 });
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'Start stream' }));
    await screen.findByRole('button', { name: '← #1 · получено · 2 байт' });
    expect(screen.getByText('7 PermissionDenied')).toBeVisible();
    await user.click(screen.getByRole('button', { name: '← #1 · получено · 2 байт' }));
    expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('<img src=x>');
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText('Вид сообщения gRPC stream'), 'hex');
    expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('08 01');
    const response = within(screen.getByLabelText('Результат gRPC'));
    await user.click(response.getByRole('tab', { name: 'Trailers (1)' }));
    expect(response.getByText('policy')).toBeVisible();
    await user.click(response.getByRole('tab', { name: 'Status / details (0)' }));
    expect(response.getByText('<script>unsafe()</script>')).toBeVisible();
  });

  it('renders only a movable window of 50 messages, exposes evictions and keeps individual selection', async () => {
    const messages = Array.from({ length: 120 }, (_, index) => fixtureGRPCStreamMessage(index + 1));
    const user = userEvent.setup();
    render(<GRPCStreamTimeline messages={messages} evicted={17} />);
    const list = within(screen.getByRole('list', { name: 'Сообщения gRPC stream' }));
    expect(list.getAllByRole('listitem')).toHaveLength(50);
    expect(list.getByRole('button', { name: '← #120 · получено · 2 байт' })).toBeVisible();
    expect(screen.getByText('В памяти: 120 · Освобождено: 17')).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Предыдущие 50' }));
    expect(
      list.queryByRole('button', { name: '← #120 · получено · 2 байт' }),
    ).not.toBeInTheDocument();
    await user.click(list.getByRole('button', { name: '← #21 · получено · 2 байт' }));
    expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('"21"');
    await user.click(screen.getByRole('button', { name: 'К последним' }));
    expect(screen.getByLabelText('Сообщение gRPC stream')).toHaveTextContent('"120"');
    expect(list.getAllByRole('listitem')).toHaveLength(50);
  });

  it('bounds a large selected JSON preview and announces the display limit', () => {
    render(
      <GRPCStreamTimeline
        messages={[fixtureGRPCStreamMessage(1, { messageJSON: 'x'.repeat(1024 * 1024) })]}
      />,
    );
    expect(screen.getByLabelText('Сообщение gRPC stream').textContent).toHaveLength(256 * 1024);
    expect(screen.getByText(/показаны первые 256 КиБ/)).toBeVisible();
  });
});
