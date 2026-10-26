package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/protobuf/importer"
	"relay/internal/security"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const grpcStreamSource = `syntax="proto3"; package stage5;
message Message { string text=1; int64 count=2; bytes data=3; }
message Failure { string reason=1; }
service Echo {
 rpc Watch(Message) returns(stream Message);
 rpc Upload(stream Message) returns(Message);
 rpc Chat(stream Message) returns(stream Message);
 rpc Reject(Message) returns(stream Message);
}`

type grpcStreamFixture struct {
	address         string
	calls, canceled atomic.Int32
	entered         chan string
}

func startGRPCStreamFixture(t *testing.T) *grpcStreamFixture {
	t.Helper()
	reg, err := importer.CompileSources(t.Context(), map[string]string{"stage5.proto": grpcStreamSource}, []string{"stage5.proto"})
	if err != nil {
		t.Fatal(err)
	}
	method, _ := reg.Method("/stage5.Echo/Watch")
	descriptor := method.Input()
	text, count := descriptor.Fields().ByName("text"), descriptor.Fields().ByName("count")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &grpcStreamFixture{address: listener.Addr().String(), entered: make(chan string, 32)}
	server := grpc.NewServer()
	failure := func(reason string) error {
		messageType, _ := reg.Types.FindMessageByName("stage5.Failure")
		detail := dynamicpb.NewMessage(messageType.Descriptor())
		detail.Set(detail.Descriptor().Fields().ByName("reason"), protoreflect.ValueOfString(reason))
		st, _ := status.New(codes.PermissionDenied, "denied "+reason).WithDetails(detail)
		return st.Err()
	}
	handler := func(kind string) grpc.StreamHandler {
		return func(_ any, stream grpc.ServerStream) error {
			fixture.calls.Add(1)
			address := ""
			if p, ok := peer.FromContext(stream.Context()); ok {
				address = p.Addr.String()
			}
			if err := stream.SendHeader(metadata.Pairs("stream-kind", kind, "connection", address)); err != nil {
				return err
			}
			stream.SetTrailer(metadata.Pairs("done", kind))
			fixture.entered <- kind
			if kind == "Reject" {
				return status.Error(codes.Unauthenticated, "authentication required")
			}
			if kind == "Watch" {
				input := dynamicpb.NewMessage(descriptor)
				if err := stream.RecvMsg(input); err != nil {
					return err
				}
				value := input.Get(text).String()
				if value == "wait" {
					<-stream.Context().Done()
					fixture.canceled.Add(1)
					return status.FromContextError(stream.Context().Err()).Err()
				}
				if strings.HasPrefix(value, "fail:") {
					return failure(strings.TrimPrefix(value, "fail:"))
				}
				for index := int64(0); index < input.Get(count).Int(); index++ {
					message := dynamicpb.NewMessage(descriptor)
					value := fmt.Sprintf("item-%d", index)
					if input.Get(text).String() == "large" {
						value = strings.Repeat("x", 512<<10)
					}
					message.Set(text, protoreflect.ValueOfString(value))
					message.Set(count, protoreflect.ValueOfInt64(index))
					if err := stream.SendMsg(message); err != nil {
						if stream.Context().Err() != nil {
							fixture.canceled.Add(1)
						}
						return err
					}
				}
				return nil
			}
			n := int64(0)
			for {
				input := dynamicpb.NewMessage(descriptor)
				err := stream.RecvMsg(input)
				if err == io.EOF {
					if kind == "Chat" {
						return nil
					}
					output := dynamicpb.NewMessage(descriptor)
					output.Set(text, protoreflect.ValueOfString("uploaded"))
					output.Set(count, protoreflect.ValueOfInt64(n))
					return stream.SendMsg(output)
				}
				if err != nil {
					if stream.Context().Err() != nil {
						fixture.canceled.Add(1)
					}
					return err
				}
				n++
				value := input.Get(text).String()
				if strings.HasPrefix(value, "fail:") {
					return failure(strings.TrimPrefix(value, "fail:"))
				}
				if kind == "Chat" {
					if err := stream.SendMsg(proto.Clone(input)); err != nil {
						return err
					}
				}
			}
		}
	}
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "stage5.Echo", HandlerType: (*grpcAppService)(nil), Metadata: "stage5.proto", Streams: []grpc.StreamDesc{
		{StreamName: "Watch", ServerStreams: true, Handler: handler("Watch")},
		{StreamName: "Upload", ClientStreams: true, Handler: handler("Upload")},
		{StreamName: "Chat", ClientStreams: true, ServerStreams: true, Handler: handler("Chat")},
		{StreamName: "Reject", ServerStreams: true, Handler: handler("Reject")},
	}}, struct{}{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return fixture
}

func newGRPCStreamApp(t *testing.T, fixture *grpcStreamFixture, kind string) (*app.Service, domain.RequestDefinition, string) {
	return newGRPCStreamAppStore(t, fixture, kind, security.NewMemoryStore())
}

func newGRPCStreamAppStore(t *testing.T, fixture *grpcStreamFixture, kind string, vault security.Store) (*app.Service, domain.RequestDefinition, string) {
	t.Helper()
	dir := t.TempDir()
	_, svc := openApp(t, filepath.Join(dir, "stream.db"), vault)
	t.Cleanup(func() { _ = app.ShutdownGRPC(svc) })
	state := loadState(t, svc)
	imported := importSchema(t, svc, app.ProtoImport{WorkspaceID: state.Workspaces[0].ID, Name: "Streams", Kind: "proto", RootFiles: []string{"stage5.proto"}, Sources: []app.ProtoSource{{Name: "stage5.proto", Content: grpcStreamSource}}})
	request := state.Requests[0]
	request.Protocol, request.HTTP = "grpc", nil
	request.GRPC = &domain.GRPCDefinition{Endpoint: fixture.address, Plaintext: true, SchemaID: imported.SchemaID, Method: "/stage5.Echo/" + kind, BodyFormat: "json", Body: `{}`, TimeoutMS: 10000}
	return svc, request, dir
}

func startGRPCStream(t *testing.T, svc *app.Service, request domain.RequestDefinition) domain.GRPCExecution {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	result, err := svc.StartGRPCStream(ctx, request)
	cancel() // the bridge context must not own the long-lived RPC
	if err != nil {
		t.Fatal(err)
	}
	return waitGRPCStream(t, svc, result.ID, func(result domain.GRPCExecution) bool { return result.StreamState != "connecting" })
}

func waitGRPCStream(t *testing.T, svc *app.Service, id string, match func(domain.GRPCExecution) bool) domain.GRPCExecution {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		result, err := svc.GetGRPCExecution(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if match(result) {
			return result
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("stream condition did not become true")
	return domain.GRPCExecution{}
}

func drainGRPCStream(t *testing.T, svc *app.Service, id string) (domain.GRPCExecution, []domain.GRPCStreamMessage) {
	t.Helper()
	sequence := 0
	messages := []domain.GRPCStreamMessage{}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := svc.PollGRPCStream(t.Context(), id, sequence)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range batch.Messages {
			if message.Sequence <= sequence {
				t.Fatalf("duplicate or unordered message: %+v", message)
			}
			sequence = message.Sequence
			messages = append(messages, message)
		}
		if batch.NextSequence != sequence {
			t.Fatalf("unexpected cursor: %+v", batch)
		}
		if err := svc.AckGRPCStream(t.Context(), id, sequence); err != nil {
			t.Fatal(err)
		}
		if batch.Execution.State != "running" && len(batch.Messages) == 0 {
			return batch.Execution, messages
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("stream did not drain")
	return domain.GRPCExecution{}, nil
}

func TestGRPCServerStreamZeroAndManyDrainAfterTerminal(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, dir := newGRPCStreamApp(t, fixture, "Watch")
	for _, count := range []int{0, 7} {
		request.GRPC.Body = fmt.Sprintf(`{"text":"start","count":%d}`, count)
		started := startGRPCStream(t, svc, request)
		terminal := awaitGRPC(t, svc, started.ID)
		if terminal.State != "completed" || terminal.StreamState != "closed" || terminal.RPCType != "server" || terminal.SentCount != 1 || terminal.ReceivedCount != count {
			t.Fatalf("bad terminal: %+v", terminal)
		}
		result, messages := drainGRPCStream(t, svc, started.ID)
		if len(messages) != count+1 || result.CodeName != "OK" || len(result.Headers) == 0 || strings.Join(grpcHeader(result.Trailers, "done"), "") != "Watch" {
			t.Fatalf("missing messages/status/metadata: %+v %+v", result, messages)
		}
		for _, message := range messages {
			if message.MessageJSON == "" || message.WireBase64 == "" || message.WireBytes == 0 {
				t.Fatalf("missing message preview: %+v", message)
			}
		}
		if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{}`, "json"); err == nil {
			t.Fatal("late send accepted")
		}
		if err := svc.HalfCloseGRPCStream(t.Context(), started.ID); err == nil {
			t.Fatal("late half-close accepted")
		}
		if err := svc.ForgetGRPCExecution(t.Context(), started.ID); err != nil {
			t.Fatal(err)
		}
	}
	assertNoSecretOnDisk(t, dir, "item-0", "start")
}

func TestGRPCClientStreamExplicitSendsAndZeroMessageHalfClose(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Upload")
	request.GRPC.Body = "invalid ignored initial body"
	for _, count := range []int{0, 3} {
		started := startGRPCStream(t, svc, request)
		if started.RPCType != "client" || started.State != "running" {
			t.Fatalf("client stream did not open: %+v", started)
		}
		for index := range count {
			if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, fmt.Sprintf(`{text:'sent-%d',count:9223372036854775807,}`, index), "json5"); err != nil {
				t.Fatal(err)
			}
		}
		if err := svc.HalfCloseGRPCStream(t.Context(), started.ID); err != nil {
			t.Fatal(err)
		}
		result, messages := drainGRPCStream(t, svc, started.ID)
		if result.State != "completed" || result.SentCount != count || result.ReceivedCount != 1 || len(messages) != count+1 || !strings.Contains(messages[len(messages)-1].MessageJSON, "uploaded") {
			t.Fatalf("client result: %+v %+v", result, messages)
		}
		if count > 0 && !strings.Contains(messages[0].MessageJSON, "9223372036854775807") {
			t.Fatal("int64 lexeme changed")
		}
	}
}

func TestGRPCBidiInteractiveSameConnectionAndNewSecrets(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, dir := newGRPCStreamApp(t, fixture, "Chat")
	_, envID, variableID := addToken(t, svc)
	const secret = "stream-secret-late-81372"
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, secret))
	started := startGRPCStream(t, svc, request)
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"`+secret+`"}`, "json"); err != nil {
		t.Fatal(err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount == 1 && len(result.Headers) > 0 })
	// The second message is sent only after the first response, on the same RPC.
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"{{token}}"}`, "json"); err != nil {
		t.Fatal(err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount == 2 })
	if fixture.calls.Load() != 1 {
		t.Fatal("interactive send opened another RPC")
	}
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"fail:{{token}}"}`, "json"); err != nil {
		t.Fatal(err)
	}
	result, messages := drainGRPCStream(t, svc, started.ID)
	if result.Code != int(codes.PermissionDenied) || result.State != "failed" || len(result.Details) != 1 || len(messages) < 4 {
		t.Fatalf("bidi final state missing: %+v %+v", result, messages)
	}
	encoded, _ := json.Marshal(struct {
		Result   domain.GRPCExecution
		Messages []domain.GRPCStreamMessage
	}{result, messages})
	if bytes.Contains(encoded, []byte(secret)) || !bytes.Contains(encoded, []byte(security.RedactedValue)) {
		t.Fatalf("late secret leaked or not redacted: %s", encoded)
	}
	for _, message := range messages {
		if strings.Contains(message.MessageJSON, security.RedactedValue) && message.WireBase64 != "" {
			t.Fatal("sensitive raw binary exposed")
		}
	}
	assertNoSecretOnDisk(t, dir, secret)
}

func TestGRPCBidiZeroMessageHalfCloseAndEarlyPeerRejection(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Chat")
	started := startGRPCStream(t, svc, request)
	if err := svc.HalfCloseGRPCStream(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	result, messages := drainGRPCStream(t, svc, started.ID)
	if result.State != "completed" || result.SentCount != 0 || result.ReceivedCount != 0 || len(messages) != 0 {
		t.Fatalf("empty bidi stream: %+v %+v", result, messages)
	}
	request.GRPC.Method = "/stage5.Echo/Reject"
	started = startGRPCStream(t, svc, request)
	result, _ = drainGRPCStream(t, svc, started.ID)
	if result.State != "failed" || result.Code != int(codes.Unauthenticated) || result.StatusMessage != "authentication required" || strings.Join(grpcHeader(result.Trailers, "done"), "") != "Reject" {
		t.Fatalf("early peer status/trailers lost: %+v", result)
	}
}

func TestGRPCStreamACKBackpressureAndCancelWake(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Watch")
	request.GRPC.Body = `{"count":200}`
	started := startGRPCStream(t, svc, request)
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount >= 64 })
	time.Sleep(30 * time.Millisecond)
	blocked, _ := svc.GetGRPCExecution(t.Context(), started.ID)
	if blocked.State != "running" || blocked.ReceivedCount > 65 {
		t.Fatalf("missing bounded receiver backpressure: %+v", blocked)
	}
	if err := svc.AckGRPCStream(t.Context(), started.ID, 1); err == nil {
		t.Fatal("undelivered ACK accepted")
	}
	batch, err := svc.PollGRPCStream(t.Context(), started.ID, 0)
	if err != nil || len(batch.Messages) != 32 {
		t.Fatalf("batch bound: %d %v", len(batch.Messages), err)
	}
	if _, err := svc.PollGRPCStream(t.Context(), started.ID, batch.NextSequence+1); err == nil {
		t.Fatal("future cursor accepted")
	}
	if err := svc.AckGRPCStream(t.Context(), started.ID, batch.NextSequence); err != nil {
		t.Fatal(err)
	}
	if err := svc.AckGRPCStream(t.Context(), started.ID, batch.NextSequence); err != nil {
		t.Fatal("duplicate ACK failed", err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount > blocked.ReceivedCount })
	if err := svc.CancelGRPCExecution(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	terminal := awaitGRPC(t, svc, started.ID)
	if terminal.State != "canceled" || terminal.CodeName != "Canceled" {
		t.Fatalf("cancel did not wake blocked receiver: %+v", terminal)
	}
	// Pending messages remain drainable after cancellation; cursor restart only
	// sees the unacknowledged suffix, never the first 32 messages again.
	after, err := svc.PollGRPCStream(t.Context(), started.ID, 0)
	if err != nil || len(after.Messages) == 0 || after.Messages[0].Sequence <= batch.NextSequence {
		t.Fatalf("terminal drain lost ACK floor: %+v %v", after, err)
	}
}

func TestGRPCStreamDrainBackpressureToEOF(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Watch")
	request.GRPC.Body = `{"count":180}`
	started := startGRPCStream(t, svc, request)
	result, messages := drainGRPCStream(t, svc, started.ID)
	if result.State != "completed" || result.ReceivedCount != 180 || len(messages) != 181 {
		t.Fatalf("ACK drain incomplete: %+v %d", result, len(messages))
	}
	for index, message := range messages {
		if message.Sequence != index+1 {
			t.Fatal("sequence gap")
		}
	}
}

func TestGRPCStreamByteBudgetLargeBatchAndDeadline(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Watch")
	request.GRPC.Body = `{"text":"large","count":80}`
	started := startGRPCStream(t, svc, request)
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount >= 10 })
	time.Sleep(30 * time.Millisecond)
	bounded, _ := svc.GetGRPCExecution(t.Context(), started.ID)
	if bounded.State != "running" || bounded.ReceivedCount >= 30 {
		t.Fatalf("byte backpressure missing: %+v", bounded)
	}
	batch, err := svc.PollGRPCStream(t.Context(), started.ID, 0)
	if err != nil || len(batch.Messages) > 2 {
		t.Fatalf("byte-bounded poll: %d %v", len(batch.Messages), err)
	}
	_ = svc.CancelGRPCExecution(t.Context(), started.ID)
	awaitGRPC(t, svc, started.ID)
	_ = svc.ForgetGRPCExecution(t.Context(), started.ID)
	request.GRPC.Body, request.GRPC.TimeoutMS = `{"text":"wait"}`, 40
	started = startGRPCStream(t, svc, request)
	result := awaitGRPC(t, svc, started.ID)
	if result.State != "failed" || result.Code != int(codes.DeadlineExceeded) {
		t.Fatalf("stream deadline lost: %+v", result)
	}
}

func TestGRPCStreamPendingTerminalHistoryPreventsEviction(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Watch")
	ids := []string{}
	for range 4 {
		started := startGRPCStream(t, svc, request)
		awaitGRPC(t, svc, started.ID)
		ids = append(ids, started.ID)
	}
	if _, err := svc.StartGRPCStream(t.Context(), request); err == nil {
		t.Fatal("pending terminal history was evicted")
	}
	for _, id := range ids {
		if _, err := svc.GetGRPCExecution(t.Context(), id); err != nil {
			t.Fatal("pending result disappeared", err)
		}
	}
	drainGRPCStream(t, svc, ids[0])
	started := startGRPCStream(t, svc, request)
	awaitGRPC(t, svc, started.ID)
	if _, err := svc.GetGRPCExecution(t.Context(), ids[0]); err == nil {
		t.Fatal("fully acknowledged result was not eligible for eviction")
	}
	if err := app.ShutdownGRPC(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetGRPCExecution(t.Context(), started.ID); err == nil {
		t.Fatal("shutdown retained stream records")
	}
}

func TestGRPCStreamEnvironmentSnapshotAndRejectedMessageKeepsConnection(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Chat")
	state, envID, variableID := addToken(t, svc)
	env := state.Environments[0]
	for index := range env.Variables {
		if env.Variables[index].ID == variableID {
			env.Variables[index].Secret, env.Variables[index].SecretRef, env.Variables[index].Value = false, "", "before-snapshot"
		}
	}
	mustState(t)(svc.SaveEnvironment(t.Context(), env))
	started := startGRPCStream(t, svc, request)
	for index := range env.Variables {
		if env.Variables[index].ID == variableID {
			env.Variables[index].Value = "after-snapshot"
		}
	}
	if env.ID != envID {
		t.Fatal("fixture environment mismatch")
	}
	mustState(t)(svc.SaveEnvironment(t.Context(), env))
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"count":"invalid"}`, "json"); err == nil {
		t.Fatal("invalid ProtoJSON was sent")
	}
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{text:'{{token}}',}`, "json5"); err != nil {
		t.Fatal(err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount == 1 })
	if err := svc.HalfCloseGRPCStream(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	result, messages := drainGRPCStream(t, svc, started.ID)
	if result.State != "completed" || result.SentCount != 1 || result.ReceivedCount != 1 || fixture.calls.Load() != 1 {
		t.Fatalf("validation changed connection: %+v", result)
	}
	for _, message := range messages {
		if !strings.Contains(message.MessageJSON, "before-snapshot") || strings.Contains(message.MessageJSON, "after-snapshot") {
			t.Fatalf("environment mutated after start: %+v", message)
		}
	}
}

type grpcStreamGateStore struct {
	security.Store
	gate    atomic.Bool
	entered chan struct{}
}

func (store *grpcStreamGateStore) Get(ctx context.Context, key string) (string, error) {
	if store.gate.Load() {
		select {
		case store.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	return store.Store.Get(ctx, key)
}

func TestGRPCStreamConcurrentWritesRejectAndCancelBlockedSecretLookup(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	vault := &grpcStreamGateStore{Store: security.NewMemoryStore(), entered: make(chan struct{}, 1)}
	svc, request, _ := newGRPCStreamAppStore(t, fixture, "Chat", vault)
	_, envID, variableID := addToken(t, svc)
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, "blocked-stream-token"))
	started := startGRPCStream(t, svc, request)
	vault.gate.Store(true)
	sent := make(chan error, 1)
	go func() { sent <- svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"{{token}}"}`, "json") }()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("Send did not enter keyring lookup")
	}
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{}`, "json"); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("concurrent Send was queued: %v", err)
	}
	if err := svc.HalfCloseGRPCStream(t.Context(), started.ID); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("concurrent half-close was queued: %v", err)
	}
	if err := svc.CancelGRPCExecution(t.Context(), started.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("canceled lookup unexpectedly sent a message")
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel did not release blocked command")
	}
	result := awaitGRPC(t, svc, started.ID)
	if result.State != "canceled" || result.SentCount != 0 {
		t.Fatalf("blocked command result: %+v", result)
	}
	if err := app.ShutdownGRPC(svc); err != nil {
		t.Fatal(err)
	}
}

func TestGRPCStreamRepeatedSecretSendsDoNotRemaskPendingMarkers(t *testing.T) {
	fixture := startGRPCStreamFixture(t)
	svc, request, _ := newGRPCStreamApp(t, fixture, "Chat")
	_, envID, variableID := addToken(t, svc)
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, "E"))
	started := startGRPCStream(t, svc, request)
	pollMessages := func(count int) domain.GRPCStreamBatch {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			batch, err := svc.PollGRPCStream(t.Context(), started.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Messages) == count {
				return batch
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("expected %d pending previews", count)
		return domain.GRPCStreamBatch{}
	}
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"{{token}}"}`, "json"); err != nil {
		t.Fatal(err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount == 1 })
	first := pollMessages(2)
	if !strings.Contains(first.Messages[0].MessageJSON, security.RedactedValue) {
		t.Fatalf("initial redaction: %+v", first)
	}
	if err := svc.SendGRPCStreamMessage(t.Context(), started.ID, `{"text":"{{token}}"}`, "json"); err != nil {
		t.Fatal(err)
	}
	waitGRPCStream(t, svc, started.ID, func(result domain.GRPCExecution) bool { return result.ReceivedCount == 2 })
	second := pollMessages(4)
	if second.Messages[0].MessageJSON != first.Messages[0].MessageJSON || second.Messages[1].MessageJSON != first.Messages[1].MessageJSON {
		t.Fatalf("pending redaction markers were processed again: %+v", second)
	}
	_ = svc.CancelGRPCExecution(t.Context(), started.ID)
	awaitGRPC(t, svc, started.ID)
}
