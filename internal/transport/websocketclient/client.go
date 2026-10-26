// Package websocketclient provides bounded RFC 6455 messages over an explicit
// HTTP/1.1 Upgrade connection. It never follows redirects or selects a proxy
// unless requested. Application histories and secret masking belong above it.
package websocketclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http/httpproxy"
)

const (
	DefaultMessageBytes = 1 << 20
	MaxMessageBytes     = 4 << 20
	closeTimeout        = time.Second
)

type Options struct {
	Header          http.Header
	Subprotocols    []string
	TLSConfig       *tls.Config
	ProxyMode       string
	ProxyURL        string
	ConnectTimeout  time.Duration
	MaxSendBytes    int
	MaxReceiveBytes int
	// These callbacks run in the reader. They must only enqueue a bounded,
	// nonblocking notification. Payloads are owned copies. Autopong is always on.
	OnPing func([]byte)
	OnPong func([]byte)
}

type Handshake struct {
	StatusCode      int
	Headers         http.Header
	Subprotocol     string
	TLS             *tls.ConnectionState
	ConnectDuration time.Duration
	Duration        time.Duration
}

type Message struct {
	Type string
	Data []byte
}

type Client struct {
	conn      *websocket.Conn
	rawConn   net.Conn
	transport *http.Transport
	maxSend   int
}

// Dial's context bounds the handshake. Read, Write and Ping each accept their
// own contexts; callers keep a reader running to service control frames.
func Dial(ctx context.Context, endpoint string, opts Options) (*Client, *Handshake, error) {
	started := time.Now()
	info := &Handshake{Headers: make(http.Header)}
	fail := func(err error) (*Client, *Handshake, error) {
		info.Duration = time.Since(started)
		return nil, info, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Scheme != "ws" && u.Scheme != "wss" {
		return fail(errors.New("WebSocket URL must use ws or wss with a host and no credentials or fragment"))
	}
	if opts.ConnectTimeout < 0 || opts.MaxSendBytes < 0 || opts.MaxSendBytes > MaxMessageBytes || opts.MaxReceiveBytes < 0 || opts.MaxReceiveBytes > MaxMessageBytes {
		return fail(errors.New("invalid WebSocket timeout or message limit"))
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.MaxSendBytes == 0 {
		opts.MaxSendBytes = DefaultMessageBytes
	}
	if opts.MaxReceiveBytes == 0 {
		opts.MaxReceiveBytes = DefaultMessageBytes
	}
	headers := opts.Header.Clone()
	for name, values := range headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return fail(errors.New("invalid WebSocket header name"))
		}
		switch strings.ToLower(name) {
		case "connection", "upgrade", "host", "sec-websocket-key", "sec-websocket-version", "sec-websocket-protocol", "sec-websocket-extensions", "sec-websocket-accept", "proxy-authorization":
			return fail(errors.New("WebSocket handshake header is managed by the transport"))
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return fail(errors.New("invalid WebSocket header value"))
			}
		}
	}
	seenProtocols := make(map[string]bool)
	for _, protocol := range opts.Subprotocols {
		if !httpguts.ValidHeaderFieldName(protocol) || seenProtocols[protocol] {
			return fail(errors.New("WebSocket subprotocols must be distinct HTTP tokens"))
		}
		seenProtocols[protocol] = true
	}
	proxy, err := proxyPolicy(opts.ProxyMode, opts.ProxyURL)
	if err != nil {
		return fail(err)
	}
	profile := &tls.Config{}
	if opts.TLSConfig != nil {
		profile = opts.TLSConfig.Clone()
	}
	if profile.MinVersion < tls.VersionTLS12 {
		profile.MinVersion = tls.VersionTLS12
	}
	if profile.MaxVersion != 0 && profile.MaxVersion < profile.MinVersion {
		return fail(errors.New("TLS maximum version is below TLS 1.2"))
	}
	profile.NextProtos = []string{"http/1.1"}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	dialer := &net.Dialer{Timeout: opts.ConnectTimeout, KeepAlive: 30 * time.Second}
	var rawMu sync.Mutex
	var rawConn net.Conn
	dialRaw := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err == nil {
			rawMu.Lock()
			rawConn = conn
			rawMu.Unlock()
		}
		return conn, err
	}
	t := &http.Transport{
		Proxy: proxy, DialContext: dialRaw, TLSClientConfig: profile,
		TLSHandshakeTimeout: opts.ConnectTimeout, ResponseHeaderTimeout: opts.ConnectTimeout,
		Protocols: protocols, DisableCompression: true, DisableKeepAlives: true,
		MaxResponseHeaderBytes: 128 << 10,
	}
	// An HTTPS proxy has a separate TLS identity. Never offer the origin's mTLS
	// certificate or use its explicit SNI on that outer connection.
	if proxy != nil {
		t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			cfg := profile.Clone()
			if outer, _ := ctx.Value(proxyTLSKey{}).(bool); outer {
				cfg.ServerName = ""
				cfg.Certificates = nil
				cfg.GetClientCertificate = nil
			}
			if cfg.ServerName == "" {
				cfg.ServerName, _, _ = net.SplitHostPort(address)
			}
			conn, err := dialRaw(ctx, network, address)
			if err != nil {
				return nil, err
			}
			secured := tls.Client(conn, cfg)
			if err := secured.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return secured, nil
		}
	}
	httpClient := &http.Client{Transport: &proxyTransport{base: t, proxy: proxy}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var traceMu sync.Mutex
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			traceMu.Lock()
			info.ConnectDuration = time.Since(started)
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if err == nil {
				traceMu.Lock()
				info.TLS = &state
				traceMu.Unlock()
			}
		},
	}
	dialctx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), opts.ConnectTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(dialctx, endpoint, &websocket.DialOptions{
		HTTPClient: httpClient, HTTPHeader: headers, Subprotocols: append([]string(nil), opts.Subprotocols...),
		CompressionMode: websocket.CompressionDisabled,
		OnPingReceived: func(_ context.Context, data []byte) bool {
			if opts.OnPing != nil {
				opts.OnPing(append([]byte(nil), data...))
			}
			return true
		},
		OnPongReceived: func(_ context.Context, data []byte) {
			if opts.OnPong != nil {
				opts.OnPong(append([]byte(nil), data...))
			}
		},
	})
	traceMu.Lock()
	info.Duration = time.Since(started)
	if response != nil {
		info.StatusCode = response.StatusCode
		info.Headers = response.Header.Clone()
		info.Subprotocol = response.Header.Get("Sec-WebSocket-Protocol")
		if response.TLS != nil {
			state := *response.TLS
			info.TLS = &state
		}
		if err != nil && response.Body != nil {
			_ = response.Body.Close()
		}
	}
	traceMu.Unlock()
	if err != nil {
		t.CloseIdleConnections()
		return nil, info, err
	}
	conn.SetReadLimit(int64(opts.MaxReceiveBytes))
	rawMu.Lock()
	physical := rawConn
	rawMu.Unlock()
	return &Client{conn: conn, rawConn: physical, transport: t, maxSend: opts.MaxSendBytes}, info, nil
}

func (c *Client) Read(ctx context.Context) (Message, error) {
	kind, data, err := c.conn.Read(ctx)
	if err != nil {
		var closed websocket.CloseError
		if errors.As(err, &closed) && !utf8.ValidString(closed.Reason) {
			return Message{}, protocolError{code: 1007, reason: "invalid UTF-8 close reason"}
		}
		return Message{}, err
	}
	message := Message{Type: "binary", Data: data}
	if kind == websocket.MessageText {
		if !utf8.Valid(data) {
			// The library validates framing but deliberately leaves text UTF-8 to
			// applications. An invalid message must never reach the bridge.
			_ = c.Close(int(websocket.StatusInvalidFramePayloadData), "invalid UTF-8 text")
			return Message{}, protocolError{code: 1007, reason: "invalid UTF-8 text"}
		}
		message.Type = "text"
	}
	return message, nil
}

func (c *Client) Write(ctx context.Context, kind string, data []byte) error {
	if len(data) > c.maxSend {
		return errors.New("WebSocket message exceeds the send limit")
	}
	var typ websocket.MessageType
	switch kind {
	case "text":
		if !utf8.Valid(data) {
			return errors.New("WebSocket text must be valid UTF-8")
		}
		typ = websocket.MessageText
	case "binary":
		typ = websocket.MessageBinary
	default:
		return errors.New("WebSocket message type must be text or binary")
	}
	// coder serializes data writers while allowing the independent reader and
	// control writer to continue. Cancellation also bounds a queued writer.
	return c.conn.Write(ctx, typ, data)
}

// Ping waits for the matching automatically generated pong. Read must be active.
func (c *Client) Ping(ctx context.Context) error { return c.conn.Ping(ctx) }

func ValidateClose(code int, reason string) error {
	valid := code >= 3000 && code <= 4999
	switch code {
	case 1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014:
		valid = true
	}
	if !valid {
		return errors.New("invalid WebSocket close code")
	}
	if len(reason) > 123 || !utf8.ValidString(reason) {
		return errors.New("WebSocket close reason must be UTF-8 and at most 123 bytes")
	}
	return nil
}

// Close attempts the close handshake for at most one second, then aborts the
// connection. It can run concurrently with Read, Write, Ping, or CloseNow.
func (c *Client) Close(code int, reason string) error {
	if err := ValidateClose(code, reason); err != nil {
		return err
	}
	timer := time.AfterFunc(closeTimeout, func() { _ = c.rawConn.Close() })
	defer timer.Stop()
	err := c.conn.Close(websocket.StatusCode(code), reason)
	c.transport.CloseIdleConnections()
	return err
}

func (c *Client) CloseNow() error {
	// coder's CloseNow waits if Close has already started. Close the underlying
	// socket first so an in-progress graceful handshake can be interrupted.
	_ = c.rawConn.Close()
	err := c.conn.CloseNow()
	c.transport.CloseIdleConnections()
	return err
}
func (c *Client) Abort() error { return c.CloseNow() }

type protocolError struct {
	code   int
	reason string
}

func (e protocolError) Error() string { return e.reason }

// IsPeerClose distinguishes an actual valid close frame from local protocol
// validation failures. A peer close may itself carry a non-success status.
func IsPeerClose(err error) bool {
	var closeErr websocket.CloseError
	return errors.As(err, &closeErr) && utf8.ValidString(closeErr.Reason)
}

// CloseInfo returns a peer or local protocol close code. Use IsPeerClose to
// distinguish these. Abrupt TCP closure has no code and returns ok=false.
func CloseInfo(err error) (code int, reason string, ok bool) {
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return int(websocket.StatusMessageTooBig), "message exceeds the receive limit", true
	}
	var protocol protocolError
	if errors.As(err, &protocol) {
		return protocol.code, protocol.reason, true
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return int(closeErr.Code), closeErr.Reason, true
	}
	return 0, "", false
}

type proxyTLSKey struct{}
type proxyTransport struct {
	base  *http.Transport
	proxy func(*http.Request) (*url.URL, error)
}

func (t *proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.proxy != nil {
		proxyURL, err := t.proxy(req)
		if err != nil {
			return nil, err
		}
		if proxyURL != nil && proxyURL.Scheme == "https" {
			req = req.Clone(context.WithValue(req.Context(), proxyTLSKey{}, true))
		}
	}
	response, err := t.base.RoundTrip(req)
	if err == nil && (response.ProtoMajor != 1 || response.ProtoMinor != 1) {
		_ = response.Body.Close()
		return nil, errors.New("WebSocket requires HTTP/1.1 Upgrade")
	}
	return response, err
}

func proxyPolicy(mode, value string) (func(*http.Request) (*url.URL, error), error) {
	switch mode {
	case "", "none":
		return nil, nil
	case "custom":
		proxy, err := parseProxy(value, false)
		if err != nil {
			return nil, err
		}
		return http.ProxyURL(proxy), nil
	case "environment":
		environment := httpproxy.FromEnvironment()
		for _, configured := range []string{environment.HTTPProxy, environment.HTTPSProxy} {
			if configured != "" {
				if _, err := parseProxy(configured, true); err != nil {
					return nil, errors.New("environment contains an invalid or unsupported proxy URL")
				}
			}
		}
		proxy := environment.ProxyFunc()
		return func(req *http.Request) (*url.URL, error) { return proxy(req.URL) }, nil
	default:
		return nil, errors.New("proxy mode must be none, environment, or custom")
	}
}

func parseProxy(value string, implicitHTTP bool) (*url.URL, error) {
	if implicitHTTP && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme")
	}
	return u, nil
}
