package instancelock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSingleDataDirectoryOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.lock")
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if second, err := Acquire(path); !errors.Is(err, ErrInUse) {
		if second != nil {
			second()
		}
		t.Fatalf("second process may own the secret cleanup queue: %v", err)
	}
	release()
	release()
	third, err := Acquire(path)
	if err != nil {
		t.Fatal("lock was not released", err)
	}
	third()
}
