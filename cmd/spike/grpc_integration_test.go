package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/registry"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const integrationRichBody = `{
  // This input passes through the CLI importer and JSON5 normalizer.
  text: 'dynamic wire', sequence: 9223372036854775807,
  payload: {
    details: {label: 'nested import', createdAt: '2026-10-23T12:34:56Z'},
    counters: {maximum: '9223372036854775807'},
    tags: ['one', 'two',],
  },
  extra: {'@type': 'type.googleapis.com/relay.test.common.Details', label: 'Any on wire'},
}`

type integrationService interface{}

type integrationObservation struct {
	method     protoreflect.MethodDescriptor
	requests   []*dynamicpb.Message
	halfClosed bool
}

// Both sides compile local schemas independently. Service, method and message
// types are selected from descriptors; no generated service or message exists.
func integrationServer(t *testing.T) (string, *registry.Registry, <-chan integrationObservation) {
	t.Helper()
	reg, err := importer.Compile(context.Background(), []string{"relay/v1/echo.proto"}, []string{"../../testdata/proto"})
	if err != nil {
		t.Fatal(err)
	}
	unary, err := reg.Method("/relay.test.v1.EchoService/Unary")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := reg.Method("/relay.test.v1.EchoService/Chat")
	if err != nil {
		t.Fatal(err)
	}
	observations := make(chan integrationObservation, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(unary.Parent().FullName()), HandlerType: (*integrationService)(nil),
		Methods: []grpc.MethodDesc{{MethodName: string(unary.Name()), Handler: func(_ any, _ context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			request := dynamicpb.NewMessage(unary.Input())
			if err := decode(request); err != nil {
				return nil, err
			}
			observations <- integrationObservation{method: unary, requests: []*dynamicpb.Message{request}}
			return integrationEcho(unary, request), nil
		}}},
		Streams: []grpc.StreamDesc{{StreamName: string(chat.Name()), ClientStreams: chat.IsStreamingClient(), ServerStreams: chat.IsStreamingServer(), Handler: func(_ any, stream grpc.ServerStream) error {
			observation := integrationObservation{method: chat}
			for {
				request := dynamicpb.NewMessage(chat.Input())
				if err := stream.RecvMsg(request); err != nil {
					if !errors.Is(err, io.EOF) {
						return err
					}
					observation.halfClosed = true
					observations <- observation
					final := dynamicpb.NewMessage(chat.Output())
					final.Set(chat.Output().Fields().ByName("text"), protoreflect.ValueOfString("after half-close"))
					final.Set(chat.Output().Fields().ByName("sequence"), protoreflect.ValueOfInt64(int64(len(observation.requests))))
					return stream.SendMsg(final)
				}
				observation.requests = append(observation.requests, request)
				if err := stream.SendMsg(integrationEcho(chat, request)); err != nil {
					return err
				}
			}
		}}},
	}, struct{}{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return listener.Addr().String(), reg, observations
}

func integrationEcho(method protoreflect.MethodDescriptor, request *dynamicpb.Message) *dynamicpb.Message {
	response := dynamicpb.NewMessage(method.Output())
	for _, field := range []protoreflect.Name{"text", "sequence"} {
		response.Set(method.Output().Fields().ByName(field), request.Get(method.Input().Fields().ByName(field)))
	}
	return response
}

func integrationArguments(endpoint string, method protoreflect.MethodDescriptor) []string {
	return []string{"grpc", "-plaintext", "-endpoint", endpoint, "-I", "../../testdata/proto", "-proto", "relay/v1/echo.proto", "-method", registry.FullMethod(method), "-json5", "-timeout", "5s"}
}

type integrationResponse struct {
	Text     string `json:"text"`
	Sequence string `json:"sequence"`
}

func integrationResponses(t *testing.T, data []byte) []integrationResponse {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var result []integrationResponse
	for {
		var response integrationResponse
		if err := decoder.Decode(&response); err != nil {
			if errors.Is(err, io.EOF) {
				return result
			}
			t.Fatalf("response is not canonical ProtoJSON with string int64: %v; output %s", err, data)
		}
		result = append(result, response)
	}
}

func integrationRichRequest(t *testing.T, reg *registry.Registry, request *dynamicpb.Message) {
	t.Helper()
	fields := request.Descriptor().Fields()
	if request.Get(fields.ByName("sequence")).Int() != math.MaxInt64 {
		t.Fatal("wire int64 lost precision")
	}
	payload := request.Get(fields.ByName("payload")).Message()
	if payload.Descriptor().FullName() != "relay.test.common.Payload" {
		t.Fatal("imported payload type was not used")
	}
	if payload.Get(payload.Descriptor().Fields().ByName("tags")).List().Len() != 2 {
		t.Fatal("repeated payload fields missing")
	}
	counters := payload.Get(payload.Descriptor().Fields().ByName("counters")).Map()
	if counters.Get(protoreflect.ValueOfString("maximum").MapKey()).Int() != math.MaxInt64 {
		t.Fatal("map int64 lost precision")
	}
	details := payload.Get(payload.Descriptor().Fields().ByName("details")).Message()
	if details.Get(details.Descriptor().Fields().ByName("label")).String() != "nested import" {
		t.Fatal("nested import message missing on wire")
	}
	timestamp := details.Get(details.Descriptor().Fields().ByName("created_at")).Message()
	if timestamp.Descriptor().FullName() != "google.protobuf.Timestamp" || timestamp.Get(timestamp.Descriptor().Fields().ByName("seconds")).Int() != time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC).Unix() {
		t.Fatal("WKT Timestamp did not survive the wire")
	}
	any := request.Get(fields.ByName("extra")).Message()
	typeURL := any.Get(any.Descriptor().Fields().ByName("type_url")).String()
	messageType, err := reg.Types.FindMessageByURL(typeURL)
	if err != nil {
		t.Fatal(err)
	}
	inner := messageType.New()
	if err := proto.Unmarshal(any.Get(any.Descriptor().Fields().ByName("value")).Bytes(), inner.Interface()); err != nil {
		t.Fatal(err)
	}
	if inner.Descriptor().FullName() != "relay.test.common.Details" || inner.Get(inner.Descriptor().Fields().ByName("label")).String() != "Any on wire" {
		t.Fatal("dynamic Any payload did not survive the wire")
	}
}

func TestGRPCCommandDynamicUnaryEndToEnd(t *testing.T) {
	endpoint, reg, observations := integrationServer(t)
	method, _ := reg.Method("/relay.test.v1.EchoService/Unary")
	args := append(integrationArguments(endpoint, method), "-body", "-")
	var output, warnings bytes.Buffer
	if err := run(context.Background(), args, strings.NewReader(integrationRichBody), &output, &warnings); err != nil {
		t.Fatal(err)
	}
	responses := integrationResponses(t, output.Bytes())
	if len(responses) != 1 || responses[0] != (integrationResponse{Text: "dynamic wire", Sequence: "9223372036854775807"}) {
		t.Fatalf("wrong unary response: %+v", responses)
	}
	observation := <-observations
	if observation.method != method || len(observation.requests) != 1 {
		t.Fatalf("wrong unary observation: %+v", observation)
	}
	integrationRichRequest(t, reg, observation.requests[0])
}

func TestGRPCCommandDynamicBidiEndToEnd(t *testing.T) {
	endpoint, reg, observations := integrationServer(t)
	method, _ := reg.Method("/relay.test.v1.EchoService/Chat")
	secondBody := filepath.Join(t.TempDir(), "second.json5")
	if err := os.WriteFile(secondBody, []byte(`{text: 'second request', sequence: -9223372036854775808,}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(integrationArguments(endpoint, method), "-body", "-", "-body", secondBody)
	var output, warnings bytes.Buffer
	if err := run(context.Background(), args, strings.NewReader(integrationRichBody), &output, &warnings); err != nil {
		t.Fatal(err)
	}
	responses := integrationResponses(t, output.Bytes())
	want := []integrationResponse{
		{Text: "dynamic wire", Sequence: "9223372036854775807"},
		{Text: "second request", Sequence: "-9223372036854775808"},
		{Text: "after half-close", Sequence: "2"},
	}
	if len(responses) != len(want) {
		t.Fatalf("lost messages or half-close response: %+v", responses)
	}
	for i := range want {
		if responses[i] != want[i] {
			t.Fatalf("response %d: got %+v, want %+v", i, responses[i], want[i])
		}
	}
	observation := <-observations
	if observation.method != method || len(observation.requests) != 2 || !observation.halfClosed {
		t.Fatalf("incorrect stream message count/half-close: %+v", observation)
	}
	integrationRichRequest(t, reg, observation.requests[0])
	second := observation.requests[1]
	if second.Get(second.Descriptor().Fields().ByName("sequence")).Int() != math.MinInt64 {
		t.Fatal("second message lost int64 precision")
	}
}
