// grpcfixture is a local dynamic gRPC server used by the desktop end-to-end
// tests. It has no generated application messages or external API dependency.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/registry"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	v1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type fixture struct {
	method         protoreflect.MethodDescriptor
	streams        []protoreflect.MethodDescriptor
	calls          atomic.Int64
	canceled       atomic.Int64
	streamCalls    atomic.Int64
	streamReceived atomic.Int64
	streamActive   atomic.Int64
	streamCanceled atomic.Int64
}

func (f *fixture) echo(ctx context.Context, request *dynamicpb.Message) (any, error) {
	f.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-origin", "first", "x-origin", "second", "x-echo-bin", first(md.Get("x-test-bin"))))
	grpc.SetTrailer(ctx, metadata.Pairs("x-trailer", "complete", "x-trailer", "done"))
	text := request.Get(f.method.Input().Fields().ByName("text")).String()
	switch text {
	case "delay":
		<-ctx.Done()
		f.canceled.Add(1)
		return nil, status.FromContextError(ctx.Err()).Err()
	case "denied":
		problem, err := status.New(codes.PermissionDenied, "policy blocks unary call "+first(md.Get("authorization"))).WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "sequence", Description: "test validation failure " + first(md.Get("authorization"))}}})
		if err != nil {
			return nil, status.Error(codes.Internal, "fixture status construction failed")
		}
		return nil, problem.Err()
	case "large":
		response := proto.Clone(request).(*dynamicpb.Message)
		response.Set(f.method.Output().Fields().ByName("text"), protoreflect.ValueOfString(string(make([]byte, 256<<10))))
		return response, nil
	default:
		return proto.Clone(request), nil
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (f *fixture) register(server *grpc.Server) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(f.method.Parent().FullName()), HandlerType: (*interface{})(nil), Metadata: f.method.ParentFile().Path(),
		Methods: []grpc.MethodDesc{{MethodName: string(f.method.Name()), Handler: func(_ any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			request := dynamicpb.NewMessage(f.method.Input())
			if err := decode(request); err != nil {
				return nil, err
			}
			if interceptor == nil {
				return f.echo(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: f, FullMethod: registry.FullMethod(f.method)}, func(ctx context.Context, message any) (any, error) { return f.echo(ctx, message.(*dynamicpb.Message)) })
		}}},
	}, f)
	methods := make([]grpc.StreamDesc, 0, len(f.streams))
	for _, method := range f.streams {
		methods = append(methods, grpc.StreamDesc{StreamName: string(method.Name()), ClientStreams: method.IsStreamingClient(), ServerStreams: method.IsStreamingServer(), Handler: func(_ any, stream grpc.ServerStream) error {
			return f.exchange(stream, method)
		}})
	}
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "relay.streaming.v1.StreamService", HandlerType: (*interface{})(nil), Metadata: f.streams[0].ParentFile().Path(), Streams: methods}, f)
}

func (f *fixture) exchange(stream grpc.ServerStream, method protoreflect.MethodDescriptor) error {
	session := f.streamCalls.Add(1)
	f.streamActive.Add(1)
	defer func() {
		f.streamActive.Add(-1)
		if stream.Context().Err() != nil {
			f.streamCanceled.Add(1)
		}
	}()
	md, _ := metadata.FromIncomingContext(stream.Context())
	if err := stream.SendHeader(metadata.Pairs("x-stream-session", strconv.FormatInt(session, 10), "x-origin", "first", "x-origin", "second", "x-echo-bin", first(md.Get("x-test-bin")))); err != nil {
		return err
	}
	stream.SetTrailer(metadata.Pairs("x-stream-trailer", "complete", "x-stream-trailer", "done"))
	fields := method.Input().Fields()
	textField, sequenceField := fields.ByName("text"), fields.ByName("sequence")
	receive := func() (*dynamicpb.Message, error) {
		message := dynamicpb.NewMessage(method.Input())
		if err := stream.RecvMsg(message); err != nil {
			return nil, err
		}
		f.streamReceived.Add(1)
		return message, nil
	}
	problem := func() error {
		result, err := status.New(codes.PermissionDenied, "policy blocks stream "+first(md.Get("authorization"))).WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "text", Description: "stream validation failure " + first(md.Get("authorization"))}}})
		if err != nil {
			return status.Error(codes.Internal, "fixture status construction failed")
		}
		return result.Err()
	}
	wait := func() error {
		<-stream.Context().Done()
		return status.FromContextError(stream.Context().Err()).Err()
	}
	switch method.Name() {
	case "Watch":
		request, err := receive()
		if err != nil {
			return err
		}
		text := request.Get(textField).String()
		if text == "delay" {
			return wait()
		}
		if text == "zero" {
			return nil
		}
		count := request.Get(sequenceField).Int()
		if count <= 0 || count > 3000 {
			count = 3
		}
		if text == "error" {
			count = 2
		}
		for index := int64(1); index <= count; index++ {
			message := dynamicpb.NewMessage(method.Output())
			message.Set(textField, protoreflect.ValueOfString(fmt.Sprintf("%s/%d", text, index)))
			message.Set(sequenceField, protoreflect.ValueOfInt64(index))
			if err := stream.SendMsg(message); err != nil {
				return err
			}
		}
		if text == "error" {
			return problem()
		}
		return nil
	case "Collect":
		var texts []string
		textBytes := 0
		for {
			message, err := receive()
			if err == io.EOF {
				response := dynamicpb.NewMessage(method.Output())
				response.Set(textField, protoreflect.ValueOfString(fmt.Sprintf("collected:%d:%s", len(texts), strings.Join(texts, ","))))
				response.Set(sequenceField, protoreflect.ValueOfInt64(int64(len(texts))))
				return stream.SendMsg(response)
			}
			if err != nil {
				return err
			}
			if len(texts) >= 3000 {
				return status.Error(codes.ResourceExhausted, "fixture message limit reached")
			}
			text := message.Get(textField).String()
			textBytes += len(text)
			if textBytes > 1<<20 {
				return status.Error(codes.ResourceExhausted, "fixture text limit reached")
			}
			if text == "delay" {
				return wait()
			}
			if text == "error" {
				return problem()
			}
			texts = append(texts, text)
		}
	default:
		for {
			message, err := receive()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			switch message.Get(textField).String() {
			case "delay":
				return wait()
			case "error":
				return problem()
			}
			if err := stream.SendMsg(message); err != nil {
				return err
			}
		}
	}
}

func serve(f *fixture, reg *registry.Registry, protocol string, secure bool) (*grpc.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	var options []grpc.ServerOption
	if secure {
		cert, err := certificate()
		if err != nil {
			listener.Close()
			return nil, "", err
		}
		options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
	}
	server := grpc.NewServer(options...)
	f.register(server)
	settings := reflection.ServerOptions{Services: server, DescriptorResolver: reg.Files}
	switch protocol {
	case "v1":
		v1.RegisterServerReflectionServer(server, reflection.NewServerV1(settings))
	case "v1alpha":
		alpha.RegisterServerReflectionServer(server, reflection.NewServer(settings))
	}
	go func() { _ = server.Serve(listener) }()
	return server, listener.Addr().String(), nil
}

func certificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Relay E2E"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, err
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	data, err := os.ReadFile("testdata/descriptors/editor.protoset")
	if err != nil {
		return err
	}
	reg, err := importer.DescriptorSet(ctx, data)
	if err != nil {
		return err
	}
	streamRegistry, err := importer.Compile(ctx, []string{"streaming/streams.proto"}, []string{"testdata/proto"})
	if err != nil {
		return err
	}
	var files []protoreflect.FileDescriptor
	for _, source := range []*registry.Registry{reg, streamRegistry} {
		source.Files.RangeFiles(func(file protoreflect.FileDescriptor) bool { files = append(files, file); return true })
	}
	reg, err = registry.New(files)
	if err != nil {
		return err
	}
	method, err := reg.Method("/relay.descriptor.v1.DescriptorService/Echo")
	if err != nil {
		return err
	}
	f := &fixture{method: method}
	for _, name := range []string{"Watch", "Collect", "Chat"} {
		streamMethod, err := reg.Method("/relay.streaming.v1.StreamService/" + name)
		if err != nil {
			return err
		}
		f.streams = append(f.streams, streamMethod)
	}
	addresses := map[string]string{}
	for _, config := range []struct {
		name, protocol string
		secure         bool
	}{{"reflection", "v1", false}, {"alpha", "v1alpha", false}, {"local", "", false}, {"tls", "v1", true}} {
		server, address, err := serve(f, reg, config.protocol, config.secure)
		if err != nil {
			return err
		}
		defer server.Stop()
		addresses[config.name] = address
	}
	stats, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	admin := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"calls": f.calls.Load(), "canceled": f.canceled.Load(), "streams": f.streamCalls.Load(), "streamReceived": f.streamReceived.Load(), "streamActive": f.streamActive.Load(), "streamCanceled": f.streamCanceled.Load()})
	})}
	defer admin.Close()
	go func() { _ = admin.Serve(stats) }()
	addresses["stats"] = "http://" + stats.Addr().String()
	if err := json.NewEncoder(os.Stdout).Encode(addresses); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gRPC fixture could not start:", err)
		os.Exit(1)
	}
}
