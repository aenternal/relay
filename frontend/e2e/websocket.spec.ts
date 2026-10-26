import { test, expect, type Page } from '@playwright/test';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, readdir, readFile, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import type { State, WebSocketSavedSession, WebSocketSessionSummary } from '../src/lib/types';

const root = resolve(import.meta.dirname, '../..');
let directory: string, binary: string, port: number;
let desktop: ChildProcess | undefined, fixture: ChildProcess | undefined;
let endpoints: Record<string, string>,
  logs = '';

async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null) return;
  await new Promise<void>((resolve) => {
    const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
    child.kill();
  });
}
async function start() {
  desktop = spawn(binary, [], {
    env: {
      ...process.env,
      RELAY_DATA_DIR: join(directory, 'data'),
      WAILS_SERVER_HOST: '127.0.0.1',
      WAILS_SERVER_PORT: String(port),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  for (const output of [desktop.stdout, desktop.stderr])
    output?.on('data', (data) => {
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
test.beforeAll(async () => {
  directory = await mkdtemp(join(tmpdir(), 'relay-websocket-e2e-'));
  binary = join(directory, 'relay');
  const fixtureBinary = join(directory, 'fixture');
  execFileSync('go', ['build', '-tags', 'server', '-o', binary, '.'], {
    cwd: join(root, 'cmd/desktop'),
    env: { ...process.env, CGO_ENABLED: '0' },
    stdio: 'pipe',
    timeout: 120000,
  });
  execFileSync('go', ['build', '-o', fixtureBinary, './internal/testutil/wsfixture'], {
    cwd: root,
    env: process.env,
    stdio: 'pipe',
    timeout: 120000,
  });
  fixture = spawn(fixtureBinary, [], { stdio: ['ignore', 'pipe', 'pipe'] });
  fixture.stderr?.on('data', (data) => {
    logs += String(data);
  });
  endpoints = await new Promise((resolve, reject) => {
    let output = '';
    const timer = setTimeout(() => reject(new Error('WebSocket fixture did not start')), 10000);
    fixture?.once('exit', () => {
      clearTimeout(timer);
      reject(new Error('Fixture exited'));
    });
    fixture?.stdout?.on('data', (data) => {
      output += String(data);
      if (!output.includes('\n')) return;
      clearTimeout(timer);
      try {
        resolve(JSON.parse(output.split('\n')[0]));
      } catch (error) {
        reject(error);
      }
    });
  });
  const server = createServer();
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('No local test port');
  port = address.port;
  await new Promise<void>((resolve) => server.close(() => resolve()));
  await start();
});
test.afterAll(async () => {
  await stop(desktop);
  await stop(fixture);
  if (directory) await rm(directory, { recursive: true, force: true });
});

test('WebSocket uses Go for ws/wss, binary/ping/close, bounded 10k events and opt-in history across restart', async ({
  page,
}) => {
  test.setTimeout(90000);
  const errors: string[] = [],
    browserTargets: string[] = [];
  const observe = (page: Page) => {
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('request', (request) => {
      if (
        Object.values(endpoints).some((endpoint) => request.url().includes(new URL(endpoint).host))
      )
        browserTargets.push(request.url());
    });
    page.on('websocket', (socket) => {
      if (
        Object.values(endpoints).some((endpoint) => socket.url().includes(new URL(endpoint).host))
      )
        browserTargets.push(socket.url());
    });
  };
  observe(page);
  const stats = async () => (await (await fetch(endpoints.stats)).json()) as Record<string, number>;
  await page.goto(`http://127.0.0.1:${port}/`);
  await page.getByRole('button', { name: 'Новый запрос', exact: true }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Название', { exact: true }).fill('Live WebSocket');
  await dialog.getByLabel('Протокол').selectOption('websocket');
  await dialog.getByRole('button', { name: 'Создать', exact: true }).click();
  await expect(dialog).toBeHidden();
  const canary = 'private-websocket-credential-canary-69c1';
  await page
    .getByLabel('WebSocket URL', { exact: true })
    .fill(`${endpoints.ws}?channel=ui&token=${canary}`);
  const tabs = page.getByRole('tablist', { name: 'Параметры WebSocket', exact: true });
  await tabs.getByRole('tab', { name: 'Заголовки', exact: true }).click();
  for (const [index, [name, value]] of [
    ['Authorization', `Bearer ${canary}`],
    ['Cookie', `session=${canary}`],
    ['Origin', 'https://relay.test'],
    ['X-Repeat', 'first'],
    ['X-Repeat', 'second'],
  ].entries()) {
    await page.getByRole('button', { name: '+ Заголовок WebSocket', exact: true }).click();
    await page.getByLabel(`Имя заголовок WebSocket ${index + 1}`, { exact: true }).fill(name);
    await page.getByLabel(`Значение заголовок WebSocket ${index + 1}`, { exact: true }).fill(value);
  }
  await tabs.getByRole('tab', { name: 'Опции', exact: true }).click();
  await page.getByLabel('WebSocket subprotocols', { exact: true }).fill('echo.v1');
  await page.getByLabel('WebSocket соединение, мс', { exact: true }).fill('500');
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  const result = page.getByLabel('Результат WebSocket', { exact: true });
  const history = result.getByLabel('История WebSocket', { exact: true });
  const body = page.getByRole('textbox', { name: 'Тело сообщения WebSocket', exact: true });
  const send = page.getByRole('button', { name: 'Send message', exact: true });
  const connect = page.getByRole('button', { name: 'Connect', exact: true });
  await connect.click();
  await expect(send).toBeEnabled();
  await expect(result).toContainText('HTTP 101');
  await expect(result).toContainText('echo.v1');
  for (const key of ['authorization', 'cookies', 'origins', 'queries', 'repeatedHeaders'])
    expect((await stats())[key]).toBe(1);
  const firstConnection = (await stats()).connections;
  await result.getByRole('tab', { name: 'Handshake', exact: true }).click();
  await expect(result).toContainText('first');
  await expect(result).toContainText('second');
  await expect(result).not.toContainText(canary);
  await result.getByRole('tab', { name: 'События', exact: true }).click();
  await body.fill('Привет, 世界 👋');
  await send.click();
  const selected = history.getByLabel('Сообщение WebSocket', { exact: true });
  await history
    .getByRole('button', { name: /^← #\d+ received text/ })
    .last()
    .click();
  await expect(selected).toContainText('Привет, 世界 👋');
  await page.getByLabel('Тип сообщения WebSocket', { exact: true }).selectOption('binary');
  await page.getByLabel('Кодировка WebSocket', { exact: true }).selectOption('hex');
  await body.fill('00 01 ff 80');
  await send.click();
  await history
    .getByRole('button', { name: /^← #\d+ received binary/ })
    .last()
    .click();
  await history.getByLabel('Вид сообщения WebSocket', { exact: true }).selectOption('hex');
  await expect(selected).toContainText('00 01 ff 80');
  await page.getByLabel('Кодировка WebSocket', { exact: true }).selectOption('base64');
  await body.fill('AP9BQQ==');
  await send.click();
  await expect.poll(async () => (await stats()).binary).toBe(2);
  await body.fill('');
  await send.click();
  await expect.poll(async () => (await stats()).binary).toBe(3);
  expect((await stats()).connections).toBe(firstConnection);
  await page.getByLabel('Тип сообщения WebSocket', { exact: true }).selectOption('text');
  await body.fill(canary);
  await send.click();
  await expect(result).toContainText(/↓\s*5\s*\//);
  await history
    .getByRole('button', { name: /^← #\d+ received text/ })
    .last()
    .click();
  await history.getByLabel('Вид сообщения WebSocket', { exact: true }).selectOption('text');
  await expect(selected).toContainText('[REDACTED]');
  await expect(result).not.toContainText(canary);
  await page.getByRole('button', { name: 'Ping', exact: true }).click();
  await expect.poll(async () => (await stats()).pings).toBeGreaterThanOrEqual(1);
  await expect(history.getByRole('button', { name: /^← #\d+ pong/ })).toBeVisible();
  await body.fill('__ping__');
  await send.click();
  await expect.poll(async () => (await stats()).pongs).toBeGreaterThanOrEqual(1);
  await expect(history.getByRole('button', { name: /^← #\d+ ping/ })).toBeVisible();
  await page.getByLabel('WebSocket close reason', { exact: true }).fill('UI done');
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click();
  await expect(connect).toBeEnabled();
  await result.getByRole('tab', { name: 'Соединение', exact: true }).click();
  await expect(result).toContainText('Close code: 1000');
  await expect(result).toContainText('clean close');
  await result.getByRole('tab', { name: 'События', exact: true }).click();
  const current = await call<State>(page, 'LoadState');
  const request = current.requests.find((entry) => entry.name === 'Live WebSocket')!;
  const sessions = () =>
    call<WebSocketSessionSummary[]>(page, 'ListWebSocketSessions', [request.workspaceId]);
  await expect(page.getByLabel('Включить payloads в историю', { exact: true })).not.toBeChecked();
  await page.getByRole('button', { name: 'Сохранить сессию', exact: true }).click();
  await expect.poll(async () => (await sessions()).length).toBe(1);
  const metadataID = (await sessions())[0].id;
  const metadata = await call<WebSocketSavedSession>(page, 'GetSavedWebSocketSession', [
    request.workspaceId,
    metadataID,
  ]);
  expect(metadata.events.every((event) => !event.text && !event.dataBase64)).toBe(true);
  expect(metadata.execution.handshakeHeaders).toEqual([]);
  expect(metadata.execution.url).not.toContain('?');
  await page.getByLabel('Включить payloads в историю', { exact: true }).check();
  await page.getByRole('button', { name: 'Сохранить сессию', exact: true }).click();
  await expect.poll(async () => (await sessions()).length).toBe(2);
  const payloadID = (await sessions()).find((entry) => entry.includePayloads)!.id;
  const payloads = await call<WebSocketSavedSession>(page, 'GetSavedWebSocketSession', [
    request.workspaceId,
    payloadID,
  ]);
  expect(payloads.events.some((event) => event.text === 'Привет, 世界 👋')).toBe(true);
  expect(JSON.stringify(payloads)).not.toContain(canary);

  await page.getByLabel('WebSocket URL', { exact: true }).fill(endpoints.ws);
  await connect.click();
  await expect(send).toBeEnabled();
  await body.fill('__burst__');
  await send.click();
  await expect(result).toContainText(/↓\s*10000\s*\//, { timeout: 15000 });
  expect(await history.getByRole('button', { name: /^[←→·] #/ }).count()).toBeLessThanOrEqual(50);
  await expect(result).toContainText('пропущено событий');
  await page.getByRole('button', { name: 'Ping', exact: true }).click();
  await expect.poll(async () => (await stats()).pings).toBeGreaterThanOrEqual(2);
  await body.fill('__drop__');
  await send.click();
  await expect(connect).toBeEnabled();
  await result.getByRole('tab', { name: 'Соединение', exact: true }).click();
  await expect(result).toContainText('Close code: 1006');
  await expect(result).toContainText('без подтверждённого close handshake');

  await page.getByLabel('WebSocket URL', { exact: true }).fill(endpoints.wss);
  await connect.click();
  await expect(result.getByRole('alert')).toContainText(/TLS|certificate/i);
  await expect(connect).toBeEnabled();
  await tabs.getByRole('tab', { name: 'TLS', exact: true }).click();
  await page.getByLabel('Отключить проверку TLS-сертификата (insecure)', { exact: true }).check();
  await connect.click();
  await expect(send).toBeEnabled();
  await tabs.getByRole('tab', { name: 'Сообщение', exact: true }).click();
  await body.fill('secure echo');
  await send.click();
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click();
  await expect(connect).toBeEnabled();
  await expect(result).toContainText('TLS 1.3');
  await page.getByLabel('WebSocket URL', { exact: true }).fill(endpoints.reject);
  await connect.click();
  await expect(result).toContainText('HTTP 401');
  await expect(connect).toBeEnabled();
  await page.getByLabel('WebSocket URL', { exact: true }).fill(endpoints.ws);
  await connect.click();
  await expect(send).toBeEnabled();
  await expect.poll(async () => (await stats()).active).toBe(1);
  await page
    .getByRole('button', { name: /^Сохранить/ })
    .first()
    .click();
  await expect
    .poll(
      async () =>
        (await call<State>(page, 'LoadState')).requests.find((entry) => entry.id === request.id)
          ?.websocket?.url,
    )
    .toBe(endpoints.ws);
  await page.getByLabel('Закрыть вкладку Live WebSocket', { exact: true }).click();
  const closeDialog = page.getByRole('dialog', { name: 'Сохранить изменения?' });
  await expect(closeDialog).toBeVisible();
  await closeDialog.getByRole('button', { name: 'Не сохранять', exact: true }).click();
  await expect.poll(async () => (await stats()).active).toBe(0);
  await page.getByRole('button', { name: 'WS Live WebSocket', exact: true }).click();
  await page
    .getByRole('button', { name: /^Сохранить/ })
    .first()
    .click();
  await page.close();
  await stop(desktop);
  await start();
  const restored = await page.context().newPage();
  observe(restored);
  await restored.goto(`http://127.0.0.1:${port}/`);
  await expect(restored.getByLabel('WebSocket URL', { exact: true })).toHaveValue(endpoints.ws);
  await restored.getByText('Сохранённые сессии WebSocket (2)', { exact: true }).click();
  await restored.getByLabel(`Открыть сессию ${payloadID}`, { exact: true }).click();
  const savedView = restored.getByLabel('Сохранённая сессия WebSocket', { exact: true });
  await expect(savedView).toContainText('С payloads');
  await savedView
    .getByRole('button', { name: /^← #\d+ received text/ })
    .first()
    .click();
  await expect(savedView.getByLabel('Сообщение WebSocket', { exact: true })).toContainText(
    'Привет, 世界 👋',
  );
  await restored.screenshot({ path: join(tmpdir(), 'relay-stage6.png'), fullPage: true });
  await restored.getByLabel(`Удалить сессию ${metadataID}`, { exact: true }).click();
  await restored.getByLabel(`Удалить сессию ${payloadID}`, { exact: true }).click();
  await expect(
    restored.getByText('Сохранённые сессии WebSocket (0)', { exact: true }),
  ).toBeVisible();
  for (const filename of await readdir(join(directory, 'data')))
    expect((await readFile(join(directory, 'data', filename))).includes(Buffer.from(canary))).toBe(
      false,
    );
  expect(logs).not.toContain(canary);
  expect(errors).toEqual([]);
  expect(browserTargets).toEqual([]);
});
