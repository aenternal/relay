package app_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"
)

func newHTTPApp(t *testing.T) (*app.Service, domain.RequestDefinition) {
	t.Helper()
	_, svc := openApp(t, filepath.Join(t.TempDir(), "http.db"), security.NewMemoryStore())
	t.Cleanup(func() {
		if err := app.ShutdownHTTP(svc); err != nil {
			t.Error(err)
		}
	})
	return svc, loadState(t, svc).Requests[0]
}
func awaitHTTP(t *testing.T, svc *app.Service, id string) domain.HTTPExecution {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := svc.GetHTTPExecution(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "running" {
			return result
		}
		select {
		case <-deadline:
			t.Fatal("HTTP execution did not finish")
		case <-ticker.C:
		}
	}
}
func executeHTTP(t *testing.T, svc *app.Service, request domain.RequestDefinition) domain.HTTPExecution {
	t.Helper()
	result, err := svc.StartHTTPRequest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return awaitHTTP(t, svc, result.ID)
}
func enable(name, value string) domain.Header {
	return domain.Header{Name: name, Value: value, Enabled: true}
}

func TestHTTPExecutionJSON5VariablesAuthAndDetachedContext(t *testing.T) {
	svc, request := newHTTPApp(t)
	state, envID, variableID := addToken(t, svc)
	const secret = "secret-Distinctive-9076"
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, secret))
	collection := state.Collections[0]
	collection.Auth = &domain.HTTPAuth{Type: "bearer", Token: "{{token}}"}
	mustState(t)(svc.SaveCollection(t.Context(), collection))
	reached := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.RawQuery != "existing=1&x=two+words&x=three" || r.URL.EscapedPath() != "/records/a%2Fb%3F%25" || strings.Join(r.Header.Values("X-Repeat"), ",") != "one,two" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("wire request mismatch: %s %s %v", r.URL.EscapedPath(), r.URL.RawQuery, r.Header)
		}
		reached <- string(body)
		w.Header().Set("X-Echo", secret)
		w.Header().Set("Trailer", "X-Tail")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write(body)
		w.Header().Set("X-Tail", "finished")
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "POST", URL: server.URL + "/records/{id}?existing=1", Version: "1.1", PathParams: []domain.Header{enable("id", "a/b?%")}, Query: []domain.Header{enable("x", "two words"), enable("x", "three")}, Headers: []domain.Header{enable("X-Repeat", "one"), enable("X-Repeat", "two")}, BodyKind: "json5", Body: "{count:9223372036854775807, token:'{{token}}',}", Auth: domain.HTTPAuth{Type: "inherit"}}
	callCtx, cancel := context.WithCancel(t.Context())
	started, err := svc.StartHTTPRequest(callCtx, request)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	result := awaitHTTP(t, svc, started.ID)
	if result.State != "completed" || result.StatusCode != 201 || result.Protocol != "HTTP/1.1" || result.Binary || result.Timing.TotalMS <= 0 {
		t.Fatalf("unexpected response: %+v", result)
	}
	wire := <-reached
	if !strings.Contains(wire, "9223372036854775807") || !strings.Contains(wire, secret) {
		t.Fatal("JSON normalization lost int64 or secret substitution")
	}
	serialized, _ := json.Marshal(result)
	if bytes.Contains(serialized, []byte(secret)) || !strings.Contains(result.BodyText, "[REDACTED]") || strings.Contains(result.URL, "existing=1") {
		t.Fatalf("unsafe execution preview: %s", serialized)
	}
	if len(result.Trailers) != 1 || result.Trailers[0].Value != "finished" {
		t.Fatalf("trailers missing: %+v", result.Trailers)
	}
	target := filepath.Join(t.TempDir(), "response.json")
	if err = app.SaveHTTPResponse(svc, result.ID, target); err != nil {
		t.Fatal(err)
	}
	download, _ := os.ReadFile(target)
	if string(download) != wire {
		t.Fatal("download must preserve the actual response")
	}
	if err = svc.ForgetHTTPExecution(t.Context(), result.ID); err != nil {
		t.Fatal(err)
	}
	if err = app.SaveHTTPResponse(svc, result.ID, target); err == nil {
		t.Fatal("forgotten response remained downloadable")
	}
}

func TestHTTPUploadFilesAndMultipartStreamWithHTTP10(t *testing.T) {
	svc, request := newHTTPApp(t)
	payload := bytes.Repeat([]byte("streamed-upload-"), 400000) // exceeds the old 5 MiB buffered adapter limit
	source := filepath.Join(t.TempDir(), "upload.bin")
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := app.RegisterHTTPFile(svc, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"binary", "multipart"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Proto != "HTTP/1.0" {
					t.Errorf("expected HTTP/1.0, got %s", r.Proto)
				}
				var uploaded []byte
				if kind == "multipart" {
					reader, e := r.MultipartReader()
					if e != nil {
						t.Error(e)
						return
					}
					field, e := reader.NextPart()
					if e != nil {
						t.Error(e)
						return
					}
					value, _ := io.ReadAll(field)
					if field.FormName() != "title" || string(value) != "hello" {
						t.Error("multipart text field mismatch")
					}
					part, e := reader.NextPart()
					if e != nil {
						t.Error(e)
						return
					}
					if part.FileName() != "upload.bin" {
						t.Error("multipart filename mismatch")
					}
					uploaded, _ = io.ReadAll(part)
					if _, e = reader.NextPart(); e != io.EOF {
						t.Errorf("multipart terminator: %v", e)
					}
				} else {
					uploaded, _ = io.ReadAll(r.Body)
				}
				if !bytes.Equal(uploaded, payload) {
					t.Errorf("uploaded bytes mismatch: got %d, want %d", len(uploaded), len(payload))
				}
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()
			request.HTTP = &domain.HTTPDefinition{Method: "POST", URL: server.URL, Version: "1.0", BodyKind: kind, FileID: selected.ID, Multipart: []domain.HTTPPart{{Name: "title", Value: "hello", Enabled: true}, {Name: "data", FileID: selected.ID, Enabled: true}}}
			result := executeHTTP(t, svc, request)
			if result.State != "completed" || result.BodyText != "ok" {
				t.Fatalf("upload failed: %+v", result)
			}
		})
	}
	request.HTTP.FileID = "/etc/passwd"
	request.HTTP.BodyKind = "binary"
	if _, err = svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("arbitrary path accepted as file grant")
	}
	if err = os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	request.HTTP.FileID = selected.ID
	if _, err = svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("changed file grant accepted")
	}
}

func TestHTTPPreviewDownloadLimitAndDecompression(t *testing.T) {
	svc, request := newHTTPApp(t)
	state := loadState(t, svc)
	state.Settings.ResponsePreviewBytes = 1024
	mustState(t)(svc.SaveSettings(t.Context(), state.Settings))
	payload := bytes.Repeat([]byte("0123456789"), 2000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gzipWriter := gzip.NewWriter(w)
		_, _ = gzipWriter.Write(payload)
		_ = gzipWriter.Close()
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1", Decompress: true, MaxDownloadBytes: 2048}
	result := executeHTTP(t, svc, request)
	if result.State != "failed" || !result.Truncated || result.ReceivedBytes != 2049 || result.PreviewBytes != 1024 || len(result.BodyText) != 1024 || result.Error != "response download limit exceeded" || !result.DownloadAvailable {
		t.Fatalf("bounded response mismatch: %+v", result)
	}
	target := filepath.Join(t.TempDir(), "partial.txt")
	if err := app.SaveHTTPResponse(svc, result.ID, target); err != nil {
		t.Fatal(err)
	}
	download, _ := os.ReadFile(target)
	if !bytes.Equal(download, payload[:2048]) {
		t.Fatal("partial download differs from decompressed response")
	}
	request.HTTP.MaxDownloadBytes = 1 << 20
	result = executeHTTP(t, svc, request)
	if result.State != "completed" || !result.Truncated || result.ReceivedBytes != int64(len(payload)) {
		t.Fatalf("preview truncation must not limit download: %+v", result)
	}
	if err := app.SaveHTTPResponse(svc, result.ID, target); err != nil {
		t.Fatal(err)
	}
	download, _ = os.ReadFile(target)
	if !bytes.Equal(download, payload) {
		t.Fatal("full download was limited by preview")
	}
}

func TestHTTPCancelDeadlineAndShutdown(t *testing.T) {
	svc, request := newHTTPApp(t)
	canceled := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL + "/stream", Version: "1.1", TimeoutMS: 5000}
	started, err := svc.StartHTTPRequest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		result, e := svc.GetHTTPExecution(t.Context(), started.ID)
		if e != nil {
			t.Fatal(e)
		}
		if result.ReceivedBytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream progress did not arrive")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = svc.CancelHTTPExecution(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	result := awaitHTTP(t, svc, started.ID)
	if result.State != "canceled" || result.BodyText != "partial" || result.StatusCode != 200 {
		t.Fatalf("partial cancellation metadata missing: %+v", result)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close the connection")
	}
	request.HTTP.URL = server.URL + "/headers"
	request.HTTP.TimeoutMS = 30
	result = executeHTTP(t, svc, request)
	if result.State != "failed" || !strings.Contains(result.Error, "deadline") {
		t.Fatalf("deadline mismatch: %+v", result)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("deadline did not close the connection")
	}
	request.HTTP.URL = server.URL + "/stream"
	request.HTTP.TimeoutMS = 5000
	started, err = svc.StartHTTPRequest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.ShutdownHTTP(svc); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.GetHTTPExecution(t.Context(), started.ID); err == nil {
		t.Fatal("shutdown retained response")
	}
	if _, err = svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("request accepted after shutdown")
	}
}

func TestHTTPTLSProfilesForceVersionsAndMutualTLS(t *testing.T) {
	svc, request := newHTTPApp(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.Proto)) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := app.RegisterHTTPFile(svc, ca)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0", "1.1", "2", "auto"} {
		t.Run(version, func(t *testing.T) {
			request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: version, TLS: domain.HTTPTLS{CAFileID: selected.ID}}
			result := executeHTTP(t, svc, request)
			if result.State != "completed" || result.TLS == nil || !result.TLS.Verified {
				t.Fatalf("verified TLS request failed: %+v", result)
			}
			want := "HTTP/" + version
			if version == "2" || version == "auto" {
				want = "HTTP/2.0"
			}
			if result.Protocol != want || result.BodyText != want {
				t.Fatalf("forced wire version %s: %+v", version, result)
			}
		})
	}
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1"}
	result := executeHTTP(t, svc, request)
	if result.State != "failed" || !strings.Contains(result.Error, "TLS certificate") {
		t.Fatalf("untrusted TLS accepted: %+v", result)
	}
	request.HTTP.TLS = domain.HTTPTLS{Insecure: true}
	result = executeHTTP(t, svc, request)
	if result.State != "completed" || result.TLS.Verified || len(result.Warnings) == 0 {
		t.Fatalf("explicit insecure profile failed: %+v", result)
	}
	// Client authentication uses a generated CA with a clientAuth leaf. The server
	// certificate keeps its independently selected trust grant above.
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Relay test client CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay-client"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(clientKey)
	clientPath := filepath.Join(t.TempDir(), "client.pem")
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	_ = os.WriteFile(clientPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), 0600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600)
	clientGrant, err := app.RegisterHTTPFile(svc, clientPath)
	if err != nil {
		t.Fatal(err)
	}
	keyGrant, err := app.RegisterHTTPFile(svc, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	mutual := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, server.URL, http.StatusFound)
			return
		}
		if len(r.TLS.VerifiedChains) == 0 {
			t.Error("missing verified client certificate")
		}
		_, _ = w.Write([]byte("mutual"))
	}))
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	mutual.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: server.TLS.Certificates, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	mutual.StartTLS()
	defer mutual.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: mutual.URL, Version: "1.1", TLS: domain.HTTPTLS{CAFileID: selected.ID, ClientCertFileID: clientGrant.ID, ClientKeyFileID: keyGrant.ID}}
	result = executeHTTP(t, svc, request)
	if result.State != "completed" || result.BodyText != "mutual" {
		t.Fatalf("mTLS failed: %+v", result)
	}
	request.HTTP.URL = mutual.URL + "/redirect"
	request.HTTP.FollowRedirects = true
	result = executeHTTP(t, svc, request)
	if result.State != "failed" || !strings.Contains(result.Error, "protect the mTLS identity") || len(result.Redirects) != 1 {
		t.Fatalf("mTLS redirect diagnostic missing: %+v", result)
	}
}

func TestHTTPValidationAndCrossOriginRedirectCredentials(t *testing.T) {
	svc, request := newHTTPApp(t)
	var connections atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("cross-origin credentials leaked: %v", r.Header)
		}
		_, _ = w.Write([]byte("redirected"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/?token=peer-secret", http.StatusFound)
	}))
	defer source.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: source.URL, Version: "1.1", FollowRedirects: true, Headers: []domain.Header{enable("Authorization", "Bearer direct-secret"), enable("Cookie", "session=direct-secret")}, Auth: domain.HTTPAuth{Type: "api-key", In: "header", KeyName: "X-API-Key", KeyValue: "direct-api-secret"}}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || result.BodyText != "redirected" || len(result.Redirects) != 1 || strings.Contains(result.Redirects[0].URL, "peer-secret") {
		t.Fatalf("redirect metadata mismatch: %+v", result)
	}
	request.HTTP.Headers = []domain.Header{enable("X-Invalid", "line\r\nbreak")}
	if _, err := svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("header injection accepted")
	}
	if connections.Load() != 1 {
		t.Fatal("invalid request reached the network")
	}
	request.HTTP.Headers = nil
	request.HTTP.URL = "http://name:password@127.0.0.1/"
	if _, err := svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("URL userinfo accepted")
	}
}

func TestHTTPConcurrencyRecordEvictionAndSessionCookies(t *testing.T) {
	svc, request := newHTTPApp(t)
	var gate atomic.Bool
	gate.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gate.Load() {
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "cookie-secret", Path: "/"})
			_, _ = w.Write([]byte("set"))
			return
		}
		cookie, _ := r.Cookie("session")
		if cookie != nil {
			_, _ = w.Write([]byte(cookie.Value))
			return
		}
		_, _ = w.Write([]byte("no cookie"))
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1", TimeoutMS: 5000}
	var active []string
	for range 8 {
		started, err := svc.StartHTTPRequest(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, started.ID)
	}
	if _, err := svc.StartHTTPRequest(t.Context(), request); err == nil {
		t.Fatal("ninth active request accepted")
	}
	for _, id := range active {
		if err := svc.CancelHTTPExecution(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range active {
		if result := awaitHTTP(t, svc, id); result.State != "canceled" {
			t.Fatalf("cancellation failed: %+v", result)
		}
	}
	gate.Store(false)
	request.HTTP.URL = server.URL + "/set"
	if result := executeHTTP(t, svc, request); result.State != "completed" {
		t.Fatal(result.Error)
	}
	request.HTTP.URL = server.URL + "/get"
	result := executeHTTP(t, svc, request)
	if result.BodyText != "cookie-secret" {
		t.Fatalf("session cookie was not sent: %+v", result)
	}
	first := result.ID
	for range 17 {
		if result = executeHTTP(t, svc, request); result.State != "completed" {
			t.Fatal(result.Error)
		}
	}
	if _, err := svc.GetHTTPExecution(t.Context(), first); err == nil {
		t.Fatal("old response record was not evicted")
	}
}

func TestHTTPFolderAuthPrecedenceAndJSONEscapedSecret(t *testing.T) {
	svc, request := newHTTPApp(t)
	state, envID, variableID := addToken(t, svc)
	const secret = "quoted\"secret<&>"
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, secret))
	collection := state.Collections[0]
	collection.Auth = &domain.HTTPAuth{Type: "bearer", Token: "{{token}}"}
	mustState(t)(svc.SaveCollection(t.Context(), collection))
	state = mustState(t)(svc.SaveFolder(t.Context(), domain.Folder{WorkspaceID: request.WorkspaceID, CollectionID: request.CollectionID, Name: "parent", Auth: &domain.HTTPAuth{Type: "basic", Username: "user", Password: "{{token}}"}}))
	parent := state.Folders[0]
	state = mustState(t)(svc.SaveFolder(t.Context(), domain.Folder{WorkspaceID: request.WorkspaceID, CollectionID: request.CollectionID, ParentID: parent.ID, Name: "child", Auth: &domain.HTTPAuth{Type: "inherit"}}))
	request.FolderID = state.Folders[1].ID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != secret {
			t.Error("nearest folder authentication was not selected")
		}
		payload, _ := io.ReadAll(r.Body)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "POST", URL: server.URL + "/:name", Version: "1.1", Auth: domain.HTTPAuth{Type: "inherit"}, PathParams: []domain.Header{enable("name", "a/b")}, BodyKind: "json", Body: `{"secret":"{{token}}"}`}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || strings.Contains(result.BodyText, "quoted") || !strings.Contains(result.BodyText, "[REDACTED]") {
		t.Fatalf("JSON escaped secret leaked in response: %+v", result)
	}
}

func TestHTTPTextPreviewAtUTF8BoundaryAndSafeErrors(t *testing.T) {
	svc, request := newHTTPApp(t)
	state := loadState(t, svc)
	state.Settings.ResponsePreviewBytes = 1024
	mustState(t)(svc.SaveSettings(t.Context(), state.Settings))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"text":"` + strings.Repeat("Я", 1024) + `"}`))
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1"}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || result.Binary || result.BodyText == "" || result.BodyBase64 != "" || !result.Truncated {
		t.Fatalf("UTF8 boundary incorrectly classified: %+v", result)
	}
	request.HTTP.Version = "2"
	result = executeHTTP(t, svc, request)
	if result.State != "failed" || !strings.Contains(result.Error, "require HTTPS") {
		t.Fatalf("forced HTTP/2 diagnostic is not actionable: %+v", result)
	}
	request.HTTP.Version = "1.1"
	request.HTTP.Proxy = domain.HTTPProxy{Mode: "custom", URL: "ftp://user:private@proxy.invalid"}
	if _, err := svc.StartHTTPRequest(t.Context(), request); err == nil || strings.Contains(err.Error(), "private") || !strings.Contains(err.Error(), "proxy scheme") {
		t.Fatalf("proxy validation diagnostic is unsafe or not useful: %v", err)
	}
}

func TestHTTPBinaryPreviewKeepsFullDownloadAndSpoolPrivate(t *testing.T) {
	svc, request := newHTTPApp(t)
	payload := []byte{0, 1, 2, 3, 255, 254, 128, 127}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1"}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || !result.Binary || result.BodyText != "" || result.BodyBase64 != "AAECA//+gH8=" {
		t.Fatalf("binary preview mismatch: %+v", result)
	}
	filename := filepath.Join(t.TempDir(), "binary.bin")
	if err := app.SaveHTTPResponse(svc, result.ID, filename); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(saved, payload) {
		t.Fatalf("binary download mismatch: %v", err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("download permissions are too broad: %v", info.Mode())
	}
}

func TestHTTPCrossOriginRedirectStripsQueryAuthAndReferer(t *testing.T) {
	svc, request := newHTTPApp(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "public=one&public=two" || r.Referer() != "" {
			t.Errorf("cross-origin query credentials or Referer leaked: %s %s", r.URL.RawQuery, r.Referer())
		}
		_, _ = w.Write([]byte("safe"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "direct-query-secret" {
			t.Error("source authentication missing")
		}
		http.Redirect(w, r, target.URL+"/?public=one&api_key=direct-query-secret&public=two", http.StatusFound)
	}))
	defer source.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: source.URL, Version: "1.1", FollowRedirects: true, Auth: domain.HTTPAuth{Type: "api-key", In: "query", KeyName: "api_key", KeyValue: "direct-query-secret"}}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || result.BodyText != "safe" || strings.Contains(result.URL, "api_key") || len(result.Redirects) != 1 || strings.Contains(result.Redirects[0].URL, "api_key") {
		t.Fatalf("unsafe query redirect result: %+v", result)
	}
}

func TestHTTPCompressionLayerLimitDiagnostic(t *testing.T) {
	svc, request := newHTTPApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", strings.Repeat("gzip,", 8)+"gzip")
		_, _ = w.Write([]byte("unused"))
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "GET", URL: server.URL, Version: "1.1", Decompress: true}
	result := executeHTTP(t, svc, request)
	if result.State != "failed" || !strings.Contains(result.Error, "limit of 8 compression layers") {
		t.Fatalf("compression layer diagnostic missing: %+v", result)
	}
}

func TestHTTP10EmptyBinaryFileHasKnownZeroLength(t *testing.T) {
	svc, request := newHTTPApp(t)
	path := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	grant, err := app.RegisterHTTPFile(svc, path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Proto != "HTTP/1.0" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			t.Errorf("empty upload framing: %s length=%d encoding=%v", r.Proto, r.ContentLength, r.TransferEncoding)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			t.Errorf("empty upload body: len=%d err=%v", len(body), err)
		}
		_, _ = w.Write([]byte("empty accepted"))
	}))
	defer server.Close()
	request.HTTP = &domain.HTTPDefinition{Method: "POST", URL: server.URL, Version: "1.0", BodyKind: "file", FileID: grant.ID}
	result := executeHTTP(t, svc, request)
	if result.State != "completed" || result.BodyText != "empty accepted" {
		t.Fatalf("empty file upload failed: %+v", result)
	}
}
