package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"
)

type wsAppFixture struct {
	url      string
	requests chan *http.Request
	closed   chan error
}

func startWSAppFixture(t *testing.T) *wsAppFixture {
	t.Helper()
	fixture := &wsAppFixture{requests: make(chan *http.Request, 8), closed: make(chan error, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests <- r.Clone(context.Background())
		w.Header().Add("X-Reply", "first")
		w.Header().Add("X-Reply", "second")
		w.Header().Set("X-Echo-Authorization", r.Header.Get("Authorization"))
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"relay.v1"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(4 << 20)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				fixture.closed <- err
				return
			}
			if kind == websocket.MessageText && string(data) == "peer-close" {
				_ = conn.Close(websocket.StatusPolicyViolation, "policy canary")
				return
			}
			if kind == websocket.MessageText && string(data) == "abrupt" {
				_ = conn.CloseNow()
				return
			}
			if err := conn.Write(ctx, kind, data); err != nil {
				return
			}
		}
	}))
	fixture.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(server.Close)
	return fixture
}

func newWSApp(t *testing.T, endpoint string, vault security.Store) (*app.Service, domain.RequestDefinition, string) {
	t.Helper()
	dir := t.TempDir()
	_, svc := openApp(t, filepath.Join(dir, "ws.db"), vault)
	t.Cleanup(func() {
		if err := app.ShutdownWebSocket(svc); err != nil {
			t.Error(err)
		}
	})
	state := loadState(t, svc)
	request := state.Requests[0]
	request.Protocol = "websocket"
	request.HTTP = nil
	request.WebSocket = &domain.WebSocketDefinition{URL: endpoint, Headers: []domain.Header{}, Query: []domain.Header{}, Subprotocols: []string{}}
	if _, err := svc.SaveRequest(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return svc, request, dir
}

func pollWS(t *testing.T, svc *app.Service, id string, predicate func(domain.WebSocketBatch) bool) domain.WebSocketBatch {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		batch, err := svc.PollWebSocket(t.Context(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(batch) {
			return batch
		}
		if time.Now().After(deadline) {
			t.Fatalf("WebSocket condition timed out: %+v events=%+v", batch.Execution, batch.Events)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func connectWS(t *testing.T, svc *app.Service, request domain.RequestDefinition) domain.WebSocketExecution {
	t.Helper()
	started, err := svc.ConnectWebSocket(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != "connecting" {
		t.Fatalf("connect did not return immediately: %+v", started)
	}
	result := pollWS(t, svc, started.ID, func(batch domain.WebSocketBatch) bool { return batch.Execution.State != "connecting" }).Execution
	if result.State != "open" {
		t.Fatalf("connection failed: %+v", result)
	}
	return result
}
func terminalWS(t *testing.T, svc *app.Service, id string) domain.WebSocketBatch {
	return pollWS(t, svc, id, func(b domain.WebSocketBatch) bool {
		return b.Execution.State == "closed" || b.Execution.State == "failed"
	})
}

func TestWebSocketHandshakeTextBinaryPingAndGracefulClose(t *testing.T) {
	fixture := startWSAppFixture(t)
	svc, request, _ := newWSApp(t, fixture.url, security.NewMemoryStore())
	request.WebSocket.Subprotocols = []string{"unselected", "relay.v1"}
	request.WebSocket.Headers = []domain.Header{{Name: "X-Tag", Value: "one", Enabled: true}, {Name: "X-Tag", Value: "two", Enabled: true}, {Name: "Origin", Value: "https://desktop.example", Enabled: true}}
	run := connectWS(t, svc, request)
	if run.HandshakeStatusCode != 101 || run.Subprotocol != "relay.v1" || run.ConnectMS <= 0 {
		t.Fatalf("handshake diagnostics missing: %+v", run)
	}
	received := <-fixture.requests
	if !slices.Equal(received.Header.Values("X-Tag"), []string{"one", "two"}) || received.Header.Get("Origin") != "https://desktop.example" {
		t.Fatalf("handshake snapshot lost: %+v", received.Header)
	}
	text := "Привет, WebSocket 🌍"
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Encoding: "text", Data: text, FileID: "inactive-invalid-grant"}); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 0xff, 0x81, 0x7f}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "binary", Encoding: "hex", Data: "00 ff\t81\r\n7f", FileID: "inactive-invalid-grant"}); err != nil {
		t.Fatal(err)
	}
	batch := pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == 2 })
	if batch.Execution.SentCount != 2 || batch.Execution.SentBytes != int64(len(text)+len(binary)) {
		t.Fatalf("message counters wrong: %+v", batch.Execution)
	}
	foundText, foundBinary := false, false
	for _, event := range batch.Events {
		if event.Type == "received" && event.Text == text {
			foundText = true
		}
		if event.Type == "received" && event.DataBase64 == base64.StdEncoding.EncodeToString(binary) {
			foundBinary = true
		}
	}
	if !foundText || !foundBinary {
		t.Fatalf("wire previews lost: %+v", batch.Events)
	}
	if err := svc.PingWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.PingWebSocket(t.Context(), run.ID); err == nil {
		t.Fatal("ping flood was accepted")
	}
	if err := svc.DisconnectWebSocket(t.Context(), run.ID, 1000, "done"); err != nil {
		t.Fatal(err)
	}
	terminal := terminalWS(t, svc, run.ID)
	if terminal.Execution.State != "closed" || terminal.Execution.CloseCode != 1000 || !terminal.Execution.CleanClose || terminal.Execution.CloseReason != "done" {
		t.Fatalf("graceful close lost: %+v", terminal.Execution)
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "after close"}); err == nil {
		t.Fatal("send after close was accepted")
	}
	if err := svc.AckWebSocket(t.Context(), run.ID, terminal.NextSequence+1); err == nil {
		t.Fatal("undelivered ACK was accepted")
	}
	if err := svc.AckWebSocket(t.Context(), run.ID, terminal.NextSequence); err != nil {
		t.Fatal(err)
	}
	if err := svc.AckWebSocket(t.Context(), run.ID, terminal.NextSequence); err != nil {
		t.Fatal(err)
	}
	if err := svc.ForgetWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PollWebSocket(t.Context(), run.ID, 0); err == nil {
		t.Fatal("forgotten connection remained")
	}
}

func TestWebSocketSnapshotSecretsAndExplicitHistory(t *testing.T) {
	fixture := startWSAppFixture(t)
	vault := security.NewMemoryStore()
	svc, request, dir := newWSApp(t, fixture.url, vault)
	state := loadState(t, svc)
	env := state.Environments[0]
	env.Variables = append(env.Variables, domain.Variable{Name: "public", Value: "before"}, domain.Variable{Name: "token", Secret: true}, domain.Variable{Name: "later", Secret: true}, domain.Variable{Name: "encoded", Secret: true})
	state, err := svc.SaveEnvironment(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	env = state.Environments[0]
	refs := map[string]string{}
	for _, variable := range env.Variables {
		refs[variable.Name] = variable.ID
	}
	const first = "WS-secret-header-canary-73829"
	const later = "WS-later-secret-canary-93833"
	const encoded = "WS-hex-decoded-canary"
	for name, value := range map[string]string{"token": first, "later": later, "encoded": hex.EncodeToString([]byte(encoded))} {
		if _, err := svc.SetSecret(t.Context(), env.ID, refs[name], value); err != nil {
			t.Fatal(err)
		}
	}
	request.WebSocket.URL = fixture.url + "/socket?visible=value&token={{token}}"
	request.WebSocket.Headers = []domain.Header{{Name: "Authorization", Value: "Bearer {{token}}", Enabled: true}}
	run := connectWS(t, svc, request)
	if strings.Contains(run.URL, first) || !strings.Contains(run.URL, "REDACTED") {
		t.Fatalf("query secret leaked: %+v", run)
	}
	// A peer can echo a future secret before its variable is first used. A later
	// send must sanitize both pending UI events and the independent save ring.
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "ordinary payload " + later}); err != nil {
		t.Fatal(err)
	}
	pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == 1 })
	state = loadState(t, svc)
	env = state.Environments[0]
	for index := range env.Variables {
		if env.Variables[index].Name == "public" {
			env.Variables[index].Value = "after"
		}
	}
	if _, err := svc.SaveEnvironment(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "{{public}} {{token}} {{later}}"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "binary", Encoding: "hex", Data: "{{encoded}}"}); err != nil {
		t.Fatal(err)
	}
	batch := pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == 3 })
	data, _ := json.Marshal(batch)
	for _, secret := range []string{first, later, encoded, hex.EncodeToString([]byte(encoded)), base64.StdEncoding.EncodeToString([]byte(later))} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret leaked through pending event: %s", secret)
		}
	}
	foundBefore := false
	for _, event := range batch.Events {
		if strings.HasPrefix(event.Text, "before ") {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatal("a live send used a changed environment instead of the connection snapshot")
	}
	if _, err := svc.SaveWebSocketSession(t.Context(), run.ID, true); err == nil {
		t.Fatal("active session was saved")
	}
	if err := svc.DisconnectWebSocket(t.Context(), run.ID, 1000, "private close payload"); err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	metadata, err := svc.SaveWebSocketSession(t.Context(), run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.IncludePayloads || metadata.Execution.CloseReason != "" || len(metadata.Execution.HandshakeHeaders) != 0 {
		t.Fatalf("default saved payload metadata leaked: %+v", metadata)
	}
	for _, event := range metadata.Events {
		if event.Text != "" || event.DataBase64 != "" {
			t.Fatal("payload persisted without explicit opt-in")
		}
	}
	full, err := svc.SaveWebSocketSession(t.Context(), run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(full)
	if !bytes.Contains(data, []byte("ordinary payload")) || !bytes.Contains(data, []byte("REDACTED")) {
		t.Fatalf("opt-in sanitized previews missing: %s", data)
	}
	for _, secret := range []string{first, later, encoded} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("known secret persisted")
		}
	}
	list, err := svc.ListWebSocketSessions(t.Context(), request.WorkspaceID)
	if err != nil || len(list) != 2 {
		t.Fatalf("saved session list: %+v %v", list, err)
	}
	got, err := svc.GetSavedWebSocketSession(t.Context(), request.WorkspaceID, full.ID)
	if err != nil || got.ID != full.ID || !got.IncludePayloads {
		t.Fatalf("saved session restore: %+v %v", got, err)
	}
	// A normal snapshot save must preserve separately saved session rows.
	if _, err := svc.SaveUI(t.Context(), loadState(t, svc).UI); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSavedWebSocketSession(t.Context(), request.WorkspaceID, full.ID); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"ws.db", "ws.db-wal"} {
		file, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, secret := range []string{first, later, encoded} {
			if bytes.Contains(file, []byte(secret)) {
				t.Fatal("secret present in database/WAL")
			}
		}
	}
	if err := svc.DeleteSavedWebSocketSession(t.Context(), request.WorkspaceID, metadata.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSavedWebSocketSession(t.Context(), request.WorkspaceID, metadata.ID); err == nil {
		t.Fatal("deleted session still available")
	}
}

func TestWebSocketFullQueueKeepsControlReaderRunning(t *testing.T) {
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		}()
		for index := 0; index < 10000; index++ {
			if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf("%05d", index))); err != nil {
				done <- err
				return
			}
		}
		if err := conn.Ping(ctx); err != nil {
			done <- err
			return
		}
		done <- conn.Close(websocket.StatusNormalClosure, "flood complete")
		<-readerDone
	}))
	defer server.Close()
	svc, request, _ := newWSApp(t, "ws"+strings.TrimPrefix(server.URL, "http"), security.NewMemoryStore())
	started, err := svc.ConnectWebSocket(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// No UI poll or ACK while the peer writes 10,000 messages and awaits a pong.
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a full UI queue blocked pong/close processing")
	}
	batch := terminalWS(t, svc, started.ID)
	if batch.Execution.ReceivedCount != 10000 || batch.Execution.DroppedEvents < 9900 || !batch.Execution.CleanClose || len(batch.Events) > 32 {
		t.Fatalf("bounded queue accounting wrong: %+v events=%d", batch.Execution, len(batch.Events))
	}
	saved, err := svc.SaveWebSocketSession(t.Context(), started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Events) > 2000 || saved.OmittedEvents < 8000 || len(saved.Events) == 0 || saved.Events[len(saved.Events)-1].Type != "close" {
		t.Fatalf("history bound or terminal drain lost: events=%d omitted=%d", len(saved.Events), saved.OmittedEvents)
	}
	cursor := 0
	count := 0
	for {
		batch, err := svc.PollWebSocket(t.Context(), started.ID, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Events) == 0 {
			break
		}
		count += len(batch.Events)
		cursor = batch.NextSequence
		if err := svc.AckWebSocket(t.Context(), started.ID, cursor); err != nil {
			t.Fatal(err)
		}
	}
	if count > 64 || count == 0 {
		t.Fatalf("retained queue has %d events", count)
	}
}

func TestWebSocketNativeFileValidationLimitsAndCloseClasses(t *testing.T) {
	fixture := startWSAppFixture(t)
	svc, request, _ := newWSApp(t, fixture.url, security.NewMemoryStore())
	request.WebSocket.MaxMessageBytes = 128
	run := connectWS(t, svc, request)
	filename := filepath.Join(t.TempDir(), "message.bin")
	contents := []byte{0x00, 0xff, 0x01}
	if err := os.WriteFile(filename, contents, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := app.RegisterHTTPFile(svc, filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "binary", Encoding: "file", FileID: file.ID, Data: "inactive retained composer"}); err != nil {
		t.Fatal(err)
	}
	pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == 1 })
	for _, input := range []domain.WebSocketSend{{Kind: "text", Data: strings.Repeat("x", 129)}, {Kind: "text", Data: string([]byte{0xff})}, {Kind: "binary", Encoding: "hex", Data: "not hex"}, {Kind: "binary", Encoding: "base64", Data: "!!"}, {Kind: "binary", Encoding: "file", FileID: filename}} {
		if err := svc.SendWebSocketMessage(t.Context(), run.ID, input); err == nil {
			t.Fatalf("invalid send accepted: %+v", input)
		}
	}
	if err := os.WriteFile(filename, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "binary", Encoding: "file", FileID: file.ID}); err == nil {
		t.Fatal("changed native grant was reused")
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "peer-close"}); err != nil {
		t.Fatal(err)
	}
	closed := terminalWS(t, svc, run.ID).Execution
	if closed.State != "closed" || closed.CloseCode != 1008 || !closed.CleanClose {
		t.Fatalf("peer policy close misclassified: %+v", closed)
	}
	run = connectWS(t, svc, request)
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "abrupt"}); err != nil {
		t.Fatal(err)
	}
	closed = terminalWS(t, svc, run.ID).Execution
	if closed.State != "failed" || closed.CloseCode != 1006 || closed.CleanClose || closed.Error == "" {
		t.Fatalf("abrupt close misclassified: %+v", closed)
	}
}

func TestWebSocketConnectCancellationConcurrencyAndShutdown(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	defer close(release)
	svc, request, _ := newWSApp(t, "ws"+strings.TrimPrefix(server.URL, "http"), security.NewMemoryStore())
	ids := []string{}
	for index := 0; index < 4; index++ {
		callctx, cancel := context.WithCancel(t.Context())
		run, err := svc.ConnectWebSocket(callctx, request)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("Connect was tied to the bridge invocation context")
		}
	}
	if _, err := svc.ConnectWebSocket(t.Context(), request); err == nil {
		t.Fatal("fifth connection admitted")
	}
	if err := svc.ForgetWebSocket(t.Context(), ids[0]); err == nil {
		t.Fatal("connecting record forgotten")
	}
	for _, id := range ids {
		if err := svc.CancelWebSocket(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		result := terminalWS(t, svc, id).Execution
		if result.State != "closed" || result.CleanClose || result.CloseCode != 1006 {
			t.Fatalf("connect cancellation: %+v", result)
		}
	}
	if err := app.ShutdownWebSocket(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConnectWebSocket(t.Context(), request); err == nil {
		t.Fatal("connect after shutdown admitted")
	}
}

func TestWebSocketShutdownSendsGoingAwayToLivePeers(t *testing.T) {
	fixture := startWSAppFixture(t)
	svc, request, _ := newWSApp(t, fixture.url, security.NewMemoryStore())
	run := connectWS(t, svc, request)
	started := time.Now()
	if err := app.ShutdownWebSocket(svc); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("shutdown was not bounded")
	}
	select {
	case err := <-fixture.closed:
		if websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("peer did not receive going-away: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("peer did not observe shutdown")
	}
	if _, err := svc.PollWebSocket(t.Context(), run.ID, 0); err == nil {
		t.Fatal("shutdown retained session previews and secret snapshots")
	}
}

type blockingWSStore struct {
	*security.MemoryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (store *blockingWSStore) Get(ctx context.Context, key string) (string, error) {
	store.once.Do(func() { close(store.entered) })
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-store.release:
		return store.MemoryStore.Get(ctx, key)
	}
}

func TestWebSocketWriterAdmissionDoesNotQueueCommands(t *testing.T) {
	fixture := startWSAppFixture(t)
	vault := &blockingWSStore{MemoryStore: security.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	svc, request, _ := newWSApp(t, fixture.url, vault)
	state := loadState(t, svc)
	env := state.Environments[0]
	env.Variables = append(env.Variables, domain.Variable{Name: "blocked", Secret: true})
	state, err := svc.SaveEnvironment(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	env = state.Environments[0]
	variable := env.Variables[len(env.Variables)-1]
	if _, err := svc.SetSecret(t.Context(), env.ID, variable.ID, "late-keyring-canary"); err != nil {
		t.Fatal(err)
	}
	run := connectWS(t, svc, request)
	done := make(chan error, 1)
	go func() {
		done <- svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "{{blocked}}"})
	}()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("send did not reach keyring")
	}
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: "queued"}); err == nil {
		t.Fatal("second writer was queued")
	}
	if err := svc.PingWebSocket(t.Context(), run.ID); err == nil {
		t.Fatal("ping was queued behind a writer")
	}
	if err := svc.CancelWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled blocked send succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop a context-aware keyring read")
	}
	terminalWS(t, svc, run.ID)
}

func TestWebSocketFourMiBBinaryAndBoundedPreview(t *testing.T) {
	fixture := startWSAppFixture(t)
	svc, request, _ := newWSApp(t, fixture.url, security.NewMemoryStore())
	request.WebSocket.MaxMessageBytes = 4 << 20
	run := connectWS(t, svc, request)
	raw := bytes.Repeat([]byte{0xf1}, 4<<20)
	for index, encoding := range []string{"hex", "base64"} {
		data := hex.EncodeToString(raw)
		if encoding == "base64" {
			data = base64.StdEncoding.EncodeToString(raw)
		}
		if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "binary", Encoding: encoding, Data: data}); err != nil {
			t.Fatal(err)
		}
		batch := pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == index+1 })
		for _, event := range batch.Events {
			if event.Type == "received" && event.Bytes == len(raw) {
				if !event.Truncated || len(event.DataBase64) != base64.StdEncoding.EncodedLen(64<<10) {
					t.Fatalf("binary preview was not bounded: %+v", event)
				}
			}
		}
	}
	_ = svc.CancelWebSocket(t.Context(), run.ID)
	terminalWS(t, svc, run.ID)
}

func TestWebSocketReceiveLimitReadDeadlineAndHandshakeFailure(t *testing.T) {
	for _, mode := range []string{"oversize", "deadline", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "unauthorized" {
					w.Header().Set("X-Handshake", "denied")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte("private error page"))
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if mode == "oversize" {
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(strings.Repeat("x", 257)))
				}
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			svc, request, _ := newWSApp(t, "ws"+strings.TrimPrefix(server.URL, "http"), security.NewMemoryStore())
			request.WebSocket.MaxMessageBytes = 256
			if mode == "deadline" {
				request.WebSocket.ReadTimeoutMS = 40
			}
			run, err := svc.ConnectWebSocket(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			result := terminalWS(t, svc, run.ID).Execution
			if result.State != "failed" || result.CleanClose || result.Error == "" {
				t.Fatalf("failure misclassified: %+v", result)
			}
			if mode == "oversize" && result.CloseCode != 1009 {
				t.Fatalf("receive limit code missing: %+v", result)
			}
			if mode == "deadline" && !strings.Contains(strings.ToLower(result.Error), "deadline") {
				t.Fatalf("read deadline diagnostic missing: %+v", result)
			}
			if mode == "unauthorized" && (result.HandshakeStatusCode != 401 || len(result.HandshakeHeaders) == 0 || strings.Contains(result.Error, "private error page")) {
				t.Fatalf("failed handshake diagnostic leaked or disappeared: %+v", result)
			}
		})
	}
}

func TestWebSocketSavedEscapedHistoryFitsSerializedBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		go func() {
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		}()
		for index := 0; index < 60; index++ {
			if err := conn.Write(ctx, websocket.MessageText, []byte(strings.Repeat("<", 64<<10))); err != nil {
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "escaped history")
	}))
	defer server.Close()
	svc, request, _ := newWSApp(t, "ws"+strings.TrimPrefix(server.URL, "http"), security.NewMemoryStore())
	run, err := svc.ConnectWebSocket(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	saved, err := svc.SaveWebSocketSession(t.Context(), run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 8<<20 || saved.OmittedEvents == 0 || len(saved.Events) == 0 {
		t.Fatalf("serialized history cap: %d bytes, %d events, %d omitted", len(encoded), len(saved.Events), saved.OmittedEvents)
	}
}

func TestWebSocketCookieCredentialValueIsRedactedFromEchoAndSavedPayloads(t *testing.T) {
	fixture := startWSAppFixture(t)
	svc, request, dir := newWSApp(t, fixture.url, security.NewMemoryStore())
	const token = "WS-cookie-only-credential-canary-88331"
	request.WebSocket.Headers = []domain.Header{{Name: "Cookie", Value: "session=" + token + "; other=secondary-cookie-credential", Enabled: true}}
	run := connectWS(t, svc, request)
	if err := svc.SendWebSocketMessage(t.Context(), run.ID, domain.WebSocketSend{Kind: "text", Data: token}); err != nil {
		t.Fatal(err)
	}
	batch := pollWS(t, svc, run.ID, func(b domain.WebSocketBatch) bool { return b.Execution.ReceivedCount == 1 })
	encoded, _ := json.Marshal(batch)
	if bytes.Contains(encoded, []byte(token)) || !bytes.Contains(encoded, []byte("REDACTED")) {
		t.Fatalf("individual cookie value was not redacted from echo: %s", encoded)
	}
	if err := svc.DisconnectWebSocket(t.Context(), run.ID, 1000, "done"); err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	saved, err := svc.SaveWebSocketSession(t.Context(), run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(saved)
	if bytes.Contains(encoded, []byte(token)) {
		t.Fatal("cookie credential persisted in opt-in session payloads")
	}
	for _, filename := range []string{"ws.db", "ws.db-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(token)) {
			t.Fatal("cookie credential present in SQLite or WAL")
		}
	}
}
