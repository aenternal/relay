package app_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"
)

// This exercises owned sockets and response files rather than counting global
// goroutines, whose lifetime depends on the Go resolver and HTTP test servers.
func TestShutdownClosesMixedProtocolsAndDisposesTransientResources(t *testing.T) {
	spools := t.TempDir()
	t.Setenv("TMPDIR", spools)
	grpcFixture := startGRPCStreamFixture(t)
	svc, grpcRequest, _ := newGRPCStreamApp(t, grpcFixture, "Watch")
	t.Cleanup(func() { _ = app.Shutdown(svc) })
	base := loadState(t, svc).Requests[0]

	httpEntered := make(chan struct{})
	httpClosed := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=shutdown-cookie-canary; HttpOnly")
		_, _ = w.Write(bytes.Repeat([]byte("response"), 1024))
		if r.URL.Path == "/slow" {
			w.(http.Flusher).Flush()
			close(httpEntered)
			<-r.Context().Done()
			close(httpClosed)
		}
	}))
	t.Cleanup(httpServer.Close)
	httpRequest := base
	httpRequest.HTTP = &domain.HTTPDefinition{Method: "GET", Version: "1.1", URL: httpServer.URL}
	completed := executeHTTP(t, svc, httpRequest)
	if !completed.DownloadAvailable {
		t.Fatal("completed response was not spooled")
	}
	httpRequest.HTTP.URL = httpServer.URL + "/slow"
	httpRun, err := svc.StartHTTPRequest(t.Context(), httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	awaitLifecycleSignal(t, httpEntered, "HTTP request did not reach server")
	files, err := filepath.Glob(filepath.Join(spools, "relay-response-*"))
	if err != nil || len(files) != 2 {
		t.Fatalf("expected completed and live response spools: %v, %v", files, err)
	}
	upload := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(upload, []byte("upload grant"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RegisterHTTPFile(svc, upload); err != nil {
		t.Fatal(err)
	}

	// Leave the gRPC bridge queue full and unacknowledged. Shutdown must release
	// the producer even though no renderer will poll or ACK again.
	grpcRequest.GRPC.Body = `{"text":"large","count":100000}`
	grpcRun := startGRPCStream(t, svc, grpcRequest)
	waitGRPCStream(t, svc, grpcRun.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount >= 12 })

	// This peer deliberately stops reading after upgrade. Its close handshake
	// consumes the bounded grace period; the other protocols must cancel before
	// that grace ends, not wait behind it in the desktop shutdown callback.
	wsEntered := make(chan struct{})
	wsRelease := make(chan struct{})
	wsClosed := make(chan struct{})
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer close(wsClosed)
		defer conn.CloseNow()
		close(wsEntered)
		<-wsRelease
		_, _, _ = conn.Read(context.Background())
	}))
	t.Cleanup(wsServer.Close)
	t.Cleanup(func() { close(wsRelease) })
	wsRequest := base
	wsRequest.Protocol, wsRequest.HTTP = "websocket", nil
	wsRequest.WebSocket = &domain.WebSocketDefinition{URL: "ws" + strings.TrimPrefix(wsServer.URL, "http")}
	wsRun := connectWS(t, svc, wsRequest)
	awaitLifecycleSignal(t, wsEntered, "WebSocket was not upgraded")

	started := time.Now()
	finished := make(chan error, 2)
	for range 2 {
		go func() { finished <- app.Shutdown(svc) }()
	}
	select {
	case <-httpClosed:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("HTTP cancellation waited behind the WebSocket close grace period")
	}
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("mixed-protocol shutdown did not finish")
		}
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("shutdown exceeded its socket grace budget: %v", elapsed)
	}
	deadline := time.Now().Add(time.Second)
	for grpcFixture.canceled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if grpcFixture.canceled.Load() == 0 {
		t.Fatal("gRPC server did not observe stream cancellation")
	}
	for _, filename := range files {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			t.Fatalf("private response spool survived shutdown: %s (%v)", filename, err)
		}
	}
	if _, err := svc.GetHTTPExecution(t.Context(), httpRun.ID); err == nil {
		t.Fatal("HTTP preview survived shutdown")
	}
	if _, err := svc.GetGRPCExecution(t.Context(), grpcRun.ID); err == nil {
		t.Fatal("gRPC preview survived shutdown")
	}
	if _, err := svc.PollWebSocket(t.Context(), wsRun.ID, 0); err == nil {
		t.Fatal("WebSocket preview survived shutdown")
	}
	if _, err := svc.StartHTTPRequest(t.Context(), httpRequest); err == nil {
		t.Fatal("HTTP admitted work after shutdown")
	}
	if _, err := svc.StartGRPCStream(t.Context(), grpcRequest); err == nil {
		t.Fatal("gRPC admitted work after shutdown")
	}
	if _, err := svc.ConnectWebSocket(t.Context(), wsRequest); err == nil {
		t.Fatal("WebSocket admitted work after shutdown")
	}
	if _, err := app.RegisterHTTPFile(svc, upload); err == nil {
		t.Fatal("native file grant admitted after shutdown")
	}
	if err := app.SaveHTTPResponse(svc, completed.ID, filepath.Join(t.TempDir(), "late.bin")); err == nil {
		t.Fatal("shutdown response remained downloadable")
	}
	if err := app.Shutdown(svc); err != nil {
		t.Fatal("repeated shutdown failed", err)
	}
}

func awaitLifecycleSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestHTTPShutdownRejectsBeforeSecretResolution(t *testing.T) {
	vault := &grpcStreamGateStore{Store: security.NewMemoryStore(), entered: make(chan struct{}, 1)}
	_, svc := openApp(t, filepath.Join(t.TempDir(), "closed.db"), vault)
	_, environmentID, variableID := addToken(t, svc)
	mustState(t)(svc.SetSecret(t.Context(), environmentID, variableID, "closed-keyring-canary"))
	request := loadState(t, svc).Requests[0]
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: "http://127.0.0.1:1", Auth: domain.HTTPAuth{Type: "bearer", Token: "{{token}}"}}
	if err := app.Shutdown(svc); err != nil {
		t.Fatal(err)
	}
	vault.gate.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := svc.StartHTTPRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("closed application prepared a new HTTP request: %v", err)
	}
	select {
	case <-vault.entered:
		t.Fatal("closed application prompted the secret provider")
	default:
	}
}
