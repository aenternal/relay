package app

import (
	"errors"
	"sync"
)

// Shutdown closes admission, cancels live HTTP/gRPC operations, and gives
// WebSocket peers their bounded going-away handshake concurrently. The caller
// should cancel the application context afterwards, so the WebSocket grace
// period can send its close frame. It is safe to call Shutdown concurrently or
// repeatedly. No new bridge method is exposed by this package-level helper.
func Shutdown(s *Service) error {
	if s == nil {
		return nil
	}
	operations := [...]func(*Service) error{ShutdownHTTP, ShutdownGRPC, ShutdownWebSocket}
	results := make([]error, len(operations))
	var work sync.WaitGroup
	work.Add(len(operations))
	for i, operation := range operations {
		go func() {
			defer work.Done()
			results[i] = operation(s)
		}()
	}
	work.Wait()
	return errors.Join(results...)
}
