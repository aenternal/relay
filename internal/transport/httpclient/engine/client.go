// Package engine provides a streaming HTTP client with explicit wire-version,
// TLS, proxy, cookie, redirect, cancellation, and decompression policies.
package engine

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	quichttp3 "github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http/httpproxy"

	"relay/internal/transport/httpclient/http10"
)

type Options struct {
	Version               string
	TLSConfig             *tls.Config
	ProxyMode             string
	ProxyURL              string
	Timeout               time.Duration
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	FollowRedirects       bool
	MaxRedirects          int
	Decompress            bool
	Jar                   http.CookieJar
	OnRedirect            func(*http.Response, *url.URL)
	SensitiveHeaders      []string
	SensitiveQueryKeys    []string
}

// Client owns a transport and all live request contexts. Do returns as soon as
// response headers arrive. The caller must close Body and eventually Client.
type Client struct {
	client   *http.Client
	version  string
	timeout  time.Duration
	lifetime context.Context
	cancel   context.CancelFunc
	close    func() error
	once     sync.Once
	closeErr error
}

func New(opts Options) (*Client, error) {
	opts.SensitiveHeaders = append([]string(nil), opts.SensitiveHeaders...)
	opts.SensitiveQueryKeys = append([]string(nil), opts.SensitiveQueryKeys...)
	if opts.Version == "" {
		opts.Version = "auto"
	}
	switch opts.Version {
	case "auto", "1.0", "1.1", "2", "h2c", "3":
	default:
		return nil, errors.New("unsupported HTTP version")
	}
	if opts.Timeout < 0 || opts.ConnectTimeout < 0 || opts.TLSHandshakeTimeout < 0 || opts.ResponseHeaderTimeout < 0 || opts.MaxRedirects < 0 {
		return nil, errors.New("HTTP timeouts and redirect limit cannot be negative")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.TLSHandshakeTimeout == 0 {
		opts.TLSHandshakeTimeout = 10 * time.Second
	}
	if opts.ResponseHeaderTimeout == 0 {
		opts.ResponseHeaderTimeout = 30 * time.Second
	}
	if opts.MaxRedirects == 0 {
		opts.MaxRedirects = 10
	}
	profile := &tls.Config{}
	if opts.TLSConfig != nil {
		profile = opts.TLSConfig.Clone()
	}
	minimum := uint16(tls.VersionTLS12)
	if opts.Version == "3" {
		minimum = tls.VersionTLS13
	}
	if profile.MinVersion < minimum {
		profile.MinVersion = minimum
	}
	if profile.MaxVersion != 0 && profile.MaxVersion < profile.MinVersion {
		return nil, errors.New("TLS maximum version is below the required minimum")
	}
	proxy, err := proxyPolicy(opts)
	if err != nil {
		return nil, err
	}
	unsupportedProxy := opts.Version == "1.0" || opts.Version == "h2c" || opts.Version == "3"
	if unsupportedProxy && opts.ProxyMode == "custom" {
		return nil, fmt.Errorf("HTTP %s does not support explicit proxy configuration", opts.Version)
	}
	var transport http.RoundTripper
	closeTransport := func() error { return nil }
	switch opts.Version {
	case "1.0":
		profile.NextProtos = nil
		transport = &http10.Transport{
			TLSConfig: profile, ConnectTimeout: opts.ConnectTimeout,
			TLSHandshakeTimeout: opts.TLSHandshakeTimeout, ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		}
	case "3":
		profile.NextProtos = []string{quichttp3.NextProtoH3}
		handshakeTimeout := min(opts.ConnectTimeout, opts.TLSHandshakeTimeout)
		t := &quichttp3.Transport{
			TLSClientConfig: profile, DisableCompression: true, MaxResponseHeaderBytes: 1 << 20,
			QUICConfig: &quic.Config{HandshakeIdleTimeout: handshakeTimeout, MaxIdleTimeout: opts.Timeout},
		}
		transport = &headerTimeoutTransport{base: t, timeout: opts.ResponseHeaderTimeout}
		closeTransport = t.Close
	default:
		proxyProfile := profile.Clone()
		proxyProfile.NextProtos = []string{"http/1.1"}
		proxyProfile.ServerName = ""
		// The request's mTLS identity belongs to its origin, never the proxy.
		proxyProfile.Certificates = nil
		proxyProfile.GetClientCertificate = nil
		protocols := &http.Protocols{}
		switch opts.Version {
		case "auto":
			protocols.SetHTTP1(true)
			protocols.SetHTTP2(true)
			profile.NextProtos = []string{"h2", "http/1.1"}
		case "1.1":
			protocols.SetHTTP1(true)
			profile.NextProtos = []string{"http/1.1"}
		case "2":
			protocols.SetHTTP2(true)
			profile.NextProtos = []string{"h2"}
			originalVerify := profile.VerifyConnection
			profile.VerifyConnection = func(cs tls.ConnectionState) error {
				if originalVerify != nil {
					if err := originalVerify(cs); err != nil {
						return err
					}
				}
				if cs.NegotiatedProtocol != "h2" {
					return errors.New("forced HTTP/2 requires TLS ALPN h2; fallback is disabled")
				}
				return nil
			}
		case "h2c":
			protocols.SetUnencryptedHTTP2(true)
			profile.NextProtos = nil
		}
		t := &http.Transport{
			Proxy: proxy, DialContext: (&net.Dialer{Timeout: opts.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig: profile, TLSHandshakeTimeout: opts.TLSHandshakeTimeout,
			ResponseHeaderTimeout: opts.ResponseHeaderTimeout, DisableCompression: true,
			Protocols: protocols, MaxResponseHeaderBytes: 1 << 20,
			IdleConnTimeout: 90 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 4,
			ExpectContinueTimeout: time.Second,
		}
		if proxy != nil {
			// HTTPS proxies speak HTTP/1.1 CONNECT even when the target requires
			// HTTP/2. Isolate that outer handshake from target ALPN and mTLS.
			t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				cfg := profile.Clone()
				if viaHTTPSProxy, _ := ctx.Value(proxyTLSKey{}).(bool); viaHTTPSProxy {
					cfg = proxyProfile.Clone()
				}
				if cfg.ServerName == "" {
					cfg.ServerName, _, _ = net.SplitHostPort(addr)
				}
				conn, err := (&net.Dialer{Timeout: opts.ConnectTimeout}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				tlsConn := tls.Client(conn, cfg)
				handshakeCtx, cancel := context.WithTimeout(ctx, opts.TLSHandshakeTimeout)
				defer cancel()
				if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
					_ = conn.Close()
					return nil, err
				}
				return tlsConn, nil
			}
		}
		transport = t
		closeTransport = func() error { t.CloseIdleConnections(); return nil }
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{version: opts.Version, timeout: opts.Timeout, lifetime: lifetime, cancel: cancel, close: closeTransport}
	c.client = &http.Client{
		Transport: &policyTransport{base: transport, opts: opts, proxy: proxy, unsupportedProxy: unsupportedProxy},
		Jar:       opts.Jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			crossed := req.Context().Value(crossOriginKey{}) == true
			if len(via) > 0 {
				previous := via[len(via)-1]
				crossed = crossed || previous.Context().Value(crossOriginKey{}) == true || origin(previous.URL) != origin(req.URL)
			}
			if crossed {
				*req = *req.WithContext(context.WithValue(req.Context(), crossOriginKey{}, true))
				stripCredentials(req.Header, opts.SensitiveHeaders)
				stripSensitiveQuery(req.URL, opts.SensitiveQueryKeys)
			}
			if opts.OnRedirect != nil {
				opts.OnRedirect(req.Response, req.URL)
			}
			if !opts.FollowRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) > opts.MaxRedirects {
				return errors.New("HTTP redirect limit exceeded")
			}
			if crossed && (len(profile.Certificates) != 0 || profile.GetClientCertificate != nil) {
				return errors.New("cross-origin redirect blocked to protect the mTLS identity; send the destination as a separate request with its own TLS profile")
			}
			return nil
		},
	}
	return c, nil
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("HTTP request is required")
	}
	if c.lifetime.Err() != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("HTTP client is closed")
	}
	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	stop := context.AfterFunc(c.lifetime, cancel)
	cleanup := func() { stop(); cancel() }
	resp, err := c.client.Do(req.Clone(ctx))
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		cleanup()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		// net/http's url.Error includes the complete query string. Keep the
		// useful underlying error and unwrap semantics without echoing URLs.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("HTTP %s request: %w", c.version, err)
	}
	resp.Body = &managedBody{ReadCloser: resp.Body, cleanup: cleanup, ctx: ctx}
	return resp, nil
}

func (c *Client) Close() error {
	c.once.Do(func() { c.cancel(); c.closeErr = c.close() })
	return c.closeErr
}

type policyTransport struct {
	base             http.RoundTripper
	opts             Options
	proxy            func(*http.Request) (*url.URL, error)
	unsupportedProxy bool
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fail := func(message string) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New(message)
	}
	if req.URL == nil || req.URL.Host == "" || req.URL.Opaque != "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return fail("request requires an http or https URL with a host")
	}
	if req.URL.User != nil {
		return fail("URL credentials are unsupported; configure explicit authentication")
	}
	if (t.opts.Version == "2" || t.opts.Version == "3") && req.URL.Scheme != "https" {
		return fail("forced HTTP/2 and HTTP/3 require an https URL; fallback is disabled")
	}
	if t.opts.Version == "h2c" && req.URL.Scheme != "http" {
		return fail("h2c prior knowledge requires an http URL; fallback is disabled")
	}
	out := req.Clone(req.Context())
	if t.proxy != nil {
		p, err := t.proxy(req)
		if err != nil {
			return fail("unable to select the configured proxy")
		}
		if p != nil {
			if t.unsupportedProxy {
				return fail("selected HTTP version does not support the configured proxy; use no proxy or NO_PROXY")
			}
			if p.Scheme == "https" {
				ctx := context.WithValue(out.Context(), proxyTLSKey{}, true)
				out = out.WithContext(ctx)
			}
		}
	}
	if req.Context().Value(crossOriginKey{}) == true {
		// The cookie jar runs after CheckRedirect. Strip here as well so a
		// domain cookie cannot cross a scheme or port boundary either.
		stripCredentials(out.Header, t.opts.SensitiveHeaders)
	}
	if t.opts.Decompress && out.Header.Get("Accept-Encoding") == "" && out.Method != http.MethodHead && out.Header.Get("Range") == "" {
		out.Header.Set("Accept-Encoding", "gzip, deflate")
	}
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if t.opts.Version == "2" && resp.ProtoMajor != 2 || t.opts.Version == "3" && resp.ProtoMajor != 3 || t.opts.Version == "h2c" && resp.ProtoMajor != 2 || t.opts.Version == "1.1" && (resp.ProtoMajor != 1 || resp.ProtoMinor != 1) {
		_ = resp.Body.Close()
		return nil, errors.New("server returned a protocol incompatible with the forced HTTP version; fallback is disabled")
	}
	if t.opts.Decompress && out.Method != http.MethodHead && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		if err := decompressResponse(resp); err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
	}
	return resp, nil
}

func proxyPolicy(opts Options) (func(*http.Request) (*url.URL, error), error) {
	switch opts.ProxyMode {
	case "", "none":
		return nil, nil
	case "environment":
		environment := httpproxy.FromEnvironment()
		// httpproxy intentionally ignores malformed environment URLs. A
		// desktop client must not silently turn a selected proxy into direct
		// traffic, so validate both configured routes before using its matcher.
		for _, configured := range []string{environment.HTTPProxy, environment.HTTPSProxy} {
			if configured == "" {
				continue
			}
			if _, err := parseProxyURL(configured, true); err != nil {
				return nil, errors.New("environment contains an invalid or unsupported proxy URL")
			}
		}
		proxyFunc := environment.ProxyFunc()
		return func(req *http.Request) (*url.URL, error) { return proxyFunc(req.URL) }, nil
	case "custom":
		p, err := parseProxyURL(opts.ProxyURL, false)
		if err != nil {
			return nil, err
		}
		return http.ProxyURL(p), nil
	default:
		return nil, errors.New("proxy mode must be none, environment, or custom")
	}
}

func parseProxyURL(value string, implicitHTTP bool) (*url.URL, error) {
	if implicitHTTP && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	p, err := url.Parse(value)
	if err != nil || p.Hostname() == "" || p.Path != "" && p.Path != "/" || p.RawQuery != "" || p.Fragment != "" {
		return nil, errors.New("proxy URL must contain a valid scheme, host, and optional port or credentials")
	}
	switch p.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("proxy scheme must be http, https, socks5, or socks5h")
	}
	return p, nil
}

type crossOriginKey struct{}
type proxyTLSKey struct{}

// QUIC's transport has no independent response-header deadline. Keep this
// context alive after headers; cancelling it earlier would abort streaming.
type headerTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *headerTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	var mu sync.Mutex
	var timer *time.Timer
	finished := false
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		mu.Lock()
		defer mu.Unlock()
		if !finished && info.Err == nil && timer == nil {
			timer = time.AfterFunc(t.timeout, cancel)
		}
	}})
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	mu.Lock()
	finished = true
	stopped := timer == nil || timer.Stop()
	mu.Unlock()
	if err != nil || !stopped {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if !stopped {
			return nil, fmt.Errorf("HTTP/3 response headers: %w", context.DeadlineExceeded)
		}
		return nil, err
	}
	resp.Body = &managedBody{ReadCloser: resp.Body, cleanup: cancel}
	return resp, nil
}

func stripCredentials(header http.Header, sensitive []string) {
	for name := range header {
		switch strings.ToLower(name) {
		case "authorization", "cookie", "proxy-authorization":
			delete(header, name)
		case "referer":
			// The automatic Referer contains the previous URL's query, which
			// may hold API keys even when no credential headers were set.
			delete(header, name)
		}
		for _, protected := range sensitive {
			if strings.EqualFold(name, protected) {
				delete(header, name)
			}
		}
	}
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// Keep surviving query fields byte-for-byte, including duplicates and their
// order; url.Values.Encode would unnecessarily normalize these fields.
func stripSensitiveQuery(u *url.URL, protected []string) {
	if len(protected) == 0 || u.RawQuery == "" {
		return
	}
	fields := strings.Split(u.RawQuery, "&")
	kept := fields[:0]
	for _, field := range fields {
		key, _, _ := strings.Cut(field, "=")
		key, err := url.QueryUnescape(key)
		remove := false
		if err == nil {
			for _, candidate := range protected {
				remove = remove || key == candidate
			}
		}
		if !remove {
			kept = append(kept, field)
		}
	}
	u.RawQuery = strings.Join(kept, "&")
}

type managedBody struct {
	io.ReadCloser
	cleanup func()
	ctx     context.Context
	once    sync.Once
}

func (b *managedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if b.ctx != nil && b.ctx.Err() != nil {
			err = b.ctx.Err()
		}
		_ = b.Close()
	}
	return n, err
}

func (b *managedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cleanup)
	return err
}
