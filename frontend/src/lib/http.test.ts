import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAppStore, persistentRequest } from './store';
import { fixtureExecution, fixtureState, testBackend } from '../test/backend';
import { newRequest, normalizeHTTP } from './types';
import type { HTTPExecution, State } from './types';
import { prettyJSON, responseHex } from './response';

afterEach(() => vi.useRealTimers());

describe('HTTP execution lifecycle', () => {
  it('sends a draft snapshot through Go without saving its body or credentials', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const request = store.getState().tabs.r1.draft;
    store.getState().editRequest('r1', {
      ...request,
      http: {
        ...request.http!,
        url: 'https://example.test',
        body: 'private body',
        auth: { ...request.http!.auth, type: 'bearer', token: 'private token' },
        form: [{ name: 'password', value: 'private form', enabled: true }],
      },
    });
    await store.getState().sendHTTPRequest('r1');
    expect(backend.api.StartHTTPRequest).toHaveBeenCalledWith(
      expect.objectContaining({
        http: expect.objectContaining({
          body: 'private body',
          auth: expect.objectContaining({ token: 'private token' }),
        }),
      }),
    );
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    await store.getState().saveRequest('r1');
    expect(backend.api.SaveRequest).toHaveBeenCalledWith(
      expect.objectContaining({
        http: expect.objectContaining({
          body: '',
          form: [expect.objectContaining({ value: '' })],
          auth: expect.objectContaining({ token: '' }),
        }),
      }),
    );
    expect(store.getState().tabs.r1.draft.http?.body).toBe('private body');
    store.getState().dispose();
  });

  it('polls only running executions, stops after completion, and isolates tabs', async () => {
    vi.useFakeTimers();
    const data = fixtureState();
    data.requests.push({ ...newRequest('w1', 'c1'), id: 'r2', name: 'Second' });
    const backend = testBackend(data);
    backend.api.StartHTTPRequest.mockImplementation(async (request) =>
      fixtureExecution(request.id, { state: 'running' }),
    );
    backend.api.GetHTTPExecution.mockImplementation(async (id) =>
      fixtureExecution(id === 'execution-r1' ? 'r1' : 'r2'),
    );
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    store.getState().openRequest('r2');
    await Promise.all([
      store.getState().sendHTTPRequest('r1'),
      store.getState().sendHTTPRequest('r2'),
    ]);
    expect(store.getState().runs.r1.execution?.state).toBe('running');
    await vi.advanceTimersByTimeAsync(150);
    expect(store.getState().runs.r1.execution?.requestId).toBe('r1');
    expect(store.getState().runs.r2.execution?.requestId).toBe('r2');
    expect(store.getState().runs.r2.execution?.state).toBe('completed');
    const calls = backend.api.GetHTTPExecution.mock.calls.length;
    await vi.advanceTimersByTimeAsync(1000);
    expect(backend.api.GetHTTPExecution).toHaveBeenCalledTimes(calls);
    store.getState().dispose();
  });

  it('continues polling when cancellation completes asynchronously', async () => {
    vi.useFakeTimers();
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(fixtureExecution('r1', { state: 'running' }));
    backend.api.GetHTTPExecution.mockResolvedValueOnce(
      fixtureExecution('r1', { state: 'running' }),
    ).mockResolvedValue(fixtureExecution('r1', { state: 'canceled' }));
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    await store.getState().sendHTTPRequest('r1');
    await store.getState().cancelHTTPRequest('r1');
    expect(store.getState().runs.r1.execution?.state).toBe('running');
    await vi.advanceTimersByTimeAsync(150);
    expect(store.getState().runs.r1.execution?.state).toBe('canceled');
    const calls = backend.api.GetHTTPExecution.mock.calls.length;
    await vi.advanceTimersByTimeAsync(1000);
    expect(backend.api.GetHTTPExecution).toHaveBeenCalledTimes(calls);
    store.getState().dispose();
  });

  it('cancels and forgets a run when its tab is closed', async () => {
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(fixtureExecution('r1', { state: 'running' }));
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    await store.getState().sendHTTPRequest('r1');
    store.getState().requestClose('r1');
    await vi.waitFor(() =>
      expect(backend.api.ForgetHTTPExecution).toHaveBeenCalledWith('execution-r1'),
    );
    expect(backend.api.CancelHTTPExecution).toHaveBeenCalledWith('execution-r1');
    expect(store.getState().runs.r1).toBeUndefined();
    store.getState().dispose();
  });

  it.each(['close', 'delete', 'dispose', 'late-start', 'rerun'] as const)(
    'waits for worker completion before forgetting a %s execution',
    async (action) => {
      vi.useFakeTimers();
      const backend = testBackend();
      let terminal = false,
        queries = 0;
      let release!: (execution: HTTPExecution) => void;
      if (action === 'late-start')
        backend.api.StartHTTPRequest.mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              release = resolve;
            }),
        );
      else
        backend.api.StartHTTPRequest.mockResolvedValueOnce(
          fixtureExecution('r1', { state: 'running' }),
        );
      backend.api.StartHTTPRequest.mockResolvedValueOnce(
        fixtureExecution('r1', { id: 'next-execution' }),
      );
      backend.api.GetHTTPExecution.mockImplementation(async () => {
        terminal = ++queries >= 2;
        return fixtureExecution('r1', { state: terminal ? 'canceled' : 'running' });
      });
      backend.api.ForgetHTTPExecution.mockImplementation(async () => {
        if (!terminal) throw new Error('worker is still running');
      });
      const store = createAppStore(backend.api);
      await store.getState().initialize();
      store.getState().openRequest('r1');
      const sending = store.getState().sendHTTPRequest('r1');
      if (action !== 'late-start') await sending;
      if (action === 'close' || action === 'late-start') store.getState().requestClose('r1');
      else if (action === 'delete') await store.getState().mutate((api) => api.DeleteRequest('r1'));
      else if (action === 'dispose') store.getState().dispose();
      else await store.getState().sendHTTPRequest('r1');
      if (action === 'late-start') release(fixtureExecution('r1', { state: 'running' }));
      await vi.advanceTimersByTimeAsync(0);
      expect(backend.api.CancelHTTPExecution).toHaveBeenCalledWith('execution-r1');
      expect(backend.api.ForgetHTTPExecution).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(150);
      await sending;
      expect(queries).toBe(2);
      expect(backend.api.ForgetHTTPExecution).toHaveBeenCalledWith('execution-r1');
      store.getState().dispose();
    },
  );

  it('drops an autosave queued behind deletion so it cannot recreate a request', async () => {
    vi.useFakeTimers();
    const backend = testBackend();
    const originalDelete = backend.api.DeleteRequest.getMockImplementation()!;
    let release!: () => void;
    backend.api.DeleteRequest.mockImplementation(async (id) => {
      const data = await originalDelete(id);
      return new Promise<State>((resolve) => {
        release = () => resolve(data);
      });
    });
    const store = createAppStore(backend.api, 100);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    store
      .getState()
      .editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Pending autosave' });
    const deleting = store.getState().mutate((api) => api.DeleteRequest('r1'));
    await vi.advanceTimersByTimeAsync(100);
    release();
    await deleting;
    await vi.advanceTimersByTimeAsync(0);
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    expect(backend.state().requests).toHaveLength(0);
    store.getState().dispose();
  });

  it('releases a late start result after the request was deleted', async () => {
    const backend = testBackend();
    let release!: (value: HTTPExecution) => void;
    backend.api.StartHTTPRequest.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const sending = store.getState().sendHTTPRequest('r1');
    await store.getState().mutate((api) => api.DeleteRequest('r1'));
    release(fixtureExecution('r1', { state: 'running' }));
    await sending;
    expect(backend.api.CancelHTTPExecution).toHaveBeenCalledWith('execution-r1');
    expect(backend.api.ForgetHTTPExecution).toHaveBeenCalledWith('execution-r1');
    expect(store.getState().runs.r1).toBeUndefined();
    store.getState().dispose();
  });

  it('cancels a start in flight and stops polling on disposal', async () => {
    vi.useFakeTimers();
    const backend = testBackend();
    let release!: (value: HTTPExecution) => void;
    backend.api.StartHTTPRequest.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const sending = store.getState().sendHTTPRequest('r1');
    await store.getState().cancelHTTPRequest('r1');
    release(fixtureExecution('r1', { state: 'running' }));
    await sending;
    expect(backend.api.CancelHTTPExecution).toHaveBeenCalledWith('execution-r1');
    expect(store.getState().runs.r1.error).toContain('отменён');
    const cleanupCalls = backend.api.GetHTTPExecution.mock.calls.length;
    store.getState().dispose();
    await vi.advanceTimersByTimeAsync(1000);
    expect(backend.api.GetHTTPExecution).toHaveBeenCalledTimes(cleanupCalls);
  });

  it.each(['password', 'token', 'keyValue'] as const)(
    'confirms closing after autosave strips a transient auth %s',
    async (field) => {
      const backend = testBackend();
      const store = createAppStore(backend.api, 60000);
      await store.getState().initialize();
      store.getState().openRequest('r1');
      const request = store.getState().tabs.r1.draft;
      store.getState().editRequest('r1', {
        ...request,
        http: {
          ...request.http!,
          auth: { ...request.http!.auth, [field]: 'literal-credential' },
        },
      });
      await store.getState().saveRequest('r1');
      expect(backend.state().requests[0].http?.auth[field]).toBe('');
      store.getState().requestClose('r1');
      expect(store.getState().closingId).toBe('r1');
      await store.getState().resolveClose('cancel');
      expect(store.getState().tabs.r1.draft.http?.auth[field]).toBe('literal-credential');
      store.getState().requestClose('r1');
      await store.getState().resolveClose('discard');
      expect(store.getState().openIds).toEqual([]);
      store.getState().dispose();
    },
  );

  it('confirms losing file selections even after their definition was autosaved', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const request = store.getState().tabs.r1.draft;
    store.getState().editRequest('r1', {
      ...request,
      http: {
        ...request.http!,
        fileId: 'ephemeral-body',
        tls: { ...request.http!.tls, caFileId: 'ephemeral-ca' },
      },
    });
    await store.getState().saveRequest('r1');
    expect(backend.state().requests[0].http?.fileId).toBe('');
    store.getState().requestClose('r1');
    expect(store.getState().closingId).toBe('r1');
    await store.getState().resolveClose('cancel');
    expect(store.getState().tabs.r1.draft.http?.fileId).toBe('ephemeral-body');
    store.getState().dispose();
  });

  it('keeps variable references, removes ephemeral file grants and respects form opt-in', () => {
    const request = newRequest('w1', 'c1');
    const http = request.http!;
    const saved = persistentRequest({
      ...request,
      http: {
        ...http,
        fileId: 'local-file',
        persistBody: true,
        form: [{ name: 'x', value: 'saved', enabled: true }],
        tls: { ...http.tls, caFileId: 'local-ca' },
        auth: { ...http.auth, token: '{{token}}' },
      },
    });
    expect(saved.http?.fileId).toBe('');
    expect(saved.http?.tls.caFileId).toBe('');
    expect(saved.http?.auth.token).toBe('{{token}}');
    expect(saved.http?.form[0].value).toBe('saved');
  });
});

describe('response rendering helpers', () => {
  it('formats JSON while preserving large integers and escaped strings', () => {
    const text = '{"id":9223372036854775807,"value":"quote \\" , ","data":[true,null]}';
    expect(prettyJSON(text)).toContain('9223372036854775807');
    expect(JSON.parse(prettyJSON(text))).toEqual(JSON.parse(text));
    expect(prettyJSON('<script>unsafe()</script>')).toBe('<script>unsafe()</script>');
  });
  it('returns raw JSON for excessive depth or formatting expansion', () => {
    const deep = '['.repeat(50000) + '0' + ']'.repeat(50000);
    expect(prettyJSON(deep)).toBe(deep);
    const wide = '['.repeat(64) + Array(20000).fill('0').join(',') + ']'.repeat(64);
    expect(prettyJSON(wide)).toBe(wide);
  });
  it('bounds hexadecimal output', () => {
    expect(responseHex(btoa('\x00\xffA'))).toContain('00 ff 41');
    expect(responseHex('', 65536, 'привет')).toContain('d0 bf d1 80');
    expect(responseHex('', 4, 'long text body')).toContain('первые 4 байт');
    expect(responseHex(btoa('0123456789'), 4)).toContain('первые 4 байт');
    expect(responseHex('!!badbase64')).toContain('Не удалось');
  });
});

describe('legacy HTTP definitions', () => {
  it('fills defaults from zero-valued older Go records', () => {
    const http = normalizeHTTP({
      bodyKind: '',
      version: '',
      timeoutMs: 0,
      maxDownloadBytes: 0,
      auth: { type: '', username: '', password: '', token: '', keyName: '', keyValue: '', in: '' },
      proxy: { mode: '', url: '' },
    });
    expect(http.bodyKind).toBe('raw');
    expect(http.version).toBe('auto');
    expect(http.auth.type).toBe('none');
    expect(http.proxy.mode).toBe('none');
    expect(http.timeoutMs).toBe(30000);
    expect(http.maxDownloadBytes).toBeGreaterThan(0);
    expect(http.query).toEqual([]);
    expect(http.multipart).toEqual([]);
  });
});
