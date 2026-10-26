// Package grpcclient invokes descriptor-selected RPCs without generated stubs.
// Connections and streams must be closed or canceled by their owner. The package
// does not log messages, metadata, endpoint credentials, or peer status text.
package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	DefaultMaxMessageBytes  = 4 << 20
	DefaultMaxMetadataBytes = 64 << 10
	DefaultConnectTimeout   = 10 * time.Second
)

// Options configures a connection. TLS certificate verification is enabled by
// default. TLSConfig is cloned; callers can supply RootCAs, client certificates,
// ServerName, or explicitly opt into InsecureSkipVerify for a single connection.
// Plaintext and TLSConfig are mutually exclusive. Zero limits use the defaults.
type Options struct {
	Plaintext        bool
	TLSConfig        *tls.Config
	Authority        string
	MaxSendBytes     int
	MaxReceiveBytes  int
	MaxMetadataBytes uint32
	ConnectTimeout   time.Duration
	// Compression accepts "none" (the default) or "gzip". Response gzip is
	// supported independently of this outgoing request setting.
	Compression string
	// Resolver preserves locally imported proto2 extensions while decoding a
	// dynamic response. It is optional for schemas without extensions.
	Resolver protoregistry.ExtensionTypeResolver
}

type Client struct {
	conn            *grpc.ClientConn
	maxSend         int
	maxMetadata     uint32
	connectDuration time.Duration
	resolver        protoregistry.ExtensionTypeResolver
}

// Dial waits until the transport is ready. target must be a host:port endpoint,
// not a URL or gRPC resolver expression, so embedded URL credentials cannot leak
// through grpc-go's connection diagnostics. The context bounds DNS, TCP and TLS.
func Dial(ctx context.Context, target string, options Options) (*Client, error) {
	started := time.Now()
	if ctx == nil {
		return nil, errors.New("gRPC connect: context is nil")
	}
	if host, port, err := net.SplitHostPort(target); err != nil || host == "" || port == "" || strings.ContainsAny(target, "/@?#") || hasControlOrSpace(target) {
		return nil, errors.New("gRPC connect: endpoint must be host:port without URL credentials or a path")
	}
	if options.Plaintext && options.TLSConfig != nil {
		return nil, errors.New("gRPC connect: plaintext cannot have TLS settings")
	}
	if options.Compression != "" && options.Compression != "none" && options.Compression != gzip.Name {
		return nil, errors.New("gRPC connect: compression must be none or gzip")
	}
	if options.MaxSendBytes < 0 || options.MaxReceiveBytes < 0 || options.ConnectTimeout < 0 {
		return nil, errors.New("gRPC connect: message limits and connect timeout must be positive")
	}
	if options.MaxSendBytes == 0 {
		options.MaxSendBytes = DefaultMaxMessageBytes
	}
	if options.MaxReceiveBytes == 0 {
		options.MaxReceiveBytes = DefaultMaxMessageBytes
	}
	if options.MaxMetadataBytes == 0 {
		options.MaxMetadataBytes = DefaultMaxMetadataBytes
	}
	if options.ConnectTimeout == 0 {
		options.ConnectTimeout = DefaultConnectTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, options.ConnectTimeout)
	defer cancel()

	var transportCredentials credentials.TransportCredentials = insecure.NewCredentials()
	attempts := &connectionAttempts{}
	if !options.Plaintext {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if options.TLSConfig != nil {
			config = options.TLSConfig.Clone()
		}
		if config.MinVersion < tls.VersionTLS12 {
			config.MinVersion = tls.VersionTLS12
		}
		transportCredentials = &observedCredentials{TransportCredentials: credentials.NewTLS(config), attempts: attempts}
	}
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(options.MaxSendBytes), grpc.MaxCallRecvMsgSize(options.MaxReceiveBytes), grpc.ForceCodecV2(boundedProtoCodec{resolver: options.Resolver})),
		grpc.WithMaxHeaderListSize(options.MaxMetadataBytes),
		grpc.WithDisableServiceConfig(),
		grpc.WithDisableRetry(),
		grpc.WithNoProxy(),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
			if err != nil {
				attempts.record("TCP connection failed", err)
			}
			return conn, err
		}),
	}
	if options.Authority != "" {
		if strings.ContainsAny(options.Authority, "/@?#") || hasControlOrSpace(options.Authority) {
			return nil, errors.New("gRPC connect: invalid authority")
		}
		dialOptions = append(dialOptions, grpc.WithAuthority(options.Authority))
	}
	if options.Compression == gzip.Name {
		dialOptions = append(dialOptions, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
	}
	conn, err := grpc.NewClient("passthrough:///"+target, dialOptions...)
	if err != nil {
		return nil, errors.New("gRPC connect: invalid TLS or authority configuration")
	}
	conn.Connect()
	for {
		state := conn.GetState()
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("gRPC connect: %s: %w", attempts.reason(), err)
		}
		if state == connectivity.Ready {
			return &Client{conn: conn, maxSend: options.MaxSendBytes, maxMetadata: options.MaxMetadataBytes, connectDuration: time.Since(started), resolver: options.Resolver}, nil
		}
		if state == connectivity.Shutdown {
			_ = conn.Close()
			return nil, errors.New("gRPC connect: connection shut down before becoming ready")
		}
		conn.WaitForStateChange(ctx, state)
	}
}

func hasControlOrSpace(value string) bool {
	return strings.IndexFunc(value, func(char rune) bool { return unicode.IsControl(char) || unicode.IsSpace(char) }) >= 0
}

// Close releases the connection and cancels its active RPCs.
func (c *Client) Close() error { return c.conn.Close() }

// ConnectDuration includes DNS, TCP and (when enabled) the verified TLS
// handshake. It excludes subsequent calls on this connection.
func (c *Client) ConnectDuration() time.Duration { return c.connectDuration }

// FullMethod builds the wire path exclusively from the method's parent service.
func FullMethod(method protoreflect.MethodDescriptor) (string, error) {
	if isNil(method) || method.IsPlaceholder() {
		return "", errors.New("gRPC method: descriptor is missing or unresolved")
	}
	service, ok := method.Parent().(protoreflect.ServiceDescriptor)
	if !ok || service.IsPlaceholder() || service.FullName() == "" || !service.FullName().IsValid() || !method.Name().IsValid() {
		return "", errors.New("gRPC method: descriptor must belong to a resolved service")
	}
	if method.Input() == nil || method.Output() == nil || method.Input().IsPlaceholder() || method.Output().IsPlaceholder() {
		return "", errors.New("gRPC method: input or output descriptor is unresolved")
	}
	return "/" + string(service.FullName()) + "/" + string(method.Name()), nil
}

// RPCError deliberately omits peer-controlled messages from Error(). GRPCStatus
// exposes the original status, including message and details, to a caller that
// explicitly chooses to display them. Do not log those values without redaction.
type RPCError struct {
	Operation, Method string
	rpcStatus         *status.Status
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("gRPC %s %s: %s", e.Operation, e.Method, e.rpcStatus.Code())
}
func (e *RPCError) GRPCStatus() *status.Status { return e.rpcStatus }

func rpcError(operation, method string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return &RPCError{Operation: operation, Method: method, rpcStatus: status.Convert(err)}
}

// UnaryResult is returned even when the peer returns a non-OK status. Message is
// nil on error. Metadata and status may contain secrets and are never logged.
type UnaryResult struct {
	Message  *dynamicpb.Message
	Headers  metadata.MD
	Trailers metadata.MD
	Status   *status.Status
	Peer     *peer.Peer
	Duration time.Duration
	// Wire contains the original decompressed response payload, before
	// protobuf decoding can reorder fields or collapse repeated singular tags.
	// It is bounded by MaxReceiveBytes and must be redacted before UI display.
	Wire []byte
}

func (c *Client) Unary(ctx context.Context, method protoreflect.MethodDescriptor, request proto.Message, md metadata.MD) (*UnaryResult, error) {
	path, err := FullMethod(method)
	if err != nil {
		return nil, err
	}
	if method.IsStreamingClient() || method.IsStreamingServer() {
		return nil, fmt.Errorf("gRPC invoke %s: streaming method requires NewStream", path)
	}
	if err := validateMessage(method.Input(), request, c.maxSend); err != nil {
		return nil, fmt.Errorf("gRPC invoke %s: %w", path, err)
	}
	ctx, err = outgoingContext(ctx, md, c.maxMetadata)
	if err != nil {
		return nil, err
	}
	result := &UnaryResult{Message: dynamicpb.NewMessage(method.Output()), Peer: new(peer.Peer)}
	codec := boundedProtoCodec{resolver: c.resolver, capture: func(wire []byte) {
		result.Wire = append([]byte(nil), wire...)
	}}
	started := time.Now()
	err = c.conn.Invoke(ctx, path, request, result.Message, grpc.Header(&result.Headers), grpc.Trailer(&result.Trailers), grpc.Peer(result.Peer), grpc.ForceCodecV2(codec))
	result.Duration = time.Since(started)
	result.Status = status.Convert(err)
	if err == nil {
		result.Status = status.New(codes.OK, "")
	}
	if err != nil {
		result.Message = nil
	}
	return result, rpcError("invoke", path, err)
}

func validateMessage(expected protoreflect.MessageDescriptor, message proto.Message, maxSend int) error {
	if isNil(message) || !message.ProtoReflect().IsValid() {
		return errors.New("request message is nil")
	}
	if message.ProtoReflect().Descriptor() != expected {
		return errors.New("request message descriptor does not match the method input")
	}
	if err := proto.CheckInitialized(message); err != nil {
		return errors.New("request message is missing required fields")
	}
	if proto.Size(message) > maxSend {
		return status.Error(codes.ResourceExhausted, "request message exceeds the configured send limit")
	}
	return nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// outgoingContext copies both explicit and inherited metadata to keep stream
// ownership clear. Binary metadata values are raw bytes in strings, as grpc-go
// requires; UI base64 conversion belongs above this transport package.
func outgoingContext(ctx context.Context, md metadata.MD, maxBytes uint32) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("gRPC metadata: context is nil")
	}
	inherited, _ := metadata.FromOutgoingContext(ctx)
	combined := metadata.Join(inherited, md)
	var size uint64
	for key, values := range combined {
		if key == "" || strings.HasPrefix(key, "grpc-") || key == "content-type" || key == "te" || key == "user-agent" {
			return nil, errors.New("gRPC metadata: empty or reserved key")
		}
		for _, char := range key {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
				return nil, errors.New("gRPC metadata: keys must use lowercase letters, digits, '-', '_' or '.'")
			}
		}
		for _, value := range values {
			if !strings.HasSuffix(key, "-bin") {
				for _, char := range []byte(value) {
					if char < 0x20 || char > 0x7e {
						return nil, errors.New("gRPC metadata: non-binary values must be printable ASCII")
					}
				}
			}
			valueBytes := uint64(len(value))
			if strings.HasSuffix(key, "-bin") {
				valueBytes = (valueBytes*8 + 5) / 6
			}
			size += uint64(len(key)+32) + valueBytes
			if size > uint64(maxBytes) {
				return nil, errors.New("gRPC metadata: configured size limit exceeded")
			}
		}
	}
	return metadata.NewOutgoingContext(ctx, combined), nil
}

type connectionAttempts struct {
	mu         sync.Mutex
	lastReason string
}

func (a *connectionAttempts) record(fallback string, err error) {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var dns *net.DNSError
	var networkError net.Error
	switch {
	case errors.As(err, &unknown):
		fallback = "TLS certificate is not signed by a trusted authority"
	case errors.As(err, &hostname):
		fallback = "TLS certificate does not match the server name"
	case errors.As(err, &invalid):
		fallback = "TLS certificate is invalid or expired"
	case errors.As(err, &dns):
		fallback = "DNS resolution failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		fallback = "TCP connection was refused"
	case errors.As(err, &networkError) && networkError.Timeout():
		fallback = "connection attempt timed out"
	}
	a.mu.Lock()
	a.lastReason = fallback
	a.mu.Unlock()
}
func (a *connectionAttempts) reason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastReason == "" {
		return "transport did not become ready"
	}
	return a.lastReason
}

type observedCredentials struct {
	credentials.TransportCredentials
	attempts *connectionAttempts
}

func (c *observedCredentials) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	secured, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, conn)
	if err != nil {
		c.attempts.record("TLS handshake failed", err)
	}
	return secured, info, err
}
func (c *observedCredentials) Clone() credentials.TransportCredentials {
	return &observedCredentials{TransportCredentials: c.TransportCredentials.Clone(), attempts: c.attempts}
}
