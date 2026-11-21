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

func (p *copilotProbe) send(command map[string]any) {
	p.t.Helper()
	line, _ := json.Marshal(command)
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatalf("writing to the probe: %v", err)
	}
}

// event hands the extension one session event; agent names a subagent's.
func (p *copilotProbe) event(typ, agent string, data map[string]any) {
	p.t.Helper()
	if data == nil {
		data = map[string]any{}
	}
	if agent != "" {
		data["agentId"] = agent
	}
	p.send(map[string]any{"op": "event", "event": map[string]any{"type": typ, "data": data}})
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
	p.event("assistant.turn_start", "", nil)
	route.next("GET notes", "poll").reply(http.StatusUnauthorized, "")
	route.none(1500*time.Millisecond, "the route refused the run's token")
	p.event("tool.execution_start", "", nil)
	p.hook("onAgentStop", map[string]any{})
	p.expect("hooked")
	route.none(300*time.Millisecond, "the route refused the run's token")
}
