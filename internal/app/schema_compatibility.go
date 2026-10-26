package app

import (
	"context"
	"crypto/sha256"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// This detects schema changes relevant to a saved request. It is deliberately a
// review notice, not a claim to implement the full Protobuf compatibility rules.
func changedProtoMethod(before, after protoreflect.MethodDescriptor, beforeShape, afterShape func(protoreflect.MessageDescriptor) [32]byte) bool {
	return before.IsStreamingClient() != after.IsStreamingClient() ||
		before.IsStreamingServer() != after.IsStreamingServer() ||
		before.Input().FullName() != after.Input().FullName() ||
		before.Output().FullName() != after.Output().FullName() ||
		beforeShape(before.Input()) != afterShape(after.Input()) ||
		beforeShape(before.Output()) != afterShape(after.Output())
}

// Follow named message and enum references once, including recursive schemas.
// Descriptor serialization excludes source locations and therefore comments.
func protoShapeCache(ctx context.Context) func(protoreflect.MessageDescriptor) [32]byte {
	cache := map[protoreflect.FullName][32]byte{}
	return func(root protoreflect.MessageDescriptor) [32]byte {
		if cached, ok := cache[root.FullName()]; ok {
			return cached
		}
		value := protoMessageShape(ctx, root)
		cache[root.FullName()] = value
		return value
	}
}

func protoMessageShape(ctx context.Context, root protoreflect.MessageDescriptor) [32]byte {
	shapes := map[string][]byte{}
	addFile := func(file protoreflect.FileDescriptor) {
		name := "$file:" + file.Path()
		if _, exists := shapes[name]; exists {
			return
		}
		original := protodesc.ToFileDescriptorProto(file)
		shape := &descriptorpb.FileDescriptorProto{Syntax: proto.String(file.Syntax().String()), Edition: original.Edition}
		if features := original.GetOptions().GetFeatures(); features != nil {
			shape.Options = &descriptorpb.FileOptions{Features: features}
		}
		shapes[name], _ = (proto.MarshalOptions{Deterministic: true}).Marshal(shape)
	}
	var addMessage func(protoreflect.MessageDescriptor)
	addEnum := func(e protoreflect.EnumDescriptor) {
		addFile(e.ParentFile())
		name := string(e.FullName())
		if _, exists := shapes[name]; !exists {
			shapes[name], _ = (proto.MarshalOptions{Deterministic: true}).Marshal(protodesc.ToEnumDescriptorProto(e))
			if e.IsClosed() {
				shapes[name] = append(shapes[name], 1)
			} else {
				shapes[name] = append(shapes[name], 0)
			}
		}
	}
	addMessage = func(message protoreflect.MessageDescriptor) {
		if ctx.Err() != nil {
			return
		}
		name := string(message.FullName())
		if _, exists := shapes[name]; exists {
			return
		}
		addFile(message.ParentFile())
		shapes[name], _ = (proto.MarshalOptions{Deterministic: true}).Marshal(protodesc.ToDescriptorProto(message))
		for i := 0; i < message.Fields().Len(); i++ {
			field := message.Fields().Get(i)
			if field.HasPresence() {
				shapes[name] = append(shapes[name], 1)
			} else {
				shapes[name] = append(shapes[name], 0)
			}
			if referenced := field.Message(); referenced != nil {
				addMessage(referenced)
			}
			if enum := field.Enum(); enum != nil {
				addEnum(enum)
			}
		}
	}
	addMessage(root)
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		digest.Write(shapes[name])
		digest.Write([]byte{0})
	}
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result
}
