import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAppStore, hasVolatileBody, isDirty } from './store';
import { fixtureState, testBackend } from '../test/backend';
import { newRequest } from './types';
import type { Capabilities, State } from './types';

afterEach(() => vi.useRealTimers());

describe('request state and persistence', () => {
  it('restores saved tabs and settings after a new store is created', async () => {
    const backend = testBackend();
    const first = createAppStore(backend.api);
    await first.getState().initialize();
    first.getState().openRequest('r1');
    await first
      .getState()
      .mutate((api) =>
        api.SaveSettings({ theme: 'light', responsePreviewBytes: 8192, historyLimit: 100 }),
      );
    const second = createAppStore(backend.api);
    await second.getState().initialize();
    expect(second.getState().openIds).toEqual(['r1']);
    expect(second.getState().activeId).toBe('r1');
    expect(second.getState().data.settings.theme).toBe('light');
    first.getState().dispose();
    second.getState().dispose();
  });

  it('debounces autosave, strips transient body and keeps its in-memory draft', async () => {
    vi.useFakeTimers();
    const backend = testBackend();
    const store = createAppStore(backend.api, 500);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const draft = store.getState().tabs.r1.draft;
    store.getState().editRequest('r1', {
      ...draft,
      name: 'Изменено',
      http: { ...draft.http!, body: 'private payload' },
    });
    await vi.advanceTimersByTimeAsync(250);
    store.getState().editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Готово' });
    await vi.advanceTimersByTimeAsync(499);
    expect(backend.api.SaveRequest).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(backend.api.SaveRequest).toHaveBeenCalledTimes(1);
    expect(backend.state().requests[0].http?.body).toBe('');
    expect(store.getState().tabs.r1.draft.http?.body).toBe('private payload');
    expect(isDirty(store.getState().tabs.r1)).toBe(false);
    expect(hasVolatileBody(store.getState().tabs.r1)).toBe(true);
    store.getState().dispose();
  });

  it('confirms cancel/discard/save including an explicit body persistence opt-in', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const draft = store.getState().tabs.r1.draft;
    store.getState().editRequest('r1', { ...draft, http: { ...draft.http!, body: 'payload' } });
    store.getState().requestClose('r1');
    expect(store.getState().closingId).toBe('r1');
    await store.getState().resolveClose('cancel');
    expect(store.getState().openIds).toEqual(['r1']);
    store.getState().requestClose('r1');
    await store.getState().resolveClose('save');
    expect(store.getState().openIds).toEqual([]);
    expect(backend.state().requests[0].http).toMatchObject({ body: 'payload', persistBody: true });
    store.getState().openRequest('r1');
    store.getState().editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Discard me' });
    store.getState().requestClose('r1');
    await store.getState().resolveClose('discard');
    expect(backend.state().requests[0].name).not.toBe('Discard me');
    store.getState().dispose();
  });

  it('does not overwrite an edit made while a save is pending', async () => {
    const backend = testBackend();
    const originalSave = backend.api.SaveRequest;
    let release!: (value: State) => void;
    backend.api.SaveRequest = vi.fn(async (item) => {
      const state = await originalSave(item);
      return new Promise<State>((resolve) => {
        release = () => resolve(state);
      });
    });
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const draft = store.getState().tabs.r1.draft;
    store.getState().editRequest('r1', { ...draft, name: 'First edit' });
    const saving = store.getState().saveRequest('r1');
    await vi.waitFor(() => expect(release).toBeDefined());
    store.getState().editRequest('r1', { ...draft, name: 'Second edit' });
    release(backend.state());
    await saving;
    expect(store.getState().tabs.r1.draft.name).toBe('Second edit');
    expect(store.getState().tabs.r1.saved.name).toBe('First edit');
    expect(isDirty(store.getState().tabs.r1)).toBe(true);
    store.getState().dispose();
  });

  it('keeps dirty state and the tab open when saving fails', async () => {
    const backend = testBackend();
    backend.api.SaveRequest.mockRejectedValueOnce(new Error('Storage unavailable'));
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    store.getState().editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Unsaved' });
    store.getState().requestClose('r1');
    await store.getState().resolveClose('save');
    expect(store.getState().openIds).toEqual(['r1']);
    expect(isDirty(store.getState().tabs.r1)).toBe(true);
    expect(store.getState().error).toBe('Storage unavailable');
    store.getState().dispose();
  });

  it('isolates persisted tabs by workspace and keeps fallback selection in the active workspace', async () => {
    const data = fixtureState();
    data.workspaces.push({ id: 'w2', name: 'Second', activeEnvironmentId: '' });
    data.collections.push({ id: 'c2', workspaceId: 'w2', name: 'Second' });
    data.requests.push({ ...newRequest('w2', 'c2'), id: 'r2', name: 'Second' });
    const backend = testBackend(data);
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    store.getState().openRequest('r2');
    await store.getState().mutate(async (api) => api.LoadState());
    expect(backend.state().ui).toMatchObject({
      workspaceId: 'w2',
      openRequestIds: ['r2'],
      activeRequestId: 'r2',
    });
    store.getState().requestClose('r2');
    await store.getState().mutate(async (api) => api.LoadState());
    expect(store.getState().activeId).toBe('');
    expect(store.getState().workspaceId).toBe('w2');
    store.getState().switchWorkspace('w1');
    await store.getState().mutate(async (api) => api.LoadState());
    expect(store.getState().activeId).toBe('r1');
    expect(store.getState().error).toBe('');
    store.getState().dispose();
  });

  it('loads ordinary collections while an OS keychain check is pending', async () => {
    const backend = testBackend();
    backend.api.GetCapabilities = vi.fn(() => new Promise<Capabilities>(() => {}));
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    expect(store.getState().loaded).toBe(true);
    expect(store.getState().data.collections).toHaveLength(1);
    store.getState().dispose();
  });
});
