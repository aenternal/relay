package registry_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"relay/internal/protobuf/registry"
)

func descriptor(t *testing.T, path string) protoreflect.FileDescriptor {
	t.Helper()
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String(path), Package: proto.String("test"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Message")}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestRegistryRejectsConflicts(t *testing.T) {
	file := descriptor(t, "a.proto")
	if _, err := registry.New([]protoreflect.FileDescriptor{file, file}); err != nil {
		t.Fatalf("shared dependency rejected: %v", err)
	}
	if _, err := registry.New([]protoreflect.FileDescriptor{file, descriptor(t, "a.proto")}); err == nil || !strings.Contains(err.Error(), "conflicting file path") {
		t.Fatalf("file conflict: %v", err)
	}
	if _, err := registry.New([]protoreflect.FileDescriptor{file, descriptor(t, "b.proto")}); err == nil || !strings.Contains(err.Error(), "test.Message") {
		t.Fatalf("symbol conflict: %v", err)
	}
}
