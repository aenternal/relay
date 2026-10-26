package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
)

// The server target is a local development shell with the same filesystem and
// secret capabilities as the desktop application, not a multi-user web service.
func configureServerPolicy() error {
	if nativeFilesAvailable {
		return nil
	}
	host := os.Getenv("WAILS_SERVER_HOST")
	if host == "" {
		return os.Setenv("WAILS_SERVER_HOST", "127.0.0.1")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("Relay development server must bind a loopback address (127.0.0.1, ::1 or localhost)")
	}
	return nil
}

// The webview renders API content as text. CSP also keeps injected content from
// loading scripts, remote images, frames or making target API requests in JS.
func secureAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/wails/") {
			header.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
