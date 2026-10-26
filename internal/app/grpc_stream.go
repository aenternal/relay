package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"relay/internal/domain"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/transport/grpcclient"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	grpcStreamReservation   = 16 << 20
	grpcStreamQueueBytes    = 14 << 20 // remaining 2 MiB reserve bounds status/metadata
	grpcStreamQueueMessages = 64
)

type grpcStreamRecord struct {
	ctx                                       context.Context
	prepared                                  *grpcPrepared
	transport                                 *grpcclient.Stream
	ready, closing                            bool
	sendBusy                                  chan struct{}
	commands                                  sync.WaitGroup
	securityMu                                sync.Mutex // prepared secrets/store and raw headers, never network I/O
	headers                                   metadata.MD
	queue                                     []domain.GRPCStreamMessage
	queueBytes, next, acknowledged, delivered int
	changed                                   chan struct{} // replaced under Service.grpcMu when ACK/terminal changes state
}

func (s *Service) PollGRPCStream(ctx context.Context, id string, afterSequence int) (domain.GRPCStreamBatch, error) {
	if err := ctx.Err(); err != nil {
		return domain.GRPCStreamBatch{}, err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	record, ok := s.initGRPC().records[id]
	if !ok || record.stream == nil {
		return domain.GRPCStreamBatch{}, errors.New("gRPC stream is no longer available")
	}
	stream := record.stream
	if afterSequence < 0 || afterSequence > stream.delivered {
		return domain.GRPCStreamBatch{}, errors.New("invalid gRPC stream cursor")
	}
	batch := domain.GRPCStreamBatch{Execution: cloneGRPCExecution(record.result), Messages: []domain.GRPCStreamMessage{}, NextSequence: max(afterSequence, stream.acknowledged)}
	bytes := 0
	for _, message := range stream.queue {
		if message.Sequence <= afterSequence {
			continue
		}
		size := grpcStreamMessageSize(message)
		if len(batch.Messages) >= 32 || len(batch.Messages) > 0 && bytes+size > 1<<20 {
			break
		}
		message.Warnings = slices.Clone(message.Warnings)
		batch.Messages = append(batch.Messages, message)
		bytes += size
		batch.NextSequence = message.Sequence
	}
	stream.delivered = max(stream.delivered, batch.NextSequence)
	return batch, nil
}

func (s *Service) AckGRPCStream(ctx context.Context, id string, sequence int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	record, ok := s.initGRPC().records[id]
	if !ok || record.stream == nil {
		return errors.New("gRPC stream is no longer available")
	}
	stream := record.stream
	if sequence < 0 || sequence > stream.delivered {
		return errors.New("cannot acknowledge an undelivered gRPC stream message")
	}
	if sequence <= stream.acknowledged {
		return nil
	}
	count := 0
	for count < len(stream.queue) && stream.queue[count].Sequence <= sequence {
		stream.queueBytes -= grpcStreamMessageSize(stream.queue[count])
		stream.queue[count] = domain.GRPCStreamMessage{}
		count++
	}
	stream.queue = stream.queue[count:]
	stream.acknowledged = sequence
	stream.wake()
	return nil
}

func (stream *grpcStreamRecord) wake() { close(stream.changed); stream.changed = make(chan struct{}) }

func (s *Service) beginGRPCStreamCommand(ctx context.Context, id string) (*grpcRecord, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	record, ok := s.initGRPC().records[id]
	if !ok || record.stream == nil {
		return nil, nil, errors.New("gRPC stream is no longer available")
	}
	stream := record.stream
	if record.result.State != "running" || stream.closing || stream.ctx.Err() != nil {
		return nil, nil, errors.New("gRPC stream is closed")
	}
	if !stream.ready {
		return nil, nil, errors.New("gRPC stream is still connecting")
	}
	if record.result.RPCType == "server" {
		return nil, nil, errors.New("server-streaming RPC sends its single request automatically")
	}
	if record.result.StreamState == "half_closed" {
		return nil, nil, errors.New("gRPC send direction is already closed")
	}
	select {
	case stream.sendBusy <- struct{}{}:
	default:
		return nil, nil, errors.New("another gRPC send or half-close is in progress")
	}
	stream.commands.Add(1)
	return record, func() { <-stream.sendBusy; stream.commands.Done() }, nil
}

func (s *Service) SendGRPCStreamMessage(ctx context.Context, id, body, format string) error {
	record, done, err := s.beginGRPCStreamCommand(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	stream := record.stream
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(stream.ctx, cancel)
	defer func() { stop(); cancel() }()
	stream.securityMu.Lock()
	previousSecrets := len(stream.prepared.secrets)
	message, err := stream.prepared.decodeBody(work, body, format)
	secrets := slices.Clone(stream.prepared.secrets)
	// Newly used keyring values also sanitize previews still waiting for ACK.
	// Do not process existing redaction markers again for unchanged credentials.
	if len(secrets) != previousSecrets {
		s.grpcMu.Lock()
		for index := range stream.queue {
			before := grpcStreamMessageSize(stream.queue[index])
			message := sanitizeGRPCStreamMessage(stream.queue[index], secrets)
			if stream.queueBytes-before+grpcStreamMessageSize(message) > grpcStreamQueueBytes {
				message.MessageJSON, message.WireBase64, message.Truncated = "", "", true
				message.Warnings = []string{"Message preview was omitted after sensitive values were redacted"}
			}
			stream.queue[index] = message
			stream.queueBytes += grpcStreamMessageSize(message) - before
		}
		record.result.Headers = grpcSafeMetadata(stream.headers, secrets)
		record.result.Endpoint = grpcSafeText(record.result.Endpoint, secrets, 65536)
		if peer := stream.transport.Peer(); peer != nil {
			if info, ok := peer.AuthInfo.(credentials.TLSInfo); ok {
				record.result.TLS = safeHTTPTLS(&info.State, secrets)
			}
		}
		s.grpcMu.Unlock()
	}
	stream.securityMu.Unlock()
	if err != nil {
		return errors.New(grpcSafeText(err.Error(), secrets, 4096))
	}
	if err := work.Err(); err != nil {
		return err
	}
	if err := stream.transport.Send(message); err != nil {
		return errors.New(grpcSafeText(err.Error(), secrets, 4096))
	}
	s.grpcMu.Lock()
	record.result.SentCount++
	index := record.result.SentCount
	s.grpcMu.Unlock()
	wire, _ := proto.Marshal(message)
	preview := grpcStreamPreview(stream.prepared, message, wire, "sent", index)
	// A successful network write stays successful if Cancel releases a blocked
	// preview producer; the final counters still record the actual write.
	_ = s.enqueueGRPCStream(stream.ctx, record, preview)
	return nil
}

func (s *Service) HalfCloseGRPCStream(ctx context.Context, id string) error {
	record, done, err := s.beginGRPCStreamCommand(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	if err := record.stream.transport.CloseSend(); err != nil {
		record.stream.securityMu.Lock()
		safe := grpcSafeText(err.Error(), record.stream.prepared.secrets, 4096)
		record.stream.securityMu.Unlock()
		return errors.New(safe)
	}
	s.grpcMu.Lock()
	if record.result.State == "running" && !record.stream.closing {
		record.result.StreamState = "half_closed"
	}
	s.grpcMu.Unlock()
	return nil
}

func (s *Service) runGRPCStream(ctx context.Context, request domain.RequestDefinition, record *grpcRecord) {
	started := time.Now()
	stream := record.stream
	var prepared *grpcPrepared
	var transport *grpcclient.Stream
	var failure error
	defer func() {
		contextErr := ctx.Err()
		hadFinalStatus := transport != nil && transport.Status() != nil
		s.grpcMu.Lock()
		stream.closing, stream.ready = true, false
		s.grpcMu.Unlock()
		record.cancel()
		if transport != nil {
			transport.Cancel()
		}
		stream.commands.Wait()
		// grpc-go can retain DATA ahead of the cancellation error. A bounded
		// best-effort drain observes already available peer status/trailers without
		// turning Cancel into an unbounded decode of buffered empty messages.
		discarded, discardedBytes := 0, 0
		if transport != nil && transport.Status() == nil {
			for discarded < 128 && discardedBytes < 4<<20 {
				message, err := transport.RecvResult()
				if err != nil {
					break
				}
				discarded++
				discardedBytes += len(message.Wire)
			}
		}
		stream.securityMu.Lock()
		defer stream.securityMu.Unlock()
		secrets := []string{}
		if prepared != nil {
			secrets = prepared.secrets
		}
		s.grpcMu.Lock()
		result := cloneGRPCExecution(record.result)
		s.grpcMu.Unlock()
		if discarded > 0 {
			result.ReceivedCount += discarded
			result.Warnings = append(result.Warnings, "Additional buffered messages were discarded while closing the stream")
		}
		result.StreamState = "closed"
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		result.TotalMS = float64(time.Since(started)) / float64(time.Millisecond)
		if transport != nil {
			result.Headers = grpcSafeMetadata(stream.headers, secrets)
			result.Trailers = grpcSafeMetadata(transport.Trailers(), secrets)
			if peer := transport.Peer(); peer != nil {
				if info, ok := peer.AuthInfo.(credentials.TLSInfo); ok {
					result.TLS = safeHTTPTLS(&info.State, secrets)
				}
			}
			if rpcStatus := transport.Status(); rpcStatus != nil && (hadFinalStatus || contextErr != nil || failure == nil) {
				result.Code, result.CodeName = int(rpcStatus.Code()), rpcStatus.Code().String()
				result.StatusMessage = grpcSafeText(rpcStatus.Message(), secrets, 16384)
				result.Details, result.Truncated = grpcStatusDetails(prepared.registry, rpcStatus, secrets)
			}
		}
		result.State = "completed"
		if failure != nil {
			result.State = "failed"
			if result.Code == 0 {
				result.Code, result.CodeName = int(status.Code(failure)), status.Code(failure).String()
			}
			result.Error = grpcSafeText(failure.Error(), secrets, 4096)
		}
		if contextErr != nil {
			if errors.Is(contextErr, context.DeadlineExceeded) {
				result.State = "failed"
				result.Code, result.CodeName = int(codes.DeadlineExceeded), codes.DeadlineExceeded.String()
				result.Error = "gRPC operation exceeded its deadline"
			} else {
				result.State = "canceled"
				result.Code, result.CodeName = int(codes.Canceled), codes.Canceled.String()
				result.Error = "gRPC operation canceled"
			}
		}
		if result.CodeName == "" {
			result.CodeName = codes.OK.String()
		}
		s.grpcMu.Lock()
		record.result = result
		stream.wake()
		s.grpcMu.Unlock()
		s.grpcState.work.Done()
	}()
	prepared, failure = s.prepareGRPCMode(ctx, request, "stream")
	stream.prepared = prepared
	if failure != nil {
		s.grpcMu.Lock()
		record.result.Code, record.result.CodeName = int(codes.InvalidArgument), codes.InvalidArgument.String()
		s.grpcMu.Unlock()
		return
	}
	rpcType := "server"
	if prepared.method.IsStreamingClient() {
		rpcType = "client"
		if prepared.method.IsStreamingServer() {
			rpcType = "bidi"
		}
	}
	s.grpcMu.Lock()
	record.result.Endpoint = grpcSafeText(prepared.endpoint, prepared.secrets, 65536)
	record.result.Method, _ = grpcclient.FullMethod(prepared.method)
	record.result.SchemaID, record.result.RPCType = request.GRPC.SchemaID, rpcType
	if prepared.options.Plaintext {
		record.result.Warnings = append(record.result.Warnings, "This gRPC connection uses plaintext")
		if request.GRPC.TLS != (domain.HTTPTLS{}) {
			record.result.Warnings = append(record.result.Warnings, "Configured TLS settings are inactive in plaintext mode")
		}
	} else if prepared.options.TLSConfig.InsecureSkipVerify {
		record.result.Warnings = append(record.result.Warnings, "TLS certificate verification is disabled for this request")
	}
	s.grpcMu.Unlock()
	client, err := grpcclient.Dial(ctx, prepared.endpoint, prepared.options)
	if err != nil {
		failure = err
		s.grpcMu.Lock()
		record.result.Code, record.result.CodeName = int(codes.Unavailable), codes.Unavailable.String()
		s.grpcMu.Unlock()
		return
	}
	defer client.Close()
	s.grpcMu.Lock()
	record.result.ConnectMS = float64(client.ConnectDuration()) / float64(time.Millisecond)
	s.grpcMu.Unlock()
	transport, failure = client.NewStream(ctx, prepared.method, prepared.metadata)
	if failure != nil {
		return
	}
	s.grpcMu.Lock()
	stream.transport, stream.ready, record.result.StreamState = transport, true, "open"
	stream.commands.Add(1)
	s.grpcMu.Unlock()
	go func() {
		defer stream.commands.Done()
		headers, _ := transport.Headers()
		stream.securityMu.Lock()
		stream.headers = headers
		safe := grpcSafeMetadata(headers, prepared.secrets)
		var tls *domain.HTTPConnectionTLS
		if peer := transport.Peer(); peer != nil {
			if info, ok := peer.AuthInfo.(credentials.TLSInfo); ok {
				tls = safeHTTPTLS(&info.State, prepared.secrets)
			}
		}
		s.grpcMu.Lock()
		record.result.Headers = safe
		record.result.TLS = tls
		s.grpcMu.Unlock()
		stream.securityMu.Unlock()
	}()
	if rpcType == "server" {
		sendErr := transport.Send(prepared.message)
		if sendErr != nil && !errors.Is(sendErr, io.EOF) {
			failure = sendErr
			return
		}
		// EOF is an early peer finish, not a successful write. Read its final
		// status below without inventing a sent message or losing rich details.
		if sendErr == nil {
			s.grpcMu.Lock()
			record.result.SentCount++
			index := record.result.SentCount
			s.grpcMu.Unlock()
			wire, _ := proto.Marshal(prepared.message)
			if failure = s.enqueueGRPCStream(ctx, record, grpcStreamPreview(prepared, prepared.message, wire, "sent", index)); failure != nil {
				return
			}
			if failure = transport.CloseSend(); failure != nil {
				return
			}
			s.grpcMu.Lock()
			record.result.StreamState = "half_closed"
			s.grpcMu.Unlock()
		}
	}
	for {
		message, err := transport.RecvResult()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			failure = err
			return
		}
		s.grpcMu.Lock()
		record.result.ReceivedCount++
		index := record.result.ReceivedCount
		s.grpcMu.Unlock()
		if failure = s.enqueueGRPCStream(ctx, record, grpcStreamPreview(prepared, message.Message, message.Wire, "received", index)); failure != nil {
			return
		}
	}
}

func grpcStreamPreview(prepared *grpcPrepared, message proto.Message, wire []byte, direction string, index int) domain.GRPCStreamMessage {
	preview := domain.GRPCStreamMessage{Direction: direction, Index: index, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), WireBytes: len(wire), WireBase64: base64.StdEncoding.EncodeToString(wire), Warnings: []string{}}
	encoded, err := jsoncodec.Encode(prepared.registry, message)
	if err != nil {
		preview.Warnings = append(preview.Warnings, "Message ProtoJSON is unavailable; inspect its binary preview or import its missing Any types")
	} else {
		preview.MessageJSON = string(encoded)
	}
	return preview
}

func sanitizeGRPCStreamMessage(message domain.GRPCStreamMessage, secrets []string) domain.GRPCStreamMessage {
	message.Warnings = slices.Clone(message.Warnings)
	redacted := redactHTTP(message.MessageJSON, secrets, jsoncodec.MaxOutputBytes+1)
	sensitive := redacted != message.MessageJSON
	message.MessageJSON = redacted
	if len(redacted) > jsoncodec.MaxOutputBytes || redacted != "" && !json.Valid([]byte(redacted)) {
		message.MessageJSON = ""
		message.Truncated = true
		sensitive = true
	}
	if !sensitive && message.WireBase64 != "" {
		wire, _ := base64.StdEncoding.DecodeString(message.WireBase64)
		sensitive = grpcContainsSecret(wire, secrets)
	}
	if sensitive && message.WireBase64 != "" {
		message.WireBase64 = ""
		message.Warnings = append(message.Warnings, "Sensitive values were redacted; binary preview is unavailable")
	}
	return message
}

func grpcStreamMessageSize(message domain.GRPCStreamMessage) int {
	bytes := len(message.MessageJSON) + len(message.WireBase64) + len(message.Timestamp) + 128
	for _, warning := range message.Warnings {
		bytes += len(warning)
	}
	return bytes
}

func (s *Service) enqueueGRPCStream(ctx context.Context, record *grpcRecord, raw domain.GRPCStreamMessage) error {
	stream := record.stream
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		stream.securityMu.Lock()
		message := sanitizeGRPCStreamMessage(raw, stream.prepared.secrets)
		size := grpcStreamMessageSize(message)
		if size > grpcStreamQueueBytes {
			stream.securityMu.Unlock()
			return errors.New("gRPC message preview exceeds the stream history limit")
		}
		s.grpcMu.Lock()
		if len(stream.queue) < grpcStreamQueueMessages && stream.queueBytes+size <= grpcStreamQueueBytes {
			stream.next++
			message.Sequence = stream.next
			stream.queue = append(stream.queue, message)
			stream.queueBytes += size
			s.grpcMu.Unlock()
			stream.securityMu.Unlock()
			return nil
		}
		changed := stream.changed
		s.grpcMu.Unlock()
		stream.securityMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
