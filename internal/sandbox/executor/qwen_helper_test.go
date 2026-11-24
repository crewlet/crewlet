package executor_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// nodeForQwen is the node a Qwen Code box's helper is run under here.
func nodeForQwen(t *testing.T) string {
	t.Helper()
	return prereqtest.OnPath(t, "node", "a Qwen Code run's helper and the stand-in it drives run under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
}

// qwenCapturedSession is the session the 0.28.4 note capture's probe minted,
// which every helper run here creates, so the replayed messages name it.
const qwenCapturedSession = "11111111-2222-4333-8444-000000000001"

// qwenNote is one note the stand-in bridge serves; After, where set, holds it
// back until the note of that id is confirmed read, so it reaches a later
// drain than that note.
type qwenNote struct {
	Seq     int    `json:"seq"`
	ID      string `json:"id"`
	Inbox   string `json:"inbox"`
	Message string `json:"message"`
	After   string `json:"-"`
	// Later serves the note only to a poll past every note before it, so it
	// reaches a later drain than they do whatever becomes of them.
	Later bool `json:"-"`
}

// qwenBridge is a stand-in for the run's notes and skill routes: it serves
// every note not yet read past the poll's cursor, at once, and records each
// read and each skill report in order.
type qwenBridge struct {
	mu     sync.Mutex
	notes  []qwenNote
	reads  []string
	skills []string
	srv    *httptest.Server
}

func newQwenBridge(t *testing.T, notes ...qwenNote) *qwenBridge {
	t.Helper()
	b := &qwenBridge{notes: notes}
	b.srv = httptest.NewServer(b)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *qwenBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/steer/notes":
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		served := []qwenNote{}
		for _, n := range b.notes {
			if n.Seq > after && !slices.Contains(b.reads, n.ID) && (n.After == "" || slices.Contains(b.reads, n.After)) &&
				(!n.Later || after >= n.Seq-1) {
				served = append(served, n)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"notes": served})
	case r.Method == http.MethodPost && r.URL.Path == "/steer/read":
		var read struct {
			IDs []string `json:"ids"`
		}
		_ = json.Unmarshal(body, &read)
		b.reads = append(b.reads, read.IDs...)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/skills":
		b.skills = append(b.skills, string(body))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *qwenBridge) read() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.reads)
}

// qwenHelperRun is one run of the helper against the stand-in Qwen Code.
type qwenHelperRun struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	log    string
	home   string
	cwd    string
}

// startQwenHelper starts the helper as a run's script does for a new
// session, its `qwen` the stand-in replaying turn, its configuration config
// with the notes route filled in, its environment env beside the stand-in's
// own.
func startQwenHelper(t *testing.T, notes string, turn string, config map[string]any,
	env ...string) *qwenHelperRun {
	t.Helper()
	return startQwenHelperAs(t, "new", nil, notes, turn, config, env...)
}

// startQwenHelperAs is [startQwenHelper] in mode (`new` or `load`), with the
// session's chat file holding held, where it is not nil, under the run's key.
func startQwenHelperAs(t *testing.T, mode string, held []byte, notes string, turn string,
	config map[string]any, env ...string) *qwenHelperRun {
	t.Helper()
	node := nodeForQwen(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	standin, err := filepath.Abs("qwen_acp_standin.mjs")
	if err != nil {
		t.Fatal(err)
	}
	writeStandIn(t, filepath.Join(bin, "qwen"), "#!/bin/sh\nexec "+shellword.Quote(node)+" "+shellword.Quote(standin)+` "$@"`+"\n")
	captures, err := filepath.Abs(filepath.Join("testdata", "qwen", "0.28.4"))
	if err != nil {
		t.Fatal(err)
	}
	run := &qwenHelperRun{log: filepath.Join(dir, "standin.log"), home: filepath.Join(dir, "qwen-home"),
		cwd: filepath.Join(dir, "work")}
	if err := os.MkdirAll(run.cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if held != nil {
		real, err := filepath.EvalSymlinks(run.cwd)
		if err != nil {
			t.Fatal(err)
		}
		chat := filepath.Join(run.home, "projects", qwenKey(real), "chats", qwenCapturedSession+".jsonl")
		if err := os.MkdirAll(filepath.Dir(chat), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(chat, held, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	full := map[string]any{"authType": "openai", "qwenHome": run.home, "servers": []string{"probe"},
		"bridge": "probe", "mcpWaitMs": 60000, "notes": notes, "namespace": "crewlet"}
	for k, v := range config {
		full[k] = v
	}
	raw, _ := json.Marshal(full)
	configPath := filepath.Join(dir, "qwen-helper.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("qwen_helper.mjs")
	if err != nil {
		t.Fatal(err)
	}
	run.cmd = exec.CommandContext(t.Context(), node, helper, "run", configPath, qwenCapturedSession, mode)
	run.cmd.Dir = run.cwd
	run.cmd.Stdin = strings.NewReader("Run the probe tools.")
	run.cmd.Stderr = os.Stderr
	run.cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"QWEN_FAKE_START="+filepath.Join(captures, "acp-wsmcp.jsonl"),
		"QWEN_FAKE_TURN="+filepath.Join(captures, turn), "QWEN_FAKE_HOME="+run.home, "QWEN_FAKE_LOG="+run.log)
	run.cmd.Env = append(run.cmd.Env, env...)
	if run.stdout, err = run.cmd.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	if err := run.cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	return run
}

// finish reads the helper's stream to its end and its exit status.
func (r *qwenHelperRun) finish(t *testing.T) (lines []map[string]any, code int) {
	t.Helper()
	scanner := bufio.NewScanner(r.stdout)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		lines = append(lines, decodeLine(t, scanner.Bytes()))
	}
	err := r.cmd.Wait()
	code = r.cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		t.Fatalf("the helper: %v", err)
	}
	return lines, code
}

func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("the helper wrote a line that is no JSON: %q", line)
	}
	return m
}

// standinSaw is the stand-in's log: what it was asked and handed.
func (r *qwenHelperRun) standinSaw(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(r.log)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		out = append(out, decodeLine(t, l))
	}
	return out
}

// typed is the helper's own lines of type kind.
func typed(lines []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == kind {
			out = append(out, l)
		}
	}
	return out
}

// steer is the notes route of the stand-in bridge.
func (b *qwenBridge) steer() string { return b.srv.URL + "/steer" }

// A QWEN RUN'S HELPER RUNS ONE TURN AND CONFIRMS EACH NOTE ONLY BY ITS RECORD,
// TWO IDENTICAL NOTES AS TWO. Against Qwen Code 0.28.4's own turn replayed
// (two tool batches, a drain after each), the helper waits for the bridge to
// connect before the prompt; answers every drain at once from what it
// polled, every note it holds; writes every session/update, a line for each
// note handed, the window and the turn's end; and confirms each note once the
// chat file records it and the response after it, in order.
//
// Mutations, each red: send the prompt before the bridge connects (the
// stand-in's first status says disconnected); hand the notes newest first;
// answer a drain only after a wait of its own.
// The note contract's confirm case for Qwen Code (notecontract_test.go).
func qwenHelperConfirmsEachNoteByItsRecord(t *testing.T) {
	t.Parallel()
	const text = "## A note from eve, sent while this turn was running\nuse staging"
	bridge := newQwenBridge(t, qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: text},
		qwenNote{Seq: 2, ID: "n-2", Inbox: "qwen_2", Message: text})
	run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1")
	lines, code := run.finish(t)
	if code != 0 {
		t.Fatalf("the helper exited %d, want 0", code)
	}
	if end := typed(lines, "crewlet_qwen_end"); len(end) != 1 || end[0]["stop_reason"] != "end_turn" {
		t.Errorf("the turn ended %v, want one end at end_turn", end)
	}
	if notes := typed(lines, "crewlet_qwen_note"); len(notes) != 2 || notes[0]["id"] != "n-1" || notes[1]["id"] != "n-2" {
		t.Errorf("the notes handed read %v, want n-1 then n-2", notes)
	}
	if w := typed(lines, "crewlet_qwen_window"); len(w) != 1 || w[0]["context_window"] != float64(200000) {
		t.Errorf("the window lines read %v, want 200000", w)
	}
	updates := 0
	for _, l := range lines {
		if l["method"] == "session/update" {
			updates++
		}
	}
	if updates < 20 {
		t.Errorf("the stream holds %d session/update lines, want the turn's every one", updates)
	}
	if got := bridge.read(); !slices.Equal(got, []string{"n-1", "n-2"}) {
		t.Errorf("the notes confirmed read are %v, want n-1 then n-2", got)
	}
	var methods []string
	for _, e := range run.standinSaw(t) {
		if m, ok := e["method"].(string); ok {
			methods = append(methods, m)
		}
		if ms, ok := e["ms"].(float64); ok && ms >= 2000 {
			t.Errorf("a drain was answered after %v ms, past the 2 s Qwen Code gives it", ms)
		}
	}
	prompt, connected := slices.Index(methods, "session/prompt"), 0
	for _, m := range methods[:max(prompt, 0)] {
		if m == "qwen/status/workspace/mcp" {
			connected++
		}
	}
	if prompt < 0 || connected < 2 {
		t.Errorf("the helper asked %v, want the MCP status until connected before session/prompt", methods)
	}
}

// A NOTE QWEN CODE NEVER RECORDED IS SERVED AGAIN, NEVER LOST. One whose
// harness died between the drain and the record is never confirmed, so the
// engine serves it again, and the next run hands and confirms it.
//
// Mutations, each red: confirm a note as it is handed (the first run
// confirms what no session holds); confirm what was handed at the end
// whatever the chat file shows.
func TestANoteQwenNeverRecordedIsServedAgainNeverLost(t *testing.T) {
	t.Parallel()
	note := qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: "## A note from eve, sent while this turn was running\nx"}
	bridge := newQwenBridge(t, note)
	first := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1", "QWEN_FAKE_DIE_AFTER_NOTE=1")
	lines, code := first.finish(t)
	if code == 0 || len(typed(lines, "crewlet_qwen_note")) != 1 {
		t.Fatalf("the first run exited %d having handed %v, want a failure after handing the note",
			code, typed(lines, "crewlet_qwen_note"))
	}
	if got := bridge.read(); len(got) != 0 {
		t.Fatalf("a note no session recorded was confirmed read: %v", got)
	}
	second := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1")
	if _, code := second.finish(t); code != 0 {
		t.Fatalf("the second run exited %d", code)
	}
	if got := bridge.read(); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("after the second run the reads are %v, want n-1 once", got)
	}
}

// A NOTE WHOSE REQUEST FAILED IS SERVED AGAIN, NEVER LOST. Qwen Code 0.28.4
// records a drained note before the request carrying it goes out; when that
// request fails (a 429 past its retries, a context overflow, a cancel), no
// reply follows the record and the model never read the note. Qwen Code never
// continues a session, so the helper confirms nothing of it, the engine
// serves it again, and the next run hands and confirms it.
//
// Mutations, each red: confirm a match at stop whatever followed it; confirm
// a note at its record, before the reply after it.
func TestAQwenNoteWhoseRequestFailedIsServedAgain(t *testing.T) {
	t.Parallel()
	note := qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: "## A note from eve, sent while this turn was running\nx"}
	bridge := newQwenBridge(t, note)
	first := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1",
		"QWEN_FAKE_FAIL_AFTER_NOTE=1")
	lines, code := first.finish(t)
	if code == 0 || len(typed(lines, "crewlet_qwen_note")) != 1 {
		t.Fatalf("the first run exited %d having handed %v, want a failure after handing the note",
			code, typed(lines, "crewlet_qwen_note"))
	}
	if got := bridge.read(); len(got) != 0 {
		t.Fatalf("a note whose request failed was confirmed read: %v", got)
	}
	second := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1")
	if _, code := second.finish(t); code != 0 {
		t.Fatalf("the second run exited %d", code)
	}
	if got := bridge.read(); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("after the second run the reads are %v, want n-1 once", got)
	}
}

// OF TWO IDENTICAL QWEN NOTES, ONLY THE ONE A REPLY FOLLOWS IS CONFIRMED. Two
// notes of one text reach two drains; the request carrying the first is
// answered, the one carrying the second fails after its record. Each record
// matches its own note in order, and only the first has a reply after it, so
// exactly one is confirmed and the other is served again.
//
// Mutations, each red: confirm a match at stop whatever followed it; confirm
// a note at its record; settle every match on any reply, the second's record
// counted against the first's reply.
func TestOfTwoIdenticalQwenNotesOnlyTheRepliedOneIsConfirmed(t *testing.T) {
	t.Parallel()
	const text = "## A note from eve, sent while this turn was running\nuse staging"
	bridge := newQwenBridge(t, qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: text},
		qwenNote{Seq: 2, ID: "n-2", Inbox: "qwen_2", Message: text, After: "n-1"})
	run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=2",
		"QWEN_FAKE_FAIL_AFTER_NOTE=2")
	lines, code := run.finish(t)
	if notes := typed(lines, "crewlet_qwen_note"); code == 0 || len(notes) != 2 {
		t.Fatalf("the run exited %d having handed %v, want both notes handed and a failure", code, notes)
	}
	if got := bridge.read(); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("the notes confirmed read are %v, want n-1 alone", got)
	}
}

// A QWEN NOTE NEVER RECORDED LEAVES THE LATER ONES TO BE CONFIRMED. The
// first note's drain records nothing (the stand-in goes on as though it had),
// the second's records it and is answered: the helper passes the first over,
// unconfirmed and served again, and confirms the second.
//
// Mutation: match a record against the oldest note handed alone, and the
// second is never confirmed.
func TestAQwenNoteNeverRecordedLeavesTheLaterOnesToBeConfirmed(t *testing.T) {
	t.Parallel()
	bridge := newQwenBridge(t,
		qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: "## A note from eve, sent while this turn was running\nfirst"},
		qwenNote{Seq: 2, ID: "n-2", Inbox: "qwen_2", Message: "## A note from eve, sent while this turn was running\nsecond",
			Later: true})
	run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=2",
		"QWEN_FAKE_UNRECORDED_NOTE=1")
	lines, code := run.finish(t)
	if notes := typed(lines, "crewlet_qwen_note"); code != 0 || len(notes) != 2 {
		t.Fatalf("the run exited %d having handed %v, want both notes handed", code, notes)
	}
	if got := bridge.read(); !slices.Equal(got, []string{"n-2"}) {
		t.Errorf("the notes confirmed read are %v, want n-2 alone", got)
	}
}

// A NOTE A COMPACTION KEEPS ONLY IN ITS SUMMARY IS NOT CONFIRMED (ADR-0177).
// The stand-in records the note, then Qwen Code 0.28.4's own chat_compression
// record of a note drained at the compaction point (chat-compact.jsonl), its
// note the case's, then the response after it. Kept plain past the prefix,
// behind another entry or not, the note is confirmed; moved into the summary
// entry, kept in a model entry, under a wider contextPrefixLength, or behind
// a line that does not parse, it is not, and the engine serves it again.
//
// Mutations, each red: pass compaction records over; read the tail from 0;
// confirm a note at its record; count a model entry; ignore
// contextPrefixLength; read only the first entry past the prefix; keep matches
// past a line that does not parse.
func TestAQwenNoteACompactionOnlySummarizedIsNotConfirmed(t *testing.T) {
	t.Parallel()
	const marker = "MARK-CN-START note at the compaction point MARK-CN-END"
	var compaction string
	for _, l := range strings.Split(string(qwenFixture(t, "0.28.4", "chat-compact.jsonl")), "\n") {
		if strings.Contains(l, `"subtype":"chat_compression"`) {
			compaction = l
		}
	}
	tail := `,{"text":"\n[User message received during tool execution]: ` + marker + `"}`
	const summary = `"parts":[{"text":"<state_snapshot>`
	if !strings.Contains(compaction, tail) || !strings.Contains(compaction, summary) {
		t.Fatal("the capture holds no compaction keeping its note plain past its summary")
	}
	note := qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: "## A note from eve, sent while this turn was running\nx"}
	quoted := strings.Trim(string(mustJSON(t, note.Message)), `"`)
	summarized := strings.Replace(strings.Replace(compaction, tail, "", 1),
		summary, `"parts":[{"text":"`+marker+`"},{"text":"<state_snapshot>`, 1)
	const noteEntry = `{"role":"user","parts":[{"functionResponse"`
	modelTail := strings.Replace(compaction, noteEntry, `{"role":"model","parts":[{"functionResponse"`, 1)
	behind := strings.Replace(compaction, noteEntry, `{"role":"user","parts":[{"text":"another entry"}]},`+noteEntry, 1)
	wider := strings.Replace(compaction, `"contextPrefixLength":2`, `"contextPrefixLength":3`, 1)
	if modelTail == compaction || behind == compaction || wider == compaction {
		t.Fatal("the capture's tail holds no user entry for the note, or its record no context prefix")
	}
	for name, tc := range map[string]struct {
		record string
		want   []string
	}{
		"kept plain past the summary":          {compaction, []string{"n-1"}},
		"kept only inside the summary":         {summarized, nil},
		"kept in a model entry":                {modelTail, nil},
		"behind another entry past the prefix": {behind, []string{"n-1"}},
		"a wider context prefix":               {wider, nil},
		"an undecodable line before the reply": {`{"type":"system","subtype":"chat_compression",`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := filepath.Join(t.TempDir(), "compaction.jsonl")
			if err := os.WriteFile(file, []byte(strings.ReplaceAll(tc.record, marker, quoted)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bridge := newQwenBridge(t, note)
			run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1",
				"QWEN_FAKE_COMPACT="+file)
			lines, code := run.finish(t)
			if code != 0 || len(typed(lines, "crewlet_qwen_note")) != 1 {
				t.Fatalf("the run exited %d having handed %v, want the note handed and 0", code,
					typed(lines, "crewlet_qwen_note"))
			}
			if got := bridge.read(); !slices.Equal(got, tc.want) {
				t.Errorf("the notes confirmed read are %v, want %v", got, tc.want)
			}
		})
	}
}

// A QWEN RUN WHOSE BRIDGE NEVER CONNECTS IS NOT STARTED. Qwen Code runs a
// turn with a disconnected server and says nothing of it, so the helper
// polls the workspace's MCP status and, at its bound, ends the run before
// the prompt, saying the bridge never connected.
//
// Mutation: send the prompt at the bound anyway.
func TestAQwenRunWhoseBridgeNeverConnectsIsNotStarted(t *testing.T) {
	t.Parallel()
	bridge := newQwenBridge(t)
	run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", map[string]any{"mcpWaitMs": 600}, "QWEN_FAKE_NEVER_CONNECT=1")
	lines, code := run.finish(t)
	if code != 1 {
		t.Errorf("the helper exited %d, want 1", code)
	}
	end := typed(lines, "crewlet_qwen_end")
	if len(end) != 1 || !strings.Contains(string(mustJSON(t, end[0])), "did not connect the run's tool bridge (probe)") {
		t.Errorf("the run ended %v, want the bridge named as never connected", end)
	}
	for _, e := range run.standinSaw(t) {
		if e["method"] == "session/prompt" {
			t.Error("the prompt went in with the bridge disconnected")
		}
	}
}

// A QWEN RUN WHOSE BRIDGE ANOTHER SERVER SERVES IS NOT STARTED. Qwen Code
// merges servers by name, a system or repository settings file's over the
// home's, so a repository's own `crewlet` connects in the bridge's place
// (measured on 0.28.4, MEASURE-P3 N8's shape); its status answer names the
// connected server's configuration and origin, and the helper ends the run
// before the prompt unless that is the bridge's address with no command. The
// bridge's own entry starts the run.
//
// Mutations, each red: drop the check; match the name alone; pass a server
// with a command beside the address.
func TestAQwenRunWhoseBridgeAnotherServerServesIsNotStarted(t *testing.T) {
	t.Parallel()
	captured, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.4", "acp-wsmcp.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const bridgeURL = "http://127.0.0.1:7/mcp/run-token-placeholder"
	const stdio = `"configOrigin":"user_settings","source":"user","removable":true,"config":{"command":"node","args":["/probe/mcp.mjs"]}`
	if !bytes.Contains(captured, []byte(stdio)) {
		t.Fatal("the capture no longer holds the probe's configuration this test rewrites")
	}
	served := func(entry string) string {
		path := filepath.Join(t.TempDir(), "acp-wsmcp.jsonl")
		if err := os.WriteFile(path, bytes.ReplaceAll(captured, []byte(stdio), []byte(entry)), 0o600); err != nil {
			t.Fatal(err)
		}
		return "QWEN_FAKE_START=" + path
	}
	for _, tc := range []struct {
		name, entry string
		started     bool
	}{
		{"the bridge's own", `"configOrigin":"user_settings","config":{"httpUrl":"` + bridgeURL + `"}`, true},
		{"a repository's of the same name", `"configOrigin":"workspace_settings","config":{"command":"node","args":["/repo/mcp.mjs"]}`, false},
		{"a system file's at another address", `"configOrigin":"system_settings","config":{"httpUrl":"http://127.0.0.1:8/other"}`, false},
		{"one with a command beside the address", `"configOrigin":"workspace_settings","config":{"command":"node","httpUrl":"` + bridgeURL + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bridge := newQwenBridge(t)
			run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", map[string]any{"bridgeURL": bridgeURL},
				served(tc.entry))
			lines, code := run.finish(t)
			prompted := slices.ContainsFunc(run.standinSaw(t), func(e map[string]any) bool { return e["method"] == "session/prompt" })
			if prompted != tc.started {
				t.Errorf("the prompt went in: %v, want %v", prompted, tc.started)
			}
			if tc.started {
				return
			}
			end := typed(lines, "crewlet_qwen_end")
			if code != 1 || len(end) != 1 || !strings.Contains(string(mustJSON(t, end[0])),
				"serves the run's tool bridge (probe) as another MCP server of the same name") {
				t.Errorf("the run exited %d and ended %v, want the bridge named as served by another server", code, end)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A QWEN RUN STOPS WITH session/cancel, NEVER A SIGNAL. Qwen Code tears its
// chat file on SIGTERM (measured on 0.28.4), so the helper, signalled as an
// end request signals it, cancels the turn, waits for its answer, closes
// Qwen Code's stdin and exits as a stopped process; no signal reaches Qwen
// Code. Replayed from 0.28.4's own cancelled turn, the stand-in holding where
// the capture's host cancelled.
//
// Mutations, each red: pass the signal to Qwen Code's process group; exit
// on the signal without cancelling.
func TestAQwenRunStopsWithCancelNeverASignal(t *testing.T) {
	t.Parallel()
	bridge := newQwenBridge(t)
	run := startQwenHelper(t, bridge.steer(), "acp-cancel.jsonl", nil)
	scanner := bufio.NewScanner(run.stdout)
	scanner.Buffer(nil, 1<<20)
	var lines []map[string]any
	signalled := false
	for scanner.Scan() {
		l := decodeLine(t, scanner.Bytes())
		lines = append(lines, l)
		// THE CALL UNDER WAY, the point at which the capture's host cancelled.
		if !signalled && strings.Contains(scanner.Text(), `"status":"in_progress"`) {
			signalled = true
			if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = run.cmd.Wait()
	if code := run.cmd.ProcessState.ExitCode(); code != 143 {
		t.Errorf("the stopped helper exited %d, want 143", code)
	}
	if end := typed(lines, "crewlet_qwen_end"); len(end) != 1 || end[0]["stop_reason"] != "cancelled" {
		t.Errorf("the turn ended %v, want cancelled", end)
	}
	saw := run.standinSaw(t)
	var cancelled, closed bool
	for _, e := range saw {
		cancelled = cancelled || e["method"] == "session/cancel"
		closed = closed || e["stdin"] == "closed"
		if e["signal"] != nil {
			t.Errorf("Qwen Code was sent %v", e["signal"])
		}
	}
	if !cancelled || !closed {
		t.Errorf("Qwen Code saw %v, want session/cancel and its stdin closed", saw)
	}
}

// AFTER QWEN CODE'S OWN 429 FAILURE THE HELPER QUOTES ONLY THE FAILING
// REQUEST'S RECORD, AND NEVER OTHERWISE. Replayed from 0.28.4's turns that
// failed on a spent quota and on a rate limit, each after seven attempts,
// with their own chat files: the helper exits 173 and quotes the provider's
// words of the request that failed last, the first record naming it, so a
// record a process of the box appended later under the same request is never
// read; a turn that finished never reads the chat file, whatever it holds.
//
// Mutations, each red: quote the last api_error record of the file; quote
// the last record naming the failed request; read the record on a turn that
// did not fail; exit 1 on the 429.
func TestAfterQwensOwn429TheHelperQuotesOnlyTheFailingRequestsRecord(t *testing.T) {
	t.Parallel()
	captures := filepath.Join("testdata", "qwen", "0.28.4")
	quotaRecord := func(t *testing.T) string {
		for _, l := range strings.Split(string(qwenFixture(t, "0.28.4", "chat-quota.jsonl")), "\n") {
			if strings.Contains(l, "qwen-code.api_error") {
				return l
			}
		}
		t.Fatal("the quota chat file holds no error record")
		return ""
	}
	forged := filepath.Join(t.TempDir(), "chat-rate-forged.jsonl")
	rate := string(qwenFixture(t, "0.28.4", "chat-rate.jsonl"))
	// THE RATE LIMIT'S LAST FAILED REQUEST, its record copied with a quota's
	// words, appended after the real one.
	var lastID string
	for _, l := range strings.Split(rate, "\n") {
		if strings.Contains(l, "qwen-code.api_error") {
			var r struct {
				SystemPayload struct {
					UIEvent map[string]any `json:"uiEvent"`
				} `json:"systemPayload"`
			}
			_ = json.Unmarshal([]byte(l), &r)
			lastID, _ = r.SystemPayload.UIEvent["execution_id"].(string)
		}
	}
	var q map[string]any
	_ = json.Unmarshal([]byte(quotaRecord(t)), &q)
	q["systemPayload"].(map[string]any)["uiEvent"].(map[string]any)["execution_id"] = lastID
	if err := os.WriteFile(forged, append([]byte(rate), append(mustJSON(t, q), '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		turn, seed string
		code       int
		said       string // the limit line's words, "" for none
	}{
		"a spent quota": {turn: "acp-quota.jsonl", seed: filepath.Join(captures, "chat-quota.jsonl"), code: 173,
			said: "429 You exceeded your current quota"},
		"a rate limit with a quota record appended": {turn: "acp-rate.jsonl", seed: forged, code: 173,
			said: "429 Rate limit reached"},
		"a finished turn with quota records": {turn: "acp-note.jsonl",
			seed: filepath.Join(captures, "chat-quota.jsonl"), code: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seed, err := filepath.Abs(tc.seed)
			if err != nil {
				t.Fatal(err)
			}
			run := startQwenHelper(t, newQwenBridge(t).steer(), tc.turn, nil, "QWEN_FAKE_CHAT_SEED="+seed)
			lines, code := run.finish(t)
			if code != tc.code {
				t.Errorf("the helper exited %d, want %d", code, tc.code)
			}
			limits := typed(lines, "crewlet_qwen_limit")
			switch {
			case tc.said == "" && len(limits) != 0:
				t.Errorf("a turn that did not fail quoted %v", limits)
			case tc.said != "" && (len(limits) != 1 ||
				!strings.HasPrefix(limits[0]["error_message"].(string), tc.said)):
				t.Errorf("the limit lines read %v, want one quoting %q", limits, tc.said)
			}
		})
	}
}

// A SKILL QWEN CODE LOADED IS REPORTED BY THE NAME IT WAS HANDED UNDER.
// Driven as Qwen Code runs the PostToolUse hook, with the input it hands one
// (measured on 0.28.4): the skill tool's load of `crewlet-probe` is reported
// as `crewlet:probe`, any other tool's call reports nothing, and the hook
// answers with an empty object either way.
//
// Mutations, each red: report by Qwen Code's spelling; report every tool.
func TestAQwenSkillLoadIsReportedByTheNameItWasHandedUnder(t *testing.T) {
	t.Parallel()
	node := nodeForQwen(t)
	for name, tc := range map[string]struct {
		input map[string]any
		want  string
	}{
		"a skill's load": {input: map[string]any{"hook_event_name": "PostToolUse", "tool_name": "skill",
			"tool_input": map[string]any{"skill": "crewlet-probe"}}, want: `{"skill":"crewlet:probe"}`},
		"another tool's call": {input: map[string]any{"hook_event_name": "PostToolUse",
			"tool_name": "run_shell_command", "tool_input": map[string]any{"command": "ls"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bridge := newQwenBridge(t)
			path := filepath.Join(t.TempDir(), "qwen-helper.json")
			if err := os.WriteFile(path, mustJSON(t, map[string]any{"skills": bridge.srv.URL + "/skills",
				"namespace": "crewlet"}), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), node, "qwen_helper.mjs", "skill", path)
			cmd.Stdin = bytes.NewReader(mustJSON(t, tc.input))
			out, err := cmd.Output()
			if err != nil || string(out) != "{}" {
				t.Fatalf("the hook answered %q, %v; want {}", out, err)
			}
			bridge.mu.Lock()
			defer bridge.mu.Unlock()
			if got := strings.Join(bridge.skills, "|"); got != tc.want {
				t.Errorf("the hook reported %q, want %q", got, tc.want)
			}
		})
	}
}

// A REAL BOX'S NOTES REACH THE BRIDGE'S ROUTE, for Qwen Code: the helper, with
// the configuration a run is handed, polls a real bridge's notes route (its
// inbox a stand-in of the engine's), hands the note to 0.28.4's replayed
// turn, and confirms it through the route by the note's own id once the
// session records it. A field either side renamed would leave every Qwen Code
// run handing nothing, with no error anywhere.
//
// Mutation: rename `message`, `seq` or `id` on mcpbridge.SteerNote, or `ids`
// on the route's read, or the field the helper reads for any of them.
func qwenHelperReachesTheBridgesOwnNotesRoute(t *testing.T) {
	t.Parallel()
	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: executor.InboxID(executor.QwenCodeName, "job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})
	run := startQwenHelper(t, mcpbridge.SteerURL(endpoint), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1")
	lines, code := run.finish(t)
	if code != 0 || len(typed(lines, "crewlet_qwen_note")) != 1 {
		t.Fatalf("the helper exited %d having handed %v, want the note handed", code, typed(lines, "crewlet_qwen_note"))
	}
	// THE HELPER'S EXIT IS THE SIGNAL: it posts its confirmations before it
	// ends.
	select {
	case ids := <-inbox.reads:
		if len(ids) != 1 || ids[0] != note.ID {
			t.Errorf("the route handed the inbox the read %v, want the note's id %q", ids, note.ID)
		}
	default:
		t.Fatal("the helper's confirmation never reached the run's inbox through the route")
	}
}

// qwenKey is the projects directory Qwen Code records a session from dir
// under: the realpath dir is, every character but a letter or a digit a dash.
func qwenKey(dir string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, dir)
}

// A LOADED QWEN CODE SESSION'S EARLIER JOB IS NEITHER THIS RUN'S STREAM NOR
// ITS CONFIRM. The run continues a session whose chat file (Qwen Code 0.28.4's
// own, chat-note.jsonl) records a note an earlier job delivered; the helper
// loads it (session/load under the box's realpath), and the stand-in replays
// its history first, as ACP's loadSession does. That replay is not written to
// the stream, which the runner reads as the run's own view and spend; and the
// same note handed again this run, its harness dying before recording it, is
// not confirmed by the earlier record, so the engine serves it again.
//
// Mutations, each red: start the session with session/new on a continued run;
// write the load's replay to the stream; scan the chat file from its start.
func TestALoadedQwenSessionsEarlierJobIsNeitherTheStreamNorTheConfirm(t *testing.T) {
	t.Parallel()
	held, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.4", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	note := qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1", Message: "MARK-R6-START drained note MARK-R6-END"}
	if !bytes.Contains(held, []byte(`"displayText":"`+note.Message+`"`)) {
		t.Fatalf("the capture records no note %q", note.Message)
	}
	bridge := newQwenBridge(t, note)
	run := startQwenHelperAs(t, "load", held, bridge.steer(), "acp-note.jsonl", nil,
		"QWEN_FAKE_AWAIT_NOTES=1", "QWEN_FAKE_DIE_AFTER_NOTE=1")
	lines, _ := run.finish(t)
	real, err := filepath.EvalSymlinks(run.cwd)
	if err != nil {
		t.Fatal(err)
	}
	var loads []any
	for _, saw := range run.standinSaw(t) {
		if load, ok := saw["load"]; ok {
			loads = append(loads, load)
		}
	}
	want := map[string]any{"sessionId": qwenCapturedSession, "cwd": real, "mcpServers": []any{}}
	if len(loads) != 1 || string(mustJSON(t, loads[0])) != string(mustJSON(t, want)) {
		t.Fatalf("the stand-in was loaded with %v, want one session/load %v", loads, want)
	}
	if got := typed(lines, "crewlet_qwen_note"); len(got) != 1 {
		t.Fatalf("the run handed %v, want the note once", got)
	}
	for _, l := range lines {
		if strings.Contains(string(mustJSON(t, l)), "REPLAYED-HISTORY") {
			t.Errorf("the stream carries the load's replay of the earlier job: %v", l)
		}
	}
	if got := bridge.read(); len(got) != 0 {
		t.Errorf("a note this run's harness never recorded was confirmed by the earlier job's record: %v", got)
	}
}

// A CARRIED QWEN CODE SESSION IS FILED UNDER THE BOX'S OWN KEY. Qwen Code
// 0.28.4's chat file, recorded at /work/proj, restored in another working
// directory: it lies under that directory's key, its first line's cwd the
// directory's realpath (the one field a resume checks) and every other byte
// as carried, and the box's other copy of it under another key is gone. A
// file whose first line names another session is refused and nothing filed.
//
// Mutations, each red: keep the first line's cwd; file it under the recorded
// key; leave another key's copy; file a chat file of another session.
func TestACarriedQwenSessionIsFiledUnderTheBoxsOwnKey(t *testing.T) {
	t.Parallel()
	node := nodeForQwen(t)
	helper, err := filepath.Abs("qwen_helper.mjs")
	if err != nil {
		t.Fatal(err)
	}
	carried, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.4", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first, rest, _ := bytes.Cut(carried, []byte("\n"))
	for _, tc := range []struct {
		name, session string
		code          int
	}{
		{"the session's own file", qwenCapturedSession, 0},
		{"another session's file", "11111111-2222-4333-8444-000000000009", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			home, cwd := filepath.Join(dir, "qwen-home"), filepath.Join(dir, "work", "box")
			stale := filepath.Join(home, "projects", "-work-old", "chats", tc.session+".jsonl")
			staged := filepath.Join(dir, "in", "projects", "-work-proj", "chats", qwenCapturedSession+".jsonl")
			for file, content := range map[string][]byte{stale: []byte("{}\n"), staged: carried} {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "qwen-helper.json")
			if err := os.WriteFile(config, mustJSON(t, map[string]any{"qwenHome": home}), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), node, helper, "restore", config, tc.session, staged)
			cmd.Dir = cwd
			out, _ := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); code != tc.code {
				t.Fatalf("the restore exited %d (%s), want %d", code, out, tc.code)
			}
			real, err := filepath.EvalSymlinks(cwd)
			if err != nil {
				t.Fatal(err)
			}
			filed := filepath.Join(home, "projects", qwenKey(real), "chats", tc.session+".jsonl")
			got, err := os.ReadFile(filed)
			if tc.code != 0 {
				if err == nil {
					t.Errorf("a refused restore filed %s", filed)
				}
				return
			}
			if err != nil {
				t.Fatalf("the session is not under the box's key: %v", err)
			}
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Errorf("the copy under another key survives the restore (%v)", err)
			}
			gotFirst, gotRest, _ := bytes.Cut(got, []byte("\n"))
			var head, wantHead map[string]any
			if err := json.Unmarshal(gotFirst, &head); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(first, &wantHead); err != nil {
				t.Fatal(err)
			}
			wantHead["cwd"] = real
			if string(mustJSON(t, head)) != string(mustJSON(t, wantHead)) || !bytes.Equal(gotRest, rest) {
				t.Errorf("the filed session's first line is %s, want the carried one with cwd %q, the rest unchanged", gotFirst, real)
			}
		})
	}
}

// A QWEN CODE CHAT FILE THAT SHRANK CONFIRMS NOTHING MORE. The run loads a
// session whose chat file (0.28.4's chat-note.jsonl) records an earlier job's
// delivery of a note and the reply after it; handed the same note, Qwen Code's
// stand-in rewrites the file shorter, down to that earlier record and reply,
// and dies. The helper reads a file shorter than what it had read as a
// rewrite and confirms nothing of it, so the note is served again rather than
// confirmed by the earlier job's record.
//
// Mutation, red: read a shrunk file again from its start, as before the run.
func TestAQwenChatFileThatShrankConfirmsNothingMore(t *testing.T) {
	t.Parallel()
	held, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.4", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bridge := newQwenBridge(t, qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1",
		Message: "MARK-R6-START drained note MARK-R6-END"})
	run := startQwenHelperAs(t, "load", held, bridge.steer(), "acp-note.jsonl", nil,
		"QWEN_FAKE_AWAIT_NOTES=1", "QWEN_FAKE_SHRINK_AFTER_NOTE=1")
	lines, _ := run.finish(t)
	if got := typed(lines, "crewlet_qwen_note"); len(got) != 1 {
		t.Fatalf("the run handed %v, want the note once", got)
	}
	real, err := filepath.EvalSymlinks(run.cwd)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := os.ReadFile(filepath.Join(run.home, "projects", qwenKey(real), "chats", qwenCapturedSession+".jsonl"))
	if err != nil || len(chat) >= len(held) || !bytes.Contains(chat, []byte(`"mid_turn_user_message"`)) {
		t.Fatalf("the chat file was not cut back to the earlier delivery (%d of %d bytes, %v)", len(chat), len(held), err)
	}
	if got := bridge.read(); len(got) != 0 {
		t.Errorf("a shrunk chat file confirmed %v by the earlier job's record", got)
	}
}

// A CONTINUED QWEN SESSION WHOSE CHAT FILE CANNOT BE MEASURED CONFIRMS NOTHING.
// The run loads a session whose chat file is not where the helper reads it as
// the turn begins; the turn goes on, Qwen Code records the note it is handed
// and the reply after it, and the helper confirms none of it: from the file's
// start, an earlier job's record of the same text would confirm a note this
// run lost, so the take stays undecided rather than read from 0.
//
// Mutation, red: begin a continued session's read at 0 where it cannot be
// measured, as a new session's.
func TestAContinuedQwenSessionItCannotMeasureConfirmsNothing(t *testing.T) {
	t.Parallel()
	bridge := newQwenBridge(t, qwenNote{Seq: 1, ID: "n-1", Inbox: "qwen_1",
		Message: "## A note from eve, sent while this turn was running\nx"})
	run := startQwenHelperAs(t, "load", nil, bridge.steer(), "acp-note.jsonl", nil, "QWEN_FAKE_AWAIT_NOTES=1")
	lines, code := run.finish(t)
	if code != 0 || len(typed(lines, "crewlet_qwen_note")) != 1 {
		t.Fatalf("the run exited %d having handed %v, want the note handed once", code,
			typed(lines, "crewlet_qwen_note"))
	}
	if got := bridge.read(); len(got) != 0 {
		t.Errorf("a continued session read from its start confirmed %v", got)
	}
}

// A QWEN RUN LOADS THE EXTENSIONS A PERSON'S SESSION LOADS: the helper starts
// Qwen Code with no `--extensions` flag, which would name the only ones to
// load.
//
// Mutation: start Qwen Code with `--extensions none`.
func TestAQwenRunLoadsItsHomesExtensions(t *testing.T) {
	t.Parallel()
	bridge := newQwenBridge(t)
	run := startQwenHelper(t, bridge.steer(), "acp-note.jsonl", map[string]any{"mcpWaitMs": 600}, "QWEN_FAKE_NEVER_CONNECT=1")
	run.finish(t)
	saw := run.standinSaw(t)
	if len(saw) == 0 {
		t.Fatal("the stand-in saw nothing")
	}
	argv, _ := saw[0]["argv"].([]any)
	if len(argv) == 0 || slices.Contains(argv, any("--extensions")) {
		t.Errorf("Qwen Code started with %v, want no --extensions", argv)
	}
}
