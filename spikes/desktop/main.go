package main

import (
	"context"
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed assets/*
var assets embed.FS

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &SpikeService{ctx: ctx}
	app := application.New(application.Options{
		Name:        "Relay — stage 0",
		Description: "Wails v3 event batching and cancellation smoke test",
		Services:    []application.Service{application.NewService(service)},
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		OnShutdown:  cancel,
	})
	application.RegisterEvent[Batch]("spike:batch")
	service.emit = func(ctx context.Context, batch Batch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		app.Event.Emit("spike:batch", batch)
		return nil
	}
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Relay — проверка Wails v3", Width: 900, Height: 600, URL: "/",
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
