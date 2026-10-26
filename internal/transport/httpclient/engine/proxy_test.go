package engine

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFixture(t *testing.T, encrypted bool, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Method == http.MethodConnect {
			upstream, err := net.DialTimeout("tcp", req.Host, time.Second)
			if err != nil {
				http.Error(w, "target unavailable", 502)
				return
			}
			client, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = rw.Flush()
			go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); _ = client.Close() }()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			_ = upstream.Close()
			return
		}
		out := req.Clone(req.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := transport.RoundTrip(out)
		if err != nil {
			http.Error(w, "target unavailable", 502)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	var s *httptest.Server
	if encrypted {
		s = httptest.NewTLSServer(handler)
	} else {
		s = httptest.NewServer(handler)
	}
	t.Cleanup(s.Close)
	return s
}

func TestHTTPAndHTTPSProxiesAndStrictHTTP2Connect(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, version := range []string{"auto", "1.1", "2"} {
			t.Run(fmt.Sprintf("tls-proxy=%t/version=%s", encrypted, version), func(t *testing.T) {
				var calls atomic.Int32
				proxy := proxyFixture(t, encrypted, &calls)
				target, profile := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Proxy-Authorization") != "" {
						t.Error("proxy credentials reached target")
					}
					_, _ = io.WriteString(w, r.Proto)
				}), true)
				if encrypted {
					profile.RootCAs.AddCert(proxy.Certificate())
				}
				proxyURL := strings.Replace(proxy.URL, "://", "://user:proxy-password@", 1)
				c := newClient(t, Options{Version: version, TLSConfig: profile, ProxyMode: "custom", ProxyURL: proxyURL})
				req, _ := http.NewRequest("GET", target.URL, nil)
				resp, body := readResponse(t, c, req)
				want := "HTTP/2.0"
				if version == "1.1" {
					want = "HTTP/1.1"
				}
				if string(body) != want || resp.Proto != want || calls.Load() != 1 {
					t.Fatalf("proxy or protocol: %s %q calls=%d", resp.Proto, body, calls.Load())
				}
			})
		}
	}
	// Forward (absolute-form) proxy requests must work for plain HTTP too.
	var calls atomic.Int32
	proxy := proxyFixture(t, false, &calls)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { _, _ = io.WriteString(w, "forwarded") }))
	defer target.Close()
	c := newClient(t, Options{ProxyMode: "custom", ProxyURL: proxy.URL})
	req, _ := http.NewRequest("GET", target.URL, nil)
	_, body := readResponse(t, c, req)
	if string(body) != "forwarded" || calls.Load() != 1 {
		t.Fatal("plain HTTP proxy was bypassed")
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
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(conn)
				header := make([]byte, 2)
				if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(reader, methods); err != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				command := make([]byte, 4)
				if _, err := io.ReadFull(reader, command); err != nil || command[0] != 5 || command[1] != 1 {
					return
				}
				var host string
				switch command[3] {
				case 1:
					address := make([]byte, 4)
					if _, err := io.ReadFull(reader, address); err != nil {
						return
					}
					host = net.IP(address).String()
				case 3:
					length, err := reader.ReadByte()
					if err != nil {
						return
					}
					address := make([]byte, int(length))
					if _, err := io.ReadFull(reader, address); err != nil {
						return
					}
					host = string(address)
				case 4:
					address := make([]byte, 16)
					if _, err := io.ReadFull(reader, address); err != nil {
						return
					}
					host = net.IP(address).String()
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(reader, port); err != nil {
					return
				}
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				calls.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close() }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	return "socks5://" + listener.Addr().String()
}

func TestSOCKS5Proxy(t *testing.T) {
	for _, version := range []string{"1.1", "2"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			proxy := socksFixture(t, &calls)
			target, profile := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "socks") }), true)
			c := newClient(t, Options{Version: version, TLSConfig: profile, ProxyMode: "custom", ProxyURL: proxy})
			req, _ := http.NewRequest("GET", target.URL, nil)
			_, body := readResponse(t, c, req)
			if string(body) != "socks" || calls.Load() != 1 {
				t.Fatal("SOCKS5 proxy bypassed")
			}
		})
	}
}

func TestEnvironmentProxyAndNoProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyCalls.Add(1)
		_, _ = io.WriteString(w, "through proxy")
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("REQUEST_METHOD", "")
	c := newClient(t, Options{ProxyMode: "environment"})
	req, _ := http.NewRequest("GET", "http://relay-test.invalid/", nil)
	_, body := readResponse(t, c, req)
	if string(body) != "through proxy" || proxyCalls.Load() != 1 {
		t.Fatal("environment proxy bypassed")
	}
	t.Setenv("NO_PROXY", "relay-test.invalid")
	// New clients snapshot the current environment, avoiding net/http's
	// process-wide cached environment that makes settings edits ineffective.
	noProxyPolicy, err := proxyPolicy(Options{ProxyMode: "environment"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := noProxyPolicy(req)
	if err != nil || selected != nil {
		t.Fatalf("NO_PROXY did not exclude host: %v %v", selected, err)
	}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { _, _ = io.WriteString(w, "direct") }))
	defer local.Close()
	t.Setenv("NO_PROXY", "*")
	for _, mode := range []string{"environment", "none"} {
		direct := newClient(t, Options{ProxyMode: mode})
		localReq, _ := http.NewRequest("GET", local.URL, nil)
		_, body = readResponse(t, direct, localReq)
		if string(body) != "direct" || proxyCalls.Load() != 1 {
			t.Fatalf("%s should use direct connection", mode)
		}
	}
}

func TestUnsupportedProxiesAndOptionsFailClearly(t *testing.T) {
	for _, version := range []string{"1.0", "h2c", "3"} {
		if _, err := New(Options{Version: version, ProxyMode: "custom", ProxyURL: "http://127.0.0.1:1"}); err == nil || !strings.Contains(err.Error(), "proxy") {
			t.Fatalf("HTTP %s accepted unsupported proxy: %v", version, err)
		}
	}
	for _, opts := range []Options{{Version: "4"}, {ProxyMode: "bad"}, {ProxyMode: "custom", ProxyURL: "ftp://user:VERY_SECRET@localhost"}, {Timeout: -1}, {TLSConfig: &tls.Config{MaxVersion: tls.VersionTLS11}}} {
		if _, err := New(opts); err == nil || strings.Contains(err.Error(), "VERY_SECRET") {
			t.Fatalf("invalid option or leaked credentials: %v", err)
		}
	}
	t.Setenv("HTTP_PROXY", "http://user:VERY_SECRET@localhost:1")
	t.Setenv("HTTPS_PROXY", "http://user:VERY_SECRET@localhost:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("REQUEST_METHOD", "")
	for _, version := range []string{"1.0", "h2c", "3"} {
		c := newClient(t, Options{Version: version, ProxyMode: "environment"})
		scheme := "http"
		if version == "3" {
			scheme = "https"
		}
		req, _ := http.NewRequest("GET", scheme+"://relay-test.invalid/", nil)
		if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "proxy") || strings.Contains(err.Error(), "VERY_SECRET") {
			t.Fatalf("unsupported environment proxy: %v", err)
		}
	}
}
