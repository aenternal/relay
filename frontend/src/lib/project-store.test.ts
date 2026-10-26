import { describe, expect, it, vi } from 'vitest';
import { createAppStore } from './store';
import { newRequest } from './types';
import type { ProjectImportResult, ProjectStats, State } from './types';
import { testBackend } from '../test/backend';

const stats: ProjectStats = {
  collections: 1,
  folders: 0,
  requests: 1,
  environments: 0,
  schemas: 0,
  secretVariables: 0,
};

describe('project transfer state', () => {
  it('waits for a pending saved definition before preparing or writing an export', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    let release!: () => void;
    const pending = store.getState().mutate(async (api) => {
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      return api.SaveRequest({ ...backend.state().requests[0], name: 'Newest saved definition' });
    });
    await vi.waitFor(() => expect(release).toBeDefined());
    backend.api.ExportProject.mockImplementation(async () => ({
      filename: 'project.json',
      data: backend.state().requests[0].name,
      warnings: [],
      stats,
    }));
    backend.api.SaveProjectFile.mockImplementation(async () => ({
      saved: true,
      warnings: [],
      stats,
    }));
    const exported = store.getState().exportProject('w1', false);
    const saved = store.getState().saveProjectFile('w1', false);
    expect(backend.api.ExportProject).not.toHaveBeenCalled();
    expect(backend.api.SaveProjectFile).not.toHaveBeenCalled();
    release();
    await pending;
    expect((await exported).data).toBe('Newest saved definition');
    expect((await saved).saved).toBe(true);
    store.getState().dispose();
  });
  it('serializes import after a pending save, preserves later draft edits and opens the imported workspace', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    const original = backend.api.SaveRequest;
    let release!: () => void;
    backend.api.SaveRequest = vi.fn(async (request) => {
      const state = await original(request);
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      return state;
    });
    backend.api.ImportProject.mockImplementation(async () => {
      await backend.api.SaveWorkspace({
        id: 'imported-workspace',
        name: 'Imported',
        activeEnvironmentId: '',
      });
      await backend.api.SaveCollection({
        id: 'imported-collection',
        workspaceId: 'imported-workspace',
        name: 'Imported',
      });
      const request = newRequest('imported-workspace', 'imported-collection', '', 'http');
      request.id = 'imported-request';
      const state = await original(request);
      return { state, workspaceId: 'imported-workspace', warnings: [], stats };
    });
    store.getState().editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Saved first' });
    const saving = store.getState().saveRequest('r1');
    await vi.waitFor(() => expect(release).toBeDefined());
    store
      .getState()
      .editRequest('r1', { ...store.getState().tabs.r1.draft, name: 'Unsaved newer edit' });
    const importing = store.getState().importProject('native', '{}', false);
    expect(backend.api.ImportProject).not.toHaveBeenCalled();
    release();
    await saving;
    await importing;
    expect(backend.api.ImportProject).toHaveBeenCalledWith('native', '{}', false);
    expect(store.getState().workspaceId).toBe('imported-workspace');
    expect(store.getState().activeId).toBe('imported-request');
    expect(store.getState().tabs.r1.draft.name).toBe('Unsaved newer edit');
    expect(backend.state().requests.find((request) => request.id === 'r1')?.name).toBe(
      'Saved first',
    );
    store.getState().dispose();
  });

  it('keeps the existing workspace and drafts when import fails', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api, 60000);
    await store.getState().initialize();
    store.getState().openRequest('r1');
    await store.getState().mutate((api) => api.LoadState());
    const before = store.getState().data;
    backend.api.ImportProject.mockRejectedValue(new Error('untrusted error-canary'));
    expect(await store.getState().importProject('insomnia', '{}', true)).toBeUndefined();
    expect(store.getState().workspaceId).toBe('w1');
    expect(store.getState().activeId).toBe('r1');
    expect(store.getState().data).toBe(before);
    expect(store.getState().error).not.toContain('error-canary');
    store.getState().dispose();
  });

  it('ignores a completed import after store disposal', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    let finish!: (value: ProjectImportResult) => void;
    backend.api.ImportProject.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const importing = store.getState().importProject('native', '{}', false);
    await vi.waitFor(() => expect(finish).toBeDefined());
    const state: State = structuredClone(store.getState().data);
    state.workspaces.push({ id: 'late-workspace', name: 'Late', activeEnvironmentId: '' });
    state.revision++;
    store.getState().dispose();
    finish({ state, workspaceId: 'late-workspace', warnings: [], stats });
    expect(await importing).toBeUndefined();
    expect(
      store.getState().data.workspaces.some((workspace) => workspace.id === 'late-workspace'),
    ).toBe(false);
    expect(store.getState().workspaceId).toBe('w1');
  });

  it('does not begin a queued import after disposal', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    let release!: () => void;
    const pending = store.getState().mutate(async (api) => {
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      return api.LoadState();
    });
    await vi.waitFor(() => expect(release).toBeDefined());
    const importing = store.getState().importProject('native', '{}', false);
    store.getState().dispose();
    release();
    await pending;
    expect(await importing).toBeUndefined();
    expect(backend.api.ImportProject).not.toHaveBeenCalled();
  });

  it('does not override navigation made while an import is running', async () => {
    const backend = testBackend();
    const store = createAppStore(backend.api);
    await store.getState().initialize();
    let finish!: (value: ProjectImportResult) => void;
    backend.api.ImportProject.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const importing = store.getState().importProject('native', '{}', false);
    await vi.waitFor(() => expect(finish).toBeDefined());
    store.getState().openRequest('r1');
    const state = structuredClone(backend.state());
    state.workspaces.push({ id: 'imported-workspace', name: 'Imported', activeEnvironmentId: '' });
    state.revision++;
    finish({ state, workspaceId: 'imported-workspace', warnings: [], stats });
    expect(await importing).toBeDefined();
    expect(store.getState().workspaceId).toBe('w1');
    expect(store.getState().activeId).toBe('r1');
    store.getState().dispose();
  });
});
