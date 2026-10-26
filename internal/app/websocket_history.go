package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"relay/internal/domain"
)

// History is optional so repositories implementing the original snapshot API do
// not need a second persistence concern unless they support explicit sessions.
type webSocketHistoryRepository interface {
	SaveWebSocketHistory(context.Context, domain.WebSocketSavedSession) (domain.WebSocketSavedSession, error)
	ListWebSocketHistory(context.Context, string) ([]domain.WebSocketSessionSummary, error)
	GetWebSocketHistory(context.Context, string, string) (domain.WebSocketSavedSession, error)
	DeleteWebSocketHistory(context.Context, string, string) error
}

func (s *Service) SaveWebSocketSession(ctx context.Context, id string, includePayloads bool) (domain.WebSocketSavedSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.WebSocketSavedSession{}, err
	}
	repo, ok := s.repo.(webSocketHistoryRepository)
	if !ok {
		return domain.WebSocketSavedSession{}, errors.New("saved WebSocket sessions are unavailable in this storage backend")
	}
	s.wsMu.Lock()
	r := s.initWebSocket().records[id]
	if r == nil {
		s.wsMu.Unlock()
		return domain.WebSocketSavedSession{}, errors.New("WebSocket session is no longer available")
	}
	if webSocketActive(r.result.State) {
		s.wsMu.Unlock()
		return domain.WebSocketSavedSession{}, errors.New("disconnect the WebSocket before saving its session")
	}
	// Commands are joined before terminal state is published, so the final
	// secret snapshot covers all sends, including a failed template resolution.
	secrets := slices.Clone(r.secrets)
	session := domain.WebSocketSavedSession{ID: newID(), WorkspaceID: r.result.WorkspaceID, RequestID: r.result.RequestID, SavedAt: time.Now().UTC().Format(time.RFC3339Nano), IncludePayloads: includePayloads, OmittedEvents: r.historyOmitted, Execution: cloneWebSocketExecution(r.result), Events: make([]domain.WebSocketEvent, 0, len(r.history))}
	for _, event := range r.history {
		session.Events = append(session.Events, cloneWebSocketEvent(event))
	}
	s.wsMu.Unlock()
	for index, event := range session.Events {
		event = sanitizeWebSocketEvent(event, secrets)
		if !includePayloads {
			event.Text = ""
			event.DataBase64 = ""
		}
		session.Events[index] = event
	}
	if !includePayloads {
		session.Execution.HandshakeHeaders = []domain.Header{}
		session.Execution.CloseReason = ""
	}
	// JSON escaping and base64 representation can increase a bounded ring's
	// serialized size. Trim oldest events before the storage record budget.
	metadata := session
	metadata.Events = []domain.WebSocketEvent{}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return domain.WebSocketSavedSession{}, errors.New("WebSocket session could not be encoded")
	}
	const budget = (8 << 20) - (128 << 10) - 32 // omitted-event digit growth also fits
	if len(encoded) > budget {
		return domain.WebSocketSavedSession{}, errors.New("WebSocket session metadata exceeds its size limit")
	}
	sizes := make([]int, len(session.Events))
	bytes := len(encoded)
	for index, event := range session.Events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return domain.WebSocketSavedSession{}, errors.New("WebSocket event could not be encoded")
		}
		sizes[index] = len(encoded) + 1
		bytes += sizes[index]
	}
	first := 0
	for first < len(session.Events) && bytes > budget {
		bytes -= sizes[first]
		session.Events[first] = domain.WebSocketEvent{}
		first++
		session.OmittedEvents++
	}
	session.Events = session.Events[first:]
	s.mu.Lock()
	defer s.mu.Unlock()
	return repo.SaveWebSocketHistory(ctx, session)
}

func (s *Service) ListWebSocketSessions(ctx context.Context, workspaceID string) ([]domain.WebSocketSessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repo, ok := s.repo.(webSocketHistoryRepository)
	if !ok {
		return nil, errors.New("saved WebSocket sessions are unavailable in this storage backend")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return repo.ListWebSocketHistory(ctx, workspaceID)
}
func (s *Service) GetSavedWebSocketSession(ctx context.Context, workspaceID, id string) (domain.WebSocketSavedSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.WebSocketSavedSession{}, err
	}
	repo, ok := s.repo.(webSocketHistoryRepository)
	if !ok {
		return domain.WebSocketSavedSession{}, errors.New("saved WebSocket sessions are unavailable in this storage backend")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return repo.GetWebSocketHistory(ctx, workspaceID, id)
}
func (s *Service) DeleteSavedWebSocketSession(ctx context.Context, workspaceID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repo, ok := s.repo.(webSocketHistoryRepository)
	if !ok {
		return errors.New("saved WebSocket sessions are unavailable in this storage backend")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return repo.DeleteWebSocketHistory(ctx, workspaceID, id)
}
