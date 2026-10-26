package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"relay/internal/domain"
)

func schemaFixture(t *testing.T) domain.ProtoSchema {
	t.Helper()
	artifact, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{Name: proto.String("catalog/api.proto"), Syntax: proto.String("proto3"), Package: proto.String("catalog.v1")}}})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(artifact)
	return domain.ProtoSchema{ID: "schema", WorkspaceID: "w", Name: "Catalog", Kind: "proto", Checksum: hex.EncodeToString(hash[:]), ImportedAt: time.Now().UTC().Format(time.RFC3339Nano), RootFiles: []string{"catalog/api.proto"}, Files: []string{"catalog/api.proto"}, DescriptorSetBase64: base64.StdEncoding.EncodeToString(artifact)}
}

func TestProtoSchemasPersistPrivateArtifactAndOwnWorkspace(t *testing.T) {
	ctx := context.Background()
	db, filename := openTestDB(t)
	state := fixture()
	state.ProtoSchemas = []domain.ProtoSchema{schemaFixture(t)}
	state.Requests[0].Protocol = "grpc"
	state.Requests[0].HTTP = nil
	state.Requests[0].GRPC = &domain.GRPCDefinition{SchemaID: "schema", Method: "/catalog.v1.Catalog/Get", BodyFormat: "json5", Body: "transient-canary"}
	saved, err := db.Save(ctx, state, 0)
	if err != nil {
		t.Fatal(err)
	}
	if state.Requests[0].GRPC.Body == "" || saved.Requests[0].GRPC.Body != "" {
		t.Fatal("schema save modified draft or persisted transient body")
	}
	// Metadata snapshots must never carry the potentially large artifact.
	wire, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "descriptorSet") || strings.Contains(string(wire), saved.ProtoSchemas[0].DescriptorSetBase64) {
		t.Fatal("private descriptor artifact crossed ordinary JSON snapshot")
	}
	var artifact string
	if err := db.sql.QueryRow("SELECT descriptor_set_base64 FROM proto_schemas WHERE id='schema'").Scan(&artifact); err != nil || artifact != state.ProtoSchemas[0].DescriptorSetBase64 {
		t.Fatalf("artifact not persisted: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loaded, err := db.Load(ctx)
	if err != nil || !reflect.DeepEqual(loaded, saved) {
		t.Fatalf("schema changed after restart: %v", err)
	}
	loaded.Settings.Theme = "dark"
	updated, err := db.Save(ctx, loaded, loaded.Revision)
	if err != nil || updated.ProtoSchemas[0].DescriptorSetBase64 != artifact {
		t.Fatalf("ordinary mutation lost descriptor: %v", err)
	}
	// Sanitization must clone slice metadata rather than alias UI drafts.
	updated.ProtoSchemas[0].Files[0] = "changed.proto"
	if loaded.ProtoSchemas[0].Files[0] != "catalog/api.proto" {
		t.Fatal("schema metadata aliases caller state")
	}
	if _, err := db.Save(ctx, domain.State{}, updated.Revision); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.sql.QueryRow("SELECT count(*) FROM proto_schemas").Scan(&count); err != nil || count != 0 {
		t.Fatalf("workspace deletion retained schema: %d %v", count, err)
	}
}

func TestProtoSchemaValidationRejectsCorruptionAndCrossWorkspaceReferences(t *testing.T) {
	for name, change := range map[string]func(*domain.State){
		"checksum":  func(s *domain.State) { s.ProtoSchemas[0].Checksum = strings.Repeat("0", 64) },
		"encoding":  func(s *domain.State) { s.ProtoSchemas[0].DescriptorSetBase64 = "invalid base64" },
		"traversal": func(s *domain.State) { s.ProtoSchemas[0].Files = []string{"../api.proto"} },
		"duplicate file": func(s *domain.State) {
			s.ProtoSchemas[0].Files = append(s.ProtoSchemas[0].Files, s.ProtoSchemas[0].Files[0])
		},
		"missing root":      func(s *domain.State) { s.ProtoSchemas[0].RootFiles = []string{"missing.proto"} },
		"missing workspace": func(s *domain.State) { s.ProtoSchemas[0].WorkspaceID = "missing" },
		"cross workspace request": func(s *domain.State) {
			s.Workspaces = append(s.Workspaces, domain.Workspace{ID: "other", Name: "Other"})
			s.ProtoSchemas[0].WorkspaceID = "other"
		},
		"invalid body format": func(s *domain.State) { s.Requests[0].GRPC.BodyFormat = "javascript" },
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := openTestDB(t)
			state := fixture()
			state.ProtoSchemas = []domain.ProtoSchema{schemaFixture(t)}
			state.Requests[0].Protocol = "grpc"
			state.Requests[0].HTTP = nil
			state.Requests[0].GRPC = &domain.GRPCDefinition{SchemaID: "schema"}
			change(&state)
			if _, err := db.Save(context.Background(), state, 0); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid schema accepted: %v", err)
			}
			current, err := db.Load(context.Background())
			if err != nil || current.Revision != 0 {
				t.Fatalf("failed schema save changed snapshot: %v", err)
			}
		})
	}
}
