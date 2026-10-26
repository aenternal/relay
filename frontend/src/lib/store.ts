import { createStore } from 'zustand/vanilla';
import {
  emptyState,
  normalizeGRPC,
  normalizeHTTP,
  normalizeWebSocket,
  protoErrorMessage,
} from './types';
import { createWebSocketActions, initialWebSocketState } from './websocket';
import type { WebSocketState } from './websocket';
import type {
  Bridge,
  Capabilities,
  HTTPExecution,
  GRPCExecution,
  GRPCStreamMessage,
  RequestDefinition,
  SelectedFile,
  State,
  UIState,
  ProtoImportInput,
  ProtoImportResult,
  NativeProtoImportInput,
  NativeProtoSelection,
  ProtoSchemaDetail,
  ProtoMethodDetail,
  ProtoValidationInput,
  ProtoValidationResult,
  ProjectFormat,
  ProjectExport,
  ProjectPreview,
  ProjectImportResult,
  SelectedProjectFile,
  ProjectFileExport,
  BuildInfo,
} from './types';

export interface Tab {
  draft: RequestDefinition;
  saved: RequestDefinition;
  version: number;
  saving: boolean;
}
export const isDirty = (tab: Tab) => JSON.stringify(tab.draft) !== JSON.stringify(tab.saved);
export const hasVolatileBody = (tab: Tab) =>
  !!(
    (tab.draft.http &&
      !tab.draft.http.persistBody &&
      (tab.draft.http.body ||
        tab.draft.http.form?.some((item) => item.value) ||
        tab.draft.http.multipart?.some((item) => item.value))) ||
    (tab.draft.grpc?.body && !tab.draft.grpc.persistBody) ||
    (tab.draft.websocket?.body && !tab.draft.websocket.persistBody)
  );

export const hasVolatileCredentials = (tab: Tab) => {
  const auth = tab.draft.http?.auth;
  return (
    (!!auth &&
      [auth.password, auth.token, auth.keyValue].some(
        (value) => value && !/^\s*\{\{[^{}]+\}\}\s*$/.test(value),
      )) ||
    !!tab.draft.grpc?.metadata?.some(
      (row) => isSensitiveMetadata(row.name) && !safeMetadataValue(row.name, row.value),
    ) ||
    (!!tab.draft.websocket &&
      ((tab.draft.websocket.headers ?? []).some(
        (row) => isSensitiveMetadata(row.name) && !safeMetadataValue(row.name, row.value),
      ) ||
        (tab.draft.websocket.query ?? []).some(
          (row) =>
            (isSensitiveMetadata(row.name) || /^(key|auth|code)$/i.test(row.name)) &&
            !!row.value &&
            !isVariableReference(row.value),
        ) ||
        persistentWebSocketURL(tab.draft.websocket.url) !== tab.draft.websocket.url))
  );
};
export const hasVolatileFiles = (tab: Tab) => {
  const http = tab.draft.http;
  return (
    (!!http &&
      !!(
        http.fileId ||
        http.tls?.caFileId ||
        http.tls?.clientCertFileId ||
        http.tls?.clientKeyFileId ||
        http.multipart?.some((part) => part.fileId)
      )) ||
    !!(
      tab.draft.grpc?.tls?.caFileId ||
      tab.draft.grpc?.tls?.clientCertFileId ||
      tab.draft.grpc?.tls?.clientKeyFileId
    ) ||
    !!(
      tab.draft.websocket?.fileId ||
      tab.draft.websocket?.tls?.caFileId ||
      tab.draft.websocket?.tls?.clientCertFileId ||
      tab.draft.websocket?.tls?.clientKeyFileId
    )
  );
};
const isVariableReference = (value: string) => /^\s*\{\{[^{}]+\}\}\s*$/.test(value);
const normalizedMetadataName = (name: string) =>
  name
    .toLowerCase()
    .replace(/-bin$/, '')
    .replace(/[-_\s]/g, '');
export const isSensitiveMetadata = (name: string) => {
  const normalized = normalizedMetadataName(name);
  return (
    [
      'authorization',
      'proxyauthorization',
      'cookie',
      'setcookie',
      'apikey',
      'xapikey',
      'token',
      'accesstoken',
      'refreshtoken',
      'xauthtoken',
      'xtoken',
      'password',
      'passwd',
      'secret',
      'clientsecret',
      'signature',
      'credential',
      'credentials',
    ].includes(normalized) ||
    normalized.includes('apikey') ||
    /(token|secret|password|signature|credential)$/.test(normalized)
  );
};
function safeMetadataValue(name: string, value: string) {
  const trimmed = value.trim();
  if (!trimmed || isVariableReference(trimmed)) return true;
  return (
    ['authorization', 'proxyauthorization'].includes(normalizedMetadataName(name)) &&
    /^(?:Bearer|Basic)\s+\{\{[^{}]+\}\}$/i.test(trimmed)
  );
}
function persistentWebSocketURL(value: string) {
  return value.replace(
    /([?&])([^=&#]+)=([^&#]*)/g,
    (match, prefix: string, name: string, content: string) => {
      try {
        const key = decodeURIComponent(name.replace(/\+/g, ' '));
        const decoded = decodeURIComponent(content.replace(/\+/g, ' '));
        return (isSensitiveMetadata(key) || /^(key|auth|code)$/i.test(key)) &&
          decoded &&
          !isVariableReference(decoded)
          ? `${prefix}${name}=`
          : match;
      } catch {
        return match;
      }
    },
  );
}
export const hasVolatileData = (tab: Tab) =>
  hasVolatileBody(tab) || hasVolatileCredentials(tab) || hasVolatileFiles(tab);

function withoutMissingProtoSchema(request: RequestDefinition, data: State): RequestDefinition {
  const schemaId = request.grpc?.schemaId;
  if (
    !schemaId ||
    data.protoSchemas?.some(
      (schema) => schema.id === schemaId && schema.workspaceId === request.workspaceId,
    )
  )
    return request;
  return { ...request, grpc: { ...request.grpc!, schemaId: '' } };
}

// Sensitive bodies stay only in the current tab unless the user opts in.
export function persistentRequest(value: RequestDefinition): RequestDefinition {
  return {
    ...value,
    http: value.http && {
      ...normalizeHTTP(value.http),
      body: value.http.persistBody ? value.http.body : '',
      form: (value.http.form ?? []).map((item) => ({
        ...item,
        value: value.http!.persistBody ? item.value : '',
      })),
      multipart: (value.http.multipart ?? []).map((item) => ({
        ...item,
        value: value.http!.persistBody ? item.value : '',
        fileId: '',
      })),
      fileId: '',
      tls: {
        ...normalizeHTTP(value.http).tls,
        caFileId: '',
        clientCertFileId: '',
        clientKeyFileId: '',
      },
      auth: Object.fromEntries(
        Object.entries(normalizeHTTP(value.http).auth).map(([key, item]) =>
          ['password', 'token', 'keyValue'].includes(key)
            ? [key, /^\s*\{\{[^{}]+\}\}\s*$/.test(item) ? item : '']
            : [key, item],
        ),
      ) as NonNullable<RequestDefinition['http']>['auth'],
    },
    grpc: value.grpc && {
      ...normalizeGRPC(value.grpc),
      body: value.grpc.persistBody ? value.grpc.body : '',
      metadata: (value.grpc.metadata ?? []).map((row) => ({
        ...row,
        value:
          isSensitiveMetadata(row.name) && !safeMetadataValue(row.name, row.value) ? '' : row.value,
      })),
      tls: {
        ...normalizeGRPC(value.grpc).tls!,
        caFileId: '',
        clientCertFileId: '',
        clientKeyFileId: '',
      },
    },
    websocket: value.websocket && {
      ...normalizeWebSocket(value.websocket),
      url: persistentWebSocketURL(value.websocket.url),
      body: value.websocket.persistBody ? value.websocket.body || '' : '',
      headers: (value.websocket.headers ?? []).map((row) => ({
        ...row,
        value:
          isSensitiveMetadata(row.name) && !safeMetadataValue(row.name, row.value) ? '' : row.value,
      })),
      query: (value.websocket.query ?? []).map((row) => ({
        ...row,
        value:
          (isSensitiveMetadata(row.name) || /^(key|auth|code)$/i.test(row.name)) &&
          !isVariableReference(row.value)
            ? ''
            : row.value,
      })),
      fileId: '',
      tls: {
        ...normalizeWebSocket(value.websocket).tls,
        caFileId: '',
        clientCertFileId: '',
        clientKeyFileId: '',
      },
    },
  };
}

export interface HTTPRun {
  starting: boolean;
  execution: HTTPExecution | null;
  error: string;
}
export interface GRPCRun {
  starting: boolean;
  kind: 'unary' | 'reflection' | 'stream';
  execution: GRPCExecution | null;
  error: string;
  notice?: string;
  messages?: GRPCStreamMessage[];
  evictedMessages?: number;
  lastSequence?: number;
  sending?: boolean;
  halfClosing?: boolean;
  draining?: boolean;
}

interface AppStore extends WebSocketState {
  data: State;
  loaded: boolean;
  error: string;
  tabs: Record<string, Tab>;
  openIds: string[];
  activeId: string;
  workspaceId: string;
  closingId: string;
  capabilities: Capabilities;
  buildInfo: BuildInfo | null;
  runs: Record<string, HTTPRun>;
  grpcRuns: Record<string, GRPCRun>;
  nativeFilesAvailable: boolean;
  selectProjectFile(): Promise<SelectedProjectFile>;
  saveProjectFile(workspaceId: string, includeBodies: boolean): Promise<ProjectFileExport>;
  exportProject(workspaceId: string, includeBodies: boolean): Promise<ProjectExport>;
  previewProjectImport(format: ProjectFormat, data: string): Promise<ProjectPreview>;
  importProject(
    format: ProjectFormat,
    data: string,
    includeBodies: boolean,
  ): Promise<ProjectImportResult | undefined>;
  importProtoSchema(
    input: ProtoImportInput | NativeProtoImportInput,
  ): Promise<ProtoImportResult | undefined>;
  inspectProtoSchema(workspaceId: string, schemaId: string): Promise<ProtoSchemaDetail>;
  inspectProtoMethod(
    workspaceId: string,
    schemaId: string,
    method: string,
  ): Promise<ProtoMethodDetail>;
  validateProtoMessage(input: ProtoValidationInput): Promise<ProtoValidationResult>;
  selectProtoImport(kind: string): Promise<NativeProtoSelection>;
  selectProtoIncludeDirectory(): Promise<SelectedFile>;
  sendHTTPRequest(id: string): Promise<void>;
  cancelHTTPRequest(id: string): Promise<void>;
  sendGRPCUnary(id: string): Promise<void>;
  sendGRPCRequest(id: string): Promise<void>;
  startGRPCStream(id: string): Promise<void>;
  sendGRPCStreamMessage(id: string): Promise<void>;
  halfCloseGRPCStream(id: string): Promise<void>;
  discoverGRPCSchema(id: string): Promise<void>;
  cancelGRPCExecution(id: string): Promise<void>;
  connectWebSocket(id: string): Promise<void>;
  sendWebSocketMessage(id: string): Promise<void>;
  pingWebSocket(id: string): Promise<void>;
  disconnectWebSocket(id: string, code?: number, reason?: string): Promise<void>;
  cancelWebSocket(id: string): Promise<void>;
  webSocketPrimary(id: string): Promise<void>;
  refreshWebSocketSessions(workspaceId: string): Promise<void>;
  saveWebSocketSession(id: string, includePayloads: boolean): Promise<boolean>;
  openSavedWebSocketSession(workspaceId: string, id: string): Promise<void>;
  closeSavedWebSocketSession(): void;
  deleteSavedWebSocketSession(workspaceId: string, id: string): Promise<void>;
  chooseHTTPFile(kind: string): Promise<SelectedFile | undefined>;
  saveHTTPResponse(id: string): Promise<boolean>;
  initialize(): Promise<void>;
  mutate(
    action: (bridge: Bridge) => Promise<State>,
    safeError?: string,
  ): Promise<State | undefined>;
  openRequest(id: string): void;
  editRequest(id: string, draft: RequestDefinition): void;
  saveRequest(id: string): Promise<boolean>;
  requestClose(id: string): void;
  resolveClose(choice: 'save' | 'discard' | 'cancel'): Promise<void>;
  switchWorkspace(id: string): void;
  clearError(): void;
  dispose(): void;
}

export function createAppStore(bridge: Bridge, autosaveMs = 800) {
  let queue: Promise<unknown> = Promise.resolve();
  const timers = new Map<string, ReturnType<typeof setTimeout>>();
  let disposed = false;
  let navigationGeneration = 0;
  const polls = new Map<string, ReturnType<typeof setTimeout>>();
  const generations = new Map<string, number>();
  const grpcPolls = new Map<string, ReturnType<typeof setTimeout>>();
  const grpcGenerations = new Map<string, number>();
  const reflectionConfigurations = new Map<string, string>();
  const reflectionConfiguration = (request: RequestDefinition, data: State) => {
    if (!request.grpc) return '';
    const {
      endpoint,
      plaintext,
      tls,
      authority,
      metadata,
      timeoutMs,
      connectTimeoutMs,
      maxSendBytes,
      maxReceiveBytes,
      compression,
      schemaId,
      method,
    } = normalizeGRPC(request.grpc);
    const activeEnvironmentId = data.workspaces.find(
      (workspace) => workspace.id === request.workspaceId,
    )?.activeEnvironmentId;
    const environment = data.environments.find(
      (entry) => entry.id === activeEnvironmentId && entry.workspaceId === request.workspaceId,
    );
    return JSON.stringify([
      request.workspaceId,
      activeEnvironmentId,
      environment,
      endpoint,
      plaintext,
      tls,
      authority,
      metadata,
      timeoutMs,
      connectTimeoutMs,
      maxSendBytes,
      maxReceiveBytes,
      compression,
      schemaId,
      method,
    ]);
  };
  let loadPromise: Promise<void> | undefined;
  const store = createStore<AppStore>()((set, get) => {
    const ws = createWebSocketActions(bridge, get, set);
    const active = (id: string, generation: number) =>
      !disposed && !!get().tabs[id] && generations.get(id) === generation;
    const cleanups = new Map<string, Promise<void>>();
    const grpcCleanups = new Map<string, Promise<void>>();
    const grpcPendingAcks = new Map<string, number>();
    const grpcCompletedOrder = new Set<string>();
    const grpcActive = (id: string, generation: number) =>
      !disposed && !!get().tabs[id] && grpcGenerations.get(id) === generation;
    function discardGRPCExecution(execution: GRPCExecution): Promise<void> {
      const existing = grpcCleanups.get(execution.id);
      if (existing) return existing;
      const cleanup = (async () => {
        try {
          if (execution.state === 'running') await bridge.CancelGRPCExecution(execution.id);
        } catch {
          /* The operation can already have finished. */
        }
        for (let attempt = 0; attempt < 40; attempt++) {
          try {
            const current =
              execution.state === 'running'
                ? await bridge.GetGRPCExecution(execution.id)
                : execution;
            if (current.state !== 'running') {
              await bridge.ForgetGRPCExecution(execution.id);
              return;
            }
          } catch {
            try {
              await bridge.ForgetGRPCExecution(execution.id);
              return;
            } catch {
              /* Worker completion can race cancellation. */
            }
          }
          await new Promise<void>((resolve) => setTimeout(resolve, 150));
        }
      })();
      grpcCleanups.set(execution.id, cleanup);
      void cleanup.finally(() => grpcCleanups.delete(execution.id));
      return cleanup;
    }
    function releaseGRPCRun(id: string) {
      grpcGenerations.set(id, (grpcGenerations.get(id) ?? 0) + 1);
      reflectionConfigurations.delete(id);
      grpcPendingAcks.delete(id);
      grpcCompletedOrder.delete(id);
      clearTimeout(grpcPolls.get(id));
      grpcPolls.delete(id);
      const execution = get().grpcRuns[id]?.execution;
      if (execution) void discardGRPCExecution(execution);
    }
    function publishGRPCRun(id: string, run: GRPCRun) {
      const current = get().grpcRuns[id]?.execution;
      // A poll issued before Cancel can return an older running snapshot afterward.
      if (
        current &&
        run.execution &&
        current.id === run.execution.id &&
        current.state !== 'running' &&
        run.execution.state === 'running'
      )
        return false;
      if (
        current?.id === run.execution?.id &&
        current?.streamState === 'half_closed' &&
        run.execution?.streamState === 'open'
      )
        run = { ...run, execution: { ...run.execution, streamState: 'half_closed' } };
      const runs = { ...get().grpcRuns, [id]: run };
      grpcCompletedOrder.delete(id);
      if (run.execution && run.execution.state !== 'running' && !run.draining)
        grpcCompletedOrder.add(id);
      const stringBytes = (value: unknown): number => {
        if (typeof value === 'string') return value.length * 2;
        if (!value || typeof value !== 'object') return 0;
        return Object.values(value).reduce<number>((total, entry) => total + stringBytes(entry), 0);
      };
      const completed = [...grpcCompletedOrder].filter(
        (key) => runs[key]?.execution && runs[key].execution!.state !== 'running',
      );
      let retainedBytes = completed.reduce((total, key) => total + stringBytes(runs[key]), 0);
      while (completed.length > 16 || retainedBytes > 32 * 1024 * 1024) {
        const key = completed.shift()!;
        const previous = runs[key];
        retainedBytes -= stringBytes(previous);
        releaseGRPCRun(key);
        runs[key] = {
          starting: false,
          kind: previous.kind,
          execution: null,
          error: '',
          notice:
            'Предыдущий ответ gRPC освобождён из памяти: сохранено до 16 результатов и 32 МиБ текста. Вызовите метод заново, чтобы получить ответ.',
        };
      }
      // Timeline entries are disposable previews. Keep active streams and their counters,
      // while releasing old entries when any active history consumes the shared budget.
      const historyLimit = Math.max(1, Math.min(2000, get().data.settings.historyLimit || 2000));
      for (const [key, previous] of Object.entries(runs)) {
        if (!previous.messages?.length) continue;
        const messages = [...previous.messages];
        let bytes = stringBytes(messages);
        let evicted = previous.evictedMessages || 0;
        while (
          messages.length > historyLimit ||
          (messages.length > 1 && bytes > 24 * 1024 * 1024)
        ) {
          bytes -= stringBytes(messages.shift());
          evicted++;
        }
        runs[key] = { ...previous, messages, evictedMessages: evicted };
      }
      let allBytes = Object.values(runs).reduce((total, value) => total + stringBytes(value), 0);
      while (allBytes > 32 * 1024 * 1024) {
        const completedKey = completed.shift();
        if (completedKey) {
          const previous = runs[completedKey];
          releaseGRPCRun(completedKey);
          const replacement: GRPCRun = {
            starting: false,
            kind: previous.kind,
            execution: null,
            error: '',
            notice:
              'Предыдущий ответ gRPC освобождён из памяти: общий лимит результатов и истории — 32 МиБ текста. Вызовите метод заново, чтобы получить ответ.',
          };
          allBytes -= stringBytes(previous) - stringBytes(replacement);
          runs[completedKey] = replacement;
          continue;
        }
        const oldest = Object.entries(runs)
          .filter(([, value]) => value.messages?.length)
          .sort(([, a], [, b]) =>
            a.messages![0].timestamp.localeCompare(b.messages![0].timestamp),
          )[0];
        if (!oldest) break;
        const [key, previous] = oldest;
        const messages = previous.messages!.slice(1);
        allBytes -= stringBytes(previous.messages![0]);
        runs[key] = { ...previous, messages, evictedMessages: (previous.evictedMessages || 0) + 1 };
      }
      set({ grpcRuns: runs });
      return !!runs[id].execution;
    }
    function pollGRPCStream(id: string, generation: number, executionId: string, failures = 0) {
      if (!grpcActive(id, generation)) return;
      grpcPolls.set(
        id,
        setTimeout(async () => {
          grpcPolls.delete(id);
          if (!grpcActive(id, generation)) return;
          try {
            const pendingAck = grpcPendingAcks.get(id);
            if (pendingAck !== undefined) {
              await bridge.AckGRPCStream(executionId, pendingAck);
              if (!grpcActive(id, generation)) return;
              grpcPendingAcks.delete(id);
            }
            const current = get().grpcRuns[id];
            const afterSequence = current.lastSequence || 0;
            const batch = await bridge.PollGRPCStream(executionId, afterSequence);
            if (!grpcActive(id, generation)) return;
            const previous = get().grpcRuns[id];
            const seen = new Set<number>();
            const messages = batch.messages.filter((message) => {
              if (message.sequence <= (previous.lastSequence || 0) || seen.has(message.sequence))
                return false;
              seen.add(message.sequence);
              return true;
            });
            const lastSequence = Math.max(
              previous.lastSequence || 0,
              ...messages.map((message) => message.sequence),
            );
            const draining = batch.execution.state === 'running' || batch.messages.length > 0;
            if (
              !publishGRPCRun(id, {
                ...previous,
                starting: false,
                kind: 'stream',
                execution: batch.execution,
                messages: [...(previous.messages || []), ...messages],
                lastSequence,
                draining,
              })
            )
              return;
            // Acknowledge only after the bounded history accepted the batch. Eviction is
            // visible in the UI and does not keep the producer's queue occupied.
            if (lastSequence > afterSequence) {
              grpcPendingAcks.set(id, lastSequence);
              await bridge.AckGRPCStream(executionId, lastSequence);
              if (!grpcActive(id, generation)) return;
              grpcPendingAcks.delete(id);
            }
            if (draining) pollGRPCStream(id, generation, executionId);
          } catch {
            if (!grpcActive(id, generation)) return;
            if (failures < 2) {
              pollGRPCStream(id, generation, executionId, failures + 1);
              return;
            }
            const previous = get().grpcRuns[id];
            releaseGRPCRun(id);
            set({
              grpcRuns: {
                ...get().grpcRuns,
                [id]: {
                  ...previous,
                  starting: false,
                  draining: false,
                  execution: previous.execution && {
                    ...previous.execution,
                    state: 'failed',
                    streamState: 'closed',
                  },
                  error:
                    'Не удалось прочитать или подтвердить сообщения gRPC после трёх попыток. Запрошена отмена; полученная история сохранена.',
                },
              },
            });
          }
        }, 150),
      );
    }
    async function reflectionCompleted(id: string, generation: number, execution: GRPCExecution) {
      if (
        execution.kind !== 'reflection' ||
        execution.state !== 'completed' ||
        !execution.schemaId ||
        !grpcActive(id, generation)
      )
        return;
      const data = await get().mutate(
        (api) => api.LoadState(),
        'Не удалось прочитать обнаруженную схему.',
      );
      if (!data || !grpcActive(id, generation)) return;
      const current = get().tabs[id]?.draft;
      if (
        current?.grpc &&
        reflectionConfigurations.get(id) === reflectionConfiguration(current, get().data)
      )
        get().editRequest(id, {
          ...current,
          grpc: { ...current.grpc, schemaId: execution.schemaId, method: '' },
        });
    }
    function pollGRPC(id: string, generation: number, executionId: string, failures = 0) {
      if (!grpcActive(id, generation)) return;
      grpcPolls.set(
        id,
        setTimeout(async () => {
          grpcPolls.delete(id);
          if (!grpcActive(id, generation)) return;
          try {
            const execution = await bridge.GetGRPCExecution(executionId);
            if (!grpcActive(id, generation)) return;
            if (
              !publishGRPCRun(id, {
                starting: false,
                kind: execution.kind === 'reflection' ? 'reflection' : 'unary',
                execution,
                error: '',
              })
            )
              return;
            if (execution.state === 'running') pollGRPC(id, generation, executionId);
            else await reflectionCompleted(id, generation, execution);
          } catch {
            if (grpcActive(id, generation)) {
              if (failures < 2) {
                pollGRPC(id, generation, executionId, failures + 1);
                return;
              }
              const kind = get().grpcRuns[id].kind;
              releaseGRPCRun(id);
              set({
                grpcRuns: {
                  ...get().grpcRuns,
                  [id]: {
                    kind,
                    starting: false,
                    execution: null,
                    error:
                      'Не удалось получить результат gRPC после трёх попыток. Запрошена отмена; вызов можно повторить.',
                  },
                },
              });
            }
          }
        }, 150),
      );
    }
    async function startGRPC(
      id: string,
      requestedKind: 'unary' | 'reflection' | 'stream' | 'call',
    ) {
      const tab = get().tabs[id];
      const run = get().grpcRuns[id];
      if (
        !tab?.draft.grpc ||
        disposed ||
        run?.starting ||
        run?.draining ||
        run?.execution?.state === 'running'
      )
        return;
      let kind: GRPCRun['kind'] = requestedKind === 'call' ? 'unary' : requestedKind;
      const snapshot = structuredClone(tab.draft);
      releaseGRPCRun(id);
      const generation = grpcGenerations.get(id)!;
      if (kind === 'reflection')
        reflectionConfigurations.set(id, reflectionConfiguration(snapshot, get().data));
      set({
        grpcRuns: { ...get().grpcRuns, [id]: { starting: true, kind, execution: null, error: '' } },
      });
      try {
        if (kind !== 'reflection') {
          const definition = snapshot.grpc!;
          if (!definition.schemaId || !definition.method)
            throw new Error('Выберите схему Protobuf и метод RPC.');
          const detail = await bridge.InspectProtoMethod(
            snapshot.workspaceId,
            definition.schemaId,
            definition.method,
          );
          if (!grpcActive(id, generation)) return;
          const streaming = detail.method.clientStreaming || detail.method.serverStreaming;
          if (requestedKind === 'call') kind = streaming ? 'stream' : 'unary';
          else if (streaming !== (kind === 'stream'))
            throw new Error(
              streaming ? 'Streaming RPC: откройте поток.' : 'Выберите streaming метод RPC.',
            );
        }
        const execution = await (kind === 'reflection'
          ? bridge.StartGRPCReflection(snapshot)
          : kind === 'stream'
            ? bridge.StartGRPCStream(snapshot)
            : bridge.StartGRPCUnary(snapshot));
        if (!grpcActive(id, generation)) {
          await discardGRPCExecution(execution);
          return;
        }
        if (
          !publishGRPCRun(id, {
            starting: false,
            kind,
            execution,
            error: '',
            ...(kind === 'stream'
              ? { messages: [], lastSequence: 0, evictedMessages: 0, draining: true }
              : {}),
          })
        )
          return;
        if (kind === 'stream') pollGRPCStream(id, generation, execution.id);
        else if (execution.state === 'running') pollGRPC(id, generation, execution.id);
        else await reflectionCompleted(id, generation, execution);
      } catch (error) {
        if (grpcActive(id, generation))
          set({
            grpcRuns: {
              ...get().grpcRuns,
              [id]: {
                starting: false,
                kind,
                execution: null,
                error: protoErrorMessage(
                  error,
                  kind === 'reflection'
                    ? 'Не удалось обнаружить схему. Используйте локальную схему Protobuf.'
                    : 'Не удалось вызвать gRPC метод.',
                ),
              },
            },
          });
      }
    }
    function discardExecution(execution: HTTPExecution): Promise<void> {
      const existing = cleanups.get(execution.id);
      if (existing) return existing;
      const cleanup = (async () => {
        try {
          if (execution.state === 'running') await bridge.CancelHTTPExecution(execution.id);
        } catch {
          /* Still try to release a result if cancellation already completed. */
        }
        // Cancel acknowledges context cancellation before the worker closes its spool.
        // Wait for its terminal state before Forget, including after tab/store disposal.
        for (let attempt = 0; attempt < 40; attempt++) {
          try {
            const current =
              execution.state === 'running'
                ? await bridge.GetHTTPExecution(execution.id)
                : execution;
            if (current.state !== 'running') {
              await bridge.ForgetHTTPExecution(execution.id);
              return;
            }
          } catch {
            try {
              await bridge.ForgetHTTPExecution(execution.id);
              return;
            } catch {
              /* Worker completion or bridge shutdown can race this call. */
            }
          }
          await new Promise<void>((resolve) => setTimeout(resolve, 150));
        }
      })();
      cleanups.set(execution.id, cleanup);
      void cleanup.finally(() => cleanups.delete(execution.id));
      return cleanup;
    }
    function releaseRun(id: string) {
      ws.releaseWebSocket(id);
      releaseGRPCRun(id);
      generations.set(id, (generations.get(id) ?? 0) + 1);
      clearTimeout(polls.get(id));
      polls.delete(id);
      const execution = get().runs[id]?.execution;
      if (execution) void discardExecution(execution);
    }
    function poll(id: string, generation: number, executionId: string) {
      if (!active(id, generation)) return;
      polls.set(
        id,
        setTimeout(async () => {
          polls.delete(id);
          if (!active(id, generation)) return;
          try {
            const execution = await bridge.GetHTTPExecution(executionId);
            if (!active(id, generation)) return;
            set({ runs: { ...get().runs, [id]: { starting: false, execution, error: '' } } });
            if (execution.state === 'running') poll(id, generation, executionId);
          } catch {
            if (active(id, generation)) {
              set({
                runs: {
                  ...get().runs,
                  [id]: {
                    ...get().runs[id],
                    starting: false,
                    error: 'Не удалось получить результат запроса.',
                  },
                },
              });
              void bridge.CancelHTTPExecution(executionId).catch(() => {});
            }
          }
        }, 150),
      );
    }
    function persistUI() {
      void get().mutate((api) => {
        const { workspaceId, openIds, activeId } = get();
        const ui: UIState = {
          workspaceId,
          openRequestIds: openIds.filter((id) => get().tabs[id]?.draft.workspaceId === workspaceId),
          activeRequestId: activeId,
        };
        return api.SaveUI(ui);
      });
    }
    function closeNow(id: string) {
      if (id === get().activeId) navigationGeneration++;
      clearTimeout(timers.get(id));
      timers.delete(id);
      releaseRun(id);
      const nextRuns = { ...get().runs };
      delete nextRuns[id];
      const nextGRPCRuns = { ...get().grpcRuns };
      delete nextGRPCRuns[id];
      const nextWSRuns = { ...get().websocketRuns };
      delete nextWSRuns[id];
      const { tabs, openIds, activeId } = get();
      const remaining = openIds.filter((item) => item !== id);
      const nextTabs = { ...tabs };
      delete nextTabs[id];
      set({
        tabs: nextTabs,
        runs: nextRuns,
        grpcRuns: nextGRPCRuns,
        websocketRuns: nextWSRuns,
        openIds: remaining,
        activeId:
          activeId === id
            ? (remaining.findLast(
                (item) => nextTabs[item]?.draft.workspaceId === get().workspaceId,
              ) ?? '')
            : activeId,
        closingId: '',
      });
      persistUI();
    }
    return {
      ...initialWebSocketState(),
      ...ws.actions,
      data: emptyState(),
      loaded: false,
      error: '',
      runs: {},
      grpcRuns: {},
      nativeFilesAvailable: false,
      tabs: {},
      openIds: [],
      activeId: '',
      workspaceId: '',
      closingId: '',
      capabilities: { secretBackend: '', secretAvailable: false, message: '' },
      buildInfo: null,
      selectProjectFile: () => bridge.SelectProjectFile(),
      async saveProjectFile(workspaceId, includeBodies) {
        await queue;
        if (disposed) throw new Error('Приложение закрыто.');
        return bridge.SaveProjectFile(workspaceId, includeBodies);
      },
      async exportProject(workspaceId, includeBodies) {
        await queue;
        if (disposed) throw new Error('Приложение закрыто.');
        return bridge.ExportProject(workspaceId, includeBodies);
      },
      previewProjectImport: (format, data) => bridge.PreviewProjectImport(format, data),
      async importProject(format, data, includeBodies) {
        const navigation = navigationGeneration;
        let result: ProjectImportResult | undefined;
        const saved = await get().mutate(async (api) => {
          if (disposed) return get().data;
          result = await api.ImportProject(format, data, includeBodies);
          return result.state;
        }, 'Не удалось импортировать проект. Данные не изменены.');
        if (!saved || !result || disposed) return undefined;
        if (navigation !== navigationGeneration) return result;
        const importedWorkspaceId = result.workspaceId;
        get().switchWorkspace(importedWorkspaceId);
        const first = saved.requests.find((request) => request.workspaceId === importedWorkspaceId);
        if (first) get().openRequest(first.id);
        return result;
      },
      async importProtoSchema(input) {
        let result: ProtoImportResult | undefined;
        await get().mutate(async (api) => {
          result =
            'selectionId' in input
              ? await api.ImportNativeProtoSchema(input)
              : await api.ImportProtoSchema(input);
          return result.schemaId ? result.state : get().data;
        }, 'Не удалось импортировать схему Protobuf.');
        return result;
      },
      inspectProtoSchema: (workspaceId, schemaId) =>
        bridge.InspectProtoSchema(workspaceId, schemaId),
      inspectProtoMethod: (workspaceId, schemaId, method) =>
        bridge.InspectProtoMethod(workspaceId, schemaId, method),
      validateProtoMessage: (input) => bridge.ValidateProtoMessage(input),
      selectProtoImport: (kind) => bridge.SelectProtoImport(kind),
      selectProtoIncludeDirectory: () => bridge.SelectProtoIncludeDirectory(),
      initialize() {
        loadPromise ??= (async () => {
          try {
            void bridge
              .NativeFilesAvailable()
              .then((nativeFilesAvailable) => {
                if (!disposed) set({ nativeFilesAvailable });
              })
              .catch(() => {});
            void bridge
              .GetBuildInfo()
              .then((buildInfo) => {
                if (!disposed) set({ buildInfo });
              })
              .catch(() => {});
            void bridge
              .GetCapabilities()
              .then((capabilities) => {
                if (!disposed) set({ capabilities });
              })
              .catch(() => {
                if (!disposed)
                  set({
                    capabilities: {
                      secretAvailable: false,
                      secretBackend: '',
                      message: 'Не удалось проверить доступность системного хранилища секретов.',
                    },
                  });
              });
            const data = await bridge.LoadState();
            data.requests = data.requests.map((request) => ({
              ...request,
              http: request.http && normalizeHTTP(request.http),
              grpc: request.grpc && normalizeGRPC(request.grpc),
              websocket: request.websocket && normalizeWebSocket(request.websocket),
            }));
            if (disposed) return;
            const ids = new Set(data.requests.map((item) => item.id));
            const openIds = (data.ui.openRequestIds ?? []).filter((id) => ids.has(id));
            const tabs = Object.fromEntries(
              openIds.map((id) => {
                const request = data.requests.find((item) => item.id === id)!;
                return [id, { draft: request, saved: request, version: 0, saving: false }];
              }),
            );
            set({
              data,
              loaded: true,
              tabs,
              openIds,
              activeId: openIds.includes(data.ui.activeRequestId)
                ? data.ui.activeRequestId
                : (openIds[0] ?? ''),
              workspaceId: data.ui.workspaceId || data.workspaces[0]?.id || '',
            });
          } catch {
            if (!disposed)
              set({ loaded: true, error: 'Не удалось открыть локальное хранилище приложения.' });
          }
        })();
        return loadPromise;
      },
      mutate(action, safeError) {
        const operation = queue.then(async () => {
          try {
            const data = await action(bridge);
            if (disposed) return undefined;
            if (data.revision >= get().data.revision) {
              // Keep unsaved drafts independent of backend snapshots.
              const valid = new Set(data.requests.map((item) => item.id));
              const runs = { ...get().runs };
              const grpcRuns = { ...get().grpcRuns };
              const websocketRuns = { ...get().websocketRuns };
              for (const id of Object.keys(get().tabs))
                if (!valid.has(id)) {
                  clearTimeout(timers.get(id));
                  timers.delete(id);
                  releaseRun(id);
                  delete runs[id];
                  delete grpcRuns[id];
                  delete websocketRuns[id];
                }
              const openIds = get().openIds.filter((id) => valid.has(id));
              const tabs = Object.fromEntries(
                Object.entries(get().tabs)
                  .filter(([id]) => valid.has(id))
                  .map(([id, tab]) => [
                    id,
                    {
                      ...tab,
                      draft: withoutMissingProtoSchema(tab.draft, data),
                      saved: withoutMissingProtoSchema(tab.saved, data),
                    },
                  ]),
              );
              const workspaceId = data.workspaces.some((item) => item.id === get().workspaceId)
                ? get().workspaceId
                : (data.workspaces[0]?.id ?? '');
              if (workspaceId !== get().workspaceId) ws.resetWebSocketHistory();
              set({
                data,
                runs,
                grpcRuns,
                websocketRuns,
                openIds,
                tabs,
                activeId:
                  valid.has(get().activeId) &&
                  tabs[get().activeId]?.draft.workspaceId === workspaceId
                    ? get().activeId
                    : (openIds.findLast((id) => tabs[id]?.draft.workspaceId === workspaceId) ?? ''),
                workspaceId,
              });
            }
            return data;
          } catch (error) {
            if (!disposed)
              set({
                error:
                  safeError ??
                  (error instanceof Error
                    ? error.message.slice(0, 400)
                    : 'Изменения не сохранены. Проверьте доступ к хранилищу.'),
              });
            return undefined;
          }
        });
        queue = operation;
        return operation;
      },
      async sendHTTPRequest(id) {
        const tab = get().tabs[id];
        if (!tab?.draft.http || disposed) return;
        const snapshot = structuredClone(tab.draft);
        releaseRun(id);
        const generation = generations.get(id)!;
        set({ runs: { ...get().runs, [id]: { starting: true, execution: null, error: '' } } });
        try {
          const execution = await bridge.StartHTTPRequest(snapshot);
          if (!active(id, generation)) {
            await discardExecution(execution);
            return;
          }
          set({ runs: { ...get().runs, [id]: { starting: false, execution, error: '' } } });
          if (execution.state === 'running') poll(id, generation, execution.id);
        } catch (error) {
          if (active(id, generation))
            set({
              runs: {
                ...get().runs,
                [id]: {
                  starting: false,
                  execution: null,
                  error:
                    error instanceof Error
                      ? error.message.slice(0, 400)
                      : 'Не удалось отправить запрос.',
                },
              },
            });
        }
      },
      sendGRPCUnary: (id) => startGRPC(id, 'unary'),
      startGRPCStream: (id) => startGRPC(id, 'stream'),
      async sendGRPCRequest(id) {
        const run = get().grpcRuns[id];
        if (run?.kind === 'stream' && run.execution?.state === 'running')
          await get().sendGRPCStreamMessage(id);
        else await startGRPC(id, 'call');
      },
      async sendGRPCStreamMessage(id) {
        const run = get().grpcRuns[id];
        const execution = run?.execution;
        const grpc = get().tabs[id]?.draft.grpc;
        if (
          !grpc ||
          !execution ||
          execution.state !== 'running' ||
          execution.streamState !== 'open' ||
          !['client', 'bidi'].includes(execution.rpcType || '') ||
          run.sending ||
          run.halfClosing
        )
          return;
        const generation = grpcGenerations.get(id)!;
        const body = grpc.body,
          format = grpc.bodyFormat || 'json';
        set({ grpcRuns: { ...get().grpcRuns, [id]: { ...run, sending: true, error: '' } } });
        try {
          await bridge.SendGRPCStreamMessage(execution.id, body, format);
        } catch (error) {
          if (grpcActive(id, generation))
            set({
              grpcRuns: {
                ...get().grpcRuns,
                [id]: {
                  ...get().grpcRuns[id],
                  error: protoErrorMessage(error, 'Не удалось отправить сообщение gRPC.'),
                },
              },
            });
        } finally {
          if (grpcActive(id, generation))
            set({
              grpcRuns: { ...get().grpcRuns, [id]: { ...get().grpcRuns[id], sending: false } },
            });
        }
      },
      async halfCloseGRPCStream(id) {
        const run = get().grpcRuns[id],
          execution = run?.execution;
        if (
          !execution ||
          execution.state !== 'running' ||
          execution.streamState !== 'open' ||
          !['client', 'bidi'].includes(execution.rpcType || '') ||
          run.sending ||
          run.halfClosing
        )
          return;
        const generation = grpcGenerations.get(id)!;
        set({ grpcRuns: { ...get().grpcRuns, [id]: { ...run, halfClosing: true, error: '' } } });
        try {
          await bridge.HalfCloseGRPCStream(execution.id);
          if (grpcActive(id, generation)) {
            const current = get().grpcRuns[id];
            publishGRPCRun(id, {
              ...current,
              execution: current.execution && {
                ...current.execution,
                streamState: current.execution.state === 'running' ? 'half_closed' : 'closed',
              },
              halfClosing: false,
            });
          }
        } catch (error) {
          if (grpcActive(id, generation))
            set({
              grpcRuns: {
                ...get().grpcRuns,
                [id]: {
                  ...get().grpcRuns[id],
                  halfClosing: false,
                  error: protoErrorMessage(error, 'Не удалось завершить отправку gRPC.'),
                },
              },
            });
        }
      },
      discoverGRPCSchema: (id) => startGRPC(id, 'reflection'),
      async cancelGRPCExecution(id) {
        const run = get().grpcRuns[id];
        if (!run) return;
        if (run.starting) {
          releaseGRPCRun(id);
          set({
            grpcRuns: {
              ...get().grpcRuns,
              [id]: { ...run, starting: false, execution: null, error: 'Операция gRPC отменена.' },
            },
          });
          return;
        }
        if (run.execution?.state !== 'running') return;
        try {
          await bridge.CancelGRPCExecution(run.execution.id);
          // The stream poller owns its cursor and continues through the final
          // empty batch, including messages queued before cancellation.
          if (run.kind === 'stream') return;
          const execution = await bridge.GetGRPCExecution(run.execution.id);
          if (!disposed && get().grpcRuns[id]?.execution?.id === execution.id) {
            clearTimeout(grpcPolls.get(id));
            grpcPolls.delete(id);
            if (!publishGRPCRun(id, { ...run, starting: false, execution, error: '' })) return;
            if (execution.state === 'running') pollGRPC(id, grpcGenerations.get(id)!, execution.id);
            else await reflectionCompleted(id, grpcGenerations.get(id)!, execution);
          }
        } catch {
          if (!disposed) set({ error: 'Не удалось отменить операцию gRPC.' });
        }
      },
      async cancelHTTPRequest(id) {
        const run = get().runs[id];
        if (!run) return;
        if (run.starting) {
          releaseRun(id);
          set({
            runs: {
              ...get().runs,
              [id]: { starting: false, execution: null, error: 'Запрос отменён.' },
            },
          });
          return;
        }
        if (run.execution?.state !== 'running') return;
        try {
          await bridge.CancelHTTPExecution(run.execution.id);
          const execution = await bridge.GetHTTPExecution(run.execution.id);
          if (!disposed && get().runs[id]?.execution?.id === execution.id) {
            clearTimeout(polls.get(id));
            polls.delete(id);
            set({ runs: { ...get().runs, [id]: { starting: false, execution, error: '' } } });
            if (execution.state === 'running') poll(id, generations.get(id)!, execution.id);
          }
        } catch {
          if (!disposed) set({ error: 'Не удалось отменить запрос.' });
        }
      },
      async chooseHTTPFile(kind) {
        try {
          return await bridge.ChooseHTTPFile(kind);
        } catch {
          if (!disposed) set({ error: 'Не удалось открыть файл.' });
          return undefined;
        }
      },
      async saveHTTPResponse(id) {
        try {
          return await bridge.SaveHTTPResponse(id);
        } catch {
          if (!disposed) set({ error: 'Не удалось сохранить ответ.' });
          return false;
        }
      },
      openRequest(id) {
        const { data, tabs, openIds } = get();
        const request = data.requests.find((item) => item.id === id);
        if (!request) return;
        navigationGeneration++;
        if (request.workspaceId !== get().workspaceId) {
          const websocketRuns = { ...get().websocketRuns };
          for (const requestId of Object.keys(websocketRuns))
            if (get().tabs[requestId]?.draft.workspaceId !== request.workspaceId) {
              ws.releaseWebSocket(requestId);
              delete websocketRuns[requestId];
            }
          ws.resetWebSocketHistory();
          set({ websocketRuns });
        }
        set({
          tabs: tabs[id]
            ? tabs
            : { ...tabs, [id]: { draft: request, saved: request, version: 0, saving: false } },
          openIds: openIds.includes(id) ? openIds : [...openIds, id],
          activeId: id,
          workspaceId: request.workspaceId,
        });
        persistUI();
      },
      editRequest(id, draft) {
        const tab = get().tabs[id];
        if (!tab) return;
        set({ tabs: { ...get().tabs, [id]: { ...tab, draft, version: tab.version + 1 } } });
        clearTimeout(timers.get(id));
        timers.set(
          id,
          setTimeout(() => {
            timers.delete(id);
            if (!disposed) void get().saveRequest(id);
          }, autosaveMs),
        );
      },
      async saveRequest(id) {
        clearTimeout(timers.get(id));
        timers.delete(id);
        const tab = get().tabs[id];
        if (!tab) return true;
        if (!isDirty(tab)) return true;
        let snapshot = structuredClone(tab.draft);
        set({ tabs: { ...get().tabs, [id]: { ...tab, saving: true } } });
        const result = await get().mutate((api) => {
          // A delete/close may finish while this save waits behind another mutation.
          // Dropping that obsolete save also prevents it from recreating a deleted request.
          if (disposed || !get().tabs[id]) return Promise.resolve(get().data);
          // A schema can be deleted while this autosave waits behind another mutation.
          snapshot = withoutMissingProtoSchema(snapshot, get().data);
          return api.SaveRequest(persistentRequest(snapshot));
        });
        const current = get().tabs[id];
        if (current) {
          set({
            tabs: {
              ...get().tabs,
              [id]: {
                ...current,
                saved: result ? withoutMissingProtoSchema(snapshot, get().data) : current.saved,
                saving: false,
              },
            },
          });
        }
        return !!result;
      },
      requestClose(id) {
        const tab = get().tabs[id];
        if (!tab) return;
        if (isDirty(tab) || hasVolatileData(tab)) {
          clearTimeout(timers.get(id));
          timers.delete(id);
          set({ closingId: id });
        } else closeNow(id);
      },
      async resolveClose(choice) {
        const id = get().closingId;
        if (!id) return;
        if (choice === 'cancel') {
          set({ closingId: '' });
          const tab = get().tabs[id];
          if (tab && isDirty(tab)) get().editRequest(id, tab.draft);
          return;
        }
        if (choice === 'save') {
          const tab = get().tabs[id];
          if (!tab) return;
          if (hasVolatileBody(tab)) {
            const draft = {
              ...tab.draft,
              http: tab.draft.http && { ...tab.draft.http, persistBody: true },
              grpc: tab.draft.grpc && { ...tab.draft.grpc, persistBody: true },
              websocket: tab.draft.websocket && { ...tab.draft.websocket, persistBody: true },
            };
            get().editRequest(id, draft);
          }
          if (!(await get().saveRequest(id))) return;
          if (get().closingId !== id) return;
          // A newer edit during save needs a fresh confirmation.
          if (get().tabs[id] && isDirty(get().tabs[id])) return;
        }
        closeNow(id);
      },
      switchWorkspace(id) {
        navigationGeneration++;
        if (id !== get().workspaceId) {
          const websocketRuns = { ...get().websocketRuns };
          for (const requestId of Object.keys(websocketRuns))
            if (get().tabs[requestId]?.draft.workspaceId !== id) {
              ws.releaseWebSocket(requestId);
              delete websocketRuns[requestId];
            }
          ws.resetWebSocketHistory();
          set({ websocketRuns });
          const grpcRuns = { ...get().grpcRuns };
          for (const requestId of Object.keys(grpcRuns))
            if (get().tabs[requestId]?.draft.workspaceId !== id) {
              releaseGRPCRun(requestId);
              delete grpcRuns[requestId];
            }
          set({ grpcRuns });
        }
        const request = get().openIds.find(
          (requestId) => get().tabs[requestId].draft.workspaceId === id,
        );
        set({ workspaceId: id, activeId: request ?? '' });
        persistUI();
      },
      clearError: () => set({ error: '' }),
      dispose() {
        if (disposed) return;
        ws.disposeWebSockets();
        Object.keys(get().runs).forEach(releaseRun);
        Object.keys(get().grpcRuns).forEach(releaseGRPCRun);
        if (Object.keys(get().grpcRuns).length) set({ grpcRuns: {} });
        disposed = true;
        polls.forEach(clearTimeout);
        polls.clear();
        grpcPolls.forEach(clearTimeout);
        grpcPolls.clear();
        timers.forEach(clearTimeout);
        timers.clear();
      },
    };
  });
  return store;
}

export type RelayStore = ReturnType<typeof createAppStore>;
