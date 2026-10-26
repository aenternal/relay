package importer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"relay/internal/protobuf/importer"
)

func TestCompileNestedImportsAndMethods(t *testing.T) {
	root, err := filepath.Abs("../../../testdata/proto")
	if err != nil {
		t.Fatal(err)
	}
	r, err := importer.Compile(context.Background(), []string{filepath.Join(root, "relay/v1/echo.proto")}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []protoreflect.FullName{"relay.test.common.Details", "google.protobuf.Timestamp", "google.protobuf.Any"} {
		if _, err := r.Types.FindMessageByName(name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	m, err := r.Method("/relay.test.v1.EchoService/Chat")
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsStreamingClient() || !m.IsStreamingServer() {
		t.Fatal("Chat lost streaming flags")
	}
	methods := r.Methods()
	if len(methods) != 2 || methods[0].FullMethod != "/relay.test.v1.EchoService/Chat" {
		t.Fatalf("methods: %+v", methods)
	}
	if _, err := r.Method("/relay.test.v1/Chat"); err == nil {
		t.Fatal("package must not resolve as a service")
	}
}

func TestCompileMissingImportDiagnostic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.proto"), []byte("syntax = \"proto3\";\nimport \"missing/types.proto\";\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := importer.Compile(context.Background(), []string{"broken.proto"}, []string{dir})
	if err == nil {
		t.Fatal("expected missing import error")
	}
	for _, want := range []string{"broken.proto:2:", "missing/types.proto", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q lacks %q", err, want)
		}
	}
}

func TestCompileAmbiguousImport(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(dir, "echo.proto"), []byte("syntax = \"proto3\"; message Echo {}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := importer.Compile(context.Background(), []string{"echo.proto"}, []string{a, b})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous import: %v", err)
	}
}

func TestStandardImportFallbackDoesNotMaskAmbiguity(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		path := filepath.Join(dir, "google/protobuf/any.proto")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("syntax = \"proto3\"; package google.protobuf; message Any {}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := importer.Compile(context.Background(), []string{"google/protobuf/any.proto"}, []string{a, b})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("standard fallback masked conflict: %v", err)
	}
}

func TestCompileCancellationAndEmptyInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := importer.Compile(ctx, []string{"anything.proto"}, nil); err == nil {
		t.Fatal("expected cancellation")
	}
	if _, err := importer.Compile(context.Background(), nil, nil); err == nil {
		t.Fatal("expected no files error")
	}
}
