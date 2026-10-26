// Package httpclient contains the bounded request/response policy shared by
// protocol-specific clients. It has no desktop-shell dependencies.
package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Config applies to one isolated TLS and resource-limit profile. Zero fields
// select secure defaults: TLS verification, a 30s deadline, 5MiB bodies and
// 64KiB headers. TLSConfig is cloned; do not mutate its referenced certificates
// or CA pools while creating or using a client.
type Config struct {
	TLSConfig              *tls.Config
	Timeout                time.Duration
	MaxRequestBodyBytes    int64
	MaxResponseBodyBytes   int64
	MaxRequestHeaderBytes  int
	MaxResponseHeaderBytes int
}

func Normalize(c Config, minTLS uint16) (Config, error) {
	if c.Timeout < 0 || c.MaxRequestBodyBytes < 0 || c.MaxResponseBodyBytes < 0 || c.MaxRequestHeaderBytes < 0 || c.MaxResponseHeaderBytes < 0 {
		return Config{}, errors.New("HTTP client: timeout and resource limits must be positive")
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRequestBodyBytes == 0 {
		c.MaxRequestBodyBytes = 5 << 20
	}
	if c.MaxResponseBodyBytes == 0 {
		c.MaxResponseBodyBytes = 5 << 20
	}
	// Reserve one extra byte for detecting a response exceeding its limit.
	if c.MaxRequestBodyBytes == int64(^uint64(0)>>1) || c.MaxResponseBodyBytes == int64(^uint64(0)>>1) {
		return Config{}, errors.New("HTTP client: body limit is too large")
	}
	if c.MaxRequestHeaderBytes == 0 {
		c.MaxRequestHeaderBytes = 64 << 10
	}
	if c.MaxResponseHeaderBytes == 0 {
		c.MaxResponseHeaderBytes = 64 << 10
	}
	if c.TLSConfig == nil {
		c.TLSConfig = &tls.Config{}
	} else {
		c.TLSConfig = c.TLSConfig.Clone()
	}
	if c.TLSConfig.MinVersion < minTLS {
		c.TLSConfig.MinVersion = minTLS
	}
	if c.TLSConfig.MaxVersion != 0 && c.TLSConfig.MaxVersion < c.TLSConfig.MinVersion {
		return Config{}, errors.New("HTTP client: TLS maximum version is below required minimum")
	}
	return c, nil
}

// Prepare validates a request and buffers at most the configured number of
// bytes. This lets HTTP/1.0 send Content-Length even for unknown-length bodies,
// and prevents HTTP/3 uploads from bypassing the same resource policy.
// The caller owns closing the original body, including on validation errors.
func Prepare(ctx context.Context, req *http.Request, c Config) (*http.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || req.URL == nil {
		return nil, errors.New("request URL is required")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, errors.New("request URL must use http or https")
	}
	if req.URL.Host == "" || !httpguts.ValidHostHeader(req.URL.Host) || req.URL.Opaque != "" {
		return nil, errors.New("request URL has an invalid host or opaque path")
	}
	if req.URL.User != nil {
		return nil, errors.New("URL credentials are unsupported; use an explicit Authorization header")
	}
	method := req.Method
	if method == "" {
		method = "GET"
	}
	if !httpguts.ValidHeaderFieldName(method) {
		return nil, errors.New("request method must be a valid HTTP token")
	}
	if method == "CONNECT" {
		return nil, errors.New("CONNECT tunneling is not supported by this bounded HTTP client")
	}
	if req.Host != "" && !httpguts.ValidHostHeader(req.Host) {
		return nil, errors.New("request Host is invalid")
	}
	if req.ContentLength < -1 {
		return nil, errors.New("request Content-Length cannot be below -1")
	}
	if len(req.TransferEncoding) != 0 || len(req.Trailer) != 0 {
		return nil, errors.New("request transfer encodings and trailers are not supported")
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headerBytes := len(method) + len(req.URL.RequestURI()) + len(host) + 64
	if strings.ContainsAny(req.URL.RequestURI(), " \r\n\t") {
		return nil, errors.New("request URL contains invalid path characters")
	}
	for name, values := range req.Header {
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, errors.New("request header name is invalid")
		}
		if strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Trailer") || strings.EqualFold(name, "Upgrade") || strings.EqualFold(name, "Expect") {
			return nil, fmt.Errorf("request transfer/upgrade header %s is unsupported", http.CanonicalHeaderKey(name))
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, fmt.Errorf("request header %s contains invalid characters", http.CanonicalHeaderKey(name))
			}
			headerBytes += len(name) + len(value) + 4
			if headerBytes > c.MaxRequestHeaderBytes {
				return nil, errors.New("request header limit exceeded")
			}
		}
	}
	if headerBytes > c.MaxRequestHeaderBytes {
		return nil, errors.New("request header limit exceeded")
	}
	if req.ContentLength > c.MaxRequestBodyBytes {
		return nil, errors.New("request body limit exceeded")
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		// Request bodies follow the net/http contract: Close must unblock Read.
		stop := context.AfterFunc(ctx, func() { _ = req.Body.Close() })
		defer stop()
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, c.MaxRequestBodyBytes+1))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		if int64(len(body)) > c.MaxRequestBodyBytes {
			return nil, errors.New("request body limit exceeded")
		}
	}
	if req.ContentLength > 0 && req.ContentLength != int64(len(body)) {
		return nil, errors.New("request Content-Length does not match body size")
	}
	for name, values := range req.Header {
		if !strings.EqualFold(name, "Content-Length") {
			continue
		}
		if len(values) != 1 {
			return nil, errors.New("request Content-Length must occur once")
		}
		length, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || length < 0 || length != int64(len(body)) {
			return nil, errors.New("request Content-Length header does not match body size")
		}
	}
	out := req.Clone(ctx)
	out.Method = method
	out.RequestURI = ""
	out.ContentLength = int64(len(body))
	out.Header = make(http.Header, len(req.Header))
	for name, values := range req.Header {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Host") || strings.EqualFold(name, "Connection") {
			continue
		}
		for _, value := range values {
			out.Header.Add(name, value)
		}
	}
	if len(body) == 0 {
		out.Body = http.NoBody
	} else {
		out.Body = io.NopCloser(bytes.NewReader(body))
	}
	return out, nil
}

// BufferResponse consumes and closes the network body before returning, so a
// response cannot outlive its timeout or retain a connection. Exceeding the
// preview limit is explicit; callers must not mistake a truncated body for EOF.
func BufferResponse(ctx context.Context, resp *http.Response, limit int64) (*http.Response, error) {
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, errors.New("response body limit exceeded")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, errors.New("response body limit exceeded")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}
