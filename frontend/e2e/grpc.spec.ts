import { test, expect, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, readdir, readFile, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import type { State } from '../src/lib/types';

const root = resolve(import.meta.dirname, '../..');
let directory: string;
let appBinary: string;
let desktop: ChildProcess | undefined;
let fixture: ChildProcess | undefined;
let endpoints: Record<string, string>;
let port: number;
let logs = '';

async function availablePort() {
  const server = createServer();
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('No test port');
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return address.port;
}
async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null) return;
  await new Promise<void>((resolve) => {
    child.once('exit', () => resolve());
    child.kill();
  });
}
async function startDesktop() {
  desktop = spawn(appBinary, [], {
    env: {
      ...process.env,
      RELAY_DATA_DIR: join(directory, 'data'),
      WAILS_SERVER_HOST: '127.0.0.1',
      WAILS_SERVER_PORT: String(port),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  desktop.stdout?.on('data', (data) => {
    logs += String(data);
  });
  desktop.stderr?.on('data', (data) => {
    logs += String(data);
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
async function state(page: Page): Promise<State> {
  return page.evaluate(async () => {
    const path = '/wails/runtime.js';
    const { Call } = await import(path);
    return Call.ByName('relay/internal/app.Service.LoadState');
  });
}

test.beforeAll(async () => {
  directory = await mkdtemp(join(tmpdir(), 'relay-grpc-e2e-'));
  appBinary = join(directory, 'relay');
  const fixtureBinary = join(directory, 'grpc-fixture');
  execFileSync('go', ['build', '-tags', 'server', '-o', appBinary, '.'], {
    cwd: join(root, 'cmd/desktop'),
    env: { ...process.env, CGO_ENABLED: '0' },
    stdio: 'pipe',
    timeout: 120000,
  });
  execFileSync('go', ['build', '-o', fixtureBinary, './internal/testutil/grpcfixture'], {
    cwd: root,
    env: process.env,
    stdio: 'pipe',
    timeout: 120000,
  });
  fixture = spawn(fixtureBinary, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
  fixture.stderr?.on('data', (data) => {
    logs += String(data);
  });
  endpoints = await new Promise<Record<string, string>>((resolve, reject) => {
    let output = '';
    const timer = setTimeout(() => reject(new Error('gRPC fixture did not start')), 10000);
    fixture?.once('exit', () => {
      clearTimeout(timer);
      reject(new Error('gRPC fixture exited'));
    });
    fixture?.stdout?.on('data', (data) => {
      output += String(data);
      if (output.includes('\n')) {
        clearTimeout(timer);
        try {
          resolve(JSON.parse(output.split('\n')[0]));
        } catch (error) {
          reject(error);
        }
      }
    });
  });
  port = await availablePort();
  await startDesktop();
});
test.afterAll(async () => {
  await stop(desktop);
  await stop(fixture);
  if (directory) await rm(directory, { recursive: true, force: true });
});

test('gRPC UI discovers v1/alpha schemas, calls dynamic unary locally and handles TLS/status/cancel', async ({
  page,
}) => {
  const errors: string[] = [];
  const browserTargets: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('request', (request) => {
    if (Object.values(endpoints).some((endpoint) => request.url().includes(endpoint)))
      browserTargets.push(request.url());
  });
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.getByRole('button', { name: 'Новый запрос', exact: true }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Название', { exact: true }).fill('Dynamic unary');
  await dialog.getByLabel('Протокол').selectOption('grpc');
  await dialog.getByRole('button', { name: 'Создать', exact: true }).click();
  await expect(dialog).toBeHidden();
  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.reflection);
  const tabs = page.getByRole('tablist', { name: 'Параметры gRPC' });
  await tabs.getByRole('tab', { name: 'Соединение / TLS', exact: true }).click();
  await page.getByLabel('Защита соединения gRPC', { exact: true }).selectOption('plaintext');
  await page.getByRole('button', { name: 'Discover via Reflection', exact: true }).click();
  const result = page.getByLabel('Результат gRPC', { exact: true });
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Схема обнаружена · v1');
  const method = '/relay.descriptor.v1.DescriptorService/Echo';
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  await page.getByLabel('Метод RPC', { exact: true }).selectOption(method);
  const selectedSchema = await page.getByLabel('Схема Protobuf', { exact: true }).inputValue();
  expect(selectedSchema).not.toBe('');
  await tabs.getByRole('tab', { name: 'Опции', exact: true }).click();
  await page.getByLabel('Сжатие gRPC', { exact: true }).selectOption('gzip');
  await page.getByLabel('gRPC таймаут соединения, мс', { exact: true }).fill('500');
  await tabs.getByRole('tab', { name: /^Metadata/ }).click();
  const canary = 'private-unary-token-canary-7a0c';
  for (const [index, pair] of [
    ['x-test-bin', 'AAH/'],
    ['authorization', `Bearer ${canary}`],
  ].entries()) {
    await page.getByRole('button', { name: '+ Metadata', exact: true }).click();
    await page.getByLabel(`Имя gRPC metadata ${index + 1}`, { exact: true }).fill(pair[0]);
    await page.getByLabel(`Значение gRPC metadata ${index + 1}`, { exact: true }).fill(pair[1]);
  }
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  await page.getByLabel('Формат сообщения Protobuf').selectOption('json5');
  await page
    .getByRole('textbox', { name: 'Тело запроса', exact: true })
    .fill("{text: 'Привет, 世界', sequence: 9223372036854775807,}");
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('0 OK');
  await expect(page.getByLabel('Сообщение ответа gRPC', { exact: true })).toContainText(
    'Привет, 世界',
  );
  await expect(page.getByLabel('Сообщение ответа gRPC', { exact: true })).toContainText(
    '"9223372036854775807"',
  );
  await page.getByLabel('Вид ответа gRPC', { exact: true }).selectOption('hex');
  await expect(page.getByLabel('Сообщение ответа gRPC', { exact: true })).toContainText('ff ff ff');
  await result.getByRole('tab', { name: /^Headers/ }).click();
  await expect(result).toContainText('x-echo-bin');
  await expect(result).toContainText('AAH/');
  await expect(result).toContainText('first');
  await expect(result).toContainText('second');
  await result.getByRole('tab', { name: /^Trailers/ }).click();
  await expect(result).toContainText('complete');
  await page.screenshot({ path: join(tmpdir(), 'relay-stage4.png'), fullPage: true });

  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.local);
  await page.getByRole('button', { name: 'Discover via Reflection', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Ошибка');
  await expect(result).toContainText('Unimplemented');
  await expect(page.getByLabel('Схема Protobuf', { exact: true })).toHaveValue(selectedSchema);
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');

  await page.getByRole('textbox', { name: 'Тело запроса', exact: true }).fill("{text:'denied'}");
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Ошибка');
  await expect(result).toContainText('7 PermissionDenied');
  await result.getByRole('tab', { name: /^Status \/ details/ }).click();
  await expect(result).toContainText('policy blocks unary call');
  await result.getByText('type.googleapis.com/google.rpc.BadRequest', { exact: true }).click();
  await expect(result).toContainText('test validation failure');
  await expect(result).not.toContainText(canary);

  await page.getByRole('textbox', { name: 'Тело запроса', exact: true }).fill("{text:'delay'}");
  const beforeCancel = await (await fetch(endpoints.stats)).json();
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect
    .poll(async () => (await (await fetch(endpoints.stats)).json()).calls)
    .toBeGreaterThan(beforeCancel.calls);
  await expect(page.getByRole('button', { name: 'Cancel', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Отменён');
  await expect
    .poll(async () => (await (await fetch(endpoints.stats)).json()).canceled)
    .toBe(beforeCancel.canceled + 1);

  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.alpha);
  await page.getByRole('button', { name: 'Discover via Reflection', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Схема обнаружена · v1alpha');
  await page.getByLabel('Метод RPC', { exact: true }).selectOption(method);
  const alphaSchema = await page.getByLabel('Схема Protobuf', { exact: true }).inputValue();
  await page
    .getByRole('textbox', { name: 'Тело запроса', exact: true })
    .fill("{text:'tls response',sequence:9223372036854775807}");
  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.tls);
  await tabs.getByRole('tab', { name: 'Соединение / TLS', exact: true }).click();
  await page.getByLabel('Защита соединения gRPC', { exact: true }).selectOption('tls');
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Ошибка');
  await expect(result).toContainText('TLS');
  await page
    .getByLabel('Отключить проверку сертификата gRPC (insecure TLS)', { exact: true })
    .check();
  await page.getByRole('button', { name: 'Call', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await result.getByRole('tab', { name: 'TLS', exact: true }).click();
  await expect(result).toContainText('TLS 1.3');
  await expect(result).toContainText('h2');

  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.local);
  await page.getByLabel('Защита соединения gRPC', { exact: true }).selectOption('plaintext');
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  await page.getByLabel('Сохранять тело на диске').check();
  await page.getByRole('button', { name: /^Сохранить/ }).click();
  await expect
    .poll(
      async () =>
        (await state(page)).requests.find((entry) => entry.name === 'Dynamic unary')?.grpc
          ?.endpoint,
    )
    .toBe(endpoints.local);
  await page.close();
  await stop(desktop);
  await startDesktop();
  const restored = await page.context().newPage();
  restored.on('pageerror', (error) => errors.push(error.message));
  await restored.goto(`http://127.0.0.1:${port}/`);
  await expect(restored.getByLabel('gRPC endpoint', { exact: true })).toHaveValue(endpoints.local);
  await expect(restored.getByLabel('Схема Protobuf', { exact: true })).toHaveValue(alphaSchema);
  await expect(restored.getByLabel('Метод RPC', { exact: true })).toHaveValue(method);
  await restored.getByRole('button', { name: 'Call', exact: true }).click();
  const restoredResult = restored.getByLabel('Результат gRPC', { exact: true });
  await expect(restoredResult.getByRole('status')).toHaveText('Завершён');
  await expect(restoredResult).toContainText('0 OK');
  expect((await state(restored)).protoSchemas.length).toBeGreaterThanOrEqual(2);
  for (const filename of await readdir(join(directory, 'data'))) {
    expect((await readFile(join(directory, 'data', filename))).includes(Buffer.from(canary))).toBe(
      false,
    );
  }
  expect(logs).not.toContain(canary);
  expect(browserTargets).toEqual([]);
  expect(errors).toEqual([]);
});

test('all streaming RPCs exchange real dynamic messages with half-close, bounded history and cleanup', async ({
  page,
}) => {
  const errors: string[] = [];
  const browserTargets: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('request', (request) => {
    if (Object.values(endpoints).some((endpoint) => request.url().includes(endpoint)))
      browserTargets.push(request.url());
  });
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.getByRole('button', { name: 'Новый запрос', exact: true }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Название', { exact: true }).fill('Live streams');
  await dialog.getByLabel('Протокол').selectOption('grpc');
  await dialog.getByRole('button', { name: 'Создать', exact: true }).click();
  await expect(dialog).toBeHidden();
  await page.getByLabel('gRPC endpoint', { exact: true }).fill(endpoints.reflection);
  const tabs = page.getByRole('tablist', { name: 'Параметры gRPC' });
  await tabs.getByRole('tab', { name: 'Соединение / TLS', exact: true }).click();
  await page.getByLabel('Защита соединения gRPC', { exact: true }).selectOption('plaintext');
  await page.getByRole('button', { name: 'Discover via Reflection', exact: true }).click();
  const result = page.getByLabel('Результат gRPC', { exact: true });
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await tabs.getByRole('tab', { name: /^Metadata/ }).click();
  const canary = 'private-stream-token-canary-b39d';
  await page.getByRole('button', { name: '+ Metadata', exact: true }).click();
  await page.getByLabel('Имя gRPC metadata 1', { exact: true }).fill('authorization');
  await page.getByLabel('Значение gRPC metadata 1', { exact: true }).fill(`Bearer ${canary}`);
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  const method = page.getByLabel('Метод RPC', { exact: true });
  const body = page.getByRole('textbox', { name: 'Тело запроса', exact: true });
  const history = page.getByLabel('История gRPC stream', { exact: true });
  const start = page.getByRole('button', { name: 'Start stream', exact: true });
  const send = page.getByRole('button', { name: 'Send message', exact: true });
  const halfClose = page.getByRole('button', { name: 'Half-close', exact: true });
  const stats = async () => (await (await fetch(endpoints.stats)).json()) as Record<string, number>;
  await page.getByLabel('Формат сообщения Protobuf').selectOption('json5');

  // The second Send happens only after the first reply, on the original stream.
  await method.selectOption('/relay.streaming.v1.StreamService/Chat');
  await body.fill("{text:'первое 世界',sequence:9223372036854775807}");
  const beforeBidi = await stats();
  await start.click();
  await expect(send).toBeEnabled();
  await expect(result).toContainText('Отправлено: 0 · Получено: 0');
  await send.click();
  await expect(result).toContainText('Отправлено: 1 · Получено: 1');
  await history.getByRole('button', { name: /^← #1 ·/ }).click();
  const selected = page.getByLabel('Сообщение gRPC stream', { exact: true });
  await expect(selected).toContainText('первое 世界');
  await expect(selected).toContainText('"9223372036854775807"');
  await page.getByLabel('Вид сообщения gRPC stream', { exact: true }).selectOption('hex');
  await expect(selected).toContainText('ff ff ff');
  await page.getByLabel('Вид сообщения gRPC stream', { exact: true }).selectOption('json');
  await body.fill("{text:'второе после ответа',sequence:2}");
  await send.click();
  await expect(result).toContainText('Отправлено: 2 · Получено: 2');
  expect((await stats()).streams).toBe(beforeBidi.streams + 1);
  await halfClose.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('0 OK');
  await expect(halfClose).toBeHidden();
  await expect(send).toBeHidden();
  await expect(history.getByRole('button', { name: /^← #2 ·/ })).toBeVisible();
  await result.getByRole('tab', { name: /^Trailers/ }).click();
  await expect(result).toContainText('x-stream-trailer');
  await expect(result).toContainText('complete');
  await result.getByRole('tab', { name: 'История', exact: true }).click();
  await page.screenshot({ path: join(tmpdir(), 'relay-stage5.png'), fullPage: true });

  // Half-close with zero sends is valid for client streaming.
  await method.selectOption('/relay.streaming.v1.StreamService/Collect');
  await start.click();
  await expect(halfClose).toBeEnabled();
  await halfClose.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 0 · Получено: 1');
  await history.getByRole('button', { name: /^← #1 ·/ }).click();
  await expect(selected).toContainText('collected:0:');

  // Multiple sends produce one final response only after the write side closes.
  await start.click();
  await expect(send).toBeEnabled();
  await body.fill("{text:'one'}");
  await send.click();
  await expect(result).toContainText('Отправлено: 1 · Получено: 0');
  await expect(send).toBeEnabled();
  await body.fill("{text:'two'}");
  await send.click();
  await expect(result).toContainText('Отправлено: 2 · Получено: 0');
  await halfClose.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 2 · Получено: 1');
  await history.getByRole('button', { name: /^← #1 ·/ }).click();
  await expect(selected).toContainText('collected:2:one,two');

  await method.selectOption('/relay.streaming.v1.StreamService/Watch');
  await body.fill("{text:'server',sequence:3}");
  await start.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 1 · Получено: 3');
  await expect(history.getByRole('button', { name: /^← #3 ·/ })).toBeVisible();
  await body.fill("{text:'zero'}");
  await start.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 1 · Получено: 0');

  // A mid-stream failure preserves earlier messages and renders rich status safely.
  await body.fill("{text:'error'}");
  await start.click();
  await expect(result.getByRole('status')).toHaveText('Ошибка');
  await expect(result).toContainText('7 PermissionDenied');
  await expect(result).toContainText('Отправлено: 1 · Получено: 2');
  await expect(history.getByRole('button', { name: /^← #2 ·/ })).toBeVisible();
  await result.getByRole('tab', { name: /^Status \/ details/ }).click();
  await result.getByText('type.googleapis.com/google.rpc.BadRequest', { exact: true }).click();
  await expect(result).toContainText('stream validation failure');
  await expect(result).not.toContainText(canary);
  await result.getByRole('tab', { name: 'История', exact: true }).click();

  await body.fill("{text:'window',sequence:80}");
  await start.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 1 · Получено: 80');
  expect(await history.getByRole('button', { name: /^[←→] #/ }).count()).toBeLessThanOrEqual(50);

  await page.getByRole('button', { name: 'Настройки', exact: true }).click();
  await page.getByLabel('Количество событий в истории', { exact: true }).fill('10');
  await page.getByRole('button', { name: 'Сохранить настройки', exact: true }).click();
  await expect(page.getByText('Настройки сохранены', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: /^Коллекции/ }).click();
  await body.fill("{text:'bounded',sequence:80}");
  await start.click();
  await expect(result.getByRole('status')).toHaveText('Завершён');
  await expect(result).toContainText('Отправлено: 1 · Получено: 80');
  await expect(history.getByRole('button', { name: /^[←→] #/ })).toHaveCount(10);

  const beforeCancel = await stats();
  await body.fill("{text:'delay'}");
  await start.click();
  await expect.poll(async () => (await stats()).streamActive).toBe(1);
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(result.getByRole('status')).toHaveText('Отменён');
  await expect
    .poll(async () => (await stats()).streamCanceled)
    .toBe(beforeCancel.streamCanceled + 1);

  // Closing a tab also cancels the real peer context and frees its execution.
  await method.selectOption('/relay.streaming.v1.StreamService/Chat');
  await start.click();
  await expect(send).toBeEnabled();
  await expect.poll(async () => (await stats()).streamActive).toBe(1);
  await page.getByLabel('Закрыть вкладку Live streams', { exact: true }).click();
  const closeDialog = page.getByRole('dialog', { name: 'Сохранить изменения?' });
  await expect(closeDialog).toBeVisible();
  await closeDialog.getByRole('button', { name: 'Не сохранять', exact: true }).click();
  await expect.poll(async () => (await stats()).streamActive).toBe(0);
  await expect
    .poll(async () => (await stats()).streamCanceled)
    .toBe(beforeCancel.streamCanceled + 2);

  for (const filename of await readdir(join(directory, 'data'))) {
    expect((await readFile(join(directory, 'data', filename))).includes(Buffer.from(canary))).toBe(
      false,
    );
  }
  expect(logs).not.toContain(canary);
  expect(browserTargets).toEqual([]);
  expect(errors).toEqual([]);
});
