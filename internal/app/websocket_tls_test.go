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
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"relay/internal/domain"
	"relay/internal/security"
)

func TestWebSocketCustomCAMTLSAndInactiveTLSGrants(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	svc, request, _ := newWSApp(t, "wss"+strings.TrimPrefix(server.URL, "https"), security.NewMemoryStore())
	request.WebSocket.ConnectTimeoutMS = 250
	run, err := svc.ConnectWebSocket(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	failed := terminalWS(t, svc, run.ID).Execution
	if failed.State != "failed" || !strings.Contains(failed.Error, "TLS certificate") {
		t.Fatalf("self-signed TLS accepted: %+v", failed)
	}
	caID := selectGRPCPEM(t, svc, "ws-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	request.WebSocket.TLS.CAFileID = caID
	run = connectWS(t, svc, request)
	if run.TLS == nil || !run.TLS.Verified || run.TLS.Version == "" {
		t.Fatalf("custom CA diagnostics missing: %+v", run)
	}
	if err := svc.CancelWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	request.WebSocket.TLS = domain.HTTPTLS{Insecure: true}
	run = connectWS(t, svc, request)
	if run.TLS == nil || run.TLS.Verified || len(run.Warnings) == 0 {
		t.Fatalf("explicit insecure warning missing: %+v", run)
	}
	if err := svc.CancelWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(48), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	clientCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(clientCertificate)
	mutual := httptest.NewUnstartedServer(handler)
	mutual.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: server.TLS.Certificates, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert}
	mutual.StartTLS()
	defer mutual.Close()
	request.WebSocket.URL = "wss" + strings.TrimPrefix(mutual.URL, "https")
	request.WebSocket.TLS = domain.HTTPTLS{CAFileID: caID}
	run, err = svc.ConnectWebSocket(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if failed = terminalWS(t, svc, run.ID).Execution; failed.State != "failed" {
		t.Fatal("mTLS server accepted missing client identity")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	request.WebSocket.TLS.ClientCertFileID = selectGRPCPEM(t, svc, "ws-client.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	request.WebSocket.TLS.ClientKeyFileID = selectGRPCPEM(t, svc, "ws-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	run = connectWS(t, svc, request)
	if run.TLS == nil || !run.TLS.Verified {
		t.Fatal("native file grants failed to provide mTLS identity")
	}
	if err := svc.CancelWebSocket(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	terminalWS(t, svc, run.ID)
	// A retained TLS profile is inactive on ws:// and must never open old grants.
	plain := httptest.NewServer(handler)
	defer plain.Close()
	request.WebSocket.URL = "ws" + strings.TrimPrefix(plain.URL, "http")
	request.WebSocket.TLS = domain.HTTPTLS{CAFileID: "stale-ca", ClientCertFileID: "stale-cert", ServerName: "{{undefined}}"}
	run = connectWS(t, svc, request)
	if run.TLS != nil {
		t.Fatal("plaintext connection reported TLS")
	}
	_ = svc.CancelWebSocket(context.Background(), run.ID)
	terminalWS(t, svc, run.ID)
}
