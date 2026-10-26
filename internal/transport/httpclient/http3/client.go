// Package http3 implements forced HTTP/3 over QUIC, without a TCP fallback.
package http3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"github.com/quic-go/quic-go"
	quichttp3 "github.com/quic-go/quic-go/http3"

	"relay/internal/transport/httpclient"
)

type Config = httpclient.Config

// Client buffers bounded responses and closes their QUIC streams before Do
// returns. It does not follow redirects. Always call Close to free UDP sockets.
type Client struct {
	config    Config
	transport *quichttp3.Transport
	lifetime  context.Context
	cancel    context.CancelFunc
}

func New(config Config) (*Client, error) {
	config, err := httpclient.Normalize(config, tls.VersionTLS13)
	if err != nil {
		return nil, err
	}
	config.TLSConfig.NextProtos = []string{quichttp3.NextProtoH3}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Client{config: config, lifetime: lifetime, cancel: cancel, transport: &quichttp3.Transport{
		TLSClientConfig:        config.TLSConfig,
		QUICConfig:             &quic.Config{HandshakeIdleTimeout: config.Timeout, MaxIdleTimeout: config.Timeout},
		MaxResponseHeaderBytes: config.MaxResponseHeaderBytes,
		DisableCompression:     true,
	}}, nil
}

func (c *Client) Close() error { c.cancel(); return c.transport.Close() }

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("HTTP/3: request is required")
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	ctx, cancel := context.WithTimeout(req.Context(), c.config.Timeout)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	if c.lifetime.Err() != nil {
		return nil, errors.New("HTTP/3 client is closed")
	}
	if req.URL == nil || req.URL.Scheme != "https" {
		return nil, errors.New("HTTP/3 requires an https URL and TLS 1.3")
	}
	prepared, err := httpclient.Prepare(ctx, req, c.config)
	if err != nil {
		return nil, fmt.Errorf("HTTP/3 prepare request: %w", err)
	}
	resp, err := c.transport.RoundTrip(prepared)
	if ctx.Err() != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("HTTP/3 QUIC request (UDP/TLS/ALPN; no fallback): %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("HTTP/3 QUIC request (UDP/TLS/ALPN; no fallback): %w", err)
	}
	if resp.ProtoMajor != 3 {
		_ = resp.Body.Close()
		return nil, errors.New("HTTP/3 QUIC request returned an unexpected protocol")
	}
	resp, err = httpclient.BufferResponse(ctx, resp, c.config.MaxResponseBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("HTTP/3 QUIC response: %w", err)
	}
	return resp, nil
}
