package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"relay/internal/domain"
)

func webSocketFixture() domain.State {
	s := fixture()
	s.Requests[0].Protocol, s.Requests[0].HTTP = "websocket", nil
	s.Requests[0].WebSocket = &domain.WebSocketDefinition{
		URL: "wss://{{host}}/socket?token={{token}}", Headers: []domain.Header{{Name: "Authorization", Value: "Bearer {{token}}", Enabled: true}},
		Query: []domain.Header{{Name: "channel", Value: "updates", Enabled: true}}, Subprotocols: []string{"echo.v1"},
		TLS:   domain.HTTPTLS{CAFileID: "private-ws-ca-grant", ClientCertFileID: "private-ws-cert-grant", ClientKeyFileID: "private-ws-key-grant", ServerName: "example.test"},
		Proxy: domain.HTTPProxy{Mode: "custom", URL: "http://{{proxyHost}}:8080"}, ConnectTimeoutMS: 1000, WriteTimeoutMS: 2000,
		MaxMessageBytes: 1 << 20, BodyKind: "binary", BodyEncoding: "hex", Body: "private-ws-body-canary-93a", FileID: "private-ws-body-grant",
	}
	return s
}

func TestWebSocketDefinitionPersistenceAndValidation(t *testing.T) {
	db, filename := openTestDB(t)
	source := webSocketFixture()
	saved, err := db.Save(t.Context(), source, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := *source.Requests[0].WebSocket
	want.Body, want.FileID, want.TLS.CAFileID, want.TLS.ClientCertFileID, want.TLS.ClientKeyFileID = "", "", "", "", ""
	if !reflect.DeepEqual(want, *saved.Requests[0].WebSocket) {
		t.Fatal("WebSocket settings changed or transient data persisted")
	}
	saved.Requests[0].WebSocket.Headers[0].Value = "mutated"
	if source.Requests[0].WebSocket.Headers[0].Value != "Bearer {{token}}" {
		t.Fatal("caller headers were aliased")
	}
	for _, name := range []string{filename, filename + "-wal"} {
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{source.Requests[0].WebSocket.Body, source.Requests[0].WebSocket.FileID, source.Requests[0].WebSocket.TLS.CAFileID, source.Requests[0].WebSocket.TLS.ClientKeyFileID} {
			if bytes.Contains(data, []byte(canary)) {
				t.Fatal("transient WebSocket data reached SQLite/WAL")
			}
		}
	}
	for name, change := range map[string]func(*domain.WebSocketDefinition){
		"header credential": func(w *domain.WebSocketDefinition) { w.Headers[0].Value = "private-header-canary" },
		"disabled query credential": func(w *domain.WebSocketDefinition) {
			w.Query = []domain.Header{{Name: "auth", Value: "private-query-canary"}}
		},
		"URL credential":   func(w *domain.WebSocketDefinition) { w.URL = "ws://user:private-userinfo-canary@example.test/" },
		"URL query":        func(w *domain.WebSocketDefinition) { w.URL = "ws://example.test/?key=private-query-canary" },
		"scheme":           func(w *domain.WebSocketDefinition) { w.URL = "https://example.test/" },
		"fragment":         func(w *domain.WebSocketDefinition) { w.URL = "ws://example.test/#fragment" },
		"message limit":    func(w *domain.WebSocketDefinition) { w.MaxMessageBytes = 8 << 20 },
		"negative timeout": func(w *domain.WebSocketDefinition) { w.ReadTimeoutMS = -1 },
		"encoding":         func(w *domain.WebSocketDefinition) { w.BodyEncoding = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			s := webSocketFixture()
			change(s.Requests[0].WebSocket)
			if _, err := db.Save(t.Context(), s, 1); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "canary") {
				t.Fatalf("unsafe definition accepted or leaked: %v", err)
			}
		})
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Load(t.Context())
	if err != nil || !reflect.DeepEqual(want, *loaded.Requests[0].WebSocket) {
		t.Fatalf("restart lost WebSocket definition: %v", err)
	}
}

func historyFixture(id string, payloads bool) domain.WebSocketSavedSession {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	return domain.WebSocketSavedSession{ID: id, WorkspaceID: "w", RequestID: "r", SavedAt: stamp, IncludePayloads: payloads,
		Execution: domain.WebSocketExecution{ID: "live-" + id, WorkspaceID: "w", RequestID: "r", State: "closed", URL: "ws://example.test/echo?token=history-private-canary", StartedAt: stamp, FinishedAt: stamp, HandshakeStatusCode: 101, HandshakeHeaders: []domain.Header{{Name: "Authorization", Value: "history-private-canary"}, {Name: "X-Fixture", Value: "safe"}}, CloseCode: 1000, CloseReason: "history-private-canary", CleanClose: true, Warnings: []string{}},
		Events:    []domain.WebSocketEvent{{Sequence: 1, Type: "received", Direction: "in", MessageType: "text", Timestamp: stamp, Text: "history-private-canary", DataBase64: "aGlzdG9yeS1wcml2YXRlLWNhbmFyeQ==", Bytes: 22, Warnings: []string{}}}}
}

func TestWebSocketSavedHistoryOptInRestartAndSnapshotOwnership(t *testing.T) {
	db, filename := openTestDB(t)
	state, err := db.Save(t.Context(), webSocketFixture(), 0)
	if err != nil {
		t.Fatal(err)
	}
	input := historyFixture("metadata", false)
	saved, err := db.SaveWebSocketHistory(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Events[0].Text != "" || saved.Events[0].DataBase64 != "" || len(saved.Execution.HandshakeHeaders) != 0 || strings.Contains(saved.Execution.URL, "?") || saved.Execution.CloseReason != "" {
		t.Fatal("metadata-only save retained payloads or handshake credentials")
	}
	if input.Events[0].Text == "" || len(input.Execution.HandshakeHeaders) != 2 {
		t.Fatal("history save modified caller")
	}
	for _, name := range []string{filename, filename + "-wal"} {
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("history-private-canary")) {
			t.Fatal("payload persisted without history opt-in")
		}
	}
	if _, err = db.GetWebSocketHistory(t.Context(), "different-workspace", saved.ID); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross-workspace history exposed")
	}
	state.Collections[0].Name = "Autosaved rename"
	state, err = db.Save(t.Context(), state, state.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetWebSocketHistory(t.Context(), "w", saved.ID); err != nil {
		t.Fatal("ordinary snapshot autosave erased history")
	}
	payload, err := db.SaveWebSocketHistory(t.Context(), historyFixture("payload", true))
	if err != nil {
		t.Fatal(err)
	}
	if payload.Events[0].Text == "" || len(payload.Execution.HandshakeHeaders) != 1 || payload.Execution.HandshakeHeaders[0].Name != "X-Fixture" {
		t.Fatal("payload opt-in or sensitive header stripping failed")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := db.ListWebSocketHistory(t.Context(), "w")
	if err != nil || len(items) != 2 {
		t.Fatalf("history restart: %v", err)
	}
	restored, err := db.GetWebSocketHistory(t.Context(), "w", "payload")
	if err != nil || !reflect.DeepEqual(restored, payload) {
		t.Fatalf("history payload changed on restart: %v", err)
	}
	if err = db.DeleteWebSocketHistory(t.Context(), "other", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetWebSocketHistory(t.Context(), "w", "payload"); err != nil {
		t.Fatal("cross-workspace delete removed history")
	}
	if err = db.DeleteWebSocketHistory(t.Context(), "w", "metadata"); err != nil {
		t.Fatal(err)
	}
	state.Requests = nil
	state.UI = domain.UIState{WorkspaceID: "w"}
	if _, err = db.Save(t.Context(), state, state.Revision); err != nil {
		t.Fatal(err)
	}
	items, err = db.ListWebSocketHistory(t.Context(), "w")
	if err != nil || len(items) != 0 {
		t.Fatal("request deletion left orphaned sessions")
	}
	if _, err = db.SaveWebSocketHistory(t.Context(), historyFixture("orphan", false)); !errors.Is(err, ErrInvalid) {
		t.Fatal("orphaned session accepted")
	}
}

func TestWebSocketHistoryQuotasAndPreUnmarshalLimits(t *testing.T) {
	db, _ := openTestDB(t)
	if _, err := db.Save(t.Context(), webSocketFixture(), 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := db.SaveWebSocketHistory(t.Context(), historyFixture(fmt.Sprintf("small-%02d", i), false)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := db.ListWebSocketHistory(t.Context(), "w")
	if err != nil || len(items) != 16 {
		t.Fatalf("count quota: %d %v", len(items), err)
	}
	if _, err = db.GetWebSocketHistory(t.Context(), "w", "small-00"); !errors.Is(err, ErrInvalid) {
		t.Fatal("oldest history not evicted")
	}
	large := historyFixture("large", true)
	large.Events = nil
	for i := 0; i < 180; i++ {
		event := historyFixture("unused", true).Events[0]
		event.Sequence = i + 1
		event.Text = strings.Repeat("x", 32<<10)
		event.DataBase64 = ""
		large.Events = append(large.Events, event)
	}
	for i := 0; i < 7; i++ {
		large.ID = fmt.Sprintf("large-%d", i)
		if _, err = db.SaveWebSocketHistory(t.Context(), large); err != nil {
			t.Fatal(err)
		}
	}
	var total, count int
	if err = db.sql.QueryRow("SELECT count(*),sum(bytes) FROM websocket_sessions").Scan(&count, &total); err != nil || total > MaxWebSocketHistoryBytes || count > 16 {
		t.Fatalf("byte quota: %d bytes / %d records: %v", total, count, err)
	}
	for _, bad := range []string{`{"events":[` + strings.Repeat("{},", 2001) + `{}]}`, strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33), `{"events":[]} {}`} {
		if err := validateWebSocketHistoryJSON([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatal("structurally unbounded history accepted before typed unmarshal")
		}
	}
	bad := historyFixture("bad-stamp", false)
	bad.Execution.StartedAt = "2026-10-25T00:00:00." + strings.Repeat("1", 10000) + "Z"
	if _, err = db.SaveWebSocketHistory(t.Context(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded fractional timestamp accepted")
	}
	masked := historyFixture("masked-url", false)
	masked.Execution.URL = "ws://[REDACTED]27.0.0.[REDACTED]:[REDACTED]234/echo?token=private-canary"
	stored, err := db.SaveWebSocketHistory(t.Context(), masked)
	if err != nil || strings.Contains(stored.Execution.URL, "?") || !strings.Contains(stored.Execution.URL, "[REDACTED]") {
		t.Fatalf("redacted display URL could not be saved safely: %v", err)
	}
}

func TestVersionThreeWebSocketDraftUpgradesToSessionHistory(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "legacy-v3.db")
	legacy, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_secret_cleanup.sql", "003_proto_schemas.sql"} {
		data, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		files["migrations/"+name] = &fstest.MapFile{Data: data}
	}
	if err = migrateFiles(context.Background(), legacy, files); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`INSERT INTO workspaces(id,payload) VALUES('w','{"id":"w","name":"Legacy"}')`, `INSERT INTO collections(id,workspace_id,payload) VALUES('c','w','{"id":"c","workspaceId":"w","name":"Legacy"}')`, `INSERT INTO requests(id,workspace_id,collection_id,payload) VALUES('r','w','c','{"id":"r","workspaceId":"w","collectionId":"c","name":"Legacy WebSocket","protocol":"websocket","websocket":{"url":"ws://localhost:9000/echo"}}')`} {
		if _, err = legacy.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := db.Load(t.Context())
	if err != nil || len(state.Requests) != 1 || state.Requests[0].WebSocket.URL != "ws://localhost:9000/echo" {
		t.Fatalf("legacy draft lost: %v", err)
	}
	if _, err = db.SaveWebSocketHistory(t.Context(), historyFixture("upgraded", false)); err != nil {
		t.Fatal(err)
	}
}
