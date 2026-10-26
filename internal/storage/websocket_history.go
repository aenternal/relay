package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"relay/internal/domain"
)

const (
	MaxWebSocketHistoryRecordBytes = 8 << 20
	MaxWebSocketHistoryBytes       = 32 << 20
	MaxWebSocketHistorySessions    = 16
)

func webSocketHistoryID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n") && utf8.ValidString(id)
}

func cleanWebSocketHistory(input domain.WebSocketSavedSession) (domain.WebSocketSavedSession, error) {
	if !webSocketHistoryID(input.ID) || !webSocketHistoryID(input.RequestID) || !webSocketHistoryID(input.WorkspaceID) || !webSocketHistoryID(input.Execution.ID) || input.Execution.RequestID != input.RequestID || input.Execution.WorkspaceID != input.WorkspaceID {
		return domain.WebSocketSavedSession{}, invalid("invalid WebSocket session ownership")
	}
	if input.Execution.State != "closed" && input.Execution.State != "failed" || input.OmittedEvents < 0 || len(input.Events) > 2000 {
		return domain.WebSocketSavedSession{}, invalid("only bounded completed WebSocket sessions can be saved")
	}
	for _, stamp := range []string{input.SavedAt, input.Execution.StartedAt, input.Execution.FinishedAt} {
		if len(stamp) > 64 {
			return domain.WebSocketSavedSession{}, invalid("invalid WebSocket session timestamp")
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			return domain.WebSocketSavedSession{}, invalid("invalid WebSocket session timestamp")
		}
	}
	result := input
	result.Execution.Warnings = slices.Clone(input.Execution.Warnings)
	result.Execution.HandshakeHeaders = slices.Clone(input.Execution.HandshakeHeaders)
	result.Events = slices.Clone(input.Events)
	// This is display metadata, never a reconnect target. Masking a short secret
	// may replace digits inside the host or port; validate its safe placeholder
	// representation while retaining the actual redacted display string.
	if result.Execution.URL != "[REDACTED]" {
		u, err := url.Parse(strings.ReplaceAll(result.Execution.URL, "[REDACTED]", "0"))
		if err != nil || u.User != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.Fragment != "" {
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket URL")
		}
	}
	// Query values are handshake credentials in many protocols; session history
	// never needs them to replay a saved request definition.
	result.Execution.URL, _, _ = strings.Cut(result.Execution.URL, "?")
	if !result.IncludePayloads {
		result.Execution.HandshakeHeaders = []domain.Header{}
		result.Execution.CloseReason = ""
	} else {
		result.Execution.HandshakeHeaders = slices.DeleteFunc(result.Execution.HandshakeHeaders, func(row domain.Header) bool { return sensitiveName(row.Name) })
	}
	if input.Execution.TLS != nil {
		copy := *input.Execution.TLS
		copy.DNSNames = slices.Clone(copy.DNSNames)
		result.Execution.TLS = &copy
	}
	budget := MaxWebSocketHistoryRecordBytes
	check := func(value string, limit int) bool {
		budget -= len(value)
		return budget >= 0 && len(value) <= limit && utf8.ValidString(value)
	}
	if !check(result.Execution.URL, 65536) || !check(result.Execution.Error, 4096) || !check(result.Execution.CloseReason, 4096) || !check(result.Execution.Subprotocol, 256) || len(result.Execution.HandshakeHeaders) > 1024 || len(result.Execution.Warnings) > 128 || result.Execution.SentCount < 0 || result.Execution.ReceivedCount < 0 || result.Execution.DroppedEvents < 0 || result.Execution.SentBytes < 0 || result.Execution.ReceivedBytes < 0 {
		return domain.WebSocketSavedSession{}, invalid("WebSocket session metadata exceeds its limit")
	}
	for _, warning := range result.Execution.Warnings {
		if !check(warning, 4096) {
			return domain.WebSocketSavedSession{}, invalid("WebSocket session warning exceeds its limit")
		}
	}
	for _, row := range result.Execution.HandshakeHeaders {
		if !check(row.Name, 4096) || !check(row.Value, 65536) {
			return domain.WebSocketSavedSession{}, invalid("saved WebSocket header exceeds its limit")
		}
	}
	if tls := result.Execution.TLS; tls != nil {
		if len(tls.DNSNames) > 256 {
			return domain.WebSocketSavedSession{}, invalid("saved WebSocket TLS metadata exceeds its limit")
		}
		for _, value := range append([]string{tls.Version, tls.ServerName, tls.NegotiatedProtocol, tls.CipherSuite, tls.Subject, tls.Issuer, tls.ExpiresAt}, tls.DNSNames...) {
			if !check(value, 16384) {
				return domain.WebSocketSavedSession{}, invalid("saved WebSocket TLS metadata exceeds its limit")
			}
		}
	}
	previous := 0
	for index := range result.Events {
		event := &result.Events[index]
		event.Warnings = slices.Clone(event.Warnings)
		if event.Sequence <= previous || event.Bytes < 0 || event.Bytes > 4<<20 || len(event.Warnings) > 128 || (event.Direction != "in" && event.Direction != "out" && event.Direction != "local") {
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket event")
		}
		previous = event.Sequence
		switch event.Type {
		case "connected", "sent", "received", "ping", "pong", "close", "error":
		default:
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket event type")
		}
		if event.MessageType != "" && event.MessageType != "text" && event.MessageType != "binary" {
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket message type")
		}
		if len(event.Timestamp) > 64 {
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket event timestamp")
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket event timestamp")
		}
		if !result.IncludePayloads {
			event.Text, event.DataBase64 = "", ""
		}
		if !check(event.Text, 64<<10) || !check(event.DataBase64, base64.StdEncoding.EncodedLen(64<<10)) {
			return domain.WebSocketSavedSession{}, invalid("saved WebSocket event preview exceeds its limit")
		}
		if event.DataBase64 != "" {
			if _, err := base64.StdEncoding.Strict().DecodeString(event.DataBase64); err != nil {
				return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket event encoding")
			}
		}
		for _, warning := range event.Warnings {
			if !check(warning, 4096) {
				return domain.WebSocketSavedSession{}, invalid("saved WebSocket event warning exceeds its limit")
			}
		}
	}
	return result, nil
}

func webSocketSummary(session domain.WebSocketSavedSession) domain.WebSocketSessionSummary {
	return domain.WebSocketSessionSummary{ID: session.ID, WorkspaceID: session.WorkspaceID, RequestID: session.RequestID, SavedAt: session.SavedAt, URL: session.Execution.URL, State: session.Execution.State, CloseCode: session.Execution.CloseCode, EventCount: len(session.Events), IncludePayloads: session.IncludePayloads, OmittedEvents: session.OmittedEvents}
}

func (db *DB) SaveWebSocketHistory(ctx context.Context, session domain.WebSocketSavedSession) (domain.WebSocketSavedSession, error) {
	session.SavedAt = time.Now().UTC().Format(time.RFC3339Nano)
	clean, err := cleanWebSocketHistory(session)
	if err != nil {
		return domain.WebSocketSavedSession{}, err
	}
	data, err := json.Marshal(clean)
	if err != nil {
		return domain.WebSocketSavedSession{}, invalid("encode WebSocket history failed")
	}
	summary, err := json.Marshal(webSocketSummary(clean))
	if err != nil || len(data)+len(summary) > MaxWebSocketHistoryRecordBytes {
		return domain.WebSocketSavedSession{}, invalid("WebSocket session exceeds the 8 MiB saved history limit")
	}
	if err := validateWebSocketHistoryJSON(data); err != nil {
		return domain.WebSocketSavedSession{}, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.WebSocketSavedSession{}, databaseError(ctx, "begin WebSocket history save", err)
	}
	defer tx.Rollback()
	var owner []byte
	err = tx.QueryRowContext(ctx, "SELECT payload FROM requests WHERE id=? AND workspace_id=?", clean.RequestID, clean.WorkspaceID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebSocketSavedSession{}, invalid("WebSocket session request no longer exists")
	}
	if err != nil {
		return domain.WebSocketSavedSession{}, databaseError(ctx, "read WebSocket history owner", err)
	}
	var request domain.RequestDefinition
	if json.Unmarshal(owner, &request) != nil || request.Protocol != "websocket" {
		return domain.WebSocketSavedSession{}, invalid("WebSocket session owner must be a WebSocket request")
	}
	changed, err := tx.ExecContext(ctx, `INSERT INTO websocket_sessions(id,workspace_id,request_id,saved_at,bytes,summary,payload) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET saved_at=excluded.saved_at, bytes=excluded.bytes, summary=excluded.summary, payload=excluded.payload
		WHERE websocket_sessions.workspace_id=excluded.workspace_id AND websocket_sessions.request_id=excluded.request_id`, clean.ID, clean.WorkspaceID, clean.RequestID, clean.SavedAt, len(data)+len(summary), string(summary), string(data))
	if err != nil {
		return domain.WebSocketSavedSession{}, databaseError(ctx, "write WebSocket history", err)
	}
	if n, err := changed.RowsAffected(); err != nil || n != 1 {
		return domain.WebSocketSavedSession{}, invalid("WebSocket history session ownership mismatch")
	}
	for {
		var count, total int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(bytes),0) FROM websocket_sessions").Scan(&count, &total); err != nil {
			return domain.WebSocketSavedSession{}, databaseError(ctx, "measure WebSocket history", err)
		}
		if count <= MaxWebSocketHistorySessions && total <= MaxWebSocketHistoryBytes {
			break
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM websocket_sessions WHERE id=(SELECT id FROM websocket_sessions ORDER BY saved_at,id LIMIT 1)"); err != nil {
			return domain.WebSocketSavedSession{}, databaseError(ctx, "trim WebSocket history", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.WebSocketSavedSession{}, databaseError(ctx, "commit WebSocket history", err)
	}
	return clean, nil
}

func (db *DB) ListWebSocketHistory(ctx context.Context, workspaceID string) ([]domain.WebSocketSessionSummary, error) {
	if !webSocketHistoryID(workspaceID) {
		return nil, invalid("invalid WebSocket history workspace")
	}
	rows, err := db.sql.QueryContext(ctx, "SELECT summary FROM websocket_sessions WHERE workspace_id=? ORDER BY saved_at DESC,id DESC LIMIT 16", workspaceID)
	if err != nil {
		return nil, databaseError(ctx, "list WebSocket history", err)
	}
	defer rows.Close()
	items := []domain.WebSocketSessionSummary{}
	for rows.Next() {
		var data []byte
		var item domain.WebSocketSessionSummary
		if err := rows.Scan(&data); err != nil {
			return nil, databaseError(ctx, "read WebSocket history summary", err)
		}
		if len(data) > 128<<10 || json.Unmarshal(data, &item) != nil || item.WorkspaceID != workspaceID || !webSocketHistoryID(item.ID) {
			return nil, invalid("invalid stored WebSocket history summary")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, "read WebSocket history summaries", err)
	}
	return items, nil
}

func (db *DB) GetWebSocketHistory(ctx context.Context, workspaceID, id string) (domain.WebSocketSavedSession, error) {
	if !webSocketHistoryID(workspaceID) || !webSocketHistoryID(id) {
		return domain.WebSocketSavedSession{}, invalid("invalid WebSocket history identity")
	}
	var data []byte
	err := db.sql.QueryRowContext(ctx, "SELECT payload FROM websocket_sessions WHERE workspace_id=? AND id=?", workspaceID, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebSocketSavedSession{}, invalid("saved WebSocket session is no longer available")
	}
	if err != nil {
		return domain.WebSocketSavedSession{}, databaseError(ctx, "load WebSocket history", err)
	}
	if err := validateWebSocketHistoryJSON(data); err != nil {
		return domain.WebSocketSavedSession{}, err
	}
	var session domain.WebSocketSavedSession
	if json.Unmarshal(data, &session) != nil || session.ID != id || session.WorkspaceID != workspaceID {
		return domain.WebSocketSavedSession{}, invalid("invalid saved WebSocket session")
	}
	return cleanWebSocketHistory(session)
}

// Validate structure before materializing slices from a saved or externally
// modified database. A small JSON array of empty objects otherwise allocates
// much more than its encoded byte budget during typed unmarshal.
func validateWebSocketHistoryJSON(data []byte) error {
	if len(data) == 0 || len(data) > MaxWebSocketHistoryRecordBytes {
		return invalid("saved WebSocket history exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	budget := 100_000
	var scan func(int, bool) error
	scan = func(depth int, events bool) error {
		budget--
		if depth > 32 || budget < 0 {
			return invalid("saved WebSocket history exceeds its structural limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return invalid("invalid saved WebSocket history JSON")
		}
		switch token {
		case json.Delim('{'):
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return invalid("invalid saved WebSocket history JSON")
				}
				budget--
				if err := scan(depth+1, depth == 0 && key == "events"); err != nil {
					return err
				}
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
				return invalid("invalid saved WebSocket history JSON")
			}
		case json.Delim('['):
			count := 0
			for decoder.More() {
				count++
				if events && count > 2000 {
					return invalid("too many saved WebSocket history events")
				}
				if err := scan(depth+1, false); err != nil {
					return err
				}
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
				return invalid("invalid saved WebSocket history JSON")
			}
		case json.Delim('}'), json.Delim(']'):
			return invalid("invalid saved WebSocket history JSON")
		}
		return nil
	}
	if err := scan(0, false); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid("invalid saved WebSocket history JSON")
	}
	return nil
}

func (db *DB) DeleteWebSocketHistory(ctx context.Context, workspaceID, id string) error {
	if !webSocketHistoryID(workspaceID) || !webSocketHistoryID(id) {
		return invalid("invalid WebSocket history identity")
	}
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM websocket_sessions WHERE workspace_id=? AND id=?", workspaceID, id); err != nil {
		return databaseError(ctx, "delete WebSocket history", err)
	}
	return nil
}
