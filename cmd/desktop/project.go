package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"relay/internal/domain"
	"relay/internal/project"
)

func (s *FileService) SelectProjectFile(ctx context.Context) (domain.SelectedProjectFile, error) {
	if !nativeFilesAvailable {
		return domain.SelectedProjectFile{}, errors.New("выбор файлов доступен в desktop-приложении")
	}
	if err := ctx.Err(); err != nil {
		return domain.SelectedProjectFile{}, err
	}
	path, err := s.shell.Dialog.OpenFile().SetTitle("Импорт проекта или коллекции").CanChooseFiles(true).CanChooseDirectories(false).AddFilter("JSON project or collection", "*.json").PromptForSingleSelection()
	if err != nil {
		return domain.SelectedProjectFile{}, errors.New("не удалось открыть диалог выбора проекта")
	}
	if path == "" {
		return domain.SelectedProjectFile{}, nil
	}
	return readSelectedProject(ctx, path)
}

// Only this OS dialog supplies a path. JavaScript cannot read arbitrary files.
func readSelectedProject(ctx context.Context, path string) (domain.SelectedProjectFile, error) {
	if err := ctx.Err(); err != nil {
		return domain.SelectedProjectFile{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return domain.SelectedProjectFile{}, errors.New("выбранный проект недоступен")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > project.MaxBytes {
		return domain.SelectedProjectFile{}, errors.New("выберите обычный JSON-файл размером до 32 МиБ")
	}
	data, err := io.ReadAll(io.LimitReader(projectFileReader{ctx: ctx, reader: file}, project.MaxBytes+1))
	if err != nil || len(data) > project.MaxBytes || !utf8.Valid(data) {
		return domain.SelectedProjectFile{}, errors.New("не удалось прочитать UTF-8 проект размером до 32 МиБ")
	}
	return domain.SelectedProjectFile{Name: filepath.Base(path), Data: string(data)}, nil
}

func (s *FileService) SaveProjectFile(ctx context.Context, workspaceID string, includeBodies bool) (domain.ProjectFileExport, error) {
	if !nativeFilesAvailable {
		return domain.ProjectFileExport{}, errors.New("сохранение файлов доступно в desktop-приложении")
	}
	exported, err := s.service.ExportProject(ctx, workspaceID, includeBodies)
	if err != nil {
		return domain.ProjectFileExport{}, err
	}
	result := domain.ProjectFileExport{Stats: exported.Stats, Warnings: exported.Warnings}
	path, err := s.shell.Dialog.SaveFile().SetFilename(exported.Filename).AddFilter("Relay project JSON", "*.json").PromptForSingleSelection()
	if err != nil {
		return result, errors.New("не удалось открыть диалог сохранения проекта")
	}
	if path == "" {
		return result, nil
	}
	if err := writeSelectedProject(ctx, path, exported.Data); err != nil {
		return result, err
	}
	result.Saved = true
	return result, nil
}

// A private sibling is renamed only after a complete write. Failure leaves an
// existing export intact, including when the operation is canceled.
func writeSelectedProject(ctx context.Context, path, data string) error {
	if len(data) > project.MaxBytes {
		return errors.New("экспорт превышает 32 МиБ")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".relay-project-*")
	if err != nil {
		return errors.New("не удалось создать файл проекта")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if _, err := io.WriteString(file, data); err != nil {
		return errors.New("не удалось записать проект")
	}
	if err := file.Sync(); err != nil {
		return errors.New("не удалось сохранить проект")
	}
	if err := file.Close(); err != nil {
		return errors.New("не удалось закрыть файл проекта")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return errors.New("не удалось заменить выбранный файл проекта")
	}
	return nil
}

type projectFileReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r projectFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
