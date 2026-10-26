package importer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestDescriptorBudgetRejectsDenseAllocationBombsBeforeUnmarshal(t *testing.T) {
	// Each empty dependency/message costs only two wire bytes but creates a
	// string/message allocation in the standard protobuf decoder.
	for _, field := range []protowire.Number{3, 4} {
		unit := protowire.AppendTag(nil, field, protowire.BytesType)
		unit = protowire.AppendBytes(unit, nil)
		file := bytes.Repeat(unit, MaxDescriptorWork+1)
		set := protowire.AppendTag(nil, 1, protowire.BytesType)
		set = protowire.AppendBytes(set, file)
		if _, err := DescriptorSet(context.Background(), set); err == nil || !strings.Contains(err.Error(), "structural") {
			t.Fatalf("dense descriptor not rejected before allocation: %v", err)
		}
	}
}

func TestSourceBudgetRejectsDenseParserAndLineAllocations(t *testing.T) {
	const canary = "source-parser-secret-canary"
	for name, body := range map[string]string{
		"declarations": strings.Repeat(";", MaxSourceWork+1),
		"comments":     strings.Repeat("/**/", MaxSourceWork+1),
		"lines":        strings.Repeat("\n", MaxSourceWork+1),
	} {
		t.Run(name, func(t *testing.T) {
			body = "// " + canary + "\n" + body
			if len(body) > MaxFileBytes {
				t.Fatal("fixture must fit the source byte budget")
			}
			_, err := CompileSources(t.Context(), map[string]string{"dense.proto": body}, nil)
			if err == nil || !strings.Contains(err.Error(), "100000") || strings.Contains(err.Error(), canary) {
				t.Fatalf("dense source did not fail safely before compilation: %v", err)
			}
			path := filepath.Join(t.TempDir(), "dense.proto")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = Compile(t.Context(), []string{path}, nil)
			if err == nil || !strings.Contains(err.Error(), "100000") || strings.Contains(err.Error(), canary) {
				t.Fatalf("dense native source bypassed the budget: %v", err)
			}
		})
	}
	_, err := CompileSources(t.Context(), map[string]string{
		"a.proto": strings.Repeat(";", MaxSourceWork/2+1),
		"b.proto": strings.Repeat(";", MaxSourceWork/2+1),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "100000") {
		t.Fatal("sources did not share one allocation budget")
	}
}

func TestSourceBudgetTreatsStringAndCommentContentsAsData(t *testing.T) {
	// Many punctuation characters inside one comment or string do not create
	// parser tokens. Escaped quotes must not end a string prematurely.
	source := `syntax="proto2"; /*` + strings.Repeat(`;"'//`, MaxSourceWork) + `*/ message A { optional string value=1 [default="` + strings.Repeat(`;\"'//`, MaxSourceWork) + `"]; }`
	if len(source) > MaxFileBytes {
		t.Fatal("fixture must fit the source byte budget")
	}
	if _, err := CompileSources(t.Context(), map[string]string{"literal.proto": source}, nil); err != nil {
		t.Fatal("literal contents counted as parser nodes:", err)
	}
}

func TestDescriptorBudgetIsSharedAcrossReflectionFilesAndRejectsMalformedWire(t *testing.T) {
	var budget DescriptorBudget
	file := protowire.AppendTag(nil, 3, protowire.BytesType)
	file = protowire.AppendBytes(file, nil)
	file = bytes.Repeat(file, 1000)
	var err error
	for range 101 {
		err = budget.AddFile(file)
		if err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "structural") {
		t.Fatal("reflection descriptors did not share one budget")
	}
	for _, data := range [][]byte{{0x0a, 0xff}, {0x0b}, {0x00}} {
		var fresh DescriptorBudget
		if err := fresh.AddFile(data); err == nil {
			t.Fatal("malformed descriptor wire accepted")
		}
	}
}
