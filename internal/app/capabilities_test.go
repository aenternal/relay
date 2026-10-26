package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"
	"relay/internal/storage"
)

type blockedAvailability struct {
	*security.MemoryStore
	entered chan struct{}
	release chan struct{}
}

func TestEnvironmentClearPromotesSnapshotBeforeVaultCleanup(t *testing.T) {
	vault := security.NewMemoryStore()
	db, svc := openApp(t, filepath.Join(t.TempDir(), "relay.db"), vault)
	_, envID, variableID := addToken(t, svc)
	saved := mustState(t)(svc.SetSecret(t.Context(), envID, variableID, "clear-token-canary"))
	previousRef := tokenVariable(t, saved, envID).SecretRef
	updated := environment(t, saved, envID)
	updated.Variables = append([]domain.Variable(nil), updated.Variables...)
	for i := range updated.Variables {
		if updated.Variables[i].ID == variableID {
			updated.Variables[i].SecretRef = ""
		}
	}
	cleared := mustState(t)(svc.SaveEnvironment(t.Context(), updated))
	if tokenVariable(t, cleared, envID).SecretRef != "" {
		t.Fatal("explicit clear retained the saved reference")
	}
	if _, err := vault.Get(t.Context(), previousRef); !errors.Is(err, security.ErrNotFound) {
		t.Fatal("retired credential not removed", err)
	}
	if pending, err := db.PendingSecretCleanup(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal("cleanup was not acknowledged", pending, err)
	}
}

func (v *blockedAvailability) Available(context.Context) error {
	close(v.entered)
	<-v.release
	return security.ErrUnavailable
}

func TestUnavailableKeychainProbeDoesNotBlockWorkspace(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vault := &blockedAvailability{MemoryStore: security.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	svc, err := app.New(t.Context(), db, vault)
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan struct{})
	go func() { defer close(completed); svc.GetCapabilities(t.Context()) }()
	<-vault.entered
	defer func() { close(vault.release); <-completed }()
	loaded := make(chan error, 1)
	go func() { _, err := svc.LoadState(t.Context()); loaded <- err }()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("an OS credential prompt blocked ordinary workspace access")
	}
}
