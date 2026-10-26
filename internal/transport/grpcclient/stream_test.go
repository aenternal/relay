package grpcclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/protobuf/registry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func startStreamTestServer(t *testing.T, method protoreflect.MethodDescriptor, handler grpc.StreamHandler, options ...grpc.ServerOption) string {
	t.Helper()
	return startReflectionServer(t, func(server *grpc.Server) {
		server.RegisterService(&grpc.ServiceDesc{
			ServiceName: string(method.Parent().FullName()), HandlerType: (*testService)(nil),
			Streams: []grpc.StreamDesc{{StreamName: string(method.Name()), ClientStreams: method.IsStreamingClient(), ServerStreams: method.IsStreamingServer(), Handler: handler}},
		}, struct{}{})
	}, options...)
}

func TestStreamWirePreservesOriginalPayloadAndOwnership(t *testing.T) {
	method := testMethods(t)["Watch"]
	original := protowire.AppendTag(nil, 2, protowire.VarintType)
	original = protowire.AppendVarint(original, 42)
	original = protowire.AppendTag(original, 1, protowire.BytesType)
	original = protowire.AppendString(original, "superseded")
	original = protowire.AppendTag(original, 1, protowire.BytesType)
	original = protowire.AppendString(original, "actual response")
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
			return err
		}
		for range 2 {
			if err := server.SendMsg(dynamicpb.NewMessage(method.Output())); err != nil {
				return err
			}
		}
		return nil
	}, grpc.ForceServerCodec(rawWireCodec{wire: original}))
	client := testClient(t, address, Options{Plaintext: true, Compression: "gzip"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.Send(request(method, "request", 0)); err != nil {
		t.Fatal(err)
	}
	first, err := stream.RecvResult()
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := proto.Marshal(first.Message)
	if err != nil || !bytes.Equal(first.Wire, original) || bytes.Equal(first.Wire, reencoded) || first.Message.Get(method.Output().Fields().ByName("text")).String() != "actual response" {
		t.Fatalf("stream payload was re-encoded: raw=%x reencoded=%x err=%v", first.Wire, reencoded, err)
	}
	first.Wire[0] = 0
	second, err := stream.RecvResult()
	if err != nil || !bytes.Equal(second.Wire, original) || first.Wire[0] != 0 {
		t.Fatalf("receive wire ownership was reused: second=%v err=%v", second, err)
	}
	if _, err := stream.RecvResult(); !errors.Is(err, io.EOF) {
		t.Fatalf("final status: %v", err)
	}
	if stream.Status().Code() != codes.OK || stream.Duration() <= 0 {
		t.Fatal("final status/duration missing")
	}
	finished := stream.Duration()
	if stream.Duration() != finished {
		t.Fatal("terminal duration kept advancing")
	}
}

func TestClientStreamResponseCardinality(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			method := testMethods(t)["Collect"]
			address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
				server.SetTrailer(metadata.Pairs("done", "cardinality"))
				for {
					err := server.RecvMsg(dynamicpb.NewMessage(method.Input()))
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return err
					}
				}
				for range count {
					if err := server.SendMsg(response(method, request(method, "result", 1))); err != nil {
						return err
					}
				}
				return nil
			})
			client := testClient(t, address, Options{Plaintext: true})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.NewStream(ctx, method, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Cancel()
			// A client stream can send zero requests before half-close.
			if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			result, err := stream.RecvResult()
			if count == 1 {
				if err != nil || result == nil {
					t.Fatalf("one response rejected: %v", err)
				}
				if _, err := stream.RecvResult(); !errors.Is(err, io.EOF) {
					t.Fatalf("one response did not terminate OK: %v", err)
				}
				if stream.Status().Code() != codes.OK {
					t.Fatal("successful client stream lost final status")
				}
			} else if result != nil || status.Code(err) != codes.Internal || stream.Status().Code() != codes.Internal {
				t.Fatalf("invalid cardinality accepted: count=%d result=%v error=%v status=%v", count, result, err, stream.Status())
			}
			// A multi-response protocol violation may be detected before the
			// peer sends trailers; only completed responses guarantee them.
			if got := stream.Trailers().Get("done"); count < 2 && (len(got) != 1 || got[0] != "cardinality") {
				t.Fatalf("cardinality response lost trailers: %v", stream.Trailers())
			}
		})
	}
}

func TestClientStreamCardinalityErrorCancelsOpenPeer(t *testing.T) {
	method := testMethods(t)["Collect"]
	peerCanceled := make(chan struct{})
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); !errors.Is(err, io.EOF) {
			return status.Error(codes.Internal, "expected request half-close")
		}
		for range 2 {
			if err := server.SendMsg(response(method, request(method, "result", 1))); err != nil {
				return err
			}
		}
		// The peer deliberately never completes the invalid RPC on its own.
		<-server.Context().Done()
		close(peerCanceled)
		return status.FromContextError(server.Context().Err()).Err()
	})
	client := testClient(t, address, Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if result, err := stream.RecvResult(); result != nil || status.Code(err) != codes.Internal || ctx.Err() != nil {
		t.Fatalf("cardinality error must finish before the RPC deadline: result=%v err=%v context=%v", result, err, ctx.Err())
	}
	select {
	case <-peerCanceled:
	case <-time.After(time.Second):
		t.Fatal("cardinality error did not cancel the peer RPC")
	}
	if stream.Status().Code() != codes.Internal {
		t.Fatalf("cancellation replaced the cardinality status: %v", stream.Status())
	}
}

func TestServerStreamAllowsZeroMessages(t *testing.T) {
	method := testMethods(t)["Watch"]
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
			return err
		}
		server.SetTrailer(metadata.Pairs("done", "empty"))
		return nil
	})
	client := testClient(t, address, Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.Send(request(method, "empty", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.RecvResult(); !errors.Is(err, io.EOF) || stream.Status().Code() != codes.OK {
		t.Fatalf("empty server stream failed: %v", err)
	}
}

func TestStreamPeerTLSAndConcurrentObservations(t *testing.T) {
	methods := testMethods(t)
	certificate, roots := testTLSCertificate(t)
	client := testClient(t, startTestServer(t, methods, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}))), Options{TLSConfig: &tls.Config{RootCAs: roots}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, methods["Chat"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	identity := stream.Peer()
	if identity == nil || identity.Addr == nil {
		t.Fatal("stream peer is missing")
	}
	info, ok := identity.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.Version < tls.VersionTLS12 || !info.State.HandshakeComplete || len(info.State.VerifiedChains) == 0 {
		t.Fatalf("stream peer TLS state missing: %v", identity.AuthInfo)
	}
	identity.Addr = nil
	if stream.Peer().Addr == nil {
		t.Fatal("caller mutated the stream's peer structure")
	}
	var observers sync.WaitGroup
	observeCtx, stop := context.WithCancel(ctx)
	defer stop()
	for range 4 {
		observers.Go(func() {
			for observeCtx.Err() == nil {
				_, _ = stream.Headers()
				_ = stream.Peer()
				_ = stream.Status()
				_ = stream.Trailers()
				_ = stream.Duration()
			}
		})
	}
	if err := stream.Send(request(methods["Chat"], "TLS", 1)); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := stream.RecvResult()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	stop()
	observers.Wait()
}

func TestStreamReceiveContinuesWhileSendIsFlowControlled(t *testing.T) {
	method := testMethods(t)["Chat"]
	responseNow := make(chan struct{})
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		if err := server.SendHeader(metadata.Pairs("server", "independent")); err != nil {
			return err
		}
		select {
		case <-responseNow:
		case <-server.Context().Done():
			return server.Context().Err()
		}
		// The server intentionally never reads client messages. Its outbound
		// direction must still work while the client's Send is flow controlled.
		if err := server.SendMsg(response(method, request(method, "independent receive", 1))); err != nil {
			return err
		}
		<-server.Context().Done()
		return server.Context().Err()
	})
	client := testClient(t, address, Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	sent := make(chan error, 1)
	go func() {
		for range 16 {
			if err := stream.Send(request(method, strings.Repeat("x", 1<<20), 1)); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	select {
	case err := <-sent:
		t.Fatalf("expected outbound flow control, got %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	closed := make(chan error, 1)
	go func() { closed <- stream.CloseSend() }()
	close(responseNow)
	message, err := stream.RecvResult()
	if err != nil || message.Message.Get(method.Output().Fields().ByName("text")).String() != "independent receive" {
		t.Fatalf("blocked Send/CloseSend also blocked receive: %v", err)
	}
	stream.Cancel()
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("canceled blocked sends succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel did not release blocked Send")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Cancel did not release queued CloseSend")
	}
	if _, err := stream.RecvResult(); status.Code(err) != codes.Canceled {
		t.Fatalf("missing canceled receive status: %v", err)
	}
}

func TestStreamSlowConsumerBackpressureAndCancel(t *testing.T) {
	method := testMethods(t)["Watch"]
	var sent atomic.Int32
	serverDone := make(chan struct{})
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		defer close(serverDone)
		if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
			return err
		}
		message := response(method, request(method, strings.Repeat("x", 1<<20), 1))
		for range 64 {
			if err := server.SendMsg(message); err != nil {
				return err
			}
			sent.Add(1)
		}
		return nil
	})
	client := testClient(t, address, Options{Plaintext: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.Send(request(method, "slow", 0)); err != nil {
		t.Fatal(err)
	}
	// With no RecvResult the transport must apply network backpressure rather
	// than collecting all 64 MiB in a package-owned read-ahead queue.
	select {
	case <-serverDone:
		t.Fatalf("server sent all messages without receive: %d", sent.Load())
	case <-time.After(40 * time.Millisecond):
	}
	if sent.Load() >= 64 {
		t.Fatal("slow consumer did not apply backpressure")
	}
	if result, err := stream.RecvResult(); err != nil || len(result.Wire) < 1<<20 {
		t.Fatalf("slow consumer lost its first response: %v", err)
	}
	stream.Cancel()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("Cancel did not release the flow-controlled server")
	}
	if _, err := stream.RecvResult(); status.Code(err) != codes.Canceled {
		t.Fatalf("slow consumer did not observe cancellation: %v", err)
	}
}

func TestStreamGzipReceiveLimitAppliesAfterDecompression(t *testing.T) {
	method := testMethods(t)["Watch"]
	address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
		if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
			return err
		}
		if err := grpc.SetSendCompressor(server.Context(), "gzip"); err != nil {
			return err
		}
		return server.SendMsg(response(method, request(method, strings.Repeat("x", 1<<16), 0)))
	})
	client := testClient(t, address, Options{Plaintext: true, MaxReceiveBytes: 1024})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, method, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if err := stream.Send(request(method, "limit", 0)); err != nil {
		t.Fatal(err)
	}
	if result, err := stream.RecvResult(); result != nil || status.Code(err) != codes.ResourceExhausted || stream.Status().Code() != codes.ResourceExhausted {
		t.Fatalf("compressed message bypassed receive limit: result=%v error=%v", result, err)
	}
}

func TestStreamCodecPreservesExtensionsAndBoundsRecursion(t *testing.T) {
	for _, deep := range []bool{false, true} {
		name := "extensions"
		if deep {
			name = "deep"
		}
		t.Run(name, func(t *testing.T) {
			file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
				Name: proto.String("stream_extensions.proto"), Package: proto.String("relay.stream.extensions"), Syntax: proto.String("proto2"),
				MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Payload"),
					Field:          []*descriptorpb.FieldDescriptorProto{{Name: proto.String("child"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".relay.stream.extensions.Payload")}},
					ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{Start: proto.Int32(100), End: proto.Int32(200)}},
				}},
				Extension: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("answer"), Extendee: proto.String(".relay.stream.extensions.Payload"), Number: proto.Int32(100), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}},
				Service:   []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Echo"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Watch"), InputType: proto.String(".relay.stream.extensions.Payload"), OutputType: proto.String(".relay.stream.extensions.Payload"), ServerStreaming: proto.Bool(true)}}}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			reg, err := registry.New([]protoreflect.FileDescriptor{file})
			if err != nil {
				t.Fatal(err)
			}
			method := file.Services().Get(0).Methods().Get(0)
			extension := dynamicpb.NewExtensionType(file.Extensions().Get(0))
			address := startStreamTestServer(t, method, func(_ any, server grpc.ServerStream) error {
				if err := server.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
					return err
				}
				message := dynamicpb.NewMessage(method.Output())
				proto.SetExtension(message, extension, int64(9223372036854775807))
				if deep {
					current := message
					for range 150 {
						next := dynamicpb.NewMessage(method.Output())
						current.Set(method.Output().Fields().Get(0), protoreflect.ValueOfMessage(next))
						current = next
					}
				}
				return server.SendMsg(message)
			})
			client := testClient(t, address, Options{Plaintext: true, Resolver: reg.Types})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.NewStream(ctx, method, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Cancel()
			if err := stream.Send(dynamicpb.NewMessage(method.Input())); err != nil {
				t.Fatal(err)
			}
			result, err := stream.RecvResult()
			if deep {
				if result != nil || status.Code(err) != codes.Internal || !strings.Contains(stream.Status().Message(), "nesting exceeds 100") {
					t.Fatalf("deep response reached formatter: result=%v error=%v", result, err)
				}
				return
			}
			if err != nil || !proto.HasExtension(result.Message, extension) || proto.GetExtension(result.Message, extension) != int64(9223372036854775807) {
				t.Fatalf("stream discarded locally imported extension: result=%v error=%v", result, err)
			}
			if _, err := stream.RecvResult(); !errors.Is(err, io.EOF) {
				t.Fatalf("extension stream did not finish: %v", err)
			}
		})
	}
}
