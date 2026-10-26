import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAppStore, hasVolatileData, persistentRequest } from './store';
import { newRequest } from './types';
import type { WebSocketBatch, WebSocketExecution, WebSocketSavedSession } from './types';
import {
  fixtureState,
  fixtureWebSocketEvent,
  fixtureWebSocketExecution,
  testBackend,
} from '../test/backend';

function setup(count = 1) {
  const data = fixtureState();
  for (let index = 1; index <= count; index++)
    data.requests.push({
      ...newRequest('w1', 'c1', '', 'websocket'),
      id: `ws${index}`,
      name: `Socket ${index}`,
      websocket: { url: `ws://localhost:8080/socket${index}` },
    });
  const backend = testBackend(data),
    store = createAppStore(backend.api, 60000);
  return { backend, store };
}
afterEach(() => vi.useRealTimers());
describe('WebSocket lifecycle', () => {
  it('normalizes legacy definitions and connects without sending; later messages use the current composer and original connection', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    expect(store.getState().tabs.ws1.draft.websocket?.headers).toEqual([]);
    await store.getState().webSocketPrimary('ws1');
    expect(backend.api.SendWebSocketMessage).not.toHaveBeenCalled();
    const draft = store.getState().tabs.ws1.draft;
    store.getState().editRequest('ws1', {
      ...draft,
      websocket: { ...draft.websocket!, url: 'ws://edited', body: 'Привет 🌍' },
    });
    await store.getState().webSocketPrimary('ws1');
    await vi.advanceTimersByTimeAsync(150);
    expect(backend.api.ConnectWebSocket).toHaveBeenCalledOnce();
    expect(backend.api.SendWebSocketMessage).toHaveBeenCalledWith('ws-ws1', {
      kind: 'text',
      encoding: 'text',
      data: 'Привет 🌍',
      fileId: '',
    });
    expect(store.getState().websocketRuns.ws1.events).toHaveLength(2);
    expect(store.getState().websocketRuns.ws1.execution?.url).toContain('socket1');
    expect(backend.api.AckWebSocket).toHaveBeenCalledWith('ws-ws1', 2);
    store.getState().dispose();
  });
  it.each(['hex', 'base64', 'file'])(
    'sends binary %s through Go with file grants untouched',
    async (encoding) => {
      vi.useFakeTimers();
      const { backend, store } = setup();
      await store.getState().initialize();
      store.getState().openRequest('ws1');
      await store.getState().connectWebSocket('ws1');
      const draft = store.getState().tabs.ws1.draft;
      store.getState().editRequest('ws1', {
        ...draft,
        websocket: {
          ...draft.websocket!,
          bodyKind: 'binary',
          bodyEncoding: encoding,
          body: '00 ff',
          fileId: encoding === 'file' ? 'opaque-grant' : '',
        },
      });
      await store.getState().sendWebSocketMessage('ws1');
      expect(backend.api.SendWebSocketMessage).toHaveBeenCalledWith('ws-ws1', {
        kind: 'binary',
        encoding,
        data: '00 ff',
        fileId: encoding === 'file' ? 'opaque-grant' : '',
      });
      store.getState().dispose();
    },
  );
  it('commits bounded events before ACK, retries lost ACK, deduplicates replay and counts sequence gaps', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    const terminal = fixtureWebSocketExecution('ws1', {
      state: 'closed',
      closeCode: 1000,
      droppedEvents: 3,
    });
    const batch: WebSocketBatch = {
      execution: terminal,
      events: [fixtureWebSocketEvent(4), fixtureWebSocketEvent(5), fixtureWebSocketEvent(5)],
      nextSequence: 5,
    };
    backend.api.PollWebSocket.mockResolvedValueOnce(batch)
      .mockResolvedValueOnce(batch)
      .mockResolvedValue({ execution: terminal, events: [], nextSequence: 5 });
    backend.api.AckWebSocket.mockImplementationOnce(async () => {
      expect(store.getState().websocketRuns.ws1.events).toHaveLength(2);
      expect(store.getState().websocketRuns.ws1.missedEvents).toBe(3);
      throw new Error('lost ACK');
    });
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    await vi.advanceTimersByTimeAsync(450);
    expect(store.getState().websocketRuns.ws1.events).toHaveLength(2);
    expect(store.getState().websocketRuns.ws1.draining).toBe(false);
    expect(backend.api.AckWebSocket).toHaveBeenCalledTimes(2);
    expect(backend.api.PollWebSocket).toHaveBeenLastCalledWith('ws-ws1', 5);
    store.getState().dispose();
  });
  it('serializes slow polling and evicts accepted previews before acknowledging', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    let release!: (value: WebSocketBatch) => void;
    backend.api.PollWebSocket.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    backend.api.AckWebSocket.mockImplementation(async () => {
      expect(store.getState().websocketRuns.ws1.events).toHaveLength(3);
      expect(store.getState().websocketRuns.ws1.evictedEvents).toBe(97);
    });
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store
      .getState()
      .mutate((api) => api.SaveSettings({ ...store.getState().data.settings, historyLimit: 3 }));
    await store.getState().connectWebSocket('ws1');
    await vi.advanceTimersByTimeAsync(1500);
    expect(backend.api.PollWebSocket).toHaveBeenCalledOnce();
    release({
      execution: fixtureWebSocketExecution('ws1'),
      events: Array.from({ length: 100 }, (_, index) => fixtureWebSocketEvent(index + 1)),
      nextSequence: 100,
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(backend.api.AckWebSocket).toHaveBeenCalledWith('ws-ws1', 100);
    store.getState().dispose();
  });
  it('keeps active connections while enforcing a shared preview byte budget', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup(2);
    backend.api.PollWebSocket.mockImplementation(async (id, after) => ({
      execution: fixtureWebSocketExecution(id.replace('ws-', '')),
      events: after ? [] : [fixtureWebSocketEvent(1, { text: 'x'.repeat(9 * 1024 * 1024) })],
      nextSequence: 1,
    }));
    await store.getState().initialize();
    for (const id of ['ws1', 'ws2']) {
      store.getState().openRequest(id);
      await store.getState().connectWebSocket(id);
    }
    await vi.advanceTimersByTimeAsync(150);
    expect(
      Object.values(store.getState().websocketRuns).every((run) => run.execution?.state === 'open'),
    ).toBe(true);
    expect(
      Object.values(store.getState().websocketRuns).reduce(
        (sum, run) => sum + run.events.length,
        0,
      ),
    ).toBe(1);
    expect(
      Object.values(store.getState().websocketRuns).reduce(
        (sum, run) => sum + run.evictedEvents,
        0,
      ),
    ).toBe(1);
    expect(backend.api.CancelWebSocket).not.toHaveBeenCalled();
    store.getState().dispose();
  });
  it('evicts the oldest completed session after sixteen terminal results', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup(17);
    backend.api.ConnectWebSocket.mockImplementation(async (request) =>
      fixtureWebSocketExecution(request.id, { state: 'closed' }),
    );
    backend.api.PollWebSocket.mockImplementation(async (id) => ({
      execution: fixtureWebSocketExecution(id.replace('ws-', ''), { state: 'closed' }),
      events: [],
      nextSequence: 0,
    }));
    await store.getState().initialize();
    for (let index = 1; index <= 17; index++) {
      const id = `ws${index}`;
      store.getState().openRequest(id);
      await store.getState().connectWebSocket(id);
      await vi.advanceTimersByTimeAsync(150);
    }
    expect(store.getState().websocketRuns.ws1.execution).toBeNull();
    expect(store.getState().websocketRuns.ws1.notice).toContain('16');
    expect(backend.api.ForgetWebSocket).toHaveBeenCalledWith('ws-ws1');
    store.getState().dispose();
  });
  it('closes with a code/reason, drains terminal events, and refuses new sends', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    await store.getState().disconnectWebSocket('ws1', 4001, 'done');
    await store.getState().sendWebSocketMessage('ws1');
    expect(backend.api.SendWebSocketMessage).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(300);
    expect(store.getState().websocketRuns.ws1.execution).toMatchObject({
      state: 'closed',
      closeCode: 4001,
      closeReason: 'done',
      cleanClose: true,
    });
    expect(store.getState().websocketRuns.ws1.events.at(-1)?.type).toBe('close');
    expect(store.getState().websocketRuns.ws1.draining).toBe(false);
    store.getState().dispose();
  });
  it('throttles manual Ping and continues to display pong events', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    await store.getState().pingWebSocket('ws1');
    await store.getState().pingWebSocket('ws1');
    expect(backend.api.PingWebSocket).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1000);
    await store.getState().pingWebSocket('ws1');
    expect(backend.api.PingWebSocket).toHaveBeenCalledTimes(2);
    expect(store.getState().websocketRuns.ws1.events.some((event) => event.type === 'pong')).toBe(
      true,
    );
    store.getState().dispose();
  });
  it.each(['close', 'delete', 'workspace', 'dispose', 'late-start'])(
    'aborts and forgets on %s without resurrecting stale results',
    async (action) => {
      vi.useFakeTimers();
      const { backend, store } = setup();
      let release!: (value: WebSocketExecution) => void;
      if (action === 'late-start')
        backend.api.ConnectWebSocket.mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              release = resolve;
            }),
        );
      await store.getState().initialize();
      store.getState().openRequest('ws1');
      const starting = store.getState().connectWebSocket('ws1');
      await vi.advanceTimersByTimeAsync(0);
      if (action !== 'late-start') await starting;
      if (action === 'close' || action === 'late-start') store.getState().requestClose('ws1');
      else if (action === 'delete')
        await store.getState().mutate((api) => api.DeleteRequest('ws1'));
      else if (action === 'workspace') store.getState().switchWorkspace('w2');
      else store.getState().dispose();
      if (action === 'late-start') release(fixtureWebSocketExecution('ws1'));
      await starting;
      await vi.advanceTimersByTimeAsync(0);
      expect(backend.api.CancelWebSocket).toHaveBeenCalledWith('ws-ws1');
      expect(backend.api.ForgetWebSocket).toHaveBeenCalledWith('ws-ws1');
      expect(store.getState().websocketRuns.ws1).toBeUndefined();
      store.getState().dispose();
    },
  );
  it('ignores a send failure arriving after the original tab was closed and reopened', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    let reject!: (error: Error) => void;
    backend.api.SendWebSocketMessage.mockImplementationOnce(
      () =>
        new Promise((_, fail) => {
          reject = fail;
        }),
    );
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    const sending = store.getState().sendWebSocketMessage('ws1');
    store.getState().requestClose('ws1');
    store.getState().openRequest('ws1');
    reject(new Error('obsolete'));
    await sending;
    expect(store.getState().websocketRuns.ws1).toBeUndefined();
    store.getState().dispose();
  });
  it('aborts after repeated bridge failure and preserves already accepted history', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    backend.api.PollWebSocket.mockResolvedValue({
      execution: fixtureWebSocketExecution('ws1'),
      events: [fixtureWebSocketEvent()],
      nextSequence: 1,
    });
    backend.api.AckWebSocket.mockRejectedValue(new Error('unavailable'));
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    await vi.advanceTimersByTimeAsync(450);
    expect(store.getState().websocketRuns.ws1.events).toHaveLength(1);
    expect(store.getState().websocketRuns.ws1.error).toContain('трёх попыток');
    expect(backend.api.CancelWebSocket).toHaveBeenCalledWith('ws-ws1');
    expect(backend.api.ForgetWebSocket).toHaveBeenCalledWith('ws-ws1');
    store.getState().dispose();
  });
});

describe('WebSocket privacy and saved history', () => {
  it('strips transient bodies, grants, sensitive headers and query values while retaining draft and references', () => {
    const request = { ...newRequest('w1', 'c1', '', 'websocket'), id: 'ws1' };
    request.websocket = {
      url: 'ws://local?token=literal&key=another&code={{code}}&safe=ok',
      body: 'private',
      fileId: 'file',
      tls: {
        caFileId: 'ca',
        clientCertFileId: 'cert',
        clientKeyFileId: 'key',
        serverName: '',
        insecure: false,
      },
      headers: [
        { name: 'Authorization', value: 'Bearer literal', enabled: true },
        { name: 'Cookie', value: '{{cookie}}', enabled: true },
      ],
      query: [{ name: 'auth', value: 'literal', enabled: false }],
    };
    const saved = persistentRequest(request);
    expect(saved.websocket).toMatchObject({
      body: '',
      fileId: '',
      url: 'ws://local?token=&key=&code={{code}}&safe=ok',
      headers: [{ value: '' }, { value: '{{cookie}}' }],
      query: [{ value: '' }],
      tls: { caFileId: '', clientCertFileId: '', clientKeyFileId: '' },
    });
    expect(request.websocket.body).toBe('private');
    expect(request.websocket.headers![0].value).toContain('literal');
    expect(hasVolatileData({ draft: request, saved: request, version: 0, saving: false })).toBe(
      true,
    );
  });
  it('saves only terminal sessions explicitly, defaults payload exclusion, then opens and deletes durable history', async () => {
    vi.useFakeTimers();
    const { backend, store } = setup();
    await store.getState().initialize();
    store.getState().openRequest('ws1');
    await store.getState().connectWebSocket('ws1');
    expect(await store.getState().saveWebSocketSession('ws1', false)).toBe(false);
    expect(backend.api.SaveWebSocketSession).not.toHaveBeenCalled();
    await store.getState().sendWebSocketMessage('ws1');
    await store.getState().disconnectWebSocket('ws1');
    await vi.advanceTimersByTimeAsync(300);
    expect(await store.getState().saveWebSocketSession('ws1', false)).toBe(true);
    expect(backend.api.SaveWebSocketSession).toHaveBeenCalledWith('ws-ws1', false);
    expect(store.getState().websocketSessions).toHaveLength(1);
    await store.getState().openSavedWebSocketSession('w1', 'saved-ws-ws1');
    expect(
      store
        .getState()
        .savedWebSocketSession?.events.every((event) => !event.text && !event.dataBase64),
    ).toBe(true);
    await store.getState().deleteSavedWebSocketSession('w1', 'saved-ws-ws1');
    expect(store.getState().savedWebSocketSession).toBeNull();
    expect(store.getState().websocketSessions).toHaveLength(0);
    store.getState().dispose();
  });
  it('loads saved history without a live connection after UI restart and drops stale cross-workspace loads', async () => {
    const { backend, store } = setup();
    const session: WebSocketSavedSession = {
      id: 'old',
      workspaceId: 'w1',
      requestId: 'ws1',
      savedAt: '',
      includePayloads: true,
      omittedEvents: 0,
      execution: fixtureWebSocketExecution('ws1', { state: 'closed' }),
      events: [fixtureWebSocketEvent()],
    };
    backend.api.GetSavedWebSocketSession.mockResolvedValueOnce(session);
    await store.getState().initialize();
    await store.getState().openSavedWebSocketSession('w1', 'old');
    expect(store.getState().savedWebSocketSession?.events).toHaveLength(1);
    expect(backend.api.ConnectWebSocket).not.toHaveBeenCalled();
    let release!: (value: WebSocketSavedSession) => void;
    backend.api.GetSavedWebSocketSession.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    const opening = store.getState().openSavedWebSocketSession('w1', 'old');
    store.getState().switchWorkspace('w2');
    release(session);
    await opening;
    expect(store.getState().savedWebSocketSession).toBeNull();
    store.getState().dispose();
  });
});
