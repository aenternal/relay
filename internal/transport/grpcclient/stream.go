package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Stream supports all streaming flags from its method descriptor. Send and
// Recv can run concurrently. Calls in each direction are serialized. Cancel
// interrupts both directions; CloseSend only half-closes the send direction.
// Receive until EOF/error for final trailers/status, or call Cancel to release
// an abandoned stream. Messages must not be mutated after Send.
type Stream struct {
	stream  grpc.ClientStream
	method  protoreflect.MethodDescriptor
	path    string
	maxSend int
	cancel  context.CancelFunc
	done    <-chan struct{}
	started time.Time
	peer    *peer.Peer
	sendMu  sync.Mutex
	recvMu  sync.Mutex
	// receiveWire is written synchronously by the per-stream codec while
	// recvMu is held. RecvResult transfers its ownership to its caller.
	receiveWire []byte
	sent        int
	received    int
	sendClosed  bool
	stateMu     sync.Mutex
	finished    bool
	finalErr    error
	finalStatus *status.Status
	trailers    metadata.MD
	duration    time.Duration
}

// StreamMessage owns one decoded response and its original decompressed
// protobuf payload. Wire excludes the gRPC frame, HTTP/2 framing and optional
// gzip compression. It preserves field order and duplicate singular tags that
// re-marshaling Message would lose. Both values must be redacted above this
// package before displaying them, and are bounded by MaxReceiveBytes.
type StreamMessage struct {
	Message *dynamicpb.Message
	Wire    []byte
}

func (c *Client) NewStream(ctx context.Context, method protoreflect.MethodDescriptor, md metadata.MD) (*Stream, error) {
	path, err := FullMethod(method)
	if err != nil {
		return nil, err
	}
	if !method.IsStreamingClient() && !method.IsStreamingServer() {
		return nil, fmt.Errorf("gRPC stream %s: unary method requires Unary", path)
	}
	ctx, err = outgoingContext(ctx, md, c.maxMetadata)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s := &Stream{method: method, path: path, maxSend: c.maxSend, cancel: cancel, done: done, started: time.Now()}
	codec := boundedProtoCodec{resolver: c.resolver, capture: func(wire []byte) {
		s.receiveWire = append([]byte(nil), wire...)
	}}
	stream, err := c.conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: method.IsStreamingClient(), ServerStreams: method.IsStreamingServer()}, path, grpc.ForceCodecV2(codec), grpc.OnFinish(func(error) { close(done) }))
	if err != nil {
		cancel()
		return nil, rpcError("open stream", path, err)
	}
	s.stream = stream
	// Context commits this stream's selected transport. grpc-go disables retry
	// for this client; its peer and TLS identity remain fixed for the call.
	if identity, ok := peer.FromContext(stream.Context()); ok {
		copy := *identity
		s.peer = &copy
	}
	return s, nil
}

func (s *Stream) Send(message proto.Message) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendClosed {
		return fmt.Errorf("gRPC send %s: send direction is already closed", s.path)
	}
	if !s.method.IsStreamingClient() && s.sent > 0 {
		return fmt.Errorf("gRPC send %s: method accepts exactly one request", s.path)
	}
	if err := validateMessage(s.method.Input(), message, s.maxSend); err != nil {
		return fmt.Errorf("gRPC send %s: %w", s.path, err)
	}
	err := s.stream.SendMsg(message)
	if err == nil {
		s.sent++
	}
	return rpcError("send", s.path, err)
}

func (s *Stream) Recv() (*dynamicpb.Message, error) {
	result, err := s.RecvResult()
	if err != nil {
		return nil, err
	}
	return result.Message, nil
}

// RecvResult receives one message without reading ahead or retaining an
// unbounded queue. Calls are serialized independently of Send and CloseSend.
// Receive until EOF/error to publish final Status, Trailers and Duration.
// A client-streaming method must return exactly one response; zero or multiple
// responses are protocol errors even when the peer's final status is OK.
func (s *Stream) RecvResult() (*StreamMessage, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	s.stateMu.Lock()
	if s.finished {
		err := s.finalErr
		s.stateMu.Unlock()
		return nil, err
	}
	s.stateMu.Unlock()
	message := dynamicpb.NewMessage(s.method.Output())
	s.receiveWire = nil
	err := s.stream.RecvMsg(message)
	if err == nil {
		s.received++
		result := &StreamMessage{Message: message, Wire: s.receiveWire}
		s.receiveWire = nil
		return result, nil
	}
	s.receiveWire = nil
	// grpc-go enforces non-server-streaming cardinality. Keep the zero-message
	// check here too so the contract is explicit at our transport boundary.
	if errors.Is(err, io.EOF) && !s.method.IsStreamingServer() && s.received == 0 {
		err = status.Error(codes.Internal, "client-streaming RPC returned no response message")
	}
	rpcErr := rpcError("receive", s.path, err)
	finalStatus := status.Convert(err)
	if errors.Is(err, io.EOF) {
		finalStatus = status.New(codes.OK, "")
	}
	// grpc-go's client-streaming wrapper can report a cardinality violation
	// before its underlying transport has finished. Stop that transport and
	// wait for OnFinish before reading trailers, which are otherwise still
	// being written by the HTTP/2 reader. Preserve the original receive status.
	s.cancel()
	<-s.done
	s.stateMu.Lock()
	s.finished, s.finalErr, s.finalStatus = true, rpcErr, finalStatus
	s.trailers = s.stream.Trailer().Copy()
	s.duration = time.Since(s.started)
	s.stateMu.Unlock()
	return nil, rpcErr
}

func (s *Stream) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendClosed {
		return nil
	}
	s.sendClosed = true
	return rpcError("half-close", s.path, s.stream.CloseSend())
}

func (s *Stream) Cancel() { s.cancel() }

// Headers blocks until initial headers arrive or the RPC terminates.
func (s *Stream) Headers() (metadata.MD, error) {
	md, err := s.stream.Header()
	return md.Copy(), rpcError("headers", s.path, err)
}

// Trailers returns a copy after Recv returns EOF/error, otherwise nil.
func (s *Stream) Trailers() metadata.MD {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.finished {
		return nil
	}
	return s.trailers.Copy()
}

// Status returns nil until Recv observes the final status.
func (s *Stream) Status() *status.Status {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.finalStatus
}

// Peer returns an independent peer structure for this call, or nil if the
// stream ended before a transport was selected. AuthInfo and addresses are
// immutable transport snapshots, as with grpc.Peer on a unary call.
func (s *Stream) Peer() *peer.Peer {
	if s.peer == nil {
		return nil
	}
	copy := *s.peer
	return &copy
}

// Duration measures time since opening the RPC. It stops when RecvResult
// observes the final EOF/status; a live stream returns its current elapsed time.
func (s *Stream) Duration() time.Duration {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.finished {
		return s.duration
	}
	return time.Since(s.started)
}
