package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
	"relay/internal/domain"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/security"
	"relay/internal/transport/httpclient/engine"
)

type httpPrepared struct {
	request     *http.Request
	options     engine.Options
	body        func(context.Context) (io.ReadCloser, error)
	preview     int64
	maxDownload int64
	secrets     []string
	warnings    []string
}

type trackingHTTPStore struct {
	security.Store
	values []string
	cache  map[string]string
}

func (s *trackingHTTPStore) Get(ctx context.Context, key string) (string, error) {
	if v, ok := s.cache[key]; ok {
		return v, nil
	}
	value, err := s.Store.Get(ctx, key)
	if err == nil {
		s.values = append(s.values, value)
		s.cache[key] = value
	}
	return value, err
}

func (s *Service) prepareHTTP(ctx context.Context, request domain.RequestDefinition) (*httpPrepared, error) {
	if request.Protocol != "http" || request.HTTP == nil {
		return nil, errors.New("select an HTTP request")
	}
	h := *request.HTTP
	if len(h.Headers) > 1024 || len(h.Query) > 1024 || len(h.PathParams) > 1024 || len(h.Form) > 1024 || len(h.Multipart) > 1024 {
		return nil, errors.New("request has too many fields")
	}
	if len(h.URL) > 65536 || len(h.Method) > 256 || len(h.Body) > security.MaxTemplateBytes {
		return nil, errors.New("request definition exceeds its limit")
	}
	s.mu.Lock()
	state, err := s.repo.Load(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, errors.New("workspace could not be loaded")
	}
	var env domain.Environment
	found := false
	for _, workspace := range state.Workspaces {
		if workspace.ID == request.WorkspaceID {
			found = true
			for _, candidate := range state.Environments {
				if candidate.ID == workspace.ActiveEnvironmentID && candidate.WorkspaceID == workspace.ID {
					env = candidate
					break
				}
			}
			break
		}
	}
	if !found {
		return nil, errors.New("request workspace no longer exists")
	}
	store := &trackingHTTPStore{Store: s.vault, cache: map[string]string{}}
	resolve := func(template string) (string, error) {
		r, e := security.ResolveText(ctx, template, env, store)
		return r.Value, e
	}
	rawURL, err := resolve(h.URL)
	if err != nil {
		return nil, err
	}
	// Escape parameter values before parsing, so slash, question mark and percent
	// in a path parameter cannot turn into path/query syntax.
	for _, field := range h.PathParams {
		if !field.Enabled {
			continue
		}
		if field.Name == "" || strings.ContainsAny(field.Name, "/{}:?#\r\n") {
			return nil, errors.New("invalid path parameter name")
		}
		value, e := resolve(field.Value)
		if e != nil {
			return nil, e
		}
		rawURL = strings.ReplaceAll(rawURL, "{"+field.Name+"}", url.PathEscape(value))
		// Colon placeholders apply only to complete path segments.
		pathEnd := len(rawURL)
		if query := strings.IndexByte(rawURL, '?'); query >= 0 {
			pathEnd = query
		}
		segments := strings.Split(rawURL[:pathEnd], "/")
		for i, segment := range segments {
			if segment == ":"+field.Name {
				segments[i] = url.PathEscape(value)
			}
		}
		rawURL = strings.Join(segments, "/") + rawURL[pathEnd:]
	}
	if len(rawURL) > 65536 {
		return nil, errors.New("resolved URL exceeds 64 KiB")
	}
	if strings.ContainsAny(rawURL, "{}") {
		return nil, errors.New("unresolved path parameter")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || !httpguts.ValidHostHeader(u.Host) || u.Fragment != "" {
		return nil, errors.New("URL must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	if _, err = url.ParseQuery(u.RawQuery); err != nil {
		return nil, errors.New("invalid URL query encoding")
	}
	for _, field := range h.Query {
		if !field.Enabled {
			continue
		}
		name, e := resolve(field.Name)
		if e != nil {
			return nil, e
		}
		value, e := resolve(field.Value)
		if e != nil {
			return nil, e
		}
		appendHTTPQuery(u, name, value)
		if len(u.String()) > 65536 {
			return nil, errors.New("resolved URL exceeds 64 KiB")
		}
	}
	method := h.Method
	if method == "" {
		method = "GET"
	}
	if !httpguts.ValidHeaderFieldName(method) || strings.EqualFold(method, "CONNECT") {
		return nil, errors.New("invalid or unsupported HTTP method")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid HTTP URL")
	}
	headerBytes := len(rawURL) + len(method) + 64
	var headerSecrets []string
	hostSeen := false
	for _, field := range h.Headers {
		if !field.Enabled {
			continue
		}
		name, e := resolve(field.Name)
		if e != nil {
			return nil, e
		}
		value, e := resolve(field.Value)
		if e != nil {
			return nil, e
		}
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, errors.New("invalid HTTP header")
		}
		switch strings.ToLower(name) {
		case "transfer-encoding", "trailer", "upgrade", "expect", "connection":
			return nil, errors.New("request transport headers are managed automatically")
		case "host":
			if hostSeen || !httpguts.ValidHostHeader(value) {
				return nil, errors.New("invalid or repeated Host header")
			}
			hostSeen = true
			req.Host = value
		default:
			req.Header.Add(name, value)
		}
		if sensitiveHTTPHeader(name) {
			headerSecrets = append(headerSecrets, value)
			if strings.EqualFold(name, "Authorization") {
				if _, token, ok := strings.Cut(value, " "); ok {
					headerSecrets = append(headerSecrets, token)
				}
			}
		}
		headerBytes += len(name) + len(value) + 4
		if headerBytes > 64<<10 {
			return nil, errors.New("request header limit exceeded")
		}
	}
	p := &httpPrepared{request: req, preview: int64(state.Settings.ResponsePreviewBytes), maxDownload: h.MaxDownloadBytes}
	if p.preview == 0 {
		p.preview = domain.DefaultPreviewBytes
	}
	if p.preview < 1024 {
		return nil, errors.New("response preview must be at least 1 KiB")
	}
	if p.preview > 32<<20 {
		p.preview = 32 << 20
		p.warnings = append(p.warnings, "Response preview was limited to 32 MiB to bound application memory")
	}
	if p.maxDownload == 0 {
		p.maxDownload = 256 << 20
	}
	if p.maxDownload < 1 || p.maxDownload > 1<<30 {
		return nil, errors.New("download limit must be between 1 byte and 1 GiB")
	}
	var authSecrets []string
	var sensitiveHeaders []string
	var sensitiveQueryKeys []string
	auth := h.Auth
	if auth.Type == "inherit" {
		auth = inheritedHTTPAuth(state, request)
	}
	switch auth.Type {
	case "", "none":
	case "basic":
		username, e := resolve(auth.Username)
		if e != nil {
			return nil, e
		}
		password, e := resolve(auth.Password)
		if e != nil {
			return nil, e
		}
		req.SetBasicAuth(username, password)
		authSecrets = append(authSecrets, password, base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	case "bearer":
		token, e := resolve(auth.Token)
		if e != nil {
			return nil, e
		}
		if !httpguts.ValidHeaderFieldValue(token) {
			return nil, errors.New("invalid bearer token")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		authSecrets = append(authSecrets, token)
	case "apikey", "api-key":
		name, e := resolve(auth.KeyName)
		if e != nil {
			return nil, e
		}
		value, e := resolve(auth.KeyValue)
		if e != nil {
			return nil, e
		}
		authSecrets = append(authSecrets, value)
		switch auth.In {
		case "", "header":
			if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
				return nil, errors.New("invalid API key header")
			}
			req.Header.Set(name, value)
			sensitiveHeaders = append(sensitiveHeaders, name)
		case "query":
			appendHTTPQuery(req.URL, name, value)
			sensitiveQueryKeys = append(sensitiveQueryKeys, name)
		default:
			return nil, errors.New("API key location must be header or query")
		}
	default:
		return nil, errors.New("unsupported authentication type")
	}
	kind := h.BodyKind
	if kind == "" {
		if h.Body != "" {
			kind = "raw"
		} else {
			kind = "none"
		}
	}
	var body string
	switch kind {
	case "none":
	case "raw":
		body, err = resolve(h.Body)
	case "json", "json5":
		input := h.Body
		if kind == "json5" {
			var normalized []byte
			normalized, err = jsoncodec.Normalize(input, jsoncodec.JSON5Like)
			if err == nil {
				input = string(normalized)
			}
		}
		if err == nil {
			var result security.ResolvedText
			result, err = security.ResolveJSON(ctx, input, env, store)
			body = result.Value
		}
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	case "form":
		var parts []string
		formBytes := 0
		for _, field := range h.Form {
			if !field.Enabled {
				continue
			}
			name, e := resolve(field.Name)
			if e != nil {
				return nil, e
			}
			value, e := resolve(field.Value)
			if e != nil {
				return nil, e
			}
			encoded := url.QueryEscape(name) + "=" + url.QueryEscape(value)
			formBytes += len(encoded) + 1
			if formBytes > security.MaxTemplateBytes {
				return nil, errors.New("form body exceeds 4 MiB")
			}
			parts = append(parts, encoded)
		}
		body = strings.Join(parts, "&")
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	case "file", "binary":
		grant, e := s.httpFile(h.FileID)
		if e != nil {
			return nil, e
		}
		file, e := openHTTPFile(grant)
		if e != nil {
			return nil, e
		}
		file.Close()
		if grant.info.Size() > 1<<30 {
			return nil, errors.New("upload file exceeds 1 GiB")
		}
		req.ContentLength = grant.info.Size()
		p.body = func(context.Context) (io.ReadCloser, error) { return openHTTPFile(grant) }
	case "multipart":
		err = s.prepareHTTPMultipart(p, h.Multipart, resolve)
	default:
		return nil, errors.New("unsupported HTTP body type")
	}
	if err != nil {
		return nil, err
	}
	if p.body == nil && body != "" {
		if len(body) > security.MaxTemplateBytes {
			return nil, errors.New("request body exceeds 4 MiB")
		}
		req.ContentLength = int64(len(body))
		p.body = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	}
	if values, ok := req.Header["Content-Length"]; ok {
		if len(values) != 1 {
			return nil, errors.New("Content-Length must occur once")
		}
		size, e := strconv.ParseInt(values[0], 10, 64)
		if e != nil || size != req.ContentLength {
			return nil, errors.New("Content-Length does not match the request body")
		}
		req.Header.Del("Content-Length")
	}
	tlsConfig, err := s.prepareHTTPTLS(h.TLS, resolve)
	if err != nil {
		return nil, err
	}
	proxyURL, err := resolve(h.Proxy.URL)
	if err != nil {
		return nil, err
	}
	if proxyURL != "" {
		if proxy, e := url.Parse(proxyURL); e == nil && proxy.User != nil {
			authSecrets = append(authSecrets, proxy.User.Username())
			if password, ok := proxy.User.Password(); ok {
				authSecrets = append(authSecrets, password)
			}
		}
	}
	timeout := h.TimeoutMS
	if timeout == 0 {
		timeout = 30000
	}
	if timeout < 1 || timeout > 24*60*60*1000 {
		return nil, errors.New("request timeout is outside 1 ms..24 hours")
	}
	for _, value := range []int{h.ConnectTimeoutMS, h.TLSHandshakeTimeoutMS, h.ResponseHeaderTimeoutMS} {
		if value < 0 || value > 24*60*60*1000 {
			return nil, errors.New("connection timeout is outside 0..24 hours")
		}
	}
	p.options = engine.Options{Version: h.Version, TLSConfig: tlsConfig, ProxyMode: h.Proxy.Mode, ProxyURL: proxyURL, Timeout: time.Duration(timeout) * time.Millisecond, ConnectTimeout: time.Duration(h.ConnectTimeoutMS) * time.Millisecond, TLSHandshakeTimeout: time.Duration(h.TLSHandshakeTimeoutMS) * time.Millisecond, ResponseHeaderTimeout: time.Duration(h.ResponseHeaderTimeoutMS) * time.Millisecond, FollowRedirects: h.FollowRedirects, MaxRedirects: h.MaxRedirects, Decompress: h.Decompress}
	headerBytes = len(req.URL.String()) + len(req.Method) + len(req.Host) + 64
	for name, values := range req.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value) + 4
		}
	}
	if headerBytes > 64<<10 {
		return nil, errors.New("request URL and headers exceed 64 KiB")
	}
	p.secrets = append(append(store.values, headerSecrets...), authSecrets...)
	for name, values := range req.Header {
		marked := sensitiveHTTPHeader(name)
		for _, value := range values {
			for _, secret := range p.secrets {
				if secret != "" && strings.Contains(value, secret) {
					marked = true
				}
			}
		}
		if marked {
			sensitiveHeaders = append(sensitiveHeaders, name)
		}
	}
	for name, values := range req.URL.Query() {
		for _, value := range values {
			for _, secret := range p.secrets {
				if secret != "" && strings.Contains(value, secret) {
					sensitiveQueryKeys = append(sensitiveQueryKeys, name)
				}
			}
		}
	}
	p.options.SensitiveQueryKeys = sensitiveQueryKeys
	p.options.SensitiveHeaders = sensitiveHeaders
	p.options.OnRedirect = httpRedirectHook
	return p, nil
}

func appendHTTPQuery(u *url.URL, name, value string) {
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += url.QueryEscape(name) + "=" + url.QueryEscape(value)
}

func (s *Service) prepareHTTPTLS(options domain.HTTPTLS, resolve func(string) (string, error)) (*tls.Config, error) {
	serverName, err := resolve(options.ServerName)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(serverName, "\x00\r\n/: ") {
		return nil, errors.New("invalid TLS server name")
	}
	result := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, InsecureSkipVerify: options.Insecure} // #nosec G402 -- explicit per-request user option
	read := func(id string) ([]byte, error) {
		grant, e := s.httpFile(id)
		if e != nil {
			return nil, e
		}
		if grant.info.Size() > 1<<20 {
			return nil, errors.New("TLS PEM file exceeds 1 MiB")
		}
		f, e := openHTTPFile(grant)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if e != nil || len(b) > 1<<20 {
			return nil, errors.New("TLS PEM file could not be read")
		}
		return b, nil
	}
	if options.CAFileID != "" {
		pem, e := read(options.CAFileID)
		if e != nil {
			return nil, e
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file does not contain a valid PEM certificate")
		}
		result.RootCAs = pool
	}
	if (options.ClientCertFileID == "") != (options.ClientKeyFileID == "") {
		return nil, errors.New("select both a client certificate and its private key")
	}
	if options.ClientCertFileID != "" {
		cert, e := read(options.ClientCertFileID)
		if e != nil {
			return nil, e
		}
		key, e := read(options.ClientKeyFileID)
		if e != nil {
			return nil, e
		}
		pair, e := tls.X509KeyPair(cert, key)
		clear(key)
		if e != nil {
			return nil, errors.New("client certificate and private key could not be loaded")
		}
		result.Certificates = []tls.Certificate{pair}
	}
	return result, nil
}

type preparedHTTPPart struct {
	header textproto.MIMEHeader
	value  string
	file   *httpFileGrant
}
type httpCountWriter int64

func (w *httpCountWriter) Write(p []byte) (int, error) {
	*w += httpCountWriter(len(p))
	return len(p), nil
}

func (s *Service) prepareHTTPMultipart(p *httpPrepared, fields []domain.HTTPPart, resolve func(string) (string, error)) error {
	var parts []preparedHTTPPart
	textBytes := 0
	for _, field := range fields {
		if !field.Enabled {
			continue
		}
		name, err := resolve(field.Name)
		if err != nil {
			return err
		}
		if strings.ContainsAny(name, "\r\n\x00") {
			return errors.New("invalid multipart field name")
		}
		part := preparedHTTPPart{header: textproto.MIMEHeader{}}
		parameters := map[string]string{"name": name}
		if field.FileID != "" {
			grant, err := s.httpFile(field.FileID)
			if err != nil {
				return err
			}
			file, err := openHTTPFile(grant)
			if err != nil {
				return err
			}
			file.Close()
			if grant.info.Size() > 1<<30 {
				return errors.New("multipart upload exceeds 1 GiB")
			}
			part.file = &grant
			filename := field.FileName
			if filename == "" {
				filename = grant.info.Name()
			}
			if strings.ContainsAny(filename, "\r\n\x00") {
				return errors.New("invalid multipart filename")
			}
			parameters["filename"] = filename
			contentType := field.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			if !httpguts.ValidHeaderFieldValue(contentType) {
				return errors.New("invalid multipart content type")
			}
			part.header.Set("Content-Type", contentType)
		} else {
			part.value, err = resolve(field.Value)
			if err != nil {
				return err
			}
		}
		textBytes += len(name) + len(part.value) + len(field.FileName) + len(field.ContentType) + 256
		if textBytes > security.MaxTemplateBytes {
			return errors.New("multipart text and headers exceed 4 MiB")
		}
		part.header.Set("Content-Disposition", mime.FormatMediaType("form-data", parameters))
		parts = append(parts, part)
	}
	var count httpCountWriter
	writer := multipart.NewWriter(&count)
	boundary := writer.Boundary()
	for _, part := range parts {
		if _, err := writer.CreatePart(part.header); err != nil {
			return errors.New("multipart headers could not be encoded")
		}
		if part.file != nil {
			count += httpCountWriter(part.file.info.Size())
		} else {
			count += httpCountWriter(len(part.value))
		}
	}
	if err := writer.Close(); err != nil {
		return errors.New("multipart request could not be encoded")
	}
	if count > 1<<30 {
		return errors.New("multipart upload exceeds 1 GiB")
	}
	p.request.ContentLength = int64(count)
	p.request.Header.Set("Content-Type", writer.FormDataContentType())
	p.body = func(ctx context.Context) (io.ReadCloser, error) {
		reader, pipe := io.Pipe()
		go func() {
			writer := multipart.NewWriter(pipe)
			_ = writer.SetBoundary(boundary)
			var err error
			defer func() {
				if err == nil {
					err = writer.Close()
				}
				_ = pipe.CloseWithError(err)
			}()
			for _, part := range parts {
				if err = ctx.Err(); err != nil {
					return
				}
				var destination io.Writer
				destination, err = writer.CreatePart(part.header)
				if err != nil {
					return
				}
				if part.file != nil {
					var file *os.File
					file, err = openHTTPFile(*part.file)
					if err != nil {
						return
					}
					_, err = io.Copy(destination, httpContextReader{ctx: ctx, reader: file})
					file.Close()
				} else {
					_, err = io.Copy(destination, bytes.NewBufferString(part.value))
				}
				if err != nil {
					return
				}
			}
		}()
		return reader, nil
	}
	return nil
}

// A nearest-folder override wins; inherit walks toward the collection. Saved
// hierarchy ownership is checked again because execution also accepts drafts.
func inheritedHTTPAuth(state domain.State, request domain.RequestDefinition) domain.HTTPAuth {
	current := request.FolderID
	visited := map[string]bool{}
	for current != "" && !visited[current] {
		visited[current] = true
		var found *domain.Folder
		for i := range state.Folders {
			folder := &state.Folders[i]
			if folder.ID == current && folder.WorkspaceID == request.WorkspaceID && folder.CollectionID == request.CollectionID {
				found = folder
				break
			}
		}
		if found == nil {
			break
		}
		if found.Auth != nil && found.Auth.Type != "inherit" {
			return *found.Auth
		}
		current = found.ParentID
	}
	for _, collection := range state.Collections {
		if collection.ID == request.CollectionID && collection.WorkspaceID == request.WorkspaceID && collection.Auth != nil && collection.Auth.Type != "inherit" {
			return *collection.Auth
		}
	}
	return domain.HTTPAuth{Type: "none"}
}
