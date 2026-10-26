package app

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
	"relay/internal/domain"
	"relay/internal/security"
	"relay/internal/transport/websocketclient"
)

type webSocketPrepared struct {
	url                       string
	options                   websocketclient.Options
	env                       domain.Environment
	store                     *trackingHTTPStore
	secrets                   []string
	writeTimeout, readTimeout time.Duration
	maxMessage                int
}

func webSocketLimits(definition domain.WebSocketDefinition) (connect, write, read time.Duration, limit int, err error) {
	for _, value := range []int{definition.ConnectTimeoutMS, definition.WriteTimeoutMS, definition.ReadTimeoutMS} {
		if value < 0 || value > 24*60*60*1000 {
			return 0, 0, 0, 0, errors.New("WebSocket timeouts must be between 0 and 24 hours")
		}
	}
	if definition.MaxMessageBytes < 0 || definition.MaxMessageBytes > 4<<20 {
		return 0, 0, 0, 0, errors.New("WebSocket message limit must be between 1 byte and 4 MiB")
	}
	connect, write, read = time.Duration(definition.ConnectTimeoutMS)*time.Millisecond, time.Duration(definition.WriteTimeoutMS)*time.Millisecond, time.Duration(definition.ReadTimeoutMS)*time.Millisecond
	if connect == 0 {
		connect = 10 * time.Second
	}
	if write == 0 {
		write = 10 * time.Second
	}
	limit = definition.MaxMessageBytes
	if limit == 0 {
		limit = 1 << 20
	}
	return
}

func (s *Service) prepareWebSocket(ctx context.Context, request domain.RequestDefinition) (p *webSocketPrepared, err error) {
	p = &webSocketPrepared{store: &trackingHTTPStore{Store: &webSocketSecretStore{Store: s.vault}, cache: map[string]string{}}}
	defer p.collectSecrets()
	definition := request.WebSocket
	connect, write, read, limit, err := webSocketLimits(*definition)
	if err != nil {
		return p, err
	}
	p.writeTimeout, p.readTimeout, p.maxMessage = write, read, limit
	state, err := s.LoadState(ctx)
	if err != nil {
		return p, errors.New("workspace could not be loaded")
	}
	found := false
	for _, workspace := range state.Workspaces {
		if workspace.ID != request.WorkspaceID {
			continue
		}
		found = true
		for _, env := range state.Environments {
			if env.ID == workspace.ActiveEnvironmentID && env.WorkspaceID == workspace.ID {
				p.env = env
				p.env.Variables = slices.Clone(env.Variables)
				break
			}
		}
		break
	}
	if !found {
		return p, errors.New("request workspace no longer exists")
	}
	if len(p.env.Variables) > 1024 {
		return p, errors.New("WebSocket environment exceeds 1024 variables")
	}
	environmentBytes := 0
	for _, variable := range p.env.Variables {
		environmentBytes += len(variable.Name) + len(variable.Value) + len(variable.SecretRef) + 64
		if environmentBytes > 1<<20 {
			return p, errors.New("WebSocket environment snapshot exceeds 1 MiB")
		}
	}
	resolve := func(template string) (string, error) {
		result, e := security.ResolveText(ctx, template, p.env, p.store)
		return result.Value, e
	}
	p.url, err = resolve(definition.URL)
	if err != nil {
		return p, err
	}
	u, err := url.Parse(p.url)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || !httpguts.ValidHostHeader(u.Host) || strings.ContainsAny(p.url, "\x00\r\n") {
		return p, errors.New("URL must be an absolute ws:// or wss:// URL without credentials or a fragment")
	}
	if _, err = url.ParseQuery(u.RawQuery); err != nil {
		return p, errors.New("invalid WebSocket URL query encoding")
	}
	for _, field := range definition.Query {
		if !field.Enabled {
			continue
		}
		name, e := resolve(field.Name)
		if e != nil {
			return p, e
		}
		value, e := resolve(field.Value)
		if e != nil {
			return p, e
		}
		appendHTTPQuery(u, name, value)
		if sensitiveGRPCMetadata(name) {
			p.secrets = append(p.secrets, value)
		}
		if len(u.String()) > 65536 {
			return p, errors.New("WebSocket URL exceeds 64 KiB")
		}
	}
	for name, values := range u.Query() {
		if sensitiveGRPCMetadata(name) {
			p.secrets = append(p.secrets, values...)
		}
	}
	p.url = u.String()
	p.options = websocketclient.Options{Header: http.Header{}, ConnectTimeout: connect, MaxSendBytes: limit, MaxReceiveBytes: limit, ProxyMode: definition.Proxy.Mode}
	bytes := len(p.url)
	for _, field := range definition.Headers {
		if !field.Enabled {
			continue
		}
		name, e := resolve(field.Name)
		if e != nil {
			return p, e
		}
		value, e := resolve(field.Value)
		if e != nil {
			return p, e
		}
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return p, errors.New("invalid WebSocket handshake header")
		}
		switch strings.ToLower(name) {
		case "host", "connection", "upgrade", "content-length", "transfer-encoding", "trailer", "sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions", "sec-websocket-accept", "sec-websocket-protocol":
			return p, errors.New("WebSocket transport headers and subprotocols are managed automatically")
		}
		p.options.Header.Add(name, value)
		bytes += len(name) + len(value) + 4
		if sensitiveGRPCMetadata(name) {
			p.secrets = append(p.secrets, value)
			if _, token, ok := strings.Cut(value, " "); ok {
				p.secrets = append(p.secrets, token)
			}
		}
		if bytes > 64<<10 {
			return p, errors.New("WebSocket handshake URL and headers exceed 64 KiB")
		}
	}
	for _, protocol := range definition.Subprotocols {
		value, e := resolve(protocol)
		if e != nil {
			return p, e
		}
		if !httpguts.ValidHeaderFieldName(value) || len(value) > 256 {
			return p, errors.New("invalid WebSocket subprotocol")
		}
		p.options.Subprotocols = append(p.options.Subprotocols, value)
		bytes += len(value) + 2
	}
	// Cookie is a list of credentials, not one indivisible secret. A peer may
	// echo just a cookie value without its name or the other cookie pairs.
	cookieRequest := http.Request{Header: p.options.Header}
	for _, cookie := range cookieRequest.Cookies() {
		if cookie.Value != "" {
			p.secrets = append(p.secrets, cookie.Value)
		}
	}
	if bytes > 64<<10 {
		return p, errors.New("WebSocket handshake URL and headers exceed 64 KiB")
	}
	if u.Scheme == "wss" {
		p.options.TLSConfig, err = s.prepareHTTPTLS(definition.TLS, resolve)
		if err != nil {
			return p, err
		}
	}
	if definition.Proxy.Mode == "custom" {
		p.options.ProxyURL, err = resolve(definition.Proxy.URL)
		if err != nil {
			return p, err
		}
		if proxy, e := url.Parse(p.options.ProxyURL); e == nil && proxy.User != nil {
			p.secrets = append(p.secrets, proxy.User.Username())
			if password, ok := proxy.User.Password(); ok {
				p.secrets = append(p.secrets, password)
			}
		}
	}
	return p, nil
}

type webSocketSecretStore struct {
	security.Store
	bytes int
}

func (store *webSocketSecretStore) Get(ctx context.Context, key string) (string, error) {
	value, err := store.Store.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if len(value) > (256<<10)-store.bytes {
		return "", errors.New("WebSocket cached secret values exceed 256 KiB")
	}
	store.bytes += len(value)
	return value, nil
}

func (p *webSocketPrepared) collectSecrets() {
	known := map[string]bool{}
	for _, value := range append(slices.Clone(p.secrets), p.store.values...) {
		if value != "" {
			known[value] = true
		}
	}
	for value := range known {
		if decoded, e := hex.DecodeString(value); e == nil && len(decoded) > 0 {
			known[string(decoded)] = true
		}
		for _, enc := range []*base64.Encoding{base64.StdEncoding.Strict(), base64.RawStdEncoding.Strict(), base64.URLEncoding.Strict(), base64.RawURLEncoding.Strict()} {
			if decoded, e := enc.DecodeString(value); e == nil && len(decoded) > 0 {
				known[string(decoded)] = true
				break
			}
		}
	}
	p.secrets = p.secrets[:0]
	for value := range known {
		p.secrets = append(p.secrets, value)
	}
}

func (s *Service) webSocketSendData(ctx context.Context, p *webSocketPrepared, input domain.WebSocketSend) (kind string, data []byte, err error) {
	defer p.collectSecrets()
	kind = input.Kind
	if kind == "" {
		kind = "text"
	}
	if kind != "text" && kind != "binary" {
		return "", nil, errors.New("WebSocket message kind must be text or binary")
	}
	maxInput := security.MaxTemplateBytes
	if kind == "binary" {
		maxInput = 8 << 20
	}
	if len(input.Data) > maxInput {
		return "", nil, errors.New("WebSocket message input exceeds its encoded size limit")
	}
	if input.Encoding == "file" {
		if kind != "binary" || input.FileID == "" {
			return "", nil, errors.New("select a native file for a binary WebSocket message")
		}
		grant, e := s.httpFile(input.FileID)
		if e != nil {
			return "", nil, e
		}
		if grant.info.Size() > int64(p.maxMessage) {
			return "", nil, errors.New("WebSocket file exceeds the message limit")
		}
		file, e := openHTTPFile(grant)
		if e != nil {
			return "", nil, e
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, int64(p.maxMessage)+1))
		if err != nil {
			return "", nil, errors.New("WebSocket file could not be read")
		}
	} else {
		resolved := input.Data
		if kind == "text" || strings.Contains(input.Data, "{{") {
			result, e := security.ResolveText(ctx, input.Data, p.env, p.store)
			if e != nil {
				return "", nil, e
			}
			resolved = result.Value
		}
		encoding := input.Encoding
		if encoding == "" {
			encoding = "text"
		}
		switch encoding {
		case "text":
			data = []byte(resolved)
		case "hex":
			data, err = hex.DecodeString(strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(resolved))
		case "base64":
			data, err = base64.StdEncoding.Strict().DecodeString(resolved)
		default:
			return "", nil, errors.New("unsupported WebSocket message encoding")
		}
		if err != nil {
			return "", nil, errors.New("invalid WebSocket hex or base64 data")
		}
		if kind == "text" && encoding != "text" {
			return "", nil, errors.New("text WebSocket messages use text encoding")
		}
	}
	if len(data) > p.maxMessage {
		return "", nil, errors.New("WebSocket message exceeds the configured limit")
	}
	if kind == "text" && !utf8.Valid(data) {
		return "", nil, errors.New("text WebSocket messages must contain valid UTF-8")
	}
	return kind, data, nil
}
