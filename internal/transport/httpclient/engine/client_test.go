package engine

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	quichttp3 "github.com/quic-go/quic-go/http3"
)

func newClient(t *testing.T, opts Options) *Client {
	t.Helper()
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func tlsFixture(t *testing.T, handler http.Handler, h2 bool) (*httptest.Server, *tls.Config) {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.EnableHTTP2 = h2
	s.StartTLS()
	t.Cleanup(s.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return s, &tls.Config{RootCAs: pool}
}

func h2cFixture(t *testing.T, handler http.Handler) string {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.Config.Protocols = &http.Protocols{}
	s.Config.Protocols.SetUnencryptedHTTP2(true)
	s.Start()
	t.Cleanup(s.Close)
	return s.URL
}

func h3Fixture(t *testing.T, handler http.Handler) (string, *tls.Config) {
	t.Helper()
	s, profile := tlsFixture(t, handler, false)
	cert := s.TLS.Certificates[0]
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &quichttp3.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(udp) }()
	t.Cleanup(func() { _ = server.Close(); _ = udp.Close(); <-done })
	return "https://" + udp.LocalAddr().String(), profile
}

func readResponse(t *testing.T, c *Client, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestGETPOSTVersionMatrix(t *testing.T) {
	for _, version := range []string{"auto", "1.0", "1.1", "2", "h2c", "3"} {
		t.Run(version, func(t *testing.T) {
			seen := make(chan string, 2)
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				seen <- req.Proto
				if req.URL.RawQuery != "a=1&a=2" || len(req.Header.Values("X-Repeated")) != 2 {
					t.Errorf("query or duplicate headers changed: %s %v", req.URL.RawQuery, req.Header)
				}
				w.Header().Add("X-Repeated", "one")
				w.Header().Add("X-Repeated", "two")
				if req.ProtoMajor != 1 || req.ProtoMinor != 0 {
					w.Header().Set("Trailer", "X-Finished")
				}
				_, _ = fmt.Fprintf(w, "%s:%s", req.Method, body)
				w.Header().Set("X-Finished", "yes")
			})
			var target string
			var profile *tls.Config
			switch version {
			case "h2c":
				target = h2cFixture(t, handler)
			case "3":
				target, profile = h3Fixture(t, handler)
			default:
				s, p := tlsFixture(t, handler, true)
				target, profile = s.URL, p
			}
			client := newClient(t, Options{Version: version, TLSConfig: profile})
			want := map[string]string{"auto": "HTTP/2.0", "1.0": "HTTP/1.0", "1.1": "HTTP/1.1", "2": "HTTP/2.0", "h2c": "HTTP/2.0", "3": "HTTP/3.0"}[version]
			for _, method := range []string{"GET", "POST"} {
				var body io.Reader
				payload := ""
				if method == "POST" {
					payload, body = "streamed payload", strings.NewReader("streamed payload")
				}
				req, _ := http.NewRequest(method, target+"/?a=1&a=2", body)
				req.Header.Add("X-Repeated", "one")
				req.Header.Add("X-Repeated", "two")
				resp, got := readResponse(t, client, req)
				if string(got) != method+":"+payload || resp.Proto != want || <-seen != want || len(resp.Header.Values("X-Repeated")) != 2 {
					t.Fatalf("response protocol/body/header: %s %q %v", resp.Proto, got, resp.Header)
				}
				if version != "1.0" && resp.Trailer.Get("X-Finished") != "yes" {
					t.Fatalf("missing response trailer: %v", resp.Trailer)
				}
				if version != "h2c" && resp.TLS == nil {
					t.Fatal("TLS details missing")
				}
			}
		})
	}
}

func TestForcedVersionsNeverFallback(t *testing.T) {
	var calls atomic.Int32
	s, profile := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), false)
	for _, version := range []string{"2", "3"} {
		c := newClient(t, Options{Version: version, TLSConfig: profile, Timeout: 150 * time.Millisecond})
		req, _ := http.NewRequest("GET", s.URL, nil)
		if _, err := c.Do(req); err == nil {
			t.Fatalf("HTTP %s silently fell back", version)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("incompatible server received a fallback request")
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer plain.Close()
	for _, tc := range []struct{ version, target string }{{"2", plain.URL}, {"3", plain.URL}, {"h2c", s.URL}, {"h2c", plain.URL}} {
		c := newClient(t, Options{Version: tc.version, Timeout: 150 * time.Millisecond})
		req, _ := http.NewRequest("GET", tc.target, nil)
		if _, err := c.Do(req); err == nil {
			t.Fatalf("HTTP %s accepted incompatible server/scheme", tc.version)
		}
	}
}

func TestStreamResponseLifetimeCancellationAndTimeout(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2", "h2c", "3"} {
		for _, action := range []string{"cancel", "timeout", "close"} {
			t.Run(version+"/"+action, func(t *testing.T) {
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				})
				var target string
				var profile *tls.Config
				switch version {
				case "h2c":
					target = h2cFixture(t, handler)
				case "3":
					target, profile = h3Fixture(t, handler)
				default:
					s, p := tlsFixture(t, handler, true)
					target, profile = s.URL, p
				}
				timeout := 3 * time.Second
				if action == "timeout" {
					timeout = 150 * time.Millisecond
				}
				c := newClient(t, Options{Version: version, TLSConfig: profile, Timeout: timeout})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
				resp, err := c.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				done := make(chan error, 1)
				go func() { _, err := io.ReadAll(resp.Body); done <- err }()
				switch action {
				case "cancel":
					cancel()
				case "close":
					_ = c.Close()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("interrupted body read succeeded")
					}
					if action == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("body timeout lost context: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("live response body was not interrupted")
				}
			})
		}
	}
}

func TestRedirectsCredentialsCookiesAndLimit(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	var destinationHeaders http.Header
	var destinationQuery string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		destinationHeaders = req.Header.Clone()
		destinationQuery = req.URL.RawQuery
		_, _ = io.WriteString(w, "destination")
	}))
	defer destination.Close()
	originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/cross":
			http.Redirect(w, req, destination.URL+"?b=1&api%5Fkey=redirect-secret&b=2&api_key=second-secret", http.StatusFound)
		case "/loop":
			http.Redirect(w, req, "/loop", http.StatusFound)
		case "/set":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "yes"})
			http.Redirect(w, req, "/cookie", http.StatusFound)
		case "/cookie":
			_, _ = io.WriteString(w, req.Header.Get("Cookie"))
		}
	}))
	defer originServer.Close()
	var redirects atomic.Int32
	c := newClient(t, Options{FollowRedirects: true, MaxRedirects: 2, Jar: jar, SensitiveHeaders: []string{"X-API-Key"}, SensitiveQueryKeys: []string{"api_key"}, OnRedirect: func(resp *http.Response, u *url.URL) {
		if resp == nil || u == nil {
			t.Error("redirect metadata missing")
		}
		redirects.Add(1)
	}})
	req, _ := http.NewRequest("GET", originServer.URL+"/set", nil)
	_, body := readResponse(t, c, req)
	if !strings.Contains(string(body), "session=yes") {
		t.Fatalf("session jar did not survive redirect: %q", body)
	}
	req, _ = http.NewRequest("GET", originServer.URL+"/cross?api_key=query-secret", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "explicit=secret")
	req.Header.Set("Proxy-Authorization", "Basic secret")
	req.Header.Set("X-API-Key", "header-secret")
	_, _ = readResponse(t, c, req)
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Referer", "X-API-Key"} {
		if destinationHeaders.Get(name) != "" {
			t.Fatalf("cross-port redirect exposed %s", name)
		}
	}
	if destinationQuery != "b=1&b=2" {
		t.Fatalf("sensitive query leaked or duplicate order changed: %s", destinationQuery)
	}
	req, _ = http.NewRequest("GET", originServer.URL+"/loop", nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("redirect limit: %v", err)
	}
	noFollow := newClient(t, Options{})
	req, _ = http.NewRequest("GET", originServer.URL+"/cross", nil)
	resp, _ := readResponse(t, noFollow, req)
	if resp.StatusCode != http.StatusFound || redirects.Load() != 5 {
		t.Fatalf("redirect policy: %d count=%d", resp.StatusCode, redirects.Load())
	}
}

func TestCrossOriginRedirectDoesNotExposeMTLSIdentity(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		destinationCalls.Add(1)
	}))
	destination.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	destination.StartTLS()
	defer destination.Close()
	originServer, profile := tlsFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/cross":
			http.Redirect(w, req, destination.URL, http.StatusFound)
		case "/same":
			http.Redirect(w, req, "/final", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "same origin")
		}
	}), true)
	profile.RootCAs.AddCert(destination.Certificate())
	cert := originServer.TLS.Certificates[0]
	for _, useCallback := range []bool{false, true} {
		cfg := profile.Clone()
		if useCallback {
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
		} else {
			cfg.Certificates = []tls.Certificate{cert}
		}
		c := newClient(t, Options{TLSConfig: cfg, FollowRedirects: true})
		req, _ := http.NewRequest("GET", originServer.URL+"/cross", nil)
		if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "mTLS identity") {
			t.Fatalf("mTLS cross-origin redirect was not explicitly blocked: %v", err)
		}
		req, _ = http.NewRequest("GET", originServer.URL+"/same", nil)
		_, body := readResponse(t, c, req)
		if string(body) != "same origin" {
			t.Fatalf("same-origin redirect failed: %q", body)
		}
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("cross-origin server received an mTLS request")
	}
}

func TestCompressionStreamsAndPreservesEncoding(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "raw-deflate"} {
		t.Run(encoding, func(t *testing.T) {
			var data bytes.Buffer
			var compressor io.WriteCloser
			switch encoding {
			case "gzip":
				compressor = gzip.NewWriter(&data)
			case "deflate":
				compressor = zlib.NewWriter(&data)
			default:
				compressor, _ = flate.NewWriter(&data, flate.DefaultCompression)
			}
			_, _ = io.WriteString(compressor, strings.Repeat("large decoded body", 10000))
			_ = compressor.Close()
			value := encoding
			if value == "raw-deflate" {
				value = "deflate"
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", value)
				_, _ = w.Write(data.Bytes())
			}))
			defer s.Close()
			c := newClient(t, Options{Decompress: true})
			req, _ := http.NewRequest("GET", s.URL, nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := io.ReadAll(io.LimitReader(resp.Body, 32))
			_ = resp.Body.Close()
			if err != nil || len(preview) != 32 || !resp.Uncompressed || resp.Header.Get("Content-Encoding") != value || resp.ContentLength != -1 {
				t.Fatalf("decode: %q %v %#v", preview, err, resp)
			}
			raw := newClient(t, Options{})
			req, _ = http.NewRequest("GET", s.URL, nil)
			resp, body := readResponse(t, raw, req)
			if resp.Uncompressed || !bytes.Equal(body, data.Bytes()) {
				t.Fatal("decompression-off changed wire body")
			}
		})
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, "raw")
	}))
	defer s.Close()
	c := newClient(t, Options{Decompress: true})
	req, _ := http.NewRequest("GET", s.URL, nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "Content-Encoding") {
		t.Fatalf("unsupported decoder: %v", err)
	}
	t.Run("compression layer limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Encoding", strings.Repeat("gzip,", 10000)+"gzip")
			_, _ = io.WriteString(w, "untrusted compressed bytes")
		}))
		defer server.Close()
		req, _ := http.NewRequest("GET", server.URL, nil)
		if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "8 compression layers") {
			t.Fatalf("compression layers were not bounded: %v", err)
		}
	})
	t.Run("multiple encoding header fields", func(t *testing.T) {
		var inner, outer bytes.Buffer
		gz := gzip.NewWriter(&inner)
		_, _ = io.WriteString(gz, "nested encodings")
		_ = gz.Close()
		zl := zlib.NewWriter(&outer)
		_, _ = zl.Write(inner.Bytes())
		_ = zl.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Add("Content-Encoding", "gzip")
			w.Header().Add("Content-Encoding", "deflate")
			_, _ = w.Write(outer.Bytes())
		}))
		defer server.Close()
		req, _ := http.NewRequest("GET", server.URL, nil)
		resp, body := readResponse(t, c, req)
		if string(body) != "nested encodings" || len(resp.Header.Values("Content-Encoding")) != 2 {
			t.Fatalf("nested body or original encoding metadata lost: %q %v", body, resp.Header)
		}
	})
}

func TestMultipartAndChunkedUploadsAreStreamed(t *testing.T) {
	for _, version := range []string{"1.1", "2", "h2c", "3"} {
		t.Run(version, func(t *testing.T) {
			gotPart := make(chan struct{})
			release := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				reader, err := req.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				part, err := reader.NextPart()
				if err != nil {
					t.Error(err)
					return
				}
				first := make([]byte, 4)
				_, _ = io.ReadFull(part, first)
				close(gotPart)
				<-release
				rest, _ := io.ReadAll(part)
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = w.Write(append(first, rest...))
			})
			var target string
			var profile *tls.Config
			switch version {
			case "h2c":
				target = h2cFixture(t, handler)
			case "3":
				target, profile = h3Fixture(t, handler)
			default:
				s, p := tlsFixture(t, handler, true)
				target, profile = s.URL, p
			}
			pr, pw := io.Pipe()
			writer := multipart.NewWriter(pw)
			req, _ := http.NewRequest("POST", target, pr)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			c := newClient(t, Options{Version: version, TLSConfig: profile})
			done := make(chan error, 1)
			go func() {
				resp, err := c.Do(req)
				if err == nil {
					defer resp.Body.Close()
					body, readErr := io.ReadAll(resp.Body)
					err = readErr
					if string(body) != "first-last" {
						err = fmt.Errorf("multipart body %q", body)
					}
				}
				done <- err
			}()
			producer := make(chan error, 1)
			go func() {
				part, err := writer.CreateFormFile("file", "payload.txt")
				if err == nil {
					_, err = io.WriteString(part, "first-")
				}
				if err == nil {
					select {
					case <-gotPart:
					case <-time.After(time.Second):
						err = errors.New("server did not see upload before producer finished")
					}
				}
				close(release)
				if err == nil {
					_, err = io.WriteString(part, "last")
				}
				_ = writer.Close()
				_ = pw.CloseWithError(err)
				producer <- err
			}()
			if err := <-producer; err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTP10ActualWireAndUnknownLengthRejected(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil || line != "POST /wire HTTP/1.0\r\n" {
			done <- fmt.Errorf("request line %q: %v", line, err)
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(line), reader)))
		if err == nil {
			body, _ := io.ReadAll(req.Body)
			if string(body) != "stream" || req.ContentLength != 6 || len(req.TransferEncoding) != 0 {
				err = errors.New("HTTP/1.0 framing invalid")
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok")
		done <- err
	}()
	c := newClient(t, Options{Version: "1.0"})
	target := "http://" + listener.Addr().String() + "/wire"
	req, _ := http.NewRequest("POST", target, io.NopCloser(strings.NewReader("stream")))
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "known Content-Length") {
		t.Fatalf("unknown HTTP/1.0 length: %v", err)
	}
	req, _ = http.NewRequest("POST", target, strings.NewReader("stream"))
	_, body := readResponse(t, c, req)
	if string(body) != "ok" {
		t.Fatal(string(body))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTLSProfilesTrustMTLSAndIsolation(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2", "3"} {
		t.Run(version, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "verified") })
			var target string
			var trusted *tls.Config
			if version == "3" {
				target, trusted = h3Fixture(t, handler)
			} else {
				s, p := tlsFixture(t, handler, true)
				target, trusted = s.URL, p
			}
			c := newClient(t, Options{Version: version})
			req, _ := http.NewRequest("GET", target, nil)
			if _, err := c.Do(req); err == nil {
				t.Fatal("self-signed certificate accepted without explicit trust")
			}
			for _, profile := range []*tls.Config{trusted, {InsecureSkipVerify: true}} {
				c := newClient(t, Options{Version: version, TLSConfig: profile})
				req, _ = http.NewRequest("GET", target, nil)
				_, _ = readResponse(t, c, req)
				if profile.MinVersion != 0 || len(profile.NextProtos) != 0 || profile.VerifyConnection != nil {
					t.Fatal("caller TLS profile mutated")
				}
			}
		})
	}
	// Generate an explicit clientAuth identity independent of the server key.
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Error("mTLS certificate absent")
		}
		_, _ = io.WriteString(w, "mutual")
	}))
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := seed.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(seed.Certificate())
	seed.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	clientCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	clientLeaf, _ := x509.ParseCertificate(der)
	pool.AddCert(clientLeaf)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	s.StartTLS()
	defer s.Close()
	for _, profile := range []*tls.Config{{RootCAs: pool}, {RootCAs: pool, Certificates: []tls.Certificate{clientCert}}} {
		c := newClient(t, Options{Version: "1.1", TLSConfig: profile})
		req, _ := http.NewRequest("GET", s.URL, nil)
		resp, err := c.Do(req)
		if len(profile.Certificates) == 0 {
			if err == nil {
				resp.Body.Close()
				t.Fatal("mTLS server accepted absent client certificate")
			}
		} else if err != nil {
			t.Fatalf("mTLS failed: %v", err)
		} else {
			resp.Body.Close()
		}
	}
}

func TestHeaderTimeoutAndErrorsHideURLSecrets(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2", "h2c", "3"} {
		t.Run(version, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
			var target string
			var profile *tls.Config
			switch version {
			case "h2c":
				target = h2cFixture(t, handler)
			case "3":
				target, profile = h3Fixture(t, handler)
			default:
				s, p := tlsFixture(t, handler, true)
				target, profile = s.URL, p
			}
			c := newClient(t, Options{Version: version, TLSConfig: profile, ResponseHeaderTimeout: 60 * time.Millisecond, Timeout: time.Second})
			req, _ := http.NewRequest("GET", target+"?token=VERY_SECRET_QUERY", nil)
			before := time.Now()
			_, err := c.Do(req)
			if err == nil || strings.Contains(err.Error(), "VERY_SECRET_QUERY") || time.Since(before) > 800*time.Millisecond {
				t.Fatalf("response header deadline or safe error failed: %v", err)
			}
		})
	}
}
