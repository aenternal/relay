package main

import (
	"context"
	"errors"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"relay/internal/app"
	"relay/internal/domain"
)

// FileService grants access only after the user selects a file in an OS dialog.
// Paths and private-key bytes never cross the UI bridge.
type FileService struct {
	service     *app.Service
	shell       *application.App
	protoMu     sync.Mutex
	protoGrants map[string]nativeProtoGrant
}

func (*FileService) NativeFilesAvailable() bool { return nativeFilesAvailable }

func (s *FileService) SelectHTTPFile(ctx context.Context, kind string) (domain.SelectedFile, error) {
	if !nativeFilesAvailable {
		return domain.SelectedFile{}, errors.New("выбор файлов доступен в desktop-приложении")
	}
	if err := ctx.Err(); err != nil {
		return domain.SelectedFile{}, err
	}
	titles := map[string]string{"body": "Выберите файл запроса", "ca": "Выберите CA сертификат PEM", "certificate": "Выберите клиентский сертификат PEM", "private-key": "Выберите закрытый ключ PEM"}
	title, ok := titles[kind]
	if !ok {
		return domain.SelectedFile{}, errors.New("неизвестный вид файла")
	}
	dialog := s.shell.Dialog.OpenFile().SetTitle(title).CanChooseFiles(true).CanChooseDirectories(false)
	if kind != "body" {
		dialog.AddFilter("PEM certificates and keys", "*.pem;*.crt;*.cer;*.key")
	}
	path, err := dialog.PromptForSingleSelection()
	if err != nil {
		return domain.SelectedFile{}, errors.New("не удалось открыть диалог выбора файла")
	}
	if path == "" {
		return domain.SelectedFile{}, nil
	}
	if err := ctx.Err(); err != nil {
		return domain.SelectedFile{}, err
	}
	return app.RegisterHTTPFile(s.service, path)
}

func (s *FileService) SaveHTTPResponse(ctx context.Context, id string) (bool, error) {
	if !nativeFilesAvailable {
		return false, errors.New("сохранение файла доступно в desktop-приложении")
	}
	result, err := s.service.GetHTTPExecution(ctx, id)
	if err != nil {
		return false, err
	}
	if !result.DownloadAvailable {
		return false, errors.New("полный ответ ещё недоступен для сохранения")
	}
	path, err := s.shell.Dialog.SaveFile().SetFilename("response.bin").PromptForSingleSelection()
	if err != nil {
		return false, errors.New("не удалось открыть диалог сохранения")
	}
	if path == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := app.SaveHTTPResponse(s.service, id, path); err != nil {
		return false, err
	}
	return true, nil
}
