package app

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
	"relay/internal/domain"
	"relay/internal/security"
	"relay/internal/transport/httpclient/engine"
)

const httpPreviewBudget int64 = 64 << 20

type httpRecord struct {
	result  domain.HTTPExecution
	cancel  context.CancelFunc
	spool   string
	reserve int64
}
type httpState struct {
	records  map[string]*httpRecord
	files    map[string]httpFileGrant
	jar      http.CookieJar
	reserved int64
	closed   bool
	work     sync.WaitGroup
}

// initHTTP is always called with httpMu held.
func (s *Service) initHTTP() *httpState {
	if s.httpState == nil {
		jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		s.httpState = &httpState{records: map[string]*httpRecord{}, files: map[string]httpFileGrant{}, jar: jar}
	}
	return s.httpState
}

func (s *Service) StartHTTPRequest(ctx context.Context, request domain.RequestDefinition) (domain.HTTPExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.HTTPExecution{}, err
	}
	// Do not load the workspace, open grants, or prompt the keyring for a bridge
	// call arriving after shutdown. The admission check below also covers a
	// shutdown that begins while request preparation is already in progress.
	s.httpMu.Lock()
	closed := s.initHTTP().closed
	s.httpMu.Unlock()
	if closed {
		return domain.HTTPExecution{}, errors.New("application is shutting down")
	}
	prepared, err := s.prepareHTTP(ctx, request)
	if err != nil {
		return domain.HTTPExecution{}, err
	}
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	executionCtx, cancel := context.WithTimeout(base, prepared.options.Timeout)
	s.httpMu.Lock()
	state := s.initHTTP()
	if state.closed {
		s.httpMu.Unlock()
		cancel()
		return domain.HTTPExecution{}, errors.New("application is shutting down")
	}
	prepared.options.Jar = state.jar
	s.httpMu.Unlock()
	client, err := engine.New(prepared.options)
	if err != nil {
		cancel()
		return domain.HTTPExecution{}, safeHTTPError(err)
	}
	id := newID()
	result := domain.HTTPExecution{ID: id, RequestID: request.ID, State: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), URL: safeHTTPURL(prepared.request.URL, prepared.secrets), Headers: []domain.Header{}, Trailers: []domain.Header{}, Warnings: slices.Clone(prepared.warnings), Redirects: []domain.HTTPRedirect{}}
	if prepared.options.TLSConfig.InsecureSkipVerify {
		result.Warnings = append(result.Warnings, "TLS certificate verification is disabled for this request")
	}
	if prepared.options.Version == "auto" || prepared.options.Version == "" {
		result.Warnings = append(result.Warnings, "Auto negotiates HTTP/1.1 or HTTP/2; select HTTP/3 explicitly")
	}
	record := &httpRecord{result: result, cancel: cancel}
	s.httpMu.Lock()
	if state.closed {
		s.httpMu.Unlock()
		cancel()
		client.Close()
		return domain.HTTPExecution{}, errors.New("application is shutting down")
	}
	active := 0
	for _, record := range state.records {
		if record.result.State == "running" {
			active++
		}
	}
	if active >= 8 {
		s.httpMu.Unlock()
		cancel()
		client.Close()
		return domain.HTTPExecution{}, errors.New("at most eight HTTP requests may run concurrently")
	}
	// Reserve the largest retained preview representation (base64). Temporary
	// encoding buffers are released when the worker exits.
	reserve := (prepared.preview*4 + 2) / 3
	for len(state.records) >= 16 || state.reserved+reserve > httpPreviewBudget {
		var victimID string
		var victim *httpRecord
		for id, record := range state.records {
			if record.result.State != "running" && (victim == nil || record.result.StartedAt < victim.result.StartedAt) {
				victimID = id
				victim = record
			}
		}
		if victim == nil {
			s.httpMu.Unlock()
			cancel()
			client.Close()
			return domain.HTTPExecution{}, errors.New("response preview memory limit reached; reduce the preview limit or cancel a request")
		}
		delete(state.records, victimID)
		state.reserved -= victim.reserve
		if victim.spool != "" {
			_ = os.Remove(victim.spool)
		}
	}
	record.reserve = reserve
	state.records[id] = record
	state.reserved += reserve
	state.work.Add(1)
	s.httpMu.Unlock()
	go s.runHTTP(executionCtx, prepared, client, record)
	return result, nil
}

func (s *Service) GetHTTPExecution(ctx context.Context, id string) (domain.HTTPExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.HTTPExecution{}, err
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	record, ok := s.initHTTP().records[id]
	if !ok {
		return domain.HTTPExecution{}, errors.New("HTTP execution is no longer available")
	}
	return cloneHTTPExecution(record.result), nil
}
func (s *Service) CancelHTTPExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	record, ok := s.initHTTP().records[id]
	if !ok {
		return errors.New("HTTP execution is no longer available")
	}
	record.cancel()
	return nil
}
func (s *Service) ForgetHTTPExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	state := s.initHTTP()
	record, ok := state.records[id]
	if !ok {
		return nil
	}
	if record.result.State == "running" {
		return errors.New("cancel the request before clearing its response")
	}
	if record.spool != "" {
		if err := os.Remove(record.spool); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("temporary response could not be removed")
		}
	}
	state.reserved -= record.reserve
	delete(state.records, id)
	return nil
}
func cloneHTTPExecution(result domain.HTTPExecution) domain.HTTPExecution {
	result.Headers = slices.Clone(result.Headers)
	result.Trailers = slices.Clone(result.Trailers)
	result.Warnings = slices.Clone(result.Warnings)
	result.Redirects = slices.Clone(result.Redirects)
	if result.TLS != nil {
		value := *result.TLS
		value.DNSNames = slices.Clone(value.DNSNames)
		result.TLS = &value
	}
	return result
}

type httpTraceTiming struct {
	mu                              sync.Mutex
	start, dns, connect, tls, first time.Time
	timing                          domain.HTTPTiming
}

func (t *httpTraceTiming) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { t.mu.Lock(); t.dns = time.Now(); t.mu.Unlock() }, DNSDone: func(httptrace.DNSDoneInfo) {
			t.mu.Lock()
			if !t.dns.IsZero() {
				t.timing.DNSMS += durationMS(time.Since(t.dns))
			}
			t.mu.Unlock()
		},
		ConnectStart: func(string, string) { t.mu.Lock(); t.connect = time.Now(); t.mu.Unlock() }, ConnectDone: func(string, string, error) {
			t.mu.Lock()
			if !t.connect.IsZero() {
				t.timing.ConnectMS += durationMS(time.Since(t.connect))
			}
			t.mu.Unlock()
		},
		TLSHandshakeStart: func() { t.mu.Lock(); t.tls = time.Now(); t.mu.Unlock() }, TLSHandshakeDone: func(tls.ConnectionState, error) {
			t.mu.Lock()
			if !t.tls.IsZero() {
				t.timing.TLSMS += durationMS(time.Since(t.tls))
			}
			t.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			t.first = time.Now()
			t.timing.FirstByteMS = durationMS(time.Since(t.start))
			t.mu.Unlock()
		},
	}
}
func durationMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (s *Service) runHTTP(ctx context.Context, p *httpPrepared, client *engine.Client, record *httpRecord) {
	defer s.httpState.work.Done()
	defer record.cancel()
	defer client.Close()
	started := time.Now()
	timing := &httpTraceTiming{start: started}
	var preview []byte
	var received int64
	var executionErr error
	var response *http.Response
	var spool *os.File
	var spoolPath string
	defer func() {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if spool != nil {
			if err := spool.Close(); err != nil && executionErr == nil {
				executionErr = errors.New("response spool could not be closed")
			}
		}
		if response == nil && spoolPath != "" {
			_ = os.Remove(spoolPath)
			spoolPath = ""
		}
		timing.mu.Lock()
		times := timing.timing
		times.TotalMS = durationMS(time.Since(started))
		if !timing.first.IsZero() {
			times.DownloadMS = durationMS(time.Since(timing.first))
		}
		timing.mu.Unlock()
		cleaned := redactHTTP(string(preview), p.secrets, int(p.preview))
		redacted := cleaned != string(preview)
		binary := !httpTextBody([]byte(cleaned))
		if response != nil && httpDeclaredText(response.Header.Get("Content-Type")) {
			cleaned = boundedHTTPText(cleaned, int(p.preview))
			binary = false
		}
		s.httpMu.Lock()
		defer s.httpMu.Unlock()
		result := &record.result
		result.ReceivedBytes = received
		result.PreviewBytes = int64(len(preview))
		result.Truncated = received > int64(len(preview))
		result.Binary = binary
		if redacted {
			result.Warnings = append(result.Warnings, "Sensitive values were redacted from the preview; saving downloads the original response")
		}
		result.Timing = times
		if binary {
			result.BodyBase64 = base64.StdEncoding.EncodeToString([]byte(cleaned))
		} else {
			result.BodyText = cleaned
		}
		if executionErr != nil {
			result.State = "failed"
			result.Error = safeHTTPError(executionErr).Error()
			if errors.Is(ctx.Err(), context.Canceled) {
				result.State = "canceled"
				result.Error = "Request canceled"
			}
			if errors.Is(executionErr, errHTTPDownloadLimit) {
				result.Truncated = true
			}
			if received > 0 {
				result.Warnings = append(result.Warnings, "The downloaded response is partial")
			}
		} else {
			result.State = "completed"
		}
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		record.spool = spoolPath
		result.DownloadAvailable = spoolPath != "" && response != nil
		retained := int64(len(result.BodyText) + len(result.BodyBase64))
		s.httpState.reserved += retained - record.reserve
		record.reserve = retained
	}()
	spool, executionErr = os.CreateTemp("", "relay-response-*")
	if executionErr != nil {
		executionErr = errors.New("response spool could not be created")
		return
	}
	spoolPath = spool.Name()
	req := p.request.Clone(httptrace.WithClientTrace(ctx, timing.trace()))
	if p.body != nil {
		req.Body, executionErr = p.body(ctx)
		if executionErr != nil {
			return
		}
		defer req.Body.Close()
		req.GetBody = func() (io.ReadCloser, error) { return p.body(ctx) }
		// net/http uses a non-nil Body with length zero to mean an unknown
		// upload length. File grants carry an exact stat size, including zero.
		if req.ContentLength == 0 {
			_ = req.Body.Close()
			req.Body = http.NoBody
			req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		}
	}
	// The callback established in Options reads only bounded, sanitized metadata
	// from this execution context.
	ctx = context.WithValue(req.Context(), httpRedirectKey{}, func(resp *http.Response, next *url.URL) {
		s.httpMu.Lock()
		if len(record.result.Redirects) < 32 {
			record.result.Redirects = append(record.result.Redirects, domain.HTTPRedirect{URL: safeHTTPURL(next, p.secrets), StatusCode: resp.StatusCode})
		}
		s.httpMu.Unlock()
	})
	req = req.WithContext(ctx)
	response, executionErr = client.Do(req)
	if executionErr != nil {
		return
	}
	s.httpMu.Lock()
	record.result.StatusCode = response.StatusCode
	record.result.Status = http.StatusText(response.StatusCode)
	record.result.Protocol = response.Proto
	record.result.Headers = safeHTTPHeaders(response.Header, p.secrets)
	record.result.TLS = safeHTTPTLS(response.TLS, p.secrets)
	if response.Request != nil {
		record.result.URL = safeHTTPURL(response.Request.URL, p.secrets)
	}
	if response.Uncompressed {
		record.result.Warnings = append(record.result.Warnings, "Response preview and download are decompressed")
	}
	s.httpMu.Unlock()
	buffer := make([]byte, 32<<10)
	for {
		remaining := p.maxDownload - received
		if remaining < 0 {
			executionErr = errHTTPDownloadLimit
			break
		}
		chunk := buffer
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining+1]
		}
		n, readErr := response.Body.Read(chunk)
		if n > 0 {
			allowed := n
			if int64(allowed) > remaining {
				allowed = int(remaining)
			}
			if allowed > 0 {
				if _, executionErr = spool.Write(chunk[:allowed]); executionErr != nil {
					executionErr = errors.New("response spool write failed")
					break
				}
				want := int64(allowed)
				if room := p.preview - int64(len(preview)); want > room {
					want = room
				}
				if want > 0 {
					preview = append(preview, chunk[:int(want)]...)
				}
			}
			received += int64(n)
			s.httpMu.Lock()
			record.result.ReceivedBytes = received
			s.httpMu.Unlock()
			if int64(n) > remaining {
				executionErr = errHTTPDownloadLimit
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				executionErr = readErr
			}
			break
		}
		if err := ctx.Err(); err != nil {
			executionErr = err
			break
		}
	}
	s.httpMu.Lock()
	record.result.Trailers = safeHTTPHeaders(response.Trailer, p.secrets)
	s.httpMu.Unlock()
}

type httpRedirectKey struct{}

func httpRedirectHook(resp *http.Response, next *url.URL) {
	if resp != nil && resp.Request != nil {
		if hook, ok := resp.Request.Context().Value(httpRedirectKey{}).(func(*http.Response, *url.URL)); ok {
			hook(resp, next)
		}
	}
}

var errHTTPDownloadLimit = errors.New("response download limit exceeded")

func safeHTTPError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("Request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("Request deadline exceeded")
	}
	if errors.Is(err, errHTTPDownloadLimit) {
		return errHTTPDownloadLimit
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) {
		return errors.New("TLS certificate verification failed; check CA and server name")
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return errors.New("DNS lookup failed; check the server hostname and network")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return errors.New("HTTP connection or response deadline exceeded")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("Connection refused; check the server address and port")
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return errors.New("Connection reset by the peer before the response completed")
	}
	var tlsRecord tls.RecordHeaderError
	if errors.As(err, &tlsRecord) {
		return errors.New("TLS handshake failed; check whether the endpoint requires HTTP or HTTPS")
	}
	if errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, zlib.ErrHeader) || errors.Is(err, zlib.ErrChecksum) {
		return errors.New("Response decompression failed; disable decompression to inspect raw bytes")
	}
	// Matching only selects fixed diagnostic classes. Never return peer-provided
	// messages, URLs, certificate names, proxy credentials, or body contents.
	message := strings.ToLower(err.Error())
	classifications := []struct{ fragment, message string }{
		{"unsupported http version", "Unsupported HTTP version; choose auto, 1.0, 1.1, 2, h2c, or 3"},
		{"requires tls alpn h2", "Server did not negotiate HTTP/2 with ALPN; choose auto or enable HTTP/2 on the server"},
		{"incompatible with the forced http version", "Server protocol does not match the forced HTTP version; fallback is disabled"},
		{"incompatible alpn", "TLS negotiated a protocol incompatible with the selected HTTP version"},
		{"require an https url", "Forced HTTP/2 and HTTP/3 require HTTPS; choose h2c for cleartext HTTP/2"},
		{"h2c prior knowledge requires", "h2c requires an HTTP URL and a server with cleartext HTTP/2 prior knowledge"},
		{"does not support explicit prox", "Selected HTTP version does not support explicit proxies; use no proxy"},
		{"does not support the configured proxy", "Selected HTTP version does not support this proxy; use no proxy or configure NO_PROXY"},
		{"unable to select the configured proxy", "Proxy selection failed; check the proxy and environment settings"},
		{"proxy url must", "Invalid proxy URL; configure a scheme, host, and optional credentials"},
		{"proxy scheme must", "Unsupported proxy scheme; choose http, https, socks5, or socks5h"},
		{"proxy mode must", "Invalid proxy mode; choose none, environment, or custom"},
		{"redirect limit exceeded", "Redirect limit exceeded; check for a redirect loop or increase the limit"},
		{"cross-origin redirect blocked", "Cross-origin redirect blocked to protect the mTLS identity; send the destination separately with its own TLS profile"},
		{"content-encoding exceeds", "Response exceeds the limit of 8 compression layers; disable decompression to inspect raw bytes"},
		{"unsupported response content-encoding", "Unsupported response encoding; disable decompression to inspect raw bytes"},
		{"tls:", "TLS handshake failed; check server name, client certificate, and supported TLS versions"},
		{"response spool", "Temporary response storage failed; check available disk space and permissions"},
		{"selected file", "Selected upload file became unavailable or changed; select it again"},
	}
	for _, class := range classifications {
		if strings.Contains(message, class.fragment) {
			return errors.New(class.message)
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return errors.New("Server closed the connection before a complete response arrived")
	}
	return errors.New("HTTP request failed; check URL, protocol, TLS, proxy, and connection settings")
}
func redactHTTP(value string, secrets []string, limit int) string {
	return redactText(value, secrets, limit)
}
func safeHTTPURL(u *url.URL, secrets []string) string {
	if u == nil {
		return ""
	}
	safe := *u
	safe.User = nil
	safe.Fragment = ""
	if safe.RawQuery != "" {
		var parts []string
		for _, part := range strings.Split(safe.RawQuery, "&") {
			key, _, _ := strings.Cut(part, "=")
			parts = append(parts, key+"="+url.QueryEscape(security.RedactedValue))
		}
		safe.RawQuery = strings.Join(parts, "&")
	}
	return redactHTTP(safe.String(), secrets, 65536)
}
func sensitiveHTTPHeader(name string) bool {
	name = strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(name))
	return name == "authorization" || name == "proxyauthorization" || name == "cookie" || name == "setcookie" || strings.Contains(name, "token") || strings.Contains(name, "apikey") || strings.Contains(name, "secret") || strings.Contains(name, "password")
}
func safeHTTPHeaders(headers http.Header, secrets []string) []domain.Header {
	out := []domain.Header{}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	budget := 64 << 10
	for _, key := range keys {
		for _, value := range headers[key] {
			if sensitiveHTTPHeader(key) {
				value = security.RedactedValue
			} else {
				value = redactHTTP(value, secrets, 8192)
			}
			if strings.EqualFold(key, "Location") {
				if parsed, err := url.Parse(value); err == nil {
					value = safeHTTPURL(parsed, secrets)
				} else {
					value = "[invalid redirect URL]"
				}
			}
			budget -= len(key) + len(value)
			if budget < 0 {
				return out
			}
			out = append(out, domain.Header{Name: redactHTTP(key, secrets, 4096), Value: value, Enabled: true})
		}
	}
	return out
}
func safeHTTPTLS(state *tls.ConnectionState, secrets []string) *domain.HTTPConnectionTLS {
	if state == nil {
		return nil
	}
	result := &domain.HTTPConnectionTLS{Version: tls.VersionName(state.Version), ServerName: redactHTTP(state.ServerName, secrets, 1024), NegotiatedProtocol: state.NegotiatedProtocol, CipherSuite: tls.CipherSuiteName(state.CipherSuite), Verified: len(state.VerifiedChains) > 0, DNSNames: []string{}}
	if len(state.PeerCertificates) > 0 {
		cert := state.PeerCertificates[0]
		result.Subject = redactHTTP(cert.Subject.String(), secrets, 2048)
		result.Issuer = redactHTTP(cert.Issuer.String(), secrets, 2048)
		for _, name := range cert.DNSNames {
			if len(result.DNSNames) >= 64 {
				break
			}
			result.DNSNames = append(result.DNSNames, redactHTTP(name, secrets, 1024))
		}
		result.ExpiresAt = cert.NotAfter.UTC().Format(time.RFC3339)
	}
	return result
}
func httpTextBody(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for _, value := range body {
		if value < 0x20 && value != '\n' && value != '\r' && value != '\t' {
			return false
		}
	}
	return true
}

func httpDeclaredText(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json") || strings.Contains(mediaType, "xml") || mediaType == "application/javascript" || mediaType == "application/x-www-form-urlencoded"
}

func boundedHTTPText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}
