import { test, expect, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { newRequest } from '../src/lib/types';
import type { State, ProtoImportResult } from '../src/lib/types';

const root = resolve(import.meta.dirname, '../..');
let directory: string, binary: string, port: number, httpTarget: string;
let desktop: ChildProcess | undefined;
const fixtures: ChildProcess[] = [];
let httpServer: Server;
let grpc: Record<string, string>, ws: Record<string, string>;
let profile = 'source',
  logs = '';
const received: { authorized: boolean; body: string }[] = [];
const canary = 'private-project-credential-canary-8fc42';

async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null) return;
  await new Promise<void>((done) => {
    const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
    child.once('exit', () => {
      clearTimeout(timer);
      done();
    });
    child.kill();
  });
}
async function start() {
  desktop = spawn(binary, [], {
    env: {
      ...process.env,
      RELAY_DATA_DIR: join(directory, profile),
      WAILS_SERVER_HOST: '127.0.0.1',
      WAILS_SERVER_PORT: String(port),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  for (const output of [desktop.stdout, desktop.stderr])
    output?.on('data', (chunk) => {
      logs += String(chunk);
    });
  await expect
    .poll(
      async () => {
        if (desktop?.exitCode !== null) throw new Error(`Desktop exited: ${logs}`);
        try {
          return (await fetch(`http://127.0.0.1:${port}/`)).status;
        } catch {
          return 0;
        }
      },
      { timeout: 15000 },
    )
    .toBe(200);
}
async function call<T>(page: Page, method: string, args: unknown[] = []): Promise<T> {
  return page.evaluate(
    async ({ method, args }) => {
      const runtime = '/wails/runtime.js';
      const { Call } = await import(runtime);
      return Call.ByName(`relay/internal/app.Service.${method}`, ...args);
    },
    { method, args },
  );
}
async function fixture(path: string) {
  const file = join(directory, path);
  execFileSync('go', ['build', '-o', file, `./internal/testutil/${path}`], {
    cwd: root,
    env: process.env,
    stdio: 'pipe',
    timeout: 120000,
  });
  const child = spawn(file, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
  fixtures.push(child);
  child.stderr?.on('data', (chunk) => {
    logs += String(chunk);
  });
  return new Promise<Record<string, string>>((done, reject) => {
    let text = '';
    const timer = setTimeout(() => reject(new Error('Fixture did not start')), 10000);
    child.once('exit', () => {
      clearTimeout(timer);
      reject(new Error('Fixture exited'));
    });
    child.stdout?.on('data', (chunk) => {
      text += String(chunk);
      if (!text.includes('\n')) return;
      clearTimeout(timer);
      try {
        done(JSON.parse(text.split('\n')[0]));
      } catch (error) {
        reject(error);
      }
    });
  });
}
test.beforeAll(async () => {
  directory = await mkdtemp(join(tmpdir(), 'relay-project-e2e-'));
  binary = join(directory, 'relay');
  execFileSync('go', ['build', '-tags', 'server', '-o', binary, '.'], {
    cwd: join(root, 'cmd/desktop'),
    env: { ...process.env, CGO_ENABLED: '0' },
    stdio: 'pipe',
    timeout: 120000,
  });
  grpc = await fixture('grpcfixture');
  ws = await fixture('wsfixture');
  httpServer = createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += String(chunk);
    received.push({ authorized: request.headers.authorization === `Bearer ${canary}`, body });
    response.setHeader('Content-Type', 'application/json');
    response.end(JSON.stringify({ restored: true, body }));
  });
  await new Promise<void>((done) => httpServer.listen(0, '127.0.0.1', done));
  const address = httpServer.address();
  if (!address || typeof address === 'string') throw new Error('No HTTP fixture port');
  httpTarget = `http://127.0.0.1:${address.port}`;
  const reservation = createServer();
  await new Promise<void>((done) => reservation.listen(0, '127.0.0.1', done));
  const appAddress = reservation.address();
  if (!appAddress || typeof appAddress === 'string') throw new Error('No application port');
  port = appAddress.port;
  await new Promise<void>((done) => reservation.close(() => done()));
  await start();
});
test.afterAll(async () => {
  await stop(desktop);
  await Promise.all(fixtures.map(stop));
  if (httpServer) await new Promise<void>((done) => httpServer.close(() => done()));
  if (directory) await rm(directory, { recursive: true, force: true });
});

async function transferDialog(page: Page) {
  await page.getByRole('button', { name: 'Импорт / экспорт', exact: true }).click();
  return page.getByRole('dialog', { name: 'Импорт / экспорт проекта', exact: true });
}

test('native project imports into a clean profile and restores HTTP, offline gRPC and WebSocket without credentials', async ({
  page,
}) => {
  test.setTimeout(120000);
  const errors: string[] = [],
    browserTargets: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const targetHosts = [new URL(httpTarget).host, new URL(ws.ws).host, grpc.local];
  page.on('request', (request) => {
    if (targetHosts.some((host) => request.url().includes(host)))
      browserTargets.push(request.url());
  });
  page.on('websocket', (socket) => {
    if (targetHosts.some((host) => socket.url().includes(host))) browserTargets.push(socket.url());
  });
  await page.goto(`http://127.0.0.1:${port}/`);
  let state = await call<State>(page, 'LoadState');
  const sourceWorkspace = state.workspaces[0],
    collection = state.collections[0];
  await call(page, 'SaveWorkspace', [{ ...sourceWorkspace, name: 'Portable Gate' }]);
  const environment = state.environments[0];
  await call(page, 'SaveEnvironment', [
    {
      ...environment,
      variables: [
        { ...environment.variables[0], value: httpTarget },
        { id: '', name: 'token', value: canary, secret: false, secretRef: '' },
      ],
    },
  ]);
  const proto = await call<ProtoImportResult>(page, 'ImportProtoSchema', [
    {
      id: '',
      workspaceId: sourceWorkspace.id,
      name: 'Offline Gate',
      kind: 'descriptor',
      sources: [],
      rootFiles: [],
      descriptorSetBase64: (
        await readFile(join(root, 'testdata/descriptors/editor.protoset'))
      ).toString('base64'),
    },
  ]);
  expect(proto.schemaId).not.toBe('');
  const http = newRequest(sourceWorkspace.id, collection.id, '', 'http');
  http.id = state.requests[0].id;
  http.name = 'Gate REST';
  http.http = {
    ...http.http!,
    method: 'POST',
    url: '{{baseUrl}}/echo',
    body: '{"message":"portable REST"}',
    bodyKind: 'json',
    persistBody: true,
    auth: { ...http.http!.auth, type: 'bearer', token: '{{token}}' },
  };
  await call(page, 'SaveRequest', [http]);
  const rpc = newRequest(sourceWorkspace.id, collection.id, '', 'grpc');
  rpc.id = '';
  rpc.name = 'Gate gRPC';
  rpc.grpc = {
    ...rpc.grpc!,
    endpoint: grpc.local,
    method: '/relay.descriptor.v1.DescriptorService/Echo',
    schemaId: proto.schemaId,
    plaintext: true,
    body: '{"text":"portable gRPC","sequence":"9223372036854775807"}',
    persistBody: true,
    metadata: [{ name: 'authorization', value: 'Bearer {{token}}', enabled: true }],
  };
  await call(page, 'SaveRequest', [rpc]);
  const socket = newRequest(sourceWorkspace.id, collection.id, '', 'websocket');
  socket.id = '';
  socket.name = 'Gate WebSocket';
  socket.websocket = {
    ...socket.websocket!,
    url: ws.ws,
    subprotocols: ['echo.v1'],
    headers: [{ name: 'Authorization', value: 'Bearer {{token}}', enabled: true }],
    body: 'portable WebSocket',
    persistBody: true,
  };
  await call(page, 'SaveRequest', [socket]);
  await page.reload();
  let dialog = await transferDialog(page);
  await dialog.getByRole('tab', { name: 'Экспорт', exact: true }).click();
  await dialog.getByRole('button', { name: 'Подготовить экспорт', exact: true }).click();
  await expect(dialog.getByLabel('Предпросмотр экспорта')).toBeVisible();
  const downloadDefault = page.waitForEvent('download');
  await dialog.getByRole('button', { name: 'Скачать JSON', exact: true }).click();
  const defaultFile = await downloadDefault;
  const defaultJSON = await readFile((await defaultFile.path())!, 'utf8');
  expect(defaultJSON).not.toContain(canary);
  expect(defaultJSON).not.toContain('portable REST');
  expect(defaultJSON).not.toContain('relay/v1/');
  await dialog.getByLabel('Включить тела запросов в экспорт').check();
  await dialog.getByRole('button', { name: 'Подготовить экспорт', exact: true }).click();
  await expect(dialog.getByRole('button', { name: 'Скачать JSON', exact: true })).toBeEnabled();
  const downloadWithBodies = page.waitForEvent('download');
  await dialog.getByRole('button', { name: 'Скачать JSON', exact: true }).click();
  const portable = await readFile((await (await downloadWithBodies).path())!, 'utf8');
  expect(portable).not.toContain(canary);
  expect(portable).toContain('portable REST');
  expect(portable).toContain('descriptorSetBase64');
  await dialog.getByRole('button', { name: 'Закрыть диалог', exact: true }).click();
  await stop(desktop);
  profile = 'clean';
  await start();
  await page.reload();
  const clean = await call<State>(page, 'LoadState');
  expect(clean.workspaces).toHaveLength(1);
  dialog = await transferDialog(page);
  await dialog.getByLabel('JSON проекта', { exact: true }).fill(portable);
  await dialog.getByRole('button', { name: 'Проверить импорт', exact: true }).click();
  await expect(dialog.getByLabel('Предпросмотр импорта')).toContainText('Portable Gate');
  expect((await call<State>(page, 'LoadState')).workspaces).toHaveLength(1);
  await dialog.getByLabel('Сохранить тела запросов из файла').check();
  await dialog
    .getByRole('button', { name: 'Импортировать в новое пространство', exact: true })
    .click();
  await expect(dialog.getByRole('status')).toContainText('Проект импортирован');
  await dialog.getByRole('button', { name: 'Закрыть диалог', exact: true }).click();
  state = await call<State>(page, 'LoadState');
  const importedWorkspace = state.workspaces.find((entry) => entry.name === 'Portable Gate')!;
  expect(importedWorkspace.id).not.toBe(sourceWorkspace.id);
  expect(state.workspaces.some((entry) => entry.id === clean.workspaces[0].id)).toBe(true);
  const imported = state.requests.filter((entry) => entry.workspaceId === importedWorkspace.id);
  expect(imported).toHaveLength(3);
  expect(imported.every((entry) => entry.id !== http.id)).toBe(true);
  expect(
    state.environments
      .find((entry) => entry.workspaceId === importedWorkspace.id)
      ?.variables.find((entry) => entry.name === 'token'),
  ).toMatchObject({ secret: true, value: '', secretRef: '' });
  expect(imported.find((entry) => entry.grpc)?.grpc?.schemaId).not.toBe(proto.schemaId);
  const options = page.getByRole('tablist', { name: 'Параметры запроса', exact: true });
  await options.getByRole('tab', { name: 'Auth', exact: true }).click();
  await page.getByLabel('Bearer token', { exact: true }).fill(canary);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByLabel('Тело ответа', { exact: true })).toContainText('portable REST');
  expect(received.some((entry) => entry.authorized && entry.body.includes('portable REST'))).toBe(
    true,
  );
  await page.locator('.request-row .tree-name').filter({ hasText: 'Gate gRPC' }).click();
  const rpcTabs = page.getByRole('tablist', { name: 'Параметры gRPC', exact: true });
  await rpcTabs.getByRole('tab', { name: /^Metadata/ }).click();
  await page.getByLabel('Значение gRPC metadata 1', { exact: true }).fill(`Bearer ${canary}`);
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(page.getByLabel('Сообщение ответа gRPC', { exact: true })).toContainText(
    'portable gRPC',
  );
  await expect(page.getByLabel('Сообщение ответа gRPC', { exact: true })).toContainText(
    '9223372036854775807',
  );
  await page.locator('.request-row .tree-name').filter({ hasText: 'Gate WebSocket' }).click();
  const wsTabs = page.getByRole('tablist', { name: 'Параметры WebSocket', exact: true });
  await wsTabs.getByRole('tab', { name: 'Заголовки', exact: true }).click();
  await page.getByLabel('Значение заголовок WebSocket 1', { exact: true }).fill(`Bearer ${canary}`);
  await wsTabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
  const wsResult = page.getByLabel('Результат WebSocket', { exact: true });
  await wsResult.getByRole('button', { name: /^← #\d+ received text/ }).click();
  await expect(wsResult.getByLabel('Сообщение WebSocket', { exact: true })).toContainText(
    'portable WebSocket',
  );
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click();
  await expect(wsResult).toContainText('closed');
  await page.keyboard.press('Control+k');
  await expect(page.getByLabel('Поиск запросов', { exact: true })).toBeFocused();
  await page.getByLabel('Поиск запросов', { exact: true }).fill('Gate REST');
  await expect(
    page.locator('.request-row .tree-name').filter({ hasText: 'Gate gRPC' }),
  ).toBeHidden();
  await page.getByLabel('Поиск запросов', { exact: true }).clear();
  await page.getByRole('button', { name: 'История запросов', exact: true }).click();
  const history = page.getByRole('dialog', { name: 'История запросов', exact: true });
  await history.getByLabel('Протокол истории').selectOption('grpc');
  await expect(history).toContainText('Gate gRPC');
  await expect(history).not.toContainText('Gate REST');
  await history.getByRole('button', { name: 'Закрыть диалог', exact: true }).click();
  await page.reload();
  expect((await call<State>(page, 'LoadState')).protoSchemas).toHaveLength(1);
  for (const file of await readdir(join(directory, 'clean')))
    if (file.startsWith('relay.db'))
      expect((await readFile(join(directory, 'clean', file))).includes(Buffer.from(canary))).toBe(
        false,
      );
  expect(browserTargets).toEqual([]);
  expect(errors).toEqual([]);
  expect(logs).not.toContain(canary);
});

test('Postman and Insomnia previews report unsupported features and append only after apply', async ({
  page,
}) => {
  await page.goto(`http://127.0.0.1:${port}/`);
  for (const [format, file, expected] of [
    ['postman', 'postman-v2.1.json', 'Postman'],
    ['insomnia', 'insomnia-v4.json', 'Insomnia'],
  ]) {
    const files = await readdir(join(root, 'testdata/project'));
    const selected =
      files.find((entry) => entry === file) ||
      files.find((entry) => entry.startsWith(format) && entry.endsWith('.json'));
    if (!selected) throw new Error(`Missing version-specific ${format} fixture`);
    const data = await readFile(join(root, 'testdata/project', selected), 'utf8');
    const before = await call<State>(page, 'LoadState');
    const dialog = await transferDialog(page);
    await dialog.getByLabel('Формат импорта проекта').selectOption(format);
    await dialog
      .getByLabel('Файл проекта')
      .setInputFiles({ name: file, mimeType: 'application/json', buffer: Buffer.from(data) });
    await dialog.getByRole('button', { name: 'Проверить импорт', exact: true }).click();
    await expect(dialog.getByLabel('Предпросмотр импорта')).toBeVisible();
    await expect(dialog).toContainText('Предупреждения');
    expect((await call<State>(page, 'LoadState')).revision).toBe(before.revision);
    await dialog
      .getByRole('button', { name: 'Импортировать в новое пространство', exact: true })
      .click();
    await expect(dialog.getByRole('status')).toContainText('Проект импортирован');
    const after = await call<State>(page, 'LoadState');
    expect(after.workspaces).toHaveLength(before.workspaces.length + 1);
    expect(after.requests.length).toBeGreaterThan(before.requests.length);
    expect(after.workspaces.some((entry) => entry.name.includes(expected))).toBe(true);
    await dialog.getByRole('button', { name: 'Закрыть диалог', exact: true }).click();
  }
  const before = await call<State>(page, 'LoadState');
  const dialog = await transferDialog(page);
  await dialog.getByLabel('Формат импорта проекта').selectOption('insomnia');
  await dialog
    .getByLabel('JSON проекта')
    .fill('{"_type":"export","__export_format":5,"resources":[]}');
  await dialog.getByRole('button', { name: 'Проверить импорт', exact: true }).click();
  await expect(dialog.getByRole('alert')).toBeVisible();
  await expect(
    dialog.getByRole('button', { name: 'Импортировать в новое пространство', exact: true }),
  ).toBeDisabled();
  expect((await call<State>(page, 'LoadState')).revision).toBe(before.revision);
});
