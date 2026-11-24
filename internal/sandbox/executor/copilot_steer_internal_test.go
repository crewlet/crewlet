package executor

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// copilotRouteCall is one request the extension made of the stand-in notes
// route, held until the case answers it.
type copilotRouteCall struct {
	verb, after string
	gone        <-chan struct{}
	answer      chan<- copilotRouteAnswer
}

type copilotRouteAnswer struct {
	status int
	body   string
}

func (c copilotRouteCall) reply(status int, body string) {
	c.answer <- copilotRouteAnswer{status, body}
}

// copilotRoute is a stand-in of the bridge's notes route: every request is
// handed to the case.
type copilotRoute struct {
	t     *testing.T
	url   string
	calls chan copilotRouteCall
}

func newCopilotRoute(t *testing.T) *copilotRoute {
	t.Helper()
	r := &copilotRoute{t: t, calls: make(chan copilotRouteCall, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		answer := make(chan copilotRouteAnswer, 1)
		r.calls <- copilotRouteCall{verb: req.Method + " " + path.Base(req.URL.Path),
			after: req.URL.Query().Get("after"), gone: req.Context().Done(), answer: answer}
		select {
		case a := <-answer:
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, a.body)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL + "/mcp/run-token/steer"
	return r
}

// next is the extension's next request, which must be want.
func (r *copilotRoute) next(want, what string) copilotRouteCall {
	r.t.Helper()
	select {
	case c := <-r.calls:
		if c.verb != want {
			r.t.Fatalf("waiting for the extension to %s, it made %s (after=%q)", what, c.verb, c.after)
		}
		return c
	case <-time.After(10 * time.Second): //fixedwait:liveness
		r.t.Fatalf("waited 10s for the extension to %s", what)
		return copilotRouteCall{}
	}
}

// none fails the case on any request made within d.
func (r *copilotRoute) none(d time.Duration, why string) {
	r.t.Helper()
	select {
	case c := <-r.calls:
		r.t.Fatalf("%s, and the extension made %s (after=%q)", why, c.verb, c.after)
	case <-time.After(d): //fixedwait:liveness
	}
}

// copilotProbeReport is one line the stand-in SDK printed.
type copilotProbeReport struct {
	Op      string         `json:"op"`
	Hooks   []string       `json:"hooks"`
	Input   map[string]any `json:"input"`
	Message string         `json:"message"`
	Level   string         `json:"level"`
	Name    string         `json:"name"`
}

// copilotProbe is the rendered extension running under node against the
// stand-in SDK (copilot_steer_probe.mjs), driven over its stdin.
type copilotProbe struct {
	t       *testing.T
	stdin   io.WriteCloser
	reports chan copilotProbeReport
	cmd     *exec.Cmd
}

// startCopilotProbe renders the extension with settings into a directory whose
// node_modules holds the stand-in SDK, and runs it.
func startCopilotProbe(t *testing.T, settings copilotExtensionSettings) *copilotProbe {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Copilot notes extension's probe runs it under node",
		"install any current node, as ci.yml does")
	dir := t.TempDir()
	sdk := filepath.Join(dir, "node_modules", "@microsoft", "copilot-sdk")
	stub, err := os.ReadFile("copilot_steer_probe.mjs")
	if err != nil {
		t.Fatal(err)
	}
	source, err := copilotExtension(settings)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(sdk, "package.json"): `{"name":"@microsoft/copilot-sdk","type":"module",` +
			`"exports":{"./extension":"./extension.mjs"}}`,
		filepath.Join(sdk, "extension.mjs"):        string(stub),
		filepath.Join(dir, "ext", "extension.mjs"): source,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := &copilotProbe{t: t, reports: make(chan copilotProbeReport, 256)}
	cmd := exec.Command(node, filepath.Join(dir, "ext", "extension.mjs")) //nolint:gosec // the suite's own probe
	p.cmd = cmd
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(p.reports)
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			var r copilotProbeReport
			if json.Unmarshal(scan.Bytes(), &r) != nil {
				r = copilotProbeReport{Op: "error", Message: scan.Text()}
			}
			p.reports <- r
		}
	}()
	t.Cleanup(func() {
		_ = p.stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return p
}

// end ends the extension's process as Copilot's extensions_reload does, and
// waits for it to be gone.
func (p *copilotProbe) end() {
	p.t.Helper()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	for range p.reports {
	}
}

func (p *copilotProbe) send(command map[string]any) {
	p.t.Helper()
	line, _ := json.Marshal(command)
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatalf("writing to the probe: %v", err)
	}
}

// event hands the extension one session event; agent names a subagent's, on
// the envelope as Copilot puts it (subagent-note).
func (p *copilotProbe) event(typ, agent string, data map[string]any) {
	p.t.Helper()
	if data == nil {
		data = map[string]any{}
	}
	event := map[string]any{"type": typ, "data": data}
	if agent != "" {
		event["agentId"] = agent
	}
	p.send(map[string]any{"op": "event", "event": event})
}

// hook calls the extension's hook name with input.
func (p *copilotProbe) hook(name string, input map[string]any) {
	p.t.Helper()
	p.send(map[string]any{"op": "hook", "name": name, "input": input})
}

// expect is the probe's next report, which must be op.
func (p *copilotProbe) expect(op string) copilotProbeReport {
	p.t.Helper()
	select {
	case r, ok := <-p.reports:
		if !ok {
			p.t.Fatalf("the probe ended waiting for %s", op)
		}
		if r.Op != op {
			p.t.Fatalf("the probe reported %s (%+v), want %s", r.Op, r, op)
		}
		return r
	case <-time.After(10 * time.Second): //fixedwait:liveness
		p.t.Fatalf("waited 10s for the probe to report %s", op)
		return copilotProbeReport{}
	}
}

// expectArmed is the probe's next report, which must be the extension's line
// saying it runs, which a capped run's collection requires ([copilotArmedMessage]).
func (p *copilotProbe) expectArmed() {
	p.t.Helper()
	if logged := p.expect("log"); logged.Message != copilotArmedMessage {
		p.t.Fatalf("the extension logged %q first, want %q", logged.Message, copilotArmedMessage)
	}
}

// copilotNote is a note as the route answers it.
func copilotNote(seq int, inbox, words string) string {
	raw, _ := json.Marshal(map[string]any{"notes": []map[string]any{{"seq": seq, "id": "note-" + inbox,
		"inbox": inbox, "message": "## A note from ada, sent while this turn was running\n\n" + words}}})
	return string(raw)
}

// The extension asks the route only while the main agent works: a subagent's
// turn opens nothing, the main agent's does, and each note is sent at once
// under the clientTurnId of its inbox id. It never posts /read: the runner
// confirms from Copilot's record.
//
// Mutation: open the gate on a subagent's turn, send without the clientTurnId,
// or post /read after a send — the case fails.
func TestTheCopilotExtensionSendsANoteWhileTheMainAgentWorks(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url})
	if joined := p.expect("joined"); strings.Join(joined.Hooks, ",") != "onAgentStop,onPostToolUse,onPostToolUseFailure" {
		t.Fatalf("hooks = %v", joined.Hooks)
	}
	p.expectArmed()
	p.event("assistant.turn_start", "sub-1", nil)
	route.none(300*time.Millisecond, "a subagent's turn started")

	p.event("assistant.turn_start", "", nil)
	poll := route.next("GET notes", "poll as the main agent works")
	if poll.after != "0" {
		t.Fatalf("first poll after=%q, want 0", poll.after)
	}
	poll.reply(http.StatusOK, copilotNote(1, "msg_a", "check X"))
	sent := p.expect("send")
	if sent.Input["clientTurnId"] != CopilotClientTurnID("msg_a") || sent.Input["mode"] != "immediate" ||
		!strings.HasSuffix(sent.Input["prompt"].(string), "check X") {
		t.Fatalf("sent %v", sent.Input)
	}
	again := route.next("GET notes", "poll past the note sent")
	if again.after != "1" {
		t.Fatalf("next poll after=%q, want past the note sent", again.after)
	}
	p.event("assistant.idle", "", nil)
	select {
	case <-again.gone:
	case <-time.After(5 * time.Second): //fixedwait:liveness
		t.Fatal("the idle session's poll was still held after 5s")
	}
	route.none(1500*time.Millisecond, "the main agent went idle")
}

// As the main agent stops, the route is asked once more and a waiting note
// sent — the run answers it before it ends; one note answered twice by the two
// asks is sent once.
//
// Mutation: ask nothing at the stop, or drop the admitted set — the case fails.
func TestTheCopilotExtensionAsksOnceMoreAsTheAgentStops(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url})
	p.expect("joined")
	p.expectArmed()
	p.event("assistant.turn_start", "", nil)
	loop := route.next("GET notes", "poll as the main agent works")

	// THE STOP'S SEND HELD while the loop's poll answers the same note, so
	// neither ask has moved its cursor past it: the note is sent once.
	p.send(map[string]any{"op": "hold-sends"})
	p.hook("onAgentStop", map[string]any{"stopReason": "end_turn"})
	stop := route.next("GET notes", "ask as the agent stops")
	stop.reply(http.StatusOK, copilotNote(1, "msg_b", "one more thing"))
	if sent := p.expect("send"); sent.Input["clientTurnId"] != CopilotClientTurnID("msg_b") {
		t.Fatalf("sent %v", sent.Input)
	}
	loop.reply(http.StatusOK, copilotNote(1, "msg_b", "one more thing"))
	select {
	case r := <-p.reports:
		t.Fatalf("the probe reported %s (%v) while the first send was held, want nothing: the note sent twice",
			r.Op, r.Input)
	case <-time.After(500 * time.Millisecond): //fixedwait:liveness
	}
	p.send(map[string]any{"op": "release-sends"})
	p.expect("hooked")
	// A SUBAGENT'S STOP asks nothing.
	p.hook("onAgentStop", map[string]any{"agentId": "sub-1"})
	p.expect("hooked")
}

// A send that fails leaves the note to be asked for again: an ask moves past
// a note only once its send has gone, and one that meets a note another ask is
// sending waits for that send. Once the gate closes, nothing more is sent.
//
// Mutation: move the cursor past a note before its send has gone — the stop's
// ask asks past it; skip a note another ask is sending — the stop returns at
// once and the loop's next poll asks past the failed note; drop the gate check
// — the second note of the closed loop's answer is sent.
func TestTheCopilotExtensionAsksAgainForANoteWhoseSendFailed(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url})
	p.expect("joined")
	p.expectArmed()
	p.event("assistant.turn_start", "", nil)
	p.send(map[string]any{"op": "hold-sends"})
	route.next("GET notes", "poll as the main agent works").reply(http.StatusOK, copilotNote(1, "msg_c", "try this"))
	p.expect("send")

	// THE STOP'S ASK, the loop's send still out: it asks before the note, and
	// waits on that send rather than moving past it.
	p.hook("onAgentStop", map[string]any{"stopReason": "end_turn"})
	stop := route.next("GET notes", "ask as the agent stops")
	if stop.after != "0" {
		t.Fatalf("the stop's ask asked after=%q while the note's send was out, want 0", stop.after)
	}
	stop.reply(http.StatusOK, copilotNote(1, "msg_c", "try this"))
	select {
	case r := <-p.reports:
		t.Fatalf("the probe reported %s (%v) while the note's send was out, want the stop waiting on it", r.Op, r.Input)
	case <-time.After(500 * time.Millisecond): //fixedwait:liveness
	}
	p.send(map[string]any{"op": "release-sends", "fail": true})
	p.expect("hooked")
	again := route.next("GET notes", "ask again for the note whose send failed")
	if again.after != "0" {
		t.Fatalf("the poll after the failed send asked after=%q, want 0: the note is never asked for again",
			again.after)
	}
	again.reply(http.StatusOK, copilotNote(1, "msg_c", "try this"))
	if sent := p.expect("send"); sent.Input["clientTurnId"] != CopilotClientTurnID("msg_c") {
		t.Fatalf("sent %v", sent.Input)
	}
	closing := route.next("GET notes", "poll past the note sent")
	p.event("assistant.idle", "", nil)
	<-closing.gone

	// TWO NOTES IN ONE ANSWER, the gate closing while the first is sent:
	// the second waits for the next turn's poll.
	p.event("assistant.turn_start", "", nil)
	both := route.next("GET notes", "poll as the next turn works")
	p.send(map[string]any{"op": "hold-sends"})
	both.reply(http.StatusOK, `{"notes":[`+
		`{"seq":2,"id":"note-d","inbox":"msg_d","message":"## A note from ada\n\nfirst"},`+
		`{"seq":3,"id":"note-e","inbox":"msg_e","message":"## A note from ada\n\nsecond"}]}`)
	if sent := p.expect("send"); sent.Input["clientTurnId"] != CopilotClientTurnID("msg_d") {
		t.Fatalf("sent %v", sent.Input)
	}
	p.event("assistant.idle", "", nil)
	p.send(map[string]any{"op": "release-sends"})
	select {
	case r := <-p.reports:
		t.Fatalf("the probe reported %s (%v) after the gate closed, want nothing more sent", r.Op, r.Input)
	case <-time.After(500 * time.Millisecond): //fixedwait:liveness
	}
	p.event("assistant.turn_start", "", nil)
	if next := route.next("GET notes", "poll as a third turn works"); next.after != "2" {
		t.Fatalf("the third turn's poll asked after=%q, want 2: past the note sent, before the one held back",
			next.after)
	}
}

// A failed poll waits before the next — a second, then twice that — and a
// 404 ends the polling for the rest of the run, as a 401 does.
//
// Mutation: poll again at once after a failure, or keep the first wait —
// a poll comes inside the wait; retry a 404 as a failure — the extension
// polls again.
func TestTheCopilotExtensionBacksOffAndStopsOnAMissingRoute(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url})
	p.expect("joined")
	p.expectArmed()
	p.event("assistant.turn_start", "", nil)
	route.next("GET notes", "poll").reply(http.StatusServiceUnavailable, "")
	route.none(800*time.Millisecond, "the route failed a moment ago")
	route.next("GET notes", "poll again after a second").reply(http.StatusServiceUnavailable, "")
	route.none(1800*time.Millisecond, "the route failed twice, and the wait doubled")
	route.next("GET notes", "poll again after two seconds").reply(http.StatusNotFound, "")
	route.none(1500*time.Millisecond, "the route is gone")
	p.event("assistant.turn_start", "", nil)
	p.hook("onAgentStop", map[string]any{})
	p.expect("hooked")
	route.none(300*time.Millisecond, "the route is gone")
}

// The turn cap: once the cap's last request is made, the run is stopped as
// that turn's tools answer — its line first, then the abort — and no ask is
// made at a stop past it.
//
// Mutation: abort only at the turn past the cap (the event path alone), or
// skip the line — the case fails.
func TestTheCopilotExtensionStopsTheRunAtItsCap(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url, Cap: 2})
	p.expect("joined")
	p.expectArmed()
	p.event("assistant.turn_start", "", nil)
	route.next("GET notes", "poll as the main agent works")
	p.hook("onPostToolUse", map[string]any{"toolName": "crewlet-echo"})
	p.expect("hooked")
	p.event("assistant.turn_start", "", nil)
	// A SUBAGENT'S TOOL is no turn of the main agent's.
	p.hook("onPostToolUse", map[string]any{"agentId": "sub-1"})
	p.expect("hooked")
	p.hook("onPostToolUse", map[string]any{"toolName": "crewlet-echo"})
	logged := p.expect("log")
	if !strings.HasPrefix(logged.Message, copilotCapMessage+"2 ") || logged.Level != "warning" {
		t.Fatalf("logged %+v", logged)
	}
	p.expect("abort")
	p.expect("hooked")
	p.hook("onAgentStop", map[string]any{})
	p.expect("hooked")
	route.none(300*time.Millisecond, "the run was past its cap at its stop")
}

// A refusal of the route ends the extension's asking for the rest of the run.
//
// Mutation: retry a 401 as a failure — the extension polls again.
func TestTheCopilotExtensionStopsOnARefusedRoute(t *testing.T) {
	route := newCopilotRoute(t)
	p := startCopilotProbe(t, copilotExtensionSettings{URL: route.url})
	p.expect("joined")
	p.expectArmed()
	p.event("assistant.turn_start", "", nil)
	route.next("GET notes", "poll").reply(http.StatusUnauthorized, "")
	route.none(1500*time.Millisecond, "the route refused the run's token")
	p.event("tool.execution_start", "", nil)
	p.hook("onAgentStop", map[string]any{})
	p.expect("hooked")
	route.none(300*time.Millisecond, "the route refused the run's token")
}

// A RESTARTED EXTENSION RESUMES ITS RUN'S CAP. Copilot's extensions_reload ends
// the extension's process and starts another in the same session (measured on
// 1.0.114: a reload let a fourth request past a cap of 3). The second process,
// reading the run's state, stops the run at the cap's last request all the
// same; a state another run left counts nothing.
//
// Mutation: count turns in memory alone, or honour another run's state: the
// restarted extension lets the third request through, or stops the other
// run's at its first.
func TestARestartedCopilotExtensionKeepsItsRunsCap(t *testing.T) {
	state := filepath.Join(t.TempDir(), "copilot-notes-state.json")
	settings := copilotExtensionSettings{Cap: 3, State: state, Run: "session-1"}
	first := startCopilotProbe(t, settings)
	first.expect("joined")
	first.expectArmed()
	for range 2 {
		first.event("assistant.turn_start", "", nil)
		first.hook("onPostToolUse", map[string]any{"toolName": "crewlet-echo"})
		first.expect("hooked")
	}
	first.end()
	kept, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("the extension kept no state: %v", err)
	}

	// ANOTHER RUN finding that state counts from nothing: its first request
	// is no third.
	other := startCopilotProbe(t, copilotExtensionSettings{Cap: 3, State: state, Run: "session-2"})
	other.expect("joined")
	other.expectArmed()
	other.event("assistant.turn_start", "", nil)
	other.hook("onPostToolUse", map[string]any{"toolName": "crewlet-echo"})
	other.expect("hooked")
	other.end()
	if err = os.WriteFile(state, kept, 0o600); err != nil {
		t.Fatal(err)
	}

	second := startCopilotProbe(t, settings)
	second.expect("joined")
	second.expectArmed()
	second.event("assistant.turn_start", "", nil)
	second.hook("onPostToolUse", map[string]any{"toolName": "extensions_reload"})
	if logged := second.expect("log"); !strings.HasPrefix(logged.Message, copilotCapMessage+"3 ") {
		t.Fatalf("the restarted extension logged %+v, want the cap of 3 reached at the run's third request", logged)
	}
	second.expect("abort")
}

// A RESTARTED EXTENSION NEVER SENDS A NOTE ITS RUN SENT: the cursor and every
// inbox id sent are in the run's state, the id kept before its send, so a
// note sent before the restart is passed over whether its send had gone (the
// cursor past it) or was cut off with the process (the cursor before it).
//
// Mutation: keep the cursor in memory alone, and the first note is sent again;
// keep the id only once its send has gone, and the cut-off note is sent again.
func TestARestartedCopilotExtensionSendsNoNoteTwice(t *testing.T) {
	route := newCopilotRoute(t)
	state := filepath.Join(t.TempDir(), "copilot-notes-state.json")
	settings := copilotExtensionSettings{URL: route.url, State: state, Run: "session-1"}
	first := startCopilotProbe(t, settings)
	first.expect("joined")
	first.expectArmed()
	first.event("assistant.turn_start", "", nil)
	route.next("GET notes", "poll as the main agent works").reply(http.StatusOK, copilotNote(1, "msg_a", "sent whole"))
	first.expect("send")
	past := route.next("GET notes", "poll past the note sent")
	if past.after != "1" {
		t.Fatalf("the poll after the send asked after=%q, want 1", past.after)
	}
	first.send(map[string]any{"op": "hold-sends"})
	past.reply(http.StatusOK, copilotNote(2, "msg_b", "cut off"))
	first.expect("send")
	first.end()

	second := startCopilotProbe(t, settings)
	second.expect("joined")
	second.expectArmed()
	second.event("assistant.turn_start", "", nil)
	resumed := route.next("GET notes", "poll as the restarted extension works")
	if resumed.after != "1" {
		t.Fatalf("the restarted extension asked after=%q, want 1: past the note its run sent", resumed.after)
	}
	resumed.reply(http.StatusOK, `{"notes":[`+
		`{"seq":1,"id":"note-msg_a","inbox":"msg_a","message":"## A note from ada\n\nsent whole"},`+
		`{"seq":2,"id":"note-msg_b","inbox":"msg_b","message":"## A note from ada\n\ncut off"}]}`)
	if next := route.next("GET notes", "poll past both notes"); next.after != "2" {
		t.Fatalf("the restarted extension asked after=%q, want 2: past the note cut off with the last process",
			next.after)
	}
	// A HOOK'S ANSWER COMES AFTER any send the answer above made, on the same
	// stream: the next report being it is no note sent again.
	second.hook("onPostToolUse", map[string]any{"toolName": "crewlet-echo"})
	if r := second.expect("hooked"); r.Name != "onPostToolUse" {
		t.Fatalf("hooked %+v", r)
	}
}
