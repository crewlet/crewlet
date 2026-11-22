package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// quickPacing is the helper's loop on pauses of a millisecond, and a poll
// bound no case reaches: a poll held past the case's end is a hang, and
// `go test -timeout` the failure.
var quickPacing = pacing{
	backoffFirst: time.Millisecond, backoffMost: time.Millisecond,
	pollFloor: time.Millisecond, pollBound: time.Hour,
	waitFirst: time.Millisecond, waitMost: time.Millisecond,
	dialBudget: time.Hour, callBound: time.Hour,
}

// record is a list a fake appends to and a case waits on, with no clock.
type record[T any] struct {
	mu      sync.Mutex
	items   []T
	changed chan struct{}
}

func (r *record[T]) add(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, v)
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
}

func (r *record[T]) all() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]T(nil), r.items...)
}

// await blocks until holds is true of the record, and answers it.
func (r *record[T]) await(holds func([]T) bool) []T {
	for {
		r.mu.Lock()
		items := append([]T(nil), r.items...)
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		changed := r.changed
		r.mu.Unlock()
		if holds(items) {
			return items
		}
		<-changed
	}
}

// poll is one poll the fake bridge answered.
type poll struct {
	token string
	after int
}

// fakeBridge serves the notes route of one job under token: the notes it
// holds past a poll's cursor, held until it holds one, and the status its
// script names for a poll (0 serves the notes; -1 drops the connection).
// polls records each poll as it arrives.
type fakeBridge struct {
	srv    *httptest.Server
	token  string
	script []int
	polls  record[poll]
	verbs  record[string]

	mu      sync.Mutex
	notes   []note
	served  int
	changed chan struct{}
}

func newFakeBridge(t *testing.T, token string, script ...int) *fakeBridge {
	t.Helper()
	b := &fakeBridge{token: token, script: script, changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/{token}/steer/{verb}", b.serve)
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// route is the job's notes route, token and all.
func (b *fakeBridge) route() string { return b.srv.URL + "/mcp/" + b.token + "/steer" }

func (b *fakeBridge) add(n note) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes = append(b.notes, n)
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *fakeBridge) serve(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")
	b.verbs.add(r.Method + " " + verb)
	if verb != "notes" || r.PathValue("token") != b.token {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	b.polls.add(poll{token: r.PathValue("token"), after: after})
	b.mu.Lock()
	n := b.served
	b.served++
	b.mu.Unlock()
	if n < len(b.script) && b.script[n] != 0 {
		if b.script[n] < 0 {
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(b.script[n])
		return
	}
	for {
		b.mu.Lock()
		var past []note
		for _, n := range b.notes {
			if n.Seq > after {
				past = append(past, n)
			}
		}
		changed := b.changed
		b.mu.Unlock()
		if len(past) > 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"notes": past})
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

// steer is one turn/steer the fake app-server was sent.
type steer struct {
	ThreadID       string `json:"threadId"`
	ExpectedTurnID string `json:"expectedTurnId"`
	Input          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"input"`
}

// fakeAppServer is a Codex app-server on a unix socket, as far as the adapter
// speaks to it: initialize, thread/resume answering the turn in flight named
// at its start ("" for none), and turn/steer, refused while refusals remain.
type fakeAppServer struct {
	path    string
	turn    string
	steers  record[steer]
	resumed record[string]
	fences  record[int]
	conns   record[*websocket.Conn]

	mu       sync.Mutex
	refusals int
}

func newFakeAppServer(t *testing.T, turn string, refusals int) *fakeAppServer {
	t.Helper()
	// t.TempDir cuts the case's name to 64 bytes, which keeps the socket's
	// path inside the 108 bytes Linux allows one.
	s := &fakeAppServer{path: filepath.Join(t.TempDir(), "s"), turn: turn, refusals: refusals}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: websocket.Server{Handler: s.serve}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		for _, c := range s.conns.all() {
			_ = c.Close()
		}
	})
	return s
}

func (s *fakeAppServer) serve(ws *websocket.Conn) {
	s.conns.add(ws)
	dec := json.NewDecoder(ws)
	for {
		var m wire
		if dec.Decode(&m) != nil {
			return
		}
		answer := map[string]any{"id": m.ID, "result": map[string]any{}}
		switch m.Method {
		case "initialized":
			continue
		case "thread/resume":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			var turns []map[string]string
			if s.turn != "" {
				turns = append(turns, map[string]string{"id": "turn-before", "status": "completed"},
					map[string]string{"id": s.turn, "status": "inProgress"})
			}
			answer["result"] = map[string]any{"thread": map[string]any{"id": p.ThreadID, "turns": turns}}
			s.resumed.add(p.ThreadID)
		case "turn/steer":
			var p steer
			_ = json.Unmarshal(m.Params, &p)
			s.steers.add(p)
			s.mu.Lock()
			refuse := s.refusals > 0
			s.refusals--
			s.mu.Unlock()
			if refuse {
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32602, "message": "refused by the fake"}
			} else {
				answer["result"] = map[string]string{"turnId": p.ExpectedTurnID}
			}
		case "fence":
			s.fences.add(len(s.fences.all()))
		}
		data, _ := json.Marshal(answer)
		if websocket.Message.Send(ws, string(data)) != nil {
			return
		}
	}
}

// notify sends a notification to every client.
func (s *fakeAppServer) notify(t *testing.T, method string, params map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"method": method, "params": params})
	for _, c := range s.conns.all() {
		if err := websocket.Message.Send(c, string(data)); err != nil {
			t.Fatal(err)
		}
	}
}

// hangUp closes every client's connection, as an app-server whose run ended.
func (s *fakeAppServer) hangUp() {
	for _, c := range s.conns.all() {
		_ = c.Close()
	}
}

// writeThread writes the thread file as the run's script does: by a rename.
func writeThread(t *testing.T, path, thread string) {
	t.Helper()
	if err := os.WriteFile(path+".tmp", []byte(thread+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// lockedBuffer is the helper's stderr, read while it writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// helperRun is one run of the helper in this process.
type helperRun struct {
	stderr lockedBuffer
	cancel context.CancelFunc
	code   chan int
}

// startHelper runs the helper on the codex-app-server adapter against
// bridge and app, the route in NOTES_ROUTE, until stop.
func startHelper(t *testing.T, bridge *fakeBridge, app *fakeAppServer, threadFile string) *helperRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &helperRun{cancel: cancel, code: make(chan int, 1)}
	args := []string{"-adapter", "codex-app-server", "-socket", app.path,
		"-thread-file", threadFile, "-notes-env", "NOTES_ROUTE"}
	env := func(name string) string {
		if name == "NOTES_ROUTE" {
			return bridge.route()
		}
		return ""
	}
	go func() { h.code <- run(ctx, args, env, &h.stderr, quickPacing) }()
	t.Cleanup(func() {
		cancel()
		<-h.code
	})
	return h
}

// stop ends the run as a signal does, and answers its exit status.
func (h *helperRun) stop() int {
	h.cancel()
	code := <-h.code
	h.code <- code
	return code
}
