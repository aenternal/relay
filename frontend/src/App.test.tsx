import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';
import { createAppStore } from './lib/store';
import { newRequest } from './lib/types';
import type { RelayStore } from './lib/store';
import { fixtureExecution, fixtureState, testBackend } from './test/backend';

const stores: RelayStore[] = [];
afterEach(() => {
  stores.forEach((store) => store.getState().dispose());
  stores.length = 0;
});

function mount(backend = testBackend()) {
  const store = createAppStore(backend.api, 60000);
  stores.push(store);
  render(<App store={store} />);
  return { backend, store, user: userEvent.setup() };
}

describe('desktop workspace', () => {
  it('opens a saved HTTP definition and enables Go HTTP execution', async () => {
    const { user } = mount();
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    expect(screen.getByRole('textbox', { name: 'Имя запроса' })).toHaveValue('Проверка сервиса');
    expect(screen.getByRole('textbox', { name: 'Тело запроса' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
    expect(screen.getByLabelText('Результат запроса')).toHaveTextContent('Нет выполнения');
    expect(screen.getByRole('checkbox', { name: 'Сохранять тело на диске' })).not.toBeChecked();
  });

  it('creates a nested folder and a request inside it through collection menus', async () => {
    const { user, backend } = mount();
    await user.click(await screen.findByLabelText('Действия коллекции Проверки API'));
    await user.click(screen.getByRole('button', { name: 'Новая папка' }));
    await user.type(screen.getByRole('textbox', { name: 'Название' }), 'v1');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Создать' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(screen.getByLabelText('Действия папки v1'));
    await user.click(screen.getByRole('button', { name: 'Вложенная папка' }));
    await user.type(screen.getByRole('textbox', { name: 'Название' }), 'users');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Создать' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(screen.getByLabelText('Действия папки users'));
    const menu = screen.getByLabelText('Действия папки users').closest('details')!;
    await user.click(within(menu).getByRole('button', { name: 'Новый запрос' }));
    await user.type(screen.getByRole('textbox', { name: 'Название' }), 'Список пользователей');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Создать' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const nested = backend.state().folders.find((item) => item.name === 'users')!;
    expect(nested.parentId).toBe(backend.state().folders.find((item) => item.name === 'v1')!.id);
    expect(
      backend.state().requests.find((item) => item.name === 'Список пользователей')?.folderId,
    ).toBe(nested.id);
    expect(screen.getByRole('textbox', { name: 'Имя запроса' })).toHaveValue(
      'Список пользователей',
    );
  });

  it('provides cancel, discard and explicit opt-in when closing a transient body', async () => {
    const { user, store, backend } = mount();
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    act(() => {
      const request = store.getState().tabs.r1.draft;
      store
        .getState()
        .editRequest('r1', { ...request, http: { ...request.http!, body: 'temporary body' } });
    });
    await user.click(screen.getByRole('button', { name: 'Закрыть вкладку Проверка сервиса' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent('Тело запроса находится только в памяти');
    await user.click(within(dialog).getByRole('button', { name: 'Отмена' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Закрыть вкладку Проверка сервиса' }));
    await user.click(screen.getByRole('button', { name: 'Сохранить включая тело' }));
    await waitFor(() =>
      expect(screen.queryByRole('tab', { name: /Проверка сервиса/ })).not.toBeInTheDocument(),
    );
    expect(backend.state().requests[0].http).toMatchObject({
      persistBody: true,
      body: 'temporary body',
    });
  });

  it('keeps secrets masked and sends them only to the vault command', async () => {
    const { user, backend } = mount();
    await user.click(await screen.findByRole('button', { name: 'Окружения' }));
    await user.click(screen.getByRole('button', { name: '+ Окружение' }));
    await user.type(screen.getByRole('textbox', { name: 'Название окружения' }), 'Development');
    await user.click(screen.getByRole('button', { name: '+ Переменная' }));
    await user.type(screen.getByRole('textbox', { name: 'Имя переменной 1' }), 'token');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Тип переменной 1' }), 'secret');
    const value = screen.getByLabelText('Значение переменной 1');
    expect(value).toHaveAttribute('type', 'password');
    await user.type(value, 'sensitive-test-token');
    await user.click(screen.getByRole('button', { name: 'Сохранить окружение' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(backend.api.SaveEnvironment).toHaveBeenCalledWith(
      expect.objectContaining({
        variables: [expect.objectContaining({ value: '', secret: true })],
      }),
    );
    expect(backend.api.SetSecret).toHaveBeenCalledWith(
      expect.any(String),
      expect.any(String),
      'sensitive-test-token',
    );
    expect(JSON.stringify(backend.state())).not.toContain('sensitive-test-token');
    expect(screen.getByText('••••••••')).toBeInTheDocument();
    expect(backend.api.DeleteSecret).not.toHaveBeenCalled();
  });

  it('clears a secret atomically with environment save and preserves it when that save fails', async () => {
    const data = fixtureState();
    data.environments = [
      {
        id: 'e1',
        workspaceId: 'w1',
        name: 'Development',
        variables: [
          { id: 'v1', name: 'token', value: '', secret: true, secretRef: 'vault/reference' },
        ],
      },
    ];
    const backend = testBackend(data);
    backend.api.SaveEnvironment.mockRejectedValueOnce(new Error('Storage unavailable'));
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'Окружения' }));
    await user.click(screen.getByRole('button', { name: 'Редактировать' }));
    await user.click(screen.getByRole('button', { name: 'Удалить сохранённый секрет' }));
    await user.click(screen.getByRole('button', { name: 'Сохранить окружение' }));
    await waitFor(() => expect(backend.api.SaveEnvironment).toHaveBeenCalled());
    expect(backend.api.DeleteSecret).not.toHaveBeenCalled();
    expect(backend.state().environments[0].variables[0].secretRef).toBe('vault/reference');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(backend.api.SaveEnvironment).toHaveBeenCalledWith(
      expect.objectContaining({ variables: [expect.objectContaining({ secretRef: '' })] }),
    );
  });

  it('saves validated display and response settings', async () => {
    const { user, backend } = mount();
    await user.click(await screen.findByRole('button', { name: 'Настройки' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Тема интерфейса' }), 'light');
    const history = screen.getByRole('spinbutton', { name: 'Количество событий в истории' });
    await user.clear(history);
    await user.type(history, '0');
    expect(screen.getByRole('button', { name: 'Сохранить настройки' })).toBeDisabled();
    await user.clear(history);
    await user.type(history, '100');
    await user.click(screen.getByRole('button', { name: 'Сохранить настройки' }));
    await screen.findByText('Настройки сохранены');
    expect(backend.state().settings).toMatchObject({ theme: 'light', historyLimit: 100 });
    expect(document.documentElement).toHaveAttribute('data-theme', 'light');
  });
});

describe('HTTP request UI', () => {
  it('sends with Ctrl Enter and displays escaped response text, status and protocol', async () => {
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(
      fixtureExecution('r1', {
        bodyText: '<img src=x onerror="window.secret=true">',
        bodyBase64: btoa('<img src=x>'),
      }),
    );
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.keyboard('{Control>}{Enter}{/Control}');
    await waitFor(() =>
      expect(screen.getByLabelText('Результат запроса')).toHaveTextContent('200 OK'),
    );
    expect(screen.getByLabelText('Результат запроса')).toHaveTextContent('HTTP/2.0');
    expect(screen.getByLabelText('Тело ответа')).toHaveTextContent('<img src=x');
    expect(screen.getByLabelText('Результат запроса').querySelector('img')).toBeNull();
    expect(screen.getByRole('button', { name: 'Сохранить ответ…' })).toBeDisabled();
    expect(backend.api.StartHTTPRequest).toHaveBeenCalledTimes(1);
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
  });

  it('labels a failed download as partial rather than promising a complete file', async () => {
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(
      fixtureExecution('r1', {
        state: 'failed',
        truncated: true,
        error: 'Download limit exceeded',
        receivedBytes: 1000,
        previewBytes: 100,
      }),
    );
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText(/Загружена только часть тела ответа/);
    expect(screen.getByRole('button', { name: 'Сохранить часть ответа…' })).toBeInTheDocument();
    expect(screen.getByLabelText('Результат запроса')).not.toHaveTextContent(
      'Полное тело можно сохранить',
    );
  });

  it('shows Cancel while running and cancels the active request with Escape', async () => {
    const backend = testBackend();
    backend.api.StartHTTPRequest.mockResolvedValue(fixtureExecution('r1', { state: 'running' }));
    backend.api.GetHTTPExecution.mockResolvedValue(fixtureExecution('r1', { state: 'canceled' }));
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Cancel' });
    await user.keyboard('{Escape}');
    await waitFor(() =>
      expect(screen.getByLabelText('Результат запроса')).toHaveTextContent('Отменён'),
    );
    expect(backend.api.CancelHTTPExecution).toHaveBeenCalledWith('execution-r1');
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
  });

  it('keeps independent response bodies when switching request tabs', async () => {
    const data = fixtureState();
    data.requests.push({ ...newRequest('w1', 'c1'), id: 'r2', name: 'Второй запрос' });
    const backend = testBackend(data);
    backend.api.StartHTTPRequest.mockImplementation(async (request) =>
      fixtureExecution(request.id, {
        bodyText: request.id === 'r1' ? 'first response' : 'second response',
      }),
    );
    const { user } = mount(backend);
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('first response');
    await user.click(screen.getByRole('button', { name: 'GET Второй запрос' }));
    await user.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('second response');
    await user.click(screen.getByRole('tab', { name: 'GET Проверка сервиса' }));
    expect(screen.getByLabelText('Тело ответа')).toHaveTextContent('first response');
    expect(backend.api.StartHTTPRequest).toHaveBeenCalledTimes(2);
  });

  it('saves collection auth as environment references and exposes inheritance on requests', async () => {
    const { user, backend } = mount();
    await user.click(await screen.findByLabelText('Действия коллекции Проверки API'));
    await user.click(screen.getByRole('button', { name: 'Авторизация' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Тип авторизации' }), 'bearer');
    await user.type(screen.getByLabelText('Bearer token'), 'literal-secret');
    expect(screen.getByRole('button', { name: 'Сохранить авторизацию' })).toBeDisabled();
    await user.clear(screen.getByLabelText('Bearer token'));
    await user.paste('{{token}}');
    await user.click(screen.getByRole('button', { name: 'Сохранить авторизацию' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(backend.state().collections[0].auth).toMatchObject({
      type: 'bearer',
      token: '{{token}}',
    });
    await user.click(screen.getByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('tab', { name: 'Auth' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Тип авторизации' }), 'inherit');
    expect(screen.getByRole('tabpanel', { name: 'Авторизация' })).toHaveTextContent(
      'ближайшей папки',
    );
  });

  it('suggests standard header names and accepts custom names', async () => {
    const { user } = mount();
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('tab', { name: /^Заголовки/ }));
    await user.click(screen.getByRole('button', { name: '+ Заголовок' }));
    const name = screen.getByRole('combobox', { name: 'Имя заголовок 1' });
    const listId = name.getAttribute('list');
    expect(listId).toBeTruthy();
    expect(
      document.getElementById(listId!)?.querySelector('option[value="Content-Type"]'),
    ).toBeInTheDocument();
    await user.type(name, 'X-Custom-Header');
    expect(name).toHaveValue('X-Custom-Header');
  });

  it('warns before discarding credentials and file selections after autosave', async () => {
    const { user, store } = mount();
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    act(() => {
      const request = store.getState().tabs.r1.draft;
      store.getState().editRequest('r1', {
        ...request,
        http: {
          ...request.http!,
          fileId: 'ephemeral-body',
          auth: { ...request.http!.auth, type: 'bearer', token: 'volatile-secret' },
        },
      });
    });
    await act(async () => {
      await store.getState().saveRequest('r1');
    });
    await user.click(screen.getByRole('button', { name: 'Закрыть вкладку Проверка сервиса' }));
    expect(screen.getByRole('dialog')).toHaveTextContent('будут потеряны при закрытии');
    expect(screen.getByRole('dialog')).toHaveTextContent('файлы не сохраняются');
    expect(screen.getByRole('dialog')).not.toHaveTextContent('volatile-secret');
    expect(
      screen.getByRole('button', { name: 'Сохранить определение и закрыть' }),
    ).toBeInTheDocument();
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Отмена' }));
    expect(store.getState().tabs.r1.draft.http?.auth.token).toBe('volatile-secret');
  });

  it('shows a visible warning for insecure TLS and disables native file pickers in server mode', async () => {
    const { user } = mount();
    await user.click(await screen.findByRole('button', { name: 'GET Проверка сервиса' }));
    await user.click(screen.getByRole('tab', { name: 'TLS' }));
    expect(screen.getByRole('button', { name: 'Корневой CA (PEM)' })).toBeDisabled();
    const insecure = screen.getByRole('checkbox', {
      name: 'Отключить проверку TLS-сертификата (insecure)',
    });
    expect(insecure).not.toBeChecked();
    await user.click(insecure);
    expect(screen.getByRole('alert')).toHaveTextContent('Соединение уязвимо');
  });
});
