package executor

import (
	"bytes"
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
// 2.7.2 answers them, and each call recorded in order.
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
	// active, as 2.7.2 does between a steered note's turn and the one it opens.
	activeAsks int
	// idle is sent a value, by each steer, for a `:wait` to return idle; with
	// none sent every wait is held open until its request ends.
	idle chan struct{}
}

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
	case path == "/api/v1/sessions" && r.Method == http.MethodPost:
		p.record("create " + string(body))
		ok(map[string]any{"id": "session_probe"})
	case path == "/api/v1/sessions/session_probe/prompts" && r.Method == http.MethodPost:
		var prompt struct {
			ID      string `json:"prompt_id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(body, &prompt)
		report, _ := os.ReadFile(p.report)
		p.mu.Lock()
		p.calls = append(p.calls, "prompt "+prompt.ID)
		p.brief, p.reportAtBrief = prompt.Content[0].Text, string(report)
		p.mu.Unlock()
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
		}
		_ = json.Unmarshal(body, &steer)
		p.mu.Lock()
		p.calls = append(p.calls, "steer "+steer.ID)
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
func kimiDriverRun(t *testing.T, probe *kimiProbe, notes string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "the Kimi driver's probe runs the driver under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	srv := httptest.NewServer(probe)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	probe.report, probe.down = filepath.Join(dir, "session-report"), filepath.Join(dir, "down")
	kimi := filepath.Join(dir, "kimi")
	stub := "#!/bin/sh\nprintf '{\"origin\":\"%s\",\"ws_url\":\"ws://127.0.0.1:1/api/v1/ws\",\"token\":\"t\"," +
		"\"kimi_agent_protocol_version\":\"1.0.0\"}\\n' \"$PROBE_ORIGIN\"\n" +
		"while [ ! -f \"$PROBE_DOWN\" ]; do sleep 0.05; done\n"
	driver := filepath.Join(dir, "kimi-driver.mjs")
	if notes == "" {
		notes = srv.URL + "/mcp/steer"
	}
	settings, _ := json.Marshal(map[string]any{"profile": "crewlet-probe", "notes": notes,
		"report": probe.report, "serverPid": filepath.Join(dir, "kimi-server.pid"), "kimi": kimi})
	for name, body := range map[string][]byte{kimi: []byte(stub), driver: []byte(kimiDriverSource),
		filepath.Join(dir, "settings.json"): settings} {
		if err := os.WriteFile(name, body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, driver, filepath.Join(dir, "settings.json")) //nolint:gosec // the suite's own probe
	cmd.Env = append(os.Environ(), "PROBE_ORIGIN="+srv.URL, "PROBE_DOWN="+probe.down, "NODE_NO_WARNINGS=1")
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
	if probe.calls[1] != "prompt crewlet-brief" {
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
		select {
		case <-time.After(20 * time.Millisecond): //fixedwait:liveness
		}
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
// (measured on 2.7.2), and ends only once no prompt is active.
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
