package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/security"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func selectGRPCPEM(t *testing.T, svc *app.Service, name string, data []byte) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := app.RegisterHTTPFile(svc, filename)
	if err != nil {
		t.Fatal(err)
	}
	return selected.ID
}

func TestGRPCTLSVerificationCustomCAMTLSAndExplicitInsecure(t *testing.T) {
	// Reuse Go's local-only test server certificate, which covers 127.0.0.1.
	certSource := httptest.NewTLSServer(http.NotFoundHandler())
	serverCertificate := certSource.TLS.Certificates[0]
	serverDER := certSource.Certificate().Raw
	certSource.Close()
	fixture := startGRPCAppFixture(t, false, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate}})))
	svc, request, _ := newGRPCApp(t, fixture.address)
	request.GRPC.Plaintext = false
	request.GRPC.ConnectTimeoutMS = 150
	request.GRPC.Body = `{"text":"secure"}`
	if result := executeGRPC(t, svc, request); result.State != "failed" || result.CodeName != "Unavailable" || !strings.Contains(result.Error, "TLS certificate") {
		t.Fatalf("untrusted certificate accepted or diagnostic lost: %+v", result)
	}
	caID := selectGRPCPEM(t, svc, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}))
	request.GRPC.TLS.CAFileID = caID
	if result := executeGRPC(t, svc, request); result.State != "completed" || result.TLS == nil || !result.TLS.Verified || result.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("custom CA or actual TLS state missing: %+v", result)
	}
	request.GRPC.TLS.ServerName = "wrong.example"
	if result := executeGRPC(t, svc, request); result.State != "failed" || !strings.Contains(result.Error, "server name") {
		t.Fatalf("wrong SNI/certificate name accepted: %+v", result)
	}
	request.GRPC.TLS = domain.HTTPTLS{Insecure: true}
	if result := executeGRPC(t, svc, request); result.State != "completed" || result.TLS == nil || result.TLS.Verified || len(result.Warnings) == 0 {
		t.Fatalf("explicit insecure state missing: %+v", result)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(12), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, clientTemplate, &clientKey.PublicKey, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCert, err := x509.ParseCertificate(clientDER)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs.AddCert(clientCert)
	mutual := startGRPCAppFixture(t, false, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate}, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert})))
	request.GRPC.Endpoint = mutual.address
	request.GRPC.TLS = domain.HTTPTLS{CAFileID: caID}
	if result := executeGRPC(t, svc, request); result.State != "failed" {
		t.Fatal("mTLS endpoint accepted a missing client certificate")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	request.GRPC.TLS.ClientCertFileID = selectGRPCPEM(t, svc, "client.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}))
	request.GRPC.TLS.ClientKeyFileID = selectGRPCPEM(t, svc, "key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if result := executeGRPC(t, svc, request); result.State != "completed" || result.TLS == nil || !result.TLS.Verified {
		t.Fatalf("native file grants did not provide mTLS identity: %+v", result)
	}
}

func TestGRPCReflectionCacheSeparatesResolvedSNIProfiles(t *testing.T) {
	certSource := httptest.NewTLSServer(http.NotFoundHandler())
	serverCertificate := certSource.TLS.Certificates[0]
	certSource.Close()
	fixture := startGRPCAppFixture(t, true, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate}})))
	svc, request, _ := newGRPCApp(t, fixture.address)
	request.GRPC.Plaintext = false
	request.GRPC.TLS = domain.HTTPTLS{Insecure: true, ServerName: "{{sni}}"}
	setSNI := func(value string) {
		t.Helper()
		state := loadState(t, svc)
		env := state.Environments[0]
		found := false
		for i := range env.Variables {
			if env.Variables[i].Name == "sni" {
				env.Variables[i].Value = value
				found = true
			}
		}
		if !found {
			env.Variables = append(env.Variables, domain.Variable{Name: "sni", Value: value})
		}
		mustState(t)(svc.SaveEnvironment(t.Context(), env))
	}
	discover := func() string {
		t.Helper()
		started, err := svc.StartGRPCReflection(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		result := awaitGRPC(t, svc, started.ID)
		if result.State != "completed" || result.SchemaID == "" {
			t.Fatalf("reflection failed: %+v", result)
		}
		return result.SchemaID
	}
	setSNI("one.test")
	first := discover()
	setSNI("two.test")
	second := discover()
	if first == second {
		t.Fatal("different resolved SNI profiles replaced the same cached schema")
	}
	if repeated := discover(); repeated != second {
		t.Fatal("unchanged resolved SNI profile did not reuse its cached schema")
	}
	setSNI("one.test")
	if repeated := discover(); repeated != first {
		t.Fatal("previous resolved SNI profile lost its cached schema")
	}
	if len(loadState(t, svc).ProtoSchemas) != 3 {
		t.Fatal("reflection cache created duplicate schemas for unchanged profiles")
	}
}

type blockingGRPCStore struct {
	*security.MemoryStore
	entered chan struct{}
}

func (s *blockingGRPCStore) Get(ctx context.Context, _ string) (string, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func TestGRPCStartAndCancelDoNotWaitForKeyring(t *testing.T) {
	vault := &blockingGRPCStore{MemoryStore: security.NewMemoryStore(), entered: make(chan struct{}, 1)}
	_, svc := openApp(t, filepath.Join(t.TempDir(), "blocked.db"), vault)
	t.Cleanup(func() { _ = app.ShutdownGRPC(svc) })
	_, envID, variableID := addToken(t, svc)
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, "only-in-keyring"))
	state := loadState(t, svc)
	request := state.Requests[0]
	request.Protocol, request.HTTP = "grpc", nil
	request.GRPC = &domain.GRPCDefinition{Endpoint: "{{token}}", Plaintext: true}
	startedAt := time.Now()
	started, err := svc.StartGRPCUnary(t.Context(), request)
	if err != nil || time.Since(startedAt) > time.Second {
		t.Fatalf("start waited for keyring: %v", err)
	}
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not resolve its secret")
	}
	if _, err := svc.LoadState(t.Context()); err != nil {
		t.Fatal("keyring held the state mutex")
	}
	if err := svc.CancelGRPCExecution(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	if result := awaitGRPC(t, svc, started.ID); result.State != "canceled" {
		t.Fatalf("cancel did not release keyring: %+v", result)
	}
}
