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
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Transport is the streaming HTTP/1.0 adapter. Unlike the bounded stage-zero
// Client, it requires a known upload length and leaves the response body live.
// Its connections are single use and are released when the body is closed.
type Transport struct {
	TLSConfig              *tls.Config
	ConnectTimeout         time.Duration
	TLSHandshakeTimeout    time.Duration
	ResponseHeaderTimeout  time.Duration
	MaxResponseHeaderBytes int
}

func (t *Transport) RoundTrip(req *http.Request) (_ *http.Response, err error) {
	if req == nil {
		return nil, errors.New("HTTP/1.0 request is required")
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	if err := validateStreamingRequest(req); err != nil {
		return nil, err
	}
	ctx := req.Context()
	port := req.URL.Port()
	if port == "" {
		port = "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
	}
	addr := net.JoinHostPort(req.URL.Hostname(), port)
	conn, err := (&net.Dialer{Timeout: t.ConnectTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("HTTP/1.0 connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		if err != nil {
			stop()
			_ = conn.Close()
		}
	}()
	trace := httptrace.ContextClientTrace(ctx)
	if trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{Conn: conn})
	}
	var rw net.Conn = conn
	var state *tls.ConnectionState
	if req.URL.Scheme == "https" {
		profile := &tls.Config{MinVersion: tls.VersionTLS12}
		if t.TLSConfig != nil {
			profile = t.TLSConfig.Clone()
		}
		profile.NextProtos = nil
		if profile.ServerName == "" {
			profile.ServerName = req.URL.Hostname()
		}
		tlsConn := tls.Client(conn, profile)
		handshakeCtx := ctx
		cancel := func() {}
		if t.TLSHandshakeTimeout > 0 {
			handshakeCtx, cancel = context.WithTimeout(ctx, t.TLSHandshakeTimeout)
		}
		if trace != nil && trace.TLSHandshakeStart != nil {
			trace.TLSHandshakeStart()
		}
		err = tlsConn.HandshakeContext(handshakeCtx)
		cancel()
		cs := tlsConn.ConnectionState()
		if trace != nil && trace.TLSHandshakeDone != nil {
			trace.TLSHandshakeDone(cs, err)
		}
		if err != nil {
			return nil, fmt.Errorf("HTTP/1.0 TLS handshake: %w", err)
		}
		if cs.NegotiatedProtocol != "" {
			return nil, errors.New("HTTP/1.0 TLS selected an incompatible ALPN protocol")
		}
		state, rw = &cs, tlsConn
	}
	stopBody := func() bool { return false }
	if req.Body != nil {
		stopBody = context.AfterFunc(ctx, func() { _ = req.Body.Close() })
	}
	defer stopBody()
	writer := bufio.NewWriter(rw)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if _, err = fmt.Fprintf(writer, "%s %s HTTP/1.0\r\nHost: %s\r\nContent-Length: %d\r\nConnection: close\r\n", method, req.URL.RequestURI(), host, req.ContentLength); err != nil {
		return nil, fmt.Errorf("HTTP/1.0 write headers: %w", err)
	}
	headers := req.Header.Clone()
	headers.Del("Host")
	headers.Del("Content-Length")
	headers.Del("Connection")
	if err = headers.Write(writer); err != nil {
		return nil, err
	}
	if _, err = writer.WriteString("\r\n"); err != nil {
		return nil, err
	}
	// Flush headers before reading a streaming body: producers may wait for
	// the server to observe them before supplying their first payload bytes.
	if err = writer.Flush(); err != nil {
		return nil, err
	}
	if req.ContentLength > 0 {
		if _, err = io.CopyN(rw, req.Body, req.ContentLength); err != nil {
			return nil, fmt.Errorf("HTTP/1.0 write body: %w", err)
		}
	}
	if trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	if t.ResponseHeaderTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(t.ResponseHeaderTimeout))
	}
	limit := t.MaxResponseHeaderBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	reader := &headerLimitReader{reader: bufio.NewReader(rw), remaining: limit}
	resp, err := http.ReadResponse(bufio.NewReader(reader), req)
	if err != nil {
		return nil, fmt.Errorf("HTTP/1.0 read headers: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	reader.headersDone = true
	if trace != nil && trace.GotFirstResponseByte != nil {
		trace.GotFirstResponseByte()
	}
	resp.Body = &streamingBody{ReadCloser: resp.Body, conn: conn, stop: stop}
	if resp.ProtoMajor != 1 || resp.ProtoMinor > 1 || len(resp.TransferEncoding) != 0 || resp.StatusCode < 200 || strings.Contains(strings.ToLower(resp.Header.Get("Connection")), "upgrade") {
		_ = resp.Body.Close()
		return nil, errors.New("HTTP/1.0 response protocol, transfer encoding, or upgrade is unsupported")
	}
	resp.TLS = state
	return resp, nil
}

func validateStreamingRequest(req *http.Request) error {
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" || !httpguts.ValidHostHeader(req.URL.Host) || req.URL.Opaque != "" {
		return errors.New("HTTP/1.0 requires a valid http or https URL")
	}
	if req.URL.User != nil {
		return errors.New("HTTP/1.0 URL credentials are unsupported; use Authorization")
	}
	if (req.Method != "" && !httpguts.ValidHeaderFieldName(req.Method)) || req.Method == "CONNECT" || (req.Host != "" && !httpguts.ValidHostHeader(req.Host)) || strings.ContainsAny(req.URL.RequestURI(), " \r\n\t") {
		return errors.New("HTTP/1.0 request method, host, or path is invalid or unsupported")
	}
	if req.ContentLength < 0 || (req.Body != nil && req.Body != http.NoBody && req.ContentLength == 0) {
		return errors.New("HTTP/1.0 streaming uploads require a known Content-Length")
	}
	if req.ContentLength > 0 && (req.Body == nil || req.Body == http.NoBody) {
		return errors.New("HTTP/1.0 Content-Length requires a request body")
	}
	if len(req.TransferEncoding) != 0 || len(req.Trailer) != 0 {
		return errors.New("HTTP/1.0 does not support transfer encodings or trailers")
	}
	for name, values := range req.Header {
		if !httpguts.ValidHeaderFieldName(name) {
			return errors.New("HTTP/1.0 invalid request header name")
		}
		switch strings.ToLower(name) {
		case "transfer-encoding", "trailer", "upgrade", "expect":
			return errors.New("HTTP/1.0 transfer, upgrade, or expect headers are unsupported")
		case "content-length":
			if len(values) != 1 || values[0] != strconv.FormatInt(req.ContentLength, 10) {
				return errors.New("HTTP/1.0 Content-Length header does not match request")
			}
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return errors.New("HTTP/1.0 invalid request header value")
			}
		}
	}
	return req.Context().Err()
}

type streamingBody struct {
	io.ReadCloser
	conn net.Conn
	stop func() bool
	once sync.Once
}

func (b *streamingBody) Close() (err error) {
	b.once.Do(func() {
		b.stop()
		_ = b.conn.Close()
		err = b.ReadCloser.Close()
	})
	return err
}
