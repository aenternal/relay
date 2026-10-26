package project

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestGuardJSONRejectsAmbiguousAndUnboundedDocuments(t *testing.T) {
	for _, data := range []string{`{"version":1,"version":2}`, `{} {}`, `{"x":[}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), "[" + strings.Repeat("0,", 200001) + "0]", string([]byte{0xff})} {
		if err := GuardJSON([]byte(data)); err == nil {
			t.Fatal("unsafe document accepted")
		}
	}
	if err := GuardJSON([]byte(`{"text":"Привет","items":[null,true,12345678901234567890]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectParsersRejectArchivesWithoutInterpretingEntries(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	entry, err := w.Create("../../archive-path-canary/relay-project.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`{"format":"relay-project","version":1,"workspace":{"id":"w","name":"archive-content-canary"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	parsers := map[string]func([]byte) (Document, []Warning, error){
		"native":   func(data []byte) (Document, []Warning, error) { return ParseNative(t.Context(), data) },
		"Postman":  ParsePostman,
		"Insomnia": ParseInsomnia,
	}
	for name, parse := range parsers {
		t.Run(name, func(t *testing.T) {
			doc, warnings, err := parse(archive.Bytes())
			if err == nil || doc.Workspace.ID != "" || len(warnings) != 0 || strings.Contains(err.Error(), "archive-path-canary") || strings.Contains(err.Error(), "archive-content-canary") {
				t.Fatal("archive was interpreted or its entries entered diagnostics")
			}
		})
	}
}
