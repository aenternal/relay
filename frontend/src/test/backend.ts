import { vi } from 'vitest';
import { emptyState, newRequest } from '../lib/types';
import type {
  Bridge,
  ProjectExport,
  ProjectPreview,
  ProjectImportResult,
  Environment,
  HTTPExecution,
  GRPCExecution,
  GRPCStreamMessage,
  State,
  UIState,
  ProtoSchema,
  ProtoSchemaDetail,
  ProtoMethodDetail,
  ProtoImportInput,
  ProtoImportResult,
  NativeProtoImportInput,
  ProtoValidationResult,
  WebSocketExecution,
  WebSocketEvent,
  WebSocketSavedSession,
} from '../lib/types';

export function fixtureProtoSchema(): ProtoSchema {
  return {
    id: 'schema1',
    workspaceId: 'w1',
    name: 'Catalog API',
    kind: 'proto',
    checksum: 'abc123',
    importedAt: '2026-10-24T10:00:00Z',
    rootFiles: ['catalog.proto'],
    files: ['catalog.proto', 'types.proto'],
  };
}

export function fixtureProtoDetail(): ProtoSchemaDetail {
  return {
    files: [
      {
        name: 'catalog.proto',
        package: 'example.catalog.v1',
        syntax: 'proto3',
        imports: ['types.proto'],
      },
    ],
    services: [
      {
        name: 'example.catalog.v1.Catalog',
        file: 'catalog.proto',
        comment: 'Catalog service',
        methods: [
          {
            fullMethod: '/example.catalog.v1.Catalog/Get',
            service: 'example.catalog.v1.Catalog',
            name: 'Get',
            input: 'example.catalog.v1.GetRequest',
            output: 'example.catalog.v1.GetResponse',
            clientStreaming: false,
            serverStreaming: false,
            comment: 'Fetch one item.',
          },
          {
            fullMethod: '/example.catalog.v1.Catalog/Watch',
            service: 'example.catalog.v1.Catalog',
            name: 'Watch',
            input: 'example.catalog.v1.GetRequest',
            output: 'example.catalog.v1.GetResponse',
            clientStreaming: false,
            serverStreaming: true,
            comment: '',
          },
        ],
      },
    ],
    messages: [
      {
        name: 'example.catalog.v1.GetRequest',
        file: 'types.proto',
        comment: 'Item key',
        fields: [
          {
            name: 'item_id',
            jsonName: 'itemId',
            number: 1,
            kind: 'int64',
            type: 'int64',
            repeated: false,
            required: false,
            presence: false,
            map: false,
            mapKey: '',
            mapValue: '',
            oneof: '',
            comment: 'Keep the exact identifier.',
            enumValues: [],
          },
        ],
      },
      { name: 'example.catalog.v1.GetResponse', file: 'types.proto', comment: '', fields: [] },
    ],
  };
}

export function fixtureProtoMethod(): ProtoMethodDetail {
  const detail = fixtureProtoDetail();
  return {
    method: detail.services[0].methods[0],
    input: detail.messages[0],
    output: detail.messages[1],
    template: '{\n  "itemId": "0"\n}',
  };
}

export function fixtureExecution(
  requestId = 'r1',
  overrides: Partial<HTTPExecution> = {},
): HTTPExecution {
  return {
    id: `execution-${requestId}`,
    requestId,
    state: 'completed',
    startedAt: '',
    finishedAt: '',
    url: 'https://example.test/response',
    protocol: 'HTTP/2.0',
    statusCode: 200,
    status: 'OK',
    headers: [{ name: 'Content-Type', value: 'application/json', enabled: true }],
    trailers: [],
    bodyBase64: btoa('{"ok":true}'),
    bodyText: '{"ok":true}',
    binary: false,
    previewBytes: 11,
    receivedBytes: 11,
    truncated: false,
    downloadAvailable: true,
    error: '',
    warnings: [],
    redirects: [],
    tls: null,
    timing: { dnsMs: 1, connectMs: 2, tlsMs: 3, firstByteMs: 4, downloadMs: 1, totalMs: 10 },
    ...overrides,
  };
}

export function fixtureState(): State {
  const data = emptyState();
  data.workspaces = [{ id: 'w1', name: 'Локальный проект', activeEnvironmentId: '' }];
  data.collections = [{ id: 'c1', workspaceId: 'w1', name: 'Проверки API' }];
  data.requests = [{ ...newRequest('w1', 'c1'), id: 'r1', name: 'Проверка сервиса' }];
  data.ui.workspaceId = 'w1';
  return data;
}
export function fixtureGRPCExecution(
  requestId = 'g1',
  overrides: Partial<GRPCExecution> = {},
): GRPCExecution {
  return {
    id: `grpc-${requestId}`,
    requestId,
    kind: 'unary',
    state: 'completed',
    startedAt: '2026-10-25T10:00:00Z',
    finishedAt: '2026-10-25T10:00:01Z',
    endpoint: 'localhost:50051',
    method: '/example.catalog.v1.Catalog/Get',
    schemaId: 'schema1',
    code: 0,
    codeName: 'OK',
    statusMessage: '',
    headers: [],
    trailers: [],
    messageJSON: '{"itemId":"9223372036854775807"}',
    wireBase64: 'CP//////////fw==',
    wireBytes: 10,
    details: [],
    error: '',
    warnings: [],
    totalMs: 12.5,
    connectMs: 2.1,
    tls: null,
    reflectionProtocol: '',
    truncated: false,
    ...overrides,
  };
}

export function fixtureGRPCStreamMessage(
  sequence = 1,
  overrides: Partial<GRPCStreamMessage> = {},
): GRPCStreamMessage {
  return {
    sequence,
    direction: 'received',
    index: sequence,
    timestamp: '2026-10-26T10:00:00Z',
    messageJSON: `{"sequence":"${sequence}"}`,
    wireBase64: 'CAE=',
    wireBytes: 2,
    warnings: [],
    truncated: false,
    ...overrides,
  };
}

export function fixtureWebSocketExecution(
  requestId = 'ws1',
  overrides: Partial<WebSocketExecution> = {},
): WebSocketExecution {
  return {
    id: `ws-${requestId}`,
    requestId,
    workspaceId: 'w1',
    state: 'open',
    url: 'ws://localhost:8080/echo',
    startedAt: '',
    finishedAt: '',
    handshakeStatusCode: 101,
    handshakeHeaders: [],
    subprotocol: '',
    closeCode: 0,
    closeReason: '',
    cleanClose: false,
    error: '',
    warnings: [],
    sentCount: 0,
    receivedCount: 0,
    sentBytes: 0,
    receivedBytes: 0,
    droppedEvents: 0,
    tls: null,
    connectMs: 2,
    totalMs: 2,
    ...overrides,
  };
}
export function fixtureWebSocketEvent(
  sequence = 1,
  overrides: Partial<WebSocketEvent> = {},
): WebSocketEvent {
  return {
    sequence,
    type: 'received',
    direction: 'in',
    messageType: 'text',
    timestamp: '2026-10-25T12:00:00Z',
    text: `hello ${sequence}`,
    dataBase64: btoa(`hello ${sequence}`),
    bytes: 7,
    truncated: false,
    warnings: [],
    ...overrides,
  };
}

export function testBackend(initial = fixtureState()) {
  const data = structuredClone(initial);
  let counter = 10;
  const secrets = new Map<string, string>();
  function commit() {
    data.revision++;
    return structuredClone(data);
  }
  function upsert<T extends { id: string }>(items: T[], item: T) {
    const saved = structuredClone({ ...item, id: item.id || `id${++counter}` });
    return [...items.filter((entry) => entry.id !== saved.id), saved];
  }
  function remove(id: string) {
    data.requests = data.requests.filter((item) => item.id !== id);
    return commit();
  }
  const executions = new Map<string, HTTPExecution>();
  const grpcExecutions = new Map<string, GRPCExecution>();
  const streamMessages = new Map<string, GRPCStreamMessage[]>();
  const wsExecutions = new Map<string, WebSocketExecution>();
  const wsEvents = new Map<string, WebSocketEvent[]>();
  const wsSessions = new Map<string, WebSocketSavedSession>();
  const api = {
    GetBuildInfo: vi.fn(async () => ({
      version: 'dev',
      commit: 'unknown',
      sourceDateEpoch: '0',
      unsigned: true,
      goos: 'linux',
      goarch: 'amd64',
      goVersion: 'go1.26.10',
    })),
    ConnectWebSocket: vi.fn(async (request) => {
      const execution = fixtureWebSocketExecution(request.id, { url: request.websocket!.url });
      wsExecutions.set(execution.id, execution);
      wsEvents.set(execution.id, []);
      return structuredClone(execution);
    }),
    SendWebSocketMessage: vi.fn(async (id, message) => {
      const events = wsEvents.get(id)!;
      events.push(
        fixtureWebSocketEvent(events.length + 1, {
          type: 'sent',
          direction: 'out',
          messageType: message.kind,
          text: message.data,
        }),
        fixtureWebSocketEvent(events.length + 2, { messageType: message.kind, text: message.data }),
      );
      const execution = wsExecutions.get(id)!;
      execution.sentCount++;
      execution.receivedCount++;
    }),
    PingWebSocket: vi.fn(async (id) => {
      const events = wsEvents.get(id)!;
      events.push(
        fixtureWebSocketEvent(events.length + 1, { type: 'ping', direction: 'out' }),
        fixtureWebSocketEvent(events.length + 2, { type: 'pong' }),
      );
    }),
    DisconnectWebSocket: vi.fn(async (id, code, reason) => {
      Object.assign(wsExecutions.get(id)!, {
        state: 'closed',
        closeCode: code,
        closeReason: reason,
        cleanClose: true,
      });
      const events = wsEvents.get(id)!;
      events.push(
        fixtureWebSocketEvent(events.length + 1, {
          type: 'close',
          direction: 'local',
          text: reason,
        }),
      );
    }),
    CancelWebSocket: vi.fn(async (id) => {
      Object.assign(wsExecutions.get(id) ?? {}, { state: 'closed', cleanClose: false });
    }),
    PollWebSocket: vi.fn(async (id, afterSequence) => ({
      execution: structuredClone(wsExecutions.get(id)!),
      events: structuredClone(
        (wsEvents.get(id) || []).filter((event) => event.sequence > afterSequence),
      ),
      nextSequence: wsEvents.get(id)?.at(-1)?.sequence || 0,
    })),
    AckWebSocket: vi.fn(async (_id, _sequence) => {
      void _id;
      void _sequence;
    }),
    ForgetWebSocket: vi.fn(async (id) => {
      wsExecutions.delete(id);
      wsEvents.delete(id);
    }),
    SaveWebSocketSession: vi.fn(async (id, includePayloads) => {
      const execution = wsExecutions.get(id)!;
      const session: WebSocketSavedSession = {
        id: `saved-${id}`,
        requestId: execution.requestId,
        workspaceId: execution.workspaceId,
        savedAt: '2026-10-25T12:00:00Z',
        includePayloads,
        omittedEvents: 0,
        execution: structuredClone(execution),
        events: (wsEvents.get(id) || []).map((event) => ({
          ...event,
          text: includePayloads ? event.text : '',
          dataBase64: includePayloads ? event.dataBase64 : '',
        })),
      };
      wsSessions.set(session.id, session);
      return structuredClone(session);
    }),
    ListWebSocketSessions: vi.fn(async (workspaceId) =>
      [...wsSessions.values()]
        .filter((entry) => entry.workspaceId === workspaceId)
        .map((entry) => ({
          id: entry.id,
          workspaceId: entry.workspaceId,
          requestId: entry.requestId,
          savedAt: entry.savedAt,
          url: entry.execution.url,
          state: entry.execution.state,
          closeCode: entry.execution.closeCode,
          eventCount: entry.events.length,
          includePayloads: entry.includePayloads,
          omittedEvents: entry.omittedEvents,
        })),
    ),
    GetSavedWebSocketSession: vi.fn(async (_workspaceId, id) =>
      structuredClone(wsSessions.get(id)!),
    ),
    DeleteSavedWebSocketSession: vi.fn(async (_workspaceId, id) => {
      wsSessions.delete(id);
    }),
    StartGRPCStream: vi.fn(async (request) => {
      const execution = fixtureGRPCExecution(request.id, {
        kind: 'stream',
        state: 'running',
        rpcType: 'bidi',
        streamState: 'open',
        sentCount: 0,
        receivedCount: 0,
        messageJSON: '',
        wireBase64: '',
        wireBytes: 0,
      });
      grpcExecutions.set(execution.id, execution);
      streamMessages.set(execution.id, []);
      return structuredClone(execution);
    }),
    SendGRPCStreamMessage: vi.fn(async (id: string, body: string, _format: string) => {
      void _format;
      const execution = grpcExecutions.get(id)!;
      const messages = streamMessages.get(id)!;
      execution.sentCount = (execution.sentCount || 0) + 1;
      execution.receivedCount = (execution.receivedCount || 0) + 1;
      messages.push(
        fixtureGRPCStreamMessage(messages.length + 1, {
          direction: 'sent',
          index: execution.sentCount,
          messageJSON: body,
        }),
      );
      messages.push(
        fixtureGRPCStreamMessage(messages.length + 1, {
          index: execution.receivedCount,
          messageJSON: body,
        }),
      );
    }),
    HalfCloseGRPCStream: vi.fn(async (id: string) => {
      const execution = grpcExecutions.get(id)!;
      grpcExecutions.set(id, { ...execution, streamState: 'closed', state: 'completed' });
    }),
    PollGRPCStream: vi.fn(async (id: string, afterSequence: number) => {
      const messages = (streamMessages.get(id) || []).filter(
        (message) => message.sequence > afterSequence,
      );
      return {
        execution: structuredClone(grpcExecutions.get(id)!),
        messages: structuredClone(messages),
        nextSequence: messages.at(-1)?.sequence || afterSequence,
      };
    }),
    AckGRPCStream: vi.fn(async (_id: string, _sequence: number) => {
      void _id;
      void _sequence;
    }),
    StartGRPCUnary: vi.fn(async (request) => {
      const execution = fixtureGRPCExecution(request.id);
      grpcExecutions.set(execution.id, execution);
      return structuredClone(execution);
    }),
    StartGRPCReflection: vi.fn(async (request) => {
      const schema = {
        ...fixtureProtoSchema(),
        id: `reflection${++counter}`,
        kind: 'descriptor',
        name: 'Server reflection',
        workspaceId: request.workspaceId,
      };
      data.protoSchemas = upsert(data.protoSchemas, schema);
      commit();
      const execution = fixtureGRPCExecution(request.id, {
        kind: 'reflection',
        schemaId: schema.id,
        reflectionProtocol: 'grpc.reflection.v1',
      });
      grpcExecutions.set(execution.id, execution);
      return structuredClone(execution);
    }),
    GetGRPCExecution: vi.fn(async (id) =>
      structuredClone(grpcExecutions.get(id) ?? fixtureGRPCExecution()),
    ),
    CancelGRPCExecution: vi.fn(async (id) => {
      const execution = grpcExecutions.get(id);
      if (execution)
        grpcExecutions.set(id, { ...execution, state: 'canceled', code: 1, codeName: 'Canceled' });
    }),
    ForgetGRPCExecution: vi.fn(async (id) => {
      grpcExecutions.delete(id);
    }),
    ImportProtoSchema: vi.fn(async (input: ProtoImportInput): Promise<ProtoImportResult> => {
      const schema: ProtoSchema = {
        ...fixtureProtoSchema(),
        id: input.id || `schema${++counter}`,
        workspaceId: input.workspaceId,
        name: input.name,
        kind: input.kind,
        files: input.sources.map((source) => source.name),
        rootFiles: input.rootFiles,
      };
      data.protoSchemas = upsert(data.protoSchemas ?? [], schema);
      return { state: commit(), schemaId: schema.id, diagnostics: [], warnings: [] };
    }),
    ImportNativeProtoSchema: vi.fn(
      async (input: NativeProtoImportInput): Promise<ProtoImportResult> => {
        const schema = {
          ...fixtureProtoSchema(),
          id: input.id || `schema${++counter}`,
          workspaceId: input.workspaceId,
          name: input.name,
        };
        data.protoSchemas = upsert(data.protoSchemas ?? [], schema);
        return { state: commit(), schemaId: schema.id, diagnostics: [], warnings: [] };
      },
    ),
    DeleteProtoSchema: vi.fn(async (_workspaceId, schemaId) => {
      data.protoSchemas = data.protoSchemas.filter((schema) => schema.id !== schemaId);
      data.requests = data.requests.map((request) =>
        request.grpc && request.grpc.schemaId === schemaId
          ? { ...request, grpc: { ...request.grpc, schemaId: '' } }
          : request,
      );
      return commit();
    }),
    InspectProtoSchema: vi.fn(async () => fixtureProtoDetail()),
    InspectProtoMethod: vi.fn(async () => fixtureProtoMethod()),
    ValidateProtoMessage: vi.fn(
      async (): Promise<ProtoValidationResult> => ({
        valid: true,
        canonicalJSON: '{\n  "itemId": "9223372036854775807"\n}',
        wireBase64: 'CP//////////fw==',
        wireBytes: 10,
        diagnostics: [],
        warnings: [],
      }),
    ),
    SelectProtoImport: vi.fn(async () => ({
      id: 'selection1',
      name: 'catalog.proto',
      kind: 'proto',
      files: ['catalog.proto'],
      rootFiles: ['catalog.proto'],
    })),
    SelectProtoIncludeDirectory: vi.fn(async () => ({ id: 'include1', name: 'shared', size: 0 })),
    NativeFilesAvailable: vi.fn(async () => false),
    ChooseHTTPFile: vi.fn(async () => ({ id: 'file1', name: 'body.bin', size: 16 })),
    SaveHTTPResponse: vi.fn(async () => true),
    StartHTTPRequest: vi.fn(async (request) => {
      const execution = fixtureExecution(request.id);
      executions.set(execution.id, execution);
      return structuredClone(execution);
    }),
    GetHTTPExecution: vi.fn(async (id) =>
      structuredClone(executions.get(id) ?? fixtureExecution()),
    ),
    CancelHTTPExecution: vi.fn(async (id) => {
      const execution = executions.get(id);
      if (execution) executions.set(id, { ...execution, state: 'canceled' });
    }),
    ForgetHTTPExecution: vi.fn(async (id) => {
      executions.delete(id);
    }),
    LoadState: vi.fn(async () => structuredClone(data)),
    GetCapabilities: vi.fn(async () => ({
      secretBackend: 'test-vault',
      secretAvailable: true,
      message: '',
    })),
    SaveWorkspace: vi.fn(async (item) => {
      data.workspaces = upsert(data.workspaces, item);
      return commit();
    }),
    DeleteWorkspace: vi.fn(async (id) => {
      data.workspaces = data.workspaces.filter((item) => item.id !== id);
      data.requests = data.requests.filter((item) => item.workspaceId !== id);
      return commit();
    }),
    SaveCollection: vi.fn(async (item) => {
      data.collections = upsert(data.collections, item);
      return commit();
    }),
    DeleteCollection: vi.fn(async (id) => {
      data.collections = data.collections.filter((item) => item.id !== id);
      data.requests = data.requests.filter((item) => item.collectionId !== id);
      return commit();
    }),
    SaveFolder: vi.fn(async (item) => {
      data.folders = upsert(data.folders, item);
      return commit();
    }),
    DeleteFolder: vi.fn(async (id) => {
      data.folders = data.folders.filter((item) => item.id !== id);
      return commit();
    }),
    SaveRequest: vi.fn(async (item) => {
      data.requests = upsert(data.requests, item);
      return commit();
    }),
    DeleteRequest: vi.fn(async (id) => remove(id)),
    SaveEnvironment: vi.fn(async (item: Environment) => {
      const environment = {
        ...item,
        id: item.id || `id${++counter}`,
        variables: item.variables.map((variable) => ({
          ...variable,
          id: variable.id || `id${++counter}`,
        })),
      };
      data.environments = upsert(data.environments, environment);
      return commit();
    }),
    DeleteEnvironment: vi.fn(async (id) => {
      data.environments = data.environments.filter((item) => item.id !== id);
      return commit();
    }),
    SetSecret: vi.fn(async (environmentId, variableId, value) => {
      const ref = `vault/${environmentId}/${variableId}`;
      secrets.set(ref, value);
      data.environments = data.environments.map((item) =>
        item.id !== environmentId
          ? item
          : {
              ...item,
              variables: item.variables.map((variable) =>
                variable.id !== variableId ? variable : { ...variable, value: '', secretRef: ref },
              ),
            },
      );
      return commit();
    }),
    DeleteSecret: vi.fn(async (environmentId, variableId) => {
      data.environments = data.environments.map((item) =>
        item.id !== environmentId
          ? item
          : {
              ...item,
              variables: item.variables.map((variable) =>
                variable.id !== variableId ? variable : { ...variable, value: '', secretRef: '' },
              ),
            },
      );
      return commit();
    }),
    SaveSettings: vi.fn(async (settings) => {
      data.settings = settings;
      return commit();
    }),
    SaveUI: vi.fn(async (ui: UIState) => {
      const valid = data.requests
        .filter((item) => item.workspaceId === ui.workspaceId)
        .map((item) => item.id);
      if (
        ui.openRequestIds.some((id) => !valid.includes(id)) ||
        (ui.activeRequestId && !valid.includes(ui.activeRequestId))
      )
        throw new Error('Cross-workspace UI state');
      data.ui = structuredClone(ui);
      return commit();
    }),
    ExportProject: vi.fn(async (): Promise<ProjectExport> => {
      throw new Error('Project export is not configured by this test');
    }),
    SelectProjectFile: vi.fn(async () => ({ name: '', data: '' })),
    SaveProjectFile: vi.fn(async () => ({
      saved: false,
      warnings: [],
      stats: {
        collections: 0,
        folders: 0,
        requests: 0,
        environments: 0,
        schemas: 0,
        secretVariables: 0,
      },
    })),
    PreviewProjectImport: vi.fn(async (): Promise<ProjectPreview> => {
      throw new Error('Project preview is not configured by this test');
    }),
    ImportProject: vi.fn(async (): Promise<ProjectImportResult> => {
      throw new Error('Project import is not configured by this test');
    }),
  } satisfies Bridge;
  return { api, state: () => structuredClone(data), secrets };
}
