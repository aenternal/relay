package http10

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fixture observes the actual bytes on TCP, not Request.Proto fields.
func wireServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		handle(conn)
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return "http://" + ln.Addr().String()
}

func TestRequestWire(t *testing.T) {
	for _, method := range []string{"GET", "POST", "CUSTOM"} {
		t.Run(method, func(t *testing.T) {
			observed := make(chan error, 1)
			url := wireServer(t, func(conn net.Conn) {
				reader := bufio.NewReader(conn)
				line, err := reader.ReadString('\n')
				if err != nil {
					observed <- err
					return
				}
				if line != method+" /path?q=1&q=2 HTTP/1.0\r\n" {
					observed <- fmt.Errorf("wire line: %q", line)
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(line), reader)))
				if err != nil {
					observed <- err
					return
				}
				body, err := io.ReadAll(req.Body)
				if err == nil && (string(body) != "payload" || req.Host == "" || req.ContentLength != 7 || len(req.TransferEncoding) != 0 || !req.Close || len(req.Header.Values("X-Repeated")) != 2) {
					err = fmt.Errorf("unexpected wire request: %#v, body %q", req, body)
				}
				observed <- err
				_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok")
			})
			client, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			// Unknown-length bodies are bounded and buffered to obtain a real Content-Length.
			req, _ := http.NewRequest(method, url+"/path?q=1&q=2", io.NopCloser(strings.NewReader("payload")))
			req.Header.Add("X-Repeated", "one")
			req.Header.Add("X-Repeated", "two")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Proto != "HTTP/1.0" {
				t.Fatalf("protocol: %s", resp.Proto)
			}
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponseErrorsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want string
		limit            int64
	}{
		{"malformed", "not HTTP\r\n\r\n", "response", 16},
		{"body limit", "HTTP/1.0 200 OK\r\n\r\n0123456789", "limit", 4},
		{"header limit", "HTTP/1.0 200 OK\r\nX-Large: " + strings.Repeat("x", 1024) + "\r\n\r\n", "header", 16},
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "transfer", 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := wireServer(t, func(c net.Conn) { _, _ = http.ReadRequest(bufio.NewReader(c)); _, _ = io.WriteString(c, tc.wire) })
			client, err := New(Config{MaxResponseBodyBytes: tc.limit, MaxResponseHeaderBytes: 512})
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", url, nil)
			_, err = client.Do(req)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error: %v, wanted %s", err, tc.want)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		want   string
	}{
		{"CONNECT", func(r *http.Request) { r.Method = "CONNECT" }, "CONNECT"},
		{"method", func(r *http.Request) { r.Method = "GET\r\nInjected" }, "method"},
		{"header", func(r *http.Request) { r.Header.Set("X-Bad", "one\r\nX-Injected: yes") }, "header"},
		{"unknown length over limit", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("12345")); r.ContentLength = -1 }, "limit"},
		{"content length mismatch", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("12")); r.ContentLength = 3 }, "Content-Length"},
		{"chunked", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, "transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := New(Config{MaxRequestBodyBytes: 4})
			req, _ := http.NewRequest("GET", "http://127.0.0.1:1", nil)
			tc.change(req)
			_, err := client.Do(req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v, wanted %s", err, tc.want)
			}
		})
	}
}

func TestCancelBlockedResponse(t *testing.T) {
	started := make(chan struct{})
	url := wireServer(t, func(c net.Conn) {
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: 100\r\n\r\n")
		close(started)
		_, _ = io.Copy(io.Discard, c)
	})
	client, _ := New(Config{})
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
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock read")
	}
}

func TestCancelBlockedRequestBody(t *testing.T) {
	client, _ := New(Config{})
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:1", reader)
	done := make(chan error, 1)
	go func() { _, err := client.Do(req); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock request body")
	}
}

func TestTLSVerificationAndProfileIsolation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	client, _ := New(Config{})
	req, _ := http.NewRequest("GET", server.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("self-signed TLS accepted by default")
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	profile := &tls.Config{RootCAs: pool}
	trusted, _ := New(Config{TLSConfig: profile})
	resp, err := trusted.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if profile.MinVersion != 0 || len(profile.NextProtos) != 0 {
		t.Fatal("caller TLS profile was mutated")
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatal("missing secure TLS details")
	}
	insecureClient, _ := New(Config{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
	defer insecureClient.Close()
	resp, err = insecureClient.Do(req)
	if err != nil {
		t.Fatalf("explicit insecure profile: %v", err)
	}
	resp.Body.Close()
}

func TestTimeoutBlockedHeaders(t *testing.T) {
	url := wireServer(t, func(c net.Conn) {
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = io.Copy(io.Discard, c)
	})
	client, _ := New(Config{Timeout: 50 * time.Millisecond})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, nil)
	if _, err := client.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}

func TestCloseUnblocksRequestAndRejectsReuse(t *testing.T) {
	started := make(chan struct{})
	url := wireServer(t, func(c net.Conn) {
		_, _ = http.ReadRequest(bufio.NewReader(c))
		close(started)
		_, _ = io.Copy(io.Discard, c)
	})
	client, _ := New(Config{})
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

func TestOversizedContentLengthDoesNotDrain(t *testing.T) {
	url := wireServer(t, func(c net.Conn) {
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1000000000\r\n\r\n")
		_, _ = io.Copy(io.Discard, c)
	})
	client, _ := New(Config{})
	defer client.Close()
	req, _ := http.NewRequest("GET", url, nil)
	done := make(chan error, 1)
	go func() { _, err := client.Do(req); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("limit error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("body rejection tried to drain the response")
	}
}
