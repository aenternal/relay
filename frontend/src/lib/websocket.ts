import { normalizeWebSocket, protoErrorMessage } from './types';
import type {
  Bridge,
  RequestDefinition,
  State,
  WebSocketEvent,
  WebSocketExecution,
  WebSocketSavedSession,
  WebSocketSessionSummary,
} from './types';

export const webSocketActive = (execution?: WebSocketExecution | null) =>
  !!execution && ['connecting', 'open', 'closing'].includes(execution.state);
const stringBytes = (value: unknown): number => {
  if (typeof value === 'string') return value.length * 2;
  if (!value || typeof value !== 'object') return 0;
  return Object.values(value).reduce<number>((sum, entry) => sum + stringBytes(entry), 0);
};
export interface WebSocketRun {
  starting: boolean;
  execution: WebSocketExecution | null;
  events: WebSocketEvent[];
  lastSequence: number;
  evictedEvents: number;
  missedEvents: number;
  draining: boolean;
  sending: boolean;
  pinging: boolean;
  disconnecting: boolean;
  error: string;
  notice: string;
}
export interface WebSocketState {
  websocketRuns: Record<string, WebSocketRun>;
  websocketSessions: WebSocketSessionSummary[];
  savedWebSocketSession: WebSocketSavedSession | null;
  websocketHistoryLoading: boolean;
  websocketHistoryError: string;
}
interface Context extends WebSocketState {
  tabs: Record<string, { draft: RequestDefinition }>;
  data: State;
  workspaceId: string;
}
const emptyRun = (): WebSocketRun => ({
  starting: false,
  execution: null,
  events: [],
  lastSequence: 0,
  evictedEvents: 0,
  missedEvents: 0,
  draining: false,
  sending: false,
  pinging: false,
  disconnecting: false,
  error: '',
  notice: '',
});
export const initialWebSocketState = (): WebSocketState => ({
  websocketRuns: {},
  websocketSessions: [],
  savedWebSocketSession: null,
  websocketHistoryLoading: false,
  websocketHistoryError: '',
});

// One serial poll per connection commits bounded previews before ACK. The Go
// reader never waits for this UI; gaps and preview eviction remain visible.
export function createWebSocketActions(
  bridge: Bridge,
  get: () => Context,
  set: (patch: Partial<WebSocketState>) => void,
) {
  let disposed = false,
    historyGeneration = 0;
  const generations = new Map<string, number>();
  const polls = new Map<string, ReturnType<typeof setTimeout>>();
  const pendingAcks = new Map<string, number>();
  const cleanups = new Map<string, Promise<void>>();
  const completedOrder = new Set<string>();
  const lastPings = new Map<string, number>();
  const active = (id: string, generation: number) =>
    !disposed && !!get().tabs[id] && generations.get(id) === generation;

  async function discard(execution: WebSocketExecution) {
    const existing = cleanups.get(execution.id);
    if (existing) return existing;
    const cleanup = (async () => {
      if (webSocketActive(execution)) {
        try {
          await bridge.CancelWebSocket(execution.id);
        } catch {
          /* Already terminal. */
        }
      }
      for (let attempt = 0; attempt < 40; attempt++) {
        try {
          await bridge.ForgetWebSocket(execution.id);
          return;
        } catch {
          /* Worker may still be closing. */
        }
        await new Promise<void>((resolve) => setTimeout(resolve, 150));
      }
    })();
    cleanups.set(execution.id, cleanup);
    void cleanup.finally(() => cleanups.delete(execution.id));
    return cleanup;
  }
  function releaseWebSocket(id: string) {
    generations.set(id, (generations.get(id) || 0) + 1);
    clearTimeout(polls.get(id));
    polls.delete(id);
    pendingAcks.delete(id);
    completedOrder.delete(id);
    lastPings.delete(id);
    const execution = get().websocketRuns[id]?.execution;
    if (execution) void discard(execution);
  }
  function publish(id: string, run: WebSocketRun) {
    const previous = get().websocketRuns[id]?.execution;
    if (
      previous?.id === run.execution?.id &&
      previous &&
      !webSocketActive(previous) &&
      webSocketActive(run.execution)
    )
      return;
    if (
      previous?.id === run.execution?.id &&
      previous?.state === 'closing' &&
      run.execution?.state === 'open'
    )
      run = { ...run, execution: { ...run.execution, state: 'closing' } };
    const runs = { ...get().websocketRuns, [id]: run };
    completedOrder.delete(id);
    if (run.execution && !webSocketActive(run.execution) && !run.draining) completedOrder.add(id);
    const eventLimit = Math.max(1, Math.min(2000, get().data.settings.historyLimit || 2000));
    for (const [key, value] of Object.entries(runs)) {
      const events = [...value.events];
      let bytes = stringBytes(events),
        evicted = value.evictedEvents;
      while (events.length > eventLimit || (events.length > 1 && bytes > 24 * 1024 * 1024)) {
        bytes -= stringBytes(events.shift());
        evicted++;
      }
      runs[key] = { ...value, events, evictedEvents: evicted };
    }
    const completed = [...completedOrder].filter(
      (key) => runs[key]?.execution && !webSocketActive(runs[key].execution),
    );
    let bytes = stringBytes(runs) + stringBytes(get().savedWebSocketSession);
    while (completed.length > 16 || (completed.length && bytes > 32 * 1024 * 1024)) {
      const key = completed.shift()!,
        old = runs[key];
      releaseWebSocket(key);
      runs[key] = {
        ...emptyRun(),
        notice:
          'Сессия освобождена из памяти: до 16 завершённых сессий и 32 МиБ истории. Сохранённые сессии доступны ниже.',
      };
      bytes -= stringBytes(old) - stringBytes(runs[key]);
    }
    while (bytes > 32 * 1024 * 1024) {
      const oldest = Object.entries(runs)
        .filter(([, value]) => value.events.length)
        .sort(([, a], [, b]) => a.events[0].timestamp.localeCompare(b.events[0].timestamp))[0];
      if (!oldest) break;
      const [key, value] = oldest;
      bytes -= stringBytes(value.events[0]);
      runs[key] = {
        ...value,
        events: value.events.slice(1),
        evictedEvents: value.evictedEvents + 1,
      };
    }
    set({ websocketRuns: runs });
  }
  function poll(id: string, generation: number, executionId: string, failures = 0) {
    if (!active(id, generation)) return;
    polls.set(
      id,
      setTimeout(async () => {
        polls.delete(id);
        if (!active(id, generation)) return;
        try {
          const pending = pendingAcks.get(id);
          if (pending !== undefined) {
            await bridge.AckWebSocket(executionId, pending);
            if (!active(id, generation)) return;
            pendingAcks.delete(id);
          }
          const after = get().websocketRuns[id].lastSequence;
          const batch = await bridge.PollWebSocket(executionId, after);
          if (!active(id, generation)) return;
          const current = get().websocketRuns[id];
          const seen = new Set<number>();
          const events = batch.events
            .filter((event) => {
              if (event.sequence <= current.lastSequence || seen.has(event.sequence)) return false;
              seen.add(event.sequence);
              return true;
            })
            .sort((a, b) => a.sequence - b.sequence);
          let cursor = current.lastSequence,
            missed = current.missedEvents;
          for (const event of events) {
            missed += Math.max(0, event.sequence - cursor - 1);
            cursor = event.sequence;
          }
          const draining = webSocketActive(batch.execution) || batch.events.length > 0;
          publish(id, {
            ...current,
            starting: false,
            execution: batch.execution,
            events: [...current.events, ...events],
            lastSequence: cursor,
            missedEvents: missed,
            draining,
          });
          if (cursor > after) {
            pendingAcks.set(id, cursor);
            await bridge.AckWebSocket(executionId, cursor);
            if (!active(id, generation)) return;
            pendingAcks.delete(id);
          }
          if (draining && get().websocketRuns[id].execution) poll(id, generation, executionId);
        } catch {
          if (!active(id, generation)) return;
          if (failures < 2) {
            poll(id, generation, executionId, failures + 1);
            return;
          }
          const current = get().websocketRuns[id];
          releaseWebSocket(id);
          publish(id, {
            ...current,
            starting: false,
            draining: false,
            execution: current.execution && { ...current.execution, state: 'failed' },
            error:
              'Не удалось прочитать или подтвердить WebSocket после трёх попыток. Соединение прервано; полученная история сохранена.',
          });
        }
      }, 150),
    );
  }
  const updateError = (id: string, generation: number, error: unknown, fallback: string) => {
    if (active(id, generation))
      publish(id, { ...get().websocketRuns[id], error: protoErrorMessage(error, fallback) });
  };
  const actions = {
    async connectWebSocket(id: string) {
      const draft = get().tabs[id]?.draft,
        run = get().websocketRuns[id];
      if (
        !draft?.websocket ||
        disposed ||
        run?.starting ||
        run?.draining ||
        webSocketActive(run?.execution)
      )
        return;
      const snapshot = structuredClone(draft);
      releaseWebSocket(id);
      const generation = generations.get(id)!;
      publish(id, { ...emptyRun(), starting: true });
      try {
        const execution = await bridge.ConnectWebSocket(snapshot);
        if (!active(id, generation)) {
          await discard(execution);
          return;
        }
        publish(id, { ...emptyRun(), execution, draining: true });
        poll(id, generation, execution.id);
      } catch (error) {
        if (active(id, generation))
          publish(id, {
            ...emptyRun(),
            error: protoErrorMessage(error, 'Не удалось открыть WebSocket.'),
          });
      }
    },
    async sendWebSocketMessage(id: string) {
      const run = get().websocketRuns[id],
        definition = get().tabs[id]?.draft.websocket;
      if (!definition || run?.execution?.state !== 'open' || run.sending || run.disconnecting)
        return;
      const generation = generations.get(id)!;
      const ws = normalizeWebSocket(definition);
      const message = {
        kind: ws.bodyKind,
        encoding: ws.bodyKind === 'text' ? 'text' : ws.bodyEncoding,
        data: ws.body,
        fileId: ws.fileId,
      };
      publish(id, { ...run, sending: true, error: '' });
      try {
        await bridge.SendWebSocketMessage(run.execution.id, message);
      } catch (error) {
        updateError(id, generation, error, 'Не удалось отправить сообщение WebSocket.');
      } finally {
        if (active(id, generation)) publish(id, { ...get().websocketRuns[id], sending: false });
      }
    },
    async pingWebSocket(id: string) {
      const run = get().websocketRuns[id];
      if (
        run?.execution?.state !== 'open' ||
        run.pinging ||
        run.disconnecting ||
        Date.now() - (lastPings.get(id) ?? -Infinity) < 1000
      )
        return;
      const generation = generations.get(id)!;
      lastPings.set(id, Date.now());
      publish(id, { ...run, pinging: true, error: '' });
      try {
        await bridge.PingWebSocket(run.execution.id);
      } catch (error) {
        updateError(id, generation, error, 'Не удалось отправить Ping.');
      } finally {
        if (active(id, generation)) publish(id, { ...get().websocketRuns[id], pinging: false });
      }
    },
    async disconnectWebSocket(id: string, code = 1000, reason = '') {
      const run = get().websocketRuns[id];
      if (run?.starting) {
        await actions.cancelWebSocket(id);
        return;
      }
      if (!run?.execution || !webSocketActive(run.execution) || run.disconnecting) return;
      if (run.execution.state !== 'open') {
        await actions.cancelWebSocket(id);
        return;
      }
      const generation = generations.get(id)!;
      publish(id, { ...run, disconnecting: true, error: '' });
      try {
        await bridge.DisconnectWebSocket(run.execution.id, code, reason);
        if (active(id, generation)) {
          const current = get().websocketRuns[id];
          publish(id, {
            ...current,
            disconnecting: false,
            execution:
              current.execution && webSocketActive(current.execution)
                ? { ...current.execution, state: 'closing' }
                : current.execution,
          });
        }
      } catch (error) {
        updateError(id, generation, error, 'Не удалось закрыть WebSocket.');
        if (active(id, generation))
          publish(id, { ...get().websocketRuns[id], disconnecting: false });
      }
    },
    async cancelWebSocket(id: string) {
      const run = get().websocketRuns[id];
      if (!run) return;
      if (run.starting) {
        releaseWebSocket(id);
        publish(id, { ...emptyRun(), notice: 'Подключение WebSocket отменено.' });
        return;
      }
      if (!run.execution || !webSocketActive(run.execution)) return;
      const generation = generations.get(id)!;
      try {
        await bridge.CancelWebSocket(run.execution.id);
      } catch (error) {
        updateError(id, generation, error, 'Не удалось прервать WebSocket.');
      }
    },
    async webSocketPrimary(id: string) {
      if (get().websocketRuns[id]?.execution?.state === 'open')
        await actions.sendWebSocketMessage(id);
      else await actions.connectWebSocket(id);
    },
    async refreshWebSocketSessions(workspaceId: string) {
      const generation = ++historyGeneration;
      set({ websocketHistoryLoading: true, websocketHistoryError: '' });
      try {
        const sessions = await bridge.ListWebSocketSessions(workspaceId);
        if (!disposed && generation === historyGeneration && get().workspaceId === workspaceId)
          set({ websocketSessions: sessions });
      } catch {
        if (!disposed && generation === historyGeneration)
          set({ websocketHistoryError: 'Не удалось прочитать сохранённые сессии.' });
      } finally {
        if (!disposed && generation === historyGeneration) set({ websocketHistoryLoading: false });
      }
    },
    async saveWebSocketSession(id: string, includePayloads: boolean) {
      const run = get().websocketRuns[id],
        workspaceId = get().workspaceId;
      if (!run?.execution || run.draining || webSocketActive(run.execution)) return false;
      const generation = generations.get(id)!;
      try {
        await bridge.SaveWebSocketSession(run.execution.id, includePayloads);
        if (!active(id, generation)) return false;
        publish(id, {
          ...get().websocketRuns[id],
          notice: includePayloads
            ? 'Сессия сохранена с обработанными preview payloads.'
            : 'Сессия сохранена без payloads.',
        });
        await actions.refreshWebSocketSessions(workspaceId);
        return true;
      } catch (error) {
        updateError(id, generation, error, 'Не удалось сохранить сессию WebSocket.');
        return false;
      }
    },
    async openSavedWebSocketSession(workspaceId: string, id: string) {
      const generation = ++historyGeneration;
      set({
        websocketHistoryLoading: true,
        websocketHistoryError: '',
        savedWebSocketSession: null,
      });
      try {
        const session = await bridge.GetSavedWebSocketSession(workspaceId, id);
        if (!disposed && generation === historyGeneration && get().workspaceId === workspaceId) {
          set({ savedWebSocketSession: session });
          const first = Object.keys(get().websocketRuns)[0];
          if (first) publish(first, get().websocketRuns[first]);
        }
      } catch {
        if (!disposed && generation === historyGeneration)
          set({ websocketHistoryError: 'Не удалось открыть сохранённую сессию.' });
      } finally {
        if (!disposed && generation === historyGeneration) set({ websocketHistoryLoading: false });
      }
    },
    closeSavedWebSocketSession() {
      ++historyGeneration;
      set({ savedWebSocketSession: null, websocketHistoryLoading: false });
    },
    async deleteSavedWebSocketSession(workspaceId: string, id: string) {
      try {
        await bridge.DeleteSavedWebSocketSession(workspaceId, id);
        if (disposed || get().workspaceId !== workspaceId) return;
        if (get().savedWebSocketSession?.id === id) set({ savedWebSocketSession: null });
        await actions.refreshWebSocketSessions(workspaceId);
      } catch {
        if (!disposed && get().workspaceId === workspaceId)
          set({ websocketHistoryError: 'Не удалось удалить сохранённую сессию.' });
      }
    },
  };
  function resetWebSocketHistory() {
    ++historyGeneration;
    set({
      websocketSessions: [],
      savedWebSocketSession: null,
      websocketHistoryError: '',
      websocketHistoryLoading: false,
    });
  }
  function disposeWebSockets() {
    Object.keys(get().websocketRuns).forEach(releaseWebSocket);
    disposed = true;
    ++historyGeneration;
    polls.forEach(clearTimeout);
    polls.clear();
    set(initialWebSocketState());
  }
  return { actions, releaseWebSocket, resetWebSocketHistory, disposeWebSockets };
}
