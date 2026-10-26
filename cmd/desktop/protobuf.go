package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"relay/internal/app"
	"relay/internal/domain"
)

type NativeProtoSelection struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Files     []string `json:"files"`
	RootFiles []string `json:"rootFiles"`
}

type NativeProtoImportInput struct {
	SelectionID string   `json:"selectionId"`
	IncludeIDs  []string `json:"includeIds"`
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspaceId"`
	Name        string   `json:"name"`
}

type nativeProtoGrant struct {
	kind  string
	paths []string
}

// Native selections grant only explicit source files/include roots. Absolute
// filesystem paths never become bridge inputs or persisted schema metadata.
func (s *FileService) grantProto(kind string, paths []string) (string, error) {
	if len(paths) == 0 || len(paths) > 256 {
		return "", errors.New("выберите от 1 до 256 файлов")
	}
	canonical := make([]string, 0, len(paths))
	for _, name := range paths {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return "", errors.New("не удалось определить выбранный путь")
		}
		real, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", errors.New("выбранный источник недоступен")
		}
		info, err := os.Stat(real)
		if err != nil || (kind == "include" || kind == "folder") && !info.IsDir() || kind != "include" && kind != "folder" && !info.Mode().IsRegular() {
			return "", errors.New("выбранный источник имеет неподходящий тип")
		}
		if kind == "files" && filepath.Ext(real) != ".proto" {
			return "", errors.New("выберите файлы с расширением .proto")
		}
		canonical = append(canonical, real)
	}
	s.protoMu.Lock()
	defer s.protoMu.Unlock()
	if s.protoGrants == nil {
		s.protoGrants = make(map[string]nativeProtoGrant)
	}
	if len(s.protoGrants) >= 128 {
		return "", errors.New("слишком много выбранных источников; перезапустите приложение")
	}
	id := rand.Text()
	s.protoGrants[id] = nativeProtoGrant{kind: kind, paths: canonical}
	return id, nil
}

func (s *FileService) SelectProtoImport(ctx context.Context, kind string) (NativeProtoSelection, error) {
	if !nativeFilesAvailable {
		return NativeProtoSelection{}, errors.New("системный выбор схем доступен в desktop-приложении")
	}
	if err := ctx.Err(); err != nil {
		return NativeProtoSelection{}, err
	}
	dialog := s.shell.Dialog.OpenFile()
	var paths []string
	var err error
	switch kind {
	case "files":
		dialog.SetTitle("Выберите корневые .proto файлы").CanChooseFiles(true).CanChooseDirectories(false).AddFilter("Protobuf source", "*.proto")
		paths, err = dialog.PromptForMultipleSelection()
	case "folder":
		dialog.SetTitle("Выберите папку со схемами .proto").CanChooseFiles(false).CanChooseDirectories(true)
		var path string
		path, err = dialog.PromptForSingleSelection()
		if path != "" {
			paths = []string{path}
		}
	case "descriptor":
		dialog.SetTitle("Выберите FileDescriptorSet").CanChooseFiles(true).CanChooseDirectories(false).AddFilter("Protobuf descriptor set", "*.protoset;*.pb;*.desc")
		var path string
		path, err = dialog.PromptForSingleSelection()
		if path != "" {
			paths = []string{path}
		}
	default:
		return NativeProtoSelection{}, errors.New("неизвестный источник схемы")
	}
	if err != nil {
		return NativeProtoSelection{}, errors.New("не удалось открыть диалог импорта схем")
	}
	if len(paths) == 0 {
		return NativeProtoSelection{Files: []string{}, RootFiles: []string{}}, nil
	}
	if err = ctx.Err(); err != nil {
		return NativeProtoSelection{}, err
	}
	id, err := s.grantProto(kind, paths)
	if err != nil {
		return NativeProtoSelection{}, err
	}
	s.protoMu.Lock()
	paths = slices.Clone(s.protoGrants[id].paths)
	s.protoMu.Unlock()
	selection := NativeProtoSelection{ID: id, Kind: "proto", Name: filepath.Base(paths[0]), Files: []string{}, RootFiles: []string{}}
	if kind == "descriptor" {
		selection.Kind = "descriptor"
	}
	if kind == "folder" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		entries := 0
		err = filepath.WalkDir(paths[0], func(path string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return walkErr
			}
			entries++
			if entries > 8192 {
				return errors.New("too many directory entries")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !entry.IsDir() && filepath.Ext(path) == ".proto" {
				if len(selection.Files) >= 256 {
					return errors.New("too many files")
				}
				relative, err := filepath.Rel(paths[0], path)
				if err != nil {
					return err
				}
				selection.Files = append(selection.Files, filepath.ToSlash(relative))
			}
			return nil
		})
		if err != nil || len(selection.Files) == 0 {
			s.protoMu.Lock()
			delete(s.protoGrants, id)
			s.protoMu.Unlock()
			return NativeProtoSelection{}, errors.New("папка недоступна, пуста или превышает лимит 256 .proto файлов / 8192 записей")
		}
	} else {
		for _, path := range paths {
			selection.Files = append(selection.Files, filepath.Base(path))
		}
	}
	selection.RootFiles = slices.Clone(selection.Files)
	return selection, nil
}

func (s *FileService) SelectProtoIncludeDirectory(ctx context.Context) (domain.SelectedFile, error) {
	if !nativeFilesAvailable {
		return domain.SelectedFile{}, errors.New("выбор include-папки доступен в desktop-приложении")
	}
	if err := ctx.Err(); err != nil {
		return domain.SelectedFile{}, err
	}
	path, err := s.shell.Dialog.OpenFile().SetTitle("Выберите include-папку (-I)").CanChooseFiles(false).CanChooseDirectories(true).PromptForSingleSelection()
	if err != nil {
		return domain.SelectedFile{}, errors.New("не удалось выбрать include-папку")
	}
	if path == "" {
		return domain.SelectedFile{}, nil
	}
	id, err := s.grantProto("include", []string{path})
	if err != nil {
		return domain.SelectedFile{}, err
	}
	return domain.SelectedFile{ID: id, Name: filepath.Base(path)}, nil
}

func (s *FileService) ImportNativeProtoSchema(ctx context.Context, input NativeProtoImportInput) (app.ProtoImportResult, error) {
	if !nativeFilesAvailable {
		return app.ProtoImportResult{}, errors.New("системный импорт доступен в desktop-приложении")
	}
	if len(input.IncludeIDs) > 32 {
		return app.ProtoImportResult{}, errors.New("слишком много include-папок")
	}
	s.protoMu.Lock()
	selected, found := s.protoGrants[input.SelectionID]
	var dirs []string
	for _, id := range input.IncludeIDs {
		include, exists := s.protoGrants[id]
		if !exists || include.kind != "include" {
			s.protoMu.Unlock()
			return app.ProtoImportResult{}, errors.New("выберите include-папку заново")
		}
		dirs = append(dirs, include.paths...)
	}
	s.protoMu.Unlock()
	if !found || selected.kind == "include" {
		return app.ProtoImportResult{}, errors.New("выберите источник схемы заново")
	}
	kind := "proto"
	if selected.kind == "descriptor" {
		kind = "descriptor"
	}
	return app.ImportProtoPaths(ctx, s.service, app.ProtoImport{ID: input.ID, WorkspaceID: input.WorkspaceID, Name: strings.TrimSpace(input.Name), Kind: kind}, selected.paths, dirs)
}
