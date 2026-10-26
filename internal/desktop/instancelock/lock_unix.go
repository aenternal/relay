//go:build linux || darwin

package instancelock

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockFile(file *os.File)     { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }
