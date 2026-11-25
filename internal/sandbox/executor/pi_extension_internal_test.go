package executor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// piRouteCall is one request the extension made of a stand-in route, held
// until the case answers it.
type piRouteCall struct {
	path, after, body string
	answer            chan<- piRouteAnswer
}

type piRouteAnswer struct {
	status int
	body   string
}

func (c piRouteCall) reply(status int, body string) { c.answer <- piRouteAnswer{status, body} }

// piRoute is a stand-in of the bridge's notes and skill routes: every request
// is handed to the case.
type piRoute struct {
	t     *testing.T
	url   string
	calls chan piRouteCall
}

func newPiRoute(t *testing.T) *piRoute {
	t.Helper()
	r := &piRoute{t: t, calls: make(chan piRouteCall, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		answer := make(chan piRouteAnswer, 1)
		r.calls <- piRouteCall{path: req.Method + " " + filepath.Base(req.URL.Path),
			after: req.URL.Query().Get("after"), body: string(body), answer: answer}
		select {
		case a := <-answer:
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, a.body)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL + "/mcp/run-token"
	return r
}

// next is the extension's next request, which must be want.
func (r *piRoute) next(want, what string) piRouteCall {
	r.t.Helper()
	select {
	case c := <-r.calls:
		if c.path != want {
			r.t.Fatalf("waiting for the extension to %s, it made %s (after=%q)", what, c.path, c.after)
		}
		return c
	case <-time.After(10 * time.Second): //fixedwait:liveness
		r.t.Fatalf("waited 10s for the extension to %s", what)
		return piRouteCall{}
	}
}

// piProbeReport is one line the stand-in API printed.
type piProbeReport struct {
	Op         string          `json:"op"`
	Name       string          `json:"name"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
	Config     json.RawMessage `json:"config"`
	Result     json.RawMessage `json:"result"`
	Event      json.RawMessage `json:"event"`
	Message    string          `json:"message"`
	Names      []string        `json:"names"`
}

// piProbe is the rendered extension running under node against the stand-in
// of pi's extension API (pi_extension_probe.mjs), driven over its stdin.
type piProbe struct {
	t       *testing.T
	stdin   io.WriteCloser
	reports chan piProbeReport
	// dir is the probe's working directory, its ctx.cwd; stderr, the file
	// its error stream goes to.
	dir, stderr string
}

// startPiProbe renders the extension with settings and runs it.
func startPiProbe(t *testing.T, settings piExtensionSettings) *piProbe {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Pi extension's probe runs it under node",
		"install any current node, as ci.yml does")
	source, err := piExtension(settings)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ext := filepath.Join(dir, "extension.mjs")
	if err := os.WriteFile(ext, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := filepath.Abs("pi_extension_probe.mjs")
	if err != nil {
		t.Fatal(err)
	}
	p := &piProbe{t: t, reports: make(chan piProbeReport, 256), dir: dir, stderr: filepath.Join(dir, "stderr")}
	errs, err := os.Create(p.stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = errs.Close() })
	cmd := exec.Command(node, probe, ext) //nolint:gosec // the suite's own probe
	cmd.Dir = dir
	cmd.Stderr = errs
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	go func() {
		defer close(p.reports)
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for scan.Scan() {
			var r piProbeReport
			_ = json.Unmarshal(scan.Bytes(), &r)
			p.reports <- r
		}
	}()
	return p
}

func (p *piProbe) send(command map[string]any) {
	p.t.Helper()
	raw, err := json.Marshal(command)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(raw, '\n')); err != nil {
		p.t.Fatal(err)
	}
}

// event plays pi's event name to every handler.
func (p *piProbe) event(name string, event map[string]any) {
	p.t.Helper()
	p.send(map[string]any{"op": "event", "name": name, "event": event})
}

// expect is the probe's next report, which must be op.
func (p *piProbe) expect(op string) piProbeReport {
	p.t.Helper()
	select {
	case r, ok := <-p.reports:
		if !ok || r.Op != op {
			p.t.Fatalf("the probe reported %+v, want %s", r, op)
		}
		return r
	case <-time.After(10 * time.Second): //fixedwait:liveness
		p.t.Fatalf("waited 10s for the probe to report %s", op)
		return piProbeReport{}
	}
}

// handled plays event name and is what its handlers returned and left the
// event as.
func (p *piProbe) handled(name string, event map[string]any) piProbeReport {
	p.t.Helper()
	p.event(name, event)
	r := p.expect("handled")
	if r.Name != name {
		p.t.Fatalf("handled %s, want %s", r.Name, name)
	}
	return r
}

// piBoundary is a boundary handler's answer.
type piBoundary struct {
	Entries []struct {
		Type       string `json:"type"`
		CustomType string `json:"customType"`
		Content    string `json:"content"`
		Display    bool   `json:"display"`
		Details    struct {
			Inbox string `json:"inbox"`
		} `json:"details"`
	} `json:"entries"`
	Continue *bool `json:"continue"`
}

func piBoundaryOf(t *testing.T, r piProbeReport) piBoundary {
	t.Helper()
	var b piBoundary
	if string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, &b); err != nil {
			t.Fatalf("the boundary answered %s: %v", r.Result, err)
		}
	}
	return b
}

// piNoteAnswer is the route's answer holding one note.
func piNoteAnswer(seq int, inbox, message string) string {
	raw, _ := json.Marshal(map[string]any{"notes": []map[string]any{
		{"seq": seq, "id": "req-" + inbox, "inbox": inbox, "message": message}}})
	return string(raw)
}

// A NOTE ADMITTED WHILE THE AGENT WORKS IS HANDED OVER AT ITS NEXT COMPLETED
// TURN'S END, whole, as a custom message never displayed and carrying its
// inbox id, with the one continuation that answers it; a turn that ended in an
// error or an abort hands nothing, since pi makes no request after it, and
// the note waits for the next.
//
// Mutation: hand notes over at every turn_end whatever its outcome, drop the
// inbox from the details, or answer continue unconditionally: this goes red.
func TestThePiExtensionHandsANoteOverAtTheNextCompletedTurn(t *testing.T) {
	t.Parallel()
	route := newPiRoute(t)
	p := startPiProbe(t, piExtensionSettings{Notes: route.url})
	p.expect("loaded")
	p.event("agent_start", nil)
	p.expect("handled")
	const note = "## A note from ada, sent while this turn was running\n\nTarget the release branch."
	route.next("GET notes", "poll for notes").reply(200, piNoteAnswer(3, "inbox-1", note))
	if after := route.next("GET notes", "poll past the note").after; after != "3" {
		t.Fatalf("the next poll asks after %q, want past the admitted note (3)", after)
	}

	quiet := piBoundaryOf(t, p.handled("turn_end", map[string]any{"outcome": "error", "entries": []any{}}))
	if len(quiet.Entries) != 0 || quiet.Continue != nil {
		t.Fatalf("an errored turn's end handed %+v", quiet)
	}
	got := piBoundaryOf(t, p.handled("turn_end", map[string]any{"outcome": "completed", "entries": []any{}}))
	if len(got.Entries) != 1 || got.Continue == nil || !*got.Continue {
		t.Fatalf("the turn's end answered %+v, want the note and one continuation", got)
	}
	e := got.Entries[0]
	if e.Type != "custom_message" || e.CustomType != piNoteType || e.Content != note || e.Display ||
		e.Details.Inbox != "inbox-1" {
		t.Fatalf("the note was handed over as %+v", e)
	}
	again := piBoundaryOf(t, p.handled("agent_before_settle", map[string]any{"entries": []any{}}))
	if len(again.Entries) != 0 || again.Continue != nil {
		t.Fatalf("the finish handed %+v after the note went", again)
	}
}

// A NOTE ADMITTED AS THE RUN FINISHES IS HANDED OVER AT agent_before_settle,
// so the run goes on to answer it rather than leave it for its review.
//
// Mutation: drop the agent_before_settle handler: this goes red.
func TestThePiExtensionHandsANoteOverAsTheRunFinishes(t *testing.T) {
	t.Parallel()
	route := newPiRoute(t)
	p := startPiProbe(t, piExtensionSettings{Notes: route.url})
	p.expect("loaded")
	p.event("agent_start", nil)
	p.expect("handled")
	route.next("GET notes", "poll for notes").reply(200, piNoteAnswer(1, "inbox-9", "late words"))
	route.next("GET notes", "poll past the note")
	got := piBoundaryOf(t, p.handled("agent_before_settle", map[string]any{"entries": []any{}}))
	if len(got.Entries) != 1 || got.Entries[0].Details.Inbox != "inbox-9" || got.Continue == nil || !*got.Continue {
		t.Fatalf("the finish answered %+v, want the note and one continuation", got)
	}
}

// THE TURN PAST THE CAP IS ABORTED AS IT STARTS, before its request, with the
// entry that says the cap stopped it; the turns within it run, and a note is
// never handed over once the cap is spent, since answering it would need one
// more.
//
// Mutation: abort at the cap-th turn, count from one too late, or hand a note
// over at a spent cap: this goes red.
func TestThePiExtensionStopsTheRunPastItsCap(t *testing.T) {
	t.Parallel()
	p := startPiProbe(t, piExtensionSettings{Cap: 2})
	p.expect("loaded")
	p.handled("turn_start", nil)
	p.handled("turn_start", nil)
	p.event("turn_start", nil)
	entry := p.expect("entry")
	if entry.CustomType != piCapType || string(entry.Data) != `{"turns":2}` {
		t.Fatalf("the cap recorded %+v", entry)
	}
	p.expect("abort")
	p.expect("handled")
}

// A SERVER THE RUN MUST HOLD THAT OFFERS NO TOOL AT THE FIRST TURN STOPS THE RUN
// THERE, with the entry naming it and no request made: pi starts a run whose
// server is unreachable or refuses its token silently, the tools just absent.
// A server that offers its tools passes, and so does one not required.
//
// Mutation: skip the check, or check at before_agent_start (before pi has
// connected), or accept another server's tools: this goes red.
func TestThePiExtensionFailsARunWhoseBridgeOffersNoTool(t *testing.T) {
	t.Parallel()
	settings := piExtensionSettings{Required: []string{"crewlet"}, Servers: []piServer{
		{Name: "crewlet", Config: map[string]any{"url": "http://127.0.0.1:9/mcp", "exposure": "direct"}},
		{Name: "docs", Config: map[string]any{"url": "http://127.0.0.1:9/docs", "exposure": "direct"}}}}

	alive := startPiProbe(t, settings)
	for _, name := range []string{"crewlet", "docs"} {
		if r := alive.expect("register"); r.Name != name {
			t.Fatalf("registered %s, want %s", r.Name, name)
		}
	}
	alive.expect("loaded")
	alive.send(map[string]any{"op": "tools", "names": []string{"read", "mcp__crewlet__report"}})
	alive.handled("turn_start", nil)

	dead := startPiProbe(t, settings)
	dead.expect("register")
	dead.expect("register")
	dead.expect("loaded")
	dead.send(map[string]any{"op": "tools", "names": []string{"read", "mcp__docs__search", "mcp__crewletx__report"}})
	dead.event("turn_start", nil)
	entry := dead.expect("entry")
	if entry.CustomType != piBridgeType || string(entry.Data) != `{"missing":["crewlet"]}` {
		t.Fatalf("the dead bridge recorded %+v", entry)
	}
	dead.expect("abort")
}

// AN MCP.JSON ENTRY UNDER ONE OF THE RUN'S SERVERS' NAMES STOPS THE RUN AT ITS
// FIRST TURN: pi 1.6.0 uses an entry of the home's or the working directory's
// mcp.json in place of a server an extension registers in the same namespace
// (`-` and `_` alike), so a home or a repository could stand its own server in
// for the seat's tools. The entry names each, and no request is made. A file
// defining only servers of its own leaves the run going, beside the run's.
//
// Mutation: skip the check, read only one of the files, or compare names
// without pi's namespace: this goes red.
func TestThePiExtensionStopsARunWhoseServerAnMCPFileRedefines(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(home, []byte(`{"mcpServers":{"home-mcp":{"command":"home-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := piExtensionSettings{Required: []string{"crewlet"}, MCPFiles: []string{home, ".pi/mcp.json"},
		Servers: []piServer{{Name: "crewlet", Config: map[string]any{"url": "http://127.0.0.1:9/mcp"}},
			{Name: "seat-docs", Config: map[string]any{"url": "http://127.0.0.1:9/docs"}}}}
	start := func(project string) *piProbe {
		t.Helper()
		p := startPiProbe(t, settings)
		p.expect("register")
		p.expect("register")
		p.expect("loaded")
		if project != "" {
			if err := os.MkdirAll(filepath.Join(p.dir, ".pi"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.dir, ".pi", "mcp.json"), []byte(project), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		p.send(map[string]any{"op": "tools", "names": []string{"mcp__crewlet__report", "mcp__home_mcp__x"}})
		return p
	}

	start(`{"mcpServers":{"repo-mcp":{"command":"repo-mcp"}}}`).handled("turn_start", nil)

	redefined := start(`{"mcpServers":{"repo-mcp":{"command":"repo-mcp"},"seat_docs":{"command":"other"}}}`)
	redefined.event("turn_start", nil)
	entry := redefined.expect("entry")
	dir, err := filepath.EvalSymlinks(redefined.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"redefined":["\"seat_docs\" is defined again in ` + filepath.Join(dir, ".pi", "mcp.json") + `"]}`
	if entry.CustomType != piBridgeType || string(entry.Data) != want {
		t.Fatalf("the redefinition recorded %+v, want %s", entry, want)
	}
	redefined.expect("abort")

	if err := os.WriteFile(home, []byte(`{"mcpServers":{"crewlet":{"url":"https://elsewhere.example.com/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	off := start("")
	off.event("turn_start", nil)
	if entry := off.expect("entry"); !strings.Contains(string(entry.Data), `\"crewlet\" is defined again in `+home) {
		t.Fatalf("the home's redefinition recorded %+v", entry)
	}
	off.expect("abort")
}

// THE SEAT'S STANDING INSTRUCTIONS ARE A PROMPT SECTION OF THEIR OWN, set on
// every run's before_agent_start, where pi renders it after its own prompt and
// any APPEND_SYSTEM.md (measured on 1.6.0); and the
// extension records that it ran, once.
//
// Mutation: drop the section, or record the extension's word on every start:
// this goes red.
func TestThePiExtensionAddsTheSeatsSectionAndSaysItRan(t *testing.T) {
	t.Parallel()
	p := startPiProbe(t, piExtensionSettings{System: "You are the seat."})
	p.expect("loaded")
	event := map[string]any{"systemPromptOptions": map[string]any{"sections": map[string]any{}}}
	p.event("before_agent_start", event)
	if entry := p.expect("entry"); entry.CustomType != piArmedType {
		t.Fatalf("the extension recorded %+v, want its word that it ran", entry)
	}
	var left struct {
		SystemPromptOptions struct {
			Sections map[string]string `json:"sections"`
		} `json:"systemPromptOptions"`
	}
	if err := json.Unmarshal(p.expect("handled").Event, &left); err != nil {
		t.Fatal(err)
	}
	if left.SystemPromptOptions.Sections["crewlet"] != "You are the seat." {
		t.Fatalf("the sections are %v", left.SystemPromptOptions.Sections)
	}
	if r := p.handled("before_agent_start", event); r.Op != "handled" {
		t.Fatalf("a second start reported %+v", r)
	}
}

// A READ OF ONE OF THE RUN'S OWN SKILL FILES IS REPORTED TO THE SKILL ROUTE BY
// THE NAME IT IS LISTED UNDER, before the model sees the file (the handler is
// awaited), a relative path read against the run's working directory; a read
// of any other file is reported nothing.
//
// Mutation: report every read, or match the path as written: this goes red.
func TestThePiExtensionReportsASkillLoad(t *testing.T) {
	t.Parallel()
	route := newPiRoute(t)
	// The run's working directory as the process reads it (symlinks
	// resolved), and the skill beside it.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(dir, "skills", "crewlet:deploy", "SKILL.md")
	p := startPiProbe(t, piExtensionSettings{Skills: map[string]string{skill: "crewlet:deploy"},
		SkillURL: route.url + "/skill"})
	p.expect("loaded")
	p.event("tool_result", map[string]any{"toolName": "read", "input": map[string]any{"path": "README.md"}})
	p.expect("handled")
	cwd, err := filepath.EvalSymlinks(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, skill)
	if err != nil {
		t.Fatal(err)
	}
	// Relative to the run's working directory, the probe's.
	p.event("tool_result", map[string]any{"toolName": "read", "input": map[string]any{"path": rel}})
	call := route.next("POST skill", "report the skill's load")
	if call.body != `{"skill":"crewlet:deploy"}` {
		t.Fatalf("the report said %s", call.body)
	}
	call.reply(200, "{}")
	p.expect("handled")
}

// THE EXTENSION ADDS WHAT THE NOTE HELPER HANDS TO THE TOOL'S RESULT: after
// each tool call it runs the helper's pi-nested adapter on the run's config,
// its input closed, and adds what it printed to the result's content after
// the tool's own; it adds nothing where the helper prints nothing or fails,
// stops one past its bound (the helper sees the signal), and runs none for a
// call a codemode script nests, whose result reaches the script.
//
// Mutation: replace the tool's content, run the helper for a nested call,
// drop the timeout, leave its input open, or keep a failed firing's output,
// and a check goes red.
func TestThePiExtensionAddsWhatTheNoteHelperHandsToTheToolsResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	helper, said, calls := filepath.Join(dir, "notehelper"), filepath.Join(dir, "said"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s|' \"$@\" >> " + shellword.Quote(calls) + "; echo >> " + shellword.Quote(calls) +
		"\nif read -r l; then echo stdin >> " + shellword.Quote(calls) + "; fi\n" +
		"c=" + shellword.Quote(calls) + "\ncase \"$(cat " + shellword.Quote(said) + ")\" in " +
		"hang) trap 'echo killed >> \"$c\"; kill $!; exit 143' TERM; sleep 30 & wait;; fail) echo partial; exit 3;; esac\n" +
		"cat " + shellword.Quote(said) + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own helper
		t.Fatal(err)
	}
	p := startPiProbe(t, piExtensionSettings{Checkouts: piTestHand(helper, 500)})
	p.expect("loaded")
	result := func(saying string, event map[string]any) string {
		t.Helper()
		if err := os.WriteFile(said, []byte(saying), 0o600); err != nil {
			t.Fatal(err)
		}
		event["content"] = []map[string]any{{"type": "text", "text": "OWN"}}
		return string(p.handled("tool_result", event).Result)
	}
	bash := func() map[string]any {
		return map[string]any{"toolName": "bash", "input": map[string]any{"command": "ls"}}
	}
	if got := result("HANDED", bash()); got != `{"content":[{"text":"OWN","type":"text"},{"type":"text","text":"HANDED"}]}` {
		t.Errorf("a firing that handed answered %s", got)
	}
	if got := result("", bash()); got != "null" {
		t.Errorf("a firing that handed nothing answered %s", got)
	}
	nested := bash()
	nested["parentToolCallId"] = "outer"
	if got := result("NESTED", nested); got != "null" {
		t.Errorf("a nested call's result answered %s", got)
	}
	if got := result("fail", bash()); got != "null" {
		t.Errorf("a failed firing answered %s", got)
	}
	if got := result("hang", bash()); got != "null" {
		t.Errorf("a firing past its bound answered %s", got)
	}
	raw, err := os.ReadFile(calls) //nolint:gosec // the case's own file
	if err != nil {
		t.Fatal(err)
	}
	firing := fmt.Sprintf("-adapter|pi-nested|-config|/run/nested.json|-room|%d|\n", piNestedFallbackRoom-len("OWN"))
	if want := strings.Repeat(firing, 4) + "killed\n"; string(raw) != want {
		t.Errorf("the helper was run as\n%s\nwant four firings, none for the nested call and none fed any input, "+
			"the last stopped at its bound:\n%s", raw, want)
	}
}

// piTestHand is a run's hand for the helper at helper, its timeout timeoutMs
// and the run's own bounds.
func piTestHand(helper string, timeoutMs int64) *piNestedHand {
	return &piNestedHand{Helper: helper, Config: "/run/nested.json", TimeoutMs: timeoutMs,
		MaxOutput: piNestedMaxOutput, MaxRoom: piNestedMaxRoom, FallbackRoom: piNestedFallbackRoom,
		MinRoom: piNestedMinRoom}
}

// THE EXTENSION HANDS THE HELPER THE ROOM PI LEAVES: -room is what
// ctx.getContextUsage leaves below the window less pi's compaction reserve
// twice (the model's override, else the setting, else 16,384), less the
// batch's results (an image 4,800 characters), four characters a token;
// at most the most room. A batch's calls run in parallel and join the
// session together, so its checkouts are handed at its last result, its
// other results each counted as pi counts a message, ceil(characters/4)
// (one ended unrun among them, a nested one never); a sequential call is a
// batch alone, and a batch a turn ended open is dropped. Where pi gives no
// tokens, the fallback, at most half what the window it names leaves an
// empty context, said once; no firing under the least room, one at it.
//
// Mutation: take the reserve once, skip the override, the image, the cap,
// the floor's edge, a sibling or its rounding, the window's fallback cap,
// or the turn's reset, or say the fallback each time, and a case goes red.
func TestThePiExtensionHandsTheHelperTheRoomPiLeaves(t *testing.T) {
	t.Parallel()
	usage := func(tokens any, window int) map[string]any {
		return map[string]any{"tokens": tokens, "contextWindow": window}
	}
	reserves := map[string]any{"compaction": map[string]any{"reserveTokens": 20_000,
		"modelOverrides": map[string]any{"p/m": map[string]any{"reserveTokens": 30_000}}}}
	texts := func(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }
	image := []map[string]any{{"type": "text", "text": "OWN"}, {"type": "image", "data": "", "mimeType": "image/png"}}
	big := strings.Repeat("R", 39_500)
	type step struct {
		name  string
		event map[string]any
		room  int // a tool_result's -room, 0 for no firing
	}
	start := func(id string) step { return step{"tool_execution_start", map[string]any{"toolCallId": id}, 0} }
	result := func(id string, content []map[string]any, room int) step {
		return step{"tool_result", map[string]any{"toolCallId": id, "toolName": "bash", "content": content}, room}
	}
	free := 200_000 - 2*16_384 - 1_000 // tokens below pi's reserve twice, 1,000 used
	for _, c := range []struct {
		name     string
		usage    map[string]any
		settings map[string]any
		model    map[string]any
		steps    []step
	}{
		{"pi's own reserve", usage(1_000, 200_000), nil, nil, []step{result("a", texts("OWN"), 4*free-3)}},
		{"the model's override", usage(1_000, 200_000), reserves, map[string]any{"provider": "p", "id": "m"},
			[]step{result("a", texts("OWN"), 4*(200_000-2*30_000-1_000)-3)}},
		{"the setting", usage(1_000, 200_000), reserves, map[string]any{"provider": "p", "id": "x"},
			[]step{result("a", texts("OWN"), 4*(200_000-2*20_000-1_000)-3)}},
		{"an image", usage(1_000, 200_000), nil, nil, []step{result("a", image, 4*free-3-4_800)}},
		{"a window past the cap", usage(0, 20_000_000), nil, nil, []step{result("a", texts("OWN"), piNestedMaxRoom)}},
		{"at the least room", usage(6_208, 40_000), nil, nil, []step{result("a", texts(""), piNestedMinRoom)}},
		{"a character under it", usage(6_208, 40_000), nil, nil, []step{result("a", texts("O"), 0)}},
		{"a parallel batch", usage(1_000, 200_000), nil, nil, []step{start("a"), start("b"), start("c"),
			result("c", texts("cloned"), 0), result("a", texts(big+"R"), 0),
			result("b", texts(big), 4*(free-2-9_876)-39_500)}},
		{"a call ended unrun", usage(1_000, 200_000), nil, nil, []step{start("a"),
			{"tool_execution_end", map[string]any{"toolCallId": "a", "result": map[string]any{"content": texts("refused")}}, 0},
			start("b"), start("c"), result("b", texts("OWN"), 0), result("c", texts("OWN"), 4*(free-2-1)-3)}},
		{"a nested call", usage(1_000, 200_000), nil, nil, []step{start("a"),
			{"tool_execution_start", map[string]any{"toolCallId": "n", "parentToolCallId": "a"}, 0},
			result("a", texts("OWN"), 4*free-3)}},
		{"sequential calls", usage(1_000, 200_000), nil, nil, []step{start("a"), result("a", texts("OWN"), 4*free-3),
			start("b"), result("b", texts("OWN"), 4*free-3)}},
		{"a turn ended open", usage(1_000, 200_000), nil, nil, []step{start("a"),
			{"turn_end", map[string]any{"entries": []any{}}, 0}, start("b"), result("b", texts("OWN"), 4*free-3)}},
		{"no tokens known", usage(nil, 200_000), nil, nil, []step{result("a", texts("OWN"), piNestedFallbackRoom-3)}},
		{"no tokens in a small window", usage(nil, 60_000), nil, nil,
			[]step{result("a", texts("OWN"), 2*(60_000-2*16_384)-3)}},
		{"no tokens in a window under the least room", usage(nil, 34_000), nil, nil, []step{result("a", texts("OWN"), 0)}},
		{"no usage, twice", nil, nil, nil, []step{result("a", texts("OWN"), piNestedFallbackRoom-3),
			result("b", texts("OWN"), piNestedFallbackRoom-3)}},
	} {
		dir := t.TempDir()
		helper, calls := filepath.Join(dir, "notehelper"), filepath.Join(dir, "calls")
		script := "#!/bin/sh\nprintf '%s ' \"$@\" >> " + shellword.Quote(calls) + "; echo >> " + shellword.Quote(calls) +
			"\necho HANDED\n"
		if err := os.WriteFile(helper, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own helper
			t.Fatal(err)
		}
		p := startPiProbe(t, piExtensionSettings{Checkouts: piTestHand(helper, 60_000)})
		p.expect("loaded")
		p.send(map[string]any{"op": "usage", "usage": c.usage})
		p.send(map[string]any{"op": "settings", "settings": c.settings})
		p.send(map[string]any{"op": "model", "model": c.model})
		var want []string
		for _, s := range c.steps {
			got := string(p.handled(s.name, s.event).Result)
			if s.name != "tool_result" {
				continue
			}
			if s.room > 0 {
				want = append(want, fmt.Sprintf("-adapter pi-nested -config /run/nested.json -room %d ", s.room))
			}
			if handed := strings.Contains(got, "HANDED"); handed != (s.room > 0) {
				t.Errorf("%s: %s's result handed %v (%s), want %v", c.name, s.event["toolCallId"], handed, got, s.room > 0)
			}
		}
		raw, err := os.ReadFile(calls) //nolint:gosec // the case's own file
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		var wantRaw strings.Builder
		for _, w := range want {
			wantRaw.WriteString(w + "\n")
		}
		if string(raw) != wantRaw.String() {
			t.Errorf("%s: the helper ran as\n%s\nwant\n%s", c.name, raw, wantRaw.String())
		}
		errs, err := os.ReadFile(p.stderr)
		if err != nil {
			t.Fatal(err)
		}
		fallback := c.usage == nil || c.usage["tokens"] == nil
		if n := strings.Count(string(errs), "pi gave no context usage"); n != map[bool]int{true: 1, false: 0}[fallback] {
			t.Errorf("%s: the fallback was said %d times:\n%s", c.name, n, errs)
		}
	}
}

// THE EXTENSION'S OUTPUT BOUND HOLDS A WHOLE ANSWER: a pi-nested answer
// within the most room a firing is handed prints at most three bytes a
// UTF-16 unit, and the extension hands nothing of an answer past its output
// bound (measured), so the bound must hold the largest; the fallback and
// least rooms lie within it.
//
// Mutation: raise the room past a third of the output bound, and this goes red.
func TestThePiExtensionsOutputBoundHoldsAWholeAnswer(t *testing.T) {
	t.Parallel()
	if answer := 3 * piNestedMaxRoom; answer > piNestedMaxOutput {
		t.Errorf("a firing may print %d bytes, past the extension's bound of %d", answer, piNestedMaxOutput)
	}
	if piNestedMinRoom > piNestedFallbackRoom || piNestedFallbackRoom > piNestedMaxRoom {
		t.Errorf("the rooms %d <= %d <= %d do not hold", piNestedMinRoom, piNestedFallbackRoom, piNestedMaxRoom)
	}
}

// THE NOTES POLL RUNS ON WHILE THE NOTE HELPER DOES: a firing still running
// holds the tool's result, never the process, so a poll answered meanwhile is
// followed by the next at once, before the helper is let go.
//
// Mutation: run the helper synchronously, and the next poll waits on it.
func TestThePiExtensionKeepsPollingWhileTheNoteHelperRuns(t *testing.T) {
	t.Parallel()
	route := newPiRoute(t)
	dir := t.TempDir()
	helper, release := filepath.Join(dir, "notehelper"), filepath.Join(dir, "release")
	script := "#!/bin/sh\nuntil [ -e " + shellword.Quote(release) + " ]; do sleep 0.05; done\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own helper
		t.Fatal(err)
	}
	p := startPiProbe(t, piExtensionSettings{Notes: route.url, Checkouts: piTestHand(helper, 60_000)})
	p.expect("loaded")
	p.event("agent_start", map[string]any{})
	p.expect("handled")
	first := route.next("GET notes", "poll for notes")
	p.event("tool_result", map[string]any{"toolName": "bash", "input": map[string]any{"command": "ls"},
		"content": []map[string]any{{"type": "text", "text": "OWN"}}})
	first.reply(200, piNoteAnswer(1, "inbox-1", "a note"))
	second := route.next("GET notes", "poll again while the helper runs")
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := p.expect("handled"); string(r.Result) != "null" {
		t.Errorf("the released firing answered %s", r.Result)
	}
	second.reply(200, `{"notes":[]}`)
}
