// Command bindings regenerates the desktop bridge with the pinned Wails CLI.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "internal", "app", "service.go")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fmt.Fprintln(os.Stderr, "run bindings from the Relay repository")
			os.Exit(1)
		}
		dir = parent
	}
	command := exec.Command("go", "run", "github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.34", "generate", "bindings", "-ts", "-i", "-b", "-names", "-d", "../../frontend/src/bindings", "-f", "-tags=server", ".")
	command.Dir = filepath.Join(dir, "cmd", "desktop")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		os.Exit(1)
	}
}
