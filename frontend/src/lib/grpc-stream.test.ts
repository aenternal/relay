import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAppStore } from './store';
import type { GRPCExecution, GRPCStreamPoll } from './types';
import { newRequest } from './types';
import {
  fixtureGRPCExecution,
  fixtureGRPCStreamMessage,
  fixtureProtoMethod,
  fixtureProtoSchema,
  fixtureState,
  testBackend,
} from '../test/backend';

function backendForStream(rpcType: 'server' | 'client' | 'bidi' = 'bidi', count = 1) {
  const data = fixtureState();
  data.protoSchemas = [fixtureProtoSchema()];
  for (let index = 1; index <= count; index++) {
    const request = { ...newRequest('w1', 'c1', '', 'grpc'), id: `g${index}`, name: 'Stream' };
    request.grpc = {
      ...request.grpc!,
      endpoint: 'localhost:50051',
      schemaId: 'schema1',
      method: '/example.catalog.v1.Catalog/Watch',
    };
    data.requests.push(request);
  }
  const backend = testBackend(data);
  const method = fixtureProtoMethod();
  backend.api.InspectProtoMethod.mockResolvedValue({
    ...method,
    method: {
      ...method.method,
      clientStreaming: rpcType !== 'server',
      serverStreaming: rpcType !== 'client',
    },
  });
  const store = createAppStore(backend.api, 60000);
  return { store, backend };
}
const execution = (overrides: Partial<GRPCExecution> = {}) =>
  fixtureGRPCExecution('g1', {
    kind: 'stream',
    rpcType: 'bidi',
    state: 'running',
    streamState: 'open',
    messageJSON: '',
    wireBase64: '',
    wireBytes: 0,
    ...overrides,
  });
afterEach(() => vi.useRealTimers());

describe('gRPC stream lifecycle', () => {
  it('opens without sending and sends again after the first bidi response, using only the current body with the original execution', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream();
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().sendGRPCRequest('g1');
    expect(backend.api.StartGRPCStream).toHaveBeenCalledOnce();
    expect(backend.api.SendGRPCStreamMessage).not.toHaveBeenCalled();
    const update = (body: string) => {
      const draft = store.getState().tabs.g1.draft;
      store.getState().editRequest('g1', {
        ...draft,
        grpc: {
          ...draft.grpc!,
          body,
          bodyFormat: 'json5',
          method: '/edited/Method',
          endpoint: 'new:5000',
        },
      });
    };
    update("{ text: 'первое' }");
    await store.getState().sendGRPCRequest('g1');
    await vi.advanceTimersByTimeAsync(150);
    expect(
      store.getState().grpcRuns.g1.messages?.filter((message) => message.direction === 'received'),
    ).toHaveLength(1);
    update("{ text: 'второе' }");
    await store.getState().sendGRPCRequest('g1');
    await vi.advanceTimersByTimeAsync(150);
    expect(backend.api.SendGRPCStreamMessage).toHaveBeenLastCalledWith(
      'grpc-g1',
      "{ text: 'второе' }",
      'json5',
    );
    expect(backend.api.StartGRPCStream).toHaveBeenCalledOnce();
    expect(store.getState().grpcRuns.g1.messages).toHaveLength(4);
    expect(backend.api.AckGRPCStream).toHaveBeenLastCalledWith('grpc-g1', 4);
    store.getState().dispose();
  });

  it.each(['client', 'bidi'] as const)(
    'half-closes %s sending while continuing to receive the final response',
    async (rpcType) => {
      vi.useFakeTimers();
      const { store, backend } = backendForStream(rpcType);
      backend.api.StartGRPCStream.mockResolvedValue(execution({ rpcType }));
      backend.api.HalfCloseGRPCStream.mockResolvedValue();
      backend.api.PollGRPCStream.mockResolvedValueOnce({
        execution: execution({ rpcType, streamState: 'half_closed' }),
        messages: [],
        nextSequence: 0,
      })
        .mockResolvedValueOnce({
          execution: execution({
            rpcType,
            state: 'completed',
            streamState: 'closed',
            receivedCount: 1,
          }),
          messages: [fixtureGRPCStreamMessage()],
          nextSequence: 1,
        })
        .mockResolvedValue({
          execution: execution({
            rpcType,
            state: 'completed',
            streamState: 'closed',
            receivedCount: 1,
          }),
          messages: [],
          nextSequence: 1,
        });
      await store.getState().initialize();
      store.getState().openRequest('g1');
      await store.getState().startGRPCStream('g1');
      await store.getState().halfCloseGRPCStream('g1');
      await store.getState().sendGRPCRequest('g1');
      expect(backend.api.SendGRPCStreamMessage).not.toHaveBeenCalled();
      expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(450);
      expect(store.getState().grpcRuns.g1.messages).toHaveLength(1);
      expect(store.getState().grpcRuns.g1.draining).toBe(false);
      expect(backend.api.PollGRPCStream).toHaveBeenCalledTimes(3);
      store.getState().dispose();
    },
  );

  it.each([0, 7])(
    'drains a server stream with %i responses and never sends a second request message',
    async (count) => {
      vi.useFakeTimers();
      const { store, backend } = backendForStream('server');
      backend.api.StartGRPCStream.mockResolvedValue(execution({ rpcType: 'server' }));
      const terminal = execution({
        rpcType: 'server',
        state: 'completed',
        streamState: 'closed',
        sentCount: 1,
        receivedCount: count,
      });
      backend.api.PollGRPCStream.mockResolvedValueOnce({
        execution: terminal,
        messages: Array.from({ length: count }, (_, index) => fixtureGRPCStreamMessage(index + 1)),
        nextSequence: count,
      }).mockResolvedValue({ execution: terminal, messages: [], nextSequence: count });
      await store.getState().initialize();
      store.getState().openRequest('g1');
      await store.getState().sendGRPCRequest('g1');
      await store.getState().sendGRPCRequest('g1');
      await vi.advanceTimersByTimeAsync(450);
      expect(store.getState().grpcRuns.g1.messages).toHaveLength(count);
      expect(store.getState().grpcRuns.g1.execution?.receivedCount).toBe(count);
      expect(store.getState().grpcRuns.g1.draining).toBe(false);
      expect(backend.api.SendGRPCStreamMessage).not.toHaveBeenCalled();
      expect(backend.api.HalfCloseGRPCStream).not.toHaveBeenCalled();
      expect(backend.api.PollGRPCStream).toHaveBeenCalledTimes(count ? 2 : 1);
      store.getState().dispose();
    },
  );

  it('keeps messages, trailers and status when the stream fails after responses', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream();
    const terminal = execution({
      state: 'failed',
      streamState: 'closed',
      code: 7,
      codeName: 'PermissionDenied',
      trailers: [{ name: 'reason', value: 'denied', enabled: true }],
    });
    backend.api.PollGRPCStream.mockResolvedValueOnce({
      execution: terminal,
      messages: [
        fixtureGRPCStreamMessage(1),
        fixtureGRPCStreamMessage(2),
        fixtureGRPCStreamMessage(2),
      ],
      nextSequence: 2,
    }).mockResolvedValue({ execution: terminal, messages: [], nextSequence: 2 });
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().startGRPCStream('g1');
    await vi.advanceTimersByTimeAsync(300);
    expect(store.getState().grpcRuns.g1.messages).toHaveLength(2);
    expect(store.getState().grpcRuns.g1.execution).toMatchObject({
      code: 7,
      trailers: [{ value: 'denied' }],
    });
    store.getState().dispose();
  });

  it('commits before ACK, retries ACK before polling again, deduplicates replayed batches and drains terminal queues', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream();
    const terminal = execution({ state: 'completed', streamState: 'closed' });
    const batch: GRPCStreamPoll = {
      execution: terminal,
      messages: [fixtureGRPCStreamMessage(1), fixtureGRPCStreamMessage(2)],
      nextSequence: 2,
    };
    backend.api.PollGRPCStream.mockResolvedValueOnce(batch)
      .mockResolvedValueOnce(batch)
      .mockResolvedValue({ execution: terminal, messages: [], nextSequence: 2 });
    backend.api.AckGRPCStream.mockImplementationOnce(async () => {
      expect(store.getState().grpcRuns.g1.messages).toHaveLength(2);
      throw new Error('lost ACK');
    });
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().startGRPCStream('g1');
    await vi.advanceTimersByTimeAsync(150);
    expect(backend.api.PollGRPCStream).toHaveBeenCalledTimes(1);
    expect(store.getState().grpcRuns.g1.draining).toBe(true);
    await vi.advanceTimersByTimeAsync(300);
    expect(backend.api.AckGRPCStream).toHaveBeenCalledTimes(2);
    expect(backend.api.PollGRPCStream).toHaveBeenLastCalledWith('grpc-g1', 2);
    expect(store.getState().grpcRuns.g1.messages).toHaveLength(2);
    expect(store.getState().grpcRuns.g1.draining).toBe(false);
    store.getState().dispose();
  });

  it('serializes polls for a slow bridge and bounds accepted history before acknowledging', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream();
    let release!: (value: GRPCStreamPoll) => void;
    backend.api.PollGRPCStream.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    ).mockResolvedValue({ execution: execution(), messages: [], nextSequence: 100 });
    backend.api.AckGRPCStream.mockImplementation(async () => {
      expect(store.getState().grpcRuns.g1.messages).toHaveLength(3);
      expect(store.getState().grpcRuns.g1.evictedMessages).toBe(97);
    });
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store
      .getState()
      .mutate((api) => api.SaveSettings({ ...store.getState().data.settings, historyLimit: 3 }));
    await store.getState().startGRPCStream('g1');
    await vi.advanceTimersByTimeAsync(1500);
    expect(backend.api.PollGRPCStream).toHaveBeenCalledTimes(1);
    release({
      execution: execution(),
      messages: Array.from({ length: 100 }, (_, index) => fixtureGRPCStreamMessage(index + 1)),
      nextSequence: 100,
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(backend.api.AckGRPCStream).toHaveBeenCalledWith('grpc-g1', 100);
    expect(store.getState().grpcRuns.g1.lastSequence).toBe(100);
    store.getState().dispose();
  });

  it('releases message previews across active streams to enforce the shared byte budget', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream('bidi', 2);
    backend.api.PollGRPCStream.mockImplementation(async (id, after) => ({
      execution: execution({ id, requestId: id.replace('grpc-', '') }),
      messages: after
        ? []
        : [fixtureGRPCStreamMessage(1, { messageJSON: 'x'.repeat(9 * 1024 * 1024) })],
      nextSequence: 1,
    }));
    await store.getState().initialize();
    for (const id of ['g1', 'g2']) {
      store.getState().openRequest(id);
      await store.getState().startGRPCStream(id);
    }
    await vi.advanceTimersByTimeAsync(150);
    expect(
      Object.values(store.getState().grpcRuns).every((run) => run.execution?.state === 'running'),
    ).toBe(true);
    expect(
      Object.values(store.getState().grpcRuns).reduce(
        (sum, run) => sum + (run.messages?.length || 0),
        0,
      ),
    ).toBe(1);
    expect(
      Object.values(store.getState().grpcRuns).reduce(
        (sum, run) => sum + (run.evictedMessages || 0),
        0,
      ),
    ).toBe(1);
    expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
    store.getState().dispose();
  });

  it('includes active stream metadata in the shared budget and frees a completed unary preview first', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream('bidi', 2);
    backend.api.InspectProtoMethod.mockResolvedValueOnce(fixtureProtoMethod());
    backend.api.StartGRPCUnary.mockResolvedValue(
      fixtureGRPCExecution('g1', {
        messageJSON: 'x'.repeat(16 * 1024 * 1024 - 128 * 1024),
      }),
    );
    backend.api.StartGRPCStream.mockResolvedValue(
      execution({
        id: 'grpc-g2',
        requestId: 'g2',
        headers: [{ name: 'large-metadata', value: 'x'.repeat(256 * 1024), enabled: true }],
      }),
    );
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().sendGRPCUnary('g1');
    expect(store.getState().grpcRuns.g1.execution).not.toBeNull();
    store.getState().openRequest('g2');
    await store.getState().startGRPCStream('g2');
    expect(store.getState().grpcRuns.g1.execution).toBeNull();
    expect(store.getState().grpcRuns.g1.notice).toContain('общий лимит');
    expect(store.getState().grpcRuns.g2.execution?.state).toBe('running');
    expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
    expect(backend.api.CancelGRPCExecution).not.toHaveBeenCalled();
    store.getState().dispose();
  });

  it.each(['close', 'delete', 'workspace', 'dispose', 'late-start'] as const)(
    'cancels and forgets a stream on %s without resurrecting a late result',
    async (action) => {
      vi.useFakeTimers();
      const { store, backend } = backendForStream();
      let release!: (value: GRPCExecution) => void;
      if (action === 'late-start')
        backend.api.StartGRPCStream.mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              release = resolve;
            }),
        );
      backend.api.GetGRPCExecution.mockResolvedValue(
        execution({ state: 'canceled', streamState: 'closed' }),
      );
      await store.getState().initialize();
      store.getState().openRequest('g1');
      const starting = store.getState().startGRPCStream('g1');
      await vi.advanceTimersByTimeAsync(0);
      if (action !== 'late-start') await starting;
      if (action === 'close' || action === 'late-start') store.getState().requestClose('g1');
      else if (action === 'delete') await store.getState().mutate((api) => api.DeleteRequest('g1'));
      else if (action === 'workspace') store.getState().switchWorkspace('w2');
      else store.getState().dispose();
      if (action === 'late-start') release(execution());
      await starting;
      await vi.advanceTimersByTimeAsync(0);
      expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1');
      expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
      expect(store.getState().grpcRuns.g1).toBeUndefined();
      store.getState().dispose();
    },
  );

  it('cancels after repeated ACK failure while preserving the accepted history', async () => {
    vi.useFakeTimers();
    const { store, backend } = backendForStream();
    backend.api.PollGRPCStream.mockResolvedValue({
      execution: execution(),
      messages: [fixtureGRPCStreamMessage()],
      nextSequence: 1,
    });
    backend.api.AckGRPCStream.mockRejectedValue(new Error('bridge down'));
    backend.api.GetGRPCExecution.mockResolvedValue(execution({ state: 'canceled' }));
    await store.getState().initialize();
    store.getState().openRequest('g1');
    await store.getState().startGRPCStream('g1');
    await vi.advanceTimersByTimeAsync(450);
    expect(backend.api.PollGRPCStream).toHaveBeenCalledTimes(1);
    expect(store.getState().grpcRuns.g1.messages).toHaveLength(1);
    expect(store.getState().grpcRuns.g1.error).toContain('трёх попыток');
    expect(backend.api.CancelGRPCExecution).toHaveBeenCalledWith('grpc-g1');
    expect(backend.api.ForgetGRPCExecution).toHaveBeenCalledWith('grpc-g1');
    store.getState().dispose();
  });
});
