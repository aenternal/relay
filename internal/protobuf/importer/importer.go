// Package importer compiles local .proto schemas without invoking protoc.
package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"relay/internal/protobuf/registry"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const (
	MaxFiles            = 512
	MaxRootFiles        = 256
	MaxFileBytes        = 2 << 20
	MaxTotalBytes       = 16 << 20
	MaxDirectoryEntries = 8192
)

// Compile resolves roots relative to includeDirs, imports standard WKT schemas,
// and creates an isolated registry containing the complete dependency graph.
// Root files may also be absolute filesystem paths.
func Compile(ctx context.Context, files []string, includeDirs []string) (*registry.Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("compile .proto: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("compile .proto: select at least one root file")
	}
	if len(files) > MaxRootFiles || len(includeDirs) > 64 {
		return nil, fmt.Errorf("compile .proto: too many selected files or include directories")
	}
	var err error
	files, includeDirs, err = expandDirectories(ctx, files, includeDirs)
	if err != nil {
		return nil, err
	}
	if len(includeDirs) > 64 {
		return nil, fmt.Errorf("compile .proto: too many include directories")
	}
	dirs := make([]string, 0, len(includeDirs)+len(files))
	addDir := func(dir string) error {
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return fmt.Errorf("include directory %q: %w", absolute, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("include directory %q is not a directory", absolute)
		}
		absolute, err = filepath.EvalSymlinks(absolute)
		if err != nil {
			return fmt.Errorf("include directory: %w", err)
		}
		for _, existing := range dirs {
			if existing == absolute {
				return nil
			}
		}
		dirs = append(dirs, absolute)
		return nil
	}
	for _, dir := range includeDirs {
		if err := addDir(dir); err != nil {
			return nil, err
		}
	}
	if len(dirs) == 0 {
		if err := addDir("."); err != nil {
			return nil, err
		}
	}
	roots := make([]string, 0, len(files))
	for _, file := range files {
		if file == "" {
			return nil, fmt.Errorf("compile .proto: empty root filename")
		}
		name := filepath.Clean(file)
		// A physical file selected by a file picker becomes a virtual import path.
		if _, err := os.Stat(name); err == nil || filepath.IsAbs(name) {
			absolute, err := filepath.Abs(name)
			if err != nil {
				return nil, err
			}
			absolute, err = filepath.EvalSymlinks(absolute)
			if err != nil {
				return nil, fmt.Errorf("root file %q: %w", file, err)
			}
			found := false
			for _, dir := range dirs {
				rel, err := filepath.Rel(dir, absolute)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					name, found = rel, true
					break
				}
			}
			if !found {
				if err := addDir(filepath.Dir(absolute)); err != nil {
					return nil, err
				}
				name = filepath.Base(absolute)
			}
		}
		roots = append(roots, filepath.ToSlash(name))
	}
	// Standard imports are a fallback for absent files only. Wrapping the local
	// resolver directly would also mask its ambiguity or permission errors.
	standards := protocompile.WithStandardImports(protocompile.ResolverFunc(func(string) (protocompile.SearchResult, error) {
		return protocompile.SearchResult{}, protoregistry.NotFound
	}))
	var mu sync.Mutex
	loaded := make(map[string][]byte)
	totalBytes := 0
	var sourceWork sourceBudget
	resolver := protocompile.ResolverFunc(func(name string) (protocompile.SearchResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return protocompile.SearchResult{}, err
		}
		if cached, ok := loaded[name]; ok {
			return protocompile.SearchResult{Source: bytes.NewReader(cached)}, nil
		}
		// Imported paths must stay inside explicitly selected include roots.
		clean := filepath.Clean(filepath.FromSlash(name))
		if !validImportPath(name) {
			return protocompile.SearchResult{}, fmt.Errorf("invalid import path %q: must be relative to include directories", name)
		}
		var selected string
		var selectedInfo fs.FileInfo
		for _, dir := range dirs {
			candidate := filepath.Join(dir, clean)
			info, err := os.Stat(candidate)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return protocompile.SearchResult{}, fmt.Errorf("import %q: %w", name, err)
			}
			if info.IsDir() {
				continue
			}
			if !info.Mode().IsRegular() {
				return protocompile.SearchResult{}, fmt.Errorf("import %q is not a regular file", name)
			}
			real, err := filepath.EvalSymlinks(candidate)
			if err != nil || !within(dir, real) {
				return protocompile.SearchResult{}, fmt.Errorf("import %q escapes its include directory through a symlink", name)
			}
			if selectedInfo != nil && !os.SameFile(selectedInfo, info) {
				return protocompile.SearchResult{}, fmt.Errorf("ambiguous import %q: both %q and %q exist", name, selected, candidate)
			}
			selected, selectedInfo = candidate, info
		}
		if selected == "" {
			if standard, err := standards.FindFileByPath(name); err == nil {
				return standard, nil
			}
			return protocompile.SearchResult{}, &MissingImportError{Path: name, Directories: append([]string(nil), dirs...)}
		}
		if len(loaded) >= MaxFiles {
			return protocompile.SearchResult{}, fmt.Errorf("schema exceeds the %d-file limit", MaxFiles)
		}
		if selectedInfo.Size() > MaxFileBytes {
			return protocompile.SearchResult{}, fmt.Errorf("import %q exceeds the 2 MiB source limit", name)
		}
		source, err := os.Open(selected)
		if err != nil {
			return protocompile.SearchResult{}, fmt.Errorf("open import %q: %w", name, err)
		}
		openedInfo, err := source.Stat()
		if err != nil || !os.SameFile(selectedInfo, openedInfo) {
			source.Close()
			return protocompile.SearchResult{}, fmt.Errorf("import %q changed while opening", name)
		}
		data, readErr := io.ReadAll(io.LimitReader(source, MaxFileBytes+1))
		closeErr := source.Close()
		if readErr != nil {
			return protocompile.SearchResult{}, fmt.Errorf("read import %q: %w", name, readErr)
		}
		if closeErr != nil {
			return protocompile.SearchResult{}, fmt.Errorf("close import %q: %w", name, closeErr)
		}
		if len(data) > MaxFileBytes || totalBytes+len(data) > MaxTotalBytes {
			return protocompile.SearchResult{}, fmt.Errorf("schema exceeds the source size limit")
		}
		if err := sourceWork.add(data); err != nil {
			return protocompile.SearchResult{}, err
		}
		loaded[name] = data
		totalBytes += len(data)
		return protocompile.SearchResult{Source: bytes.NewReader(data)}, nil
	})
	return compile(ctx, roots, resolver)
}

func compile(ctx context.Context, roots []string, resolver protocompile.Resolver) (*registry.Registry, error) {
	compiler := protocompile.Compiler{Resolver: resolver, SourceInfoMode: protocompile.SourceInfoStandard, MaxParallelism: 2}
	compiled, err := compiler.Compile(ctx, roots...)
	if err != nil {
		return nil, fmt.Errorf("compile .proto: %w", err)
	}
	descriptors := make([]protoreflect.FileDescriptor, len(compiled))
	for i, file := range compiled {
		descriptors[i] = file
	}
	r, err := registry.New(descriptors)
	if err != nil {
		return nil, err
	}
	if len(r.FilesInfo()) > MaxFiles {
		return nil, fmt.Errorf("schema dependency closure exceeds the %d-file limit", MaxFiles)
	}
	return r, nil
}

func within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validImportPath(name string) bool {
	if name == "" || len(name) > 1024 || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	return !filepath.IsAbs(clean) && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) && filepath.ToSlash(clean) == name
}

func expandDirectories(ctx context.Context, files, includeDirs []string) ([]string, []string, error) {
	roots := make([]string, 0, len(files))
	entries := 0
	for _, selected := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(selected)
		if err != nil || !info.IsDir() {
			roots = append(roots, selected)
			continue
		}
		includeDirs = append(includeDirs, selected)
		err = filepath.WalkDir(selected, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > MaxDirectoryEntries {
				return fmt.Errorf("selected directories exceed the %d-entry scan limit", MaxDirectoryEntries)
			}
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 && strings.EqualFold(filepath.Ext(entry.Name()), ".proto") {
				roots = append(roots, path)
				if len(roots) > MaxRootFiles {
					return fmt.Errorf("schema exceeds the %d-root limit", MaxRootFiles)
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("scan .proto directory: %w", err)
		}
	}
	if len(roots) == 0 {
		return nil, nil, fmt.Errorf("compile .proto: selected directory contains no .proto files")
	}
	return roots, includeDirs, nil
}

// CompileSources compiles virtual import paths in memory. Missing imports may
// resolve only to bundled standard schemas; this API never reads local files.
func CompileSources(ctx context.Context, sources map[string]string, roots []string) (*registry.Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(sources) == 0 || len(sources) > MaxFiles {
		return nil, fmt.Errorf("compile .proto: provide between 1 and %d source files", MaxFiles)
	}
	total := 0
	var sourceWork sourceBudget
	copySources := make(map[string]string, len(sources))
	for name, body := range sources {
		if !validImportPath(name) {
			return nil, fmt.Errorf("compile .proto: invalid virtual import path")
		}
		if len(body) > MaxFileBytes {
			return nil, fmt.Errorf("source %q exceeds the 2 MiB source limit", name)
		}
		total += len(body)
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("schema exceeds the 16 MiB source limit")
		}
		if err := sourceWork.add([]byte(body)); err != nil {
			return nil, err
		}
		copySources[name] = body
	}
	if len(roots) == 0 {
		for name := range copySources {
			roots = append(roots, name)
		}
		sort.Strings(roots)
	}
	if len(roots) > MaxRootFiles {
		return nil, fmt.Errorf("too many root .proto files")
	}
	for _, name := range roots {
		if _, ok := copySources[name]; !ok {
			return nil, fmt.Errorf("root .proto file %q is not in supplied sources", name)
		}
	}
	resolver := protocompile.WithStandardImports(protocompile.ResolverFunc(func(name string) (protocompile.SearchResult, error) {
		if err := ctx.Err(); err != nil {
			return protocompile.SearchResult{}, err
		}
		if !validImportPath(name) {
			return protocompile.SearchResult{}, fmt.Errorf("invalid virtual import path")
		}
		if body, ok := copySources[name]; ok {
			return protocompile.SearchResult{Source: strings.NewReader(body)}, nil
		}
		return protocompile.SearchResult{}, &MissingImportError{Path: name}
	}))
	return compile(ctx, roots, resolver)
}
