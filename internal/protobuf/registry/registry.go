// Package registry owns an isolated, immutable schema registry for one import.
package registry

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Registry resolves dynamic messages, including Any payloads and extensions.
// Callers must not mutate Files after publishing a Registry to other goroutines.
type Registry struct {
	Files   *protoregistry.Files
	Types   *dynamicpb.Types
	methods map[string]protoreflect.MethodDescriptor
}

// MethodInfo contains names derived from descriptors, never source filenames.
type MethodInfo struct {
	FullMethod      string `json:"fullMethod"`
	Service         string `json:"service"`
	Name            string `json:"name"`
	Input           string `json:"input"`
	Output          string `json:"output"`
	ClientStreaming bool   `json:"clientStreaming"`
	ServerStreaming bool   `json:"serverStreaming"`
	Comment         string `json:"comment"`
}

// New registers each file and its transitive imports. Conflicting file paths or
// fully qualified symbols fail instead of replacing an existing descriptor.
func New(files []protoreflect.FileDescriptor) (*Registry, error) {
	r := &Registry{Files: new(protoregistry.Files), methods: make(map[string]protoreflect.MethodDescriptor)}
	seen := make(map[string]protoreflect.FileDescriptor)
	var add func(protoreflect.FileDescriptor) error
	add = func(file protoreflect.FileDescriptor) error {
		if file == nil {
			return fmt.Errorf("register schema: nil file descriptor")
		}
		if previous, ok := seen[file.Path()]; ok {
			if previous != file {
				return fmt.Errorf("register schema: conflicting file path %q", file.Path())
			}
			return nil
		}
		seen[file.Path()] = file
		imports := file.Imports()
		for i := 0; i < imports.Len(); i++ {
			if err := add(imports.Get(i).FileDescriptor); err != nil {
				return err
			}
		}
		if err := r.Files.RegisterFile(file); err != nil {
			return fmt.Errorf("register schema %q: %w", file.Path(), err)
		}
		services := file.Services()
		for i := 0; i < services.Len(); i++ {
			methods := services.Get(i).Methods()
			for j := 0; j < methods.Len(); j++ {
				method := methods.Get(j)
				r.methods[FullMethod(method)] = method
			}
		}
		return nil
	}
	for _, file := range files {
		if err := add(file); err != nil {
			return nil, err
		}
	}
	r.Types = dynamicpb.NewTypes(r.Files)
	return r, nil
}

// FullMethod returns grpc-go's service/method path from a method descriptor.
func FullMethod(method protoreflect.MethodDescriptor) string {
	return "/" + string(method.Parent().FullName()) + "/" + string(method.Name())
}

func (r *Registry) Method(fullMethod string) (protoreflect.MethodDescriptor, error) {
	if r == nil {
		return nil, fmt.Errorf("resolve gRPC method: no schema imported")
	}
	method, ok := r.methods[fullMethod]
	if !ok {
		if !strings.HasPrefix(fullMethod, "/") || strings.Count(fullMethod, "/") != 2 {
			return nil, fmt.Errorf("invalid gRPC method %q: expected /package.Service/Method", fullMethod)
		}
		return nil, fmt.Errorf("gRPC method %q is not present in imported schemas", fullMethod)
	}
	return method, nil
}

// Methods returns a deterministic method listing suitable for service selectors.
func (r *Registry) Methods() []MethodInfo {
	if r == nil {
		return nil
	}
	result := make([]MethodInfo, 0, len(r.methods))
	for _, method := range r.methods {
		result = append(result, methodInfo(method))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].FullMethod < result[j].FullMethod })
	return result
}

func methodInfo(method protoreflect.MethodDescriptor) MethodInfo {
	return MethodInfo{
		FullMethod: FullMethod(method), Service: string(method.Parent().FullName()), Name: string(method.Name()),
		Input: string(method.Input().FullName()), Output: string(method.Output().FullName()),
		ClientStreaming: method.IsStreamingClient(), ServerStreaming: method.IsStreamingServer(), Comment: comment(method),
	}
}
