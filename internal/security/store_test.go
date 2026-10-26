package security

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

type fakeKeyring struct {
	set    func(string, string, string) error
	get    func(string, string) (string, error)
	delete func(string, string) error
}

func (f fakeKeyring) Set(s, k, v string) error        { return f.set(s, k, v) }
func (f fakeKeyring) Get(s, k string) (string, error) { return f.get(s, k) }
func (f fakeKeyring) Delete(s, k string) error        { return f.delete(s, k) }

func TestOSStoreSanitizesBackendErrors(t *testing.T) {
	secret := "never-print-this-secret"
	s := &OSStore{service: "relay-test", backend: fakeKeyring{
		set: func(s, k, v string) error {
			if s != "relay-test" || k != "reference" || v != secret {
				t.Fatal("wrong backend arguments")
			}
			return errors.New(secret)
		},
		get:    func(string, string) (string, error) { return secret, errors.New(secret) },
		delete: func(string, string) error { return errors.New(secret) },
	}}
	for _, err := range []error{s.Set(t.Context(), "reference", secret), s.Delete(t.Context(), "reference")} {
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	value, err := s.Get(t.Context(), "reference")
	if value != "" || !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe get: %q %v", value, err)
	}
}

func TestOSStoreAvailabilityProbeIsReadOnly(t *testing.T) {
	calls := 0
	s := &OSStore{service: "relay", backend: fakeKeyring{
		set:    func(string, string, string) error { t.Fatal("probe wrote keychain"); return nil },
		delete: func(string, string) error { t.Fatal("probe deleted keychain"); return nil },
		get: func(service, key string) (string, error) {
			calls++
			if service != "relay" || key != "relay.availability-probe" {
				t.Fatal("unexpected probe")
			}
			return "", keyring.ErrNotFound
		},
	}}
	if err := s.Available(t.Context()); err != nil || calls != 1 {
		t.Fatalf("availability: %v, calls %d", err, calls)
	}
}

func TestOSStoreContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := &OSStore{service: "relay", backend: fakeKeyring{
		get: func(string, string) (string, error) { t.Fatal("called canceled backend"); return "", nil },
	}}
	if _, err := s.Get(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	s.backend = fakeKeyring{get: func(string, string) (string, error) { cancel(); return "secret", nil }}
	if value, err := s.Get(ctx, "key"); value != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %q %v", value, err)
	}
}

func TestMemoryStoreLifecycleAndMissing(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Set(t.Context(), "a", "secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(t.Context(), "a"); err != nil || got != "secret" {
		t.Fatalf("get: %q %v", got, err)
	}
	if err := s.Delete(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Set(t.Context(), "\nunsafe", "secret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal(err)
	}
}
