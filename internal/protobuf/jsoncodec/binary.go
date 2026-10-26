package jsoncodec

import (
	"fmt"

	"relay/internal/protobuf/registry"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// EncodeBinary uses stable field/map ordering for repeatable editor previews.
func EncodeBinary(message proto.Message) ([]byte, error) {
	if message == nil {
		return nil, fmt.Errorf("encode Protobuf wire: no message supplied")
	}
	if proto.Size(message) > MaxInputBytes {
		return nil, fmt.Errorf("Protobuf wire exceeds the 4 MiB limit")
	}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode Protobuf wire: required field is missing or message is invalid")
	}
	return data, nil
}

// DecodeBinary decodes real protobuf wire data, including imported extensions.
// Unknown fields survive a round trip; malformed wire diagnostics omit bytes.
func DecodeBinary(reg *registry.Registry, descriptor protoreflect.MessageDescriptor, data []byte) (*dynamicpb.Message, error) {
	if reg == nil || reg.Types == nil || descriptor == nil {
		return nil, fmt.Errorf("decode Protobuf wire: select an imported message")
	}
	if len(data) > MaxInputBytes {
		return nil, fmt.Errorf("Protobuf wire exceeds the 4 MiB limit")
	}
	message := dynamicpb.NewMessage(descriptor)
	if err := (proto.UnmarshalOptions{Resolver: reg.Types, RecursionLimit: MaxDepth}).Unmarshal(data, message); err != nil {
		return nil, fmt.Errorf("decode Protobuf wire: malformed input or missing required field")
	}
	return message, nil
}
