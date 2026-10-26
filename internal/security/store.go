// Package security resolves environment variables and keeps secret values in an
// operating system credential store. It never persists a cleartext fallback.
package security

import (
	"context"
	"errors"
	"sync"

	"github.com/zalando/go-keyring"
)

var (
	ErrUnavailable = errors.New("secret store unavailable")
	ErrNotFound    = errors.New("secret not found")
	ErrInvalidKey  = errors.New("invalid secret reference")
)

// Store errors must not contain values or credentials. Implementations outside
// this package are treated as untrusted when resolving variables.
type Store interface {
	Set(context.Context, string, string) error
	Get(context.Context, string) (string, error)
	Delete(context.Context, string) error
}

type keyringBackend interface {
	Set(service, key, value string) error
	Get(service, key string) (string, error)
	Delete(service, key string) error
}

type nativeKeyring struct{}

func (nativeKeyring) Set(service, key, value string) error    { return keyring.Set(service, key, value) }
func (nativeKeyring) Get(service, key string) (string, error) { return keyring.Get(service, key) }
func (nativeKeyring) Delete(service, key string) error        { return keyring.Delete(service, key) }

// OSStore uses Secret Service on Linux, Keychain on macOS, and Credential
// Manager on Windows, as provided by go-keyring. Calls are synchronous: the
// upstream API cannot interrupt an operating system prompt or a pending call.
// Context is checked before and after each call without creating orphaned
// goroutines. A canceled Set/Delete may therefore have completed in the OS.
type OSStore struct {
	service string
	backend keyringBackend
}

func NewOSStore() *OSStore { return NewOSStoreWithService("relay") }

func NewOSStoreWithService(service string) *OSStore {
	return &OSStore{service: service, backend: nativeKeyring{}}
}

func (*OSStore) Backend() string { return "os-keyring" }

// Available makes a read-only probe; a missing probe item means the store is
// reachable. It never writes or deletes a user's keychain entries.
func (s *OSStore) Available(ctx context.Context) error {
	_, err := s.Get(ctx, "relay.availability-probe")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (s *OSStore) Set(ctx context.Context, key, value string) error {
	if err := s.check(ctx, key); err != nil {
		return err
	}
	err := s.backend.Set(s.service, key, value)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return safeStoreError(err)
}

func (s *OSStore) Get(ctx context.Context, key string) (string, error) {
	if err := s.check(ctx, key); err != nil {
		return "", err
	}
	value, err := s.backend.Get(s.service, key)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", safeStoreError(err)
	}
	return value, nil
}

func (s *OSStore) Delete(ctx context.Context, key string) error {
	if err := s.check(ctx, key); err != nil {
		return err
	}
	err := s.backend.Delete(s.service, key)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return safeStoreError(err)
}

func (s *OSStore) check(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.backend == nil || s.service == "" {
		return ErrUnavailable
	}
	return validateKey(key)
}

func validateKey(key string) error {
	if key == "" || len(key) > 512 {
		return ErrInvalidKey
	}
	for _, c := range key {
		if c < 0x20 || c == 0x7f {
			return ErrInvalidKey
		}
	}
	return nil
}

// Do not wrap backend errors: DBus peers and system commands may embed values,
// account names, or other private content in their error strings.
func safeStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, keyring.ErrNotFound), errors.Is(err, ErrNotFound):
		return ErrNotFound
	case errors.Is(err, ErrInvalidKey):
		return ErrInvalidKey
	default:
		return ErrUnavailable
	}
}

// MemoryStore is an explicit, process-local test utility. Production startup
// uses OSStore; it must never silently switch to this store after an OS error.
type MemoryStore struct {
	mu     sync.RWMutex
	values map[string]string
}

func NewMemoryStore() *MemoryStore   { return &MemoryStore{values: make(map[string]string)} }
func (*MemoryStore) Backend() string { return "memory" }

func (s *MemoryStore) Set(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateKey(key); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.values[key]
	if !ok {
		return "", ErrNotFound
	}
	return value, nil
}

func (s *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[key]; !ok {
		return ErrNotFound
	}
	delete(s.values, key)
	return nil
}
