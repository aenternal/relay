package grpcclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The server and client use runtime descriptors exclusively: no generated
// service, request, or response types and no external protoc executable.
func testMethods(t *testing.T) map[string]protoreflect.MethodDescriptor {
	t.Helper()
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("transport_test.proto"), Package: proto.String("relay.transport.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Request"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("text"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("sequence"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			}},
			{Name: proto.String("Response"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("text"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("sequence"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Echo"), Method: []*descriptorpb.MethodDescriptorProto{
			{Name: proto.String("Unary"), InputType: proto.String(".relay.transport.v1.Request"), OutputType: proto.String(".relay.transport.v1.Response")},
			{Name: proto.String("Chat"), InputType: proto.String(".relay.transport.v1.Request"), OutputType: proto.String(".relay.transport.v1.Response"), ClientStreaming: proto.Bool(true), ServerStreaming: proto.Bool(true)},
			{Name: proto.String("Watch"), InputType: proto.String(".relay.transport.v1.Request"), OutputType: proto.String(".relay.transport.v1.Response"), ServerStreaming: proto.Bool(true)},
			{Name: proto.String("Collect"), InputType: proto.String(".relay.transport.v1.Request"), OutputType: proto.String(".relay.transport.v1.Response"), ClientStreaming: proto.Bool(true)},
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]protoreflect.MethodDescriptor{}
	service := file.Services().Get(0)
	for i := 0; i < service.Methods().Len(); i++ {
		method := service.Methods().Get(i)
		methods[string(method.Name())] = method
	}
	return methods
}

func request(method protoreflect.MethodDescriptor, text string, sequence int64) *dynamicpb.Message {
	message := dynamicpb.NewMessage(method.Input())
	message.Set(message.Descriptor().Fields().ByName("text"), protoreflect.ValueOfString(text))
	message.Set(message.Descriptor().Fields().ByName("sequence"), protoreflect.ValueOfInt64(sequence))
	return message
}

func response(method protoreflect.MethodDescriptor, message *dynamicpb.Message) *dynamicpb.Message {
	out := dynamicpb.NewMessage(method.Output())
	for _, name := range []protoreflect.Name{"text", "sequence"} {
		out.Set(out.Descriptor().Fields().ByName(name), message.Get(message.Descriptor().Fields().ByName(name)))
	}
	return out
}

type testService interface{}

func startTestServer(t *testing.T, methods map[string]protoreflect.MethodDescriptor, options ...grpc.ServerOption) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(options...)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "relay.transport.v1.Echo", HandlerType: (*testService)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Unary", Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			input := dynamicpb.NewMessage(methods["Unary"].Input())
			if err := decode(input); err != nil {
				return nil, err
			}
			incoming, _ := metadata.FromIncomingContext(ctx)
			_ = grpc.SetHeader(ctx, metadata.Join(metadata.Pairs("server", "dynamic"), metadata.MD{"echo-tag": incoming.Get("tag"), "echo-bin": incoming.Get("payload-bin")}))
			_ = grpc.SetTrailer(ctx, metadata.Pairs("done", "unary"))
			text := input.Get(input.Descriptor().Fields().ByName("text")).String()
			switch text {
			case "wait":
				<-ctx.Done()
				return nil, status.FromContextError(ctx.Err()).Err()
			case "fail":
				return nil, peerFailure()
			case "big":
				return response(methods["Unary"], request(methods["Unary"], strings.Repeat("x", 1024), 0)), nil
			}
			return response(methods["Unary"], input), nil
		}}},
		Streams: []grpc.StreamDesc{
			{StreamName: "Chat", ClientStreams: true, ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				_ = stream.SendHeader(metadata.Pairs("server", "dynamic"))
				stream.SetTrailer(metadata.Pairs("done", "chat"))
				for {
					input := dynamicpb.NewMessage(methods["Chat"].Input())
					if err := stream.RecvMsg(input); err != nil {
						if errors.Is(err, io.EOF) {
							return stream.SendMsg(response(methods["Chat"], request(methods["Chat"], "after-half-close", 99)))
						}
						return err
					}
					if input.Get(input.Descriptor().Fields().ByName("text")).String() == "fail" {
						return peerFailure()
					}
					if err := stream.SendMsg(response(methods["Chat"], input)); err != nil {
						return err
					}
					if input.Get(input.Descriptor().Fields().ByName("text")).String() == "pause" {
						<-stream.Context().Done()
						return status.FromContextError(stream.Context().Err()).Err()
					}
				}
			}},
			{StreamName: "Watch", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				input := dynamicpb.NewMessage(methods["Watch"].Input())
				if err := stream.RecvMsg(input); err != nil {
					return err
				}
				for range 2 {
					if err := stream.SendMsg(response(methods["Watch"], input)); err != nil {
						return err
					}
				}
				return nil
			}},
			{StreamName: "Collect", ClientStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				var count int64
				for {
					input := dynamicpb.NewMessage(methods["Collect"].Input())
					if err := stream.RecvMsg(input); err != nil {
						if errors.Is(err, io.EOF) {
							return stream.SendMsg(response(methods["Collect"], request(methods["Collect"], "count", count)))
						}
						return err
					}
					count++
				}
			}},
		},
	}, struct{}{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return listener.Addr().String()
}

func peerFailure() error {
	s, _ := status.New(codes.PermissionDenied, "peer secret token").WithDetails(&errdetails.ErrorInfo{Reason: "DENIED", Domain: "test"})
	return s.Err()
}

func testClient(t *testing.T, address string, options Options) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestUnaryDynamicMetadataAndStatus(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Unary(ctx, methods["Unary"], request(methods["Unary"], "hello", 9223372036854775807), metadata.Pairs("tag", "a", "tag", "b", "payload-bin", "\x00\xff"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Get(result.Message.Descriptor().Fields().ByName("sequence")).Int() != 9223372036854775807 {
		t.Fatal("int64 did not survive wire roundtrip")
	}
	if fmt.Sprint(result.Headers.Get("echo-tag")) != "[a b]" || result.Headers.Get("echo-bin")[0] != "\x00\xff" || result.Trailers.Get("done")[0] != "unary" || result.Status.Code() != codes.OK {
		t.Fatalf("wrong result: %+v", result)
	}
	result, err = client.Unary(ctx, methods["Unary"], request(methods["Unary"], "fail", 0), nil)
	if status.Code(err) != codes.PermissionDenied || result.Message != nil || result.Status.Message() != "peer secret token" || len(result.Status.Details()) != 1 || result.Trailers.Get("done")[0] != "unary" {
		t.Fatalf("missing peer status/trailers: %v, %+v", err, result)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("peer text leaked into diagnostic: %v", err)
	}
}

func TestBidiInterleavingAndHalfClose(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, methods["Chat"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if stream.Status() != nil || stream.Trailers() != nil {
		t.Fatal("stream prematurely terminal")
	}
	for i := int64(0); i < 3; i++ {
		if err := stream.Send(request(methods["Chat"], "hello", i)); err != nil {
			t.Fatal(err)
		}
		message, err := stream.Recv()
		if err != nil || message.Get(message.Descriptor().Fields().ByName("sequence")).Int() != i {
			t.Fatalf("wrong echo: %v %v", message, err)
		}
	}
	headers, err := stream.Headers()
	if err != nil || headers.Get("server")[0] != "dynamic" {
		t.Fatalf("wrong initial headers: %v %v", headers, err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(request(methods["Chat"], "late", 0)); err == nil {
		t.Fatal("send after half-close accepted")
	}
	last, err := stream.Recv()
	if err != nil || last.Get(last.Descriptor().Fields().ByName("text")).String() != "after-half-close" {
		t.Fatalf("half-close canceled incoming direction: %v %v", last, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("wanted EOF, got %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("repeated Recv: %v", err)
	}
	if stream.Status().Code() != codes.OK || stream.Trailers().Get("done")[0] != "chat" {
		t.Fatal("final status/trailers missing")
	}
	copy := stream.Trailers()
	copy.Set("done", "changed")
	if stream.Trailers().Get("done")[0] != "chat" {
		t.Fatal("mutable trailer ownership")
	}
}

func TestStreamCancelDeadlineAndPeerFailure(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	t.Run("cancel", func(t *testing.T) {
		stream, err := client.NewStream(context.Background(), methods["Chat"], nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Headers(); err != nil {
			t.Fatal(err)
		}
		stream.Cancel()
		if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
			t.Fatalf("wanted canceled, got %v", err)
		}
		if stream.Status().Code() != codes.Canceled {
			t.Fatal("missing canceled final status")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		stream, err := client.NewStream(ctx, methods["Chat"], nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Cancel()
		if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("wanted deadline, got %v", err)
		}
	})
	t.Run("midstream peer status", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.NewStream(ctx, methods["Chat"], nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Cancel()
		if err := stream.Send(request(methods["Chat"], "first", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(request(methods["Chat"], "fail", 2)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.PermissionDenied || strings.Contains(err.Error(), "secret") {
			t.Fatalf("wrong status or unsafe error: %v", err)
		}
		if stream.Status().Message() != "peer secret token" || len(stream.Status().Details()) != 1 || stream.Trailers().Get("done")[0] != "chat" {
			t.Fatal("lost peer details/trailers")
		}
	})
}

func TestOtherStreamingFlags(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := client.NewStream(ctx, methods["Watch"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Cancel()
	if err := watch.Send(request(methods["Watch"], "watch", 1)); err != nil {
		t.Fatal(err)
	}
	if err := watch.Send(request(methods["Watch"], "invalid second request", 2)); err == nil {
		t.Fatal("server stream accepted extra request")
	}
	if err := watch.CloseSend(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := watch.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := watch.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("watch final status: %v", err)
	}
	collect, err := client.NewStream(ctx, methods["Collect"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer collect.Cancel()
	for range 3 {
		if err := collect.Send(request(methods["Collect"], "item", 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := collect.CloseSend(); err != nil {
		t.Fatal(err)
	}
	message, err := collect.Recv()
	if err != nil || message.Get(message.Descriptor().Fields().ByName("sequence")).Int() != 3 {
		t.Fatalf("collect response: %v %v", message, err)
	}
	if _, err := collect.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("collect final status: %v", err)
	}
}

func TestLimitsAndInputValidation(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true, MaxSendBytes: 64, MaxReceiveBytes: 64})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, md := range []metadata.MD{
		{":authority": {"secret"}}, {"UPPER": {"secret"}}, {"grpc-status": {"secret"}}, {"content-type": {"secret"}}, {"authorization": {"secret\n"}}, {"large": {strings.Repeat("s", DefaultMaxMetadataBytes)}},
	} {
		if _, err := client.Unary(ctx, methods["Unary"], request(methods["Unary"], "ok", 0), md); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("metadata validation missing or unsafe: %v", err)
		}
	}
	if _, err := client.Unary(ctx, nil, request(methods["Unary"], "ok", 0), nil); err == nil {
		t.Fatal("nil descriptor accepted")
	}
	if _, err := client.Unary(ctx, methods["Chat"], request(methods["Chat"], "ok", 0), nil); err == nil {
		t.Fatal("streaming descriptor accepted as unary")
	}
	if _, err := client.NewStream(ctx, methods["Unary"], nil); err == nil {
		t.Fatal("unary descriptor accepted as streaming")
	}
	if _, err := client.Unary(ctx, methods["Unary"], dynamicpb.NewMessage(methods["Unary"].Output()), nil); err == nil {
		t.Fatal("wrong message type accepted")
	}
	var nilMessage *dynamicpb.Message
	if _, err := client.Unary(ctx, methods["Unary"], nilMessage, nil); err == nil {
		t.Fatal("typed nil accepted")
	}
	if _, err := client.Unary(ctx, methods["Unary"], request(methods["Unary"], strings.Repeat("s", 100), 0), nil); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("send limit: %v", err)
	}
	result, err := client.Unary(ctx, methods["Unary"], request(methods["Unary"], "big", 0), nil)
	if status.Code(err) != codes.ResourceExhausted || result.Status.Code() != codes.ResourceExhausted {
		t.Fatalf("receive limit: %v", err)
	}
	path, err := FullMethod(methods["Unary"])
	if err != nil || path != "/relay.transport.v1.Echo/Unary" {
		t.Fatalf("wrong method path: %s %v", path, err)
	}
}

func TestCancelUnblocksFlowControlledSend(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, methods["Chat"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.Send(request(methods["Chat"], "pause", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		// The server has stopped reading. Flow control must eventually block a
		// sender, and cancellation must interrupt that sender without sendMu.
		for range 16 {
			if err := stream.Send(request(methods["Chat"], strings.Repeat("x", 1<<20), 0)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		t.Fatalf("expected backpressure before cancellation, got %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	stream.Cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked sends completed despite cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock Send")
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation final status: %v", err)
	}
}

func TestUnaryDeadlineAndConnectionClose(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Unary(ctx, methods["Unary"], request(methods["Unary"], "wait", 0), nil); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("unary deadline: %v", err)
	}
	stream, err := client.NewStream(context.Background(), methods["Chat"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if _, err := stream.Headers(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("connection close did not cancel stream: %v", err)
	}
}

func TestConcurrentStreamDirections(t *testing.T) {
	methods := testMethods(t)
	client := testClient(t, startTestServer(t, methods), Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, methods["Chat"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	const count = 24
	sendErrors := make(chan error, count)
	var sends sync.WaitGroup
	for i := range count {
		sends.Go(func() { sendErrors <- stream.Send(request(methods["Chat"], "concurrent", int64(i))) })
	}
	closed := make(chan error, 1)
	go func() { sends.Wait(); closed <- stream.CloseSend() }()
	seen := map[int64]bool{}
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[message.Get(message.Descriptor().Fields().ByName("sequence")).Int()] = true
		_ = stream.Trailers()
		_ = stream.Status()
	}
	for range count {
		if err := <-sendErrors; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if len(seen) != count+1 {
		t.Fatalf("expected all messages and final half-close response, received %d", len(seen))
	}
}

func testTLSCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("certificate not parsed")
	}
	return cert, roots
}

func TestTLSVerificationDefaultAndExplicitSettings(t *testing.T) {
	methods := testMethods(t)
	certificate, roots := testTLSCertificate(t)
	address := startTestServer(t, methods, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})))
	mtlsAddress := startTestServer(t, methods, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})))
	for _, test := range []struct {
		name      string
		address   string
		options   Options
		wantError string
	}{
		{name: "default roots reject untrusted CA", address: address, wantError: "trusted authority"},
		{name: "explicit trusted CA", address: address, options: Options{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}},
		// Certificate verification can only be disabled by a request-scoped opt-in.
		{name: "explicit insecure opt-in", address: address, options: Options{TLSConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}},
		{name: "trusted CA rejects wrong server name", address: address, options: Options{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "wrong.example"}}, wantError: "server name"},
		{name: "mutual TLS", address: mtlsAddress, options: Options{TLSConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: "localhost", MinVersion: tls.VersionTLS12}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Allow platform trust-store initialization the same budget as
			// other connections; test rejection reasons, not verification speed.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, test.address, test.options)
			if client != nil {
				t.Cleanup(func() { _ = client.Close() })
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("TLS must reject connection with reason %q: %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer rpcCancel()
			if _, err := client.Unary(rpcCtx, methods["Unary"], request(methods["Unary"], "tls", 0), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDialValidationAndCancellation(t *testing.T) {
	for _, target := range []string{"https://user:secret@example.test:443/path", "127.0.0.1", "user:secret@localhost:443", "localhost:", ":443", "local host:443", "local\x00host:443", "local\u2000host:443"} {
		if _, err := Dial(context.Background(), target, Options{}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid or unsafe endpoint diagnostic: %v", err)
		}
	}
	for _, options := range []Options{{Plaintext: true, TLSConfig: &tls.Config{}}, {MaxSendBytes: -1}, {MaxReceiveBytes: -1}, {ConnectTimeout: -1}, {Authority: "secret\n"}, {Authority: "secret\x00"}, {Authority: "secret\t"}} {
		if _, err := Dial(context.Background(), "127.0.0.1:1", options); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid options accepted or unsafe: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dial(ctx, "127.0.0.1:1", Options{Plaintext: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial context cancellation: %v", err)
	}
}

func TestUnresolvedDescriptorAndRequiredInput(t *testing.T) {
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String("required.proto"), Package: proto.String("required.test"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Request"), Field: []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("text"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}}}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Service"), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: proto.String("Call"), InputType: proto.String(".required.test.Request"), OutputType: proto.String(".required.test.Missing"),
		}}}},
	}
	descriptor, err := (protodesc.FileOptions{AllowUnresolvable: true}).New(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	method := descriptor.Services().Get(0).Methods().Get(0)
	if _, err := FullMethod(method); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("unresolved message descriptor accepted: %v", err)
	}
	message := dynamicpb.NewMessage(method.Input())
	if err := validateMessage(method.Input(), message, DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing required field accepted: %v", err)
	}
	message.Set(method.Input().Fields().ByName("text"), protoreflect.ValueOfString("present"))
	if err := validateMessage(method.Input(), message, DefaultMaxMessageBytes); err != nil {
		t.Fatal(err)
	}
}
