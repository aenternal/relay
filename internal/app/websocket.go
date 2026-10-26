package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"relay/internal/domain"
	"relay/internal/security"
	"relay/internal/transport/websocketclient"
)

const (
	webSocketReservation  = 16 << 20
	webSocketQueueBytes   = 8 << 20
	webSocketHistoryBytes = 6 << 20
	webSocketPreviewBytes = 64 << 10
)

type webSocketState struct {
	records  map[string]*webSocketRecord
	reserved int
	closed   bool
	work     sync.WaitGroup
}
type webSocketRecord struct {
	result                                                                           domain.WebSocketExecution
	ctx                                                                              context.Context
	cancel                                                                           context.CancelFunc
	client                                                                           *websocketclient.Client
	prepared                                                                         *webSocketPrepared
	securityMu                                                                       sync.Mutex
	secrets                                                                          []string // snapshot protected by wsMu; callbacks never wait for keyring I/O
	queue, history                                                                   []domain.WebSocketEvent
	queueBytes, historyBytes, historyOmitted, next, acknowledged, delivered, reserve int
	sendBusy                                                                         chan struct{}
	commands                                                                         sync.WaitGroup
	lastPing                                                                         time.Time
	closing, canceled                                                                bool
	droppedControls                                                                  atomic.Int64
}

func (s *Service) initWebSocket() *webSocketState {
	if s.wsState == nil {
		s.wsState = &webSocketState{records: map[string]*webSocketRecord{}}
	}
	return s.wsState
}
func webSocketActive(state string) bool {
	return state == "connecting" || state == "open" || state == "closing"
}

func (s *Service) ConnectWebSocket(ctx context.Context, request domain.RequestDefinition) (domain.WebSocketExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.WebSocketExecution{}, err
	}
	if request.Protocol != "websocket" || request.WebSocket == nil {
		return domain.WebSocketExecution{}, errors.New("select a WebSocket request")
	}
	d := *request.WebSocket
	if _, _, _, _, err := webSocketLimits(d); err != nil {
		return domain.WebSocketExecution{}, err
	}
	if len(d.URL) > 65536 || len(d.Headers) > 1024 || len(d.Query) > 1024 || len(d.Subprotocols) > 64 {
		return domain.WebSocketExecution{}, errors.New("WebSocket request definition exceeds its limit")
	}
	d.Headers = slices.Clone(d.Headers)
	d.Query = slices.Clone(d.Query)
	d.Subprotocols = slices.Clone(d.Subprotocols)
	d.Body = ""
	d.FileID = ""
	request.WebSocket = &d
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	work, cancel := context.WithCancel(base)
	result := domain.WebSocketExecution{ID: newID(), RequestID: request.ID, WorkspaceID: request.WorkspaceID, State: "connecting", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), HandshakeHeaders: []domain.Header{}, Warnings: []string{}}
	record := &webSocketRecord{result: result, ctx: work, cancel: cancel, sendBusy: make(chan struct{}, 1), reserve: webSocketReservation, queue: []domain.WebSocketEvent{}, history: []domain.WebSocketEvent{}}
	s.wsMu.Lock()
	state := s.initWebSocket()
	if state.closed {
		s.wsMu.Unlock()
		cancel()
		return domain.WebSocketExecution{}, errors.New("application is shutting down")
	}
	active := 0
	for _, r := range state.records {
		if webSocketActive(r.result.State) {
			active++
		}
	}
	if active >= 4 {
		s.wsMu.Unlock()
		cancel()
		return domain.WebSocketExecution{}, errors.New("at most four WebSocket connections may run concurrently")
	}
	for len(state.records) >= 16 || state.reserved+record.reserve > 64<<20 {
		var victim *webSocketRecord
		victimID := ""
		for id, r := range state.records {
			if webSocketActive(r.result.State) || len(r.queue) > 0 {
				continue
			}
			if victim == nil || r.result.StartedAt < victim.result.StartedAt {
				victim, victimID = r, id
			}
		}
		if victim == nil {
			s.wsMu.Unlock()
			cancel()
			return domain.WebSocketExecution{}, errors.New("WebSocket memory limit reached; read or clear a completed session, or disconnect another connection")
		}
		state.reserved -= victim.reserve
		delete(state.records, victimID)
	}
	state.records[result.ID] = record
	state.reserved += record.reserve
	state.work.Add(1)
	s.wsMu.Unlock()
	go s.runWebSocket(request, record)
	return cloneWebSocketExecution(result), nil
}

func (s *Service) runWebSocket(request domain.RequestDefinition, r *webSocketRecord) {
	defer s.wsState.work.Done()
	started := time.Now()
	p, err := s.prepareWebSocket(r.ctx, request)
	s.wsMu.Lock()
	r.prepared = p
	r.secrets = slices.Clone(p.secrets)
	if parsed, e := url.Parse(p.url); e == nil {
		r.result.URL = safeHTTPURL(parsed, r.secrets)
	}
	if request.WebSocket.TLS.Insecure && strings.HasPrefix(p.url, "wss:") {
		r.result.Warnings = append(r.result.Warnings, "TLS certificate verification is disabled for this connection")
	}
	s.wsMu.Unlock()
	if err != nil {
		s.finishWebSocket(r, webSocketPreparationError{err}, started)
		return
	}
	p.options.OnPing = func(data []byte) { s.webSocketControl(r, "ping", "in", data) }
	p.options.OnPong = func(data []byte) { s.webSocketControl(r, "pong", "in", data) }
	client, handshake, err := websocketclient.Dial(r.ctx, p.url, p.options)
	s.wsMu.Lock()
	r.client = client
	if handshake != nil {
		r.result.HandshakeStatusCode = handshake.StatusCode
		r.result.HandshakeHeaders = webSocketHeaders(handshake.Headers, r.secrets)
		r.result.Subprotocol = webSocketText(handshake.Subprotocol, r.secrets, 256)
		r.result.TLS = safeHTTPTLS(handshake.TLS, r.secrets)
		r.result.ConnectMS = float64(handshake.ConnectDuration) / float64(time.Millisecond)
	}
	if err == nil && r.ctx.Err() == nil {
		r.result.State = "open"
		s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: "connected", Direction: "local"})
	}
	s.wsMu.Unlock()
	if err != nil {
		s.finishWebSocket(r, err, started)
		return
	}
	defer client.Abort()
	if r.ctx.Err() != nil {
		s.finishWebSocket(r, r.ctx.Err(), started)
		return
	}
	for {
		readctx := r.ctx
		cancel := func() {}
		if p.readTimeout > 0 {
			readctx, cancel = context.WithTimeout(r.ctx, p.readTimeout)
		}
		message, readerr := client.Read(readctx)
		cancel()
		if readerr != nil {
			s.finishWebSocket(r, readerr, started)
			return
		}
		s.wsMu.Lock()
		event := webSocketPreview("received", "in", message.Type, message.Data, r.secrets)
		r.result.ReceivedCount++
		r.result.ReceivedBytes += int64(len(message.Data))
		s.appendWebSocketEvent(r, event)
		s.wsMu.Unlock()
	}
}

func (s *Service) finishWebSocket(r *webSocketRecord, err error, started time.Time) {
	// Closing the transport releases any pending writer before joining commands.
	s.wsMu.Lock()
	r.closing = true
	client := r.client
	s.wsMu.Unlock()
	r.cancel()
	if client != nil {
		_ = client.Abort()
	}
	r.commands.Wait()
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	r.result.State = "failed"
	r.result.CloseCode = 1006
	r.result.CleanClose = false
	if code, reason, ok := websocketclient.CloseInfo(err); ok {
		r.result.State = "failed"
		if websocketclient.IsPeerClose(err) {
			r.result.State = "closed"
		}
		r.result.CloseCode = code
		r.result.CloseReason = webSocketText(reason, r.secrets, 123)
		r.result.CleanClose = websocketclient.IsPeerClose(err) && code != 1006 && code != 1015
		if !r.result.CleanClose {
			r.result.Error = webSocketText(webSocketError(err), r.secrets, 4096)
		}
	} else if r.canceled {
		r.result.State = "closed"
		r.result.Error = "WebSocket connection canceled"
	} else if err != nil {
		r.result.Error = webSocketText(webSocketError(err), r.secrets, 4096)
	}
	r.result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	r.result.TotalMS = float64(time.Since(started)) / float64(time.Millisecond)
	if r.result.Error != "" {
		s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: "error", Direction: "local", Text: r.result.Error})
	}
	s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: "close", Direction: "in", Text: r.result.CloseReason})
	state := s.initWebSocket()
	state.reserved -= r.reserve
	r.prepared = nil
	r.client = nil
	r.reserve = r.queueBytes + r.historyBytes + (2 << 20)
	state.reserved += r.reserve
}

func (s *Service) webSocketControl(r *webSocketRecord, kind, direction string, data []byte) {
	if !s.wsMu.TryLock() {
		r.droppedControls.Add(1)
		return
	}
	defer s.wsMu.Unlock()
	if webSocketActive(r.result.State) {
		s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: kind, Direction: direction, MessageType: "binary", Bytes: len(data), Warnings: []string{"Control frame payload is omitted"}})
	}
}

func webSocketEventSize(e domain.WebSocketEvent) int {
	n := len(e.Text) + len(e.DataBase64) + len(e.Type) + len(e.Direction) + len(e.MessageType) + len(e.Timestamp) + 128
	for _, w := range e.Warnings {
		n += len(w)
	}
	return n
}
func cloneWebSocketEvent(e domain.WebSocketEvent) domain.WebSocketEvent {
	e.Warnings = slices.Clone(e.Warnings)
	return e
}
func cloneWebSocketExecution(e domain.WebSocketExecution) domain.WebSocketExecution {
	e.HandshakeHeaders = slices.Clone(e.HandshakeHeaders)
	e.Warnings = slices.Clone(e.Warnings)
	if e.TLS != nil {
		t := *e.TLS
		t.DNSNames = slices.Clone(t.DNSNames)
		e.TLS = &t
	}
	return e
}

// The reader never waits for UI acknowledgements: it must continue processing
// control frames so slow or suspended renderers cannot block pong and close.
func (s *Service) appendWebSocketEvent(r *webSocketRecord, event domain.WebSocketEvent) {
	r.result.DroppedEvents += int(r.droppedControls.Swap(0))
	r.next++
	event.Sequence = r.next
	event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	if event.Warnings == nil {
		event.Warnings = []string{}
	}
	size := webSocketEventSize(event)
	for len(r.queue) > 0 && (len(r.queue) >= 64 || r.queueBytes+size > webSocketQueueBytes) {
		r.queueBytes -= webSocketEventSize(r.queue[0])
		r.queue[0] = domain.WebSocketEvent{}
		r.queue = r.queue[1:]
		r.result.DroppedEvents++
	}
	r.queue = append(r.queue, event)
	r.queueBytes += size
	for len(r.history) > 0 && (len(r.history) >= 2000 || r.historyBytes+size > webSocketHistoryBytes) {
		r.historyBytes -= webSocketEventSize(r.history[0])
		r.history[0] = domain.WebSocketEvent{}
		r.history = r.history[1:]
		r.historyOmitted++
	}
	r.history = append(r.history, event)
	r.historyBytes += size
}

func (s *Service) PollWebSocket(ctx context.Context, id string, afterSequence int) (domain.WebSocketBatch, error) {
	if err := ctx.Err(); err != nil {
		return domain.WebSocketBatch{}, err
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	r := s.initWebSocket().records[id]
	if r == nil {
		return domain.WebSocketBatch{}, errors.New("WebSocket session is no longer available")
	}
	if afterSequence < 0 || afterSequence > r.delivered {
		return domain.WebSocketBatch{}, errors.New("invalid WebSocket event cursor")
	}
	r.result.DroppedEvents += int(r.droppedControls.Swap(0))
	batch := domain.WebSocketBatch{Execution: cloneWebSocketExecution(r.result), Events: []domain.WebSocketEvent{}, NextSequence: max(afterSequence, r.acknowledged)}
	bytes := 0
	for _, event := range r.queue {
		if event.Sequence <= afterSequence {
			continue
		}
		size := webSocketEventSize(event)
		if len(batch.Events) >= 32 || (len(batch.Events) > 0 && bytes+size > 1<<20) {
			break
		}
		batch.Events = append(batch.Events, cloneWebSocketEvent(event))
		bytes += size
		batch.NextSequence = event.Sequence
	}
	r.delivered = max(r.delivered, batch.NextSequence)
	return batch, nil
}

func (s *Service) AckWebSocket(ctx context.Context, id string, sequence int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	r := s.initWebSocket().records[id]
	if r == nil {
		return errors.New("WebSocket session is no longer available")
	}
	if sequence < 0 || sequence > r.delivered {
		return errors.New("cannot acknowledge an undelivered WebSocket event")
	}
	if sequence <= r.acknowledged {
		return nil
	}
	n := 0
	for n < len(r.queue) && r.queue[n].Sequence <= sequence {
		r.queueBytes -= webSocketEventSize(r.queue[n])
		r.queue[n] = domain.WebSocketEvent{}
		n++
	}
	r.queue = r.queue[n:]
	r.acknowledged = sequence
	return nil
}

func (s *Service) beginWebSocketCommand(ctx context.Context, id string) (*webSocketRecord, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	r := s.initWebSocket().records[id]
	if r == nil {
		return nil, nil, errors.New("WebSocket session is no longer available")
	}
	if r.result.State != "open" || r.closing || r.ctx.Err() != nil {
		return nil, nil, errors.New("WebSocket connection is not open")
	}
	select {
	case r.sendBusy <- struct{}{}:
	default:
		return nil, nil, errors.New("another WebSocket write or ping is in progress")
	}
	r.commands.Add(1)
	return r, func() { <-r.sendBusy; r.commands.Done() }, nil
}

func (s *Service) SendWebSocketMessage(ctx context.Context, id string, input domain.WebSocketSend) error {
	r, done, err := s.beginWebSocketCommand(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	work, cancel := context.WithTimeout(ctx, r.prepared.writeTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	r.securityMu.Lock()
	kind, data, err := s.webSocketSendData(work, r.prepared, input)
	secrets := slices.Clone(r.prepared.secrets)
	s.wsMu.Lock()
	if len(secrets) != len(r.secrets) {
		s.resanitizeWebSocket(r, secrets)
	}
	s.wsMu.Unlock()
	r.securityMu.Unlock()
	if err != nil {
		return errors.New(webSocketText(err.Error(), secrets, 4096))
	}
	if err = r.client.Write(work, kind, data); err != nil {
		return errors.New(webSocketText(webSocketError(err), secrets, 4096))
	}
	s.wsMu.Lock()
	r.result.SentCount++
	r.result.SentBytes += int64(len(data))
	s.appendWebSocketEvent(r, webSocketPreview("sent", "out", kind, data, r.secrets))
	s.wsMu.Unlock()
	return nil
}

func (s *Service) PingWebSocket(ctx context.Context, id string) error {
	r, done, err := s.beginWebSocketCommand(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	s.wsMu.Lock()
	if time.Since(r.lastPing) < time.Second {
		s.wsMu.Unlock()
		return errors.New("WebSocket ping is limited to once per second")
	}
	r.lastPing = time.Now()
	s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: "ping", Direction: "out"})
	s.wsMu.Unlock()
	work, cancel := context.WithTimeout(ctx, r.prepared.writeTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := r.client.Ping(work); err != nil {
		return errors.New("WebSocket ping failed or timed out")
	}
	return nil
}

func (s *Service) DisconnectWebSocket(ctx context.Context, id string, code int, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := websocketclient.ValidateClose(code, reason); err != nil {
		return err
	}
	s.wsMu.Lock()
	r := s.initWebSocket().records[id]
	if r == nil {
		s.wsMu.Unlock()
		return errors.New("WebSocket session is no longer available")
	}
	if !webSocketActive(r.result.State) || r.closing {
		s.wsMu.Unlock()
		return nil
	}
	if r.result.State == "connecting" {
		s.wsMu.Unlock()
		return errors.New("WebSocket connection is still connecting; cancel it instead")
	}
	r.closing = true
	r.result.State = "closing"
	r.commands.Add(1)
	s.appendWebSocketEvent(r, domain.WebSocketEvent{Type: "close", Direction: "out", Text: webSocketText(reason, r.secrets, 123)})
	s.wsMu.Unlock()
	go func() { defer r.commands.Done(); _ = r.client.Close(code, reason) }()
	return nil
}

func (s *Service) CancelWebSocket(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.wsMu.Lock()
	r := s.initWebSocket().records[id]
	if r == nil {
		s.wsMu.Unlock()
		return errors.New("WebSocket session is no longer available")
	}
	if !webSocketActive(r.result.State) {
		s.wsMu.Unlock()
		return nil
	}
	r.canceled = true
	r.closing = true
	r.cancel()
	client := r.client
	s.wsMu.Unlock()
	if client != nil {
		_ = client.Abort()
	}
	return nil
}

func (s *Service) ForgetWebSocket(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	state := s.initWebSocket()
	r := state.records[id]
	if r == nil {
		return nil
	}
	if webSocketActive(r.result.State) {
		return errors.New("disconnect or cancel the WebSocket before clearing it")
	}
	state.reserved -= r.reserve
	delete(state.records, id)
	return nil
}

func ShutdownWebSocket(s *Service) error {
	if s == nil {
		return nil
	}
	s.wsMu.Lock()
	state := s.initWebSocket()
	if state.closed {
		s.wsMu.Unlock()
		state.work.Wait()
		s.clearWebSocketState(state)
		return nil
	}
	state.closed = true
	var closes sync.WaitGroup
	for _, r := range state.records {
		if !webSocketActive(r.result.State) {
			continue
		}
		r.closing = true
		if r.client != nil {
			r.result.State = "closing"
			r.commands.Add(1)
			closes.Add(1)
			go func() { defer closes.Done(); defer r.commands.Done(); _ = r.client.Close(1001, "Application shutdown") }()
		} else {
			r.canceled = true
			r.cancel()
		}
	}
	s.wsMu.Unlock()
	closes.Wait()
	s.wsMu.Lock()
	clients := []*websocketclient.Client{}
	for _, r := range state.records {
		r.cancel()
		if r.client != nil {
			clients = append(clients, r.client)
		}
	}
	s.wsMu.Unlock()
	for _, client := range clients {
		_ = client.Abort()
	}
	state.work.Wait()
	s.clearWebSocketState(state)
	return nil
}

// Workers and their commands must finish before transient previews and secret
// snapshots are released. Concurrent shutdown callers can safely clear twice.
func (s *Service) clearWebSocketState(state *webSocketState) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	clear(state.records)
	state.reserved = 0
}

func webSocketText(value string, secrets []string, limit int) string {
	// A pending event can be sanitized again after a new keyring variable is
	// first used. Existing markers stay intact even for single-character secrets.
	var out strings.Builder
	for {
		part, rest, found := strings.Cut(value, security.RedactedValue)
		room := limit - out.Len()
		if room <= 0 {
			break
		}
		out.WriteString(redactText(part, secrets, room))
		if !found || out.Len() >= limit {
			break
		}
		room = limit - out.Len()
		marker := security.RedactedValue
		if len(marker) > room {
			marker = marker[:room]
		}
		out.WriteString(marker)
		value = rest
	}
	text := out.String()
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return text
}

func webSocketPreview(eventType, direction, kind string, data []byte, secrets []string) domain.WebSocketEvent {
	e := domain.WebSocketEvent{Type: eventType, Direction: direction, MessageType: kind, Bytes: len(data), Warnings: []string{}}
	if kind == "text" {
		e.Text = webSocketText(string(data), secrets, webSocketPreviewBytes)
	}
	if grpcContainsSecret(data, secrets) {
		e.Warnings = append(e.Warnings, "Binary preview was omitted because it contains a sensitive value")
	} else {
		preview := data
		if len(preview) > webSocketPreviewBytes {
			preview = preview[:webSocketPreviewBytes]
		}
		e.DataBase64 = base64.StdEncoding.EncodeToString(preview)
	}
	if len(data) > webSocketPreviewBytes {
		e.Truncated = true
		e.Warnings = append(e.Warnings, "Message preview was limited to 64 KiB")
	}
	return e
}

func (s *Service) resanitizeWebSocket(r *webSocketRecord, secrets []string) {
	r.secrets = slices.Clone(secrets)
	r.result.URL = webSocketText(r.result.URL, secrets, 65536)
	r.result.Subprotocol = webSocketText(r.result.Subprotocol, secrets, 256)
	r.result.CloseReason = webSocketText(r.result.CloseReason, secrets, 123)
	r.result.Error = webSocketText(r.result.Error, secrets, 4096)
	for i := range r.result.HandshakeHeaders {
		r.result.HandshakeHeaders[i].Name = webSocketText(r.result.HandshakeHeaders[i].Name, secrets, 4096)
		r.result.HandshakeHeaders[i].Value = webSocketText(r.result.HandshakeHeaders[i].Value, secrets, 8192)
	}
	if r.result.TLS != nil {
		t := r.result.TLS
		t.ServerName = webSocketText(t.ServerName, secrets, 1024)
		t.Subject = webSocketText(t.Subject, secrets, 2048)
		t.Issuer = webSocketText(t.Issuer, secrets, 2048)
		for i := range t.DNSNames {
			t.DNSNames[i] = webSocketText(t.DNSNames[i], secrets, 1024)
		}
	}
	for i := range r.queue {
		r.queueBytes -= webSocketEventSize(r.queue[i])
		r.queue[i] = sanitizeWebSocketEvent(r.queue[i], secrets)
		r.queueBytes += webSocketEventSize(r.queue[i])
	}
	for i := range r.history {
		r.historyBytes -= webSocketEventSize(r.history[i])
		r.history[i] = sanitizeWebSocketEvent(r.history[i], secrets)
		r.historyBytes += webSocketEventSize(r.history[i])
	}
	for len(r.queue) > 0 && r.queueBytes > webSocketQueueBytes {
		r.queueBytes -= webSocketEventSize(r.queue[0])
		r.queue[0] = domain.WebSocketEvent{}
		r.queue = r.queue[1:]
		r.result.DroppedEvents++
	}
	for len(r.history) > 0 && r.historyBytes > webSocketHistoryBytes {
		r.historyBytes -= webSocketEventSize(r.history[0])
		r.history[0] = domain.WebSocketEvent{}
		r.history = r.history[1:]
		r.historyOmitted++
	}
}

func sanitizeWebSocketEvent(e domain.WebSocketEvent, secrets []string) domain.WebSocketEvent {
	e.Text = webSocketText(e.Text, secrets, webSocketPreviewBytes)
	e.Warnings = slices.Clone(e.Warnings)
	if data, err := base64.StdEncoding.DecodeString(e.DataBase64); err != nil || grpcContainsSecret(data, secrets) {
		if e.DataBase64 != "" {
			e.Warnings = append(e.Warnings, "Binary preview was omitted after sensitive values were redacted")
		}
		e.DataBase64 = ""
	}
	return e
}

func webSocketHeaders(headers http.Header, secrets []string) []domain.Header {
	// Header names/values share the gRPC sensitive-name matcher (including
	// signature, credential and passwd); handshake protocol headers remain visible.
	keys := make([]string, 0, len(headers))
	for name := range headers {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	out := []domain.Header{}
	budget := 64 << 10
	for _, name := range keys {
		for _, raw := range headers[name] {
			value := security.RedactedValue
			if !sensitiveGRPCMetadata(name) {
				value = webSocketText(raw, secrets, 8192)
			}
			key := webSocketText(name, secrets, 4096)
			budget -= len(key) + len(value)
			if budget < 0 {
				return out
			}
			out = append(out, domain.Header{Name: key, Value: value, Enabled: true})
		}
	}
	return out
}

func webSocketError(err error) string {
	var preparation webSocketPreparationError
	if errors.As(err, &preparation) {
		return preparation.Error()
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "message limit") || strings.Contains(text, "read limited") || strings.Contains(text, "message too big") {
		return "WebSocket message exceeds the configured limit"
	}
	if strings.Contains(text, "invalid utf-8") || strings.Contains(text, "invalid utf8") {
		return "WebSocket peer sent an invalid UTF-8 text message"
	}
	return strings.ReplaceAll(strings.ReplaceAll(safeHTTPError(err).Error(), "HTTP(S)", "WS(S)"), "HTTP", "WebSocket")
}

type webSocketPreparationError struct{ error }
