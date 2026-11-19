package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// bounded is the case's context under a liveness bound: what a case waits on
// arrives at once when the claim holds, so the bound only turns a hang into a
// failure that says what never came.
func bounded(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// step is one wait's context under a liveness bound of its own, for a case
// whose volume makes its whole run long: the bound restarts with each wait, so
// a slow runner that keeps delivering passes, and a wait nothing ends fails.
func step(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), stepBound)
	t.Cleanup(cancel)
	return ctx
}

// stepBound is step's bound. A wait moves at most one ReadLimit's message,
// and a loaded race shard moved these cases' events at about a second a
// mebibyte, so a wait still open after two minutes is one that never ends.
const stepBound = 2 * time.Minute

// The commands and events the internal cases send, declared in a test file so
// they never reach the inventory.
var (
	testCall  = Method[testParams, testResult]{name: "Test.call"}
	testEvent = Event[testEventParams]{name: "Test.event"}
	testOther = Event[testEventParams]{name: "Test.other"}
)

type testParams struct {
	Text string `json:"text,omitempty"`
}

type testResult struct {
	Text string `json:"text"`
}

type testEventParams struct {
	N    int    `json:"n"`
	Text string `json:"text,omitempty"`
}

// peer is the browser's end of a connection a case scripts: it reads the
// commands the client sends and writes what a browser would.
type peer struct {
	t     testing.TB
	read  func() ([]byte, error)
	write func([]byte) error
	// stop ends the peer's end of the transport, as a browser that went
	// away does.
	stop func()
}

// request is one command as the peer reads it.
type request struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID SessionID       `json:"sessionId"`
}

// recv is the next command the client sent; usable on any goroutine.
func (p *peer) recv() (request, error) {
	b, err := p.read()
	if err != nil {
		return request{}, err
	}
	var r request
	if err := json.Unmarshal(b, &r); err != nil {
		return request{}, err
	}
	return r, nil
}

// next is the next command the client sent; on the test's own goroutine only.
func (p *peer) next() request {
	p.t.Helper()
	r, err := p.recv()
	if err != nil {
		p.t.Fatalf("the peer read no command: %v", err)
	}
	return r
}

// send writes one message.
func (p *peer) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.write(b)
}

// answer answers r with result, on r's session.
func (p *peer) answer(r request, result any) error {
	return p.send(map[string]any{"id": r.ID, "result": result, "sessionId": r.SessionID})
}

// refuse answers r with Chromium's error, its data included.
func (p *peer) refuse(r request, code int64, message string, data any) error {
	return p.send(map[string]any{
		"id": r.ID, "sessionId": r.SessionID,
		"error": map[string]any{"code": code, "message": message, "data": data},
	})
}

// event sends an event on session (empty: the browser's).
func (p *peer) event(session SessionID, method string, params any) error {
	m := map[string]any{"method": method, "params": params}
	if session != "" {
		m["sessionId"] = session
	}
	return p.send(m)
}

// attach announces a child session of target on the browser session, as
// Chromium's Target.attachedToTarget does for a target already running.
func (p *peer) attach(session SessionID, target TargetID, kind string) error {
	return p.announceChild(session, target, kind, false)
}

// attachHeld announces one Chromium holds, as it announces a target it starts
// once auto-attach is on.
func (p *peer) attachHeld(session SessionID, target TargetID, kind string) error {
	return p.announceChild(session, target, kind, true)
}

// announceChild is Target.attachedToTarget for a child session of target,
// held or not.
func (p *peer) announceChild(session SessionID, target TargetID, kind string, held bool) error {
	return p.event("", TargetAttachedToTarget.name, map[string]any{
		"sessionId":          session,
		"targetInfo":         map[string]any{"targetId": target, "type": kind, "url": "about:blank"},
		"waitingForDebugger": held,
	})
}

// attachment is one session a peer announces, and the target it is attached
// to.
type attachment struct {
	session SessionID
	target  TargetID
}

// announce attaches each session to its target as Chromium does, on the
// browser session, and hands the sessions back once the connection has read
// every announcement: a session is the connection's from its announcement on,
// and an id looked up before it is one that has ended.
func announce(ctx context.Context, t testing.TB, c *Conn, p *peer, as ...attachment) []*Session {
	t.Helper()
	l := c.Browser().Listen(TargetAttachedToTarget)
	defer l.Close()
	for _, a := range as {
		if err := p.attach(a.session, a.target, "page"); err != nil {
			t.Fatal(err)
		}
	}
	for range as {
		if _, err := l.Next(ctx); err != nil {
			t.Fatalf("an announcement never reached the connection: %v", err)
		}
	}
	out := make([]*Session, 0, len(as))
	for _, a := range as {
		s := c.Session(a.session)
		if err := s.Err(); err != nil {
			t.Fatalf("session %s, announced, has ended: %v", a.session, err)
		}
		out = append(out, s)
	}
	return out
}

// socketPair is a connection dialled to a websocket served in memory, and
// the peer on the server's end. Every byte crosses net.Pipe, whose blocking a
// synctest bubble counts as durable, so one pair serves a case in a bubble and
// out of one, and a peer that reads nothing holds the client's next write.
func socketPair(t testing.TB, log *slog.Logger) (*Conn, *peer) {
	t.Helper()
	return socketPairDialled(t.Context(), t, log)
}

// socketPairDialled is socketPair, its connection dialled under ctx.
func socketPairDialled(ctx context.Context, t testing.TB, log *slog.Logger) (*Conn, *peer) {
	t.Helper()
	l := &memListener{conns: make(chan net.Conn), done: make(chan struct{})}
	accepted := make(chan *websocket.Conn, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		accepted <- ws
	})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-served
	})
	client := &http.Client{Transport: &http.Transport{DialContext: l.dial}}
	c, err := Dial(ctx, "ws://browser.test/devtools/browser/case", DialOptions{HTTPClient: client, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ws := <-accepted
	// The peer's end lives as long as the case, whatever ends the dial's
	// context: only its stop, or the connection's end, ends it.
	live := context.WithoutCancel(ctx)
	p := &peer{
		t: t,
		read: func() ([]byte, error) {
			_, b, err := ws.Read(live)
			return b, err
		},
		write: func(b []byte) error { return ws.Write(live, websocket.MessageText, b) },
		stop:  func() { _ = ws.CloseNow() },
	}
	t.Cleanup(func() {
		_ = c.Close()
		p.stop()
	})
	return c, p
}

// memListener is a listener whose connections are net.Pipe's far ends, each
// handed over as dial makes it.
type memListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr{} }

// dial is a new connection's near end, once Accept has taken its far end.
func (l *memListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	near, far := net.Pipe()
	select {
	case l.conns <- far:
		return near, nil
	case <-l.done:
	case <-ctx.Done():
	}
	_, _ = near.Close(), far.Close()
	return nil, net.ErrClosed
}

type memAddr struct{}

func (memAddr) Network() string { return "memory" }
func (memAddr) String() string  { return "memory" }

// records is a log handler that keeps every record a connection logs, at
// every level, and signals each one.
type records struct {
	mu    sync.Mutex
	lines []string
	wake  chan struct{}
}

func newRecords() *records { return &records{wake: make(chan struct{}, 1)} }

func (r *records) logger() *slog.Logger { return slog.New(r) }

func (r *records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String() + " " + rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, b.String())
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *records) WithGroup(string) slog.Handler { return r }

// text is every record kept so far, one a line.
func (r *records) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// await waits for a record whose message is msg, or for ctx.
func (r *records) await(ctx context.Context, msg string) error {
	for {
		if strings.Contains(r.text(), " "+msg) {
			return nil
		}
		select {
		case <-r.wake:
		case <-ctx.Done():
			return errors.New("no " + msg + " was logged: " + r.text())
		}
	}
}
