import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  fixtureGRPCExecution,
  fixtureProtoSchema,
  fixtureState,
  testBackend,
} from '../test/backend';
import {
  createAppStore,
  hasVolatileCredentials,
  hasVolatileFiles,
  persistentRequest,
} from './store';
import type { GRPCExecution } from './types';
import { newRequest, normalizeGRPC } from './types';

function grpcBackend(count = 1) {
  const data = fixtureState();
  data.protoSchemas = [fixtureProtoSchema()];
  const request = { ...newRequest('w1', 'c1', '', 'grpc'), id: 'g1', name: 'Unary' };
  request.grpc = {
    ...request.grpc!,
    endpoint: 'localhost:50051',
    schemaId: 'schema1',
    method: '/example.catalog.v1.Catalog/Get',
  };
  data.requests.push(request);
  for (let index = 2; index <= count; index++)
    data.requests.push({ ...structuredClone(request), id: `g${index}`, name: `Unary ${index}` });
  return testBackend(data);
}
afterEach(() => vi.useRealTimers());

describe('gRPC execution lifecycle', () => {
  it('sends the unsaved snapshot and persists only body opt-in, safe metadata references and reusable TLS settings', async () => {
    const backend = grpcBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    const draft = store.getState().tabs.g1.draft;
    const grpc = normalizeGRPC(draft.grpc!);
    store.getState().editRequest('g1', {
      ...draft,
      grpc: {
        ...grpc,
        body: '{"itemId":"9223372036854775807"}',
        metadata: [
          { name: 'authorization', value: 'Bearer literal-canary', enabled: true },
          { name: 'authorization', value: 'Bearer {{token}}', enabled: true },
          { name: 'x-signature-bin', value: 'private-base64', enabled: false },
          { name: 'x-request-id', value: 'trace-id', enabled: true },
        ],
        tls: {
          ...grpc.tls!,
          caFileId: 'ephemeral-ca',
          clientCertFileId: 'ephemeral-cert',
          clientKeyFileId: 'ephemeral-key',
          serverName: 'api.test',
        },
      },
    });
    await store.getState().sendGRPCUnary('g1');
    expect(backend.api.StartGRPCUnary).toHaveBeenCalledWith(
      expect.objectContaining({
        grpc: expect.objectContaining({
          body: '{"itemId":"9223372036854775807"}',
          metadata: expect.arrayContaining([
            expect.objectContaining({ value: 'Bearer literal-canary' }),
          ]),
        }),
      }),
    );
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    await store.getState().saveRequest('g1');
    const saved = backend.state().requests.find((request) => request.id === 'g1')!;
    expect(saved.grpc?.body).toBe('');
    expect(saved.grpc?.metadata?.map((row) => row.value)).toEqual([
      '',
      'Bearer {{token}}',
      '',
      'trace-id',
    ]);
    expect(saved.grpc?.tls).toMatchObject({
      caFileId: '',
      clientCertFileId: '',
      clientKeyFileId: '',
      serverName: 'api.test',
    });
    expect(hasVolatileCredentials(store.getState().tabs.g1)).toBe(true);
    expect(hasVolatileFiles(store.getState().tabs.g1)).toBe(true);
    store.getState().requestClose('g1');
    expect(store.getState().closingId).toBe('g1');
    await store.getState().resolveClose('cancel');
    expect(store.getState().tabs.g1.draft.grpc?.metadata?.[0].value).toContain('literal-canary');
    store.getState().dispose();
  });

  it('blocks streaming methods before starting a unary transport', async () => {
    const backend = grpcBackend();
    const method = await backend.api.InspectProtoMethod();
    backend.api.InspectProtoMethod.mockResolvedValue({
      ...method,
      method: { ...method.method, serverStreaming: true },
    });
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().sendGRPCUnary('g1');
    expect(backend.api.StartGRPCUnary).not.toHaveBeenCalled();
    expect(store.getState().grpcRuns.g1.error).toContain('Streaming RPC');
    store.getState().dispose();
  });

  it('continues polling when cancellation acknowledges before the worker finishes', async () => {
    vi.useFakeTimers();
    const backend = grpcBackend();
    backend.api.StartGRPCUnary.mockResolvedValue(fixtureGRPCExecution('g1', { state: 'running' }));
    backend.api.GetGRPCExecution.mockResolvedValueOnce(
      fixtureGRPCExecution('g1', { state: 'running' }),
    ).mockResolvedValue(fixtureGRPCExecution('g1', { state: 'canceled' }));
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().sendGRPCUnary('g1');
    await store.getState().cancelGRPCExecution('g1');
    expect(store.getState().grpcRuns.g1.execution?.state).toBe('running');
    await vi.advanceTimersByTimeAsync(150);
    expect(store.getState().grpcRuns.g1.execution?.state).toBe('canceled');
    const calls = backend.api.GetGRPCExecution.mock.calls.length;
    await vi.advanceTimersByTimeAsync(1000);
    expect(backend.api.GetGRPCExecution).toHaveBeenCalledTimes(calls);
    store.getState().dispose();
  });

  it.each(['close', 'delete', 'workspace', 'dispose', 'late-start'] as const)(
    'cancels and waits for completion before forgetting on %s',
    async (action) => {
      vi.useFakeTimers();
      const backend = grpcBackend();
      let release!: (execution: GRPCExecution) => void;
      let terminal = false,
        queries = 0;
      if (action === 'late-start')
        backend.api.StartGRPCUnary.mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              release = resolve;
            }),
        );
      else
        backend.api.StartGRPCUnary.mockResolvedValue(
          fixtureGRPCExecution('g1', { state: 'running' }),
        );
      backend.api.GetGRPCExecution.mockImplementation(async () => {
        terminal = ++queries >= 2;
        return fixtureGRPCExecution('g1', { state: terminal ? 'canceled' : 'running' });
      });
      backend.api.ForgetGRPCExecution.mockImplementation(async () => {
        if (!terminal) throw new Error('Worker is still running');
      });
      const store = createAppStore(backend.api);
      await store.getState().initialize();
      store.getState().openRequest('g1');
      const sending = store.getState().sendGRPCUnary('g1');
      await vi.advanceTimersByTimeAsync(0);
      if (action !== 'late-start') await sending;
      if (action === 'close' || action === 'late-start') store.getState().requestClose('g1');
      else if (action === 'delete') await store.getState().mutate((api) => api.DeleteRequest('g1'));
      else if (action === 'workspace') store.getState().switchWorkspace('w2');
      else store.getState().dispose();
      if (action === 'late-start') release(fixtureGRPCExecution('g1', { state: 'running' }));
      await vi.advanceTimersByTimeAsync(0);
      expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1');
      expect(backend.api.ForgetGRPCExecution).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(150);
      await sending;
      expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
      expect(store.getState().grpcRuns.g1).toBeUndefined();
      store.getState().dispose();
    },
  );

  it('ignores duplicate Call while starting and allows Cancel before Start returns', async () => {
    const backend = grpcBackend();
    let release!: (execution: GRPCExecution) => void;
    backend.api.StartGRPCUnary.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    );
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    const first = store.getState().sendGRPCUnary('g1');
    await vi.waitFor(() => expect(backend.api.StartGRPCUnary).toHaveBeenCalledTimes(1));
    await store.getState().sendGRPCUnary('g1');
    await store.getState().cancelGRPCExecution('g1');
    release(fixtureGRPCExecution('g1', { state: 'running' }));
    await first;
    expect(backend.api.StartGRPCUnary).toHaveBeenCalledTimes(1);
    expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1');
    expect(store.getState().grpcRuns.g1.error).toContain('отменена');
    store.getState().dispose();
  });

  it('selects a discovered schema, then preserves the local choice after a reflection failure', async () => {
    const backend = grpcBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().discoverGRPCSchema('g1');
    expect(store.getState().tabs.g1.draft.grpc?.schemaId).toMatch(/^reflection/);
    expect(store.getState().tabs.g1.draft.grpc?.method).toBe('');
    const current = store.getState().tabs.g1.draft;
    store.getState().editRequest('g1', {
      ...current,
      grpc: { ...current.grpc!, schemaId: 'schema1', method: '/example.catalog.v1.Catalog/Get' },
    });
    backend.api.StartGRPCReflection.mockResolvedValue(
      fixtureGRPCExecution('g1', {
        kind: 'reflection',
        state: 'failed',
        error: 'Reflection is not enabled',
      }),
    );
    await store.getState().discoverGRPCSchema('g1');
    expect(store.getState().tabs.g1.draft.grpc?.schemaId).toBe('schema1');
    expect(store.getState().tabs.g1.draft.grpc?.method).toBe('/example.catalog.v1.Catalog/Get');
    await store.getState().sendGRPCUnary('g1');
    expect(backend.api.StartGRPCUnary).toHaveBeenCalledOnce();
    store.getState().dispose();
  });

  it.each(['endpoint', 'environment'])(
    'does not replace schema or method when reflection finishes after %s changes',
    async (changed) => {
      const backend = grpcBackend();
      const original = backend.api.StartGRPCReflection.getMockImplementation()!;
      let release!: () => void;
      backend.api.StartGRPCReflection.mockImplementationOnce(async (request) => {
        const execution = await original(request);
        return new Promise((resolve) => {
          release = () => resolve(execution);
        });
      });
      const store = createAppStore(backend.api, 60000);
      await store.getState().initialize();
      store.getState().openRequest('g1');
      const discovering = store.getState().discoverGRPCSchema('g1');
      await vi.waitFor(() => expect(release).toBeTypeOf('function'));
      const current = store.getState().tabs.g1.draft;
      if (changed === 'endpoint')
        store.getState().editRequest('g1', {
          ...current,
          grpc: { ...current.grpc!, endpoint: 'new-server:50051' },
        });
      else {
        await store.getState().mutate((api) =>
          api.SaveEnvironment({
            id: 'env2',
            workspaceId: 'w1',
            name: 'Other server',
            variables: [],
          }),
        );
        const workspace = store.getState().data.workspaces[0];
        await store
          .getState()
          .mutate((api) => api.SaveWorkspace({ ...workspace, activeEnvironmentId: 'env2' }));
      }
      release();
      await discovering;
      expect(store.getState().tabs.g1.draft.grpc).toMatchObject({
        endpoint: changed === 'endpoint' ? 'new-server:50051' : 'localhost:50051',
        schemaId: 'schema1',
        method: '/example.catalog.v1.Catalog/Get',
      });
      expect(
        store.getState().data.protoSchemas.some((schema) => schema.name === 'Server reflection'),
      ).toBe(true);
      store.getState().dispose();
    },
  );

  it('uses secure defaults for older persisted definitions and honors body opt-in', () => {
    const legacy = normalizeGRPC({
      endpoint: '',
      method: '',
      body: '',
      persistBody: false,
      timeoutMs: 0,
    });
    expect(legacy).toMatchObject({
      plaintext: false,
      metadata: [],
      timeoutMs: 30000,
      maxReceiveBytes: 4 * 1024 * 1024,
      compression: 'none',
    });
    const request = newRequest('w1', 'c1', '', 'grpc');
    const saved = persistentRequest({
      ...request,
      grpc: {
        ...request.grpc!,
        persistBody: true,
        body: 'saved body',
        metadata: [{ name: 'authorization-bin', value: '{{metadata}}', enabled: true }],
      },
    });
    expect(saved.grpc?.body).toBe('saved body');
    expect(saved.grpc?.metadata?.[0].value).toBe('{{metadata}}');
  });

  it('releases the oldest completed previews across many open tabs while retaining running executions', async () => {
    vi.useFakeTimers();
    const backend = grpcBackend(34);
    backend.api.StartGRPCUnary.mockImplementation(async (request) =>
      fixtureGRPCExecution(request.id, { state: request.id === 'g1' ? 'running' : 'completed' }),
    );
    backend.api.GetGRPCExecution.mockResolvedValue(
      fixtureGRPCExecution('g1', { state: 'running' }),
    );
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    for (let index = 1; index <= 34; index++) {
      const id = `g${index}`;
      store.getState().openRequest(id);
      await store.getState().sendGRPCUnary(id);
    }
    expect(Object.values(store.getState().grpcRuns).filter((run) => run.execution)).toHaveLength(
      17,
    );
    expect(store.getState().grpcRuns.g1.execution?.state).toBe('running');
    expect(store.getState().grpcRuns.g2).toMatchObject({
      execution: null,
      notice: expect.stringContaining('освобождён'),
    });
    expect(store.getState().grpcRuns.g34.execution?.state).toBe('completed');
    expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g2');
    expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalledWith('grpc-g1');
    backend.api.GetGRPCExecution.mockResolvedValue(
      fixtureGRPCExecution('g1', { state: 'canceled' }),
    );
    store.getState().dispose();
  });

  it('bounds retained preview strings even when fewer than 16 responses are open', async () => {
    const backend = grpcBackend(2);
    backend.api.StartGRPCUnary.mockImplementation(async (request) =>
      fixtureGRPCExecution(request.id, { messageJSON: 'x'.repeat(9 * 1024 * 1024) }),
    );
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    for (const id of ['g1', 'g2']) {
      store.getState().openRequest(id);
      await store.getState().sendGRPCUnary(id);
    }
    expect(store.getState().grpcRuns.g1.execution).toBeNull();
    expect(store.getState().grpcRuns.g2.execution).not.toBeNull();
    expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
    store.getState().dispose();
  });

  it('does not resurrect a terminal result with an older in-flight running poll', async () => {
    vi.useFakeTimers();
    const backend = grpcBackend();
    backend.api.StartGRPCUnary.mockResolvedValue(fixtureGRPCExecution('g1', { state: 'running' }));
    let release!: (execution: GRPCExecution) => void;
    backend.api.GetGRPCExecution.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    ).mockResolvedValue(fixtureGRPCExecution('g1', { state: 'canceled' }));
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().sendGRPCUnary('g1');
    await vi.advanceTimersByTimeAsync(150);
    await store.getState().cancelGRPCExecution('g1');
    expect(store.getState().grpcRuns.g1.execution?.state).toBe('canceled');
    release(fixtureGRPCExecution('g1', { state: 'running' }));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.getState().grpcRuns.g1.execution?.state).toBe('canceled');
    const calls = backend.api.GetGRPCExecution.mock.calls.length;
    await vi.advanceTimersByTimeAsync(1000);
    expect(backend.api.GetGRPCExecution).toHaveBeenCalledTimes(calls);
    store.getState().dispose();
  });

  it.each([true, false])(
    'retries failed result reads and recovers=%s without leaving Call blocked',
    async (recovers) => {
      vi.useFakeTimers();
      const backend = grpcBackend();
      backend.api.StartGRPCUnary.mockResolvedValue(
        fixtureGRPCExecution('g1', { state: 'running' }),
      );
      backend.api.GetGRPCExecution.mockRejectedValueOnce(new Error('Transient bridge error'));
      if (!recovers)
        backend.api.GetGRPCExecution.mockRejectedValueOnce(
          new Error('Transient bridge error'),
        ).mockRejectedValueOnce(new Error('Transient bridge error'));
      backend.api.GetGRPCExecution.mockResolvedValue(
        fixtureGRPCExecution('g1', { state: recovers ? 'completed' : 'canceled' }),
      );
      const store = createAppStore(backend.api, 60000);
      await store.getState().initialize();
      store.getState().openRequest('g1');
      await store.getState().sendGRPCUnary('g1');
      await vi.advanceTimersByTimeAsync(450);
      const run = store.getState().grpcRuns.g1;
      expect(run.starting).toBe(false);
      expect(run.execution?.state).not.toBe('running');
      if (recovers) {
        expect(run.execution?.state).toBe('completed');
        expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
      } else {
        expect(run.error).toContain('трёх попыток');
        expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1');
        expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
      }
      store.getState().dispose();
    },
  );
});
