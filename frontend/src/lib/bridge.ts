import * as service from '../bindings/relay/internal/app/service';
import * as files from '../bindings/relay/desktop/fileservice';
import type {
  State as WireState,
  HTTPExecution as WireExecution,
  RequestDefinition as WireRequest,
  GRPCExecution as WireGRPCExecution,
  GRPCStreamBatch as WireGRPCStreamBatch,
  WebSocketExecution as WireWebSocketExecution,
  WebSocketEvent as WireWebSocketEvent,
  WebSocketBatch as WireWebSocketBatch,
  WebSocketSavedSession as WireWebSocketSavedSession,
  ProjectExport as WireProjectExport,
  ProjectPreview as WireProjectPreview,
  ProjectImportResult as WireProjectImportResult,
  ProjectFileExport as WireProjectFileExport,
} from '../bindings/relay/internal/domain/models';
import type {
  ProtoImportResult as WireProtoImportResult,
  ProtoSchemaDetails as WireProtoSchemaDetails,
  ProtoMethodDetails as WireProtoMethodDetails,
  ProtoValidationResult as WireProtoValidationResult,
} from '../bindings/relay/internal/app/models';
import type { NativeProtoSelection as WireNativeProtoSelection } from '../bindings/relay/desktop/models';
import type { MessageInfo as WireMessageInfo } from '../bindings/relay/internal/protobuf/registry/models';
import { normalizeHTTP, normalizeGRPC, normalizeWebSocket } from './types';
import type {
  Bridge,
  State,
  HTTPExecution,
  ProtoImportResult,
  ProtoSchemaDetail,
  RequestDefinition,
  GRPCExecution,
  GRPCStreamPoll,
  WebSocketExecution,
  WebSocketBatch,
  WebSocketSavedSession,
  WebSocketSessionSummary,
} from './types';

// Wails generates nullable slice types. The UI works with concrete lists.
export function normalizeState(value: WireState): State {
  return {
    ...value,
    workspaces: value.workspaces ?? [],
    collections: value.collections ?? [],
    folders: value.folders ?? [],
    requests: (value.requests ?? []).map((request) => ({
      ...request,
      http:
        request.http &&
        normalizeHTTP({
          ...request.http,
          headers: request.http.headers ?? [],
          query: request.http.query ?? [],
          pathParams: request.http.pathParams ?? [],
          form: request.http.form ?? [],
          multipart: request.http.multipart ?? [],
        }),
      grpc:
        request.grpc && normalizeGRPC({ ...request.grpc, metadata: request.grpc.metadata ?? [] }),
      websocket:
        request.websocket &&
        normalizeWebSocket({
          ...request.websocket,
          headers: request.websocket.headers ?? [],
          query: request.websocket.query ?? [],
          subprotocols: request.websocket.subprotocols ?? [],
        }),
    })),
    environments: (value.environments ?? []).map((environment) => ({
      ...environment,
      variables: environment.variables ?? [],
    })),
    protoSchemas: (value.protoSchemas ?? []).map((schema) => ({
      ...schema,
      files: schema.files ?? [],
      rootFiles: schema.rootFiles ?? [],
    })),
    ui: { ...value.ui, openRequestIds: value.ui.openRequestIds ?? [] },
  };
}

function normalizeProtoMessage(value: WireMessageInfo) {
  return {
    ...value,
    fields: (value.fields ?? []).map((field) => ({ ...field, enumValues: field.enumValues ?? [] })),
  };
}

function normalizeProtoDetails(value: WireProtoSchemaDetails): ProtoSchemaDetail {
  return {
    files: (value.files ?? []).map((file) => ({ ...file, imports: file.imports ?? [] })),
    services: (value.services ?? []).map((entry) => ({ ...entry, methods: entry.methods ?? [] })),
    messages: (value.messages ?? []).map(normalizeProtoMessage),
  };
}

function normalizeProtoImport(value: WireProtoImportResult): ProtoImportResult {
  return {
    ...value,
    state: normalizeState(value.state),
    diagnostics: value.diagnostics ?? [],
    warnings: value.warnings ?? [],
  };
}

function wireRequest(value: RequestDefinition): WireRequest {
  return {
    ...value,
    grpc: value.grpc && normalizeGRPC(value.grpc),
    websocket: value.websocket && normalizeWebSocket(value.websocket),
  };
}

function normalizeExecution(value: WireExecution): HTTPExecution {
  return {
    ...value,
    headers: value.headers ?? [],
    trailers: value.trailers ?? [],
    warnings: value.warnings ?? [],
    redirects: value.redirects ?? [],
    tls: value.tls && { ...value.tls, dnsNames: value.tls.dnsNames ?? [] },
  };
}

function normalizeGRPCExecution(value: WireGRPCExecution): GRPCExecution {
  return {
    ...value,
    rpcType:
      value.rpcType === 'server' || value.rpcType === 'client' || value.rpcType === 'bidi'
        ? value.rpcType
        : '',
    streamState:
      value.streamState === 'connecting' ||
      value.streamState === 'open' ||
      value.streamState === 'half_closed' ||
      value.streamState === 'closed'
        ? value.streamState
        : '',
    headers: value.headers ?? [],
    trailers: value.trailers ?? [],
    details: value.details ?? [],
    warnings: value.warnings ?? [],
    tls: value.tls && { ...value.tls, dnsNames: value.tls.dnsNames ?? [] },
  };
}

function normalizeGRPCStream(value: WireGRPCStreamBatch): GRPCStreamPoll {
  return {
    execution: normalizeGRPCExecution(value.execution),
    messages: (value.messages ?? []).map((message) => ({
      ...message,
      direction: message.direction === 'sent' ? 'sent' : 'received',
      warnings: message.warnings ?? [],
    })),
    nextSequence: value.nextSequence,
  };
}

function normalizeWebSocketExecution(value: WireWebSocketExecution): WebSocketExecution {
  return {
    ...value,
    handshakeHeaders: value.handshakeHeaders ?? [],
    warnings: value.warnings ?? [],
    tls: value.tls && { ...value.tls, dnsNames: value.tls.dnsNames ?? [] },
  };
}

function normalizeWebSocketEvent(value: WireWebSocketEvent) {
  return { ...value, warnings: value.warnings ?? [] };
}

function normalizeWebSocketBatch(value: WireWebSocketBatch): WebSocketBatch {
  return {
    ...value,
    execution: normalizeWebSocketExecution(value.execution),
    events: (value.events ?? []).map(normalizeWebSocketEvent),
  };
}

function normalizeSavedWebSocket(value: WireWebSocketSavedSession): WebSocketSavedSession {
  return {
    ...value,
    execution: normalizeWebSocketExecution(value.execution),
    events: (value.events ?? []).map(normalizeWebSocketEvent),
  };
}

export const desktopBridge: Bridge = {
  GetBuildInfo: () => service.GetBuildInfo(),
  ExportProject: (workspaceId, includeBodies) =>
    service
      .ExportProject(workspaceId, includeBodies)
      .then((value: WireProjectExport) => ({ ...value, warnings: value.warnings ?? [] })),
  PreviewProjectImport: (format, data) =>
    service
      .PreviewProjectImport(format, data)
      .then((value: WireProjectPreview) => ({ ...value, warnings: value.warnings ?? [] })),
  ImportProject: (format, data, includeBodies) =>
    service.ImportProject(format, data, includeBodies).then((value: WireProjectImportResult) => ({
      ...value,
      state: normalizeState(value.state),
      warnings: value.warnings ?? [],
    })),
  SelectProjectFile: () => files.SelectProjectFile(),
  SaveProjectFile: (workspaceId, includeBodies) =>
    files
      .SaveProjectFile(workspaceId, includeBodies)
      .then((value: WireProjectFileExport) => ({ ...value, warnings: value.warnings ?? [] })),
  ConnectWebSocket: (request) =>
    service.ConnectWebSocket(wireRequest(request)).then(normalizeWebSocketExecution),
  SendWebSocketMessage: (id, message) => service.SendWebSocketMessage(id, message),
  PingWebSocket: (id) => service.PingWebSocket(id),
  DisconnectWebSocket: (id, code, reason) => service.DisconnectWebSocket(id, code, reason),
  CancelWebSocket: (id) => service.CancelWebSocket(id),
  PollWebSocket: (id, sequence) =>
    service.PollWebSocket(id, sequence).then(normalizeWebSocketBatch),
  AckWebSocket: (id, sequence) => service.AckWebSocket(id, sequence),
  ForgetWebSocket: (id) => service.ForgetWebSocket(id),
  SaveWebSocketSession: (id, includePayloads) =>
    service.SaveWebSocketSession(id, includePayloads).then(normalizeSavedWebSocket),
  ListWebSocketSessions: (workspaceID) =>
    service
      .ListWebSocketSessions(workspaceID)
      .then((value: WebSocketSessionSummary[] | null) => value ?? []),
  GetSavedWebSocketSession: (workspaceID, id) =>
    service.GetSavedWebSocketSession(workspaceID, id).then(normalizeSavedWebSocket),
  DeleteSavedWebSocketSession: (workspaceID, id) =>
    service.DeleteSavedWebSocketSession(workspaceID, id),
  LoadState: () => service.LoadState().then(normalizeState),
  SaveWorkspace: (value) => service.SaveWorkspace(value).then(normalizeState),
  DeleteWorkspace: (id) => service.DeleteWorkspace(id).then(normalizeState),
  SaveCollection: (value) =>
    service.SaveCollection({ ...value, auth: value.auth ?? null }).then(normalizeState),
  DeleteCollection: (id) => service.DeleteCollection(id).then(normalizeState),
  SaveFolder: (value) =>
    service.SaveFolder({ ...value, auth: value.auth ?? null }).then(normalizeState),
  DeleteFolder: (id) => service.DeleteFolder(id).then(normalizeState),
  SaveRequest: (value) => service.SaveRequest(wireRequest(value)).then(normalizeState),
  DeleteRequest: (id) => service.DeleteRequest(id).then(normalizeState),
  SaveEnvironment: (value) => service.SaveEnvironment(value).then(normalizeState),
  DeleteEnvironment: (id) => service.DeleteEnvironment(id).then(normalizeState),
  SetSecret: (environmentId, variableId, value) =>
    service.SetSecret(environmentId, variableId, value).then(normalizeState),
  DeleteSecret: (environmentId, variableId) =>
    service.DeleteSecret(environmentId, variableId).then(normalizeState),
  SaveSettings: (value) => service.SaveSettings(value).then(normalizeState),
  SaveUI: (value) => service.SaveUI(value).then(normalizeState),
  GetCapabilities: () => service.GetCapabilities(),
  StartHTTPRequest: (value) =>
    service.StartHTTPRequest(wireRequest(value)).then(normalizeExecution),
  GetHTTPExecution: (id) => service.GetHTTPExecution(id).then(normalizeExecution),
  CancelHTTPExecution: (id) => service.CancelHTTPExecution(id),
  ForgetHTTPExecution: (id) => service.ForgetHTTPExecution(id),
  StartGRPCUnary: (value) =>
    service.StartGRPCUnary(wireRequest(value)).then(normalizeGRPCExecution),
  StartGRPCReflection: (value) =>
    service.StartGRPCReflection(wireRequest(value)).then(normalizeGRPCExecution),
  StartGRPCStream: (value) =>
    service.StartGRPCStream(wireRequest(value)).then(normalizeGRPCExecution),
  SendGRPCStreamMessage: (id, body, format) => service.SendGRPCStreamMessage(id, body, format),
  HalfCloseGRPCStream: (id) => service.HalfCloseGRPCStream(id),
  PollGRPCStream: (id, afterSequence) =>
    service.PollGRPCStream(id, afterSequence).then(normalizeGRPCStream),
  AckGRPCStream: (id, sequence) => service.AckGRPCStream(id, sequence),
  GetGRPCExecution: (id) => service.GetGRPCExecution(id).then(normalizeGRPCExecution),
  CancelGRPCExecution: (id) => service.CancelGRPCExecution(id),
  ForgetGRPCExecution: (id) => service.ForgetGRPCExecution(id),
  ChooseHTTPFile: (kind) => files.SelectHTTPFile(kind),
  SaveHTTPResponse: (id) => files.SaveHTTPResponse(id),
  NativeFilesAvailable: () => files.NativeFilesAvailable(),
  ImportProtoSchema: (input) => service.ImportProtoSchema(input).then(normalizeProtoImport),
  ImportNativeProtoSchema: (input) =>
    files.ImportNativeProtoSchema(input).then(normalizeProtoImport),
  DeleteProtoSchema: (workspaceId, schemaId) =>
    service.DeleteProtoSchema(workspaceId, schemaId).then(normalizeState),
  InspectProtoSchema: (workspaceId, schemaId) =>
    service.InspectProtoSchema(workspaceId, schemaId).then(normalizeProtoDetails),
  InspectProtoMethod: (workspaceId, schemaId, method) =>
    service
      .InspectProtoMethod(workspaceId, schemaId, method)
      .then((value: WireProtoMethodDetails) => ({
        ...value,
        input: normalizeProtoMessage(value.input),
        output: normalizeProtoMessage(value.output),
      })),
  ValidateProtoMessage: (input) =>
    service.ValidateProtoMessage(input).then((value: WireProtoValidationResult) => ({
      ...value,
      diagnostics: value.diagnostics ?? [],
      warnings: value.warnings ?? [],
    })),
  SelectProtoImport: (kind) =>
    files.SelectProtoImport(kind).then((value: WireNativeProtoSelection) => ({
      ...value,
      files: value.files ?? [],
      rootFiles: value.rootFiles ?? [],
    })),
  SelectProtoIncludeDirectory: () => files.SelectProtoIncludeDirectory(),
};
