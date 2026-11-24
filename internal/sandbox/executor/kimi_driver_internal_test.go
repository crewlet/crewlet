package executor

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// kimiProbe is a stand-in of `kimi web` and the notes route the driver
// (kimi_driver.js) is run against: KAP's routes the driver calls, answered as
// 2.7.3 answers them, and each call recorded in order.
type kimiProbe struct {
	t      *testing.T
	report string
	down   string

	mu     sync.Mutex
	calls  []string
	brief  string
	steers []string
	// reportAtBrief is the report file as the brief was posted.
	reportAtBrief string
	// early is the route's notes while the first turn runs, late those it
	// adds once a `:wait` has returned idle; idled counts those waits.
	early, late []map[string]any
	idled       int
	// steerCodes is KAP's code for each steer, in turn; past the last, 0.
	steerCodes []int
	// activeAsks is how many asks of the prompts route answer a prompt still
	// active, as 2.7.3 does between a steered note's turn and the one it opens.
	activeAsks int
	// home and work are the run's KIMI_CODE_HOME and working directory; a
	// session created there is filed as Kimi files one, its record holding
	// its profile's bind (bind), and the continued session is session_probe.
	home, work, bind string
	prompt           string
	// webDenied is the run's settings' webDenied; disabled is the brief's
	// disabled_tools as posted, nil where the body carried none, and
	// steerDisabled each steer's, "none" where its body carried none.
	webDenied     bool
	disabled      *[]string
	steerDisabled []string
	// idle is sent a value, by each steer, for a `:wait` to return idle; with
	// none sent every wait is held open until its request ends.
	idle chan struct{}
	// servers are the run's MCP servers its settings name, and listed KAP's
	// answer to the guard's listing of them (an envelope's data, or listCode
	// and its message where that is not 0).
	servers  []string
	listed   []map[string]any
	listCode int
	// inactive are the run's servers' tools the session lists inactive, of
	// those the tools route lists (submit_work of each server); config,
	// plugins and manifests KAP's answers naming hooks and tool lists; turns
	// the main agent's turns as its record holds them at idle.
	inactive []string
	config   map[string]any
	plugins  []map[string]any
	manifest map[string]string
	turns    []map[string]any
	// events are the frames the session's socket sends once the brief is
	// posted (briefed), its socket served only where there are any; stderr is
	// the driver's.
	events  []string
	briefed chan struct{}
	stderr  bytes.Buffer
}

// kimiMCPHome is the home's MCP file the probe's run names in its settings.
const kimiMCPHome = "/home/u/.kimi-code/mcp.json"

func (p *kimiProbe) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *kimiProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ok := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": data})
	}
	if r.URL.Path == "/mcp/steer/notes" {
		p.mu.Lock()
		p.calls = append(p.calls, "notes after="+r.URL.Query().Get("after"))
		notes := slices.Clone(p.early)
		if p.idled > 0 {
			notes = append(notes, p.late...)
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"notes": notes})
		return
	}
	if r.Header.Get("Authorization") != "Bearer t" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch path := r.URL.Path; {
	case path == "/api/v1/mcp/servers" && r.Method == http.MethodGet:
		p.record("servers cwd=" + r.URL.Query().Get("cwd"))
		if p.listCode != 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": p.listCode, "msg": "Invalid JSON in /repo/.mcp.json"})
			return
		}
		ok(p.listed)
	case path == "/api/v1/sessions" && r.Method == http.MethodPost:
		p.record("create " + string(body))
		if p.bind == "" {
			ok(map[string]any{"id": "session_probe", "workspace_id": "wd_probe"})
			return
		}
		wire := filepath.Join(p.home, "sessions", "wd_throwaway", "session_throwaway", "agents", "main", "wire.jsonl")
		_ = os.MkdirAll(filepath.Dir(wire), 0o700)
		_ = os.WriteFile(wire, []byte(`{"type":"metadata"}`+"\n"+p.bind+"\n"), 0o600)
		ok(map[string]any{"id": "session_throwaway", "workspace_id": "wd_throwaway"})
	case path == "/api/v1/tools" && r.Method == http.MethodGet:
		p.record("tools")
		var tools []map[string]any
		for _, s := range p.servers {
			name := "mcp__" + s + "__submit_work"
			tools = append(tools, map[string]any{"name": name, "source": "mcp", "active": !slices.Contains(p.inactive, name)})
		}
		ok(map[string]any{"tools": append(tools, map[string]any{"name": "Bash", "source": "builtin", "active": true})})
	case path == "/api/v1/config":
		p.record("config")
		ok(p.config)
	case path == "/api/v1/plugins":
		p.record("plugins")
		ok(map[string]any{"plugins": p.plugins})
	case strings.HasPrefix(path, "/api/v1/plugins/"):
		id := strings.TrimPrefix(path, "/api/v1/plugins/")
		p.record("plugin " + id)
		ok(map[string]any{"id": id, "manifest_path": p.manifest[id]})
	case path == "/api/v1/sessions/session_probe/agents/main/turns":
		p.record("turns")
		ok(map[string]any{"turns": p.turns, "has_more": false})
	case path == "/api/v1/sessions/session_throwaway:delete":
		p.record("delete")
		ok(map[string]any{"deleted": true})
	case path == "/api/v1/sessions/session_probe" && r.Method == http.MethodGet:
		p.record("get")
		ok(map[string]any{"id": "session_probe"})
	case path == "/api/v1/sessions/session_probe/prompts" && r.Method == http.MethodPost:
		var prompt struct {
			ID      string `json:"prompt_id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Disabled *[]string `json:"disabled_tools"`
		}
		_ = json.Unmarshal(body, &prompt)
		report, _ := os.ReadFile(p.report)
		p.mu.Lock()
		recorded := prompt.ID
		if strings.HasPrefix(recorded, "crewlet-brief-") {
			recorded = "crewlet-brief"
		}
		p.calls = append(p.calls, "prompt "+recorded)
		p.brief, p.reportAtBrief, p.prompt, p.disabled = prompt.Content[0].Text, string(report), prompt.ID, prompt.Disabled
		p.mu.Unlock()
		if p.briefed != nil {
			close(p.briefed)
		}
		ok(map[string]any{"prompt_id": prompt.ID, "status": "running"})
	case path == "/api/v1/sessions/session_probe/prompts":
		p.mu.Lock()
		p.calls = append(p.calls, "prompts")
		var active any
		if p.activeAsks > 0 {
			p.activeAsks--
			active = map[string]any{"prompt_id": "crewlet-note-x", "status": "running"}
		}
		p.mu.Unlock()
		ok(map[string]any{"active": active, "queued": []any{}})
	case path == "/api/v1/sessions/session_probe/steer":
		var steer struct {
			ID      string `json:"prompt_id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Disabled *[]string `json:"disabled_tools"`
		}
		_ = json.Unmarshal(body, &steer)
		p.mu.Lock()
		p.calls = append(p.calls, "steer "+steer.ID)
		disabled := "none"
		if steer.Disabled != nil {
			disabled = strings.Join(*steer.Disabled, ",")
		}
		p.steerDisabled = append(p.steerDisabled, disabled)
		p.steers = append(p.steers, steer.ID+" "+steer.Content[0].Text)
		code := 0
		if n := len(p.steers); n <= len(p.steerCodes) {
			code = p.steerCodes[n-1]
		}
		p.mu.Unlock()
		p.idle <- struct{}{}
		if code != 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "prompt_id conflict"})
			return
		}
		ok(map[string]any{"prompt_id": steer.ID, "status": "running"})
	case path == "/api/v1/sessions/session_probe:wait":
		p.record("wait")
		select {
		case <-p.idle:
			p.mu.Lock()
			p.idled++
			p.mu.Unlock()
			ok(map[string]any{"session": map[string]any{"activity": "idle", "last_turn_outcome": "completed"},
				"pending_interactions": []any{}, "timed_out": false})
		case <-r.Context().Done():
		}
	case path == "/api/v1/sessions/session_probe:cancel":
		p.record("cancel")
		ok(map[string]any{})
	case path == "/api/v1/shutdown":
		p.record("shutdown")
		_ = os.WriteFile(p.down, nil, 0o600)
		ok(map[string]any{})
	default:
		p.record("unexpected " + r.Method + " " + path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// kimiDriverRun starts the driver against probe, the brief on its stdin, its
// notes route probe's own where notes is "", and returns the running command
// and its stdout.
func kimiDriverRun(t *testing.T, probe *kimiProbe, notes string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Kimi driver's probe runs the driver under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	srv := httptest.NewServer(probe)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	probe.report, probe.down = filepath.Join(dir, "session-report"), filepath.Join(dir, "down")
	if probe.home == "" {
		probe.home, probe.work = filepath.Join(dir, "home"), filepath.Join(dir, "work")
	}
	if err := os.MkdirAll(probe.work, 0o700); err != nil {
		t.Fatal(err)
	}
	kimi := filepath.Join(dir, "kimi")
	ws := "ws://127.0.0.1:1/api/v1/ws"
	if len(probe.events) > 0 {
		ws = "ws" + strings.TrimPrefix(kimiEventSocket(t, probe), "http") + "/api/v1/ws"
	}
	stub := "#!/bin/sh\nprintf '{\"origin\":\"%s\",\"ws_url\":\"%s\",\"token\":\"t\"," +
		"\"kimi_agent_protocol_version\":\"1.0.0\"}\\n' \"$PROBE_ORIGIN\" \"$PROBE_WS\"\n" +
		"while [ ! -f \"$PROBE_DOWN\" ]; do sleep 0.05; done\n"
	driver := filepath.Join(dir, "kimi-driver.mjs")
	if notes == "" {
		notes = srv.URL + "/mcp/steer"
	}
	settings, _ := json.Marshal(map[string]any{"profile": "crewlet-probe", "notes": notes,
		"report": probe.report, "reportStaging": probe.report + ".next", "serverPid": filepath.Join(dir, "kimi-server.pid"), "kimi": kimi,
		"servers": probe.servers, "mcpConfig": kimiMCPHome, "webDenied": probe.webDenied})
	for name, body := range map[string][]byte{kimi: []byte(stub), driver: []byte(kimiDriverSource),
		filepath.Join(dir, "settings.json"): settings} {
		if err := os.WriteFile(name, body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, append([]string{driver, filepath.Join(dir, "settings.json")}, args...)...) //nolint:gosec // the suite's own probe
	cmd.Dir = probe.work
	cmd.Env = append(os.Environ(), "PROBE_ORIGIN="+srv.URL, "PROBE_WS="+ws, "PROBE_DOWN="+probe.down,
		"NODE_NO_WARNINGS=1", "KIMI_CODE_HOME="+probe.home)
	cmd.Stderr = &probe.stderr
	cmd.Stdin = strings.NewReader("Fix the flake.\n$(not run)")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, &stdout
}

// kimiDriverEnds waits for the driver to exit and returns its status.
func kimiDriverEnds(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return cmd.ProcessState.ExitCode()
	case <-time.After(60 * time.Second): //fixedwait:liveness
		t.Fatal("the driver did not end within 60s")
		return 0
	}
}

// kimiNote is a note as the bridge's route answers it.
func kimiNote(seq int, inbox, message string) map[string]any {
	return map[string]any{"seq": seq, "id": "n" + inbox, "inbox": inbox, "message": message}
}

// THE DRIVER REPORTS THE SESSION ONCE, BEFORE THE BRIEF, POSTS THE BRIEF IN A
// BODY, AND STEERS EACH NOTE UNDER ITS INBOX ID: a note while the turn runs, a
// conflict (40927) taken as admitted, a note waiting at idle steered to keep
// the run going, the route's /read never posted, and the server shut down as
// the run ends, exiting 0 on a completed turn.
//
// Mutation: write the report after the brief, steer under another prompt_id,
// re-steer on a conflict, skip the idle ask, or post /read — the case fails.
func TestTheKimiDriverReportsOnceAndSteersEachNoteUnderItsInboxID(t *testing.T) {
	probe := &kimiProbe{t: t, idle: make(chan struct{}, 4),
		early:      []map[string]any{kimiNote(1, "inbox1", "## A note from ada\n\nTarget main.")},
		late:       []map[string]any{kimiNote(2, "inbox2", "## A note from bob\n\nAlso docs.")},
		steerCodes: []int{0, 40927}}
	cmd, stdout := kimiDriverRun(t, probe, "")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.reportAtBrief != "session_probe\n" {
		t.Errorf("the report as the brief was posted = %q, want the session's id, whole", probe.reportAtBrief)
	}
	if probe.brief != "Fix the flake.\n$(not run)" {
		t.Errorf("the brief posted = %q, want stdin byte for byte", probe.brief)
	}
	if !slices.Equal(probe.steers, []string{"crewlet-note-inbox1 ## A note from ada\n\nTarget main.",
		"crewlet-note-inbox2 ## A note from bob\n\nAlso docs."}) {
		t.Errorf("steers = %q, want each note once under its inbox id", probe.steers)
	}
	create := probe.calls[0]
	if !strings.HasPrefix(create, "create ") || !strings.Contains(create, `"permission_mode":"auto"`) ||
		!strings.Contains(create, `"profile":"crewlet-probe"`) {
		t.Errorf("the session was created with %s", create)
	}
	if probe.calls[1] != "prompt crewlet-brief" || !strings.HasPrefix(probe.prompt, "crewlet-brief-") ||
		len(probe.prompt) != len("crewlet-brief-")+36 {
		t.Errorf("calls = %q, want the brief right after the session", probe.calls)
	}
	if !slices.Contains(probe.calls, "shutdown") || slices.ContainsFunc(probe.calls, func(c string) bool {
		return strings.HasPrefix(c, "unexpected")
	}) {
		t.Errorf("calls = %q, want a shutdown and nothing else", probe.calls)
	}
	if !strings.Contains(stdout.String(), `{"type":"crewlet.end","asked":false,"code":0}`) {
		t.Errorf("stdout lacks the driver's end:\n%s", stdout)
	}
}

// A WEB-DENIED RUN'S BRIEF AND NOTES TURN KIMI'S WEB TOOLS OFF FOR THE WHOLE
// SESSION: FetchURL and WebSearch posted as the disabled_tools of the brief and
// of every steered note, which every agent of the session is held to, a
// subagent included, a note's turn at idle too (measured on 2.7.3); any other
// run posts an empty list, so a session a denied run carried gets its web back.
//
// Mutation: post the list on every run, post none on a run that keeps the web,
// leave a web tool off it, or post none with a note — the case fails.
func TestAWebDeniedKimiRunsBriefDisablesTheWebToolsForTheSession(t *testing.T) {
	for _, denied := range []bool{true, false} {
		probe := &kimiProbe{t: t, idle: make(chan struct{}, 2), webDenied: denied,
			early: []map[string]any{kimiNote(1, "inbox1", "## A note from ada\n\nTarget main.")}}
		probe.idle <- struct{}{}
		cmd, stdout := kimiDriverRun(t, probe, "")
		if code := kimiDriverEnds(t, cmd); code != 0 {
			t.Fatalf("denied %v: the driver exited %d, want 0:\n%s", denied, code, stdout)
		}
		want := []string{}
		if denied {
			want = kimiWebTools
		}
		probe.mu.Lock()
		switch {
		case probe.disabled == nil:
			t.Errorf("denied %v: the brief carried no disabled_tools, want %q", denied, want)
		case !slices.Equal(*probe.disabled, want):
			t.Errorf("denied %v: the brief's disabled_tools = %q, want %q", denied, *probe.disabled, want)
		}
		if note := strings.Join(want, ","); !slices.Equal(probe.steerDisabled, []string{note}) {
			t.Errorf("denied %v: the steered notes' disabled_tools = %q, want one note with %q", denied,
				probe.steerDisabled, note)
		}
		probe.mu.Unlock()
	}
}

// AN ASKED END CANCELS THE TURN AND STOPS THE SERVER: SIGTERM while the session
// works is a `:cancel`, a shutdown, `crewlet.end` with asked, and exit 143.
//
// Mutation: exit on SIGTERM without the cancel — the case fails.
func TestTheKimiDriverEndsOnRequestByCancellingTheTurn(t *testing.T) {
	probe := &kimiProbe{t: t, idle: make(chan struct{})}
	cmd, stdout := kimiDriverRun(t, probe, "")
	for {
		probe.mu.Lock()
		waiting := slices.Contains(probe.calls, "wait")
		probe.mu.Unlock()
		if waiting {
			break
		}
		<-time.After(20 * time.Millisecond) //fixedwait:liveness
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := kimiDriverEnds(t, cmd); code != 143 {
		t.Fatalf("the driver exited %d, want 143:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !slices.Contains(probe.calls, "cancel") || !slices.Contains(probe.calls, "shutdown") {
		t.Errorf("calls = %q, want the turn cancelled and the server stopped", probe.calls)
	}
	if !strings.Contains(stdout.String(), `{"type":"crewlet.end","asked":true,"code":143}`) {
		t.Errorf("stdout lacks the asked end:\n%s", stdout)
	}
}

// A SESSION THAT READS IDLE WITH A PROMPT STILL ACTIVE IS NOT DONE: the driver
// waits on it again, since ending there cancelled a steered note's turn
// (measured on 2.7.3), and ends only once no prompt is active.
//
// Mutation: end at the first idle `:wait` (drop settled) — one wait, and the
// case fails.
func TestTheKimiDriverWaitsOutAPromptStillActiveAtIdle(t *testing.T) {
	probe := &kimiProbe{t: t, idle: make(chan struct{}, 2), activeAsks: 1}
	probe.idle <- struct{}{}
	probe.idle <- struct{}{}
	cmd, stdout := kimiDriverRun(t, probe, "")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	waits := 0
	for _, c := range probe.calls {
		if c == "wait" {
			waits++
		}
	}
	if waits != 2 {
		t.Fatalf("calls = %q, want a second wait after the prompt still active", probe.calls)
	}
}

// kimiFiled files a session_probe for a run in probe's directory through the
// driver's own rekey, as the run's script does, and returns its main record.
func kimiFiled(t *testing.T, probe *kimiProbe) string {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Kimi driver's probe runs the driver under it", "install any current node")
	dir := t.TempDir()
	probe.home, probe.work = filepath.Join(dir, "home"), filepath.Join(dir, "work")
	src := filepath.Join(dir, "carried", "session_probe")
	for name, body := range kimiSessionFiles("session_probe", "/elsewhere") {
		kimiWrite(t, filepath.Join(src, name), body)
	}
	kimiWrite(t, filepath.Join(probe.home, "agents", "crewlet", "crewlet-probe.md"),
		"---\nname: crewlet-probe\n---\n${base_prompt}\n\nSTANDING-NOW\n")
	settings := filepath.Join(dir, "s.json")
	kimiWrite(t, settings, `{}`)
	driver := filepath.Join(dir, "d.mjs")
	kimiWrite(t, driver, kimiDriverSource)
	if err := os.MkdirAll(probe.work, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, driver, settings, "rekey", "session_probe", src) //nolint:gosec // the suite's own probe
	cmd.Dir, cmd.Env = probe.work, append(os.Environ(), "KIMI_CODE_HOME="+probe.home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rekey: %v\n%s", err, out)
	}
	wires, _ := filepath.Glob(filepath.Join(probe.home, "sessions", "wd_work_*", "session_probe", "agents", "main", "wire.jsonl"))
	if len(wires) != 1 {
		t.Fatalf("the session was filed at %v", wires)
	}
	return wires[0]
}

// A CONTINUED SESSION'S INSTRUCTIONS ARE RENEWED BY ITS BIND, AND IT REPORTS
// NOTHING: the driver creates a throwaway session with the run's profile in the
// run's directory, appends its bind to the continued session's record, deletes
// it, opens the continued session and posts the brief there as it is; no
// report is written, as a resume names exactly the session it continues.
//
// Mutation: skip the renewal, keep the throwaway, write a report on a resume,
// or post to another session — the case fails.
func TestAContinuedKimiSessionIsRenewedByItsBindAndReportsNothing(t *testing.T) {
	const bind = `{"type":"profile.bind","agentId":"main","profileName":"crewlet-probe","systemPrompt":"RENDERED-NOW"}`
	probe := &kimiProbe{t: t, idle: make(chan struct{}, 1), bind: bind}
	wire := kimiFiled(t, probe)
	probe.idle <- struct{}{}
	cmd, stdout := kimiDriverRun(t, probe, "", "resume", "session_probe")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	record, _ := os.ReadFile(wire)
	if !strings.HasSuffix(string(record), bind+"\n") {
		t.Errorf("the continued record ends %q, want the fresh bind", record)
	}
	order := []string{}
	for _, c := range probe.calls {
		if strings.HasPrefix(c, "create") || c == "delete" || c == "get" || strings.HasPrefix(c, "prompt ") {
			order = append(order, strings.SplitN(c, " ", 2)[0])
		}
	}
	if !slices.Equal(order, []string{"create", "delete", "get", "prompt"}) {
		t.Errorf("calls = %q, want the throwaway created and deleted, then the session opened and prompted", probe.calls)
	}
	if probe.brief != "Fix the flake.\n$(not run)" || probe.reportAtBrief != "" {
		t.Errorf("brief %q, report %q; want the brief as it is and no report", probe.brief, probe.reportAtBrief)
	}
	if _, err := os.Stat(probe.report); !os.IsNotExist(err) {
		t.Errorf("a resume wrote a report (%v)", err)
	}
}

// kimiEventSocket serves the session's socket as KAP does, by the WebSocket
// handshake, and sends probe's events once the brief is posted, holding the
// connection open until the case ends. Its address is returned.
func kimiEventSocket(t *testing.T, probe *kimiProbe) string {
	t.Helper()
	probe.briefed = make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // the protocol's own
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept[:]) + "\r\n\r\n")
		_ = buf.Flush()
		<-probe.briefed
		for _, event := range probe.events {
			frame := []byte{0x81, byte(len(event))}
			if len(event) >= 126 {
				frame = []byte{0x81, 126, byte(len(event) >> 8), byte(len(event))}
			}
			_, _ = conn.Write(append(frame, event...))
		}
		<-t.Context().Done()
	}))
	t.Cleanup(srv.CloseClientConnections)
	t.Cleanup(srv.Close)
	return srv.URL
}

// THE GUARD REFUSES A RUN WHOSE OWN SERVER A REPOSITORY DISPLACES, before any
// session is made: Kimi, asked where each server it loads for the run's
// directory comes from, names a repository's file for one of the run's
// servers, or cannot load them, its error naming the file; a run whose
// servers are the home file's goes on (measured on 2.7.2).
//
// Mutation: drop the guard, or compare the origin to nothing: a case fails.
func TestTheKimiGuardRefusesARunARepositoryDisplacesTheServersOf(t *testing.T) {
	for _, c := range []struct {
		name     string
		listed   []map[string]any
		listCode int
		want     string
	}{
		{"a repository's server by the run's name", []map[string]any{
			{"name": "crewlet", "origin": "/repo/.kimi-code/mcp.json"}, {"name": "home-mcp", "origin": kimiMCPHome}},
			0, "/repo/.kimi-code/mcp.json names an MCP server crewlet"},
		{"a repository file Kimi cannot read", nil, 40001, "Invalid JSON in /repo/.mcp.json"},
		{"none of the run's servers", []map[string]any{{"name": "home-mcp", "origin": kimiMCPHome}}, 0,
			"did not load the MCP server crewlet"},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := &kimiProbe{t: t, idle: make(chan struct{}), servers: []string{"crewlet"}, listed: c.listed,
				listCode: c.listCode}
			cmd, stdout := kimiDriverRun(t, probe, "")
			if code := kimiDriverEnds(t, cmd); code != 1 {
				t.Fatalf("the driver exited %d, want 1:\n%s", code, stdout)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if !strings.Contains(probe.stderr.String(), c.want) {
				t.Errorf("the driver said %q, want %q", probe.stderr.String(), c.want)
			}
			if len(probe.calls) == 0 || !strings.HasPrefix(probe.calls[0], "servers cwd=") ||
				slices.ContainsFunc(probe.calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
				t.Errorf("calls = %q, want the listing alone, no session made", probe.calls)
			}
		})
	}
	t.Run("the home's own servers", func(t *testing.T) {
		probe := &kimiProbe{t: t, idle: make(chan struct{}, 1), servers: []string{"crewlet"},
			listed: []map[string]any{{"name": "crewlet", "origin": kimiMCPHome}, {"name": "repo", "origin": "/repo/.mcp.json"}}}
		probe.idle <- struct{}{}
		cmd, stdout := kimiDriverRun(t, probe, "")
		if code := kimiDriverEnds(t, cmd); code != 0 {
			t.Fatalf("the driver exited %d, want 0:\n%s%s", code, stdout, &probe.stderr)
		}
	})
}

// A CALL OF THE RUN'S OWN TOOL THAT KIMI DENIES BY A PERMISSION RULE ENDS THE
// RUN: no rule the run could add overrides a deny (2.7.2 decides every deny
// first), so the turn is cancelled and the run fails naming Kimi's message; a
// denied call of another server's tool is the session's own affair.
//
// Mutation: drop the event's check, or match any server's tool: a case fails.
func TestAKimiRunEndsWhereKimiDeniesItsOwnTool(t *testing.T) {
	denied := func(tool string) string {
		return `{"type":"item.ended","item":{"type":"tool_call","name":"` + tool +
			`","outcome":"failed","error":"Tool \"` + tool + `\" was denied by permission rule."}}`
	}
	probe := &kimiProbe{t: t, idle: make(chan struct{}), servers: []string{"crewlet"},
		listed: []map[string]any{{"name": "crewlet", "origin": kimiMCPHome}},
		events: []string{denied("mcp__crewlet-other__x"), denied("mcp__crewlet__report")}}
	cmd, stdout := kimiDriverRun(t, probe, "")
	if code := kimiDriverEnds(t, cmd); code != 1 {
		t.Fatalf("the driver exited %d, want 1:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if got := probe.stderr.String(); !strings.Contains(got, "mcp__crewlet__report by a permission rule") ||
		strings.Contains(got, "crewlet-other") {
		t.Errorf("the driver said %q, want the run's own tool's denial alone", got)
	}
	if !slices.Contains(probe.calls, "cancel") || !slices.Contains(probe.calls, "shutdown") {
		t.Errorf("calls = %q, want the turn cancelled and the server stopped", probe.calls)
	}
}

// A NOTE A HOOK KEEPS FROM THE MODEL ENDS THE RUN as the session goes idle:
// Kimi's record of the main agent's turns holds the note's turn ended
// `blocked` before any step, as a UserPromptSubmit hook ends it (measured on
// 2.7.2), and the run fails naming the note and each such hook config.toml
// lists whose matcher matches its text; a blocked turn of a prompt this run
// did not post is not the run's.
//
// Mutation: skip the turns check, take any prompt as the run's, name a hook
// whose matcher does not match, or drop the stream's refusal line: a case
// fails.
func TestAKimiRunEndsWhereAHookKeepsANoteFromTheModel(t *testing.T) {
	blocked := func(prompt string) []map[string]any {
		return []map[string]any{{"turn_id": "t_2", "end_reason": "blocked", "steps": []any{}, "items": []map[string]any{
			{"type": "user_message", "item_id": prompt, "text": "ship it"},
			{"type": "assistant_message", "text": "<hook_result hook_event=\"UserPromptSubmit\">\nno notes\n</hook_result>"}}}}
	}
	hooks := map[string]any{"hooks": []any{
		map[string]any{"event": "UserPromptSubmit", "matcher": "^ship", "command": "exit 2"},
		map[string]any{"event": "UserPromptSubmit", "matcher": "^never", "command": "exit 2"}}}
	for _, c := range []struct {
		name  string
		turns []map[string]any
		code  int
	}{
		{"the run's own note", blocked(KimiNoteID("a")), 1},
		{"a prompt the run did not post", blocked(KimiNoteID("other")), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := &kimiProbe{t: t, idle: make(chan struct{}, 1), early: []map[string]any{kimiNote(1, "a", "ship it")},
				turns: c.turns, config: hooks}
			cmd, stdout := kimiDriverRun(t, probe, "")
			if code := kimiDriverEnds(t, cmd); code != c.code {
				t.Fatalf("the driver exited %d, want %d:\n%s%s", code, c.code, stdout, &probe.stderr)
			}
			if c.code == 0 {
				return
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			want := []string{"Kimi kept the note a from the model: a UserPromptSubmit hook blocked it (no notes)",
				filepath.Join(probe.home, "config.toml") + `: its [[hooks]] entry event UserPromptSubmit, matcher "^ship"`}
			for _, w := range want {
				if !strings.Contains(probe.stderr.String(), w) {
					t.Errorf("the driver said %q, want %q", probe.stderr.String(), w)
				}
			}
			if strings.Contains(probe.stderr.String(), "^never") || !strings.Contains(stdout.String(), `"type":"crewlet.refused"`) {
				t.Errorf("the driver named a hook that does not match, or put no refusal on the stream:\n%s%s", stdout, &probe.stderr)
			}
		})
	}
}
