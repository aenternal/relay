package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"relay/internal/project"
)

func TestProjectFileSelectionBounds(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "project.json")
	if err := os.WriteFile(path, []byte(`{"format":"relay-project"}`), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := readSelectedProject(context.Background(), path)
	if err != nil || selected.Name != "project.json" || selected.Data != `{"format":"relay-project"}` {
		t.Fatalf("selection: %+v, %v", selected, err)
	}
	if _, err := readSelectedProject(context.Background(), directory); err == nil {
		t.Fatal("directory accepted")
	}
	file, err := os.Create(filepath.Join(directory, "large.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(project.MaxBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	if _, err := readSelectedProject(context.Background(), file.Name()); err == nil {
		t.Fatal("oversized file accepted")
	}
	if err := os.WriteFile(path, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSelectedProject(context.Background(), path); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestProjectFileAtomicSaveCancellation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "project.json")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeSelectedProject(ctx, path, "replacement"); err == nil {
		t.Fatal("canceled save succeeded")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatalf("old export changed: %q, %v", data, err)
	}
	if err := writeSelectedProject(context.Background(), path, "replacement"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("incomplete replacement: %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("private export permissions: %v", info.Mode())
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary export leaked: %v, %v", entries, err)
	}
}
