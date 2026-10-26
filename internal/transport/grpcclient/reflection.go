package grpcclient

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/registry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	v1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	DefaultReflectionTimeout = 30 * time.Second
	MaxReflectionServices    = 2048
	MaxReflectionRequests    = 2048
)

// ReflectionOptions can reduce the resource limits of one discovery. Zero
// values select the defaults; limits above the hard caps are rejected.
type ReflectionOptions struct {
	MaxFiles, MaxBytes, MaxServices, MaxRequests int
	Timeout                                      time.Duration
}

// ReflectionResult contains a complete, validated descriptor closure. The
// binary descriptor set can be cached independently of this live connection.
// Services omits the two reflection protocol services themselves.
type ReflectionResult struct {
	Registry      *registry.Registry
	DescriptorSet []byte
	Protocol      string
	Services      []string
}

// Reflection discovers services and their transitive file dependencies on the
// same connection and with the same metadata/TLS settings used by Unary. It
// tries v1 first and falls back to v1alpha only for an UNIMPLEMENTED status.
// Both attempts share a deadline. Streams are closed/canceled on every exit.
func (c *Client) Reflection(ctx context.Context, md metadata.MD, options ReflectionOptions) (*ReflectionResult, error) {
	if err := options.defaults(); err != nil {
		return nil, err
	}
	ctx, err := outgoingContext(ctx, md, c.maxMetadata)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	result, err := c.reflectVersion(ctx, options, "v1")
	if status.Code(err) == codes.Unimplemented {
		result, err = c.reflectVersion(ctx, options, "v1alpha")
		if status.Code(err) == codes.Unimplemented {
			return nil, rpcError("reflection is not enabled; import a .proto or descriptor set", "", err)
		}
	}
	return result, err
}

func (o *ReflectionOptions) defaults() error {
	if o.MaxFiles == 0 {
		o.MaxFiles = importer.MaxFiles
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = importer.MaxTotalBytes
	}
	if o.MaxServices == 0 {
		o.MaxServices = MaxReflectionServices
	}
	if o.MaxRequests == 0 {
		o.MaxRequests = MaxReflectionRequests
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultReflectionTimeout
	}
	if o.MaxFiles < 1 || o.MaxFiles > importer.MaxFiles || o.MaxBytes < 1 || o.MaxBytes > importer.MaxTotalBytes || o.MaxServices < 1 || o.MaxServices > MaxReflectionServices || o.MaxRequests < 1 || o.MaxRequests > MaxReflectionRequests || o.Timeout < 0 || o.Timeout > 24*time.Hour {
		return errors.New("gRPC reflection: invalid discovery limits")
	}
	return nil
}

type reflectionRequest struct {
	list     bool
	symbol   string
	filename string
}

type reflectionResponse struct {
	services []string
	files    [][]byte
	isList   bool
	isFiles  bool
}

type reflectionExchange func(reflectionRequest) (reflectionResponse, error)

func (c *Client) reflectVersion(ctx context.Context, options ReflectionOptions, protocol string) (*ReflectionResult, error) {
	// Cancel this individual stream before opening a fallback stream; the shared
	// parent context retains the original deadline across both attempts.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exchange, closeSend, err := c.reflectionStream(ctx, options.MaxBytes+DefaultMaxMetadataBytes, protocol)
	if err != nil {
		return nil, rpcError("reflection", "", err)
	}
	defer closeSend()
	requests := 0
	ask := func(request reflectionRequest) (reflectionResponse, error) {
		if requests >= options.MaxRequests {
			return reflectionResponse{}, errors.New("gRPC reflection: request limit exceeded")
		}
		requests++
		response, err := exchange(request)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return response, errors.New("gRPC reflection: server closed before completing discovery")
			}
			return response, rpcError("reflection", "", err)
		}
		if request.list && !response.isList || !request.list && !response.isFiles {
			return response, errors.New("gRPC reflection: unexpected response type")
		}
		return response, nil
	}
	listed, err := ask(reflectionRequest{list: true})
	if err != nil {
		return nil, err
	}
	if len(listed.services) > options.MaxServices {
		return nil, errors.New("gRPC reflection: service limit exceeded")
	}
	services := make([]string, 0, len(listed.services))
	seenServices := make(map[string]bool)
	for _, name := range listed.services {
		if len(name) > 1024 || !protoreflect.FullName(name).IsValid() {
			return nil, errors.New("gRPC reflection: server advertised an invalid service name")
		}
		if name == "grpc.reflection.v1.ServerReflection" || name == "grpc.reflection.v1alpha.ServerReflection" || seenServices[name] {
			continue
		}
		seenServices[name] = true
		services = append(services, name)
	}
	if len(services) == 0 {
		return nil, errors.New("gRPC reflection: server did not advertise any application services")
	}
	sort.Strings(services)
	files := make(map[string]*descriptorpb.FileDescriptorProto)
	loadedServices := make(map[string]bool)
	var queue []string
	bytes := 0
	var budget importer.DescriptorBudget
	add := func(response reflectionResponse) error {
		if len(response.files) == 0 {
			return errors.New("gRPC reflection: server returned no file descriptors")
		}
		for _, wire := range response.files {
			// Count all received descriptor bytes, including repeated files. A
			// peer must not bypass the budget by repeating an existing closure.
			bytes += len(wire)
			if len(wire) == 0 || bytes > options.MaxBytes {
				return errors.New("gRPC reflection: descriptor byte limit exceeded")
			}
			if err := budget.AddFile(wire); err != nil {
				return errors.New("gRPC reflection: descriptor structure exceeds its limit or is invalid")
			}
			file := new(descriptorpb.FileDescriptorProto)
			if err := (proto.UnmarshalOptions{RecursionLimit: 100}).Unmarshal(wire, file); err != nil {
				return errors.New("gRPC reflection: invalid binary file descriptor")
			}
			if !validReflectionPath(file.GetName()) {
				return errors.New("gRPC reflection: invalid descriptor file path")
			}
			if previous, ok := files[file.GetName()]; ok {
				if !proto.Equal(previous, file) {
					return errors.New("gRPC reflection: conflicting descriptors for one file")
				}
				continue
			}
			if len(files) >= options.MaxFiles {
				return errors.New("gRPC reflection: descriptor file or byte limit exceeded")
			}
			files[file.GetName()] = file
			for _, service := range file.GetService() {
				name := service.GetName()
				if file.GetPackage() != "" {
					name = file.GetPackage() + "." + name
				}
				loadedServices[name] = true
			}
			for _, dependency := range file.GetDependency() {
				if !validReflectionPath(dependency) {
					return errors.New("gRPC reflection: invalid descriptor dependency path")
				}
				queue = append(queue, dependency)
			}
		}
		return nil
	}
	for _, service := range services {
		if loadedServices[service] {
			continue
		}
		response, err := ask(reflectionRequest{symbol: service})
		if err != nil {
			return nil, err
		}
		if err := add(response); err != nil {
			return nil, err
		}
	}
	requested := make(map[string]bool)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := files[name]; ok || requested[name] {
			continue
		}
		requested[name] = true
		response, err := ask(reflectionRequest{filename: name})
		if status.Code(err) == codes.NotFound {
			// DescriptorSet below can resolve omitted standard WKT descriptors
			// offline. Missing application imports still fail that validation.
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := add(response); err != nil {
			return nil, err
		}
		if _, ok := files[name]; !ok {
			return nil, errors.New("gRPC reflection: requested dependency is missing from the response")
		}
	}
	set := new(descriptorpb.FileDescriptorSet)
	for _, file := range files {
		set.File = append(set.File, file)
	}
	sort.Slice(set.File, func(i, j int) bool { return set.File[i].GetName() < set.File[j].GetName() })
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(set)
	if err != nil || len(data) > options.MaxBytes {
		return nil, errors.New("gRPC reflection: descriptor set exceeds byte limit")
	}
	reg, err := importer.DescriptorSet(ctx, data)
	if err != nil {
		if ctx.Err() != nil {
			return nil, rpcError("reflection", "", status.FromContextError(ctx.Err()).Err())
		}
		return nil, errors.New("gRPC reflection: invalid or incomplete descriptor dependency closure")
	}
	for _, service := range services {
		descriptor, err := reg.Files.FindDescriptorByName(protoreflect.FullName(service))
		if _, ok := descriptor.(protoreflect.ServiceDescriptor); err != nil || !ok {
			return nil, errors.New("gRPC reflection: advertised service is absent from the descriptors")
		}
	}
	// Include any omitted standard imports resolved offline in the reusable
	// cache, and apply limits to that complete dependency closure as well.
	if reg.Files.NumFiles() > options.MaxFiles {
		return nil, errors.New("gRPC reflection: descriptor dependency closure exceeds file limit")
	}
	data, err = reg.DescriptorSet()
	if err != nil || len(data) > options.MaxBytes {
		return nil, errors.New("gRPC reflection: descriptor dependency closure exceeds byte limit")
	}
	return &ReflectionResult{Registry: reg, DescriptorSet: data, Protocol: protocol, Services: services}, nil
}

func validReflectionPath(name string) bool {
	return name != "" && len(name) <= 1024 && !strings.ContainsAny(name, "\\:\x00\r\n") && !strings.HasPrefix(name, "/") && name != "." && name != ".." && !strings.HasPrefix(name, "../") && path.Clean(name) == name
}

// The generated v1/v1alpha clients have distinct Go types. Normalize only the
// three reflection requests needed for service discovery, without wire casts.
func (c *Client) reflectionStream(ctx context.Context, maxReceive int, protocol string) (reflectionExchange, func() error, error) {
	options := []grpc.CallOption{grpc.MaxCallRecvMsgSize(maxReceive)}
	if protocol == "v1" {
		stream, err := v1.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx, options...)
		if err != nil {
			return nil, nil, err
		}
		return func(q reflectionRequest) (reflectionResponse, error) {
			request := new(v1.ServerReflectionRequest)
			switch {
			case q.list:
				request.MessageRequest = &v1.ServerReflectionRequest_ListServices{ListServices: ""}
			case q.symbol != "":
				request.MessageRequest = &v1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: q.symbol}
			default:
				request.MessageRequest = &v1.ServerReflectionRequest_FileByFilename{FileByFilename: q.filename}
			}
			if err := stream.Send(request); err != nil && !errors.Is(err, io.EOF) {
				return reflectionResponse{}, err
			}
			response, err := stream.Recv()
			if err != nil {
				return reflectionResponse{}, err
			}
			if peerError := response.GetErrorResponse(); peerError != nil {
				return reflectionResponse{}, reflectionStatus(peerError.GetErrorCode())
			}
			out := reflectionResponse{}
			if list := response.GetListServicesResponse(); list != nil {
				out.isList = true
				for _, service := range list.Service {
					out.services = append(out.services, service.GetName())
				}
			}
			if files := response.GetFileDescriptorResponse(); files != nil {
				out.isFiles, out.files = true, files.FileDescriptorProto
			}
			return out, nil
		}, stream.CloseSend, nil
	}
	stream, err := alpha.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx, options...)
	if err != nil {
		return nil, nil, err
	}
	return func(q reflectionRequest) (reflectionResponse, error) {
		request := new(alpha.ServerReflectionRequest)
		switch {
		case q.list:
			request.MessageRequest = &alpha.ServerReflectionRequest_ListServices{ListServices: ""}
		case q.symbol != "":
			request.MessageRequest = &alpha.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: q.symbol}
		default:
			request.MessageRequest = &alpha.ServerReflectionRequest_FileByFilename{FileByFilename: q.filename}
		}
		if err := stream.Send(request); err != nil && !errors.Is(err, io.EOF) {
			return reflectionResponse{}, err
		}
		response, err := stream.Recv()
		if err != nil {
			return reflectionResponse{}, err
		}
		if peerError := response.GetErrorResponse(); peerError != nil {
			return reflectionResponse{}, reflectionStatus(peerError.GetErrorCode())
		}
		out := reflectionResponse{}
		if list := response.GetListServicesResponse(); list != nil {
			out.isList = true
			for _, service := range list.Service {
				out.services = append(out.services, service.GetName())
			}
		}
		if files := response.GetFileDescriptorResponse(); files != nil {
			out.isFiles, out.files = true, files.FileDescriptorProto
		}
		return out, nil
	}, stream.CloseSend, nil
}

func reflectionStatus(code int32) error {
	if code <= 0 || code > int32(codes.Unauthenticated) {
		return status.Error(codes.Unknown, "invalid reflection error status")
	}
	// Peer text may contain credentials. Callers receive the code and a stable
	// phase diagnostic, never an unsanitized reflection error message.
	return status.Error(codes.Code(code), "reflection request failed")
}
