package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// copilotFixture is testdata/copilot/1.0.114/<name>: a real Copilot 1.0.114
// run's stream or events.jsonl, against a stand-in model (its BYOK variables)
// and a stand-in bridge, with the vendor's own prompt text replaced
// (`[the vendor's text, not kept in this fixture]`) and every other byte as the
// harness wrote it.
func copilotFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "copilot", "1.0.114", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// copilotScriptUsage is what a run's script adds to its stream once its harness
// exits ([copilotUsage]), run under sh over events, a session's events.jsonl,
// that held before lines when the run started.
func copilotScriptUsage(t *testing.T, events []byte, before int) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(file, events, 0o600); err != nil {
		t.Fatal(err)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	script := "ev=" + sandbox.ShellQuote(file) + " el=" + strconv.Itoa(before) + "\n" + copilotUsage()
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatalf("the usage part of the script failed: %v", err)
	}
	return out
}

// copilotCollected is what a run whose harness wrote stream and whose session
// is events leaves in its result file: the stream, then the script's usage.
func copilotCollected(t *testing.T, stream, events string, before int) []byte {
	t.Helper()
	return append(copilotFixture(t, stream), copilotScriptUsage(t, copilotFixture(t, events), before)...)
}

// The run's answer is its last root message, and its spend the usage records
// its own process wrote, read after the script's mark.
//
// Mutation: read the answer off the `result` line, or sum the records without
// the mark — the case fails.
func TestACopilotRunsAnswerAndSpendAreItsOwn(t *testing.T) {
	dec := &copilotEvents{}
	if err := eachLine(bytes.NewReader(copilotCollected(t, "tool.stream.jsonl", "tool.events.jsonl", 0)), dec,
		maxLineBytes); err != nil {
		t.Fatal(err)
	}
	res := dec.Result()
	if !res.Success || res.Text != "ANSWER after tool" {
		t.Fatalf("result = %+v, want the success the run's last root message said", res)
	}
	if res.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("session = %q, want the stream's own", res.SessionID)
	}
	// Two requests: 1001+1002 prompt tokens with 500 cached read and 7 written
	// each, 11+12 output — what the stand-in model answered and Copilot recorded.
	spent, stated := dec.runSpend()
	if !stated {
		t.Fatal("the run's spend was not stated, and the script marked its usage")
	}
	charged := spent.on(sandbox.Result{})
	if charged.InputTokens != 2003 || charged.CacheReadTokens != 1000 || charged.CacheWriteTokens != 14 ||
		charged.OutputTokens != 23 || charged.CostUSD != 0 {
		t.Errorf("charged %+v, want input 2003 (the cache inside it), cache 1000/14, output 23, no price", charged)
	}
	if res.Turns != 2 || dec.reportedWindow() != 128000 {
		t.Errorf("turns %d, window %d; want the two requests and their 128000-token prompt budget",
			res.Turns, dec.reportedWindow())
	}
	if !strings.Contains(res.Transcript, "crewlet-echo") {
		t.Errorf("the transcript names no call:\n%s", res.Transcript)
	}
}

// A run this runner launched end to end (the engine's own rendered script,
// settings, instructions and extension, in a box against the stand-ins, a note
// sent while its tool ran) reads as its answer and its own spend off the result
// file its script left, and the note as held off its session's record.
//
// Mutation: read the script's usage mark as no mark, or confirm a note off
// another clientTurnId — the case fails.
func TestALaunchedCopilotRunReadsAsItsAnswerSpendAndNote(t *testing.T) {
	res := (Copilot{}).Parse(copilotFixture(t, "launched.result.jsonl"))
	if !res.Success || res.Text != "ANSWER saw note" {
		t.Fatalf("result = %+v, want the answer to the note", res)
	}
	if res.InputTokens != 2049 || res.CacheReadTokens != 1000 || res.CacheWriteTokens != 14 || res.OutputTokens != 69 {
		t.Errorf("charged in %d (cache %d/%d), out %d; want 2049 (1000/14), 69", res.InputTokens,
			res.CacheReadTokens, res.CacheWriteTokens, res.OutputTokens)
	}
	if !strings.Contains(res.Transcript, "[steer]") {
		t.Errorf("the transcript does not show the note:\n%s", res.Transcript)
	}
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotFixture(t, "launched.events.jsonl")),
		[]string{"msg_inbox1"})
	if err != nil || !held["msg_inbox1"] {
		t.Fatalf("held = %v (%v), want the note", held, err)
	}
}

// A usage record before the script's mark is not charged: one Copilot
// printed itself would be counted twice beside the script's. A stream with no
// mark states no spend.
//
// Mutation: count a record whatever came before it — the case fails.
func TestACopilotUsageRecordBeforeTheMarkIsNotCharged(t *testing.T) {
	events := copilotFixture(t, "tool.events.jsonl")
	unmarked := append(copilotFixture(t, "tool.stream.jsonl"), events...)
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(unmarked), dec, maxLineBytes)
	if spent, stated := dec.runSpend(); stated || spent != (runSpend{}) {
		t.Fatalf("spend %+v stated %v, want none stated with no mark", spent, stated)
	}
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(append(unmarked, copilotScriptUsage(t, events, 0)...)), dec, maxLineBytes)
	spent, _ := dec.runSpend()
	if got := spent.on(sandbox.Result{}); got.InputTokens != 2003 || got.OutputTokens != 23 {
		t.Fatalf("charged %+v, want the two records after the mark alone (2003 in, 23 out)", got)
	}
}

// A continued session's usage is the run's own: the records after the start
// this process wrote, never the session's earlier processes'.
//
// Mutation: take every record of the file, or the records after the first
// start — the case fails.
func TestACopilotContinuedRunIsChargedItsOwnRequestsAlone(t *testing.T) {
	events := copilotFixture(t, "resumed.events.jsonl")
	// The first process wrote 16 lines (its two requests); the resume one.
	out := copilotScriptUsage(t, events, 16)
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(out), dec, maxLineBytes)
	spent, _ := dec.runSpend()
	if got := spent.on(sandbox.Result{}); got.InputTokens != 1003 || got.OutputTokens != 13 || dec.requests != 1 {
		t.Fatalf("charged %+v over %d requests, want the resume's one request (1003 in, 13 out)", got, dec.requests)
	}
	// AND A PROCESS THAT WROTE NO START OF ITS OWN is charged nothing, never
	// the run before it.
	none := copilotScriptUsage(t, events, 23)
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(none), dec, maxLineBytes)
	if spent, stated := dec.runSpend(); !stated || spent != (runSpend{}) {
		t.Fatalf("a run that wrote no start was charged %+v (stated %v), want nothing, stated", spent, stated)
	}
}

// A rate limit is reported as the run's limit line and its terminal event, for
// the entry to sit its plan out.
//
// Mutation: keep no limit line for `rate_limit` — the case fails.
func TestACopilotRateLimitIsReportedAsALimit(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "ratelimit.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !strings.Contains(res.Error, "429") {
		t.Fatalf("result = %+v, want a failure saying the 429", res)
	}
	limit := dec.limitReport()
	if !strings.Contains(limit, `"type":"session.error"`) || !strings.Contains(limit, `"errorType":"rate_limit"`) ||
		res.Event != limit {
		t.Fatalf("limit %q, event %q: want the session.error line in both", limit, res.Event)
	}
}

// A run the extension stopped at its cap reads as capped: its line, the abort,
// the aborted idle and exit 1, all four. The same abort without the line (as
// Ctrl+C leaves one, `user_initiated` too) is an abort, not a cap.
//
// Mutation: read a cap off the abort without the line — the second half
// fails; drop the line's arm — the first does.
func TestACopilotRunStoppedAtItsCapReadsAsCapped(t *testing.T) {
	stream := copilotFixture(t, "capped.stream.jsonl")
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(stream), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !res.RoundCapped || !strings.Contains(res.Error, "turn cap of 3") {
		t.Fatalf("result = %+v, want a run capped at 3", res)
	}
	var unlabelled []byte
	for l := range bytes.Lines(stream) {
		if !bytes.Contains(l, []byte(copilotCapMessage)) {
			unlabelled = append(unlabelled, l...)
		}
	}
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(unlabelled), dec, maxLineBytes)
	if res := dec.Result(); res.Success || res.RoundCapped || !strings.Contains(res.Error, "aborted") {
		t.Fatalf("an abort with no cap line read as %+v, want an aborted run", res)
	}
}

// The cap's line alone labels nothing: a run that logged it and answered is
// an answer (forged-cap: an extension that only logged the line).
//
// Mutation: read the line alone as a cap (drop the abort, idle and exit from
// the condition) — the case fails.
func TestACopilotCapLineWithoutAnAbortedExitIsIgnored(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "forged-cap.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if !res.Success || res.RoundCapped {
		t.Fatalf("result = %+v, want the answer, not a capped run", res)
	}
}

// A seat tool a repository's hook refused fails the run, saying so: the run
// could not do its work through its seat.
//
// Mutation: drop the denied check — the run reads as a success.
func TestACopilotRunWhoseSeatToolAHookRefusedFails(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "hook-denied.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !strings.Contains(res.Error, "crewlet-echo") ||
		!strings.Contains(res.Error, "Denied by preToolUse hook: no crewlet here") {
		t.Fatalf("result = %+v, want a failure naming the tool and the hook's reason", res)
	}
}

// A conversation past its model's prompt reads as context exhausted.
//
// Mutation: map no errorType to ContextExhausted — the case fails.
func TestACopilotRunPastItsContextReadsAsExhausted(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "refused-note.stream.jsonl")), dec, maxLineBytes)
	if res := dec.Result(); res.Success || !res.ContextExhausted {
		t.Fatalf("result = %+v, want a run whose context was exhausted", res)
	}
}

// A note is held only once Copilot's own record shows it reached the model:
// its line under the extension's clientTurnId, then a usage record after it.
//
// Mutation: confirm on the line alone — killed-in-gap and refused-note are
// confirmed; drop the queued delivery — queued-note is not.
func TestACopilotNoteIsHeldOnlyOnceARequestCarriedIt(t *testing.T) {
	cases := []struct {
		fixture string
		held    bool
	}{
		{"steered.events.jsonl", true},
		{"queued-note.events.jsonl", true},
		// The process killed after the line, before its request answered.
		{"killed-in-gap.events.jsonl", false},
		// The request the note rode refused before it was made.
		{"refused-note.events.jsonl", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotFixture(t, c.fixture)),
				[]string{"msg_inbox1", "msg_never"})
			if err != nil {
				t.Fatal(err)
			}
			if held["msg_inbox1"] != c.held || held["msg_never"] {
				t.Fatalf("held = %v, want msg_inbox1 %v and nothing never sent", held, c.held)
			}
		})
	}
}

// copilotArchive is a Copilot archive holding files, by member name.
func copilotArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range files {
			if !yield(n) {
				return
			}
		}
	}) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(files[name])
	}
	_ = tw.Close()
	_ = zw.Close()
	return out.Bytes()
}

// A Copilot session's transcript files each line by what it is: the brief, the
// model's call and its result, the note under its heading, the answer.
//
// Mutation: read a steering message as the brief (skip the heading split) —
// the note is filed as a brief.
func TestACopilotTranscriptFilesTheNoteUnderItsHeading(t *testing.T) {
	archive := copilotArchive(t, map[string][]byte{
		"session-state/55555555-5555-4555-8555-555555555555/events.jsonl": copilotFixture(t, "steered.events.jsonl"),
		"session-state/55555555-5555-4555-8555-555555555555/session.db":   []byte("SQLite format 3\x00"),
	})
	b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
	got, err := b.decode(t.Context(), Copilot{}, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EntryKind
	for _, e := range got.Entries {
		kinds = append(kinds, e.Kind)
	}
	want := []EntryKind{EntryBrief, EntryToolCall, EntryToolResult, EntryNote, EntryModel}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if call := got.Entries[1]; call.Tool != "crewlet-slow" || got.Entries[2].Tool != "crewlet-slow" {
		t.Errorf("the call and its result name %q and %q, want crewlet-slow", call.Tool, got.Entries[2].Tool)
	}
	if note := got.Entries[3].Text; !strings.Contains(note, "please also check X") {
		t.Errorf("the note's entry is %q", note)
	}
}

// A run is started on Copilot's own posture: the brief on stdin, every engine
// server enabled by name over a trusted repository's settings, the session by
// `--session-id`, never `--trust-folder` (refused beside it) nor `-p`.
//
// Mutation: drop `--enable-mcp-server`, or add `--trust-folder` — the case fails.
func TestACopilotRunIsStartedOnItsPosture(t *testing.T) {
	paths := Paths{Home: "/box"}
	req := sandbox.RunRequest{Model: "gpt-5.2", Effort: "high",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {}, "linear": {}}}
	script := copilotScript(req, Session{ID: "s-1"}, paths, paths.MCPConfig(), time.Second)
	for _, want := range []string{"copilot --output-format json --yolo --no-ask-user --no-auto-update",
		"--disable-client-telemetry", "--disable-builtin-mcps", "--sync-session=off",
		"--additional-mcp-config '@/box/.crewlet/mcp.json'",
		"--enable-mcp-server 'crewlet' --enable-mcp-server 'linear'", "--model 'gpt-5.2'",
		"--reasoning-effort 'high'", `set -- "$@" --session-id 's-1'`,
		"older than 1.0.114"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	invocation := ""
	for l := range strings.Lines(script) {
		if strings.Contains(l, "copilot --output-format") {
			invocation = l
		}
	}
	for _, refused := range []string{"--trust-folder", " -p ", "--prompt", "--resume", "--experimental"} {
		if strings.Contains(invocation, refused) {
			t.Errorf("the invocation holds %q: %s", refused, invocation)
		}
	}
}

// Every provider variable the entry did not set is unset; one it set stays.
//
// Mutation: unset the whole family — the entry's provider is lost.
func TestACopilotRunKeepsOnlyItsEntrysProvider(t *testing.T) {
	off := copilotProviderOff(map[string]string{"COPILOT_PROVIDER_BASE_URL": "https://example.com"})
	if slices.Contains(off, "COPILOT_PROVIDER_BASE_URL") || !slices.Contains(off, "COPILOT_PROVIDER_API_KEY") ||
		len(off) != len(copilotProviders)-1 {
		t.Fatalf("unset %v", off)
	}
}

// The run is pinned to its own home and trusted by COPILOT_ALLOW_ALL=true
// (route (a), measured to widen nothing beyond --yolo); the switches that open
// what a repository loads in prompt mode are unset.
//
// Mutation: leave COPILOT_ALLOW_ALL out of the pins — the case fails.
func TestACopilotRunIsPinnedToItsHomeAndTrust(t *testing.T) {
	paths := Paths{Home: "/box"}
	pins := map[string]string{}
	for _, p := range (Copilot{}).Pins(t.Context(), sandbox.RunRequest{}, paths) {
		pins[p.Name] = p.Value
	}
	want := map[string]string{"COPILOT_HOME": "/box/.copilot", "COPILOT_AUTO_UPDATE": "false",
		"COPILOT_TELEMETRY_DISABLED": "true", "COPILOT_MCP_TOOL_CACHE": "false", "COPILOT_ALLOW_ALL": "true"}
	for k, v := range want {
		if pins[k] != v {
			t.Errorf("%s = %q, want %q", k, pins[k], v)
		}
	}
	unset := (Copilot{}).Output(paths).Unset
	for _, name := range []string{"GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS", "COPILOT_SKILLS_DIRS",
		"COPILOT_TASK_WAIT_TIMEOUT_SECONDS", "COPILOT_CLI_VERSION"} {
		if !slices.Contains(unset, name) {
			t.Errorf("%s is not unset", name)
		}
	}
}

// WriteConfig writes the run's settings, instructions told of notes, its
// extension wired to its route and cap, and its MCP servers — and never the
// login's config.json, whose tokens and mode a local cell's seeded login keeps.
//
// Mutation: write config.json, or drop `experimental` — the case fails.
func TestCopilotWriteConfigWritesItsHomeAndKeepsTheLogin(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	paths := PathsFor(box)
	login := `{"copilotTokens":{"https://github.com:u":"gho_placeholder"},"trustedFolders":[]}`
	box.Put(paths.CopilotHome()+"/config.json", login)
	req := sandbox.RunRequest{System: "Work well.", Limits: sandbox.Limits{MaxTurns: 7},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: "http://engine/mcp/tok/steer"},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
			URL: "http://engine/mcp/tok", Headers: map[string]string{"Authorization": "Bearer t"},
			CallTimeout: 90 * time.Second}}}
	cfg, err := (Copilot{}).WriteConfig(t.Context(), box, req, paths)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		raw, err := box.ReadFile(t.Context(), p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return string(raw)
	}
	if got := read(paths.CopilotHome() + "/config.json"); got != login {
		t.Errorf("config.json = %s, want the login as seeded", got)
	}
	var settings map[string]any
	_ = json.Unmarshal([]byte(read(paths.CopilotSettings())), &settings)
	if settings["experimental"] != true {
		t.Errorf("settings = %v, want experimental", settings)
	}
	if got := read(paths.CopilotInstructions()); !strings.HasPrefix(got, "Work well.\n\n") ||
		!strings.HasSuffix(got, CopilotSteerInstruction) {
		t.Errorf("instructions = %q, want the system text then the notes instruction", got)
	}
	ext := read(paths.CopilotExtension() + "/extension.mjs")
	if !strings.Contains(ext, `const config = {"url":"http://engine/mcp/tok/steer","cap":7}`) ||
		strings.Contains(ext, copilotExtensionConfig) {
		t.Errorf("the extension's settings are not its own:\n%s", firstLine(ext[strings.Index(ext, "const config"):], 200))
	}
	if cfg != paths.MCPConfig() {
		t.Fatalf("config path %q", cfg)
	}
	var mcp struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	_ = json.Unmarshal([]byte(read(cfg)), &mcp)
	s := mcp.Servers["crewlet"]
	if s["type"] != "http" || s["url"] != "http://engine/mcp/tok" || s["timeout"] != float64(90000) ||
		len(s["tools"].([]any)) != 1 {
		t.Errorf("crewlet = %v", s)
	}
}

// A run with neither notes nor a cap is written no extension, and what an
// earlier run wrote in the home is the clear's to remove: the instructions,
// the extension and a fresh session's copy.
//
// Mutation: drop the extension's directory from the artifacts, or write the
// extension for every run — the case fails.
func TestACopilotRunsArtifactsAreItsHomesOwnFiles(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	paths := PathsFor(box)
	if _, err := (Copilot{}).WriteConfig(t.Context(), box, sandbox.RunRequest{}, paths); err != nil {
		t.Fatal(err)
	}
	if ext, _ := box.ReadFile(t.Context(), paths.CopilotExtension()+"/extension.mjs"); ext != nil {
		t.Error("an extension was written for a run with no notes and no cap")
	}
	files, dirs := (Copilot{}).artifacts(paths)
	if !slices.Equal(files, []string{paths.CopilotInstructions(), paths.CopilotExtensionFresh()}) ||
		!slices.Equal(dirs, []string{paths.CopilotExtension()}) {
		t.Fatalf("artifacts = %v, %v", files, dirs)
	}
}

// A note's inbox id is the job's and the note's alone, the same every time it
// is asked: a note served again is read back by the id it was sent under.
//
// Mutation: hash the note alone — two jobs' ids agree.
func TestACopilotInboxIDIsTheJobsAndTheNotes(t *testing.T) {
	c := Copilot{}
	a := c.inboxID("job-1", "note-1")
	if a != c.inboxID("job-1", "note-1") || a == c.inboxID("job-2", "note-1") || a == c.inboxID("job-1", "note-2") ||
		len(a) != 32 {
		t.Fatalf("inbox ids: %q", a)
	}
}

// The runner reads a note's confirmation off the box's own copy of the
// session, and says it could not where the record is not there to read.
//
// Mutation: answer an unreadable record as no note held — the error case fails.
func TestACopilotNoteIsConfirmedOffTheBoxsRecord(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	const session = "55555555-5555-4555-8555-555555555555"
	box.Put(PathsFor(box).CopilotSessionDir(session)+"/events.jsonl", string(copilotFixture(t, "steered.events.jsonl")))
	held, err := (Copilot{}).notesHeld(t.Context(), box, session, []string{"msg_inbox1"})
	if err != nil || !held["msg_inbox1"] {
		t.Fatalf("held = %v (%v), want the steered note", held, err)
	}
	box.ReadErr = func(string) error { return io.ErrUnexpectedEOF }
	if _, err := (Copilot{}).notesHeld(t.Context(), box, session, []string{"msg_inbox1"}); err == nil {
		t.Fatal("an unreadable record answered as a reading")
	}
}

// The extension holds its settings' placeholder exactly once.
//
// Mutation: a second placeholder in copilot_steer.js — rendering refuses.
func TestTheCopilotExtensionHoldsItsPlaceholderOnce(t *testing.T) {
	if _, err := copilotExtension(copilotExtensionSettings{Cap: 1}); err != nil {
		t.Fatal(err)
	}
}

// A session carried from another box continues only on and from 1.0.114 or
// later, and never on a box older than its writer.
//
// Mutation: drop the box-older-than-writer arm — the last case fails.
func TestACopilotSessionContinuesOnlyOnItsFloor(t *testing.T) {
	cases := []struct {
		box, writer string
		ok          bool
	}{
		{"1.0.114", "1.0.114", true},
		{"1.0.120", "1.0.114", true},
		{"1.0.103", "1.0.114", false},
		{"1.0.114", "1.0.103", false},
		{"1.0.114", "1.0.120", false},
	}
	for _, c := range cases {
		if ok, why := (Copilot{}).Continues(c.box, c.writer); ok != c.ok {
			t.Errorf("Continues(%s, %s) = %v (%s), want %v", c.box, c.writer, ok, why, c.ok)
		}
	}
}

// A pack carries the conversation and the sql tool's database, never the login
// or the checkout's snapshots, and a restore in another home continues it.
//
// Mutation: pack the whole session directory, or config.json — the members
// differ; drop the restore's move — the second home holds nothing.
func TestACopilotSessionPacksAndRestoresItsOwnFilesAlone(t *testing.T) {
	for _, tool := range []string{"tar", "gzip", "cksum"} {
		prereqtest.OnPath(t, tool, "the case runs the box's pack and restore", "install "+tool)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's pack and restore", "install a POSIX shell")
	const id = "55555555-5555-4555-8555-555555555555"
	from := Paths{Home: t.TempDir()}
	dir := from.CopilotSessionDir(id)
	for name, body := range map[string]string{
		dir + "/events.jsonl":               string(copilotFixture(t, "steered.events.jsonl")),
		dir + "/session.db":                 "SQLite format 3\x00",
		dir + "/workspace.yaml":             "cwd: /box/repo\n",
		dir + "/rewind-file-snapshots/a":    "checkout bytes",
		from.CopilotHome() + "/config.json": `{"copilotTokens":{}}`,
		from.WorkDir() + "/.keep":           "",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest := from.WorkDir() + "/pack.tgz"
	pack := (Copilot{}).Pack(from, id, from.WorkDir()+"/staged", dest, false)
	if out, err := exec.Command(sh, "-c", pack).CombinedOutput(); err != nil {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, h.Name)
	}
	want := []string{"session-state/" + id + "/events.jsonl", "session-state/" + id + "/session.db"}
	if !slices.Equal(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}

	to := Paths{Home: t.TempDir()}
	if err := os.MkdirAll(to.WorkDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to.SessionCarried(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := copilotSessionGate(Session{ID: id, Continues: true, Fresh: "fresh-id"}, to, false) +
		`printf '%s\n' "$s" "$k" "$w" "$@"`
	out, err := exec.Command(sh, "-c", "set --\n"+gate).CombinedOutput()
	if err != nil {
		t.Fatalf("gate: %v\n%s", err, out)
	}
	if got := string(out); got != id+"\nyes\n\n--session-id\n"+id+"\n" {
		t.Fatalf("the gate said %q, want the carried session continued", got)
	}
	if _, err := os.Stat(to.CopilotSessionDir(id) + "/events.jsonl"); err != nil {
		t.Fatalf("the restored session is not in the second home: %v", err)
	}
}
