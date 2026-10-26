import { test, expect, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createServer } from 'node:net';
import { createServer as createHTTPServer, type Server } from 'node:http';
import { gzipSync } from 'node:zlib';
import type { State } from '../src/lib/types';

let directory: string;
let binary: string;
let process: ChildProcess | undefined;
let port: number;
let logs = '';
let fixture: Server;
let target: string;
let canceledRequests = 0;
const received: { url: string; version: string; authorization: string; body: string }[] = [];
const root = resolve(import.meta.dirname, '../..');

test('desktop shell blocks foreign bridge origins, DNS rebinding and inline scripts', async ({
  page,
  request,
}) => {
  const origin = `http://127.0.0.1:${port}`;
  const response = await request.get(origin);
  expect(response.headers()['content-security-policy']).toContain("script-src 'self'");
  expect(response.headers()['content-security-policy']).toContain("connect-src 'self'");
  expect(response.headers()['x-content-type-options']).toBe('nosniff');
  expect(response.headers()['referrer-policy']).toBe('no-referrer');
  expect((await request.get(origin, { headers: { Host: 'rebind.invalid' } })).status()).toBe(403);
  expect(
    (
      await request.post(`${origin}/wails/runtime`, {
        headers: { Origin: 'https://foreign.invalid' },
        data: '{}',
      })
    ).status(),
  ).toBe(403);
  await page.goto(origin);
  await expect(page.getByRole('button', { name: 'Импорт / экспорт', exact: true })).toBeVisible();
  await page.evaluate(() => {
    const scope = window as unknown as {
      relayUnsafeScript?: boolean;
      relayPolicyViolations?: string[];
    };
    scope.relayPolicyViolations = [];
    document.addEventListener('securitypolicyviolation', (event) =>
      scope.relayPolicyViolations!.push(event.violatedDirective),
    );
    const script = document.createElement('script');
    script.textContent = 'window.relayUnsafeScript = true';
    document.body.append(script);
  });
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (window as unknown as { relayPolicyViolations: string[] }).relayPolicyViolations.length,
      ),
    )
    .toBeGreaterThan(0);
  expect(
    await page.evaluate(
      () => (window as unknown as { relayUnsafeScript?: boolean }).relayUnsafeScript,
    ),
  ).toBeUndefined();
});

async function availablePort() {
  const socket = createServer();
  await new Promise<void>((resolve) => socket.listen(0, '127.0.0.1', resolve));
  const address = socket.address();
  if (!address || typeof address === 'string') throw new Error('No test port');
  await new Promise<void>((resolve) => socket.close(() => resolve()));
  return address.port;
}

async function start() {
  process = spawn(binary, [], {
    env: {
      ...globalThis.process.env,
      RELAY_DATA_DIR: join(directory, 'data'),
      WAILS_SERVER_HOST: '127.0.0.1',
      WAILS_SERVER_PORT: String(port),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  process.stdout?.on('data', (data) => {
    logs += String(data);
  });
  process.stderr?.on('data', (data) => {
    logs += String(data);
  });
  await expect
    .poll(
      async () => {
        if (process?.exitCode !== null) throw new Error(`Desktop exited: ${logs}`);
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

async function stop() {
  const current = process;
  process = undefined;
  if (!current || current.exitCode !== null) return;
  await new Promise<void>((resolve) => {
    current.once('exit', () => resolve());
    current.kill();
  });
}

async function state(page: Page): Promise<State> {
  return page.evaluate(async () => {
    const path = '/wails/runtime.js';
    const { Call } = await import(path);
    return Call.ByName('relay/internal/app.Service.LoadState');
  });
}

async function createNamed(page: Page, name: string) {
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Название', { exact: true }).fill(name);
  await dialog.getByRole('button', { name: 'Создать', exact: true }).click();
  await expect(dialog).toBeHidden();
}

test.beforeAll(async () => {
  directory = await mkdtemp(join(tmpdir(), 'relay-desktop-e2e-'));
  binary = join(directory, globalThis.process.platform === 'win32' ? 'relay.exe' : 'relay');
  execFileSync('go', ['build', '-tags', 'server', '-o', binary, '.'], {
    cwd: join(root, 'cmd/desktop'),
    env: { ...globalThis.process.env, CGO_ENABLED: '0' },
    stdio: 'pipe',
    timeout: 120000,
  });
  port = await availablePort();
  fixture = createHTTPServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    const body = Buffer.concat(chunks).toString();
    const pathname = new URL(request.url ?? '/', 'http://fixture').pathname;
    received.push({
      url: request.url ?? '',
      version: request.httpVersion,
      authorization: request.headers.authorization ?? '',
      body,
    });
    if (pathname === '/slow') {
      response.writeHead(200, { 'Content-Type': 'text/plain' });
      response.write('waiting');
      const timer = setTimeout(() => response.end('late'), 30000);
      response.on('close', () => {
        clearTimeout(timer);
        canceledRequests++;
      });
    } else if (pathname === '/redirect') {
      response.writeHead(302, { Location: '/echo?redirected=yes' }).end();
    } else if (pathname === '/big') {
      response.writeHead(200, { 'Content-Type': 'text/plain' });
      response.end('bounded preview '.repeat(140000));
    } else if (pathname === '/html') {
      response.writeHead(200, { 'Content-Type': 'text/html' });
      response.end('<script>window.__relayInjected = true</script><h1>untrusted response</h1>');
    } else if (pathname === '/gzip') {
      response.writeHead(200, { 'Content-Type': 'application/json', 'Content-Encoding': 'gzip' });
      response.end(gzipSync('{"compressed":true}'));
    } else {
      response.writeHead(200, {
        'Content-Type': 'application/json',
        'X-Repeat': ['first', 'second'],
        Trailer: 'X-Final',
      });
      response.write(
        JSON.stringify({
          method: request.method,
          url: request.url,
          version: request.httpVersion,
          authorization: request.headers.authorization,
          body,
        }),
      );
      response.addTrailers({ 'X-Final': 'complete' });
      response.end();
    }
  });
  await new Promise<void>((resolve) => fixture.listen(0, '127.0.0.1', resolve));
  const address = fixture.address();
  if (!address || typeof address === 'string') throw new Error('No HTTP fixture port');
  target = `http://127.0.0.1:${address.port}`;
  await start();
});

test.afterAll(async () => {
  await stop();
  fixture?.closeAllConnections();
  if (fixture) await new Promise<void>((resolve) => fixture.close(() => resolve()));
  if (directory) await rm(directory, { recursive: true, force: true });
});

test('real Wails bridge persists collections, folders, requests, environments, settings and tabs across a process restart', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${port}/`);
  await expect(page.getByLabel('Имя запроса')).toHaveValue('Новый запрос');

  await page.getByLabel('Действия рабочего пространства').click();
  await page.getByRole('button', { name: 'Создать пространство', exact: true }).click();
  await createNamed(page, 'Persistence workspace');
  await expect(page.getByLabel('Рабочее пространство', { exact: true })).toHaveValue(/.+/);
  await expect
    .poll(async () => {
      const current = await state(page);
      return current.workspaces.find((item) => item.id === current.ui.workspaceId)?.name;
    })
    .toBe('Persistence workspace');

  await page.getByLabel('Создать коллекцию', { exact: true }).click();
  await createNamed(page, 'Catalog');
  await page.getByLabel('Действия коллекции Catalog').click();
  await page.getByRole('button', { name: 'Новая папка', exact: true }).click();
  await createNamed(page, 'v1');
  await page.getByLabel('Действия папки v1').click();
  await page
    .getByLabel('Дерево коллекций')
    .getByRole('button', { name: 'Новый запрос', exact: true })
    .click();
  await createNamed(page, 'Get items');

  await page.getByLabel('URL запроса').fill('{{baseUrl}}/items?expand=true');
  await page.getByLabel('HTTP метод').fill('POST');
  await page.getByLabel('Сохранять тело на диске').check();
  await page
    .getByRole('textbox', { name: 'Тело запроса', exact: true })
    .fill('{"filter":"active"}');
  await expect
    .poll(async () => {
      const request = (await state(page)).requests.find((item) => item.name === 'Get items');
      return request?.http?.body;
    })
    .toBe('{"filter":"active"}');

  await page.getByRole('button', { name: 'Окружения', exact: true }).click();
  await page.getByRole('button', { name: '+ Окружение', exact: true }).click();
  const environmentDialog = page.getByRole('dialog');
  await environmentDialog.getByLabel('Название окружения').fill('Staging');
  await environmentDialog.getByRole('button', { name: '+ Переменная', exact: true }).click();
  await environmentDialog.getByLabel('Имя переменной 1', { exact: true }).fill('baseUrl');
  await environmentDialog
    .getByLabel('Значение переменной 1', { exact: true })
    .fill('https://staging.example.test');
  await environmentDialog.getByRole('button', { name: '+ Переменная', exact: true }).click();
  await environmentDialog.getByLabel('Имя переменной 2', { exact: true }).fill('token');
  await environmentDialog.getByLabel('Тип переменной 2', { exact: true }).selectOption('secret');
  await expect(
    environmentDialog.getByLabel('Значение переменной 2', { exact: true }),
  ).toHaveAttribute('type', 'password');
  await environmentDialog.getByRole('button', { name: 'Сохранить окружение', exact: true }).click();
  await expect(environmentDialog).toBeHidden();
  await page.getByRole('button', { name: 'Активировать', exact: true }).click();

  await page.getByRole('button', { name: 'Настройки', exact: true }).click();
  await page.getByLabel('Тема интерфейса').selectOption('dark');
  await page.getByLabel('Размер превью ответа, байт').fill('1048576');
  await page.getByLabel('Количество событий в истории').fill('500');
  await page.getByRole('button', { name: 'Сохранить настройки', exact: true }).click();
  await expect(page.getByText('Настройки сохранены', { exact: true })).toBeVisible();
  const before = await state(page);

  await page.close();
  await stop();
  await start();
  const restored = await page.context().newPage();
  restored.on('pageerror', (error) => errors.push(error.message));
  await restored.goto(`http://127.0.0.1:${port}/`);
  await expect(restored.getByLabel('Имя запроса')).toHaveValue('Get items');
  await expect(restored.getByLabel('URL запроса')).toHaveValue('{{baseUrl}}/items?expand=true');
  await expect(restored.getByLabel('HTTP метод')).toHaveValue('POST');
  await expect(restored.getByRole('textbox', { name: 'Тело запроса', exact: true })).toHaveText(
    '{"filter":"active"}',
  );
  await expect(restored.locator('html')).toHaveAttribute('data-theme', 'dark');
  await restored.screenshot({ path: join(tmpdir(), 'relay-stage1.png'), fullPage: true });
  await expect.poll(async () => (await state(restored)).settings).toEqual(before.settings);
  const after = await state(restored);
  expect(after.workspaces).toEqual(before.workspaces);
  expect(after.collections).toEqual(before.collections);
  expect(after.folders).toEqual(before.folders);
  expect(after.requests).toEqual(before.requests);
  expect(after.environments).toEqual(before.environments);
  expect(after.ui).toEqual(before.ui);

  // An unapproved body stays in its tab and is absent from both the bridge
  // snapshot and ordinary SQLite, including while an autosave runs.
  const canary = 'do-not-persist-body-canary-1d60';
  await restored.getByLabel('Сохранять тело на диске').uncheck();
  await restored.getByRole('textbox', { name: 'Тело запроса', exact: true }).fill(canary);
  await expect
    .poll(
      async () =>
        (await state(restored)).requests.find((item) => item.name === 'Get items')?.http?.body,
    )
    .toBe('');
  await restored.getByLabel('Закрыть вкладку Get items').click();
  await expect(restored.getByRole('dialog', { name: 'Сохранить изменения?' })).toBeVisible();
  await restored.getByRole('button', { name: 'Отмена', exact: true }).click();
  await expect(restored.getByRole('textbox', { name: 'Тело запроса', exact: true })).toHaveText(
    canary,
  );
  await restored.getByLabel('Закрыть вкладку Get items').click();
  await restored.getByRole('button', { name: 'Не сохранять', exact: true }).click();
  await expect(restored.getByRole('dialog')).toBeHidden();
  for (const filename of await readdir(join(directory, 'data'))) {
    expect((await readFile(join(directory, 'data', filename))).includes(Buffer.from(canary))).toBe(
      false,
    );
  }
  expect(logs).not.toContain(canary);
  await restored.getByLabel('Действия папки v1').click();
  await restored
    .getByLabel('Дерево коллекций')
    .getByRole('button', { name: 'Удалить', exact: true })
    .click();
  await restored.getByRole('dialog').getByRole('button', { name: 'Удалить', exact: true }).click();
  await expect
    .poll(async () => {
      const current = await state(restored);
      return (
        current.folders.length + current.requests.filter((item) => item.name === 'Get items').length
      );
    })
    .toBe(0);
  expect(errors).toEqual([]);
  await restored.screenshot({ path: join(directory, 'desktop.png'), fullPage: true });
});

test('HTTP UI uses Go transport for JSON5, redirects, compression, bounded previews and cancellation', async ({
  page,
}) => {
  const browserErrors: string[] = [];
  const browserTargetRequests: string[] = [];
  page.on('pageerror', (error) => browserErrors.push(error.message));
  page.on('request', (request) => {
    if (request.url().startsWith(target)) browserTargetRequests.push(request.url());
  });
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.getByRole('button', { name: 'Новый запрос', exact: true }).first().click();
  await createNamed(page, 'HTTP echo');
  await page.getByLabel('URL запроса').fill(`${target}/echo`);
  await page.getByLabel('HTTP версия').selectOption('1.1');
  await page.getByLabel('HTTP метод').fill('POST');
  await page.getByLabel('Тип тела запроса').selectOption('json5');
  await page
    .getByRole('textbox', { name: 'Тело запроса', exact: true })
    .fill("{message: 'hello', sequence: 9223372036854775807,}");

  const options = page.getByRole('tablist', { name: 'Параметры запроса' });
  await options.getByRole('tab', { name: 'Params', exact: true }).click();
  for (const [index, value] of ['first', 'second'].entries()) {
    await page.getByRole('button', { name: '+ Query параметр', exact: true }).click();
    await page.getByLabel(`Имя query параметр ${index + 1}`, { exact: true }).fill('tag');
    await page.getByLabel(`Значение query параметр ${index + 1}`, { exact: true }).fill(value);
  }
  await options.getByRole('tab', { name: 'Auth', exact: true }).click();
  await page.getByRole('combobox', { name: 'Тип авторизации', exact: true }).selectOption('bearer');
  const token = 'transient-e2e-token-9e1c';
  await page.getByLabel('Bearer token', { exact: true }).fill(token);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  const result = page.getByLabel('Результат запроса', { exact: true });
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('200 OK');
  await expect(result).toContainText('HTTP/1.1');
  await expect(page.getByLabel('Тело ответа', { exact: true })).toContainText('hello');
  await expect
    .poll(() => received.find((item) => item.authorization === `Bearer ${token}`)?.url)
    .toBe('/echo?tag=first&tag=second');
  expect(received.find((item) => item.authorization === `Bearer ${token}`)?.body).toContain(
    '9223372036854775807',
  );
  await expect(page.getByLabel('Тело ответа', { exact: true })).not.toContainText(token);
  await result.getByRole('tab', { name: /^Trailers/ }).click();
  await expect(result).toContainText('X-Final');
  await expect(result).toContainText('complete');
  await result.getByRole('tab', { name: 'Тело', exact: true }).click();
  await page.screenshot({ path: join(tmpdir(), 'relay-stage2.png'), fullPage: true });

  await options.getByRole('tab', { name: 'Auth', exact: true }).click();
  await page.getByRole('combobox', { name: 'Тип авторизации', exact: true }).selectOption('none');
  await options.getByRole('tab', { name: 'Опции', exact: true }).click();
  await page.getByLabel('Следовать редиректам', { exact: true }).check();
  await page.getByLabel('Распаковывать gzip / deflate', { exact: true }).check();
  await page.getByLabel('URL запроса').fill(`${target}/redirect`);
  await page.getByLabel('HTTP метод').fill('GET');
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await result.getByRole('tab', { name: /^Редиректы/ }).click();
  await expect(result).toContainText('302');
  await expect(result).toContainText('/echo?redirected=');

  await page.getByLabel('URL запроса').fill(`${target}/gzip`);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await result.getByRole('tab', { name: 'Тело', exact: true }).click();
  await expect(page.getByLabel('Тело ответа', { exact: true })).toContainText('"compressed": true');
  await result.getByRole('tab', { name: /^Headers/ }).click();
  await expect(result).toContainText('Content-Encoding');
  await expect(result).toContainText('gzip');

  await page.getByLabel('URL запроса').fill(`${target}/html`);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await result.getByRole('tab', { name: 'Тело', exact: true }).click();
  await expect(page.getByLabel('Тело ответа', { exact: true })).toContainText('<script>');
  expect(await page.evaluate(() => '__relayInjected' in window)).toBe(false);
  await expect(page.getByRole('heading', { name: 'untrusted response' })).toHaveCount(0);

  await page.getByLabel('URL запроса').fill(`${target}/big`);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Превью ограничено');
  await expect(page.getByLabel('Тело ответа', { exact: true })).toHaveText(/^bounded preview /);
  await expect(result.getByRole('button', { name: 'Сохранить ответ…' })).toBeDisabled();

  const previousCanceled = canceledRequests;
  await page.getByLabel('URL запроса').fill(`${target}/slow`);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Выполняется…');
  await expect.poll(() => received.at(-1)?.url).toContain('/slow');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Отменён');
  await expect.poll(() => canceledRequests).toBe(previousCanceled + 1);
  await expect(page.getByRole('button', { name: 'Send', exact: true })).toBeEnabled();
  expect(browserTargetRequests).toEqual([]);
  expect(browserErrors).toEqual([]);
  expect(logs).not.toContain(token);
  for (const filename of await readdir(join(directory, 'data'))) {
    expect((await readFile(join(directory, 'data', filename))).includes(Buffer.from(token))).toBe(
      false,
    );
  }
});

test('Protobuf imports persist offline and validate JSON5 through dynamic binary messages without protoc', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const sources = join(directory, 'catalog-schemas');
  await mkdir(join(sources, 'common'), { recursive: true });
  const serviceSource = `syntax = "proto3";
package relay.e2e.v1;
import "common/types.proto";
// A catalog used by the desktop integration test.
service Catalog {
  // Fetch an item with a precise sequence number.
  rpc GetItem(ItemRequest) returns (ItemResponse);
  rpc Watch(ItemRequest) returns (stream ItemResponse);
  rpc Upload(stream ItemRequest) returns (ItemResponse);
  rpc Chat(stream ItemRequest) returns (stream ItemResponse);
}`;
  await writeFile(join(sources, 'catalog.proto'), serviceSource);
  await writeFile(
    join(sources, 'common/types.proto'),
    `syntax = "proto3";
package relay.e2e.v1;
import "google/protobuf/any.proto";
message ItemRequest {
  int64 sequence = 1;
  string label = 2;
  repeated Detail items = 3;
  oneof selection { string by_id = 4; int32 by_index = 5; }
  google.protobuf.Any payload = 6;
  uint64 total = 7;
}
message Detail { int32 count = 1; }
message ItemResponse { string text = 1; }
`,
  );
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.getByRole('button', { name: 'Protobuf', exact: true }).click();
  await page.getByRole('button', { name: '+ Импорт схемы', exact: true }).click();
  let dialog = page.getByRole('dialog', { name: 'Импорт Protobuf', exact: true });
  await dialog.getByLabel('Название схемы', { exact: true }).fill('Missing dependency');
  await dialog.getByLabel('Файлы Protobuf', { exact: true }).setInputFiles({
    name: 'catalog.proto',
    mimeType: 'text/plain',
    buffer: Buffer.from(serviceSource),
  });
  await dialog.getByRole('button', { name: 'Импортировать', exact: true }).click();
  await expect(dialog.getByLabel('Диагностика Protobuf')).toContainText('common/types.proto');
  expect((await state(page)).protoSchemas).toHaveLength(0);
  await dialog.getByRole('button', { name: 'Отмена', exact: true }).click();

  await page.getByRole('button', { name: '+ Импорт схемы', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Импорт Protobuf', exact: true });
  await dialog.getByLabel('Название схемы', { exact: true }).fill('Catalog schemas');
  await dialog.getByLabel('Папка Protobuf', { exact: true }).setInputFiles(sources);
  await expect(dialog.getByLabel('Выбранные файлы Protobuf')).toContainText('common/types.proto');
  await dialog.getByRole('button', { name: 'Импортировать', exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(page.getByLabel('Дерево сервисов Protobuf')).toContainText('relay.e2e.v1');
  const fullMethod = '/relay.e2e.v1.Catalog/GetItem';
  await page.getByRole('button', { name: `Метод ${fullMethod}`, exact: true }).click();
  await expect(
    page.getByText('Fetch an item with a precise sequence number.', { exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel('Дерево сервисов Protobuf')).toContainText('Bidi');
  const imported = (await state(page)).protoSchemas[0];
  expect(imported.checksum).toMatch(/^[a-f0-9]{64}$/);
  expect(imported.files).toContain('google/protobuf/any.proto');
  expect(imported).not.toHaveProperty('descriptorSetBase64');
  await page.getByRole('button', { name: 'Создать gRPC запрос', exact: true }).click();
  await expect(page.getByLabel('Схема Protobuf', { exact: true })).toHaveValue(imported.id);
  await expect(page.getByLabel('Метод RPC', { exact: true })).toHaveValue(fullMethod);
  await expect(page.getByRole('button', { name: 'Validate', exact: true })).toBeEnabled();
  await page.getByLabel('Формат сообщения Protobuf').selectOption('json5');
  const body = `{sequence: 9223372036854775807, total: 18446744073709551615,
  label: 'hello', payload: {'@type': 'type.googleapis.com/relay.e2e.v1.Detail', count: 7},}`;
  await page.getByRole('textbox', { name: 'Тело запроса', exact: true }).fill(body);
  await page.getByRole('button', { name: 'Validate', exact: true }).click();
  const preview = page.getByLabel('Предпросмотр Protobuf', { exact: true });
  await expect(preview.getByRole('status')).toContainText('Валидный Protobuf');
  await expect(page.getByLabel('Canonical ProtoJSON', { exact: true })).toContainText(
    '"9223372036854775807"',
  );
  await expect(page.getByLabel('Canonical ProtoJSON', { exact: true })).toContainText(
    '"18446744073709551615"',
  );
  await page.getByRole('tab', { name: 'Protobuf hex', exact: true }).click();
  await expect(page.getByLabel('Protobuf hex', { exact: true })).toContainText(
    '08 ff ff ff ff ff ff ff ff 7f',
  );
  await page.getByRole('button', { name: 'Format', exact: true }).click();
  await expect(page.getByLabel('Формат сообщения Protobuf')).toHaveValue('json');
  await page.getByRole('tab', { name: 'Canonical ProtoJSON', exact: true }).click();
  const canonical = await page.getByLabel('Canonical ProtoJSON', { exact: true }).innerText();
  const canary = 'invalid-field-value-canary-57e9';
  await page
    .getByRole('textbox', { name: 'Тело запроса', exact: true })
    .fill(`{"items":[{"count":"${canary}"}]}`);
  await page.getByRole('button', { name: 'Validate', exact: true }).click();
  const diagnostics = page.getByLabel('Диагностика Protobuf');
  await expect(diagnostics).toContainText('items[0].count');
  await expect(diagnostics).not.toContainText(canary);
  await page.getByRole('textbox', { name: 'Тело запроса', exact: true }).fill(canonical);
  await page.getByLabel('Сохранять тело на диске').check();
  await expect
    .poll(
      async () =>
        (await state(page)).requests.find((request) => request.grpc?.method === fullMethod)?.grpc
          ?.body,
    )
    .toBe(canonical);
  await page.screenshot({ path: join(tmpdir(), 'relay-stage3.png'), fullPage: true });

  await page.close();
  await stop();
  await start();
  const restored = await page.context().newPage();
  restored.on('pageerror', (error) => errors.push(error.message));
  await restored.goto(`http://127.0.0.1:${port}/`);
  await expect(restored.getByLabel('Метод RPC', { exact: true })).toHaveValue(fullMethod);
  await expect(restored.getByLabel('Схема Protobuf', { exact: true })).toHaveValue(imported.id);
  await expect(restored.getByRole('button', { name: 'Validate', exact: true })).toBeEnabled();
  await restored.getByRole('button', { name: 'Validate', exact: true }).click();
  await expect(restored.getByLabel('Предпросмотр Protobuf').getByRole('status')).toContainText(
    'Валидный Protobuf',
  );
  expect((await state(restored)).protoSchemas[0]).toEqual(imported);

  await restored.getByRole('button', { name: 'Protobuf', exact: true }).click();
  await writeFile(
    join(sources, 'catalog.proto'),
    serviceSource.replace(
      'rpc GetItem(ItemRequest) returns (ItemResponse);',
      'rpc GetItem(ItemRequest) returns (stream ItemResponse);',
    ),
  );
  await restored.getByRole('button', { name: 'Переимпорт', exact: true }).click();
  const reimport = restored.getByRole('dialog', { name: 'Переимпортировать схему', exact: true });
  await reimport.getByLabel('Папка Protobuf', { exact: true }).setInputFiles(sources);
  await reimport.getByRole('button', { name: 'Импортировать', exact: true }).click();
  await expect(reimport).toBeHidden();
  await expect(restored.getByLabel('Диагностика Protobuf')).toContainText('streaming type changed');
  const replacement = (await state(restored)).protoSchemas[0];
  expect(replacement.id).toBe(imported.id);
  expect(replacement.checksum).not.toBe(imported.checksum);

  await restored.getByRole('button', { name: '+ Импорт схемы', exact: true }).click();
  const descriptorDialog = restored.getByRole('dialog', { name: 'Импорт Protobuf', exact: true });
  await descriptorDialog.getByLabel('Название схемы', { exact: true }).fill('Descriptor sample');
  await descriptorDialog
    .getByLabel('Descriptor set Protobuf', { exact: true })
    .setInputFiles(join(root, 'testdata/descriptors/editor.protoset'));
  await descriptorDialog.getByRole('button', { name: 'Импортировать', exact: true }).click();
  await expect(descriptorDialog).toBeHidden();
  await expect(
    restored.getByRole('button', {
      name: 'Метод /relay.descriptor.v1.DescriptorService/Echo',
      exact: true,
    }),
  ).toBeVisible();
  await expect.poll(async () => (await state(restored)).protoSchemas.length).toBe(2);
  expect(errors).toEqual([]);
  expect(logs).not.toContain(canary);
});
