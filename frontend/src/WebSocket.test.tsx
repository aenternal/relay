import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';
import { WebSocketTimeline } from './components/WebSocketTimeline';
import { createAppStore } from './lib/store';
import type { RelayStore } from './lib/store';
import { newRequest } from './lib/types';
import { fixtureState, fixtureWebSocketEvent, testBackend } from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  act(() => stores.forEach((store) => store.getState().dispose()));
  stores.length = 0;
});
function setup(nativeFiles = false) {
  const data = fixtureState();
  data.requests.push({
    ...newRequest('w1', 'c1', '', 'websocket'),
    id: 'ws1',
    name: 'Socket',
    websocket: { url: 'ws://localhost:8080/echo' },
  });
  data.ui.openRequestIds = ['ws1'];
  data.ui.activeRequestId = 'ws1';
  const backend = testBackend(data);
  backend.api.NativeFilesAvailable.mockResolvedValue(nativeFiles);
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { backend, store, user: userEvent.setup() };
}
describe('WebSocket UI', () => {
  it('connects using Ctrl Enter, sends interactive text without a new connection, displays pong and closes using Escape', async () => {
    const { backend, store, user } = setup();
    await screen.findByRole('button', { name: 'Connect' });
    fireEvent.keyDown(window, { key: 'Enter', ctrlKey: true });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send message' })).toBeEnabled());
    expect(backend.api.SendWebSocketMessage).not.toHaveBeenCalled();
    act(() => {
      const draft = store.getState().tabs.ws1.draft;
      store.getState().editRequest('ws1', {
        ...draft,
        websocket: { ...draft.websocket!, body: 'Привет <img src=x>' },
      });
    });
    fireEvent.keyDown(window, { key: 'Enter', ctrlKey: true });
    await waitFor(() =>
      expect(screen.getByLabelText('Сообщение WebSocket')).toHaveTextContent('Привет <img src=x>'),
    );
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
    expect(backend.api.ConnectWebSocket).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: 'Ping' }));
    await screen.findByRole('button', { name: /pong/ });
    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(store.getState().websocketRuns.ws1.execution?.state).toBe('closed'));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Connect' })).toBeEnabled());
    expect(backend.api.DisconnectWebSocket).toHaveBeenCalledWith('ws-ws1', 1000, '');
    expect(backend.api.CancelWebSocket).not.toHaveBeenCalled();
  });
  it('composes binary hex/base64 and chooses files through the native grant picker', async () => {
    const { backend, store, user } = setup(true);
    backend.api.ChooseHTTPFile.mockResolvedValue({
      id: 'native-grant',
      name: 'payload.bin',
      size: 2,
    });
    await user.click(await screen.findByRole('button', { name: 'Connect' }));
    await user.selectOptions(screen.getByLabelText('Тип сообщения WebSocket'), 'binary');
    await user.selectOptions(screen.getByLabelText('Кодировка WebSocket'), 'base64');
    act(() => {
      const draft = store.getState().tabs.ws1.draft;
      store
        .getState()
        .editRequest('ws1', { ...draft, websocket: { ...draft.websocket!, body: 'AP8=' } });
    });
    await user.click(screen.getByRole('button', { name: 'Send message' }));
    expect(backend.api.SendWebSocketMessage).toHaveBeenLastCalledWith('ws-ws1', {
      kind: 'binary',
      encoding: 'base64',
      data: 'AP8=',
      fileId: '',
    });
    await user.selectOptions(screen.getByLabelText('Кодировка WebSocket'), 'file');
    await user.click(screen.getByRole('button', { name: 'Файл сообщения WebSocket' }));
    await screen.findByText('payload.bin');
    await user.click(screen.getByRole('button', { name: 'Send message' }));
    expect(backend.api.ChooseHTTPFile).toHaveBeenCalledWith('body');
    expect(backend.api.SendWebSocketMessage).toHaveBeenLastCalledWith(
      'ws-ws1',
      expect.objectContaining({ encoding: 'file', fileId: 'native-grant' }),
    );
  });
  it('ignores a native file selection returned after the original editor has closed', async () => {
    const { backend, store, user } = setup(true);
    let release!: (value: { id: string; name: string; size: number }) => void;
    backend.api.ChooseHTTPFile.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    await screen.findByRole('button', { name: 'Connect' });
    await user.selectOptions(screen.getByLabelText('Тип сообщения WebSocket'), 'binary');
    await user.selectOptions(screen.getByLabelText('Кодировка WebSocket'), 'file');
    await user.click(screen.getByRole('button', { name: 'Файл сообщения WebSocket' }));
    await act(async () => {
      store.getState().requestClose('ws1');
      await store.getState().resolveClose('discard');
      store.getState().openRequest('ws1');
    });
    await act(async () => {
      release({ id: 'obsolete-file', name: 'old.bin', size: 2 });
    });
    expect(store.getState().tabs.ws1.draft.websocket?.fileId).toBe('');
    expect(store.getState().tabs.ws1.draft.websocket?.bodyKind).toBe('text');
  });

  it('retains all connection settings while editing the URL and shows handshake/TLS/close diagnostics', async () => {
    const { backend, store, user } = setup();
    const connect = backend.api.ConnectWebSocket.getMockImplementation()!;
    const poll = backend.api.PollWebSocket.getMockImplementation()!;
    const details = {
      subprotocol: 'chat',
      handshakeHeaders: [{ name: 'X-Peer', value: 'relay', enabled: true }],
      tls: {
        version: 'TLS 1.3',
        serverName: 'localhost',
        negotiatedProtocol: 'http/1.1',
        cipherSuite: 'test',
        verified: true,
        subject: 'local',
        issuer: 'root',
        dnsNames: [],
        expiresAt: '',
      },
    };
    backend.api.ConnectWebSocket.mockImplementation(async (request) => ({
      ...(await connect(request)),
      ...details,
    }));
    backend.api.PollWebSocket.mockImplementation(async (id, afterSequence) => {
      const batch = await poll(id, afterSequence);
      return { ...batch, execution: { ...batch.execution, ...details } };
    });
    await screen.findByRole('button', { name: 'Connect' });
    const url = screen.getByLabelText('WebSocket URL');
    act(() => {
      const draft = store.getState().tabs.ws1.draft;
      store.getState().editRequest('ws1', {
        ...draft,
        websocket: { ...draft.websocket!, subprotocols: ['chat'], readTimeoutMs: 1234 },
      });
    });
    fireEvent.change(url, { target: { value: 'wss://new.test/socket' } });
    expect(store.getState().tabs.ws1.draft.websocket).toMatchObject({
      subprotocols: ['chat'],
      readTimeoutMs: 1234,
    });
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const response = within(screen.getByLabelText('Результат WebSocket'));
    await user.click(response.getByRole('tab', { name: 'Handshake' }));
    expect(response.getByText('relay')).toBeVisible();
    await user.click(response.getByRole('tab', { name: 'Соединение' }));
    expect(response.getByText(/TLS 1.3/)).toBeVisible();
    fireEvent.change(screen.getByLabelText('WebSocket close code'), { target: { value: '4001' } });
    fireEvent.change(screen.getByLabelText('WebSocket close reason'), {
      target: { value: 'finished' },
    });
    await user.click(screen.getByRole('button', { name: 'Disconnect' }));
    expect(backend.api.DisconnectWebSocket).toHaveBeenCalledWith('ws-ws1', 4001, 'finished');
  });
  it('saves session metadata explicitly with payloads off by default and opens/deletes history', async () => {
    const { backend, store, user } = setup();
    await user.click(await screen.findByRole('button', { name: 'Connect' }));
    expect(screen.getByLabelText('Включить payloads в историю')).not.toBeChecked();
    expect(screen.getByRole('button', { name: 'Сохранить сессию' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Disconnect' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Сохранить сессию' })).toBeEnabled(),
    );
    await user.click(screen.getByRole('button', { name: 'Сохранить сессию' }));
    expect(backend.api.SaveWebSocketSession).toHaveBeenCalledWith('ws-ws1', false);
    await user.click(screen.getByText('Сохранённые сессии WebSocket (1)'));
    await user.click(screen.getByRole('button', { name: 'Открыть сессию saved-ws-ws1' }));
    await screen.findByLabelText('Сохранённая сессия WebSocket');
    expect(screen.getByText(/Payloads не сохранены/)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Удалить сессию saved-ws-ws1' }));
    await waitFor(() => expect(store.getState().websocketSessions).toHaveLength(0));
    expect(screen.queryByLabelText('Сохранённая сессия WebSocket')).not.toBeInTheDocument();
  });
  it('renders fifty events at a time, exposes losses, selects individual binary hex/base64 and preserves text safely', async () => {
    const user = userEvent.setup();
    render(
      <WebSocketTimeline
        events={Array.from({ length: 120 }, (_, index) =>
          fixtureWebSocketEvent(index + 1, {
            text: '<script>test()</script>',
            dataBase64: 'AP8=',
            messageType: 'binary',
          }),
        )}
        evicted={17}
        missed={4}
      />,
    );
    const list = within(screen.getByRole('list', { name: 'События WebSocket' }));
    expect(list.getAllByRole('listitem')).toHaveLength(50);
    expect(screen.getByText(/Пропущено: 4/)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Предыдущие 50' }));
    await user.click(list.getByRole('button', { name: '← #21 received binary 7 байт' }));
    expect(screen.getByLabelText('Сообщение WebSocket')).toHaveTextContent(
      '<script>test()</script>',
    );
    await user.selectOptions(screen.getByLabelText('Вид сообщения WebSocket'), 'hex');
    expect(screen.getByLabelText('Сообщение WebSocket')).toHaveTextContent('00 ff');
    await user.selectOptions(screen.getByLabelText('Вид сообщения WebSocket'), 'base64');
    expect(screen.getByLabelText('Сообщение WebSocket')).toHaveTextContent('AP8=');
    await user.click(screen.getByRole('button', { name: 'К последним' }));
    expect(list.getByRole('button', { name: '← #120 received binary 7 байт' })).toBeVisible();
  });
});
