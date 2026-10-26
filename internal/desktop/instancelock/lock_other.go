//go:build !linux && !darwin && !windows

package instancelock

import (
	"errors"
	"os"
)

func lockFile(*os.File) error { return errors.New("unsupported platform") }
func unlockFile(*os.File)     {}
