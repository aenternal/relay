package importer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"relay/internal/protobuf/importer"
)

func TestDirectoryImportAndDescriptorSetRoundTrip(t *testing.T) {
	root, _ := filepath.Abs("../../../testdata/proto")
	r, err := importer.Compile(context.Background(), []string{filepath.Join(root, "matrix")}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Methods()) != 4 {
		t.Fatalf("methods: %+v", r.Methods())
	}
	set, err := r.DescriptorSet()
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := importer.DescriptorSet(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := roundTrip.DescriptorSet()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(set, second) {
		t.Fatal("descriptor export is not stable")
	}
	if len(roundTrip.Messages()) != len(r.Messages()) {
		t.Fatal("dependency closure changed")
	}
	services := roundTrip.Services()
	if len(services) != 1 || !strings.Contains(services[0].Comment, "every RPC shape") || !strings.Contains(services[0].Methods[0].Comment, "exact full method") {
		t.Fatalf("source comments lost: %+v", services)
	}
}

func TestDescriptorSetMissingDependenciesDuplicatesAndMalformed(t *testing.T) {
	ctx := context.Background()
	r, err := importer.CompileSources(ctx, map[string]string{"a.proto": `syntax="proto3"; package test; import "google/protobuf/timestamp.proto"; message A { google.protobuf.Timestamp stamp=1; }`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := r.DescriptorSet()
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		t.Fatal(err)
	}
	for _, f := range set.File {
		if f.GetName() == "a.proto" {
			set.File = []*descriptorpb.FileDescriptorProto{f}
			break
		}
	}
	data, _ = proto.Marshal(set)
	imported, err := importer.DescriptorSet(ctx, data)
	if err != nil {
		t.Fatal("omitted bundled WKT:", err)
	}
	if len(imported.FilesInfo()) != 2 {
		t.Fatal(imported.FilesInfo())
	}
	set.File[0].Dependency[0] = "absent.proto"
	data, _ = proto.Marshal(set)
	if _, err := importer.DescriptorSet(ctx, data); err == nil || !strings.Contains(err.Error(), "absent.proto") {
		t.Fatalf("missing dependency: %v", err)
	}
	set.File[0].Dependency = nil
	set.File = append(set.File, set.File[0])
	data, _ = proto.Marshal(set)
	if _, err := importer.DescriptorSet(ctx, data); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate: %v", err)
	}
	for _, invalid := range [][]byte{nil, []byte("not a descriptor set"), {0xff}, {}} {
		if _, err := importer.DescriptorSet(ctx, invalid); err == nil {
			t.Fatal("accepted malformed descriptor set")
		}
	}
	if _, err := importer.DescriptorSet(ctx, make([]byte, importer.MaxTotalBytes+1)); err == nil {
		t.Fatal("missing descriptor limit")
	}
}

func TestVirtualImportsConflictsCyclesPathsAndLimits(t *testing.T) {
	ctx := context.Background()
	for name, sources := range map[string]map[string]string{
		"missing":         {"a.proto": `syntax="proto3"; import "missing.proto";`},
		"symbol conflict": {"a.proto": `syntax="proto3"; message A {}`, "b.proto": `syntax="proto3"; message A {}`},
		"cycle":           {"a.proto": `syntax="proto3"; import "b.proto";`, "b.proto": `syntax="proto3"; import "a.proto";`},
		"traversal":       {"../secret.proto": `syntax="proto3";`},
		"absolute":        {"/secret.proto": `syntax="proto3";`},
		"windows path":    {`C:\secret.proto`: `syntax="proto3";`},
		"alias":           {"a/../b.proto": `syntax="proto3";`},
		"large":           {"a.proto": strings.Repeat(" ", importer.MaxFileBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := importer.CompileSources(ctx, sources, nil); err == nil {
				t.Fatal("accepted invalid import")
			}
		})
	}
	sources := make(map[string]string)
	for i := range importer.MaxFiles + 1 {
		sources["file"+strings.Repeat("x", i)+".proto"] = ""
	}
	if _, err := importer.CompileSources(ctx, sources, nil); err == nil {
		t.Fatal("missing file limit")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := importer.CompileSources(ctx, map[string]string{"a.proto": ""}, nil); err == nil {
		t.Fatal("missing cancellation")
	}
}

func TestFilesystemSymlinkContainmentAndEmptyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture needs privileges on Windows")
	}
	include, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.proto"), []byte(`syntax="proto3"; message Secret {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.proto"), filepath.Join(include, "secret.proto")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(include, "root.proto"), []byte("syntax=\"proto3\"; import \"secret.proto\";"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Compile(context.Background(), []string{"root.proto"}, []string{include}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink escaped: %v", err)
	}
	if _, err := importer.Compile(context.Background(), []string{t.TempDir()}, nil); err == nil {
		t.Fatal("empty directory accepted")
	}
	// A file explicitly selected outside an include root is authorized as its
	// own root, while transitive imports above remain contained.
	if err := os.Remove(filepath.Join(include, "secret.proto")); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Compile(context.Background(), []string{filepath.Join(outside, "secret.proto")}, []string{include}); err != nil {
		t.Fatal(err)
	}
}

func TestEditions2023AndExplicitUnsupportedEdition(t *testing.T) {
	r, err := importer.CompileSources(context.Background(), map[string]string{"edition.proto": `edition="2023"; package editions; message Input { string text=1; int32 required_count=2 [features.field_presence=LEGACY_REQUIRED]; }`}, nil)
	if err != nil {
		t.Fatal("supported edition 2023:", err)
	}
	if got := r.FilesInfo()[0].Syntax; got != "editions" {
		t.Fatalf("syntax: %q", got)
	}
	if _, err := importer.CompileSources(context.Background(), map[string]string{"edition.proto": `edition="2024"; message Input {}`}, nil); err == nil {
		t.Fatal("unsupported edition 2024 accepted")
	}
	data, _ := r.DescriptorSet()
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		t.Fatal(err)
	}
	set.File[0].Edition = descriptorpb.Edition_EDITION_2024.Enum()
	data, _ = proto.Marshal(set)
	if _, err := importer.DescriptorSet(context.Background(), data); err == nil || !strings.Contains(err.Error(), "edition 2023") {
		t.Fatalf("descriptor edition unsupported: %v", err)
	}
}
