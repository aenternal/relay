package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"relay/internal/domain"
	"relay/internal/protobuf/importer"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func nativeFixture(t *testing.T) domain.State {
	t.Helper()
	reg, err := importer.CompileSources(t.Context(), map[string]string{"echo.proto": `syntax="proto3"; package portable; message Input { string text=1; } service Echo { rpc Call(Input) returns(Input); }`}, []string{"echo.proto"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := reg.DescriptorSet()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	return domain.State{
		Workspaces:  []domain.Workspace{{ID: "w", Name: "Portable", ActiveEnvironmentID: "e"}},
		Collections: []domain.Collection{{ID: "c", WorkspaceID: "w", Name: "Requests"}},
		Folders:     []domain.Folder{{ID: "f", WorkspaceID: "w", CollectionID: "c", Name: "Nested"}},
		Requests: []domain.RequestDefinition{
			{ID: "http", WorkspaceID: "w", CollectionID: "c", FolderID: "f", Name: "HTTP", Protocol: "http", HTTP: &domain.HTTPDefinition{Method: "POST", URL: "https://example.test/echo?channel=portable", Version: "1.1", BodyKind: "json", Body: `{"text":"portable"}`, PersistBody: true, FileID: "local-file", TLS: domain.HTTPTLS{CAFileID: "local-ca"}, Auth: domain.HTTPAuth{Type: "bearer", Token: "{{token}}"}}},
			{ID: "grpc", WorkspaceID: "w", CollectionID: "c", Name: "gRPC", Protocol: "grpc", GRPC: &domain.GRPCDefinition{Endpoint: "localhost:9000", Method: "/portable.Echo/Call", SchemaID: "schema", BodyFormat: "json", Body: `{"text":"portable"}`, PersistBody: true}},
			{ID: "ws", WorkspaceID: "w", CollectionID: "c", Name: "WebSocket", Protocol: "websocket", WebSocket: &domain.WebSocketDefinition{URL: "wss://example.test/echo", BodyKind: "text", BodyEncoding: "text", Body: "portable", PersistBody: true, FileID: "local-ws-file", Subprotocols: []string{"echo.v1"}}},
		},
		Environments: []domain.Environment{{ID: "e", WorkspaceID: "w", Name: "Local", Variables: []domain.Variable{{ID: "host", Name: "host", Value: "example.test"}, {ID: "token", Name: "token", Secret: true, SecretRef: "keyring-native-canary-0123456789"}}}},
		ProtoSchemas: []domain.ProtoSchema{{ID: "schema", WorkspaceID: "w", Name: "Echo", Kind: "proto", Checksum: hex.EncodeToString(digest[:]), ImportedAt: time.Now().UTC().Format(time.RFC3339Nano), RootFiles: []string{"echo.proto"}, Files: []string{"echo.proto"}, DescriptorSetBase64: base64.StdEncoding.EncodeToString(artifact)}},
		Settings:     domain.DefaultSettings(), UI: domain.UIState{WorkspaceID: "w", OpenRequestIDs: []string{"http"}, ActiveRequestID: "http"},
	}
}

func TestNativeRoundTripDefaultsAndFreshIDs(t *testing.T) {
	state := nativeFixture(t)
	doc, warnings, err := Export(t.Context(), state, "w", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Fatal("missing body/file/secret privacy warnings")
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"keyring-native-canary-0123456789", "local-file", "local-ca", "local-ws-file", `\"text\":\"portable\"`} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("private field exported: %q", secret)
		}
	}
	if len(doc.Schemas) != 1 || doc.Schemas[0].DescriptorSetBase64 == "" {
		t.Fatal("descriptor artifact omitted")
	}
	parsed, _, err := ParseNative(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	identified, err := Reidentify(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if identified.Workspace.ID == "w" || identified.Requests[0].ID == "http" || identified.Requests[1].GRPC.SchemaID != "" && identified.Requests[1].GRPC.SchemaID != identified.Schemas[0].Metadata.ID || identified.Workspace.ActiveEnvironmentID != identified.Environments[0].ID {
		t.Fatal("fresh IDs or references broken")
	}
	if identified.Requests[0].HTTP.Body != "" || identified.Requests[1].GRPC.Body != "" || identified.Requests[2].WebSocket.Body != "" {
		t.Fatal("body default not private")
	}
	if state.Requests[0].HTTP.FileID != "local-file" || state.Requests[0].HTTP.Body == "" || state.Environments[0].Variables[1].SecretRef == "" {
		t.Fatal("source mutated")
	}
	withBodies, _, err := Export(t.Context(), state, "w", Options{IncludeBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	if withBodies.Requests[0].HTTP.Body == "" || withBodies.Requests[1].GRPC.Body == "" || withBodies.Requests[2].WebSocket.Body != "portable" {
		t.Fatal("explicit body export lost")
	}
}

func TestNativeSanitizesLiteralCredentialsAcrossFields(t *testing.T) {
	state := nativeFixture(t)
	const token = "literal-native-canary-0123456789"
	h := state.Requests[0].HTTP
	h.URL = "https://user:" + token + "@example.test/echo?token=" + token + "&q=one&q=two"
	h.Headers = []domain.Header{{Name: "Authorization", Value: "Bearer " + token, Enabled: true}, {Name: "Cookie", Value: "session=" + token, Enabled: false}, {Name: "X-Trace", Value: "echo " + token}, {Name: "X-Encoded", Value: base64.StdEncoding.EncodeToString([]byte(token))}}
	h.Proxy = domain.HTTPProxy{Mode: "custom", URL: "http://private-user-canary:" + token + "@{{proxyHost}}:8080"}
	h.Body = `{"text":"` + token + `"}`
	h.Auth = domain.HTTPAuth{Type: "basic", Username: "private-user-canary", Password: token}
	state.Environments[0].Variables = append(state.Environments[0].Variables, domain.Variable{ID: "public-token", Name: "apiToken", Value: token}, domain.Variable{ID: "echo-token", Name: "echo", Value: "copy " + token})
	doc, _, err := Export(t.Context(), state, "w", Options{IncludeBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(doc)
	for _, secret := range []string{token, base64.StdEncoding.EncodeToString([]byte(token)), "private-user-canary", "keyring-native-canary-0123456789"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
	if !doc.Environments[0].Variables[2].Secret || doc.Environments[0].Variables[2].Value != "" || !doc.Environments[0].Variables[3].Secret {
		t.Fatal("public credential variables not converted")
	}
	if !strings.Contains(doc.Requests[0].HTTP.URL, "q=one&q=two") {
		t.Fatal("repeated public query lost")
	}
}

func TestNativeRejectsMalformedArtifactsGraphsAndJSON(t *testing.T) {
	state := nativeFixture(t)
	doc, _, err := Export(t.Context(), state, "w", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("checksum", func(t *testing.T) {
		bad := doc
		bad.Schemas = append([]Schema{}, doc.Schemas...)
		bad.Schemas[0].Metadata.Checksum = strings.Repeat("0", 64)
		if _, _, err := Prepare(t.Context(), bad, Options{}); err == nil {
			t.Fatal("accepted bad checksum")
		}
	})
	t.Run("binary", func(t *testing.T) {
		bad := doc
		bad.Schemas = append([]Schema{}, doc.Schemas...)
		raw := []byte("not a descriptor")
		digest := sha256.Sum256(raw)
		bad.Schemas[0].DescriptorSetBase64 = base64.StdEncoding.EncodeToString(raw)
		bad.Schemas[0].Metadata.Checksum = hex.EncodeToString(digest[:])
		if _, _, err := Prepare(t.Context(), bad, Options{}); err == nil {
			t.Fatal("accepted invalid descriptor")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		bad := doc
		bad.Collections = append([]domain.Collection{}, doc.Collections...)
		bad.Collections[0].ID = doc.Workspace.ID
		if _, err := Reidentify(bad); err == nil {
			t.Fatal("accepted duplicate ID")
		}
	})
	t.Run("dangling", func(t *testing.T) {
		bad := doc
		bad.Requests = append([]domain.RequestDefinition{}, doc.Requests...)
		bad.Requests[0].FolderID = "missing"
		if _, err := Reidentify(bad); err == nil {
			t.Fatal("accepted dangling ref")
		}
	})
	t.Run("owner", func(t *testing.T) {
		bad := doc
		bad.Folders = append([]domain.Folder{}, doc.Folders...)
		bad.Folders[0].WorkspaceID = "other"
		if _, err := Reidentify(bad); err == nil {
			t.Fatal("accepted cross-workspace")
		}
	})
	for _, data := range []string{`{"format":"relay-project","format":"relay-project","version":1}`, `{"format":"relay-project","version":2}`, strings.Repeat("[", 34) + strings.Repeat("]", 34), `{"format":"relay-project","version":1,"script":"canary"}`} {
		if _, _, err := ParseNative(t.Context(), []byte(data)); err == nil {
			t.Fatalf("accepted invalid native document: %.100s", data)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Prepare(ctx, doc, Options{}); err == nil {
		t.Fatal("ignored canceled context")
	}
}

func TestNativeRejectsKnownCredentialInDescriptor(t *testing.T) {
	state := nativeFixture(t)
	state.Environments[0].Variables = append(state.Environments[0].Variables, domain.Variable{ID: "secret-name", Name: "password", Value: "portable"})
	if _, _, err := Export(t.Context(), state, "w", Options{}); err == nil || strings.Contains(err.Error(), "portable") {
		t.Fatal("descriptor credential must fail with a static error")
	}
}

func TestNativeRejectsEncodedCredentialCopiesInDescriptorComments(t *testing.T) {
	const canary = "native credential canary +/= ? 0123456789"
	for name, encoded := range map[string]string{
		"base64":    base64.StdEncoding.EncodeToString([]byte(canary)),
		"base64url": base64.URLEncoding.EncodeToString([]byte(canary)),
		"url":       url.QueryEscape(canary),
		"hex":       hex.EncodeToString([]byte(canary)),
	} {
		t.Run(name, func(t *testing.T) {
			state := nativeFixture(t)
			artifact, err := base64.StdEncoding.DecodeString(state.ProtoSchemas[0].DescriptorSetBase64)
			if err != nil {
				t.Fatal(err)
			}
			set := new(descriptorpb.FileDescriptorSet)
			if err = proto.Unmarshal(artifact, set); err != nil {
				t.Fatal(err)
			}
			set.File[0].SourceCodeInfo = &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{LeadingComments: proto.String("Encoded credential copy: " + encoded)}}}
			artifact, err = proto.Marshal(set)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(artifact, []byte(canary)) {
				t.Fatal("test requires an encoded-only credential copy")
			}
			digest := sha256.Sum256(artifact)
			state.ProtoSchemas[0].Checksum = hex.EncodeToString(digest[:])
			state.ProtoSchemas[0].DescriptorSetBase64 = base64.StdEncoding.EncodeToString(artifact)
			state.Environments[0].Variables = append(state.Environments[0].Variables, domain.Variable{ID: "encoded-secret", Name: "apiToken", Value: canary})
			if _, _, err = Export(t.Context(), state, "w", Options{}); err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), encoded) {
				t.Fatal("encoded descriptor credential must fail with a static error")
			}
		})
	}
}
