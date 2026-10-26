export interface Workspace {
  id: string;
  name: string;
  activeEnvironmentId: string;
}
export interface Collection {
  auth?: HTTPAuth | null;
  id: string;
  workspaceId: string;
  name: string;
}
export interface Folder {
  auth?: HTTPAuth | null;
  id: string;
  workspaceId: string;
  collectionId: string;
  parentId: string;
  name: string;
}
export interface Header {
  name: string;
  value: string;
  enabled: boolean;
}
export interface HTTPDefinition {
  method: string;
  url: string;
  version: string;
  headers: Header[];
  body: string;
  persistBody: boolean;
  query: Header[];
  pathParams: Header[];
  bodyKind: string;
  form: Header[];
  multipart: HTTPPart[];
  fileId: string;
  auth: HTTPAuth;
  tls: HTTPTLS;
  proxy: { mode: string; url: string };
  timeoutMs: number;
  connectTimeoutMs: number;
  tlsHandshakeTimeoutMs: number;
  responseHeaderTimeoutMs: number;
  followRedirects: boolean;
  maxRedirects: number;
  decompress: boolean;
  maxDownloadBytes: number;
}
export interface HTTPPart extends Header {
  fileId: string;
  fileName: string;
  contentType: string;
}
export interface HTTPAuth {
  type: string;
  username: string;
  password: string;
  token: string;
  keyName: string;
  keyValue: string;
  in: string;
}
export interface HTTPTLS {
  caFileId: string;
  clientCertFileId: string;
  clientKeyFileId: string;
  serverName: string;
  insecure: boolean;
}
export interface SelectedFile {
  id: string;
  name: string;
  size: number;
}
export interface HTTPExecution {
  id: string;
  requestId: string;
  state: string;
  startedAt: string;
  finishedAt: string;
  url: string;
  protocol: string;
  statusCode: number;
  status: string;
  headers: Header[];
  trailers: Header[];
  bodyBase64: string;
  bodyText: string;
  binary: boolean;
  previewBytes: number;
  receivedBytes: number;
  truncated: boolean;
  downloadAvailable: boolean;
  error: string;
  warnings: string[];
  redirects: { url: string; statusCode: number }[];
  tls: {
    version: string;
    serverName: string;
    negotiatedProtocol: string;
    cipherSuite: string;
    verified: boolean;
    subject: string;
    issuer: string;
    dnsNames: string[];
    expiresAt: string;
  } | null;
  timing: {
    dnsMs: number;
    connectMs: number;
    tlsMs: number;
    firstByteMs: number;
    downloadMs: number;
    totalMs: number;
  };
}
export function normalizeHTTP(value: Partial<HTTPDefinition>): HTTPDefinition {
  return {
    url: '',
    body: '',
    persistBody: false,
    fileId: '',
    followRedirects: true,
    decompress: true,
    ...value,
    method: value.method || 'GET',
    version: value.version || 'auto',
    bodyKind: value.bodyKind || 'raw',
    timeoutMs: value.timeoutMs || 30000,
    connectTimeoutMs: value.connectTimeoutMs || 10000,
    tlsHandshakeTimeoutMs: value.tlsHandshakeTimeoutMs || 10000,
    responseHeaderTimeoutMs: value.responseHeaderTimeoutMs || 30000,
    maxRedirects: value.maxRedirects || 10,
    maxDownloadBytes: value.maxDownloadBytes || 256 * 1024 * 1024,
    headers: value.headers ?? [],
    query: value.query ?? [],
    pathParams: value.pathParams ?? [],
    form: value.form ?? [],
    multipart: value.multipart ?? [],
    auth: {
      username: '',
      password: '',
      token: '',
      keyName: '',
      keyValue: '',
      ...value.auth,
      type: value.auth?.type || 'none',
      in: value.auth?.in || 'header',
    },
    tls: {
      caFileId: '',
      clientCertFileId: '',
      clientKeyFileId: '',
      serverName: '',
      insecure: false,
      ...value.tls,
    },
    proxy: { url: '', ...value.proxy, mode: value.proxy?.mode || 'none' },
  };
}
export interface GRPCDefinition {
  schemaId?: string;
  bodyFormat?: string;
  endpoint: string;
  method: string;
  body: string;
  persistBody: boolean;
  plaintext?: boolean;
  tls?: HTTPTLS;
  authority?: string;
  metadata?: Header[];
  timeoutMs?: number;
  connectTimeoutMs?: number;
  maxSendBytes?: number;
  maxReceiveBytes?: number;
  compression?: string;
}
export function normalizeGRPC(value: GRPCDefinition): Required<GRPCDefinition> {
  return {
    ...value,
    schemaId: value.schemaId || '',
    bodyFormat: value.bodyFormat || 'json',
    plaintext: value.plaintext ?? false,
    authority: value.authority || '',
    metadata: value.metadata ?? [],
    timeoutMs: value.timeoutMs || 30000,
    connectTimeoutMs: value.connectTimeoutMs || 10000,
    maxSendBytes: value.maxSendBytes || 4 * 1024 * 1024,
    maxReceiveBytes: value.maxReceiveBytes || 4 * 1024 * 1024,
    compression: value.compression || 'none',
    tls: {
      caFileId: '',
      clientCertFileId: '',
      clientKeyFileId: '',
      serverName: '',
      insecure: false,
      ...value.tls,
    },
  };
}
export interface GRPCStatusDetail {
  typeURL: string;
  messageJSON: string;
  wireBase64: string;
  error: string;
}
export interface GRPCExecution {
  id: string;
  requestId: string;
  kind: string;
  state: string;
  startedAt: string;
  finishedAt: string;
  endpoint: string;
  method: string;
  schemaId: string;
  code: number;
  codeName: string;
  statusMessage: string;
  headers: Header[];
  trailers: Header[];
  messageJSON: string;
  wireBase64: string;
  wireBytes: number;
  details: GRPCStatusDetail[];
  error: string;
  warnings: string[];
  totalMs: number;
  connectMs: number;
  tls: HTTPExecution['tls'];
  reflectionProtocol: string;
  truncated: boolean;
  rpcType?: 'server' | 'client' | 'bidi' | '';
  streamState?: 'connecting' | 'open' | 'half_closed' | 'closed' | '';
  sentCount?: number;
  receivedCount?: number;
}
export interface GRPCStreamMessage {
  sequence: number;
  direction: 'sent' | 'received';
  index: number;
  timestamp: string;
  messageJSON: string;
  wireBase64: string;
  wireBytes: number;
  warnings: string[];
  truncated: boolean;
}
export interface GRPCStreamPoll {
  execution: GRPCExecution;
  messages: GRPCStreamMessage[];
  nextSequence: number;
}
export interface WebSocketDefinition {
  url: string;
  headers?: Header[];
  query?: Header[];
  subprotocols?: string[];
  tls?: HTTPTLS;
  proxy?: { mode: string; url: string };
  connectTimeoutMs?: number;
  writeTimeoutMs?: number;
  readTimeoutMs?: number;
  maxMessageBytes?: number;
  bodyKind?: string;
  bodyEncoding?: string;
  body?: string;
  persistBody?: boolean;
  fileId?: string;
}
export function normalizeWebSocket(value: WebSocketDefinition): Required<WebSocketDefinition> {
  return {
    ...value,
    headers: value.headers ?? [],
    query: value.query ?? [],
    subprotocols: value.subprotocols ?? [],
    tls: {
      caFileId: '',
      clientCertFileId: '',
      clientKeyFileId: '',
      serverName: '',
      insecure: false,
      ...value.tls,
    },
    proxy: { mode: 'none', url: '', ...value.proxy },
    connectTimeoutMs: value.connectTimeoutMs || 10000,
    writeTimeoutMs: value.writeTimeoutMs || 10000,
    readTimeoutMs: value.readTimeoutMs ?? 0,
    maxMessageBytes: value.maxMessageBytes || 1024 * 1024,
    bodyKind: value.bodyKind || 'text',
    bodyEncoding: value.bodyEncoding || 'hex',
    body: value.body || '',
    persistBody: value.persistBody ?? false,
    fileId: value.fileId || '',
  };
}
export interface WebSocketExecution {
  id: string;
  requestId: string;
  workspaceId: string;
  state: string;
  url: string;
  startedAt: string;
  finishedAt: string;
  handshakeStatusCode: number;
  handshakeHeaders: Header[];
  subprotocol: string;
  closeCode: number;
  closeReason: string;
  cleanClose: boolean;
  error: string;
  warnings: string[];
  sentCount: number;
  receivedCount: number;
  sentBytes: number;
  receivedBytes: number;
  droppedEvents: number;
  tls: HTTPExecution['tls'];
  connectMs: number;
  totalMs: number;
}
export interface WebSocketEvent {
  sequence: number;
  type: string;
  direction: string;
  messageType: string;
  timestamp: string;
  text: string;
  dataBase64: string;
  bytes: number;
  truncated: boolean;
  warnings: string[];
}
export interface WebSocketBatch {
  execution: WebSocketExecution;
  events: WebSocketEvent[];
  nextSequence: number;
}
export interface WebSocketSend {
  kind: string;
  encoding: string;
  data: string;
  fileId: string;
}
export interface WebSocketSavedSession {
  id: string;
  workspaceId: string;
  requestId: string;
  savedAt: string;
  includePayloads: boolean;
  omittedEvents: number;
  execution: WebSocketExecution;
  events: WebSocketEvent[];
}
export interface WebSocketSessionSummary {
  id: string;
  workspaceId: string;
  requestId: string;
  savedAt: string;
  url: string;
  state: string;
  closeCode: number;
  eventCount: number;
  includePayloads: boolean;
  omittedEvents: number;
}
export interface RequestDefinition {
  id: string;
  workspaceId: string;
  collectionId: string;
  folderId: string;
  name: string;
  protocol: string;
  http: HTTPDefinition | null;
  grpc: GRPCDefinition | null;
  websocket: WebSocketDefinition | null;
}
export interface Variable {
  id: string;
  name: string;
  value: string;
  secret: boolean;
  secretRef: string;
}
export interface Environment {
  id: string;
  workspaceId: string;
  name: string;
  variables: Variable[];
}
export interface Settings {
  theme: string;
  responsePreviewBytes: number;
  historyLimit: number;
}
export interface UIState {
  workspaceId: string;
  openRequestIds: string[];
  activeRequestId: string;
}
export interface State {
  revision: number;
  workspaces: Workspace[];
  collections: Collection[];
  folders: Folder[];
  requests: RequestDefinition[];
  environments: Environment[];
  protoSchemas: ProtoSchema[];
  settings: Settings;
  ui: UIState;
}
export interface Capabilities {
  secretBackend: string;
  secretAvailable: boolean;
  message: string;
}
export interface Bridge {
  GetBuildInfo(): Promise<BuildInfo>;
  SelectProjectFile(): Promise<SelectedProjectFile>;
  SaveProjectFile(workspaceId: string, includeBodies: boolean): Promise<ProjectFileExport>;
  ExportProject(workspaceId: string, includeBodies: boolean): Promise<ProjectExport>;
  PreviewProjectImport(format: ProjectFormat, data: string): Promise<ProjectPreview>;
  ImportProject(
    format: ProjectFormat,
    data: string,
    includeBodies: boolean,
  ): Promise<ProjectImportResult>;
  LoadState(): Promise<State>;
  SaveWorkspace(value: Workspace): Promise<State>;
  DeleteWorkspace(id: string): Promise<State>;
  SaveCollection(value: Collection): Promise<State>;
  DeleteCollection(id: string): Promise<State>;
  SaveFolder(value: Folder): Promise<State>;
  DeleteFolder(id: string): Promise<State>;
  SaveRequest(value: RequestDefinition): Promise<State>;
  DeleteRequest(id: string): Promise<State>;
  SaveEnvironment(value: Environment): Promise<State>;
  DeleteEnvironment(id: string): Promise<State>;
  SetSecret(environmentId: string, variableId: string, value: string): Promise<State>;
  DeleteSecret(environmentId: string, variableId: string): Promise<State>;
  SaveSettings(value: Settings): Promise<State>;
  SaveUI(value: UIState): Promise<State>;
  GetCapabilities(): Promise<Capabilities>;
  StartHTTPRequest(request: RequestDefinition): Promise<HTTPExecution>;
  GetHTTPExecution(id: string): Promise<HTTPExecution>;
  CancelHTTPExecution(id: string): Promise<void>;
  ForgetHTTPExecution(id: string): Promise<void>;
  StartGRPCUnary(request: RequestDefinition): Promise<GRPCExecution>;
  StartGRPCReflection(request: RequestDefinition): Promise<GRPCExecution>;
  StartGRPCStream(request: RequestDefinition): Promise<GRPCExecution>;
  SendGRPCStreamMessage(id: string, body: string, format: string): Promise<void>;
  HalfCloseGRPCStream(id: string): Promise<void>;
  PollGRPCStream(id: string, afterSequence: number): Promise<GRPCStreamPoll>;
  AckGRPCStream(id: string, sequence: number): Promise<void>;
  GetGRPCExecution(id: string): Promise<GRPCExecution>;
  CancelGRPCExecution(id: string): Promise<void>;
  ForgetGRPCExecution(id: string): Promise<void>;
  ConnectWebSocket(request: RequestDefinition): Promise<WebSocketExecution>;
  SendWebSocketMessage(id: string, message: WebSocketSend): Promise<void>;
  PingWebSocket(id: string): Promise<void>;
  DisconnectWebSocket(id: string, code: number, reason: string): Promise<void>;
  CancelWebSocket(id: string): Promise<void>;
  PollWebSocket(id: string, afterSequence: number): Promise<WebSocketBatch>;
  AckWebSocket(id: string, sequence: number): Promise<void>;
  ForgetWebSocket(id: string): Promise<void>;
  SaveWebSocketSession(id: string, includePayloads: boolean): Promise<WebSocketSavedSession>;
  ListWebSocketSessions(workspaceId: string): Promise<WebSocketSessionSummary[]>;
  GetSavedWebSocketSession(workspaceId: string, id: string): Promise<WebSocketSavedSession>;
  DeleteSavedWebSocketSession(workspaceId: string, id: string): Promise<void>;
  ChooseHTTPFile(kind: string): Promise<SelectedFile>;
  SaveHTTPResponse(id: string): Promise<boolean>;
  NativeFilesAvailable(): Promise<boolean>;
  ImportProtoSchema(input: ProtoImportInput): Promise<ProtoImportResult>;
  ImportNativeProtoSchema(input: NativeProtoImportInput): Promise<ProtoImportResult>;
  DeleteProtoSchema(workspaceId: string, schemaId: string): Promise<State>;
  InspectProtoSchema(workspaceId: string, schemaId: string): Promise<ProtoSchemaDetail>;
  InspectProtoMethod(
    workspaceId: string,
    schemaId: string,
    method: string,
  ): Promise<ProtoMethodDetail>;
  ValidateProtoMessage(input: ProtoValidationInput): Promise<ProtoValidationResult>;
  SelectProtoImport(kind: string): Promise<NativeProtoSelection>;
  SelectProtoIncludeDirectory(): Promise<SelectedFile>;
}

export interface BuildInfo {
  version: string;
  commit: string;
  sourceDateEpoch: string;
  unsigned: boolean;
  goos: string;
  goarch: string;
  goVersion: string;
}

export type ProjectFormat = 'native' | 'postman' | 'insomnia';
export interface ProjectWarning {
  code: string;
  path: string;
  message: string;
}
export interface ProjectStats {
  collections: number;
  folders: number;
  requests: number;
  environments: number;
  schemas: number;
  secretVariables: number;
}
export interface ProjectExport {
  filename: string;
  data: string;
  warnings: ProjectWarning[];
  stats: ProjectStats;
}
export interface ProjectPreview {
  format: string;
  workspaceName: string;
  warnings: ProjectWarning[];
  stats: ProjectStats;
}
export interface ProjectImportResult {
  state: State;
  workspaceId: string;
  warnings: ProjectWarning[];
  stats: ProjectStats;
}
export interface SelectedProjectFile {
  name: string;
  data: string;
}
export interface ProjectFileExport {
  saved: boolean;
  warnings: ProjectWarning[];
  stats: ProjectStats;
}

export interface ProtoSchema {
  id: string;
  workspaceId: string;
  name: string;
  kind: string;
  checksum: string;
  importedAt: string;
  rootFiles: string[];
  files: string[];
}
export interface ProtoDiagnostic {
  file: string;
  path: string;
  message: string;
  line: number;
  column: number;
}
export interface ProtoImportInput {
  id: string;
  workspaceId: string;
  name: string;
  kind: string;
  rootFiles: string[];
  sources: { name: string; content: string }[];
  descriptorSetBase64: string;
}
export interface ProtoImportResult {
  state: State;
  schemaId: string;
  diagnostics: ProtoDiagnostic[];
  warnings: string[];
}
export interface NativeProtoSelection {
  id: string;
  name: string;
  kind: string;
  files: string[];
  rootFiles: string[];
}
export interface NativeProtoImportInput {
  selectionId: string;
  includeIds: string[];
  id: string;
  workspaceId: string;
  name: string;
}
export interface ProtoMethod {
  fullMethod: string;
  service: string;
  name: string;
  input: string;
  output: string;
  clientStreaming: boolean;
  serverStreaming: boolean;
  comment: string;
}
export function rpcKind(method: ProtoMethod) {
  return method.clientStreaming
    ? method.serverStreaming
      ? 'Bidi streaming'
      : 'Client streaming'
    : method.serverStreaming
      ? 'Server streaming'
      : 'Unary';
}

// Schema commands return value-free application errors. Wails may wrap that
// message in Error.cause; never stringify a payload or a whole bridge error.
export function protoErrorMessage(error: unknown, fallback: string) {
  function message(value: unknown): string {
    if (typeof value === 'string') return value;
    if (
      value &&
      typeof value === 'object' &&
      'message' in value &&
      typeof value.message === 'string'
    )
      return value.message;
    return '';
  }
  const cause = error && typeof error === 'object' && 'cause' in error ? error.cause : undefined;
  return (message(cause).trim() || message(error).trim()).slice(0, 400) || fallback;
}
export interface ProtoField {
  name: string;
  jsonName: string;
  number: number;
  kind: string;
  type: string;
  repeated: boolean;
  required: boolean;
  presence: boolean;
  map: boolean;
  mapKey: string;
  mapValue: string;
  oneof: string;
  comment: string;
  enumValues: { name: string; number: number }[];
}
export interface ProtoMessage {
  name: string;
  file: string;
  comment: string;
  fields: ProtoField[];
}
export interface ProtoSchemaDetail {
  files: { name: string; package: string; syntax: string; imports: string[] }[];
  services: { name: string; file: string; comment: string; methods: ProtoMethod[] }[];
  messages: ProtoMessage[];
}
export interface ProtoMethodDetail {
  method: ProtoMethod;
  input: ProtoMessage;
  output: ProtoMessage;
  template: string;
}
export interface ProtoValidationInput {
  workspaceId: string;
  schemaId: string;
  method: string;
  body: string;
  format: string;
}
export interface ProtoValidationResult {
  valid: boolean;
  canonicalJSON: string;
  wireBase64: string;
  wireBytes: number;
  diagnostics: ProtoDiagnostic[];
  warnings: string[];
}

export const emptyState = (): State => ({
  revision: 0,
  workspaces: [],
  collections: [],
  folders: [],
  requests: [],
  environments: [],
  protoSchemas: [],
  settings: { theme: 'system', responsePreviewBytes: 5 * 1024 * 1024, historyLimit: 2000 },
  ui: { workspaceId: '', openRequestIds: [], activeRequestId: '' },
});

export function newRequest(
  workspaceId: string,
  collectionId: string,
  folderId = '',
  protocol = 'http',
): RequestDefinition {
  return {
    id: '',
    workspaceId,
    collectionId,
    folderId,
    name: 'Новый запрос',
    protocol,
    http: protocol === 'http' ? normalizeHTTP({}) : null,
    grpc:
      protocol === 'grpc'
        ? normalizeGRPC({
            endpoint: '',
            method: '',
            body: '',
            persistBody: false,
            schemaId: '',
            bodyFormat: 'json',
          })
        : null,
    websocket: protocol === 'websocket' ? normalizeWebSocket({ url: '' }) : null,
  };
}
