package grpcclient

import (
	"errors"

	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Use grpc-go's pooled marshal implementation and retain the normal proto
// content subtype. Bound unmarshal recursion before a peer's deeply nested
// response reaches dynamic message formatting or status detail rendering.
type boundedProtoCodec struct {
	resolver protoregistry.ExtensionTypeResolver
	capture  func([]byte)
}

func (boundedProtoCodec) Name() string { return "proto" }

func (boundedProtoCodec) Marshal(value any) (mem.BufferSlice, error) {
	return encoding.GetCodecV2("proto").Marshal(value)
}

func (c boundedProtoCodec) Unmarshal(data mem.BufferSlice, value any) error {
	message, ok := value.(proto.Message)
	if !ok || isNil(message) {
		return errors.New("gRPC message: protobuf message is required")
	}
	buffer := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buffer.Free()
	if err := (proto.UnmarshalOptions{RecursionLimit: 100, Resolver: c.resolver}).Unmarshal(buffer.ReadOnlyData(), message); err != nil {
		return errors.New("gRPC message: invalid protobuf or nesting exceeds 100 levels")
	}
	if c.capture != nil {
		c.capture(buffer.ReadOnlyData())
	}
	return nil
}
