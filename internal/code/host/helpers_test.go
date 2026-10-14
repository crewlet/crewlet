package host_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// newCase is a solo fleet of o with RepoName declared under settings.
func newCase(t *testing.T, o codetest.SoloOptions, settings code.RepoSettings) *codetest.Case {
	t.Helper()
	f := codetest.SoloNode(t, o)
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings})
	return &codetest.Case{T: t, Fleet: f, Ctx: t.Context()}
}

// fast is a Timing a case can wait out: the production's shape, at a scale of
// tens of milliseconds — but for the stall, which ends a real git process
// that is merely busy (its keepalives are five seconds apart), so a case that
// means to stall one scales it alone.
var fast = host.Timing{PendingGrace: 500 * time.Millisecond, PendingFirst: 40 * time.Millisecond,
	PendingMax: 80 * time.Millisecond, Keepalive: 200 * time.Millisecond, Handshake: 5 * time.Second,
	Stall: host.Production.Stall}

// call is one call a recording estate saw.
type call struct {
	op    string
	level statelog.ReadLevel
	at    time.Time
}

// recording is a host's estate that records every call — and answers any a
// case overrides in its own way.
type recording struct {
	host.Estate
	propose func(ctx context.Context, inner host.Estate, p code.Proposal) (code.ProposeResult, error)
	outcome func(ctx context.Context, inner host.Estate, q code.OutcomeQuery) (code.PushOutcome, error)
	resolve func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error)
	state   func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error)

	mu    sync.Mutex
	calls []call
}

// wrap is a WrapEstate that records into e.
func (e *recording) wrap(inner host.Estate) host.Estate {
	e.Estate = inner
	return e
}

func (e *recording) note(op string, level statelog.ReadLevel) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call{op: op, level: level, at: time.Now()})
}

// seen is every call so far of op.
func (e *recording) seen(op string) []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []call
	for _, c := range e.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

// linearizable counts the RepoState calls so far read through the barrier.
func (e *recording) linearizable() int {
	n := 0
	for _, c := range e.seen("state") {
		if c.level == statelog.ReadLinearizable {
			n++
		}
	}
	return n
}

func (e *recording) Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	e.note("resolve", q.Fresh.Level)
	if e.resolve != nil {
		return e.resolve(ctx, e.Estate, q)
	}
	return e.Estate.Resolve(ctx, q)
}

func (e *recording) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	e.note("state", q.Fresh.Level)
	if e.state != nil {
		return e.state(ctx, e.Estate, q)
	}
	return e.Estate.RepoState(ctx, q)
}

func (e *recording) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	e.note("propose", "")
	if e.propose != nil {
		return e.propose(ctx, e.Estate, p)
	}
	return e.Estate.Propose(ctx, p)
}

func (e *recording) PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	e.note("outcome", "")
	if e.outcome != nil {
		return e.outcome(ctx, e.Estate, q)
	}
	return e.Estate.PushOutcome(ctx, q)
}

// logs is a logger a case reads back.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// logger is a JSON logger writing into l.
func (l *logs) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// count is how many lines carry msg and every one of attrs ("key":"value"
// JSON fragments).
func (l *logs) count(msg string, attrs ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.Lines(l.buf.String()) {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		all := true
		for _, a := range attrs {
			all = all && strings.Contains(line, a)
		}
		if all {
			n++
		}
	}
	return n
}

// get is a hand-built GET of RepoName's advertisement on node i, as who, under
// protocol ("" for none).
func get(t *testing.T, c *codetest.Case, i int, who, service, protocol string) (*http.Response, string) {
	t.Helper()
	n := c.Fleet.Nodes()[i]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodGet,
		n.URL()+"/git/"+codetest.RepoName+"/info/refs?service="+service, nil)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "" {
		req.Header.Set("Git-Protocol", protocol)
	}
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	resp, err := n.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", service, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to GET %s: %v", service, err)
	}
	return resp, string(raw)
}

// post is a hand-built POST of a git service's RPC to node i, as who, under
// the Git-Protocol protocol ("" for none) — a request a case needs that git
// never sends alone.
func post(t *testing.T, c *codetest.Case, i int, who, service, protocol, body string) (*http.Response, string) {
	t.Helper()
	n := c.Fleet.Nodes()[i]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost, n.URL()+"/git/"+codetest.RepoName+"/"+service,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-"+service+"-request")
	if protocol != "" {
		req.Header.Set("Git-Protocol", protocol)
	}
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	resp, err := n.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", service, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to POST %s: %v", service, err)
	}
	return resp, string(raw)
}

// raw is a POST of a git service's RPC to node 0, as who, under the
// Git-Protocol protocol ("" for none), SENT RAW: a Content-Length of declared
// over body as given — a request Go's client refuses to send, a length its
// body lacks — and the body ended by the client's half-close. A request that is
// not answered within ten seconds fails the case.
func raw(t *testing.T, c *codetest.Case, who, service, protocol string, declared int64,
	body string) (*http.Response, string) {

	t.Helper()
	addr := strings.TrimPrefix(c.Fleet.Nodes()[0].URL(), "http://")
	conn, err := (&net.Dialer{}).DialContext(c.Ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := "POST /git/" + codetest.RepoName + "/" + service + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("crewlet:"+c.Fleet.Token(t, who))) +
		"\r\nContent-Type: application/x-" + service + "-request\r\n" +
		"Content-Length: " + strconv.FormatInt(declared, 10) + "\r\n"
	if protocol != "" {
		head += "Git-Protocol: " + protocol + "\r\n"
	}
	if _, err := io.WriteString(conn, head+"\r\n"+body); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("a %s request declaring %d bytes and sending %d was not answered: %v", service, declared,
			len(body), err)
	}
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to a raw %s request: %v", service, err)
	}
	_ = resp.Body.Close()
	return resp, string(answer)
}

// pkt is s as one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushRows is every push row of RepoName.
func pushRows(t *testing.T, c *codetest.Case) []code.PushRow {
	t.Helper()
	a, err := c.Fleet.Nodes()[0].Read(c.Ctx, code.ReadQuery{Kind: code.ReadPushes, Repo: codetest.RepoName,
		Limit: 200, Fresh: codetest.Linearizable})
	if err != nil {
		t.Fatalf("read the pushes: %v", err)
	}
	return a.Pushes
}
