package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"relay/internal/app"
	"relay/internal/domain"
	"relay/internal/project"
	"relay/internal/security"
)

type projectVault struct {
	calls atomic.Int64
	store *security.MemoryStore
}

func (v *projectVault) Get(ctx context.Context, key string) (string, error) {
	v.calls.Add(1)
	return v.store.Get(ctx, key)
}
func (v *projectVault) Set(ctx context.Context, key, value string) error {
	v.calls.Add(1)
	return v.store.Set(ctx, key, value)
}
func (v *projectVault) Delete(ctx context.Context, key string) error {
	v.calls.Add(1)
	return v.store.Delete(ctx, key)
}

func TestProjectExportImportAllProtocolsPreservesExistingStateAndRestoresSchemas(t *testing.T) {
	sourceDir := t.TempDir()
	sourceVault := &projectVault{store: security.NewMemoryStore()}
	_, source := openApp(t, filepath.Join(sourceDir, "source.sqlite"), sourceVault)
	state := loadState(t, source)
	workspaceID, collectionID := state.Workspaces[0].ID, state.Collections[0].ID
	importedSchema := importSchema(t, source, schemaInput(workspaceID))
	schemaID := importedSchema.SchemaID
	httpRequest := importedSchema.State.Requests[0]
	httpRequest.HTTP.URL = "https://example.test/echo"
	httpRequest.HTTP.BodyKind = "json"
	httpRequest.HTTP.Body = `{"message":"portable HTTP"}`
	httpRequest.HTTP.PersistBody = true
	mustState(t)(source.SaveRequest(t.Context(), httpRequest))
	grpcState := mustState(t)(source.SaveRequest(t.Context(), domain.RequestDefinition{WorkspaceID: workspaceID, CollectionID: collectionID, Name: "Portable gRPC", Protocol: "grpc", GRPC: &domain.GRPCDefinition{Endpoint: "127.0.0.1:9000", Method: "/sample.Echo/Call", SchemaID: schemaID, BodyFormat: "json5", Body: `{count: "9223372036854775807", names: ["portable"]}`, PersistBody: true}}))
	mustState(t)(source.SaveRequest(t.Context(), domain.RequestDefinition{WorkspaceID: workspaceID, CollectionID: collectionID, Name: "Portable WebSocket", Protocol: "websocket", WebSocket: &domain.WebSocketDefinition{URL: "wss://example.test/echo", BodyKind: "text", BodyEncoding: "text", Body: "portable WebSocket", PersistBody: true}}))
	_, envID, varID := addToken(t, source)
	const secret = "vault-project-canary-0123456789"
	mustState(t)(source.SetSecret(t.Context(), envID, varID, secret))
	state = loadState(t, source)
	env := environment(t, state, envID)
	env.Variables = append(env.Variables, domain.Variable{Name: "apiPassword", Value: "public-password-canary-0123456789"})
	mustState(t)(source.SaveEnvironment(t.Context(), env))
	sourceVault.calls.Store(0)
	defaultExport, err := source.ExportProject(t.Context(), workspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(defaultExport.Data, "portable HTTP") || strings.Contains(defaultExport.Data, "portable WebSocket") {
		t.Fatal("default export included request bodies")
	}
	exported, err := source.ExportProject(t.Context(), workspaceID, true)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Stats.Requests != 3 || exported.Stats.Schemas != 1 || exported.Stats.SecretVariables != 2 {
		t.Fatalf("incorrect report: %+v", exported.Stats)
	}
	for _, value := range []string{secret, "public-password-canary-0123456789", tokenVariable(t, state, envID).SecretRef} {
		if value != "" && strings.Contains(exported.Data, value) {
			t.Fatal("export leaked credentials or vault references")
		}
	}
	if sourceVault.calls.Load() != 0 {
		t.Fatal("export accessed the vault")
	}
	targetDir := t.TempDir()
	targetVault := &projectVault{store: security.NewMemoryStore()}
	targetFile := filepath.Join(targetDir, "target.sqlite")
	targetDB, target := openApp(t, targetFile, targetVault)
	before := loadState(t, target)
	settings := before.Settings
	settings.Theme = "dark"
	before = mustState(t)(target.SaveSettings(t.Context(), settings))
	targetVault.calls.Store(0)
	preview, err := target.PreviewProjectImport(t.Context(), "native", exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Stats != exported.Stats || preview.WorkspaceName != state.Workspaces[0].Name || !reflect.DeepEqual(loadState(t, target), before) {
		t.Fatal("preview report or nonmutation failed")
	}
	result, err := target.ImportProject(t.Context(), "native", exported.Data, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceID == workspaceID || result.WorkspaceID == before.Workspaces[0].ID || len(result.State.Workspaces) != 2 || result.State.Settings != before.Settings || !reflect.DeepEqual(result.State.UI, before.UI) {
		t.Fatal("import replaced existing data or settings")
	}
	if targetVault.calls.Load() != 0 {
		t.Fatal("project import accessed the vault")
	}
	if err = targetDB.Close(); err != nil {
		t.Fatal(err)
	}
	_, target = openApp(t, targetFile, targetVault)
	result.State = loadState(t, target)
	var grpcRequest domain.RequestDefinition
	for _, request := range result.State.Requests {
		if request.WorkspaceID == result.WorkspaceID && request.GRPC != nil {
			grpcRequest = request
		}
	}
	if grpcRequest.ID == "" || grpcRequest.GRPC.SchemaID == schemaID || grpcRequest.ID == grpcState.Requests[len(grpcState.Requests)-1].ID {
		t.Fatal("gRPC request/schema IDs were not replaced")
	}
	validated, err := target.ValidateProtoMessage(t.Context(), app.ProtoValidationRequest{WorkspaceID: result.WorkspaceID, SchemaID: grpcRequest.GRPC.SchemaID, Method: grpcRequest.GRPC.Method, Format: grpcRequest.GRPC.BodyFormat, Body: grpcRequest.GRPC.Body})
	if err != nil || !validated.Valid || !strings.Contains(validated.CanonicalJSON, "9223372036854775807") {
		t.Fatalf("offline imported descriptor unusable: %+v %v", validated, err)
	}
	withoutBodies, err := target.ImportProject(t.Context(), "native", exported.Data, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range withoutBodies.State.Requests {
		if request.WorkspaceID != withoutBodies.WorkspaceID {
			continue
		}
		if request.HTTP != nil && request.HTTP.Body != "" || request.GRPC != nil && request.GRPC.Body != "" || request.WebSocket != nil && request.WebSocket.Body != "" {
			t.Fatal("default import persisted bodies")
		}
	}
	assertNoSecretOnDisk(t, targetDir, secret, "public-password-canary-0123456789", tokenVariable(t, state, envID).SecretRef)
}

func TestProjectImportRejectsBadArtifactsAndGraphsWithoutChangingRevision(t *testing.T) {
	_, svc := openApp(t, filepath.Join(t.TempDir(), "relay.sqlite"), security.NewMemoryStore())
	state := loadState(t, svc)
	schema := importSchema(t, svc, schemaInput(state.Workspaces[0].ID))
	before := schema.State
	exported, err := svc.ExportProject(t.Context(), state.Workspaces[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var original project.Document
	if err = json.Unmarshal([]byte(exported.Data), &original); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*project.Document){
		"checksum":        func(d *project.Document) { d.Schemas[0].Metadata.Checksum = strings.Repeat("0", 64) },
		"artifact":        func(d *project.Document) { d.Schemas[0].DescriptorSetBase64 = "invalid base64" },
		"duplicate":       func(d *project.Document) { d.Collections[0].ID = d.Workspace.ID },
		"foreign owner":   func(d *project.Document) { d.Requests[0].WorkspaceID = "foreign-workspace" },
		"dangling folder": func(d *project.Document) { d.Requests[0].FolderID = "missing-folder" },
	} {
		t.Run(name, func(t *testing.T) {
			var bad project.Document
			if err = json.Unmarshal([]byte(exported.Data), &bad); err != nil {
				t.Fatal(err)
			}
			change(&bad)
			data, _ := json.Marshal(bad)
			if _, err = svc.PreviewProjectImport(t.Context(), "native", string(data)); err == nil {
				t.Fatal("bad preview accepted")
			}
			if _, err = svc.ImportProject(t.Context(), "native", string(data), true); err == nil {
				t.Fatal("bad import accepted")
			}
			if after := loadState(t, svc); !reflect.DeepEqual(after, before) {
				t.Fatal("failed import changed database")
			}
		})
	}
	if _, err = svc.ImportProject(t.Context(), "unsupported", `{}`, false); err == nil {
		t.Fatal("unsupported format accepted")
	}
	if _, err = svc.ImportProject(t.Context(), "native", strings.Repeat(" ", project.MaxBytes+1), false); err == nil {
		t.Fatal("oversized import accepted")
	}
	if _, err = svc.ExportProject(t.Context(), "missing", false); err == nil {
		t.Fatal("missing workspace exported")
	}
}
