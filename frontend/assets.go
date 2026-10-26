// Package frontend embeds the Vite production output for the native shell.
package frontend

import (
	"embed"
	"io/fs"
)

// The .keep sentinel allows headless core tests before the frontend build.
// Desktop release builds must first run npm ci && npm run build in this directory.
//
//go:embed all:dist
var embedded embed.FS

func Assets() fs.FS {
	assets, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic("embedded frontend directory is missing")
	}
	return assets
}
