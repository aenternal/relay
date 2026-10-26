// wsfixture is a loopback-only WebSocket peer for desktop end-to-end tests.
// Stdout contains one endpoint JSON object. Statistics contain counters only;
// request credentials, URLs, headers and message payloads are never logged.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

type fixture struct {
	connections, active, received, text, binary atomic.Int64
	pings, pongs, closed, abrupt                atomic.Int64
	authorization, cookies, origins, queries    atomic.Int64
	repeatedHeaders                             atomic.Int64
	mu                                          sync.Mutex
	peers                                       map[*peer]struct{}
	closing                                     bool
	lifetime                                    context.Context
}

type peer struct {
	conn     *websocket.Conn
	raw      net.Conn
	pingBusy atomic.Bool
	pingWork sync.WaitGroup
}

// The pinned library's CloseNow waits when a graceful Close has already begun.
// Retaining the physical socket lets fixture teardown interrupt that wait.
type captureWriter struct {
	http.ResponseWriter
	raw net.Conn
}

func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		w.raw = conn
	}
	return conn, rw, err
}

func (p *peer) close(code websocket.StatusCode, reason string) error {
	timer := time.AfterFunc(250*time.Millisecond, func() { _ = p.raw.Close() })
	defer timer.Stop()
	return p.conn.Close(code, reason)
}

func (f *fixture) handler(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/stats":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"connections": f.connections.Load(), "active": f.active.Load(),
			"received": f.received.Load(), "text": f.text.Load(), "binary": f.binary.Load(),
			"pings": f.pings.Load(), "pongs": f.pongs.Load(),
			"closed": f.closed.Load(), "abrupt": f.abrupt.Load(),
			"authorization": f.authorization.Load(), "cookies": f.cookies.Load(),
			"origins": f.origins.Load(), "queries": f.queries.Load(),
			"repeatedHeaders": f.repeatedHeaders.Load(),
		})
	case "/reject":
		w.Header().Set("X-Fixture", "reject")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, strings.Repeat("rejected", 16)) // fixed 128 bytes
	case "/echo":
		f.echo(w, request)
	default:
		http.NotFound(w, request)
	}
}

func (f *fixture) echo(w http.ResponseWriter, request *http.Request) {
	w.Header().Add("X-Fixture", "first")
	w.Header().Add("X-Fixture", "second")
	captured := &captureWriter{ResponseWriter: w}
	conn, err := websocket.Accept(captured, request, &websocket.AcceptOptions{
		Subprotocols: []string{"echo.v1"},
		// This fixture deliberately accepts a desktop editor's custom Origin.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
		OnPingReceived: func(context.Context, []byte) bool {
			f.pings.Add(1)
			return true
		},
		OnPongReceived: func(context.Context, []byte) { f.pongs.Add(1) },
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(4 << 20)
	p := &peer{conn: conn, raw: captured.raw}
	f.mu.Lock()
	if f.closing {
		f.mu.Unlock()
		_ = conn.CloseNow()
		return
	}
	f.peers[p] = struct{}{}
	f.connections.Add(1)
	f.active.Add(1)
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(f.lifetime)
	defer func() {
		cancel()
		_ = p.raw.Close()
		_ = conn.CloseNow()
		p.pingWork.Wait()
		f.mu.Lock()
		delete(f.peers, p)
		f.active.Add(-1)
		f.mu.Unlock()
	}()
	if request.Header.Get("Authorization") != "" {
		f.authorization.Add(1)
	}
	if request.Header.Get("Cookie") != "" {
		f.cookies.Add(1)
	}
	if request.Header.Get("Origin") != "" {
		f.origins.Add(1)
	}
	if request.URL.RawQuery != "" {
		f.queries.Add(1)
	}
	if len(request.Header.Values("X-Repeat")) >= 2 {
		f.repeatedHeaders.Add(1)
	}
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			f.finish(err)
			return
		}
		f.received.Add(1)
		if kind == websocket.MessageText {
			f.text.Add(1)
			switch string(data) {
			case "__burst__":
				writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				for index := 1; index <= 10000; index++ {
					err = conn.Write(writeCtx, websocket.MessageText, []byte("event-"+strconv.Itoa(index)))
					if err != nil {
						break
					}
				}
				stop()
				if err != nil {
					f.finish(err)
					return
				}
				continue
			case "__close__":
				if err := p.close(websocket.StatusNormalClosure, "server done"); err == nil {
					f.closed.Add(1)
				} else {
					f.finish(err)
				}
				return
			case "__error__":
				// Only this intentional close reason reflects a credential, so E2E
				// can verify the app masks peer diagnostics before display/storage.
				reason := "fixture failure " + request.Header.Get("Authorization")
				if len(reason) > 123 {
					reason = reason[:123]
					for !utf8.ValidString(reason) {
						reason = reason[:len(reason)-1]
					}
				}
				_ = p.close(websocket.StatusInternalError, reason)
				return
			case "__drop__":
				f.abrupt.Add(1)
				_ = p.raw.Close()
				return
			case "__ping__":
				if p.pingBusy.CompareAndSwap(false, true) {
					p.pingWork.Add(1)
					go func() {
						defer p.pingWork.Done()
						defer p.pingBusy.Store(false)
						pingCtx, stop := context.WithTimeout(ctx, 2*time.Second)
						defer stop()
						_ = conn.Ping(pingCtx)
					}()
				}
				continue
			}
		} else {
			f.binary.Add(1)
		}
		writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		err = conn.Write(writeCtx, kind, data)
		stop()
		if err != nil {
			f.finish(err)
			return
		}
	}
}

func (f *fixture) finish(err error) {
	code := websocket.CloseStatus(err)
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		f.closed.Add(1)
	} else if code < 0 {
		f.abrupt.Add(1)
	}
}

func (f *fixture) shutdown() {
	f.mu.Lock()
	f.closing = true
	peers := make([]*peer, 0, len(f.peers))
	for p := range f.peers {
		peers = append(peers, p)
	}
	f.mu.Unlock()
	var work sync.WaitGroup
	for _, p := range peers {
		work.Add(1)
		go func() { defer work.Done(); _ = p.close(websocket.StatusGoingAway, "Fixture shutdown") }()
	}
	work.Wait()
}

func run() error {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fixture{peers: map[*peer]struct{}{}, lifetime: lifetime}
	plain := httptest.NewUnstartedServer(http.HandlerFunc(f.handler))
	secure := httptest.NewUnstartedServer(http.HandlerFunc(f.handler))
	plain.Config.ErrorLog = log.New(io.Discard, "", 0)
	secure.Config.ErrorLog = log.New(io.Discard, "", 0)
	plain.Start()
	secure.StartTLS()
	defer plain.Close()
	defer secure.Close()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"ws":     "ws" + strings.TrimPrefix(plain.URL, "http") + "/echo",
		"wss":    "ws" + strings.TrimPrefix(secure.URL, "http") + "/echo",
		"reject": "ws" + strings.TrimPrefix(plain.URL, "http") + "/reject",
		"stats":  plain.URL + "/stats",
	}); err != nil {
		return err
	}
	<-signals.Done()
	f.shutdown()
	cancel()
	return nil
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "WebSocket fixture failed")
		os.Exit(1)
	}
}
