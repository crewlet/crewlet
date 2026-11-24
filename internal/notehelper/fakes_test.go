package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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
	// skills records each report to the skill route, answered with
	// skillStatus (0: 204).
	skills      record[skillReport]
	skillStatus atomic.Int32

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
	mux.HandleFunc("/mcp/{token}/skill", b.serveSkill)
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// skillReport is one report the fake skill route was sent.
type skillReport struct {
	token, method, skill string
}

func (b *fakeBridge) serveSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Skill string `json:"skill"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.skills.add(skillReport{token: r.PathValue("token"), method: r.Method, skill: body.Skill})
	if status := int(b.skillStatus.Load()); status != 0 {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// skillRoute is the job's skill route, token and all.
func (b *fakeBridge) skillRoute() string { return b.srv.URL + "/mcp/" + b.token + "/skill" }

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

// resume is one thread/resume the fake app-server was sent.
type resume struct {
	ThreadID     string `json:"threadId"`
	ExcludeTurns bool   `json:"excludeTurns"`
}

// fakeAppServer is a Codex app-server on a unix socket, as far as the adapter
// speaks to it, answering as 0.171.0 was measured to: initialize;
// thread/resume, refused while join refusals remain, as a thread just started
// refuses one; thread/turns/list, newest first, the turn in flight ("" for
// none) first while there is one; and turn/steer, refused for no turn in
// flight or another turn than the one named, and while refusals remain.
type fakeAppServer struct {
	path       string
	steers     record[steer]
	resumed    record[resume]
	fences     record[int]
	conns      record[*websocket.Conn]
	interrupts record[interruptCall]
	// answeredLists records each thread/turns/list once its answer is on the
	// wire: a client that took it in order has the turn it names in flight
	// before any notice sent after.
	answeredLists record[int]

	mu       sync.Mutex
	turn     string
	refusals int
	// listed, when set, is the turn thread/turns/list says is in progress,
	// whatever runs: a list answered just before its turn ended.
	listed string
	// marker is the cap marker each interrupt looks for as it arrives.
	marker string
	// interruptFails answers every interrupt with an internal error.
	interruptFails bool
	// joinRefusals is how many thread/resumes are refused before one joins.
	joinRefusals int
}

// refuseJoins refuses the next n thread/resumes.
func (s *fakeAppServer) refuseJoins(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joinRefusals = n
}

// failInterrupts makes every interrupt fail, the turn left running.
func (s *fakeAppServer) failInterrupts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptFails = true
}

// set sets the fake's listed turn and the marker it looks for.
func (s *fakeAppServer) set(listed, marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed, s.marker = listed, marker
}

// interruptCall is one turn/interrupt the fake app-server was sent.
type interruptCall struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	// marked is whether the cap marker was on disk as it arrived.
	marked bool
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
			var p resume
			_ = json.Unmarshal(m.Params, &p)
			answer["result"] = map[string]any{"thread": map[string]any{"id": p.ThreadID, "turns": []any{},
				"status": map[string]any{"type": "active"}}}
			s.mu.Lock()
			if s.joinRefusals > 0 {
				s.joinRefusals--
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32601, "message": "list_turns is not supported yet"}
			}
			s.mu.Unlock()
			s.resumed.add(p)
		case "thread/turns/list":
			s.mu.Lock()
			turns := []map[string]string{{"id": "turn-before", "status": "completed"}}
			if newest := cmp.Or(s.listed, s.turn); newest != "" {
				turns = append([]map[string]string{{"id": newest, "status": "inProgress"}}, turns...)
			}
			s.mu.Unlock()
			answer["result"] = map[string]any{"data": turns}
		case "turn/steer":
			var p steer
			_ = json.Unmarshal(m.Params, &p)
			s.steers.add(p)
			s.mu.Lock()
			turn, refuse := s.turn, s.refusals > 0
			s.refusals--
			s.mu.Unlock()
			var refusal map[string]any
			switch {
			case turn == "":
				refusal = map[string]any{"code": -32600, "message": "no active turn to steer"}
			case p.ExpectedTurnID != turn:
				refusal = map[string]any{"code": -32600,
					"message": "expected active turn id `" + p.ExpectedTurnID + "` but found `" + turn + "`"}
			case refuse:
				refusal = map[string]any{"code": -32602, "message": "refused by the fake"}
			}
			if refusal != nil {
				delete(answer, "result")
				answer["error"] = refusal
			} else {
				answer["result"] = map[string]string{"turnId": turn}
			}
		case "turn/interrupt":
			var p interruptCall
			_ = json.Unmarshal(m.Params, &p)
			s.mu.Lock()
			marker, fails := s.marker, s.interruptFails
			s.mu.Unlock()
			if marker != "" {
				_, err := os.Stat(marker)
				p.marked = err == nil
			}
			s.interrupts.add(p)
			s.mu.Lock()
			turn := s.turn
			if turn != "" && p.TurnID == turn && !fails {
				s.turn = ""
			}
			s.mu.Unlock()
			switch {
			case fails:
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32603, "message": "internal error"}
			case p.TurnID == "":
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32600, "message": "Invalid request: missing field `turnId`"}
			case turn == "":
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32600, "message": "no active turn to interrupt"}
			case p.TurnID != turn:
				delete(answer, "result")
				answer["error"] = map[string]any{"code": -32600,
					"message": "expected active turn id " + p.TurnID + " but found " + turn}
			}
		case "fence":
			s.fences.add(len(s.fences.all()))
		}
		data, _ := json.Marshal(answer)
		if websocket.Message.Send(ws, string(data)) != nil {
			return
		}
		if m.Method == "thread/turns/list" {
			s.answeredLists.add(len(s.answeredLists.all()))
		}
	}
}

// joinedTurn blocks until a client's thread/turns/list was answered: the
// helper has joined, its turn in flight set from the answer before any
// notice after it reaches the helper ([appServer.callApplied]).
func (s *fakeAppServer) joinedTurn() {
	s.answeredLists.await(func(l []int) bool { return len(l) > 0 })
}

// setTurn changes the turn in flight and tells no client, as a turn that
// moved on before its notice reached the helper.
func (s *fakeAppServer) setTurn(turn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = turn
}

// startTurn starts turn and sends turn/started, in the measured shape.
func (s *fakeAppServer) startTurn(t *testing.T, thread, turn string) {
	t.Helper()
	s.setTurn(turn)
	s.notify(t, "turn/started", map[string]any{"threadId": thread, "turn": map[string]any{
		"id": turn, "rootTurnId": turn, "items": []any{}, "itemsView": "notLoaded", "status": "inProgress"}})
}

// completeTurn ends turn and sends turn/completed.
func (s *fakeAppServer) completeTurn(t *testing.T, thread, turn string) {
	t.Helper()
	s.setTurn("")
	s.notify(t, "turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{
		"id": turn, "rootTurnId": turn, "items": []any{}, "status": "completed"}})
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

// digestFlags is the -skill-digest flags the runner hands the helper for
// skills as it installs them: one name=hex each.
func digestFlags(skills ...skillFile) []string {
	var out []string
	for _, s := range skills {
		sum := sha256.Sum256([]byte(s.body))
		out = append(out, "-skill-digest", filepath.Base(filepath.Dir(s.path))+"="+hex.EncodeToString(sum[:]))
	}
	return out
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
	// writes is every write's length, for a case awaiting a line.
	writes record[int]
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	b.writes.add(n)
	return n, err
}

// await blocks until holds is true of what was written, with no clock.
func (b *lockedBuffer) await(holds func(string) bool) {
	b.writes.await(func([]int) bool { return holds(b.String()) })
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
// bridge and app, the notes route in NOTES_ROUTE, until stop.
func startHelper(t *testing.T, bridge *fakeBridge, app *fakeAppServer, threadFile string) *helperRun {
	t.Helper()
	return startHelperWith(t, bridge, app, threadFile, "-notes-env", "NOTES_ROUTE")
}

// startHelperWith is startHelper with flags of its own beside the adapter's,
// the notes route in NOTES_ROUTE and the skill route in SKILLS_ROUTE.
func startHelperWith(t *testing.T, bridge *fakeBridge, app *fakeAppServer, threadFile string, flags ...string) *helperRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &helperRun{cancel: cancel, code: make(chan int, 1)}
	args := append([]string{"-adapter", "codex-app-server", "-socket", app.path,
		"-thread-file", threadFile}, flags...)
	env := func(name string) string {
		switch name {
		case "NOTES_ROUTE":
			return bridge.route()
		case "SKILLS_ROUTE":
			return bridge.skillRoute()
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
