// Package app coordinates persistence and secrets independently of the desktop shell.
package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"relay/internal/domain"
	"relay/internal/security"
)

type Repository interface {
	Load(context.Context) (domain.State, error)
	Save(context.Context, domain.State, int64) (domain.State, error)
	QueueSecretCleanup(context.Context, string) error
	PendingSecretCleanup(context.Context) ([]string, error)
	AcknowledgeSecretCleanup(context.Context, string) error
}

type Service struct {
	ctx            context.Context
	httpMu         sync.Mutex
	httpState      *httpState
	grpcMu         sync.Mutex
	grpcState      *grpcState
	wsMu           sync.Mutex
	wsState        *webSocketState
	repo           Repository
	vault          security.Store
	mu             sync.Mutex
	cleanupPending bool
}

type Capabilities struct {
	SecretBackend   string `json:"secretBackend"`
	SecretAvailable bool   `json:"secretAvailable"`
	Message         string `json:"message"`
}

func New(ctx context.Context, repo Repository, vault security.Store) (*Service, error) {
	if repo == nil || vault == nil {
		return nil, errors.New("application: storage and secret provider are required")
	}
	s := &Service{repo: repo, vault: vault, ctx: ctx}
	state, err := repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state.Revision == 0 && len(state.Workspaces) == 0 {
		workspaceID, collectionID, requestID, envID := newID(), newID(), newID(), newID()
		state.Workspaces = []domain.Workspace{{ID: workspaceID, Name: "Личное пространство", ActiveEnvironmentID: envID}}
		state.Collections = []domain.Collection{{ID: collectionID, WorkspaceID: workspaceID, Name: "Мои запросы"}}
		state.Requests = []domain.RequestDefinition{{ID: requestID, WorkspaceID: workspaceID, CollectionID: collectionID, Name: "Новый запрос", Protocol: "http", HTTP: &domain.HTTPDefinition{Method: "GET", URL: "{{baseUrl}}/", Version: "auto", Headers: []domain.Header{}}}}
		state.Environments = []domain.Environment{{ID: envID, WorkspaceID: workspaceID, Name: "Локальное", Variables: []domain.Variable{{ID: newID(), Name: "baseUrl", Value: "http://localhost:8080"}}}}
		state.Settings = domain.DefaultSettings()
		state.UI = domain.UIState{WorkspaceID: workspaceID, OpenRequestIDs: []string{requestID}, ActiveRequestID: requestID}
		if _, err = repo.Save(ctx, state, state.Revision); err != nil {
			return nil, err
		}
	}
	s.drainSecrets(ctx)
	return s, nil
}

func newID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("secure random source unavailable")
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:])
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("name must contain 1..256 characters without line breaks")
	}
	return name, nil
}

func (s *Service) LoadState(ctx context.Context) (domain.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repo.Load(ctx)
}

func (s *Service) GetCapabilities(ctx context.Context) Capabilities {
	s.mu.Lock()
	cleanupPending := s.cleanupPending
	s.mu.Unlock()
	result := Capabilities{SecretBackend: "os-keyring", SecretAvailable: true}
	if provider, ok := s.vault.(interface{ Backend() string }); ok {
		result.SecretBackend = provider.Backend()
	}
	if provider, ok := s.vault.(interface{ Available(context.Context) error }); ok {
		if err := provider.Available(ctx); err != nil {
			result.SecretAvailable = false
			result.Message = "Системное хранилище секретов недоступно или заблокировано. Обычные переменные можно сохранять; секреты в SQLite не записываются."
		}
	}
	if cleanupPending {
		result.Message += " Удаление недействующих секретов будет повторено после разблокировки системного хранилища."
	}
	return result
}

func (s *Service) mutate(ctx context.Context, change func(*domain.State) error) (domain.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.repo.Load(ctx)
	if err != nil {
		return domain.State{}, err
	}
	if err = change(&state); err != nil {
		return domain.State{}, err
	}
	result, err := s.repo.Save(ctx, state, state.Revision)
	if err == nil {
		s.drainSecrets(ctx)
	}
	return result, err
}

// SQLite commits reference changes and their cleanup outbox atomically. Vault
// deletion happens after commit and is retried on startup or the next mutation.
// A failed OS operation never corrupts a live saved reference or its value.
func (s *Service) drainSecrets(ctx context.Context) {
	refs, err := s.repo.PendingSecretCleanup(ctx)
	if err != nil {
		s.cleanupPending = true
		return
	}
	s.cleanupPending = false
	for _, ref := range refs {
		if err := s.deleteVault(ctx, ref); err != nil {
			s.cleanupPending = true
			continue
		}
		if err := s.repo.AcknowledgeSecretCleanup(ctx, ref); err != nil {
			s.cleanupPending = true
		}
	}
}

func upsert[T any](items []T, incoming T, id string, getID func(T) string) []T {
	for i, item := range items {
		if getID(item) == id {
			items[i] = incoming
			return items
		}
	}
	return append(items, incoming)
}

func requireExisting[T any](items []T, id string, getID func(T) string) error {
	if slices.ContainsFunc(items, func(v T) bool { return getID(v) == id }) {
		return nil
	}
	return errors.New("selected record no longer exists; refresh the workspace")
}

func (s *Service) SaveWorkspace(ctx context.Context, item domain.Workspace) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		var err error
		if item.Name, err = cleanName(item.Name); err != nil {
			return err
		}
		if item.ID == "" {
			item.ID = newID()
		} else if err = requireExisting(state.Workspaces, item.ID, func(v domain.Workspace) string { return v.ID }); err != nil {
			return err
		}
		state.Workspaces = upsert(state.Workspaces, item, item.ID, func(v domain.Workspace) string { return v.ID })
		if state.UI.WorkspaceID == "" {
			state.UI.WorkspaceID = item.ID
		}
		return nil
	})
}

func (s *Service) SaveCollection(ctx context.Context, item domain.Collection) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		var err error
		if item.Name, err = cleanName(item.Name); err != nil {
			return err
		}
		if item.ID == "" {
			item.ID = newID()
		} else if err = requireExisting(state.Collections, item.ID, func(v domain.Collection) string { return v.ID }); err != nil {
			return err
		}
		state.Collections = upsert(state.Collections, item, item.ID, func(v domain.Collection) string { return v.ID })
		return nil
	})
}

func (s *Service) SaveFolder(ctx context.Context, item domain.Folder) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		var err error
		if item.Name, err = cleanName(item.Name); err != nil {
			return err
		}
		if item.ID == "" {
			item.ID = newID()
		} else if err = requireExisting(state.Folders, item.ID, func(v domain.Folder) string { return v.ID }); err != nil {
			return err
		}
		state.Folders = upsert(state.Folders, item, item.ID, func(v domain.Folder) string { return v.ID })
		return nil
	})
}

func (s *Service) SaveRequest(ctx context.Context, item domain.RequestDefinition) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		var err error
		if item.Name, err = cleanName(item.Name); err != nil {
			return err
		}
		if item.ID == "" {
			item.ID = newID()
		} else if err = requireExisting(state.Requests, item.ID, func(v domain.RequestDefinition) string { return v.ID }); err != nil {
			return err
		}
		state.Requests = upsert(state.Requests, item, item.ID, func(v domain.RequestDefinition) string { return v.ID })
		return nil
	})
}

func (s *Service) SaveEnvironment(ctx context.Context, item domain.Environment) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		var err error
		if item.Name, err = cleanName(item.Name); err != nil {
			return err
		}
		var previous *domain.Environment
		if item.ID == "" {
			item.ID = newID()
		} else {
			for i := range state.Environments {
				if state.Environments[i].ID == item.ID {
					previous = &state.Environments[i]
					break
				}
			}
			if previous == nil {
				return errors.New("environment no longer exists")
			}
		}
		for i := range item.Variables {
			v := &item.Variables[i]
			if err := security.ValidateVariableName(v.Name); err != nil {
				return err
			}
			if v.Secret && v.Value != "" {
				return errors.New("secret values must be saved directly to the system secret store")
			}
			if !v.Secret && v.SecretRef != "" {
				return errors.New("public variables cannot have secret references")
			}
			oldRef := ""
			if previous != nil {
				for _, old := range previous.Variables {
					if old.ID == v.ID && old.Secret {
						oldRef = old.SecretRef
						break
					}
				}
			}
			if v.SecretRef != "" && v.SecretRef != oldRef {
				return errors.New("secret reference is not owned by the selected variable")
			}
			if v.ID == "" {
				v.ID = newID()
			}
			if v.Secret {
				v.Value = ""
			}
		}
		state.Environments = upsert(state.Environments, item, item.ID, func(v domain.Environment) string { return v.ID })
		return nil
	})
}

func findVariable(state *domain.State, environmentID, variableID string) (*domain.Variable, error) {
	for i := range state.Environments {
		if state.Environments[i].ID == environmentID {
			for j := range state.Environments[i].Variables {
				if state.Environments[i].Variables[j].ID == variableID {
					return &state.Environments[i].Variables[j], nil
				}
			}
		}
	}
	return nil, errors.New("selected environment variable no longer exists")
}

func (s *Service) SetSecret(ctx context.Context, environmentID, variableID, value string) (domain.State, error) {
	if value == "" || len(value) > 64<<10 {
		return domain.State{}, errors.New("secret must contain 1..65536 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.repo.Load(ctx)
	if err != nil {
		return domain.State{}, err
	}
	v, err := findVariable(&state, environmentID, variableID)
	if err != nil {
		return domain.State{}, err
	}
	if !v.Secret {
		return domain.State{}, errors.New("select the secret variable type before saving its value")
	}
	ref := newID()
	// A durable prequeue also recovers a crash between the vault write and DB
	// commit. A successful Save promotes the new live reference out of the queue.
	if err = s.repo.QueueSecretCleanup(ctx, ref); err != nil {
		return domain.State{}, err
	}
	if err = s.vault.Set(ctx, ref, value); err != nil {
		s.drainSecrets(ctx)
		if ctx.Err() != nil {
			return domain.State{}, ctx.Err()
		}
		return domain.State{}, errors.New("system secret store could not save the secret; unlock or configure the OS keychain")
	}
	v.SecretRef = ref
	v.Value = ""
	result, err := s.repo.Save(ctx, state, state.Revision)
	if err != nil {
		s.drainSecrets(ctx)
		return domain.State{}, err
	}
	s.drainSecrets(ctx)
	return result, nil
}

func (s *Service) deleteVault(ctx context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	if err := s.vault.Delete(ctx, ref); err != nil && !errors.Is(err, security.ErrNotFound) {
		return errors.New("system secret store could not remove the secret; unlock the OS keychain and retry")
	}
	return nil
}

func (s *Service) DeleteSecret(ctx context.Context, environmentID, variableID string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		v, err := findVariable(state, environmentID, variableID)
		if err != nil {
			return err
		}
		v.SecretRef = ""
		return nil
	})
}

func normalizeUI(state *domain.State) {
	if !slices.ContainsFunc(state.Workspaces, func(v domain.Workspace) bool { return v.ID == state.UI.WorkspaceID }) {
		state.UI = domain.UIState{OpenRequestIDs: []string{}}
		if len(state.Workspaces) > 0 {
			state.UI.WorkspaceID = state.Workspaces[0].ID
		}
	}
	state.UI.OpenRequestIDs = slices.DeleteFunc(state.UI.OpenRequestIDs, func(id string) bool {
		return !slices.ContainsFunc(state.Requests, func(v domain.RequestDefinition) bool { return v.ID == id && v.WorkspaceID == state.UI.WorkspaceID })
	})
	if !slices.Contains(state.UI.OpenRequestIDs, state.UI.ActiveRequestID) {
		state.UI.ActiveRequestID = ""
		if len(state.UI.OpenRequestIDs) > 0 {
			state.UI.ActiveRequestID = state.UI.OpenRequestIDs[0]
		}
	}
}

func (s *Service) DeleteRequest(ctx context.Context, id string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		if err := requireExisting(state.Requests, id, func(v domain.RequestDefinition) string { return v.ID }); err != nil {
			return err
		}
		state.Requests = slices.DeleteFunc(state.Requests, func(v domain.RequestDefinition) bool { return v.ID == id })
		normalizeUI(state)
		return nil
	})
}

func (s *Service) DeleteFolder(ctx context.Context, id string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		if err := requireExisting(state.Folders, id, func(v domain.Folder) string { return v.ID }); err != nil {
			return err
		}
		removed := map[string]bool{id: true}
		for changed := true; changed; {
			changed = false
			for _, folder := range state.Folders {
				if removed[folder.ParentID] && !removed[folder.ID] {
					removed[folder.ID] = true
					changed = true
				}
			}
		}
		state.Folders = slices.DeleteFunc(state.Folders, func(v domain.Folder) bool { return removed[v.ID] })
		state.Requests = slices.DeleteFunc(state.Requests, func(v domain.RequestDefinition) bool { return removed[v.FolderID] })
		normalizeUI(state)
		return nil
	})
}

func (s *Service) DeleteCollection(ctx context.Context, id string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		if err := requireExisting(state.Collections, id, func(v domain.Collection) string { return v.ID }); err != nil {
			return err
		}
		state.Collections = slices.DeleteFunc(state.Collections, func(v domain.Collection) bool { return v.ID == id })
		state.Folders = slices.DeleteFunc(state.Folders, func(v domain.Folder) bool { return v.CollectionID == id })
		state.Requests = slices.DeleteFunc(state.Requests, func(v domain.RequestDefinition) bool { return v.CollectionID == id })
		normalizeUI(state)
		return nil
	})
}

func (s *Service) DeleteEnvironment(ctx context.Context, id string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		if err := requireExisting(state.Environments, id, func(v domain.Environment) string { return v.ID }); err != nil {
			return err
		}
		state.Environments = slices.DeleteFunc(state.Environments, func(v domain.Environment) bool { return v.ID == id })
		for i := range state.Workspaces {
			if state.Workspaces[i].ActiveEnvironmentID == id {
				state.Workspaces[i].ActiveEnvironmentID = ""
			}
		}
		return nil
	})
}

func (s *Service) DeleteWorkspace(ctx context.Context, id string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		if err := requireExisting(state.Workspaces, id, func(v domain.Workspace) string { return v.ID }); err != nil {
			return err
		}
		if len(state.Workspaces) == 1 {
			return errors.New("create another workspace before removing the last one")
		}
		state.Workspaces = slices.DeleteFunc(state.Workspaces, func(v domain.Workspace) bool { return v.ID == id })
		state.Collections = slices.DeleteFunc(state.Collections, func(v domain.Collection) bool { return v.WorkspaceID == id })
		state.Folders = slices.DeleteFunc(state.Folders, func(v domain.Folder) bool { return v.WorkspaceID == id })
		state.Requests = slices.DeleteFunc(state.Requests, func(v domain.RequestDefinition) bool { return v.WorkspaceID == id })
		state.Environments = slices.DeleteFunc(state.Environments, func(v domain.Environment) bool { return v.WorkspaceID == id })
		state.ProtoSchemas = slices.DeleteFunc(state.ProtoSchemas, func(v domain.ProtoSchema) bool { return v.WorkspaceID == id })
		normalizeUI(state)
		return nil
	})
}

func (s *Service) SaveSettings(ctx context.Context, settings domain.Settings) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error { state.Settings = settings; return nil })
}

func (s *Service) SaveUI(ctx context.Context, ui domain.UIState) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error { state.UI = ui; return nil })
}
