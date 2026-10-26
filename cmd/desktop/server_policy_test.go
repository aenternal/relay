//go:build server

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDevelopmentServerOnlyBindsLoopback(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1", "127.12.0.2", "::1", "localhost"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("WAILS_SERVER_HOST", host)
			if err := configureServerPolicy(); err != nil {
				t.Fatal(err)
			}
			if host == "" && os.Getenv("WAILS_SERVER_HOST") != "127.0.0.1" {
				t.Fatal("default bind was not explicit loopback")
			}
		})
	}
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.2", "rebind-secret-canary.test", "localhost:8080", "127.0.0.1.example.test"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("WAILS_SERVER_HOST", host)
			err := configureServerPolicy()
			if err == nil || strings.Contains(err.Error(), "rebind-secret-canary") {
				t.Fatalf("unsafe binding or diagnostic: %v", err)
			}
		})
	}
}

func TestAssetPolicyForbidsActiveRemoteContent(t *testing.T) {
	response := httptest.NewRecorder()
	secureAssets(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("safe local asset")) })).ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/wails/runtime", nil))
	policy := response.Header().Get("Content-Security-Policy")
	for _, required := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "object-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(policy, required) {
			t.Fatalf("missing policy %s", required)
		}
	}
	if strings.Contains(policy, "unsafe-eval") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("unsafe asset response headers")
	}
}
