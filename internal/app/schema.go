package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"relay/internal/domain"
	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/protobuf/registry"

	"github.com/bufbuild/protocompile/reporter"
)

const protoWorkTimeout = 30 * time.Second

// Compiling and inspecting schemas never holds the persistence mutex. A small
// process-wide work budget also bounds parallel descriptor/JSON allocations.
var protoWorkSlots = make(chan struct{}, 2)

func acquireProtoWork(ctx, lifecycle context.Context) (context.Context, func(), error) {
	bounded, cancel := context.WithTimeout(ctx, protoWorkTimeout)
	stop := context.AfterFunc(lifecycle, cancel)
	cleanup := func() { stop(); cancel() }
	if lifecycle.Err() != nil {
		cancel()
	}
	if err := bounded.Err(); err != nil {
		cleanup()
		return nil, nil, err
	}
	select {
	case protoWorkSlots <- struct{}{}:
		return bounded, func() { cleanup(); <-protoWorkSlots }, nil
	default:
		cleanup()
		return nil, nil, errors.New("Protobuf tools are busy; wait for the current import or validation and try again")
	}
}

func (s *Service) ImportProtoSchema(ctx context.Context, input ProtoImport) (ProtoImportResult, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return ProtoImportResult{}, err
	}
	defer done()
	if input.Kind == "" {
		input.Kind = "proto"
	}
	var compiled *registry.Registry
	switch input.Kind {
	case "proto":
		if input.DescriptorSetBase64 != "" {
			return s.failedProtoImport(ctx, errors.New("source imports cannot also contain a binary descriptor set"), nil)
		}
		if len(input.Sources) == 0 || len(input.Sources) > importer.MaxFiles {
			return s.failedProtoImport(ctx, fmt.Errorf("select between 1 and %d source files", importer.MaxFiles), nil)
		}
		sources := make(map[string]string, len(input.Sources))
		for _, source := range input.Sources {
			if _, duplicate := sources[source.Name]; duplicate {
				return s.failedProtoImport(ctx, errors.New("source import contains a duplicate virtual file path"), nil)
			}
			sources[source.Name] = source.Content
		}
		compiled, err = importer.CompileSources(work, sources, input.RootFiles)
	case "descriptor":
		if len(input.Sources) != 0 {
			return s.failedProtoImport(ctx, errors.New("descriptor imports cannot also contain source files"), nil)
		}
		data, decodeErr := decodeProtoArtifact(input.DescriptorSetBase64)
		if decodeErr != nil {
			err = decodeErr
		} else {
			compiled, err = importer.DescriptorSet(work, data)
		}
	default:
		err = errors.New("select a .proto source import or a binary descriptor set")
	}
	if err != nil {
		return s.failedProtoImport(ctx, err, nil)
	}
	return s.persistProtoImport(work, input, compiled)
}

// ImportProtoPaths is deliberately a package function: only the desktop file
// service may provide physical paths selected by native dialogs. Wails exposes
// the Service methods above, never this filesystem capability.
func ImportProtoPaths(ctx context.Context, s *Service, input ProtoImport, files, includeDirs []string) (ProtoImportResult, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return ProtoImportResult{}, err
	}
	defer done()
	if input.Kind == "" {
		input.Kind = "proto"
	}
	var compiled *registry.Registry
	switch input.Kind {
	case "proto":
		compiled, err = importer.Compile(work, files, includeDirs)
	case "descriptor":
		if len(files) != 1 {
			err = errors.New("select exactly one binary descriptor set file")
			break
		}
		var data []byte
		data, err = readProtoDescriptorFile(files[0])
		if err == nil {
			compiled, err = importer.DescriptorSet(work, data)
		}
	default:
		err = errors.New("select a .proto source import or a binary descriptor set")
	}
	if err != nil {
		physicalPaths := append(slices.Clone(files), includeDirs...)
		for _, file := range files {
			physicalPaths = append(physicalPaths, filepath.Dir(file))
		}
		return s.failedProtoImport(ctx, err, physicalPaths)
	}
	return s.persistProtoImport(work, input, compiled)
}

func readProtoDescriptorFile(filename string) ([]byte, error) {
	selected, err := os.Stat(filename)
	if err != nil || !selected.Mode().IsRegular() {
		return nil, errors.New("select a regular binary descriptor file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("selected binary descriptor file could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(selected, info) {
		return nil, errors.New("selected binary descriptor file changed while opening")
	}
	if info.Size() == 0 || info.Size() > importer.MaxTotalBytes {
		return nil, errors.New("binary descriptor file must contain between 1 byte and 16 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, importer.MaxTotalBytes+1))
	if err != nil || len(data) > importer.MaxTotalBytes {
		return nil, errors.New("selected binary descriptor file could not be read within the 16 MiB limit")
	}
	return data, nil
}

func decodeProtoArtifact(encoded string) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(importer.MaxTotalBytes) {
		return nil, errors.New("binary descriptor set must contain between 1 byte and 16 MiB")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > importer.MaxTotalBytes {
		return nil, errors.New("binary descriptor set is not valid bounded base64 data")
	}
	return data, nil
}

func (s *Service) failedProtoImport(ctx context.Context, cause error, physicalPaths []string) (ProtoImportResult, error) {
	if ctx.Err() != nil {
		return ProtoImportResult{}, ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return ProtoImportResult{}, fmt.Errorf("Protobuf import canceled or exceeded its 30-second limit: %w", cause)
	}
	state, err := s.LoadState(ctx)
	if err != nil {
		return ProtoImportResult{}, err
	}
	return ProtoImportResult{State: state, Diagnostics: []ProtoDiagnostic{protoDiagnostic(cause, physicalPaths)}, Warnings: []string{}}, nil
}

func protoDiagnostic(cause error, physicalPaths []string) ProtoDiagnostic {
	result := ProtoDiagnostic{Message: safeProtoDiagnosticMessage(cause)}
	var positioned reporter.ErrorWithPos
	if errors.As(cause, &positioned) {
		position := positioned.GetPosition()
		result.File, result.Line, result.Column = position.Filename, position.Line, position.Col
	}
	var validation *jsoncodec.ValidationError
	if errors.As(cause, &validation) {
		result.Path, result.Line, result.Column, result.Message = validation.Path, validation.Line, validation.Column, validation.Message
	}
	var syntax *jsoncodec.SyntaxError
	if errors.As(cause, &syntax) {
		result.Line, result.Column, result.Message = syntax.Line, syntax.Column, syntax.Message
	}
	// Native import diagnostics can contain include-directory paths. Virtual file
	// names remain useful while physical paths selected in the dialog stay in Go.
	paths := slices.Clone(physicalPaths)
	for _, name := range physicalPaths {
		if absolute, err := filepath.Abs(name); err == nil {
			paths = append(paths, absolute)
		}
		if resolved, err := filepath.EvalSymlinks(name); err == nil {
			paths = append(paths, resolved)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, name := range paths {
		if name != "" && name != "." && name != string(filepath.Separator) {
			result.Message = strings.ReplaceAll(result.Message, name, filepath.Base(name))
		}
	}
	if filepath.IsAbs(result.File) {
		result.File = filepath.Base(result.File)
	}
	result.Message = boundedProtoText(result.Message, 4096)
	result.File = boundedProtoText(result.File, 1024)
	return result
}

// The compiler may quote string literals, identifiers, defaults, and custom
// option values in errors. Offline tools do not know the environment's secret
// values, so classify the failure without copying arbitrary compiler text.
func safeProtoDiagnosticMessage(cause error) string {
	var missing *importer.MissingImportError
	if errors.As(cause, &missing) {
		return fmt.Sprintf("import %q not found; provide the dependency in selected sources or include directories", missing.Path)
	}
	message := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(message, "duplicate"), strings.Contains(message, "already defined"), strings.Contains(message, "conflict"):
		return "Protobuf schema contains a duplicate file path or conflicting definition; use unique names"
	case strings.Contains(message, "edition"):
		return "unsupported Protobuf edition or edition feature; this release supports edition 2023"
	case strings.Contains(message, "limit"), strings.Contains(message, "exceeds"), strings.Contains(message, "too many"), strings.Contains(message, "between 1"):
		return "Protobuf input exceeds a file, byte, nesting, or structural limit; reduce the selected schema"
	case strings.Contains(message, "symlink"), strings.Contains(message, "invalid import path"), strings.Contains(message, "invalid virtual import path"), strings.Contains(message, "invalid file path"), strings.Contains(message, "invalid dependency path"):
		return "invalid Protobuf import path; use relative paths inside the selected include directories"
	case strings.Contains(message, "ambiguous import"):
		return "ambiguous Protobuf import; select include directories containing one definition per virtual path"
	case strings.Contains(message, "not found"), strings.Contains(message, "missing"), strings.Contains(message, "could not resolve"):
		return "Protobuf dependency or definition is missing; provide imported files and check their declarations"
	case strings.Contains(message, "syntax"):
		return "invalid Protobuf syntax; review this position and use proto2, proto3, or edition 2023"
	case strings.Contains(message, "option"), strings.Contains(message, "default"):
		return "invalid Protobuf option or default value; check its type and allowed values"
	case strings.Contains(message, "base64"), strings.Contains(message, "descriptor"):
		return "invalid binary Protobuf descriptor set; select a complete FileDescriptorSet or import source files"
	default:
		return "invalid Protobuf schema; review the declaration at this position and its referenced types"
	}
}

func boundedProtoText(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	return text
}

func restoreProtoSchema(ctx context.Context, schema domain.ProtoSchema) (*registry.Registry, error) {
	data, err := decodeProtoArtifact(schema.DescriptorSetBase64)
	if err != nil {
		return nil, errors.New("stored Protobuf schema is unavailable; import it again")
	}
	digest := sha256.Sum256(data)
	if schema.Checksum != hex.EncodeToString(digest[:]) {
		return nil, errors.New("stored Protobuf schema checksum does not match; import it again")
	}
	reg, err := importer.DescriptorSet(ctx, data)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("stored Protobuf schema could not be restored; import it again")
	}
	return reg, nil
}

func (s *Service) schemaSnapshot(ctx context.Context, workspaceID, schemaID string) (domain.State, domain.ProtoSchema, error) {
	state, err := s.LoadState(ctx)
	if err != nil {
		return domain.State{}, domain.ProtoSchema{}, err
	}
	for _, schema := range state.ProtoSchemas {
		if schema.ID == schemaID && schema.WorkspaceID == workspaceID {
			return state, schema, nil
		}
	}
	return domain.State{}, domain.ProtoSchema{}, errors.New("selected Protobuf schema no longer exists in this workspace")
}

func protoRootNames(roots []string, files []registry.FileInfo) []string {
	selected := map[string]bool{}
	for _, root := range roots {
		root = filepath.ToSlash(filepath.Clean(root))
		for _, file := range files {
			if root == file.Name || strings.HasSuffix(root, "/"+file.Name) {
				selected[file.Name] = true
			}
		}
	}
	if len(selected) == 0 {
		dependencies := map[string]bool{}
		for _, file := range files {
			for _, dependency := range file.Imports {
				dependencies[dependency] = true
			}
		}
		for _, file := range files {
			if !dependencies[file.Name] {
				selected[file.Name] = true
			}
		}
	}
	result := make([]string, 0, len(selected))
	for name := range selected {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (s *Service) persistProtoImport(ctx context.Context, input ProtoImport, compiled *registry.Registry) (ProtoImportResult, error) {
	name := input.Name
	if strings.TrimSpace(name) == "" {
		name = "Protobuf schema"
	}
	name, err := cleanName(name)
	if err != nil {
		return ProtoImportResult{}, err
	}
	artifact, err := compiled.DescriptorSet()
	if err != nil || len(artifact) == 0 || len(artifact) > importer.MaxTotalBytes {
		return s.failedProtoImport(ctx, errors.New("compiled descriptor set exceeds the 16 MiB persistence limit"), nil)
	}
	if err := importer.ValidateDescriptorSetBudget(artifact); err != nil {
		return s.failedProtoImport(ctx, err, nil)
	}
	digest := sha256.Sum256(artifact)
	fileInfos := compiled.FilesInfo()
	files := make([]string, 0, len(fileInfos))
	for _, file := range fileInfos {
		files = append(files, file.Name)
	}
	roots := protoRootNames(input.RootFiles, fileInfos)
	warnings := []string{}
	if len(roots) > importer.MaxRootFiles {
		roots = roots[:importer.MaxRootFiles]
		warnings = append(warnings, "Only the first 256 root file names are listed; the complete descriptor set remains available.")
	}
	item := domain.ProtoSchema{ID: input.ID, WorkspaceID: input.WorkspaceID, Name: name, Kind: input.Kind,
		Checksum: hex.EncodeToString(digest[:]), ImportedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RootFiles: roots, Files: files,
		DescriptorSetBase64: base64.StdEncoding.EncodeToString(artifact)}
	var previous domain.ProtoSchema
	var before *registry.Registry
	methodChanges := map[string]string{}
	if item.ID == "" {
		item.ID = newID()
	} else {
		var snapshot domain.State
		snapshot, previous, err = s.schemaSnapshot(ctx, item.WorkspaceID, item.ID)
		if err != nil {
			return ProtoImportResult{}, err
		}
		before, err = restoreProtoSchema(ctx, previous)
		if err != nil {
			return ProtoImportResult{}, err
		}
		beforeShape, afterShape := protoShapeCache(ctx), protoShapeCache(ctx)
		for _, request := range snapshot.Requests {
			if request.WorkspaceID != item.WorkspaceID || request.GRPC == nil || request.GRPC.SchemaID != item.ID {
				continue
			}
			method := request.GRPC.Method
			if _, alreadyCompared := methodChanges[method]; alreadyCompared {
				continue
			}
			if err := ctx.Err(); err != nil {
				return ProtoImportResult{}, err
			}
			afterMethod, resolveErr := compiled.Method(method)
			if resolveErr != nil {
				methodChanges[method] = "its selected RPC method is missing from the imported schema."
				continue
			}
			beforeMethod, resolveErr := before.Method(method)
			if resolveErr != nil || changedProtoMethod(beforeMethod, afterMethod, beforeShape, afterShape) {
				methodChanges[method] = "its RPC message schema or streaming type changed; validate the body again."
			} else {
				methodChanges[method] = ""
			}
		}
	}
	if len(compiled.Methods()) == 0 {
		warnings = append(warnings, "The schema contains no RPC methods; its message types are available in the schema inspector.")
	}
	omittedWarnings := 0
	state, err := s.mutate(ctx, func(state *domain.State) error {
		if !slices.ContainsFunc(state.Workspaces, func(workspace domain.Workspace) bool { return workspace.ID == item.WorkspaceID }) {
			return errors.New("selected workspace no longer exists")
		}
		if previous.ID != "" {
			current := slices.IndexFunc(state.ProtoSchemas, func(schema domain.ProtoSchema) bool {
				return schema.ID == previous.ID && schema.WorkspaceID == previous.WorkspaceID
			})
			if current < 0 || state.ProtoSchemas[current].Checksum != previous.Checksum || state.ProtoSchemas[current].ImportedAt != previous.ImportedAt {
				return errors.New("the schema changed during import; refresh and import again")
			}
			for _, request := range state.Requests {
				if request.WorkspaceID != item.WorkspaceID || request.GRPC == nil || request.GRPC.SchemaID != item.ID {
					continue
				}
				change, compared := methodChanges[request.GRPC.Method]
				if !compared {
					change = "the schema was reimported while this request changed; validate its method and body again."
				}
				if change != "" {
					if len(warnings) < 255 {
						warnings = append(warnings, fmt.Sprintf("Saved request %q: %s", request.Name, change))
					} else {
						omittedWarnings++
					}
				}
			}
		}
		state.ProtoSchemas = upsert(state.ProtoSchemas, item, item.ID, func(schema domain.ProtoSchema) string { return schema.ID })
		return nil
	})
	if err != nil {
		return ProtoImportResult{}, err
	}
	if omittedWarnings > 0 {
		warnings = append(warnings, fmt.Sprintf("%d additional saved requests require validation after this schema change.", omittedWarnings))
	}
	return ProtoImportResult{State: state, SchemaID: item.ID, Diagnostics: []ProtoDiagnostic{}, Warnings: warnings}, nil
}

func (s *Service) DeleteProtoSchema(ctx context.Context, workspaceID, schemaID string) (domain.State, error) {
	return s.mutate(ctx, func(state *domain.State) error {
		index := slices.IndexFunc(state.ProtoSchemas, func(schema domain.ProtoSchema) bool {
			return schema.ID == schemaID && schema.WorkspaceID == workspaceID
		})
		if index < 0 {
			return errors.New("selected Protobuf schema no longer exists in this workspace")
		}
		state.ProtoSchemas = slices.Delete(state.ProtoSchemas, index, index+1)
		for i := range state.Requests {
			request := &state.Requests[i]
			if request.WorkspaceID == workspaceID && request.GRPC != nil && request.GRPC.SchemaID == schemaID {
				definition := *request.GRPC
				definition.SchemaID = ""
				request.GRPC = &definition
			}
		}
		return nil
	})
}

func (s *Service) InspectProtoSchema(ctx context.Context, workspaceID, schemaID string) (ProtoSchemaDetails, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return ProtoSchemaDetails{}, err
	}
	defer done()
	_, schema, err := s.schemaSnapshot(work, workspaceID, schemaID)
	if err != nil {
		return ProtoSchemaDetails{}, err
	}
	reg, err := restoreProtoSchema(work, schema)
	if err != nil {
		return ProtoSchemaDetails{}, err
	}
	return ProtoSchemaDetails{Schema: schema, Files: reg.FilesInfo(), Services: reg.Services(), Messages: reg.Messages()}, work.Err()
}

func (s *Service) InspectProtoMethod(ctx context.Context, workspaceID, schemaID, method string) (ProtoMethodDetails, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return ProtoMethodDetails{}, err
	}
	defer done()
	_, schema, err := s.schemaSnapshot(work, workspaceID, schemaID)
	if err != nil {
		return ProtoMethodDetails{}, err
	}
	reg, err := restoreProtoSchema(work, schema)
	if err != nil {
		return ProtoMethodDetails{}, err
	}
	descriptor, err := reg.Method(method)
	if err != nil {
		return ProtoMethodDetails{}, errors.New("selected RPC method is not present in the imported schema")
	}
	template, err := jsoncodec.Template(reg, descriptor.Input())
	if err != nil {
		return ProtoMethodDetails{}, err
	}
	result := ProtoMethodDetails{Template: string(template)}
	for _, info := range reg.Methods() {
		if info.FullMethod == method {
			result.Method = info
			break
		}
	}
	for _, info := range reg.Messages() {
		if info.Name == string(descriptor.Input().FullName()) {
			result.Input = info
		}
		if info.Name == string(descriptor.Output().FullName()) {
			result.Output = info
		}
	}
	return result, work.Err()
}

func (s *Service) ValidateProtoMessage(ctx context.Context, input ProtoValidationRequest) (ProtoValidationResult, error) {
	result := ProtoValidationResult{Diagnostics: []ProtoDiagnostic{}, Warnings: []string{}}
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return result, err
	}
	defer done()
	_, schema, err := s.schemaSnapshot(work, input.WorkspaceID, input.SchemaID)
	if err != nil {
		return result, err
	}
	reg, err := restoreProtoSchema(work, schema)
	if err != nil {
		return result, err
	}
	descriptor, err := reg.Method(input.Method)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, ProtoDiagnostic{Message: "selected RPC method is not present in the imported schema"})
		return result, nil
	}
	format := jsoncodec.Format(input.Format)
	if format == "" {
		format = jsoncodec.StrictJSON
	}
	if format != jsoncodec.StrictJSON && format != jsoncodec.JSON5Like {
		result.Diagnostics = append(result.Diagnostics, ProtoDiagnostic{Message: "choose Strict ProtoJSON or the supported JSON5-like format"})
		return result, nil
	}
	message, err := jsoncodec.Decode(reg, descriptor.Input(), input.Body, format)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, protoDiagnostic(err, nil))
		return result, nil
	}
	wire, err := jsoncodec.EncodeBinary(message)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, protoDiagnostic(err, nil))
		return result, nil
	}
	// Decode the actual wire before rendering the canonical preview. This proves
	// that binary serialization, not just JSON normalization, succeeded.
	roundTrip, err := jsoncodec.DecodeBinary(reg, descriptor.Input(), wire)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, protoDiagnostic(err, nil))
		return result, nil
	}
	canonical, err := jsoncodec.Encode(reg, roundTrip)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, protoDiagnostic(err, nil))
		return result, nil
	}
	if err = work.Err(); err != nil {
		return result, err
	}
	result.Valid = true
	result.CanonicalJSON = string(canonical)
	result.WireBase64 = base64.StdEncoding.EncodeToString(wire)
	result.WireBytes = len(wire)
	if strings.Contains(input.Body, "{{") {
		result.Warnings = append(result.Warnings, "Environment variables are not resolved during offline validation; string placeholders remain literal strings.")
	}
	return result, nil
}
