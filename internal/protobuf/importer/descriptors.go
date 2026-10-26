package importer

import (
	"context"
	"fmt"

	"relay/internal/protobuf/registry"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// DescriptorSet imports a binary FileDescriptorSet, resolving omitted standard
// imports offline. Other missing dependencies and all name conflicts fail.
func DescriptorSet(ctx context.Context, data []byte) (*registry.Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxTotalBytes {
		return nil, fmt.Errorf("descriptor set must contain between 1 byte and 16 MiB")
	}
	if err := ValidateDescriptorSetBudget(data); err != nil {
		return nil, err
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := (proto.UnmarshalOptions{RecursionLimit: 100}).Unmarshal(data, set); err != nil {
		return nil, fmt.Errorf("invalid binary FileDescriptorSet")
	}
	if len(set.File) == 0 || len(set.File) > MaxFiles {
		return nil, fmt.Errorf("descriptor set must contain between 1 and %d files", MaxFiles)
	}
	byName := make(map[string]*descriptorpb.FileDescriptorProto, len(set.File))
	for _, f := range set.File {
		if f == nil || !validImportPath(f.GetName()) {
			return nil, fmt.Errorf("descriptor set has an invalid file path")
		}
		if _, exists := byName[f.GetName()]; exists {
			return nil, fmt.Errorf("descriptor set has duplicate file path %q", f.GetName())
		}
		if f.GetSyntax() == "editions" && f.GetEdition() != descriptorpb.Edition_EDITION_2023 {
			return nil, fmt.Errorf("unsupported Protobuf edition: this release supports edition 2023")
		}
		if f.GetSyntax() != "" && f.GetSyntax() != "proto2" && f.GetSyntax() != "proto3" && f.GetSyntax() != "editions" {
			return nil, fmt.Errorf("descriptor set has unsupported syntax")
		}
		byName[f.GetName()] = f
	}
	standards := protocompile.WithStandardImports(protocompile.ResolverFunc(func(string) (protocompile.SearchResult, error) {
		return protocompile.SearchResult{}, protoregistry.NotFound
	}))
	var addStandard func(string) error
	addStandard = func(name string) error {
		if _, exists := byName[name]; exists {
			return nil
		}
		if !validImportPath(name) {
			return fmt.Errorf("descriptor set has an invalid dependency path")
		}
		resolved, err := standards.FindFileByPath(name)
		if err != nil || resolved.Desc == nil {
			return fmt.Errorf("descriptor dependency %q is missing; include imported files in the descriptor set", name)
		}
		f := protodesc.ToFileDescriptorProto(resolved.Desc)
		byName[name] = f
		set.File = append(set.File, f)
		if len(set.File) > MaxFiles {
			return fmt.Errorf("descriptor dependency closure exceeds the %d-file limit", MaxFiles)
		}
		for _, dependency := range f.GetDependency() {
			if err := addStandard(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	for _, f := range set.File {
		for _, dependency := range f.GetDependency() {
			if err := addStandard(dependency); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("invalid descriptor set: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var descriptors []protoreflect.FileDescriptor
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool { descriptors = append(descriptors, f); return true })
	return registry.New(descriptors)
}
