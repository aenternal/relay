package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/protobuf/importer"
	"relay/internal/protobuf/jsoncodec"
	"relay/internal/security"
)

const schemaRoot = `syntax = "proto3";
package sample;
import "deps/shared.proto";
import "google/protobuf/any.proto";
import "google/protobuf/timestamp.proto";
message Input {
  int64 count = 1;
  repeated string names = 2;
  map<string, int64> totals = 3;
  shared.Nested nested = 4;
  oneof pick { string token = 5; int32 code = 6; }
  google.protobuf.Any payload = 7;
  google.protobuf.Timestamp at = 8;
  optional bool enabled = 9;
}
message Output { string reply = 1; }
service Echo {
  rpc Call(Input) returns (Output);
  rpc Watch(Input) returns (stream Output);
  rpc Upload(stream Input) returns (Output);
  rpc Chat(stream Input) returns (stream Output);
}`

const schemaDependency = `syntax = "proto3"; package shared; message Nested { string label = 1; }`

func schemaInput(workspaceID string) app.ProtoImport {
	return app.ProtoImport{WorkspaceID: workspaceID, Name: "Sample schema", Kind: "proto", RootFiles: []string{"api.proto"}, Sources: []app.ProtoSource{
		{Name: "api.proto", Content: schemaRoot}, {Name: "deps/shared.proto", Content: schemaDependency},
	}}
}

func importSchema(t *testing.T, svc *app.Service, input app.ProtoImport) app.ProtoImportResult {
	t.Helper()
	result, err := svc.ImportProtoSchema(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 || result.SchemaID == "" {
		t.Fatalf("schema import failed: %+v", result.Diagnostics)
	}
	return result
}

func TestProtoImportPersistsPrivateArtifactAndValidatesRealWireAfterRestart(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "relay.sqlite3")
	vault := security.NewMemoryStore()
	db, svc := openApp(t, filename, vault)
	initial := loadState(t, svc)
	input := schemaInput(initial.Workspaces[0].ID)
	imported := importSchema(t, svc, input)
	if imported.State.Revision <= initial.Revision || len(imported.State.ProtoSchemas) != 1 {
		t.Fatal("import did not atomically save the schema")
	}
	schema := imported.State.ProtoSchemas[0]
	if schema.Checksum == "" || schema.DescriptorSetBase64 == "" || !slices.Contains(schema.Files, "deps/shared.proto") || !slices.Contains(schema.Files, "google/protobuf/any.proto") {
		t.Fatal("schema is not self-contained")
	}
	bridgeJSON, err := json.Marshal(imported)
	if err != nil || strings.Contains(string(bridgeJSON), schema.DescriptorSetBase64) || strings.Contains(string(bridgeJSON), "descriptorSetBase64") {
		t.Fatal("private artifact was exposed by the application snapshot")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, restarted := openApp(t, filename, vault)
	details, err := restarted.InspectProtoSchema(t.Context(), schema.WorkspaceID, schema.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Services) != 1 || len(details.Services[0].Methods) != 4 || len(details.Messages) < 3 {
		t.Fatalf("restored schema inspection is incomplete: %+v", details.Services)
	}
	for _, method := range details.Services[0].Methods {
		if !strings.HasPrefix(method.FullMethod, "/sample.Echo/") {
			t.Fatalf("method is not fully qualified: %s", method.FullMethod)
		}
	}
	method, err := restarted.InspectProtoMethod(t.Context(), schema.WorkspaceID, schema.ID, "/sample.Echo/Call")
	if err != nil || method.Input.Name != "sample.Input" || method.Output.Name != "sample.Output" || method.Template == "" {
		t.Fatalf("method/template inspection: %+v, %v", method, err)
	}
	check := app.ProtoValidationRequest{WorkspaceID: schema.WorkspaceID, SchemaID: schema.ID, Method: method.Method.FullMethod, Body: method.Template, Format: "json"}
	validTemplate, err := restarted.ValidateProtoMessage(t.Context(), check)
	if err != nil || !validTemplate.Valid {
		t.Fatalf("generated template is invalid: %+v %v", validTemplate.Diagnostics, err)
	}
	const bodyCanary = "offline-body-must-never-persist-f792"
	check.Format = "json5"
	check.Body = `{ // retain exact numeric lexemes
  count: 9223372036854775807,
  names: ['first', 'second',],
  totals: {max: '9223372036854775807'},
  nested: {label: '` + bodyCanary + `'},
  token: '{{secretStillLiteral}}',
  payload: {'@type': 'type.googleapis.com/shared.Nested', label: 'dynamic'},
  at: '2026-10-24T00:00:00Z',
  enabled: false,
}`
	result, err := restarted.ValidateProtoMessage(t.Context(), check)
	if err != nil || !result.Valid || result.WireBytes < 1 || len(result.Warnings) == 0 {
		t.Fatalf("validation failed: %+v %v", result, err)
	}
	if !strings.Contains(result.CanonicalJSON, `"9223372036854775807"`) || !strings.Contains(result.CanonicalJSON, "{{secretStillLiteral}}") {
		t.Fatalf("canonical JSON rounded integers or resolved secrets: %s", result.CanonicalJSON)
	}
	wire, err := base64.StdEncoding.DecodeString(result.WireBase64)
	if err != nil || len(wire) != result.WireBytes {
		t.Fatal("wire preview is not bounded binary data")
	}
	reg, err := importer.CompileSources(t.Context(), map[string]string{"api.proto": schemaRoot, "deps/shared.proto": schemaDependency}, []string{"api.proto"})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := reg.Method(check.Method)
	if err != nil {
		t.Fatal(err)
	}
	message, err := jsoncodec.DecodeBinary(reg, descriptor.Input(), wire)
	if err != nil || message.Get(descriptor.Input().Fields().ByName("count")).Int() != 9223372036854775807 {
		t.Fatalf("binary wire did not roundtrip the int64: %v", err)
	}
	second, err := restarted.ValidateProtoMessage(t.Context(), check)
	if err != nil || result.WireBase64 != second.WireBase64 {
		t.Fatal("identical messages produced different deterministic wire bytes")
	}
	after := loadState(t, restarted)
	if after.Revision != imported.State.Revision {
		t.Fatal("offline validation wrote application state")
	}
	assertNoSecretOnDisk(t, dir, bodyCanary, "secretStillLiteral")
}

func TestProtoImportDiagnosticsAndWorkspaceIsolation(t *testing.T) {
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite3"), security.NewMemoryStore())
	state := loadState(t, svc)
	input := schemaInput(state.Workspaces[0].ID)
	bad := input
	bad.Sources = bad.Sources[:1]
	failed, err := svc.ImportProtoSchema(t.Context(), bad)
	if err != nil || len(failed.Diagnostics) != 1 || failed.SchemaID != "" || failed.State.Revision != state.Revision || len(failed.State.ProtoSchemas) != 0 {
		t.Fatalf("missing dependency did not leave state unchanged: %+v %v", failed.Diagnostics, err)
	}
	if !strings.Contains(failed.Diagnostics[0].Message, "deps/shared.proto") || failed.Diagnostics[0].File == "" || failed.Diagnostics[0].Line < 1 {
		t.Fatalf("missing dependency lost source position/include hint: %+v", failed.Diagnostics)
	}
	bad = input
	bad.Sources = append(slices.Clone(input.Sources), input.Sources[0])
	failed, err = svc.ImportProtoSchema(t.Context(), bad)
	if err != nil || len(failed.Diagnostics) == 0 || !strings.Contains(failed.Diagnostics[0].Message, "duplicate") {
		t.Fatal("duplicate virtual path was silently overwritten")
	}
	bad = input
	bad.Sources = []app.ProtoSource{{Name: "../escape.proto", Content: schemaRoot}}
	bad.RootFiles = []string{"../escape.proto"}
	failed, err = svc.ImportProtoSchema(t.Context(), bad)
	if err != nil || len(failed.Diagnostics) == 0 {
		t.Fatal("traversal virtual path was accepted")
	}
	imported := importSchema(t, svc, input)
	state = mustState(t)(svc.SaveWorkspace(t.Context(), domain.Workspace{Name: "Other"}))
	otherID := state.Workspaces[len(state.Workspaces)-1].ID
	if _, err := svc.InspectProtoSchema(t.Context(), otherID, imported.SchemaID); err == nil {
		t.Fatal("schema could be inspected from another workspace")
	}
	if _, err := svc.DeleteProtoSchema(t.Context(), otherID, imported.SchemaID); err == nil {
		t.Fatal("schema could be deleted from another workspace")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := svc.ImportProtoSchema(canceled, input); err == nil {
		t.Fatal("canceled import succeeded")
	}
}

func TestProtoValidationDiagnosticsAreTransientAndExcludeBodyValues(t *testing.T) {
	dir := t.TempDir()
	_, svc := openApp(t, filepath.Join(dir, "relay.sqlite3"), security.NewMemoryStore())
	state := loadState(t, svc)
	imported := importSchema(t, svc, schemaInput(state.Workspaces[0].ID))
	const canary = "invalid-field-secret-canary-9e78"
	for _, body := range []string{
		`{"count":"` + canary + `"}`,
		`{"token":"` + canary + `","code":2}`,
		`{"names":"` + canary + `"}`,
		`{"totals":{"` + canary + `":false}}`,
		`{"payload":{"@type":"` + canary + `","secret":"x"}}`,
		`{"` + canary + `":1}`,
	} {
		result, err := svc.ValidateProtoMessage(t.Context(), app.ProtoValidationRequest{WorkspaceID: state.Workspaces[0].ID, SchemaID: imported.SchemaID, Method: "/sample.Echo/Call", Format: "json", Body: body})
		if err != nil || result.Valid || len(result.Diagnostics) == 0 || result.CanonicalJSON != "" || result.WireBase64 != "" {
			t.Fatalf("invalid message was accepted: %+v %v", result, err)
		}
		data, _ := json.Marshal(result)
		if strings.Contains(string(data), canary) {
			t.Fatalf("diagnostic exposed body value: %s", data)
		}
		if result.Diagnostics[0].Line < 1 || result.Diagnostics[0].Column < 1 {
			t.Fatalf("diagnostic has no editor position: %+v", result.Diagnostics)
		}
	}
	assertNoSecretOnDisk(t, dir, canary)
}

func TestProtoCompilerDiagnosticsExcludeSourceLiteralsAndSymbols(t *testing.T) {
	dir := t.TempDir()
	vault := &projectVault{store: security.NewMemoryStore()}
	_, svc := openApp(t, filepath.Join(dir, "relay.sqlite3"), vault)
	state := loadState(t, svc)
	vault.calls.Store(0)
	for name, source := range map[string]string{
		"edition": `edition="SchemaSourceCanary9e78"; message A {}`,
		"symbol":  `syntax="proto3"; message A { SchemaSourceCanary9e78 value=1; }`,
		"custom option": `syntax="proto2";
import "google/protobuf/descriptor.proto";
enum Mode { GOOD=0; }
extend google.protobuf.FileOptions { optional Mode custom_mode=50001; }
option (custom_mode)=SchemaSourceCanary9e78;`,
	} {
		t.Run(name, func(t *testing.T) {
			// Verify that this real compiler failure contains the canary before
			// the application turns it into a safe diagnostic DTO.
			_, compilerErr := importer.CompileSources(t.Context(), map[string]string{"unsafe.proto": source}, nil)
			if compilerErr == nil || !strings.Contains(compilerErr.Error(), "SchemaSourceCanary9e78") {
				t.Fatal("fixture does not exercise an echoing compiler diagnostic")
			}
			failed, err := svc.ImportProtoSchema(t.Context(), app.ProtoImport{
				WorkspaceID: state.Workspaces[0].ID, Name: "Invalid", Kind: "proto",
				RootFiles: []string{"unsafe.proto"}, Sources: []app.ProtoSource{{Name: "unsafe.proto", Content: source}},
			})
			if err != nil || failed.SchemaID != "" || len(failed.Diagnostics) != 1 || failed.State.Revision != state.Revision {
				t.Fatalf("invalid source changed the profile: %v", err)
			}
			diagnostic := failed.Diagnostics[0]
			encoded, err := json.Marshal(failed)
			if err != nil || strings.Contains(string(encoded), "SchemaSourceCanary9e78") || diagnostic.File != "unsafe.proto" || diagnostic.Line < 1 || diagnostic.Column < 1 || diagnostic.Message == "" {
				t.Fatal("source content leaked or useful diagnostic position was lost")
			}
		})
	}
	if vault.calls.Load() != 0 {
		t.Fatal("offline diagnostics consulted the keyring")
	}
	assertNoSecretOnDisk(t, dir, "SchemaSourceCanary9e78")
}

func TestProtoReimportNoticesAndDeleteKeepSavedRequests(t *testing.T) {
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite3"), security.NewMemoryStore())
	state := loadState(t, svc)
	input := schemaInput(state.Workspaces[0].ID)
	imported := importSchema(t, svc, input)
	state = mustState(t)(svc.SaveRequest(t.Context(), domain.RequestDefinition{WorkspaceID: input.WorkspaceID, CollectionID: state.Collections[0].ID, Protocol: "grpc", Name: "Saved Call", GRPC: &domain.GRPCDefinition{SchemaID: imported.SchemaID, Method: "/sample.Echo/Call", BodyFormat: "json5", Body: "{count:'7'}", PersistBody: true}}))
	requestID := state.Requests[len(state.Requests)-1].ID
	input.ID = imported.SchemaID
	unchanged := importSchema(t, svc, input)
	if len(unchanged.Warnings) != 0 || unchanged.SchemaID != imported.SchemaID || len(unchanged.State.ProtoSchemas) != 1 {
		t.Fatalf("unchanged schema reported compatibility problems: %+v", unchanged.Warnings)
	}
	input.Sources[1].Content = strings.Replace(schemaDependency, "string label = 1", "bytes label = 1", 1)
	changed := importSchema(t, svc, input)
	if len(changed.Warnings) != 1 || !strings.Contains(changed.Warnings[0], "Saved Call") {
		t.Fatalf("transitive message change was not reported: %+v", changed.Warnings)
	}
	input.Sources[0].Content = strings.Replace(schemaRoot, "rpc Call(Input) returns (Output);", "", 1)
	removed := importSchema(t, svc, input)
	if len(removed.Warnings) != 1 || !strings.Contains(removed.Warnings[0], "missing") {
		t.Fatalf("missing saved method was not reported: %+v", removed.Warnings)
	}
	state = mustState(t)(svc.DeleteProtoSchema(t.Context(), input.WorkspaceID, imported.SchemaID))
	if len(state.ProtoSchemas) != 0 {
		t.Fatal("schema was not deleted")
	}
	index := slices.IndexFunc(state.Requests, func(request domain.RequestDefinition) bool { return request.ID == requestID })
	if index < 0 || state.Requests[index].GRPC.SchemaID != "" || state.Requests[index].GRPC.Method != "/sample.Echo/Call" || state.Requests[index].GRPC.Body != "{count:'7'}" {
		t.Fatal("deleting a schema destroyed its saved request")
	}
}

func TestProtoNativeSourcesAndBinaryDescriptorImports(t *testing.T) {
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite3"), security.NewMemoryStore())
	state := loadState(t, svc)
	input := schemaInput(state.Workspaces[0].ID)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deps"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, source := range input.Sources {
		if err := os.WriteFile(filepath.Join(dir, source.Name), []byte(source.Content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	native, err := app.ImportProtoPaths(t.Context(), svc, app.ProtoImport{WorkspaceID: input.WorkspaceID, Name: "Native"}, []string{dir}, []string{dir})
	if err != nil || len(native.Diagnostics) != 0 || native.SchemaID == "" {
		t.Fatalf("native folder import failed: %+v %v", native.Diagnostics, err)
	}
	data, _ := json.Marshal(native)
	if strings.Contains(string(data), dir) {
		t.Fatal("native physical path was sent to the webview")
	}
	artifact := native.State.ProtoSchemas[0].DescriptorSetBase64
	binary, err := base64.StdEncoding.DecodeString(artifact)
	if err != nil {
		t.Fatal(err)
	}
	binaryFile := filepath.Join(dir, "schema.pb")
	if err := os.WriteFile(binaryFile, binary, 0600); err != nil {
		t.Fatal(err)
	}
	nativeBinary, err := app.ImportProtoPaths(t.Context(), svc, app.ProtoImport{WorkspaceID: input.WorkspaceID, Name: "Native Binary", Kind: "descriptor"}, []string{binaryFile}, nil)
	if err != nil || len(nativeBinary.Diagnostics) != 0 || nativeBinary.SchemaID == "" || nativeBinary.State.ProtoSchemas[1].Kind != "descriptor" {
		t.Fatalf("native binary descriptor import failed: %+v %v", nativeBinary.Diagnostics, err)
	}
	imported := importSchema(t, svc, app.ProtoImport{WorkspaceID: input.WorkspaceID, Name: "Binary", Kind: "descriptor", DescriptorSetBase64: artifact})
	if len(imported.State.ProtoSchemas) != 3 || imported.State.ProtoSchemas[0].Checksum != imported.State.ProtoSchemas[2].Checksum {
		t.Fatal("binary descriptor import changed the isolated registry")
	}
	broken := filepath.Join(dir, "broken.proto")
	if err := os.WriteFile(broken, []byte(`syntax="proto3"; import "missing.proto";`), 0600); err != nil {
		t.Fatal(err)
	}
	failed, err := app.ImportProtoPaths(t.Context(), svc, app.ProtoImport{WorkspaceID: input.WorkspaceID}, []string{broken}, []string{dir})
	if err != nil || len(failed.Diagnostics) == 0 {
		t.Fatal("native missing import has no diagnostic")
	}
	data, _ = json.Marshal(failed.Diagnostics)
	if strings.Contains(string(data), dir) || !strings.Contains(string(data), "missing.proto") {
		t.Fatalf("native diagnostic lost its useful virtual name or exposed a physical path: %s", data)
	}
}

func TestProtoReimportReportsInheritedPresenceSemantics(t *testing.T) {
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite3"), security.NewMemoryStore())
	state := loadState(t, svc)
	input := app.ProtoImport{WorkspaceID: state.Workspaces[0].ID, Name: "Presence", Kind: "proto", RootFiles: []string{"presence.proto"}, Sources: []app.ProtoSource{{Name: "presence.proto", Content: `syntax="proto2"; package presence; message Input { optional int32 count=1; } service Echo { rpc Call(Input) returns (Input); }`}}}
	imported := importSchema(t, svc, input)
	_ = mustState(t)(svc.SaveRequest(t.Context(), domain.RequestDefinition{WorkspaceID: input.WorkspaceID, CollectionID: state.Collections[0].ID, Name: "Presence Call", Protocol: "grpc", GRPC: &domain.GRPCDefinition{SchemaID: imported.SchemaID, Method: "/presence.Echo/Call"}}))
	input.ID = imported.SchemaID
	input.Sources[0].Content = `syntax="proto3"; package presence; message Input { int32 count=1; } service Echo { rpc Call(Input) returns (Input); }`
	changed := importSchema(t, svc, input)
	if len(changed.Warnings) != 1 || !strings.Contains(changed.Warnings[0], "Presence Call") {
		t.Fatalf("inherited syntax/presence change was not reported: %+v", changed.Warnings)
	}
}
