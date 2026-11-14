package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

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

// pair is a connection and its scripted peer, over one transport.
type pair func(t testing.TB, log *slog.Logger) (*Conn, *peer)

// transports is every transport a connection runs over, for a case that holds
// both to one claim.
var transports = []struct {
	name string
	pair pair
}{
	{"pipe", pipePair},
	{"socket", socketPair},
}

// pipePair is a connection over a pair of in-process pipes and the peer on
// their other ends, framed as Chromium's --remote-debugging-pipe frames.
func pipePair(t testing.TB, log *slog.Logger) (*Conn, *peer) {
	clientReads, peerWrites := io.Pipe()
	peerReads, clientWrites := io.Pipe()
	c := NewPipe(clientReads, clientWrites, Options{Logger: log})
	in := newFrames(peerReads)
	p := &peer{
		t: t,
		read: func() ([]byte, error) {
			m, err := in.next()
			return m.body, err
		},
		write: func(b []byte) error {
			framed := append(bytes.Clone(b), 0)
			_, err := peerWrites.Write(framed)
			return err
		},
		stop: func() {
			_ = peerWrites.Close()
			_ = peerReads.Close()
		},
	}
	t.Cleanup(func() {
		_ = c.Close()
		p.stop()
	})
	return c, p
}

// socketPair is a connection dialled to a websocket an httptest server
// accepts, and the peer on the server's end.
func socketPair(t testing.TB, log *slog.Logger) (*Conn, *peer) {
	t.Helper()
	return socketPairDialled(t.Context(), t, log)
}

// socketPairDialled is socketPair, its connection dialled under ctx.
func socketPairDialled(ctx context.Context, t testing.TB, log *slog.Logger) (*Conn, *peer) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		accepted <- ws
	}))
	t.Cleanup(srv.Close)
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), DialOptions{HTTPClient: srv.Client(), Logger: log})
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
