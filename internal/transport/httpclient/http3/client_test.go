package http3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	quichttp3 "github.com/quic-go/quic-go/http3"
)

func quicServer(t *testing.T, handler http.Handler) (string, *tls.Config) {
	t.Helper()
	certServer := httptest.NewTLSServer(handler)
	cert := certServer.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(certServer.Certificate())
	certServer.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &quichttp3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(udp) }()
	t.Cleanup(func() { _ = server.Close(); _ = udp.Close(); <-done })
	return "https://" + udp.LocalAddr().String(), &tls.Config{RootCAs: pool}
}

func TestActualHTTP3AndSecureDefaults(t *testing.T) {
	var observed atomic.Int32
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.Store(int32(r.ProtoMajor))
		body, _ := io.ReadAll(r.Body)
		if r.Method != "GET" || string(body) != "payload" {
			t.Errorf("request: %s %q", r.Method, body)
		}
		w.Header().Add("Trailer", "X-Finished")
		_, _ = w.Write([]byte("ok"))
		w.Header().Set("X-Finished", "yes")
	}))
	client, _ := New(Config{Timeout: 2 * time.Second})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, strings.NewReader("payload"))
	if _, err := client.Do(req); err == nil {
		t.Fatal("self-signed TLS accepted by default")
	}
	trusted, _ := New(Config{TLSConfig: profile})
	defer trusted.Close()
	req, _ = http.NewRequest("GET", url, strings.NewReader("payload"))
	resp, err := trusted.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 3 || observed.Load() != 3 || resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("not HTTP/3 TLS1.3: %#v", resp)
	}
	if resp.Trailer.Get("X-Finished") != "yes" {
		t.Fatalf("trailers missing: %v", resp.Trailer)
	}
	if profile.MinVersion != 0 || len(profile.NextProtos) != 0 {
		t.Fatal("caller TLS profile was mutated")
	}
	insecureClient, _ := New(Config{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
	defer insecureClient.Close()
	req, _ = http.NewRequest("GET", url, strings.NewReader("payload"))
	resp, err = insecureClient.Do(req)
	if err != nil {
		t.Fatalf("explicit insecure profile: %v", err)
	}
	resp.Body.Close()
}

func TestCancelBlockedRead(t *testing.T) {
	started := make(chan struct{})
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	client, _ := New(Config{TLSConfig: profile})
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	done := make(chan error, 1)
	go func() { _, err := client.Do(req); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock HTTP/3 read")
	}
}

func TestNoTCPFallback(t *testing.T) {
	for _, test := range []struct {
		name             string
		clientTimeout    time.Duration
		requestTimeout   time.Duration
		handshakeTimeout time.Duration
	}{
		{name: "request deadline", clientTimeout: 5 * time.Second, requestTimeout: 100 * time.Millisecond},
		{name: "QUIC deadline", clientTimeout: 5 * time.Second, requestTimeout: 5 * time.Second, handshakeTimeout: 100 * time.Millisecond},
		{name: "equal deadlines", clientTimeout: 100 * time.Millisecond, requestTimeout: 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			var tcpConnections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			// Count connections even when a forbidden TCP fallback would fail
			// certificate verification before reaching the HTTP handler.
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					tcpConnections.Add(1)
				}
			}
			server.StartTLS()
			t.Cleanup(server.Close)
			// Keep UDP silent on the same port, without relying on how the OS
			// reports packets sent to a closed UDP port.
			udp, err := net.ListenPacket("udp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = udp.Close() })
			client, err := New(Config{Timeout: test.clientTimeout})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			if test.handshakeTimeout != 0 {
				client.transport.QUICConfig.HandshakeIdleTimeout = test.handshakeTimeout
			}
			ctx, cancel := context.WithTimeout(t.Context(), test.requestTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Do(req)
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || !strings.Contains(err.Error(), "QUIC") {
				t.Fatalf("missing typed QUIC request timeout: %v", err)
			}
			if test.requestTimeout < test.clientTimeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request deadline cause was lost: %v", err)
			}
			if test.handshakeTimeout != 0 {
				var idle *quic.IdleTimeoutError
				var handshake *quic.HandshakeTimeoutError
				if !errors.As(err, &idle) && !errors.As(err, &handshake) {
					t.Fatalf("QUIC deadline cause was lost: %v", err)
				}
			}
			if tcpConnections.Load() != 0 {
				t.Fatal("fell back to TCP")
			}
		})
	}
}

func TestResponseLimit(t *testing.T) {
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("too large")) }))
	client, _ := New(Config{TLSConfig: profile, MaxResponseBodyBytes: 4})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, nil)
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error: %v", err)
	}
}

func TestRejectUnsupportedInputs(t *testing.T) {
	client, _ := New(Config{})
	defer client.Close()
	for _, tc := range []struct{ method, url, want string }{
		{"GET", "http://127.0.0.1", "https"},
		{"CONNECT", "https://127.0.0.1", "CONNECT"},
	} {
		req, _ := http.NewRequest(tc.method, tc.url, nil)
		if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error: %v", err)
		}
	}
}

func TestTimeoutBlockedResponse(t *testing.T) {
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	client, _ := New(Config{TLSConfig: profile, Timeout: 100 * time.Millisecond})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, nil)
	if _, err := client.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}

func TestResponseHeaderLimit(t *testing.T) {
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("a", 1024))
		w.WriteHeader(http.StatusOK)
	}))
	client, _ := New(Config{TLSConfig: profile, MaxResponseHeaderBytes: 128})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("oversized headers accepted")
	}
}

func TestCloseUnblocksRequestAndRejectsReuse(t *testing.T) {
	started := make(chan struct{})
	url, profile := quicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	client, _ := New(Config{TLSConfig: profile})
	req, _ := http.NewRequest("GET", url, nil)
	done := make(chan error, 1)
	go func() { _, err := client.Do(req); done <- err }()
	<-started
	_ = client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock request")
	}
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("client reused after Close: %v", err)
	}
}
