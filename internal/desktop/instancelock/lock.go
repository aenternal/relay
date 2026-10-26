// Package instancelock serializes desktop processes sharing a data directory.
// The OS releases the lock after a crash; the lock file is never unlinked.
package instancelock

import (
	"errors"
	"os"
	"sync"
)

var ErrInUse = errors.New("Relay уже использует это хранилище; закройте другое окно приложения")

func Acquire(path string) (func(), error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("application lock must be a regular file")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("application lock could not be opened")
	}
	if err = lockFile(file); err != nil {
		file.Close()
		return nil, ErrInUse
	}
	var once sync.Once
	return func() { once.Do(func() { unlockFile(file); _ = file.Close() }) }, nil
}
