package grpcclient

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"relay/internal/protobuf/registry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestUnaryWirePreservesOriginalFieldOrderAndRepeatedSingularTags(t *testing.T) {
	method := testMethods(t)["Unary"]
	var original []byte
	original = protowire.AppendTag(original, 2, protowire.VarintType)
	original = protowire.AppendVarint(original, 42)
	original = protowire.AppendTag(original, 1, protowire.BytesType)
	original = protowire.AppendString(original, "superseded")
	original = protowire.AppendTag(original, 1, protowire.BytesType)
	original = protowire.AppendString(original, "actual response")
	address := startReflectionServer(t, func(server *grpc.Server) {
		registerReflectedEcho(server, method)
	}, grpc.ForceServerCodec(rawWireCodec{wire: original}))
	client := testClient(t, address, Options{Plaintext: true})
	result, err := client.Unary(context.Background(), method, request(method, "request", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := proto.Marshal(result.Message)
	if err != nil || !bytes.Equal(result.Wire, original) || bytes.Equal(result.Wire, reencoded) || result.Message.Get(method.Output().Fields().ByName("text")).String() != "actual response" {
		t.Fatalf("original wire was canonicalized: raw=%x reencoded=%x err=%v", result.Wire, reencoded, err)
	}
}

type rawWireCodec struct{ wire []byte }

func (rawWireCodec) Name() string                  { return "proto" }
func (c rawWireCodec) Marshal(any) ([]byte, error) { return bytes.Clone(c.wire), nil }
func (rawWireCodec) Unmarshal(wire []byte, value any) error {
	return proto.Unmarshal(wire, value.(proto.Message))
}

func TestUnaryRejectsDeepResponseBeforeFormatting(t *testing.T) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("nested.proto"), Package: proto.String("relay.nested"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Node"), Field: []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("child"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".relay.nested.Node"),
		}}}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Recursive"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Echo"), InputType: proto.String(".relay.nested.Node"), OutputType: proto.String(".relay.nested.Node")}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	method := file.Services().Get(0).Methods().Get(0)
	deep := dynamicpb.NewMessage(method.Output())
	current := deep
	for range 150 {
		next := dynamicpb.NewMessage(method.Output())
		current.Set(method.Output().Fields().Get(0), protoreflect.ValueOfMessage(next))
		current = next
	}
	address := startReflectionServer(t, func(server *grpc.Server) {
		server.RegisterService(&grpc.ServiceDesc{
			ServiceName: string(method.Parent().FullName()), HandlerType: (*testService)(nil),
			Methods: []grpc.MethodDesc{{MethodName: string(method.Name()), Handler: func(_ any, _ context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				input := dynamicpb.NewMessage(method.Input())
				if err := decode(input); err != nil {
					return nil, err
				}
				return deep, nil
			}}},
		}, struct{}{})
	})
	client := testClient(t, address, Options{Plaintext: true})
	result, err := client.Unary(context.Background(), method, dynamicpb.NewMessage(method.Input()), nil)
	if status.Code(err) != codes.Internal || result.Message != nil || !strings.Contains(result.Status.Message(), "nesting exceeds 100") {
		t.Fatalf("deep response reached UI: result=%+v err=%v", result, err)
	}
}

func TestUnaryPreservesLocallyImportedProto2Extensions(t *testing.T) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("extensions.proto"), Package: proto.String("relay.extensions"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Payload"), ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{Start: proto.Int32(100), End: proto.Int32(200)}}}},
		Extension:   []*descriptorpb.FieldDescriptorProto{{Name: proto.String("answer"), Extendee: proto.String(".relay.extensions.Payload"), Number: proto.Int32(100), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}},
		Service:     []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Echo"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Unary"), InputType: proto.String(".relay.extensions.Payload"), OutputType: proto.String(".relay.extensions.Payload")}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New([]protoreflect.FileDescriptor{file})
	if err != nil {
		t.Fatal(err)
	}
	extension := dynamicpb.NewExtensionType(file.Extensions().Get(0))
	method := file.Services().Get(0).Methods().Get(0)
	address := startReflectionServer(t, func(server *grpc.Server) {
		server.RegisterService(&grpc.ServiceDesc{
			ServiceName: string(method.Parent().FullName()), HandlerType: (*testService)(nil),
			Methods: []grpc.MethodDesc{{MethodName: string(method.Name()), Handler: func(_ any, _ context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				input := dynamicpb.NewMessage(method.Input())
				if err := decode(input); err != nil {
					return nil, err
				}
				output := dynamicpb.NewMessage(method.Output())
				proto.SetExtension(output, extension, int64(9223372036854775807))
				return output, nil
			}}},
		}, struct{}{})
	})
	client := testClient(t, address, Options{Plaintext: true, Resolver: reg.Types})
	result, err := client.Unary(context.Background(), method, dynamicpb.NewMessage(method.Input()), nil)
	if err != nil || !proto.HasExtension(result.Message, extension) || proto.GetExtension(result.Message, extension) != int64(9223372036854775807) {
		t.Fatalf("local proto2 extension was discarded as an unknown field: result=%+v err=%v", result, err)
	}
}
