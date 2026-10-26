package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/registry"
	"relay/internal/security"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const grpcAppSource = `syntax="proto3"; package stage4;
message Message { string text=1; int64 count=2; bytes data=3; }
message Failure { string reason=1; }
service Echo { rpc Call(Message) returns(Message); rpc Watch(Message) returns(stream Message); }`

type grpcAppFixture struct {
	address string
	reg     *registry.Registry
	entered chan struct{}
	closed  atomic.Int32
	calls   atomic.Int32
}

type grpcAppService interface{}

func startGRPCAppFixture(t *testing.T, reflected bool, options ...grpc.ServerOption) *grpcAppFixture {
	t.Helper()
	reg, err := importer.CompileSources(t.Context(), map[string]string{"stage4.proto": grpcAppSource}, []string{"stage4.proto"})
	if err != nil {
		t.Fatal(err)
	}
	method, _ := reg.Method("/stage4.Echo/Call")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &grpcAppFixture{address: listener.Addr().String(), reg: reg, entered: make(chan struct{}, 32)}
	server := grpc.NewServer(options...)
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "stage4.Echo", HandlerType: (*grpcAppService)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Call", Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			input := dynamicpb.NewMessage(method.Input())
			if err := decode(input); err != nil {
				return nil, err
			}
			fixture.calls.Add(1)
			md, _ := metadata.FromIncomingContext(ctx)
			_ = grpc.SetHeader(ctx, metadata.Join(metadata.Pairs("server", "dynamic"), metadata.MD{"echo-tag": md.Get("tag"), "echo-bin": md.Get("data-bin"), "echo-authorization": md.Get("authorization")}))
			_ = grpc.SetTrailer(ctx, metadata.Pairs("done", "unary"))
			text := input.Get(method.Input().Fields().ByName("text")).String()
			switch {
			case text == "wait":
				fixture.entered <- struct{}{}
				<-ctx.Done()
				fixture.closed.Add(1)
				return nil, status.FromContextError(ctx.Err()).Err()
			case strings.HasPrefix(text, "fail:"):
				reason := strings.TrimPrefix(text, "fail:")
				messageType, _ := reg.Types.FindMessageByName("stage4.Failure")
				detail := dynamicpb.NewMessage(messageType.Descriptor())
				detail.Set(detail.Descriptor().Fields().ByName("reason"), protoreflect.ValueOfString(reason))
				s := status.New(codes.PermissionDenied, "denied "+reason)
				s, _ = s.WithDetails(detail)
				return nil, s.Err()
			case text == "big":
				input.Set(method.Input().Fields().ByName("text"), protoreflect.ValueOfString(strings.Repeat("z", 4096)))
			}
			return input, nil
		}}},
		Streams:  []grpc.StreamDesc{{StreamName: "Watch", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error { return status.Error(codes.Unimplemented, "not used") }}},
		Metadata: "stage4.proto",
	}, struct{}{})
	if reflected {
		reflectionv1.RegisterServerReflectionServer(server, reflection.NewServerV1(reflection.ServerOptions{Services: server, DescriptorResolver: reg.Files}))
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return fixture
}

func newGRPCApp(t *testing.T, address string) (*app.Service, domain.RequestDefinition, string) {
	t.Helper()
	dir := t.TempDir()
	_, svc := openApp(t, filepath.Join(dir, "grpc.db"), security.NewMemoryStore())
	t.Cleanup(func() {
		if err := app.ShutdownGRPC(svc); err != nil {
			t.Error(err)
		}
	})
	state := loadState(t, svc)
	imported := importSchema(t, svc, app.ProtoImport{WorkspaceID: state.Workspaces[0].ID, Name: "gRPC fixture", Kind: "proto", RootFiles: []string{"stage4.proto"}, Sources: []app.ProtoSource{{Name: "stage4.proto", Content: grpcAppSource}}})
	request := state.Requests[0]
	request.Protocol, request.HTTP = "grpc", nil
	request.GRPC = &domain.GRPCDefinition{Endpoint: address, Plaintext: true, SchemaID: imported.SchemaID, Method: "/stage4.Echo/Call", BodyFormat: "json", Body: `{}`}
	return svc, request, dir
}

func awaitGRPC(t *testing.T, svc *app.Service, id string) domain.GRPCExecution {
	t.Helper()
	deadline := time.After(8 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := svc.GetGRPCExecution(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "running" {
			return result
		}
		select {
		case <-deadline:
			t.Fatal("gRPC execution did not finish")
		case <-ticker.C:
		}
	}
}

func executeGRPC(t *testing.T, svc *app.Service, request domain.RequestDefinition) domain.GRPCExecution {
	t.Helper()
	started, err := svc.StartGRPCUnary(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return awaitGRPC(t, svc, started.ID)
}

func grpcHeader(headers []domain.Header, name string) []string {
	values := []string{}
	for _, h := range headers {
		if h.Name == name {
			values = append(values, h.Value)
		}
	}
	return values
}

func TestGRPCUnaryJSON5Int64MetadataDetachedContextAndSecretRedaction(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, dir := newGRPCApp(t, fixture.address)
	_, envID, variableID := addToken(t, svc)
	const secret = "gRPC-secret-quoted-\"-4193"
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, secret))
	request.GRPC.BodyFormat = "json5"
	request.GRPC.Body = `{text:'{{token}}', count:9223372036854775807, data:'AP8=',}`
	request.GRPC.Metadata = []domain.Header{enable("tag", "one"), enable("tag", "two"), enable("data-bin", "AP8="), enable("authorization", "Bearer {{token}}")}
	request.GRPC.Compression = "gzip"
	ctx, cancel := context.WithCancel(t.Context())
	started, err := svc.StartGRPCUnary(ctx, request)
	cancel()
	if err != nil || started.State != "running" {
		t.Fatalf("start failed: %+v, %v", started, err)
	}
	result := awaitGRPC(t, svc, started.ID)
	if result.State != "completed" || result.Code != 0 || result.CodeName != "OK" || !strings.Contains(result.MessageJSON, "9223372036854775807") || result.WireBytes == 0 || result.WireBase64 != "" || result.TotalMS <= 0 || result.ConnectMS <= 0 {
		t.Fatalf("unexpected unary result: %+v", result)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("gRPC-secret")) || !strings.Contains(result.MessageJSON, security.RedactedValue) {
		t.Fatalf("secret leaked to execution: %s", encoded)
	}
	if strings.Join(grpcHeader(result.Headers, "echo-tag"), ",") != "one,two" || strings.Join(grpcHeader(result.Headers, "echo-bin"), ",") != "AP8=" || strings.Join(grpcHeader(result.Trailers, "done"), ",") != "unary" {
		t.Fatalf("metadata repeated/binary/trailers changed: %+v %+v", result.Headers, result.Trailers)
	}
	assertNoSecretOnDisk(t, dir, secret, "gRPC-secret")
	// Caller-owned execution slices cannot mutate the retained response.
	result.Headers[0].Value = "mutated"
	retained, _ := svc.GetGRPCExecution(t.Context(), result.ID)
	if retained.Headers[0].Value == "mutated" {
		t.Fatal("retained execution aliases a returned slice")
	}
}

func TestGRPCPeerStatusDetailsAndUnknownAnyFallback(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, _ := newGRPCApp(t, fixture.address)
	request.GRPC.Body = `{"text":"fail:permission required"}`
	result := executeGRPC(t, svc, request)
	if result.State != "failed" || result.Code != int(codes.PermissionDenied) || !strings.Contains(result.StatusMessage, "permission required") || len(result.Details) != 1 || !strings.Contains(result.Details[0].MessageJSON, "permission required") || result.Details[0].WireBase64 == "" || len(result.Trailers) == 0 {
		t.Fatalf("peer status/details/trailers lost: %+v", result)
	}
	// A schema may describe the service without registering a peer's detail type.
	local := strings.Replace(grpcAppSource, "message Failure { string reason=1; }", "", 1)
	imported := importSchema(t, svc, app.ProtoImport{WorkspaceID: request.WorkspaceID, Name: "Without detail", Kind: "proto", Sources: []app.ProtoSource{{Name: "stage4.proto", Content: local}}})
	request.GRPC.SchemaID = imported.SchemaID
	result = executeGRPC(t, svc, request)
	if len(result.Details) != 1 || result.Details[0].MessageJSON != "" || result.Details[0].WireBase64 == "" || !strings.Contains(result.Details[0].Error, "not present") {
		t.Fatalf("unknown Any detail fallback missing: %+v", result.Details)
	}
}

func TestGRPCReflectionPersistsSchemaAndDisabledUsesLocalSchema(t *testing.T) {
	fixture := startGRPCAppFixture(t, true)
	svc, request, dir := newGRPCApp(t, fixture.address)
	request.GRPC.SchemaID = ""
	started, err := svc.StartGRPCReflection(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	result := awaitGRPC(t, svc, started.ID)
	if result.State != "completed" || result.SchemaID == "" || result.ReflectionProtocol != "v1" {
		t.Fatalf("reflection did not persist a schema: %+v", result)
	}
	details, err := svc.InspectProtoSchema(t.Context(), request.WorkspaceID, result.SchemaID)
	if err != nil || len(details.Services) == 0 {
		t.Fatalf("reflected schema cannot be inspected: %+v %v", details, err)
	}
	reflectedID := result.SchemaID
	refreshed, err := svc.StartGRPCReflection(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitGRPC(t, svc, refreshed.ID); result.SchemaID != reflectedID || len(loadState(t, svc).ProtoSchemas) != 2 {
		t.Fatalf("reflection refresh duplicated the cached schema: %+v", result)
	}
	_, restarted := openApp(t, filepath.Join(dir, "grpc.db"), security.NewMemoryStore())
	t.Cleanup(func() { _ = app.ShutdownGRPC(restarted) })
	if _, err := restarted.InspectProtoSchema(t.Context(), request.WorkspaceID, reflectedID); err != nil {
		t.Fatalf("cached reflection schema did not survive restart: %v", err)
	}
	request.GRPC.SchemaID = result.SchemaID
	request.GRPC.Body = `{"text":"from reflection"}`
	if unary := executeGRPC(t, svc, request); unary.State != "completed" || !strings.Contains(unary.MessageJSON, "from reflection") {
		t.Fatalf("reflected method cannot be invoked: %+v", unary)
	}
	noReflection := startGRPCAppFixture(t, false)
	request.GRPC.Endpoint = noReflection.address
	started, err = svc.StartGRPCReflection(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	result = awaitGRPC(t, svc, started.ID)
	if result.State != "failed" || result.Code != int(codes.Unimplemented) || !strings.Contains(result.Error, "import a local") {
		t.Fatalf("disabled reflection not actionable: %+v", result)
	}
	if unary := executeGRPC(t, svc, request); unary.State != "completed" {
		t.Fatalf("local schema unexpectedly requires reflection: %+v", unary)
	}
}

func TestGRPCBinaryAndNumericKeyringSecretsNeverReachPreviews(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, dir := newGRPCApp(t, fixture.address)
	_, envID, variableID := addToken(t, svc)
	const raw = "binary-secret-4193\x00\xff"
	secret := base64.StdEncoding.EncodeToString([]byte(raw))
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, secret))
	request.GRPC.Body = `{"text":"safe","data":"{{token}}"}`
	request.GRPC.Metadata = []domain.Header{enable("data-bin", "{{token}}")}
	result := executeGRPC(t, svc, request)
	if result.State != "completed" || result.WireBase64 != "" || !strings.Contains(result.MessageJSON, security.RedactedValue) || strings.Join(grpcHeader(result.Headers, "echo-bin"), ",") != security.RedactedValue {
		t.Fatalf("binary keyring secret was exposed: %+v", result)
	}
	assertNoSecretOnDisk(t, dir, raw, secret)
	mustState(t)(svc.SetSecret(t.Context(), envID, variableID, "9223372036854775807"))
	request.GRPC.Metadata = nil
	request.GRPC.Body = `{"count":"{{token}}"}`
	result = executeGRPC(t, svc, request)
	if result.State != "completed" || result.WireBase64 != "" || strings.Contains(result.MessageJSON, "9223372036854775807") {
		t.Fatalf("numeric keyring secret was exposed: %+v", result)
	}
}

func TestGRPCTransientSensitiveMetadataVariantsAreRedacted(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, _ := newGRPCApp(t, fixture.address)
	for _, name := range []string{"x-signature", "credential", "credentials", "passwd", "x-signature-bin"} {
		const secret = "literal-sensitive-metadata-4193"
		value := secret
		if strings.HasSuffix(name, "-bin") {
			value = base64.StdEncoding.EncodeToString([]byte(secret))
		}
		request.GRPC.Metadata = []domain.Header{enable(name, value)}
		request.GRPC.Body = `{"text":"` + secret + `"}`
		result := executeGRPC(t, svc, request)
		if result.State != "completed" || result.WireBase64 != "" || strings.Contains(result.MessageJSON, secret) || !strings.Contains(result.MessageJSON, security.RedactedValue) {
			t.Fatalf("sensitive metadata %s not tracked: %+v", name, result)
		}
	}
}

func TestGRPCShortSecretsBoundJSONExpansionAndInactiveTLSIsPreserved(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, _ := newGRPCApp(t, fixture.address)
	request.GRPC.Metadata = []domain.Header{enable("x-token", "E")}
	request.GRPC.TLS = domain.HTTPTLS{CAFileID: "inactive-grant", ServerName: "inactive.example", Insecure: true}
	request.GRPC.Body = `{"text":"E"}`
	result := executeGRPC(t, svc, request)
	if result.State != "completed" || strings.Count(result.MessageJSON, security.RedactedValue) != 1 || strings.Contains(result.MessageJSON, "[R[REDACTED]") || result.WireBase64 != "" || len(result.Warnings) < 2 {
		t.Fatalf("redaction recursively expanded a marker or inactive TLS was used: %+v", result)
	}
	request.GRPC.Body = `{"text":"` + strings.Repeat("E", 1<<20) + `"}`
	result = executeGRPC(t, svc, request)
	if result.State != "completed" || !result.Truncated || result.MessageJSON != "" || result.WireBase64 != "" || result.WireBytes < 1<<20 {
		t.Fatalf("expanded JSON was not bounded: state=%s truncated=%v JSON=%d wire=%d", result.State, result.Truncated, len(result.MessageJSON), result.WireBytes)
	}
}

func TestGRPCCancelDeadlineShutdownAndConcurrencyBudget(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, _ := newGRPCApp(t, fixture.address)
	request.GRPC.Body = `{"text":"wait"}`
	started := []domain.GRPCExecution{}
	for range 4 {
		run, err := svc.StartGRPCUnary(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, run)
	}
	if _, err := svc.StartGRPCUnary(t.Context(), request); err == nil {
		t.Fatal("more than four operations admitted")
	}
	for range 4 {
		select {
		case <-fixture.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("operation did not reach the server")
		}
	}
	if err := svc.ForgetGRPCExecution(t.Context(), started[0].ID); err == nil {
		t.Fatal("running operation forgotten before cancellation")
	}
	if err := svc.CancelGRPCExecution(t.Context(), started[0].ID); err != nil {
		t.Fatal(err)
	}
	if result := awaitGRPC(t, svc, started[0].ID); result.State != "canceled" || result.CodeName != "Canceled" {
		t.Fatalf("cancel did not reach worker: %+v", result)
	}
	if err := svc.ForgetGRPCExecution(t.Context(), started[0].ID); err != nil {
		t.Fatal(err)
	}
	request.GRPC.TimeoutMS = 40
	if result := executeGRPC(t, svc, request); result.State != "failed" || result.CodeName != "DeadlineExceeded" {
		t.Fatalf("deadline lost: %+v", result)
	}
	if err := app.ShutdownGRPC(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartGRPCUnary(t.Context(), request); err == nil {
		t.Fatal("shutdown admitted another operation")
	}
	deadline := time.Now().Add(time.Second)
	for fixture.closed.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fixture.closed.Load() != 5 {
		t.Fatalf("server contexts remained live: %d closed", fixture.closed.Load())
	}
}

func TestGRPCValidationLimitsAndSafeBinaryResponse(t *testing.T) {
	fixture := startGRPCAppFixture(t, false)
	svc, request, _ := newGRPCApp(t, fixture.address)
	for _, md := range [][]domain.Header{{enable("Bad-Name", "x")}, {enable("grpc-timeout", "x")}, {enable("x", "bad\nvalue")}, {enable("x-bin", "??")}} {
		request.GRPC.Metadata = md
		if result := executeGRPC(t, svc, request); result.State != "failed" || result.CodeName != "InvalidArgument" {
			t.Fatalf("invalid metadata not rejected: %+v", result)
		}
	}
	if fixture.calls.Load() != 0 {
		t.Fatal("invalid metadata reached an RPC handler")
	}
	request.GRPC.Metadata = nil
	request.GRPC.Method = "/stage4.Echo/Watch"
	if result := executeGRPC(t, svc, request); result.State != "failed" || !strings.Contains(result.Error, "streaming") {
		t.Fatalf("streaming method accepted as unary: %+v", result)
	}
	request.GRPC.Method = "/stage4.Echo/Call"
	request.GRPC.Body = `{"text":"big"}`
	request.GRPC.MaxReceiveBytes = 128
	if result := executeGRPC(t, svc, request); result.State != "failed" || result.CodeName != "ResourceExhausted" {
		t.Fatalf("receive limit ignored: %+v", result)
	}
	request.GRPC.MaxReceiveBytes = 0
	request.GRPC.MaxSendBytes = 1
	if result := executeGRPC(t, svc, request); result.State != "failed" || result.CodeName != "ResourceExhausted" {
		t.Fatalf("send limit ignored: %+v", result)
	}
	request.GRPC.MaxSendBytes = 0
	request.GRPC.Body = `{"text":"binary","count":"9223372036854775807","data":"AP8="}`
	result := executeGRPC(t, svc, request)
	wire, err := base64.StdEncoding.DecodeString(result.WireBase64)
	method, _ := fixture.reg.Method("/stage4.Echo/Call")
	message := dynamicpb.NewMessage(method.Output())
	if err != nil || proto.Unmarshal(wire, message) != nil || result.State != "completed" || message.Get(method.Output().Fields().ByName("count")).Int() != 9223372036854775807 {
		t.Fatalf("binary response did not preserve protobuf/int64: %+v %v", result, err)
	}
}
