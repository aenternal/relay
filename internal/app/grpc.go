package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"relay/internal/domain"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/protobuf/registry"
	"relay/internal/security"
	"relay/internal/transport/grpcclient"

	_ "google.golang.org/genproto/googleapis/rpc/errdetails" // standard google.rpc status detail descriptors
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const grpcResultBudget = 64 << 20

type grpcRecord struct {
	result  domain.GRPCExecution
	cancel  context.CancelFunc
	reserve int
	stream  *grpcStreamRecord
}

type grpcState struct {
	records      map[string]*grpcRecord
	reserved     int
	closed       bool
	work         sync.WaitGroup
	reflectionMu sync.Mutex
	reflections  map[string]string
}

// initGRPC is called with grpcMu held. Execution state is deliberately transient:
// request/response messages, peer status and metadata never enter SQLite.
func (s *Service) initGRPC() *grpcState {
	if s.grpcState == nil {
		s.grpcState = &grpcState{records: map[string]*grpcRecord{}, reflections: map[string]string{}}
	}
	return s.grpcState
}

func (s *Service) StartGRPCUnary(ctx context.Context, request domain.RequestDefinition) (domain.GRPCExecution, error) {
	return s.startGRPC(ctx, request, "unary")
}

func (s *Service) StartGRPCStream(ctx context.Context, request domain.RequestDefinition) (domain.GRPCExecution, error) {
	return s.startGRPC(ctx, request, "stream")
}

// StartGRPCReflection discovers descriptors only after the explicit UI action.
// Success persists a self-contained schema and returns its ID; ordinary unary
// calls use the selected local schema and never perform implicit reflection.
func (s *Service) StartGRPCReflection(ctx context.Context, request domain.RequestDefinition) (domain.GRPCExecution, error) {
	return s.startGRPC(ctx, request, "reflection")
}

func (s *Service) startGRPC(ctx context.Context, request domain.RequestDefinition, kind string) (domain.GRPCExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.GRPCExecution{}, err
	}
	if request.Protocol != "grpc" || request.GRPC == nil {
		return domain.GRPCExecution{}, errors.New("select a gRPC request")
	}
	timeout, receive, err := grpcLimits(*request.GRPC)
	if err != nil {
		return domain.GRPCExecution{}, err
	}
	if len(request.GRPC.Metadata) > 1024 || len(request.GRPC.Body) > security.MaxTemplateBytes || len(request.GRPC.Endpoint) > 65536 || len(request.GRPC.Method) > 1024 || len(request.GRPC.Authority) > 1024 {
		return domain.GRPCExecution{}, errors.New("gRPC request definition exceeds its size limit")
	}
	// Copy mutable slices before the bridge invocation returns to its caller.
	definition := *request.GRPC
	definition.Metadata = slices.Clone(definition.Metadata)
	request.GRPC = &definition
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	work, cancel := context.WithTimeout(base, timeout)
	result := domain.GRPCExecution{ID: newID(), RequestID: request.ID, Kind: kind, State: "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Headers: []domain.Header{}, Trailers: []domain.Header{},
		Details: []domain.GRPCStatusDetail{}, Warnings: []string{}}
	// Maximum retained JSON, wire base64, status details and metadata. Active
	// workers reserve their whole result before allocating or connecting.
	reserve := jsoncodec.MaxOutputBytes + base64.StdEncoding.EncodedLen(receive) + (1 << 20) + (256 << 10)
	if kind == "stream" {
		reserve = grpcStreamReservation
		result.StreamState = "connecting"
	}
	s.grpcMu.Lock()
	state := s.initGRPC()
	if state.closed {
		s.grpcMu.Unlock()
		cancel()
		return domain.GRPCExecution{}, errors.New("application is shutting down")
	}
	active := 0
	for _, record := range state.records {
		if record.result.State == "running" {
			active++
		}
	}
	if active >= 4 {
		s.grpcMu.Unlock()
		cancel()
		return domain.GRPCExecution{}, errors.New("at most four gRPC operations may run concurrently")
	}
	for len(state.records) >= 16 || state.reserved+reserve > grpcResultBudget {
		victimID := ""
		var victim *grpcRecord
		for id, record := range state.records {
			if record.stream != nil && len(record.stream.queue) > 0 {
				continue
			}
			if record.result.State != "running" && (victim == nil || record.result.StartedAt < victim.result.StartedAt) {
				victimID, victim = id, record
			}
		}
		if victim == nil {
			s.grpcMu.Unlock()
			cancel()
			return domain.GRPCExecution{}, errors.New("gRPC response memory limit reached; finish reading or clear a completed response, or cancel another operation")
		}
		state.reserved -= victim.reserve
		delete(state.records, victimID)
	}
	record := &grpcRecord{result: result, cancel: cancel, reserve: reserve}
	if kind == "stream" {
		record.stream = &grpcStreamRecord{ctx: work, changed: make(chan struct{}), sendBusy: make(chan struct{}, 1)}
	}
	state.records[result.ID] = record
	state.reserved += reserve
	state.work.Add(1)
	s.grpcMu.Unlock()
	go s.runGRPC(work, request, record)
	return cloneGRPCExecution(result), nil
}

func (s *Service) GetGRPCExecution(ctx context.Context, id string) (domain.GRPCExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.GRPCExecution{}, err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	record, ok := s.initGRPC().records[id]
	if !ok {
		return domain.GRPCExecution{}, errors.New("gRPC execution is no longer available")
	}
	return cloneGRPCExecution(record.result), nil
}

func (s *Service) CancelGRPCExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	record, ok := s.initGRPC().records[id]
	if !ok {
		return errors.New("gRPC execution is no longer available")
	}
	record.cancel()
	return nil
}

func (s *Service) ForgetGRPCExecution(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.grpcMu.Lock()
	defer s.grpcMu.Unlock()
	state := s.initGRPC()
	record, ok := state.records[id]
	if !ok {
		return nil
	}
	if record.result.State == "running" {
		return errors.New("cancel the gRPC operation before clearing its response")
	}
	state.reserved -= record.reserve
	delete(state.records, id)
	return nil
}

func ShutdownGRPC(s *Service) error {
	s.grpcMu.Lock()
	state := s.initGRPC()
	state.closed = true
	for _, record := range state.records {
		record.cancel()
	}
	s.grpcMu.Unlock()
	state.work.Wait()
	s.grpcMu.Lock()
	clear(state.records)
	clear(state.reflections)
	state.reserved = 0
	s.grpcMu.Unlock()
	return nil
}

func cloneGRPCExecution(result domain.GRPCExecution) domain.GRPCExecution {
	result.Headers = slices.Clone(result.Headers)
	result.Trailers = slices.Clone(result.Trailers)
	result.Warnings = slices.Clone(result.Warnings)
	result.Details = slices.Clone(result.Details)
	if result.TLS != nil {
		copy := *result.TLS
		copy.DNSNames = slices.Clone(copy.DNSNames)
		result.TLS = &copy
	}
	return result
}

func (s *Service) runGRPC(ctx context.Context, request domain.RequestDefinition, record *grpcRecord) {
	if record.stream != nil {
		s.runGRPCStream(ctx, request, record)
		return
	}
	started := time.Now()
	result := record.result
	var prepared *grpcPrepared
	var failure error
	defer func() {
		contextErr := ctx.Err()
		record.cancel()
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		result.TotalMS = float64(time.Since(started)) / float64(time.Millisecond)
		secrets := []string{}
		if prepared != nil {
			secrets = prepared.secrets
		}
		if failure != nil {
			result.State = "failed"
			if result.Code == 0 {
				result.Code, result.CodeName = int(codes.Unknown), codes.Unknown.String()
			}
			result.Error = grpcSafeText(failure.Error(), secrets, 4096)
		} else {
			result.State = "completed"
		}
		// Cancellation is assessed before cancel() above changes the context.
		if contextErr != nil && failure != nil {
			if errors.Is(contextErr, context.DeadlineExceeded) {
				result.Code, result.CodeName = int(codes.DeadlineExceeded), codes.DeadlineExceeded.String()
				result.Error = "gRPC operation exceeded its deadline"
			} else if result.Code != int(codes.OK) {
				result.State = "canceled"
				result.Code, result.CodeName = int(codes.Canceled), codes.Canceled.String()
				result.Error = "gRPC operation canceled"
			}
		}
		s.grpcMu.Lock()
		record.result = result
		s.grpcMu.Unlock()
		s.grpcState.work.Done()
	}()
	prepared, failure = s.prepareGRPC(ctx, request, result.Kind == "reflection")
	if failure != nil {
		result.Code, result.CodeName = int(codes.InvalidArgument), codes.InvalidArgument.String()
		return
	}
	result.Endpoint = grpcSafeText(prepared.endpoint, prepared.secrets, 65536)
	if prepared.options.Plaintext {
		result.Warnings = append(result.Warnings, "This gRPC connection uses plaintext")
		if request.GRPC.TLS != (domain.HTTPTLS{}) {
			result.Warnings = append(result.Warnings, "Configured TLS settings are inactive in plaintext mode")
		}
	} else if prepared.options.TLSConfig.InsecureSkipVerify {
		result.Warnings = append(result.Warnings, "TLS certificate verification is disabled for this request")
	}
	client, err := grpcclient.Dial(ctx, prepared.endpoint, prepared.options)
	if err != nil {
		failure = err
		result.Code, result.CodeName = int(codes.Unavailable), codes.Unavailable.String()
		return
	}
	defer client.Close()
	result.ConnectMS = float64(client.ConnectDuration()) / float64(time.Millisecond)
	if result.Kind == "reflection" {
		var reflected *grpcclient.ReflectionResult
		reflected, failure = client.Reflection(ctx, prepared.metadata, grpcclient.ReflectionOptions{})
		if failure != nil {
			code := status.Code(failure)
			result.Code, result.CodeName = int(code), code.String()
			if code == codes.Unimplemented {
				failure = errors.New("server reflection is disabled or unsupported; import a local .proto schema or descriptor set")
			}
			return
		}
		if grpcContainsSecret(reflected.DescriptorSet, prepared.secrets) {
			failure = errors.New("reflected schema contains sensitive request values and was not saved")
			result.Code, result.CodeName = int(codes.FailedPrecondition), codes.FailedPrecondition.String()
			return
		}
		var imported ProtoImportResult
		imported, failure = s.persistGRPCReflection(ctx, request, prepared, reflected.Registry)
		if failure != nil {
			return
		}
		if len(imported.Diagnostics) > 0 || imported.SchemaID == "" {
			failure = errors.New("reflected schema could not be saved")
			return
		}
		result.SchemaID = imported.SchemaID
		result.ReflectionProtocol = reflected.Protocol
		result.Warnings = append(result.Warnings, imported.Warnings...)
		result.Code, result.CodeName = int(codes.OK), codes.OK.String()
		return
	}
	result.Method, _ = grpcclient.FullMethod(prepared.method)
	result.SchemaID = request.GRPC.SchemaID
	unary, err := client.Unary(ctx, prepared.method, prepared.message, prepared.metadata)
	failure = err
	if unary == nil {
		if err != nil {
			code := status.Code(err)
			result.Code, result.CodeName = int(code), code.String()
		}
		return
	}
	result.Headers = grpcSafeMetadata(unary.Headers, prepared.secrets)
	result.Trailers = grpcSafeMetadata(unary.Trailers, prepared.secrets)
	if unary.Peer != nil {
		if info, ok := unary.Peer.AuthInfo.(credentials.TLSInfo); ok {
			result.TLS = safeHTTPTLS(&info.State, prepared.secrets)
		}
	}
	if unary.Status != nil {
		result.Code, result.CodeName = int(unary.Status.Code()), unary.Status.Code().String()
		result.StatusMessage = grpcSafeText(unary.Status.Message(), prepared.secrets, 16384)
		result.Details, result.Truncated = grpcStatusDetails(prepared.registry, unary.Status, prepared.secrets)
	}
	if unary.Message == nil {
		return
	}
	wire := unary.Wire
	result.WireBytes = len(wire)
	if grpcContainsSecret(wire, prepared.secrets) {
		result.Warnings = append(result.Warnings, "Sensitive values were redacted from the response; its binary preview is unavailable")
	} else {
		result.WireBase64 = base64.StdEncoding.EncodeToString(wire)
	}
	encoded, err := jsoncodec.Encode(prepared.registry, unary.Message)
	if err != nil {
		result.Warnings = append(result.Warnings, "Response ProtoJSON is unavailable; inspect the binary preview or import its missing Any types")
		return
	}
	result.MessageJSON = redactHTTP(string(encoded), prepared.secrets, jsoncodec.MaxOutputBytes+1)
	if result.MessageJSON != string(encoded) && result.WireBase64 != "" {
		result.WireBase64 = ""
		result.Warnings = append(result.Warnings, "Sensitive values were redacted from the response; its binary preview is unavailable")
	}
	if len(result.MessageJSON) > jsoncodec.MaxOutputBytes || !json.Valid([]byte(result.MessageJSON)) {
		result.MessageJSON = ""
		result.Truncated = true
		result.Warnings = append(result.Warnings, "Response JSON preview was omitted after sensitive values were redacted")
	}
}

// Reflection refreshes the previously discovered schema for the same live
// connection profile. The fingerprint includes credentials, but neither it nor
// those credentials are persisted. Saved schemas remain usable after restart.
func (s *Service) persistGRPCReflection(ctx context.Context, request domain.RequestDefinition, prepared *grpcPrepared, reg *registry.Registry) (ProtoImportResult, error) {
	serverName := ""
	if prepared.options.TLSConfig != nil {
		serverName = prepared.options.TLSConfig.ServerName
	}
	profile, _ := json.Marshal(struct {
		Workspace, Endpoint, Authority, ServerName string
		Plaintext                                  bool
		TLS                                        domain.HTTPTLS
		Metadata                                   metadata.MD
	}{request.WorkspaceID, prepared.endpoint, prepared.options.Authority, serverName, prepared.options.Plaintext, request.GRPC.TLS, prepared.metadata})
	digest := sha256.Sum256(profile)
	key := hex.EncodeToString(digest[:])
	clear(profile)
	state := s.grpcState
	state.reflectionMu.Lock()
	defer state.reflectionMu.Unlock()
	s.grpcMu.Lock()
	id := state.reflections[key]
	s.grpcMu.Unlock()
	name := "Reflection " + grpcSafeText(prepared.endpoint, prepared.secrets, 100)
	if id != "" {
		_, previous, err := s.schemaSnapshot(ctx, request.WorkspaceID, id)
		if err != nil {
			if ctx.Err() != nil {
				return ProtoImportResult{}, ctx.Err()
			}
			id = ""
		} else if previous.Kind != "descriptor" {
			id = ""
		} else {
			name = previous.Name
		}
	}
	imported, err := s.persistProtoImport(ctx, ProtoImport{ID: id, WorkspaceID: request.WorkspaceID, Name: name, Kind: "descriptor"}, reg)
	if err == nil && imported.SchemaID != "" {
		s.grpcMu.Lock()
		if len(state.reflections) >= 256 {
			for old := range state.reflections {
				delete(state.reflections, old)
				break
			}
		}
		state.reflections[key] = imported.SchemaID
		s.grpcMu.Unlock()
	}
	return imported, err
}

func grpcSafeText(value string, secrets []string, limit int) string {
	return boundedProtoText(redactHTTP(value, secrets, limit), limit)
}

func grpcContainsSecret(value []byte, secrets []string) bool {
	for _, pattern := range redactionSecretVariants(secrets) {
		if bytes.Contains(value, []byte(pattern)) {
			return true
		}
	}
	return false
}

func sensitiveGRPCMetadata(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), "-bin")
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(name)
	return sensitiveHTTPHeader(name) || normalized == "passwd" || strings.Contains(normalized, "signature") || strings.Contains(normalized, "credential")
}

func grpcSafeMetadata(md metadata.MD, secrets []string) []domain.Header {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := []domain.Header{}
	budget := 64 << 10
	for _, key := range keys {
		if key == "grpc-status-details-bin" {
			continue // rendered as bounded status details below
		}
		for _, raw := range md[key] {
			value := raw
			if sensitiveGRPCMetadata(key) || grpcContainsSecret([]byte(raw), secrets) {
				value = security.RedactedValue
			} else if strings.HasSuffix(key, "-bin") {
				value = base64.StdEncoding.EncodeToString([]byte(raw))
			} else {
				value = grpcSafeText(raw, secrets, 8192)
			}
			budget -= len(key) + len(value)
			if budget < 0 {
				return result
			}
			result = append(result, domain.Header{Name: grpcSafeText(key, secrets, 1024), Value: value, Enabled: true})
		}
	}
	return result
}

func grpcStatusDetails(reg *registry.Registry, rpcStatus *status.Status, secrets []string) ([]domain.GRPCStatusDetail, bool) {
	result := []domain.GRPCStatusDetail{}
	budget := 1 << 20
	for _, detail := range rpcStatus.Proto().Details {
		if len(result) >= 128 || len(detail.Value) > budget {
			return result, true
		}
		item := domain.GRPCStatusDetail{TypeURL: grpcSafeText(detail.TypeUrl, secrets, 1024)}
		if !grpcContainsSecret(detail.Value, secrets) {
			item.WireBase64 = base64.StdEncoding.EncodeToString(detail.Value)
		} else {
			item.Error = "Sensitive values were redacted; binary detail is unavailable"
		}
		messageType, err := reg.Types.FindMessageByURL(detail.TypeUrl)
		if err != nil && strings.HasPrefix(detail.TypeUrl, "type.googleapis.com/google.rpc.") {
			messageType, err = protoregistry.GlobalTypes.FindMessageByURL(detail.TypeUrl)
		}
		if err != nil {
			item.Error = "Detail type is not present in the selected schema; raw protobuf bytes are shown when safe"
		} else {
			message := dynamicpb.NewMessage(messageType.Descriptor())
			if err = (proto.UnmarshalOptions{RecursionLimit: jsoncodec.MaxDepth, Resolver: reg.Types}).Unmarshal(detail.Value, message); err == nil {
				var encoded []byte
				encoded, err = jsoncodec.Encode(reg, message)
				if err == nil {
					item.MessageJSON = redactHTTP(string(encoded), secrets, (1<<20)+1)
					if item.MessageJSON != string(encoded) && item.WireBase64 != "" {
						item.WireBase64 = ""
						item.Error = "Sensitive values were redacted; binary detail is unavailable"
					}
					if len(item.MessageJSON) > 1<<20 || !json.Valid([]byte(item.MessageJSON)) {
						item.MessageJSON = ""
						item.Error = "Status detail JSON preview was omitted after sensitive values were redacted"
					}
				}
			}
			if err != nil {
				item.Error = "Status detail could not be decoded with the selected schema"
			}
		}
		budget -= len(item.TypeURL) + len(item.MessageJSON) + len(item.WireBase64) + len(item.Error)
		if budget < 0 {
			return result, true
		}
		result = append(result, item)
	}
	return result, false
}
