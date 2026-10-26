package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"

	"relay/frontend"
	"relay/internal/app"
	"relay/internal/buildinfo"
	"relay/internal/desktop/instancelock"
	"relay/internal/security"
	"relay/internal/storage"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.JSON())
		return
	}
	if err := run(); err != nil {
		// Never log command arguments, request data or the database contents.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := configureServerPolicy(); err != nil {
		return err
	}
	if _, err := fs.Stat(frontend.Assets(), "index.html"); err != nil {
		return fmt.Errorf("desktop frontend is missing: build frontend before starting Relay")
	}
	dir := os.Getenv("RELAY_DATA_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("application data directory is unavailable")
		}
		dir = filepath.Join(base, "relay")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("application data directory cannot be created")
	}
	release, err := instancelock.Acquire(filepath.Join(dir, "relay.lock"))
	if err != nil {
		return err
	}
	defer release()
	db, err := storage.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := app.New(ctx, db, security.NewOSStore())
	if err != nil {
		return err
	}
	defer app.Shutdown(service)
	files := &FileService{service: service}
	desktop := application.New(application.Options{
		Name: "Relay", Description: "Локальный API-клиент",
		Services: []application.Service{application.NewService(service), application.NewService(files)},
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(frontend.Assets()), Middleware: secureAssets, DisableLogging: true},
		Mac:      application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		OnShutdown: func() {
			_ = app.Shutdown(service)
			cancel()
		},
	})
	files.shell = desktop
	desktop.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Relay", Width: 1440, Height: 900, URL: "/",
	})
	return desktop.Run()
}
