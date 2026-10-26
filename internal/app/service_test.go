package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"
	"relay/internal/storage"
)

func openApp(t *testing.T, filename string, vault security.Store) (*storage.DB, *app.Service) {
	t.Helper()
	db, err := storage.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc, err := app.New(t.Context(), db, vault)
	if err != nil {
		t.Fatal(err)
	}
	return db, svc
}

func loadState(t *testing.T, svc *app.Service) domain.State {
	t.Helper()
	state, err := svc.LoadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func mustState(t *testing.T) func(domain.State, error) domain.State {
	t.Helper()
	return func(state domain.State, err error) domain.State {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
}

func environment(t *testing.T, state domain.State, id string) domain.Environment {
	t.Helper()
	for _, env := range state.Environments {
		if env.ID == id {
			return env
		}
	}
	t.Fatal("environment not found")
	return domain.Environment{}
}

func tokenVariable(t *testing.T, state domain.State, envID string) domain.Variable {
	t.Helper()
	for _, v := range environment(t, state, envID).Variables {
		if v.Name == "token" {
			return v
		}
	}
	t.Fatal("token variable not found")
	return domain.Variable{}
}

func addToken(t *testing.T, svc *app.Service) (domain.State, string, string) {
	t.Helper()
	state := loadState(t, svc)
	env := state.Environments[0]
	env.Variables = append(env.Variables, domain.Variable{Name: "token", Secret: true})
	state = mustState(t)(svc.SaveEnvironment(t.Context(), env))
	v := tokenVariable(t, state, env.ID)
	return state, env.ID, v.ID
}

func assertNoSecretOnDisk(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range files {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Fatalf("credential leaked to %s", filepath.Base(filename))
			}
		}
	}
}

func TestSavedWorkspaceAndSecretSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "relay.sqlite3")
	vault := security.NewMemoryStore()
	db, svc := openApp(t, filename, vault)
	initial := loadState(t, svc)
	if len(initial.Workspaces) != 1 || len(initial.Requests) != 1 || initial.Revision < 1 {
		t.Fatalf("initial state missing: %+v", initial)
	}
	state := mustState(t)(svc.SaveWorkspace(t.Context(), domain.Workspace{Name: "API workspace"}))
	workspace := state.Workspaces[len(state.Workspaces)-1]
	state = mustState(t)(svc.SaveCollection(t.Context(), domain.Collection{Name: "API", WorkspaceID: workspace.ID}))
	collection := state.Collections[len(state.Collections)-1]
	state = mustState(t)(svc.SaveFolder(t.Context(), domain.Folder{Name: "Auth", WorkspaceID: workspace.ID, CollectionID: collection.ID}))
	folder := state.Folders[len(state.Folders)-1]
	state = mustState(t)(svc.SaveEnvironment(t.Context(), domain.Environment{Name: "Local", WorkspaceID: workspace.ID, Variables: []domain.Variable{{Name: "baseUrl", Value: "https://example.com"}, {Name: "token", Secret: true}}}))
	env := state.Environments[len(state.Environments)-1]
	variable := tokenVariable(t, state, env.ID)
	const secret = "stage1-restart-token-3176f246"
	state = mustState(t)(svc.SetSecret(t.Context(), env.ID, variable.ID, secret))
	variable = tokenVariable(t, state, env.ID)
	if variable.SecretRef == "" || variable.Value != "" {
		t.Fatal("secret was not converted to a reference")
	}
	state = mustState(t)(svc.SaveRequest(t.Context(), domain.RequestDefinition{Name: "Me", WorkspaceID: workspace.ID, CollectionID: collection.ID, FolderID: folder.ID, Protocol: "http", HTTP: &domain.HTTPDefinition{Method: "POST", URL: "{{baseUrl}}/me", Version: "auto", Headers: []domain.Header{{Name: "Authorization", Value: "Bearer {{token}}", Enabled: true}}, Body: `{"count":9223372036854775807,"token":"{{token}}"}`, PersistBody: true}}))
	request := state.Requests[len(state.Requests)-1]
	workspace.ActiveEnvironmentID = env.ID
	state = mustState(t)(svc.SaveWorkspace(t.Context(), workspace))
	state = mustState(t)(svc.SaveSettings(t.Context(), domain.Settings{Theme: "dark", ResponsePreviewBytes: 2 << 20, HistoryLimit: 300}))
	state = mustState(t)(svc.SaveUI(t.Context(), domain.UIState{WorkspaceID: workspace.ID, OpenRequestIDs: []string{request.ID}, ActiveRequestID: request.ID}))
	encoded, err := json.Marshal(state)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatal("secret leaked to desktop bridge snapshot")
	}
	assertNoSecretOnDisk(t, dir, secret)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, restarted := openApp(t, filename, vault)
	restored := loadState(t, restarted)
	if !reflect.DeepEqual(state, restored) {
		t.Fatal("saved workspace, tabs, requests, or preferences changed on restart")
	}
	if got := tokenVariable(t, restored, env.ID); got.SecretRef != variable.SecretRef {
		t.Fatal("secret reference changed on restart")
	}
	resolved, err := security.ResolveJSON(t.Context(), request.HTTP.Body, environment(t, restored, env.ID), vault)
	if err != nil || !strings.Contains(resolved.Value, secret) || strings.Contains(resolved.Redacted, secret) {
		t.Fatalf("secret could not be resolved safely: %v", err)
	}
	state = mustState(t)(restarted.DeleteWorkspace(t.Context(), workspace.ID))
	if len(state.Workspaces) != 1 || len(state.Folders) != 0 || len(state.Environments) != 1 || len(state.UI.OpenRequestIDs) != 0 {
		t.Fatal("workspace deletion left descendants or invalid open tabs")
	}
	if _, err := vault.Get(t.Context(), variable.SecretRef); !errors.Is(err, security.ErrNotFound) {
		t.Fatal("workspace credential was not cleaned up")
	}
	assertNoSecretOnDisk(t, dir, secret)
}

type failingRepository struct {
	*storage.DB
	failSave bool
}

func (r *failingRepository) Save(ctx context.Context, state domain.State, revision int64) (domain.State, error) {
	if r.failSave {
		return domain.State{}, errors.New("injected storage failure")
	}
	return r.DB.Save(ctx, state, revision)
}

type faultStore struct {
	*security.MemoryStore
	failDelete bool
	failSet    bool
	setRefs    []string
}

func (s *faultStore) Set(ctx context.Context, ref, value string) error {
	s.setRefs = append(s.setRefs, ref)
	if s.failSet {
		return errors.New("unsafe backend diagnostic containing private token")
	}
	return s.MemoryStore.Set(ctx, ref, value)
}
func (s *faultStore) Delete(ctx context.Context, ref string) error {
	if s.failDelete {
		return errors.New("unsafe backend diagnostic containing private token")
	}
	return s.MemoryStore.Delete(ctx, ref)
}

func TestFailedSavePreservesLiveCredentialAndCleansUncommittedReplacement(t *testing.T) {
	dir := t.TempDir()
	vault := &faultStore{MemoryStore: security.NewMemoryStore()}
	db, _ := openApp(t, filepath.Join(dir, "relay.sqlite3"), vault)
	repo := &failingRepository{DB: db}
	svc, err := app.New(t.Context(), repo, vault)
	if err != nil {
		t.Fatal(err)
	}
	_, envID, varID := addToken(t, svc)
	const oldSecret = "original-safe-token-c46e672a"
	const newSecret = "replacement-safe-token-a5a5afe6"
	state := mustState(t)(svc.SetSecret(t.Context(), envID, varID, oldSecret))
	previousRef := tokenVariable(t, state, envID).SecretRef
	invalidEnvironment := environment(t, state, envID)
	invalidEnvironment.Variables = []domain.Variable{{Name: "duplicate"}, {Name: "duplicate"}}
	if _, err := svc.SaveEnvironment(t.Context(), invalidEnvironment); err == nil {
		t.Fatal("invalid environment was saved")
	}
	if got, err := vault.Get(t.Context(), previousRef); err != nil || got != oldSecret {
		t.Fatal("environment validation failure destroyed persisted credential")
	}
	repo.failSave = true
	if _, err := svc.DeleteSecret(t.Context(), envID, varID); err == nil {
		t.Fatal("injected save failure was ignored")
	}
	if got, err := vault.Get(t.Context(), previousRef); err != nil || got != oldSecret {
		t.Fatal("failed removal destroyed persisted credential")
	}
	if _, err := svc.SetSecret(t.Context(), envID, varID, newSecret); err == nil {
		t.Fatal("injected replacement failure was ignored")
	}
	newRef := vault.setRefs[len(vault.setRefs)-1]
	if newRef == previousRef {
		t.Fatal("replacement overwrote live credential")
	}
	current := loadState(t, svc)
	if tokenVariable(t, current, envID).SecretRef != previousRef {
		t.Fatal("failed replacement changed saved reference")
	}
	if got, err := vault.Get(t.Context(), previousRef); err != nil || got != oldSecret {
		t.Fatal("failed replacement destroyed live credential")
	}
	repo.failSave = false
	if _, err = app.New(t.Context(), repo, vault); err != nil {
		t.Fatal(err)
	}
	if _, err = vault.Get(t.Context(), newRef); !errors.Is(err, security.ErrNotFound) {
		t.Fatal("uncommitted replacement was not cleaned up on restart")
	}
	if refs, err := db.PendingSecretCleanup(t.Context()); err != nil || len(refs) != 0 {
		t.Fatalf("cleanup queue not drained: %v %v", refs, err)
	}
	assertNoSecretOnDisk(t, dir, oldSecret, newSecret)
}

func TestLockedVaultKeepsCleanupQueuedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "relay.sqlite3")
	vault := &faultStore{MemoryStore: security.NewMemoryStore()}
	db, svc := openApp(t, filename, vault)
	_, envID, varID := addToken(t, svc)
	const secret = "locked-store-token-bfa463d7"
	state := mustState(t)(svc.SetSecret(t.Context(), envID, varID, secret))
	ref := tokenVariable(t, state, envID).SecretRef
	vault.failDelete = true
	state = mustState(t)(svc.DeleteSecret(t.Context(), envID, varID))
	if tokenVariable(t, state, envID).SecretRef != "" {
		t.Fatal("secret still configured after committed removal")
	}
	pending, err := db.PendingSecretCleanup(t.Context())
	if err != nil || !slices.Contains(pending, ref) {
		t.Fatalf("failed cleanup was lost: %v %v", pending, err)
	}
	if _, err = app.New(t.Context(), db, vault); err != nil {
		t.Fatal("a locked keychain prevented ordinary application startup")
	}
	pending, err = db.PendingSecretCleanup(t.Context())
	if err != nil || !slices.Contains(pending, ref) {
		t.Fatal("restart discarded pending cleanup")
	}
	vault.failDelete = false
	if _, err = app.New(t.Context(), db, vault); err != nil {
		t.Fatal(err)
	}
	if _, err = vault.Get(t.Context(), ref); !errors.Is(err, security.ErrNotFound) {
		t.Fatal("deferred credential survived successful cleanup")
	}
	pending, err = db.PendingSecretCleanup(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("successful cleanup was not acknowledged")
	}
	assertNoSecretOnDisk(t, dir, secret)
}

func TestSecretWriteFailureDoesNotExposeBackendDiagnostic(t *testing.T) {
	vault := &faultStore{MemoryStore: security.NewMemoryStore(), failSet: true}
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite3"), vault)
	_, envID, varID := addToken(t, svc)
	const secret = "submitted-token-17dcc88a"
	_, err := svc.SetSecret(t.Context(), envID, varID, secret)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "private token") {
		t.Fatalf("unsafe error: %v", err)
	}
	if tokenVariable(t, loadState(t, svc), envID).SecretRef != "" {
		t.Fatal("failed vault write configured the variable")
	}
}
