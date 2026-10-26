package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"time"

	"relay/internal/domain"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/protobuf/registry"
	"relay/internal/security"
	"relay/internal/transport/grpcclient"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type grpcPrepared struct {
	endpoint string
	options  grpcclient.Options
	metadata metadata.MD
	registry *registry.Registry
	method   protoreflect.MethodDescriptor
	message  *dynamicpb.Message
	secrets  []string
	env      domain.Environment
	store    *trackingHTTPStore
}

func grpcLimits(definition domain.GRPCDefinition) (time.Duration, int, error) {
	timeout := definition.TimeoutMS
	if timeout == 0 {
		timeout = 30000
	}
	if timeout < 1 || timeout > 24*60*60*1000 || definition.ConnectTimeoutMS < 0 || definition.ConnectTimeoutMS > 24*60*60*1000 {
		return 0, 0, errors.New("gRPC deadlines must be between 1 ms and 24 hours")
	}
	for _, limit := range []int{definition.MaxSendBytes, definition.MaxReceiveBytes} {
		if limit < 0 || limit > 4<<20 {
			return 0, 0, errors.New("gRPC message limits must be between 1 byte and 4 MiB")
		}
	}
	receive := definition.MaxReceiveBytes
	if receive == 0 {
		receive = grpcclient.DefaultMaxMessageBytes
	}
	return time.Duration(timeout) * time.Millisecond, receive, nil
}

func (s *Service) prepareGRPC(ctx context.Context, request domain.RequestDefinition, reflection bool) (p *grpcPrepared, err error) {
	mode := "unary"
	if reflection {
		mode = "reflection"
	}
	return s.prepareGRPCMode(ctx, request, mode)
}

func (s *Service) prepareGRPCMode(ctx context.Context, request domain.RequestDefinition, mode string) (p *grpcPrepared, err error) {
	p = &grpcPrepared{metadata: metadata.MD{}}
	if request.Protocol != "grpc" || request.GRPC == nil {
		return p, errors.New("select a gRPC request")
	}
	definition := request.GRPC
	if len(definition.Metadata) > 1024 || len(definition.Endpoint) > 65536 || len(definition.Authority) > 1024 || len(definition.Method) > 1024 || len(definition.Body) > security.MaxTemplateBytes {
		return p, errors.New("gRPC request definition exceeds its size limit")
	}
	state, err := s.LoadState(ctx)
	if err != nil {
		return p, errors.New("workspace could not be loaded")
	}
	var env domain.Environment
	found := false
	for _, workspace := range state.Workspaces {
		if workspace.ID != request.WorkspaceID {
			continue
		}
		found = true
		for _, candidate := range state.Environments {
			if candidate.ID == workspace.ActiveEnvironmentID && candidate.WorkspaceID == workspace.ID {
				env = candidate
				break
			}
		}
		break
	}
	if !found {
		return p, errors.New("request workspace no longer exists")
	}
	p.env = env
	store := &trackingHTTPStore{Store: &grpcBoundedStore{Store: s.vault}, cache: map[string]string{}}
	p.store = store
	defer p.collectSecrets()
	resolve := func(template string) (string, error) {
		value, err := security.ResolveText(ctx, template, env, store)
		return value.Value, err
	}
	p.endpoint, err = resolve(definition.Endpoint)
	if err != nil {
		return p, err
	}
	if host, port, e := net.SplitHostPort(p.endpoint); e != nil || host == "" || port == "" || len(p.endpoint) > 65536 || strings.ContainsAny(p.endpoint, "/@?#\r\n\t ") {
		return p, errors.New("gRPC endpoint must be host:port without a URL scheme, credentials or path")
	}
	authority, err := resolve(definition.Authority)
	if err != nil {
		return p, err
	}
	if strings.ContainsAny(authority, "\x00\r\n\t /@?#") {
		return p, errors.New("invalid gRPC authority")
	}
	p.options = grpcclient.Options{Plaintext: definition.Plaintext, Authority: authority,
		MaxSendBytes: definition.MaxSendBytes, MaxReceiveBytes: definition.MaxReceiveBytes,
		ConnectTimeout: time.Duration(definition.ConnectTimeoutMS) * time.Millisecond,
		Compression:    definition.Compression}
	if !definition.Plaintext {
		p.options.TLSConfig, err = s.prepareHTTPTLS(definition.TLS, resolve)
		if err != nil {
			return p, err
		}
	}
	metadataBytes := 0
	for _, row := range definition.Metadata {
		if !row.Enabled {
			continue
		}
		name, e := resolve(row.Name)
		if e != nil {
			return p, e
		}
		if name == "" || strings.HasPrefix(name, "grpc-") || name == "content-type" || name == "te" || name == "user-agent" {
			return p, errors.New("gRPC metadata has an empty or reserved name")
		}
		for _, char := range name {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
				return p, errors.New("gRPC metadata names must use lowercase letters, digits, '-', '_' or '.'")
			}
		}
		value, e := resolve(row.Value)
		if e != nil {
			return p, e
		}
		sensitive := sensitiveGRPCMetadata(name)
		if sensitive {
			p.secrets = append(p.secrets, value)
			if _, token, ok := strings.Cut(value, " "); ok {
				p.secrets = append(p.secrets, token)
			}
		}
		metadataBytes += len(name) + len(value) + 32
		if metadataBytes > 64<<10 {
			return p, errors.New("gRPC metadata exceeds 64 KiB")
		}
		if strings.HasSuffix(name, "-bin") {
			secretBinary := sensitive || grpcContainsSecret([]byte(value), store.values) || grpcContainsSecret([]byte(value), p.secrets)
			decoded, e := base64.StdEncoding.Strict().DecodeString(value)
			if e != nil {
				return p, errors.New("binary gRPC metadata must contain standard base64")
			}
			value = string(decoded)
			if secretBinary {
				p.secrets = append(p.secrets, value)
			}
		} else {
			for _, char := range []byte(value) {
				if char < 0x20 || char > 0x7e {
					return p, errors.New("non-binary gRPC metadata must contain printable ASCII")
				}
			}
		}
		p.metadata[name] = append(p.metadata[name], value)
	}
	if mode == "reflection" {
		return p, nil
	}
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return p, err
	}
	defer done()
	var selected domain.ProtoSchema
	for _, candidate := range state.ProtoSchemas {
		if candidate.ID == definition.SchemaID && candidate.WorkspaceID == request.WorkspaceID {
			selected = candidate
			break
		}
	}
	if selected.ID == "" {
		return p, errors.New("select an imported schema or discover services with server reflection")
	}
	p.registry, err = restoreProtoSchema(work, selected)
	if err != nil {
		return p, err
	}
	p.options.Resolver = p.registry.Types
	p.method, err = p.registry.Method(definition.Method)
	if err != nil {
		return p, errors.New("selected RPC method is not present in the imported schema")
	}
	streaming := p.method.IsStreamingClient() || p.method.IsStreamingServer()
	if mode != "stream" && streaming {
		return p, errors.New("selected RPC is streaming; choose a unary method")
	}
	if mode == "stream" && !streaming {
		return p, errors.New("selected RPC is unary; use Call instead of opening a stream")
	}
	if mode == "stream" && p.method.IsStreamingClient() {
		return p, nil
	}
	p.message, err = p.decodeBody(work, definition.Body, definition.BodyFormat)
	return p, err
}

func (p *grpcPrepared) decodeBody(ctx context.Context, body, bodyFormat string) (*dynamicpb.Message, error) {
	if len(body) > security.MaxTemplateBytes {
		return nil, errors.New("gRPC message exceeds 4 MiB")
	}
	defer p.collectSecrets()
	format := jsoncodec.Format(bodyFormat)
	if format == "" {
		format = jsoncodec.StrictJSON
	}
	normalized, err := jsoncodec.Normalize(body, format)
	if err != nil {
		return nil, err
	}
	resolved, err := security.ResolveJSON(ctx, string(normalized), p.env, p.store)
	if err != nil {
		return nil, err
	}
	return jsoncodec.Decode(p.registry, p.method.Input(), resolved.Value, jsoncodec.StrictJSON)
}

func (p *grpcPrepared) collectSecrets() {
	known := map[string]bool{}
	for _, value := range p.secrets {
		if value != "" {
			known[value] = true
		}
	}
	if p.store != nil {
		for _, value := range p.store.values {
			if value != "" {
				known[value] = true
			}
		}
	}
	for value := range known {
		for _, encoding := range []*base64.Encoding{base64.StdEncoding.Strict(), base64.RawStdEncoding.Strict(), base64.URLEncoding.Strict(), base64.RawURLEncoding.Strict()} {
			if decoded, err := encoding.DecodeString(value); err == nil && len(decoded) != 0 {
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

// A long-lived stream may resolve different variables in successive sends.
// Bound its cached keyring material independently of the number of sends.
type grpcBoundedStore struct {
	security.Store
	bytes int
}

func (s *grpcBoundedStore) Get(ctx context.Context, key string) (string, error) {
	value, err := s.Store.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if len(value) > (4<<20)-s.bytes {
		return "", errors.New("gRPC secret values exceed 4 MiB")
	}
	s.bytes += len(value)
	return value, nil
}
