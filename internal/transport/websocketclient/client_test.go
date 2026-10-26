package websocketclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wsURL(server *httptest.Server) string { return "ws" + strings.TrimPrefix(server.URL, "http") }

func echoHandler(t *testing.T, before func(*http.Request), protocols ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if before != nil {
			before(r)
		}
		w.Header().Add("X-Handshake", "first")
		w.Header().Add("X-Handshake", "second")
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: protocols})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(MaxMessageBytes)
		for {
			kind, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, body); err != nil {
				return
			}
		}
	}
}

func dialTest(t *testing.T, ctx context.Context, url string, opts Options) (*Client, *Handshake) {
	t.Helper()
	c, info, err := Dial(ctx, url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, info
}

func roundTrip(t *testing.T, ctx context.Context, c *Client, kind string, data []byte) {
	t.Helper()
	if err := c.Write(ctx, kind, data); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != kind || !bytes.Equal(got.Data, data) {
		t.Fatalf("message mismatch: kind=%s length=%d", got.Type, len(got.Data))
	}
}

func TestEchoHeadersSubprotocolAndLogicalMessages(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(echoHandler(t, func(r *http.Request) {
		if r.Proto != "HTTP/1.1" || !reflect.DeepEqual(r.Header.Values("X-Repeat"), []string{"one", "two"}) || r.Header.Get("Origin") != "https://editor.invalid" || r.Header.Get("Cookie") != "session=canary" {
			t.Errorf("unexpected upgrade headers or protocol")
		}
	}, "relay.binary"))
	defer server.Close()
	c, info := dialTest(t, ctx, wsURL(server), Options{Header: http.Header{"X-Repeat": {"one", "two"}, "Origin": {"https://editor.invalid"}, "Cookie": {"session=canary"}}, Subprotocols: []string{"relay.json", "relay.binary"}})
	if info.StatusCode != 101 || info.Subprotocol != "relay.binary" || !reflect.DeepEqual(info.Headers.Values("X-Handshake"), []string{"first", "second"}) || info.Duration <= 0 || info.ConnectDuration <= 0 {
		t.Fatalf("incomplete handshake: %+v", info)
	}
	roundTrip(t, ctx, c, "text", []byte("Привет, мир 🌍"))
	roundTrip(t, ctx, c, "binary", []byte{0, 255, 1, 0, 128})
	roundTrip(t, ctx, c, "binary", bytes.Repeat([]byte{0, 255}, 200_000))
}

func TestControlFramesAutopongAndCorrelatedPing(t *testing.T) {
	ctx := testContext(t)
	serverPong := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		go func() { serverPong <- conn.Ping(r.Context()) }()
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	var pings, pongs atomic.Int32
	c, _ := dialTest(t, ctx, wsURL(server), Options{OnPing: func(data []byte) {
		if len(data) == 0 {
			t.Error("missing ping payload")
		}
		pings.Add(1)
	}, OnPong: func([]byte) { pongs.Add(1) }})
	readerDone := make(chan error, 1)
	go func() { _, err := c.Read(ctx); readerDone <- err }()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverPong:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if pings.Load() != 1 || pongs.Load() != 1 {
		t.Fatalf("controls: ping=%d pong=%d", pings.Load(), pongs.Load())
	}
	if err := c.Close(1000, "done"); err != nil {
		t.Fatal(err)
	}
	<-readerDone
}

func TestLogicalFragmentedTextAllowsUTF8AcrossFragments(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		writer, err := conn.Writer(r.Context(), websocket.MessageText)
		if err != nil {
			return
		}
		// Each Writer.Write emits a non-final frame. A multibyte code point can
		// legitimately cross that boundary; validation applies to the message.
		for _, piece := range [][]byte{[]byte("fragmented "), {0xd0}, {0x9f}, []byte("ривет")} {
			if _, err := writer.Write(piece); err != nil {
				return
			}
		}
		if err := writer.Close(); err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	c, _ := dialTest(t, ctx, wsURL(server), Options{})
	message, err := c.Read(ctx)
	if err != nil || message.Type != "text" || string(message.Data) != "fragmented Привет" {
		t.Fatalf("fragmented logical message: %+v %v", message, err)
	}
}

func TestClosePeerNormalAbnormalAndBoundedHandshake(t *testing.T) {
	for _, scenario := range []string{"normal", "abnormal", "unresponsive"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := testContext(t)
			release := make(chan struct{})
			defer close(release)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				switch scenario {
				case "normal":
					_ = conn.Close(websocket.StatusNormalClosure, "finished")
				case "abnormal":
					_ = conn.CloseNow()
				case "unresponsive":
					<-release
				}
			}))
			defer server.Close()
			c, _ := dialTest(t, ctx, wsURL(server), Options{})
			if scenario == "unresponsive" {
				started := time.Now()
				_ = c.Close(1000, "done")
				if elapsed := time.Since(started); elapsed > 1800*time.Millisecond {
					t.Fatalf("close blocked for %s", elapsed)
				}
				return
			}
			_, err := c.Read(ctx)
			if err == nil {
				t.Fatal("expected closure")
			}
			code, reason, ok := CloseInfo(err)
			if scenario == "normal" && (!ok || code != 1000 || reason != "finished") {
				t.Fatalf("normal close %d %q %t: %v", code, reason, ok, err)
			}
			if scenario == "abnormal" && ok {
				t.Fatalf("abrupt TCP close reported a close frame: %v", err)
			}
		})
	}
}

func TestRedirectAndFailedHandshakeDiagnostics(t *testing.T) {
	ctx := testContext(t)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, status := range []int{http.StatusFound, http.StatusUnauthorized} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.Header().Set("X-Diagnostic", "available")
			w.WriteHeader(status)
		}))
		c, info, err := Dial(ctx, wsURL(server), Options{Header: http.Header{"Authorization": {"Bearer private"}}})
		server.Close()
		if err == nil || c != nil || info.StatusCode != status || info.Headers.Get("X-Diagnostic") != "available" || info.Duration <= 0 {
			t.Fatalf("failure diagnostics status=%d: %+v %v", status, info, err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestMessageValidationLimitsAndConnectionSurvivesRejectedSend(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(echoHandler(t, nil))
	defer server.Close()
	c, _ := dialTest(t, ctx, wsURL(server), Options{MaxSendBytes: 16, MaxReceiveBytes: 16})
	for _, input := range []struct {
		kind string
		data []byte
	}{{"text", []byte{255}}, {"binary", bytes.Repeat([]byte{1}, 17)}, {"ping", nil}} {
		if err := c.Write(ctx, input.kind, input.data); err == nil {
			t.Fatal("accepted invalid outgoing message")
		}
	}
	roundTrip(t, ctx, c, "text", []byte("still open"))
	roundTrip(t, ctx, c, "binary", bytes.Repeat([]byte{255}, 16))
	for _, kind := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_ = conn.Write(r.Context(), kind, bytes.Repeat([]byte{'a'}, 17))
			_, _, _ = conn.Read(r.Context())
		}))
		peer, _ := dialTest(t, ctx, wsURL(oversized), Options{MaxReceiveBytes: 16})
		_, err := peer.Read(ctx)
		code, _, ok := CloseInfo(err)
		_ = peer.CloseNow()
		oversized.Close()
		if !ok || code != 1009 {
			t.Fatalf("oversized message: %v, code=%d", err, code)
		}
	}
}

func TestInvalidIncomingTextAndCloseReason(t *testing.T) {
	for _, reason := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "close"}[reason], func(t *testing.T) {
			ctx := testContext(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if reason {
					_ = conn.Close(1000, string([]byte{255}))
					return
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte{255})
				_, _, _ = conn.Read(r.Context())
			}))
			defer server.Close()
			c, _ := dialTest(t, ctx, wsURL(server), Options{})
			message, err := c.Read(ctx)
			code, _, ok := CloseInfo(err)
			if !ok || code != 1007 || len(message.Data) != 0 {
				t.Fatalf("invalid UTF-8 escaped validation: %+v %v", message, err)
			}
		})
	}
}

func TestInputValidationAndCloseCodes(t *testing.T) {
	ctx := testContext(t)
	for _, endpoint := range []string{"http://localhost", "wss://user:password@localhost/", "ws://localhost/#secret", "ws:///path", "file:///tmp/x"} {
		if c, _, err := Dial(ctx, endpoint, Options{}); err == nil || c != nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	for _, opts := range []Options{{MaxSendBytes: MaxMessageBytes + 1}, {MaxReceiveBytes: -1}, {ConnectTimeout: -1}, {ProxyMode: "invalid"}, {ProxyMode: "custom", ProxyURL: "ftp://localhost"}, {Header: http.Header{"Sec-WebSocket-Key": {"manual"}}}, {Header: http.Header{"X-Test": {"line\r\nbreak"}}}, {Subprotocols: []string{"space token"}}, {Subprotocols: []string{"same", "same"}}, {TLSConfig: &tls.Config{MaxVersion: tls.VersionTLS11}}} {
		if c, _, err := Dial(ctx, "ws://localhost:1", opts); err == nil || c != nil {
			t.Fatalf("accepted options %+v", opts)
		}
	}
	for _, code := range []int{0, 999, 1004, 1005, 1006, 1015, 2999, 5000} {
		if ValidateClose(code, "") == nil {
			t.Fatalf("accepted close code %d", code)
		}
	}
	for _, code := range []int{1000, 1001, 1007, 1014, 3000, 4999} {
		if err := ValidateClose(code, "Привет"); err != nil {
			t.Fatalf("rejected close code %d: %v", code, err)
		}
	}
	if ValidateClose(1000, strings.Repeat("x", 124)) == nil || ValidateClose(1000, string([]byte{255})) == nil {
		t.Fatal("accepted invalid close reason")
	}
}

func TestReadCancelHandshakeTimeoutAndBlockedWriter(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(echoHandler(t, nil))
	defer server.Close()
	c, _ := dialTest(t, ctx, wsURL(server), Options{})
	readctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := c.Read(readctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stall.Close()
	started := time.Now()
	if _, info, err := Dial(ctx, wsURL(stall), Options{ConnectTimeout: 40 * time.Millisecond}); err == nil || info.Duration < 30*time.Millisecond || time.Since(started) > time.Second {
		t.Fatalf("handshake deadline not enforced: %v %+v", err, info)
	}
	stop := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer peer.CloseNow()
		<-stop
	}))
	defer blocked.Close()
	defer close(stop)
	writer, _ := dialTest(t, ctx, wsURL(blocked), Options{MaxSendBytes: MaxMessageBytes})
	writeCtx, writeCancel := context.WithTimeout(ctx, 70*time.Millisecond)
	defer writeCancel()
	// Repeated full-size messages exceed local TCP buffering even on hosts with
	// an unusually large send buffer; the first blocked write must honor ctx.
	message := bytes.Repeat([]byte{1}, MaxMessageBytes)
	for range 32 {
		err := writer.Write(writeCtx, "binary", message)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked write: %v", err)
			}
			return
		}
	}
	t.Fatal("slow peer did not apply bounded backpressure")
}

func testTLSIdentity(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay.test"}, DNSNames: []string{"relay.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	return identity, roots
}

func TestTLSVerifyCustomCAMTLSExplicitSNIAndHTTP11Only(t *testing.T) {
	ctx := testContext(t)
	identity, roots := testTLSIdentity(t)
	var helloMu sync.Mutex
	var helloName string
	var helloProtocols []string
	server := httptest.NewUnstartedServer(echoHandler(t, func(r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || r.Proto != "HTTP/1.1" {
			t.Error("mTLS or wire protocol absent")
		}
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, NextProtos: []string{"h2", "http/1.1"}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		helloMu.Lock()
		helloName = hello.ServerName
		helloProtocols = append([]string(nil), hello.SupportedProtos...)
		helloMu.Unlock()
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	if c, _, err := Dial(ctx, wsURL(server), Options{}); err == nil || c != nil {
		t.Fatal("default TLS accepted unknown CA")
	}
	if c, _, err := Dial(ctx, wsURL(server), Options{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "relay.test"}}); err == nil || c != nil {
		t.Fatal("mTLS accepted missing client certificate")
	}
	profile := &tls.Config{RootCAs: roots, ServerName: "relay.test", Certificates: []tls.Certificate{identity}, NextProtos: []string{"h2"}}
	c, info := dialTest(t, ctx, wsURL(server), Options{TLSConfig: profile})
	if info.TLS == nil || info.TLS.NegotiatedProtocol != "" && info.TLS.NegotiatedProtocol != "http/1.1" || info.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("missing TLS result: %+v", info.TLS)
	}
	helloMu.Lock()
	name, protocols := helloName, helloProtocols
	helloMu.Unlock()
	if name != "relay.test" || len(protocols) > 1 || len(protocols) == 1 && protocols[0] != "http/1.1" || !reflect.DeepEqual(profile.NextProtos, []string{"h2"}) {
		t.Fatalf("SNI/ALPN profile not isolated: %q %v", name, protocols)
	}
	roundTrip(t, ctx, c, "text", []byte("secure"))
	insecure, _ := dialTest(t, ctx, wsURL(server), Options{TLSConfig: &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{identity}}})
	roundTrip(t, ctx, insecure, "text", []byte("explicitly insecure"))
}
