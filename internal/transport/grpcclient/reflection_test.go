package grpcclient

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/registry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	v1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func startReflectionServer(t *testing.T, register func(*grpc.Server), options ...grpc.ServerOption) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(options...)
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return listener.Addr().String()
}

func registerReflectedEcho(server *grpc.Server, method protoreflect.MethodDescriptor) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(method.Parent().FullName()), HandlerType: (*testService)(nil), Metadata: method.ParentFile().Path(),
		Methods: []grpc.MethodDesc{{MethodName: string(method.Name()), Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			message := dynamicpb.NewMessage(method.Input())
			if err := decode(message); err != nil {
				return nil, err
			}
			_ = grpc.SetHeader(ctx, metadata.Pairs("from", "reflection-echo"))
			return response(method, message), nil
		}}},
	}, struct{}{})
}

func TestReflectionV1AndAlphaDynamicInvocationAndOfflineCache(t *testing.T) {
	for _, protocol := range []string{"v1", "v1alpha"} {
		t.Run(protocol, func(t *testing.T) {
			methods := testMethods(t)
			reg, err := registry.New([]protoreflect.FileDescriptor{methods["Unary"].ParentFile()})
			if err != nil {
				t.Fatal(err)
			}
			var verified atomic.Int32
			interceptor := grpc.StreamInterceptor(func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
				md, _ := metadata.FromIncomingContext(stream.Context())
				if strings.Join(md.Get("tag"), ",") != "first,second" || len(md.Get("payload-bin")) != 1 || md.Get("payload-bin")[0] != "\x00\xff" || strings.Join(md.Get(":authority"), "") != "schema.example" {
					return status.Error(codes.Unauthenticated, "credential-canary")
				}
				verified.Add(1)
				return handler(server, stream)
			})
			address := startReflectionServer(t, func(server *grpc.Server) {
				registerReflectedEcho(server, methods["Unary"])
				opts := reflection.ServerOptions{Services: server, DescriptorResolver: reg.Files}
				if protocol == "v1" {
					v1.RegisterServerReflectionServer(server, reflection.NewServerV1(opts))
				} else {
					alpha.RegisterServerReflectionServer(server, reflection.NewServer(opts))
				}
			}, interceptor)
			client := testClient(t, address, Options{Plaintext: true, Authority: "schema.example"})
			discovered, err := client.Reflection(context.Background(), metadata.Pairs("tag", "first", "tag", "second", "payload-bin", "\x00\xff"), ReflectionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if discovered.Protocol != protocol || strings.Join(discovered.Services, ",") != "relay.transport.v1.Echo" || verified.Load() != 1 {
				t.Fatalf("wrong protocol/services/metadata: %+v; streams=%d", discovered, verified.Load())
			}
			cached, err := importer.DescriptorSet(context.Background(), discovered.DescriptorSet)
			if err != nil {
				t.Fatalf("offline descriptor cache: %v", err)
			}
			method, err := cached.Method("/relay.transport.v1.Echo/Unary")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Unary(context.Background(), method, request(method, "Привет, 世界 😀", 9223372036854775807), nil)
			if err != nil || result.Message.Get(method.Output().Fields().ByName("text")).String() != "Привет, 世界 😀" || result.Message.Get(method.Output().Fields().ByName("sequence")).Int() != 9223372036854775807 {
				t.Fatalf("reflected dynamic invocation: %+v, %v", result, err)
			}
			if result.Peer == nil || result.Peer.Addr == nil || result.Duration <= 0 || client.ConnectDuration() <= 0 {
				t.Fatal("missing actual peer or timings")
			}
		})
	}
}

type customReflection struct {
	v1.UnimplementedServerReflectionServer
	serve func(v1.ServerReflection_ServerReflectionInfoServer) error
}

func (r customReflection) ServerReflectionInfo(stream v1.ServerReflection_ServerReflectionInfoServer) error {
	return r.serve(stream)
}

type countingAlphaReflection struct {
	alpha.UnimplementedServerReflectionServer
	calls *atomic.Int32
}

func (r countingAlphaReflection) ServerReflectionInfo(alpha.ServerReflection_ServerReflectionInfoServer) error {
	r.calls.Add(1)
	return status.Error(codes.Internal, "alpha-should-not-be-called")
}

func TestReflectionDisabledAndNeverFallbackOnAuthenticationOrOtherErrors(t *testing.T) {
	disabled := testClient(t, startReflectionServer(t, func(*grpc.Server) {}), Options{Plaintext: true})
	if _, err := disabled.Reflection(context.Background(), nil, ReflectionOptions{}); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "import a .proto") {
		t.Fatalf("disabled reflection diagnostic: %v", err)
	}
	for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied, codes.Unavailable, codes.InvalidArgument} {
		t.Run(code.String(), func(t *testing.T) {
			var alphaCalls atomic.Int32
			address := startReflectionServer(t, func(server *grpc.Server) {
				v1.RegisterServerReflectionServer(server, customReflection{serve: func(v1.ServerReflection_ServerReflectionInfoServer) error {
					return status.Error(code, "credential-canary")
				}})
				alpha.RegisterServerReflectionServer(server, countingAlphaReflection{calls: &alphaCalls})
			})
			client := testClient(t, address, Options{Plaintext: true})
			_, err := client.Reflection(context.Background(), nil, ReflectionOptions{})
			if status.Code(err) != code || alphaCalls.Load() != 0 || strings.Contains(err.Error(), "credential-canary") {
				t.Fatalf("unsafe fallback or peer diagnostic: %v; alpha=%d", err, alphaCalls.Load())
			}
		})
	}
}

func TestReflectionFallbackSharesDeadlineAndListLimits(t *testing.T) {
	remaining := make(chan time.Duration, 1)
	address := startReflectionServer(t, func(server *grpc.Server) {
		v1.RegisterServerReflectionServer(server, customReflection{serve: func(stream v1.ServerReflection_ServerReflectionInfoServer) error {
			select {
			case <-time.After(35 * time.Millisecond):
				return status.Error(codes.Unimplemented, "old server")
			case <-stream.Context().Done():
				return status.FromContextError(stream.Context().Err()).Err()
			}
		}})
		alpha.RegisterServerReflectionServer(server, deadlineAlphaReflection{remaining: remaining})
	})
	client := testClient(t, address, Options{Plaintext: true})
	if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{Timeout: 100 * time.Millisecond}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("fallback timeout: %v", err)
	}
	select {
	case left := <-remaining:
		if left > 80*time.Millisecond {
			t.Fatalf("fallback reset discovery deadline: %v remaining", left)
		}
	default:
		t.Fatal("v1 UNIMPLEMENTED did not fall back to alpha")
	}
	address = startReflectionServer(t, func(server *grpc.Server) {
		v1.RegisterServerReflectionServer(server, customReflection{serve: func(stream v1.ServerReflection_ServerReflectionInfoServer) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return stream.Send(&v1.ServerReflectionResponse{MessageResponse: &v1.ServerReflectionResponse_ListServicesResponse{ListServicesResponse: &v1.ListServiceResponse{Service: []*v1.ServiceResponse{{Name: "test.First"}, {Name: "test.Second"}}}}})
		}})
	})
	client = testClient(t, address, Options{Plaintext: true})
	if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{MaxServices: 1}); err == nil || !strings.Contains(err.Error(), "service limit") {
		t.Fatalf("service list limit: %v", err)
	}
	for _, options := range []ReflectionOptions{{MaxFiles: -1}, {MaxFiles: 513}, {MaxBytes: 17 << 20}, {MaxServices: 2049}, {MaxRequests: -1}, {Timeout: -1}} {
		if _, err := client.Reflection(context.Background(), nil, options); err == nil {
			t.Fatalf("invalid limits accepted: %+v", options)
		}
	}
}

type deadlineAlphaReflection struct {
	alpha.UnimplementedServerReflectionServer
	remaining chan time.Duration
}

func (r deadlineAlphaReflection) ServerReflectionInfo(stream alpha.ServerReflection_ServerReflectionInfoServer) error {
	deadline, _ := stream.Context().Deadline()
	r.remaining <- time.Until(deadline)
	<-stream.Context().Done()
	return status.FromContextError(stream.Context().Err()).Err()
}

// A controllable server deliberately omits transitive imports from each
// FileContainingSymbol response. Discovery must request those filenames, reject
// malformed closures, and never loop on repeated/cyclic dependencies.
func descriptorReflectionServer(t *testing.T, service string, roots [][]byte, dependencies map[string][][]byte, requested *atomic.Int32) string {
	t.Helper()
	return startReflectionServer(t, func(server *grpc.Server) {
		v1.RegisterServerReflectionServer(server, customReflection{serve: func(stream v1.ServerReflection_ServerReflectionInfoServer) error {
			for {
				request, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				out := &v1.ServerReflectionResponse{OriginalRequest: request}
				switch request.MessageRequest.(type) {
				case *v1.ServerReflectionRequest_ListServices:
					out.MessageResponse = &v1.ServerReflectionResponse_ListServicesResponse{ListServicesResponse: &v1.ListServiceResponse{Service: []*v1.ServiceResponse{{Name: service}}}}
				case *v1.ServerReflectionRequest_FileContainingSymbol:
					out.MessageResponse = &v1.ServerReflectionResponse_FileDescriptorResponse{FileDescriptorResponse: &v1.FileDescriptorResponse{FileDescriptorProto: roots}}
				case *v1.ServerReflectionRequest_FileByFilename:
					if requested != nil {
						requested.Add(1)
					}
					if files, ok := dependencies[request.GetFileByFilename()]; ok {
						out.MessageResponse = &v1.ServerReflectionResponse_FileDescriptorResponse{FileDescriptorResponse: &v1.FileDescriptorResponse{FileDescriptorProto: files}}
					} else {
						out.MessageResponse = &v1.ServerReflectionResponse_ErrorResponse{ErrorResponse: &v1.ErrorResponse{ErrorCode: int32(codes.NotFound), ErrorMessage: "credential-canary"}}
					}
				}
				if err := stream.Send(out); err != nil {
					return err
				}
			}
		}})
	})
}

func marshalDescriptor(t *testing.T, descriptor *descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	data, err := proto.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReflectionDependencyClosureAndResourceLimits(t *testing.T) {
	method := testMethods(t)["Unary"]
	root := protodesc.ToFileDescriptorProto(method.ParentFile())
	root.Dependency = []string{"common.proto"}
	dependency := &descriptorpb.FileDescriptorProto{Name: proto.String("common.proto"), Syntax: proto.String("proto3"), Package: proto.String("relay.common")}
	var requested atomic.Int32
	address := descriptorReflectionServer(t, "relay.transport.v1.Echo", [][]byte{marshalDescriptor(t, root)}, map[string][][]byte{"common.proto": {marshalDescriptor(t, dependency)}}, &requested)
	client := testClient(t, address, Options{Plaintext: true})
	result, err := client.Reflection(context.Background(), nil, ReflectionOptions{})
	if err != nil || requested.Load() != 1 || result.Registry.Files.NumFiles() != 2 {
		t.Fatalf("missing transitive filename query: result=%+v err=%v requests=%d", result, err, requested.Load())
	}
	for name, options := range map[string]ReflectionOptions{
		"files": {MaxFiles: 1}, "bytes": {MaxBytes: 10}, "requests": {MaxRequests: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.Reflection(context.Background(), nil, options); err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("resource limit not enforced: %v", err)
			}
		})
	}
	for name, roots := range map[string][][]byte{
		"missing":       {marshalDescriptor(t, root)},
		"cycle":         {marshalDescriptor(t, root), marshalDescriptor(t, &descriptorpb.FileDescriptorProto{Name: proto.String("common.proto"), Syntax: proto.String("proto3"), Dependency: []string{root.GetName()}})},
		"conflict":      {marshalDescriptor(t, root), marshalDescriptor(t, &descriptorpb.FileDescriptorProto{Name: root.Name, Syntax: proto.String("proto2")})},
		"malformed":     {{0xff, 0xff}},
		"path":          {marshalDescriptor(t, &descriptorpb.FileDescriptorProto{Name: proto.String("../credential-canary")})},
		"wrong service": {marshalDescriptor(t, dependency)},
	} {
		t.Run(name, func(t *testing.T) {
			address := descriptorReflectionServer(t, "relay.transport.v1.Echo", roots, nil, nil)
			client := testClient(t, address, Options{Plaintext: true})
			if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{Timeout: time.Second}); err == nil || strings.Contains(err.Error(), "credential-canary") {
				t.Fatalf("invalid descriptor closure accepted or diagnostic leaked: %v", err)
			}
		})
	}
}

func TestReflectionMissingStandardImportsAreCachedOffline(t *testing.T) {
	method := testMethods(t)["Unary"]
	root := protodesc.ToFileDescriptorProto(method.ParentFile())
	root.Dependency = []string{"google/protobuf/any.proto"}
	address := descriptorReflectionServer(t, "relay.transport.v1.Echo", [][]byte{marshalDescriptor(t, root)}, nil, nil)
	client := testClient(t, address, Options{Plaintext: true})
	result, err := client.Reflection(context.Background(), nil, ReflectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(result.DescriptorSet, set); err != nil || len(set.File) != 2 {
		t.Fatalf("missing self-contained offline WKT closure: %v, %v", set, err)
	}
	if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{MaxFiles: 1}); err == nil {
		t.Fatal("offline standard import bypassed file limit")
	}
}

func TestReflectionDeadlinesCancellationAndMalformedServiceNames(t *testing.T) {
	var closed atomic.Int32
	address := startReflectionServer(t, func(server *grpc.Server) {
		v1.RegisterServerReflectionServer(server, customReflection{serve: func(stream v1.ServerReflection_ServerReflectionInfoServer) error {
			defer closed.Add(1)
			<-stream.Context().Done()
			return status.FromContextError(stream.Context().Err()).Err()
		}})
	})
	client := testClient(t, address, Options{Plaintext: true})
	if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{Timeout: 25 * time.Millisecond}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("reflection timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Reflection(ctx, nil, ReflectionOptions{}); done <- err }()
	cancel()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("reflection cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reflection cancellation did not interrupt receive")
	}
	for _, name := range []string{"", "relay.😀", "/package.Service", "package.Service/Method"} {
		address := descriptorReflectionServer(t, name, nil, nil, nil)
		client := testClient(t, address, Options{Plaintext: true})
		if _, err := client.Reflection(context.Background(), nil, ReflectionOptions{}); err == nil {
			t.Fatalf("invalid service name %q accepted", name)
		}
	}
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("abandoned reflection stream was not canceled on peer")
	}
}

func TestReflectionTLSUsesSameTrustAndClientCertificate(t *testing.T) {
	methods := testMethods(t)
	reg, err := registry.New([]protoreflect.FileDescriptor{methods["Unary"].ParentFile()})
	if err != nil {
		t.Fatal(err)
	}
	certificate, roots := testTLSCertificate(t)
	address := startReflectionServer(t, func(server *grpc.Server) {
		registerReflectedEcho(server, methods["Unary"])
		v1.RegisterServerReflectionServer(server, reflection.NewServerV1(reflection.ServerOptions{Services: server, DescriptorResolver: reg.Files}))
	}, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})))
	client := testClient(t, address, Options{TLSConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: "localhost"}})
	result, err := client.Reflection(context.Background(), nil, ReflectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	method, err := result.Registry.Method("/relay.transport.v1.Echo/Unary")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Unary(context.Background(), method, request(method, "mTLS", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := response.Peer.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.NegotiatedProtocol != "h2" || len(info.State.VerifiedChains) == 0 || info.State.ServerName != "localhost" {
		t.Fatalf("missing actual verified TLS state: %+v", response.Peer)
	}
}

type compressionStats struct{ gzipHeaders atomic.Int32 }

func (*compressionStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (s *compressionStats) HandleRPC(_ context.Context, event stats.RPCStats) {
	if headers, ok := event.(*stats.InHeader); ok && headers.Compression == "gzip" {
		s.gzipHeaders.Add(1)
	}
}
func (*compressionStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (*compressionStats) HandleConn(context.Context, stats.ConnStats) {}

func TestUnaryGzipAndCompressedMessageLimits(t *testing.T) {
	methods := testMethods(t)
	observed := new(compressionStats)
	address := startTestServer(t, methods, grpc.StatsHandler(observed))
	client := testClient(t, address, Options{Plaintext: true, Compression: "gzip"})
	result, err := client.Unary(context.Background(), methods["Unary"], request(methods["Unary"], strings.Repeat("compressible", 4000), 3), nil)
	if err != nil || observed.gzipHeaders.Load() != 1 || len(result.Message.Get(methods["Unary"].Output().Fields().ByName("text")).String()) != 48000 {
		t.Fatalf("gzip request/response: %+v, %v, gzip headers=%d", result, err, observed.gzipHeaders.Load())
	}
	limited := testClient(t, address, Options{Plaintext: true, Compression: "gzip", MaxReceiveBytes: 128})
	if _, err := limited.Unary(context.Background(), methods["Unary"], request(methods["Unary"], "big", 0), nil); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("decoded gzip exceeded receive limit: %v", err)
	}
	if _, err := Dial(context.Background(), address, Options{Plaintext: true, Compression: "credential-canary"}); err == nil || strings.Contains(err.Error(), "credential-canary") {
		t.Fatalf("unsafe unsupported compression validation: %v", err)
	}
}
