// Package http10 implements direct HTTP/1.0 over TCP or verified TLS. Each
// request uses a separate connection with an explicit Content-Length and close.
package http10

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"relay/internal/transport/httpclient"
)

type Config = httpclient.Config

// Client is safe for concurrent requests. Do does not follow redirects or
// implement proxying, tunneling, keep-alive, or protocol upgrades.
type Client struct {
	config   Config
	lifetime context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	active   map[net.Conn]struct{}
}

func New(config Config) (*Client, error) {
	config, err := httpclient.Normalize(config, tls.VersionTLS12)
	if err != nil {
		return nil, err
	}
	// Advertising h2 on an HTTP/1.0 socket would violate ALPN.
	config.TLSConfig.NextProtos = nil
	lifetime, cancel := context.WithCancel(context.Background())
	return &Client{config: config, lifetime: lifetime, cancel: cancel, active: make(map[net.Conn]struct{})}, nil
}

func (c *Client) Close() error {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for conn := range c.active {
		_ = conn.Close()
	}
	return nil
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("HTTP/1.0: request is required")
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	ctx, cancel := context.WithTimeout(req.Context(), c.config.Timeout)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	if c.lifetime.Err() != nil {
		return nil, errors.New("HTTP/1.0 client is closed")
	}
	prepared, err := httpclient.Prepare(ctx, req, c.config)
	if err != nil {
		return nil, fmt.Errorf("HTTP/1.0 prepare request: %w", err)
	}
	resp, err := c.do(ctx, prepared)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("HTTP/1.0 request: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("HTTP/1.0 request: %w", err)
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, errors.New("client is closed")
	}
	port := req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(req.URL.Hostname(), port)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP connect: %w", err)
	}
	defer conn.Close()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("client is closed")
	}
	c.active[conn] = struct{}{}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.active, conn); c.mu.Unlock() }()
	// Closing the connection interrupts TLS, request writes and blocked reads.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var rw net.Conn = conn
	var tlsState *tls.ConnectionState
	if req.URL.Scheme == "https" {
		profile := c.config.TLSConfig.Clone()
		if profile.ServerName == "" {
			profile.ServerName = req.URL.Hostname()
		}
		tlsConn := tls.Client(conn, profile)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		state := tlsConn.ConnectionState()
		if state.NegotiatedProtocol != "" {
			return nil, errors.New("TLS selected an ALPN protocol incompatible with HTTP/1.0")
		}
		tlsState = &state
		rw = tlsConn
	}
	writer := bufio.NewWriter(rw)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if _, err := fmt.Fprintf(writer, "%s %s HTTP/1.0\r\nHost: %s\r\nContent-Length: %s\r\nConnection: close\r\n", req.Method, req.URL.RequestURI(), host, strconv.FormatInt(req.ContentLength, 10)); err != nil {
		return nil, fmt.Errorf("write request line: %w", err)
	}
	if err := req.Header.Write(writer); err != nil {
		return nil, fmt.Errorf("write request headers: %w", err)
	}
	if _, err := writer.WriteString("\r\n"); err != nil {
		return nil, fmt.Errorf("write request headers: %w", err)
	}
	if _, err := io.Copy(writer, req.Body); err != nil {
		return nil, fmt.Errorf("write request body: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush request: %w", err)
	}
	reader := &headerLimitReader{reader: bufio.NewReader(rw), remaining: c.config.MaxResponseHeaderBytes}
	resp, err := http.ReadResponse(bufio.NewReader(reader), req)
	if err != nil {
		return nil, fmt.Errorf("read response headers: %w", err)
	}
	reader.headersDone = true
	// net/http's parsed response body may drain to EOF on Close. Close our
	// single-use connection first, so rejecting a huge body never drains it.
	resp.Body = &connectionBody{ReadCloser: resp.Body, conn: conn}
	if resp.ProtoMajor != 1 || resp.ProtoMinor > 1 {
		_ = resp.Body.Close()
		return nil, errors.New("response protocol is incompatible with HTTP/1.0 mode")
	}
	// HTTP/1.0 has no transfer codings. Never silently consume a server's
	// HTTP/1.1 chunking as if the requested wire mode supported it.
	if len(resp.TransferEncoding) != 0 || strings.Contains(strings.ToLower(resp.Header.Get("Connection")), "upgrade") || resp.StatusCode == http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return nil, errors.New("response transfer encoding or upgrade is unsupported in HTTP/1.0 mode")
	}
	if resp.StatusCode >= 100 && resp.StatusCode < 200 {
		_ = resp.Body.Close()
		return nil, errors.New("informational responses are unsupported in HTTP/1.0 mode")
	}
	resp.TLS = tlsState
	return httpclient.BufferResponse(ctx, resp, c.config.MaxResponseBodyBytes)
}

type connectionBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *connectionBody) Close() error {
	_ = b.conn.Close()
	return b.ReadCloser.Close()
}

// Reading headers byte by byte prevents bufio's read-ahead from charging body
// bytes against the header budget. The normal buffered reader takes over once
// the blank line is reached; the hard byte bound limits even one huge line.
type headerLimitReader struct {
	reader      io.Reader
	remaining   int
	headersDone bool
	last        [4]byte
	n           int
}

func (r *headerLimitReader) Read(p []byte) (int, error) {
	if r.headersDone {
		return r.reader.Read(p)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining <= 0 {
		return 0, errors.New("response header limit exceeded")
	}
	n, err := r.reader.Read(p[:1])
	if n > 0 {
		r.remaining--
		r.last[r.n%4] = p[0]
		r.n++
		if r.n >= 4 && r.last[(r.n-4)%4] == '\r' && r.last[(r.n-3)%4] == '\n' && r.last[(r.n-2)%4] == '\r' && r.last[(r.n-1)%4] == '\n' {
			r.headersDone = true
		}
	}
	return n, err
}
