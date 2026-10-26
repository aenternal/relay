package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"relay/internal/domain"
	"testing"
)

func TestHTTPOptionsNeverPersistImplicitBodiesOrFileCapabilities(t *testing.T) {
	db, path := openTestDB(t)
	state := fixture()
	h := state.Requests[0].HTTP
	h.BodyKind = "multipart"
	h.Form = []domain.Header{{Name: "field", Value: "form-canary", Enabled: true}}
	h.Multipart = []domain.HTTPPart{{Name: "part", Value: "multipart-canary", FileID: "file-capability-canary", FileName: "data.bin", Enabled: true}}
	h.FileID = "file-capability-canary"
	h.TLS = domain.HTTPTLS{CAFileID: "ca-capability-canary", ClientKeyFileID: "private-key-capability-canary"}
	h.Auth = domain.HTTPAuth{Type: "bearer", Token: "{{token}}"}
	state.Collections[0].Auth = &domain.HTTPAuth{Type: "basic", Username: "user", Password: "{{token}}"}
	saved, err := db.Save(t.Context(), state, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Form[0].Value != "form-canary" || h.Multipart[0].FileID != "file-capability-canary" {
		t.Fatal("sanitize mutated the live draft")
	}
	clean := saved.Requests[0].HTTP
	if clean.Form[0].Value != "" || clean.Multipart[0].Value != "" || clean.Multipart[0].FileID != "" || clean.TLS.ClientKeyFileID != "" {
		t.Fatal("unsafe persistence", clean)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"form-canary", "multipart-canary", "file-capability-canary", "ca-capability-canary", "private-key-capability-canary"} {
			if bytes.Contains(data, []byte(canary)) {
				t.Fatal("transient value entered SQLite/WAL")
			}
		}
	}
	state.Requests[0].HTTP.PersistBody = true
	saved, err = db.Save(t.Context(), state, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Requests[0].HTTP.Multipart[0].Value != "multipart-canary" {
		t.Fatal("explicit body opt-in lost multipart value")
	}
}

func TestHTTPStoredAuthRequiresReferencesAtEveryScope(t *testing.T) {
	for _, scope := range []string{"request", "collection", "folder", "query", "proxy"} {
		t.Run(scope, func(t *testing.T) {
			db, _ := openTestDB(t)
			state := fixture()
			auth := &domain.HTTPAuth{Type: "bearer", Token: "secret-literal-canary"}
			switch scope {
			case "request":
				state.Requests[0].HTTP.Auth = *auth
			case "collection":
				state.Collections[0].Auth = auth
			case "folder":
				state.Folders[0].Auth = auth
			case "query":
				state.Requests[0].HTTP.Query = []domain.Header{{Name: "access_token", Value: auth.Token}}
			case "proxy":
				state.Requests[0].HTTP.Proxy = domain.HTTPProxy{Mode: "custom", URL: "http://user:secret-literal-canary@localhost:8080"}
			}
			_, err := db.Save(t.Context(), state, 0)
			if !errors.Is(err, ErrInvalid) {
				t.Fatal("literal credentials accepted", err)
			}
			if bytes.Contains([]byte(err.Error()), []byte(auth.Token)) {
				t.Fatal("error disclosed credentials")
			}
		})
	}
}
