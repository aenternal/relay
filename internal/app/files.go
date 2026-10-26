package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"relay/internal/domain"
)

type httpFileGrant struct {
	path string
	info os.FileInfo
}

// RegisterHTTPFile is called only by the native file picker. Webview request
// payloads carry opaque grants and cannot grant themselves filesystem access.
func RegisterHTTPFile(s *Service, path string) (domain.SelectedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return domain.SelectedFile{}, errors.New("selected file could not be opened")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return domain.SelectedFile{}, errors.New("select a regular file")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return domain.SelectedFile{}, errors.New("invalid selected file")
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	state := s.initHTTP()
	if state.closed {
		return domain.SelectedFile{}, errors.New("application is shutting down")
	}
	if len(state.files) >= 256 {
		return domain.SelectedFile{}, errors.New("too many selected files; restart to clear unused grants")
	}
	id := newID()
	state.files[id] = httpFileGrant{path: absolute, info: info}
	return domain.SelectedFile{ID: id, Name: info.Name(), Size: info.Size()}, nil
}

func (s *Service) httpFile(id string) (httpFileGrant, error) {
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	grant, ok := s.initHTTP().files[id]
	if !ok {
		return httpFileGrant{}, errors.New("select the file again using the native file picker")
	}
	return grant, nil
}

func openHTTPFile(grant httpFileGrant) (*os.File, error) {
	f, err := os.Open(grant.path)
	if err != nil {
		return nil, errors.New("selected file is unavailable; select it again")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(grant.info, info) || info.Size() != grant.info.Size() || !info.ModTime().Equal(grant.info.ModTime()) {
		f.Close()
		return nil, errors.New("selected file changed; select it again")
	}
	return f, nil
}

// SaveHTTPResponse is called only after a native save dialog chose the path.
// The open descriptor pins the spool while Forget or Shutdown removes its name.
func SaveHTTPResponse(s *Service, id, path string) error {
	s.httpMu.Lock()
	record, ok := s.initHTTP().records[id]
	if !ok || record.result.State == "running" || record.spool == "" {
		s.httpMu.Unlock()
		return errors.New("response download is unavailable")
	}
	source, err := os.Open(record.spool)
	s.httpMu.Unlock()
	if err != nil {
		return errors.New("response download is unavailable")
	}
	defer source.Close()
	// A temporary sibling makes failed copies leave an existing destination intact.
	destination, err := os.CreateTemp(filepath.Dir(path), ".relay-download-*")
	if err != nil {
		return errors.New("download destination could not be opened")
	}
	temporary := destination.Name()
	defer os.Remove(temporary)
	if _, err = io.Copy(destination, source); err != nil {
		destination.Close()
		return errors.New("response download could not be saved")
	}
	if err = destination.Sync(); err != nil {
		destination.Close()
		return errors.New("response download could not be saved")
	}
	if err = destination.Close(); err != nil {
		return errors.New("response download could not be saved")
	}
	if err = os.Rename(temporary, path); err != nil {
		return errors.New("response download could not replace the destination")
	}
	return nil
}

// ShutdownHTTP cancels live connections and removes all private response spools.
func ShutdownHTTP(s *Service) error {
	s.httpMu.Lock()
	state := s.initHTTP()
	state.closed = true
	for _, record := range state.records {
		record.cancel()
	}
	s.httpMu.Unlock()
	state.work.Wait()
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	var result error
	for _, record := range state.records {
		if record.spool != "" {
			if err := os.Remove(record.spool); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.New("some temporary responses could not be removed")
			}
		}
	}
	clear(state.records)
	clear(state.files)
	state.jar = nil
	state.reserved = 0
	return result
}

// A small context wrapper also bounds file reads when their reader is used by
// multipart.Copy. File descriptors are closed by each request's body lifecycle.
type httpContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r httpContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
