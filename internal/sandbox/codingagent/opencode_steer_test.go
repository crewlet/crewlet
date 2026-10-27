package codingagent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// probeScript drives the plugin under node: opencode_steer_probe.mjs, beside
// the plugin it drives.
const probeScript = "opencode_steer_probe.mjs"

// nodeForProbe is the node the plugin is driven under, by internal/e2e's
// policy for its client replay: where CI is set a missing node FAILS the case,
// because a skip would pass the build with the plugin never run; elsewhere it
// skips, for a fast loop on a machine without one. The skip is declared
// nowhere in internal/skipgate, so the make targets that gate a change refuse
// to start without node (require-node) rather than report it.
func nodeForProbe(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	switch strings.ToLower(os.Getenv("CI")) {
	case "1", "true", "yes":
		t.Fatalf("node is not on PATH, so the OpenCode notes plugin would go unrun and "+
			"the build would still pass: %v", err)
	}
	t.Skip("node is not installed; the OpenCode notes plugin's probe needs it")
	return ""
}

// notesCall is one request the plugin made of the notes route, held until the
// case answers it or the plugin lets go of it.
type notesCall struct {
	// verb is the method and the route's last segment: `GET notes` or
	// `POST read`.
	verb  string
	after string
	body  string
	at    time.Time
	// gone is closed once the plugin has let go of the request.
	gone   <-chan struct{}
	answer chan<- notesAnswer
}

type notesAnswer struct {
	status int
	body   string
}

// reply answers the call.
func (c *notesCall) reply(status int, body string) { c.answer <- notesAnswer{status, body} }

// notesRoute is a stand-in of the bridge's notes route (internal/api/mcpbridge,
// SteerPattern): every request is handed to the case, which answers it.
type notesRoute struct {
	t     *testing.T
	srv   *httptest.Server
	calls chan *notesCall
}

func newNotesRoute(t *testing.T) *notesRoute {
	t.Helper()
	r := &notesRoute{t: t, calls: make(chan *notesCall, 64)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		answer := make(chan notesAnswer, 1)
		r.calls <- &notesCall{
			verb: req.Method + " " + path.Base(req.URL.Path), after: req.URL.Query().Get("after"),
			body: string(body), at: time.Now(), gone: req.Context().Done(), answer: answer,
		}
		select {
		case a := <-answer:
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, a.body)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// url is the route as a run's configuration hands it to the plugin.
func (r *notesRoute) url() string { return r.srv.URL + "/mcp/run-token/steer" }

// next is the plugin's next request, failing the case after ten seconds.
func (r *notesRoute) next(want, what string) *notesCall {
	r.t.Helper()
	select {
	case c := <-r.calls:
		if c.verb != want {
			r.t.Fatalf("waiting for the plugin to %s, it made %s (after=%q, body %q), want %s",
				what, c.verb, c.after, c.body, want)
		}
		return c
	case <-time.After(10 * time.Second):
		r.t.Fatalf("waited 10s for the plugin to %s", what)
		return nil
	}
}

// none fails the case if the plugin makes any request within d.
func (r *notesRoute) none(d time.Duration, why string) {
	r.t.Helper()
	select {
	case c := <-r.calls:
		r.t.Fatalf("%s, and the plugin made %s (after=%q, body %q)", why, c.verb, c.after, c.body)
	case <-time.After(d):
	}
}

// letGo waits for the plugin to let go of a request it made.
func (r *notesRoute) letGo(c *notesCall, why string) {
	r.t.Helper()
	select {
	case <-c.gone:
	case <-time.After(5 * time.Second):
		r.t.Fatalf("%s, and the plugin still holds its %s after 5s", why, c.verb)
	}
}

// probeReport is one line the probe printed.
type probeReport struct {
	Op      string         `json:"op"`
	Name    string         `json:"name"`
	Method  string         `json:"method"`
	URL     string         `json:"url"`
	Ms      float64        `json:"ms"`
	Cleanup bool           `json:"cleanup"`
	Input   map[string]any `json:"input"`
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
}

// probe is the plugin running under node, driven over the probe's stdin.
type probe struct {
	t       *testing.T
	stdin   io.WriteCloser
	reports chan probeReport
	mu      sync.Mutex
	stderr  bytes.Buffer
}

func startProbe(t *testing.T, node, plugin string) *probe {
	t.Helper()
	script, err := filepath.Abs(probeScript)
	if err != nil {
		t.Fatal(err)
	}
	p := &probe{t: t, reports: make(chan probeReport, 256)}
	cmd := exec.Command(node, script, plugin) //nolint:gosec // the suite's own probe, of the plugin under test
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = lockedWriter{&p.mu, &p.stderr}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the probe: %v", err)
	}
	go func() {
		defer close(p.reports)
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			var r probeReport
			if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
				r = probeReport{Op: "error", Message: "an unreadable report: " + scan.Text()}
			}
			p.reports <- r
		}
	}()
	t.Cleanup(func() {
		_ = p.stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return p
}

// lockedWriter is a writer the probe's stderr copier and a failing case's read
// of it can share.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func (p *probe) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *probe) send(command map[string]any) {
	p.t.Helper()
	line, err := json.Marshal(command)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatalf("writing to the probe: %v\n%s", err, p.stderrText())
	}
}

// setup sets a plugin up as name, against url and for session.
func (p *probe) setup(name, url, session string) {
	p.t.Helper()
	p.send(map[string]any{"op": "setup", "name": name, "url": url, "session": session})
}

// event hands plugin name one event of the server's about session.
func (p *probe) event(name, typ, session string, data map[string]any) {
	p.t.Helper()
	fields := map[string]any{"sessionID": session}
	for k, v := range data {
		fields[k] = v
	}
	p.send(map[string]any{"op": "event", "name": name, "event": map[string]any{"type": typ, "data": fields}})
}

// expect is the probe's next report but a subscription's, which must be op.
func (p *probe) expect(op string) probeReport {
	p.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-p.reports:
			switch {
			case !ok:
				p.t.Fatalf("the probe ended waiting for %s:\n%s", op, p.stderrText())
			case r.Op == "subscribed":
				continue
			case r.Op != op:
				p.t.Fatalf("the probe reported %s (%+v), want %s\n%s", r.Op, r, op, p.stderrText())
			}
			return r
		case <-deadline:
			p.t.Fatalf("waited 10s for the probe to report %s\n%s", op, p.stderrText())
		}
	}
}

// pollStarts is the instants, on the plugin's own clock, at which its next n
// polls of the notes route began — reported once [probe] was asked to watch
// fetches ({"op":"watch-fetches"}), skipping subscriptions.
func (p *probe) pollStarts(n int) []time.Duration {
	p.t.Helper()
	var out []time.Duration
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case r, ok := <-p.reports:
			switch {
			case !ok:
				p.t.Fatalf("the probe ended waiting for poll %d\n%s", len(out)+1, p.stderrText())
			case r.Op == "subscribed":
			case r.Op != "fetch":
				p.t.Fatalf("the probe reported %s (%+v), want the start of a poll\n%s", r.Op, r, p.stderrText())
			case r.Method == http.MethodGet && strings.Contains(r.URL, "/notes?"):
				out = append(out, time.Duration(r.Ms*float64(time.Millisecond)))
			}
		case <-deadline:
			p.t.Fatalf("waited 10s for the start of poll %d\n%s", len(out)+1, p.stderrText())
		}
	}
	return out
}

// quiet fails the case if the probe reports anything but a subscription
// within d.
func (p *probe) quiet(d time.Duration, why string) {
	p.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case r, ok := <-p.reports:
			if ok && r.Op == "subscribed" {
				continue
			}
			p.t.Fatalf("%s, and the probe reported %s (%+v)\n%s", why, r.Op, r, p.stderrText())
		case <-deadline:
			return
		}
	}
}

// noteNumbered is a note as the bridge's route answers it — the bridge's own
// type, so a field it renames is a field this stand-in renames too.
func noteNumbered(seq int) mcpbridge.SteerNote {
	id := "note-" + string(rune('0'+seq))
	return mcpbridge.SteerNote{Seq: seq, ID: id, Inbox: codingagent.OpenCodeInboxID("job-1", id),
		Message: "## A note from ada, sent while this turn was running\n\nnote " + id}
}

func notesBody(notes ...mcpbridge.SteerNote) string {
	if notes == nil {
		notes = []mcpbridge.SteerNote{}
	}
	raw, _ := json.Marshal(map[string]any{"notes": notes})
	return string(raw)
}

// wantAdmitted fails the case unless input is note admitted to session as the
// plugin must admit it: under its inbox id, as a steer, without resuming the
// session.
func wantAdmitted(t *testing.T, input map[string]any, session string, note mcpbridge.SteerNote) {
	t.Helper()
	want := map[string]any{"sessionID": session, "id": note.Inbox, "text": note.Message,
		"delivery": "steer", "resume": false}
	if len(input) != len(want) {
		t.Errorf("the admission is %v, want exactly %v", input, want)
	}
	for k, v := range want {
		if input[k] != v {
			t.Errorf("the admission's %s = %#v, want %#v (whole: %v)", k, input[k], v, input)
		}
	}
}

const (
	steerSession = "ses_0123456789abcdef0123456789abcdef"
	otherSession = "ses_fedcba9876543210fedcba9876543210"
)

// THE PLUGIN ADMITS A PERSON'S NOTES INTO THE RUN'S SESSION AT ITS NEXT STEP —
// run as the engine writes it into a real box, under node, against a stand-in
// of the bridge's notes route and of the context OpenCode hands a plugin:
//
//   - its setup resolves at once, and nothing is polled before the run's own
//     session's execution starts;
//   - each note is admitted under its inbox id, as a steer, without resuming
//     the session, and the cursor moves past it only once it is admitted;
//   - a note is confirmed read only on its own session's delivery of an id the
//     plugin admitted, and nothing is admitted or polled once the execution
//     settles — until OpenCode starts another;
//   - a failing route is asked again after a pause that doubles and resets on
//     a success; a refused token (401) or a run with no notes route (404) ends
//     it, as does its cleanup;
//   - a run wired to nothing polls nothing, and the plugin writes nothing
//     under its own directory, which OpenCode reloads it on any change to.
//
// Mutations, each red: admit with `resume: true`; open the gate at setup;
// advance the cursor on a failed admission; leave the gate open on
// `succeeded`; confirm any delivery; retry a 401 or a 404; drop the doubling,
// or its reset; await a poll in setup; write a file beside the plugin.
func TestThePluginAdmitsNotesAtTheRunsNextStep(t *testing.T) {
	t.Parallel()
	node := nodeForProbe(t)

	// THE PLUGIN AS A RUN IS HANDED IT: written by WriteConfig into a real
	// box, its route named by a reference to a file holding it.
	_, box := openCodeBox(t, t.TempDir())
	p := codingagent.PathsFor(box)
	const configured = "http://127.0.0.1:9/mcp/run-token/steer"
	cfgPath, err := codingagent.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: configured},
	}, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	plugin := steerPluginOf(t, cfgPath)
	if plugin.Package != p.OpenCodeSteerPlugin() {
		t.Fatalf("the configuration names the plugin at %q, want %q", plugin.Package, p.OpenCodeSteerPlugin())
	}
	if got := readReference(t, plugin.Options.URL); got != configured {
		t.Fatalf("the plugin's route reads as %q, want %q", got, configured)
	}
	entry := filepath.Join(plugin.Package, "index.js")
	before := snapshot(t, plugin.Package)
	t.Cleanup(func() {
		after := snapshot(t, plugin.Package)
		if len(after) != len(before) {
			t.Errorf("the plugin's directory changed while it ran:\nbefore %v\nafter  %v", keys(before), keys(after))
		}
		for name, was := range before {
			if after[name] != was {
				t.Errorf("%s in the plugin's directory changed while it ran", name)
			}
		}
	})

	t.Run("admits each note at its next step and confirms only what OpenCode delivered", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		if r := pr.expect("setup"); r.Ms > 1000 || !r.Cleanup {
			t.Fatalf("setup took %.0f ms and returned a cleanup: %v, want it at once and with one", r.Ms, r.Cleanup)
		}
		route.none(300*time.Millisecond, "the session's execution has not started")
		pr.event("a", "session.execution.started", otherSession, nil)
		route.none(300*time.Millisecond, "only another session's execution started")

		pr.event("a", "session.execution.started", steerSession, nil)
		poll := route.next("GET notes", "poll once the session's execution started")
		if poll.after != "0" {
			t.Errorf("the first poll asks past %q, want 0", poll.after)
		}
		first := noteNumbered(1)
		poll.reply(http.StatusOK, notesBody(first))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, first)
		if !pr.expect("prompt").OK {
			t.Fatal("the stand-in refused an admission it was not told to")
		}
		held := route.next("GET notes", "poll past the note it admitted")
		if held.after != "1" {
			t.Errorf("the poll after an admitted note asks past %q, want 1", held.after)
		}

		// CONFIRMED ONLY ON ITS OWN SESSION'S DELIVERY OF A NOTE IT ADMITTED.
		pr.event("a", "session.inbox.delivered", otherSession, map[string]any{"inboxID": first.Inbox})
		pr.event("a", "session.inbox.delivered", steerSession,
			map[string]any{"inboxID": codingagent.OpenCodeInboxID("job-1", "not-admitted")})
		route.none(300*time.Millisecond, "OpenCode delivered no note this plugin admitted to its session")
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": first.Inbox})
		read := route.next("POST read", "confirm the note OpenCode delivered")
		if read.body != `{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want the delivered note's id", read.body)
		}
		read.reply(http.StatusNoContent, "")

		// THE EXECUTION SETTLES: the held poll is let go and nothing more is
		// asked, until OpenCode starts another execution.
		pr.event("a", "session.execution.succeeded", steerSession, nil)
		route.letGo(held, "the session's execution succeeded")
		route.none(1500*time.Millisecond, "the session's execution succeeded")
		pr.event("a", "session.execution.started", steerSession, nil)
		poll = route.next("GET notes", "poll again once OpenCode started another execution")
		if poll.after != "1" {
			t.Errorf("the poll of a later execution asks past %q, want 1", poll.after)
		}

		// A NOTE LEFT IN A BATCH WHEN THE EXECUTION SETTLES IS NOT ADMITTED.
		pr.send(map[string]any{"op": "hold-prompts", "name": "a"})
		second, third := noteNumbered(2), noteNumbered(3)
		poll.reply(http.StatusOK, notesBody(second, third))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, second)
		pr.event("a", "session.execution.succeeded", steerSession, nil)
		pr.send(map[string]any{"op": "release-prompts", "name": "a"})
		if !pr.expect("prompt").OK {
			t.Fatal("the stand-in refused an admission it was not told to")
		}
		pr.quiet(500*time.Millisecond, "the execution settled before the third note")
		route.none(time.Second, "the session's execution succeeded")
	})

	t.Run("moves its cursor past a note only once the note is admitted", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.send(map[string]any{"op": "fail-prompts", "name": "a", "count": 1})
		pr.event("a", "session.execution.started", steerSession, nil)
		first := noteNumbered(1)
		refused := route.next("GET notes", "poll once the session's execution started")
		refused.reply(http.StatusOK, notesBody(first))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, first)
		if pr.expect("prompt").OK {
			t.Fatal("the stand-in admitted what it was told to refuse")
		}
		again := route.next("GET notes", "ask again for the note it could not admit")
		if again.after != "0" {
			t.Errorf("after a failed admission the poll asks past %q, want 0 — the note not admitted", again.after)
		}
		if gap := again.at.Sub(refused.at); gap < 900*time.Millisecond {
			t.Errorf("asked again %s after a failed admission, want a pause of a second", gap)
		}
		again.reply(http.StatusOK, notesBody(first))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, first)
		if !pr.expect("prompt").OK {
			t.Fatal("the stand-in refused an admission it was not told to")
		}
		if past := route.next("GET notes", "poll past the admitted note"); past.after != "1" {
			t.Errorf("after the admission the poll asks past %q, want 1", past.after)
		}
	})

	t.Run("asks a failing route again after a pause that doubles and resets", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.event("a", "session.execution.started", steerSession, nil)
		var calls []*notesCall
		for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusOK,
			http.StatusServiceUnavailable} {
			c := route.next("GET notes", "poll")
			calls = append(calls, c)
			if status == http.StatusOK {
				// A SUCCESS THAT ADMITTED A NOTE, whose next poll is at once
				// — one that admitted nothing waits out the floor instead.
				c.reply(status, notesBody(noteNumbered(1)))
				continue
			}
			c.reply(status, `{"error":"try again"}`)
		}
		last := route.next("GET notes", "poll after the success's own failure")
		calls = append(calls, last)
		gap := func(i int) time.Duration { return calls[i+1].at.Sub(calls[i].at) }
		if g := gap(0); g < 900*time.Millisecond {
			t.Errorf("asked again %s after the first failure, want a second's pause", g)
		}
		if g := gap(1); g < 1800*time.Millisecond {
			t.Errorf("asked again %s after the second failure in a row, want the pause doubled to two seconds", g)
		}
		if g := gap(2); g > 900*time.Millisecond {
			t.Errorf("asked again %s after a poll that admitted a note, want at once", g)
		}
		if g := gap(3); g < 900*time.Millisecond || g > 3500*time.Millisecond {
			t.Errorf("asked again %s after a failure that followed a success, want the pause back at a second", g)
		}
	})

	t.Run("waits a second before polling again after an answer that admitted nothing", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.send(map[string]any{"op": "watch-fetches"})
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.event("a", "session.execution.started", steerSession, nil)
		// AN EMPTY ANSWER AT ONCE, as a server that would not hold the poll
		// gives one — twice, so a floor that only spaced the first is red.
		first := route.next("GET notes", "poll")
		first.reply(http.StatusOK, notesBody())
		second := route.next("GET notes", "poll after an empty answer")
		second.reply(http.StatusOK, `{"notes":[]}`)
		third := route.next("GET notes", "poll after a second empty answer")
		// MEASURED WHERE THE FLOOR IS KEPT: from one poll's start to the
		// next's, on the plugin's own clock. The route sees each arrive, and
		// the first pays for the client's setup and its connection, so the
		// gap it measures to the second is short by that much — under load,
		// by more than the tolerance below.
		starts := pr.pollStarts(3)
		for i := range 2 {
			if gap := starts[i+1] - starts[i]; gap < 950*time.Millisecond {
				t.Errorf("poll %d began %s after an empty answer that arrived at once, want a second's floor",
					i+2, gap)
			}
		}
		// AND AN ANSWER THAT ARRIVED AFTER THE FLOOR IS ASKED AGAIN AT ONCE:
		// the long poll's own empty answer, after its hold.
		time.Sleep(1200 * time.Millisecond)
		answered := time.Now()
		third.reply(http.StatusOK, notesBody())
		if fourth := route.next("GET notes", "poll after a held empty answer"); fourth.at.Sub(answered) > 800*time.Millisecond {
			t.Errorf("a poll held past the floor was asked again %s after its answer, want at once",
				fourth.at.Sub(answered))
		}
	})

	t.Run("ends when the route refuses the run's token", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.event("a", "session.execution.started", steerSession, nil)
		route.next("GET notes", "poll").reply(http.StatusUnauthorized, `{"error":"invalid_token"}`)
		route.none(2500*time.Millisecond, "the route refused the run's token")
		pr.event("a", "session.execution.started", steerSession, nil)
		route.none(1500*time.Millisecond, "the route refused the run's token before this execution")
	})

	t.Run("ends when the run has no notes route", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.event("a", "session.execution.started", steerSession, nil)
		first := noteNumbered(1)
		route.next("GET notes", "poll").reply(http.StatusOK, notesBody(first))
		pr.expect("prompt-started")
		pr.expect("prompt")
		held := route.next("GET notes", "poll past the admitted note")
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": first.Inbox})
		route.next("POST read", "confirm the delivered note").reply(http.StatusNotFound, `{"error":"not_found"}`)
		route.letGo(held, "the route answered that the run has no notes")
		route.none(2500*time.Millisecond, "the route answered that the run has no notes")
	})

	t.Run("ends everything at its cleanup", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		pr.event("a", "session.execution.started", steerSession, nil)
		held := route.next("GET notes", "poll")
		pr.send(map[string]any{"op": "cleanup", "name": "a"})
		pr.expect("cleaned")
		route.letGo(held, "the plugin was cleaned up")
		pr.event("a", "session.execution.started", steerSession, nil)
		route.none(1500*time.Millisecond, "the plugin was cleaned up")
	})

	t.Run("polls nothing for a run wired to no notes", func(t *testing.T) {
		t.Parallel()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("no-route", "", steerSession)
		if pr.expect("setup").Cleanup {
			t.Error("a plugin given no route returned a cleanup, as if it had started something")
		}
		pr.setup("no-session", route.url(), "")
		if pr.expect("setup").Cleanup {
			t.Error("a plugin given no session returned a cleanup, as if it had started something")
		}
		for _, name := range []string{"no-route", "no-session"} {
			pr.event(name, "session.execution.started", steerSession, nil)
			pr.event(name, "session.execution.started", "", nil)
		}
		route.none(time.Second, "the plugin was given nothing to poll")
	})
}

// routeInbox is a run's inbox as the real bridge serves it to the plugin: the
// notes it holds, answered past each poll's cursor, and every read the route
// handed it.
type routeInbox struct {
	mu    sync.Mutex
	notes []mcpbridge.SteerNote
	reads chan []string
}

func (i *routeInbox) Take(context.Context, string) string { return "" }

func (i *routeInbox) Wait(_ context.Context, after int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []mcpbridge.SteerNote
	for _, n := range i.notes {
		if n.Seq > after {
			out = append(out, n)
		}
	}
	// NOTHING MORE ARRIVES: a poll past the last note is held to the
	// route's own hold.
	return out, make(chan struct{})
}

func (i *routeInbox) Read(_ context.Context, ids []string) error {
	i.reads <- ids
	return nil
}

func (i *routeInbox) Close() {}

// THE PLUGIN READS THE BRIDGE'S OWN NOTES ROUTE. The other cases answer the
// plugin from a stand-in of the route; this one runs it against a real bridge
// serving a run's session, its inbox a stand-in of the engine's, so the
// route's answer and its read are crossed as a run's CLI crosses them — the
// note's place, its id, the id it is admitted under and its message, the
// cursor the plugin asks past and the ids it confirms. A field either side
// renamed would leave every OpenCode run admitting nothing, with no error
// anywhere: the plugin skips a note it cannot read, and the note waits for the
// review.
//
// Mutation: rename `inbox`, `message` or `seq` on mcpbridge.SteerNote, or `ids`
// on the route's read, or the field the plugin reads for any of them, and this
// goes red.
func TestThePluginReadsTheBridgesOwnNotesRoute(t *testing.T) {
	t.Parallel()
	node := nodeForProbe(t)
	_, box := openCodeBox(t, t.TempDir())
	p := codingagent.PathsFor(box)
	cfgPath, err := codingagent.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool,
			URL: "http://127.0.0.1:9/mcp/run-token/steer"},
	}, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	entry := filepath.Join(steerPluginOf(t, cfgPath).Package, "index.js")

	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: codingagent.OpenCodeInboxID("job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})

	pr := startProbe(t, node, entry)
	pr.setup("a", mcpbridge.SteerURL(endpoint), steerSession)
	pr.expect("setup")
	pr.event("a", "session.execution.started", steerSession, nil)
	wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, note)
	if !pr.expect("prompt").OK {
		t.Fatal("the stand-in refused an admission it was not told to")
	}
	pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
	select {
	case ids := <-inbox.reads:
		if len(ids) != 1 || ids[0] != note.ID {
			t.Errorf("the route handed the inbox the read %v, want the note's id %q", ids, note.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the plugin's confirmation never reached the run's inbox through the route\n%s",
			pr.stderrText())
	}
	// AND THE NEXT POLL ASKS PAST THE NOTE IT ADMITTED: nothing is admitted
	// twice.
	pr.quiet(1500*time.Millisecond, "the only note was admitted already")
}

// openCodePlugin is a run configuration's plugin entry.
type openCodePlugin struct {
	Package string `json:"package"`
	Options struct {
		URL string `json:"url"`
	} `json:"options"`
}

// steerPluginOf is the one plugin the run's configuration at path names.
func steerPluginOf(t *testing.T, path string) openCodePlugin {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the run's configuration, under the test's box
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	var cfg struct {
		Plugins []openCodePlugin `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(cfg.Plugins) != 1 {
		t.Fatalf("the configuration names %d plugins, want the notes plugin alone:\n%s", len(cfg.Plugins), raw)
	}
	return cfg.Plugins[0]
}

// readReference is what OpenCode reads a `{file:<path>}` reference as: the
// file's content, trimmed.
func readReference(t *testing.T, ref string) string {
	t.Helper()
	file, ok := referencedPath(ref)
	if !ok {
		t.Fatalf("%q is not a reference to a file", ref)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // a value file under the test's box
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return strings.TrimSpace(string(raw))
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
