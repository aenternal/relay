package websocketclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFixture(t *testing.T, encrypted bool, calls, certificates *atomic.Int32) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.TLS != nil && certificates != nil {
			certificates.Store(int32(len(r.TLS.PeerCertificates)))
		}
		destination := r.Host
		if !strings.Contains(destination, ":") {
			destination += ":80"
		}
		upstream, err := net.DialTimeout("tcp", destination, time.Second)
		if err != nil {
			http.Error(w, "unavailable", 502)
			return
		}
		defer upstream.Close()
		client, reader, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if r.Method == http.MethodConnect {
			_, _ = reader.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = reader.Flush()
		} else {
			r.RequestURI = ""
			r.URL.Scheme = ""
			r.URL.Host = ""
			r.Header.Del("Proxy-Authorization")
			if err := r.Write(upstream); err != nil {
				return
			}
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	})
	server := httptest.NewUnstartedServer(handler)
	if encrypted {
		server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

func TestHTTPHTTPSAndSOCKS5Proxies(t *testing.T) {
	for _, targetTLS := range []bool{false, true} {
		for _, proxyTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("targetTLS=%t/proxyTLS=%t", targetTLS, proxyTLS), func(t *testing.T) {
				ctx := testContext(t)
				identity, roots := testTLSIdentity(t)
				var calls, offeredIdentity atomic.Int32
				proxy := proxyFixture(t, proxyTLS, &calls, &offeredIdentity)
				if proxyTLS {
					roots.AddCert(proxy.Certificate())
				}
				target := httptest.NewUnstartedServer(echoHandler(t, func(r *http.Request) {
					if r.Header.Get("Proxy-Authorization") != "" {
						t.Error("proxy credentials reached origin")
					}
					if targetTLS && (r.TLS == nil || len(r.TLS.PeerCertificates) != 1) {
						t.Error("origin mTLS certificate absent")
					}
				}))
				if targetTLS {
					target.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
					target.StartTLS()
				} else {
					target.Start()
				}
				defer target.Close()
				profile := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{identity}}
				if targetTLS {
					profile.ServerName = "relay.test"
				}
				proxyURL := strings.Replace(proxy.URL, "://", "://user:proxy-canary@", 1)
				c, _ := dialTest(t, ctx, wsURL(target), Options{ProxyMode: "custom", ProxyURL: proxyURL, TLSConfig: profile})
				roundTrip(t, ctx, c, "text", []byte("proxied"))
				if calls.Load() != 1 || offeredIdentity.Load() != 0 {
					t.Fatalf("proxy policy failed: calls=%d offered certificates=%d", calls.Load(), offeredIdentity.Load())
				}
			})
		}
	}
	ctx := testContext(t)
	target := httptest.NewServer(echoHandler(t, nil))
	defer target.Close()
	for _, scheme := range []string{"socks5", "socks5h"} {
		var calls atomic.Int32
		proxy := socksFixture(t, &calls)
		c, _ := dialTest(t, ctx, wsURL(target), Options{ProxyMode: "custom", ProxyURL: scheme + "://" + proxy})
		roundTrip(t, ctx, c, "binary", []byte{1, 255, 0})
		if calls.Load() != 1 {
			t.Fatalf("%s proxy bypassed", scheme)
		}
	}
}

func socksFixture(t *testing.T, calls *atomic.Int32) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				request := make([]byte, 4)
				if _, err := io.ReadFull(conn, request); err != nil || request[1] != 1 {
					return
				}
				var hostname string
				switch request[3] {
				case 1:
					ip := make([]byte, 4)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					hostname = net.IP(ip).String()
				case 3:
					length := make([]byte, 1)
					if _, err := io.ReadFull(conn, length); err != nil {
						return
					}
					name := make([]byte, int(length[0]))
					if _, err := io.ReadFull(conn, name); err != nil {
						return
					}
					hostname = string(name)
				case 4:
					ip := make([]byte, 16)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					hostname = net.IP(ip).String()
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(conn, port); err != nil {
					return
				}
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(hostname, fmt.Sprint(binary.BigEndian.Uint16(port))), time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				calls.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(done) }()
				_, _ = io.Copy(conn, upstream)
				_ = conn.Close()
				<-done
			}()
		}
	}()
	return listener.Addr().String()
}

func TestProxyDefaultNoneEnvironmentAndMalformedEnvironment(t *testing.T) {
	ctx := testContext(t)
	var calls atomic.Int32
	proxy := proxyFixture(t, false, &calls, nil)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	server := httptest.NewServer(echoHandler(t, nil))
	defer server.Close()
	c, _ := dialTest(t, ctx, wsURL(server), Options{})
	roundTrip(t, ctx, c, "text", []byte("direct"))
	if calls.Load() != 0 {
		t.Fatal("implicit environment proxy used")
	}
	// httpproxy exempts loopback. Inspect an external-looking destination using
	// the policy only; no external socket is opened.
	policy, err := proxyPolicy("environment", "")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://relay.invalid/socket", nil)
	selected, err := policy(request)
	if err != nil || selected == nil || selected.String() != proxy.URL {
		t.Fatalf("environment policy: %v %v", selected, err)
	}
	t.Setenv("HTTPS_PROXY", "ftp://invalid.example")
	if _, _, err := Dial(ctx, wsURL(server), Options{ProxyMode: "environment"}); err == nil {
		t.Fatal("invalid selected environment proxy silently fell back")
	}
}

// A raw peer is useful here only for a failed HTTP upgrade. The WebSocket
// framing itself is always implemented by the pinned library.
func TestHandshakeContextCancellationBeforeHeaders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, err = http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan error, 1)
	go func() { _, _, err := Dial(ctx, "ws://"+listener.Addr().String(), Options{}); done <- err }()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled dial succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt handshake")
	}
	<-peerDone
}
