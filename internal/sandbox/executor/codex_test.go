package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// codexBridgeURL is a bridge endpoint whose path carries its job's token, as
// every bridge endpoint's does (mcpbridge.PathPrefix).
const codexBridgeURL = "http://127.0.0.1:9/mcp/run-token-placeholder"

// recorded is a file the stand-in recorded, a line each.
func recorded(t *testing.T, record, name string) []string {
	t.Helper()
	return lines(t, filepath.Join(record, name))
}

// A CODEX RUN REPORTS THE THREAD ITS HARNESS MINTED AND IS CHARGED ITS ROLLOUT:
// its session is the thread `exec --json` printed, reported whole before
// collection; its answer the last agent message; its spend, window and plan
// limits the rollout lines its script appended, never turn.completed's thread
// total. The brief is on stdin; the bridge's url, which carries the job's
// token, is in the configuration file and on no command line; the posture is
// on the server's argv; CODEX_HOME is the box's own.
//
// Mutation: charge turn.completed's usage, put the MCP servers on argv, drop
// the thread watcher, or the CODEX_HOME pin, and this goes red.
func TestACodexRunReportsTheThreadItsHarnessMintedAndIsChargedItsRollout(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	const brief = "fix the flake: \"quoted\", $dollar and `ticks`"
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{
		Brief: brief, System: "You are the seat.", Model: "gpt-test", Effort: "high",
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true},
		},
	})
	if !res.Success || res.Text != "done" {
		t.Fatalf("the run reads %q (success %v, error %q), want the answer done", res.Text, res.Success, res.Error)
	}
	if res.SessionID != codexStandInThread {
		t.Errorf("the run's session is %q, want the thread Codex minted %q", res.SessionID, codexStandInThread)
	}
	if got := boxFile(t, box, p.SessionReport()); got != codexStandInThread+"\n" {
		t.Errorf("the session report holds %q, want the thread on one line", got)
	}
	if res.InputTokens != 300 || res.CacheReadTokens != 200 || res.OutputTokens != 40 {
		t.Errorf("the run is charged %d in (%d cached), %d out; want the rollout turn's 300 (200), 40",
			res.InputTokens, res.CacheReadTokens, res.OutputTokens)
	}
	if res.ReportedWindow != 272000 {
		t.Errorf("the reported window is %d, want the raw 272000 behind the rollout's 258400", res.ReportedWindow)
	}
	if !strings.Contains(res.Limits, `"300":{"used":0.375`) {
		t.Errorf("the limit report %q holds no five-hour window", res.Limits)
	}

	server := recorded(t, record, "server-argv")
	for _, want := range []string{"--listen", "unix://" + p.CodexSocket(), `sandbox_mode="danger-full-access"`,
		`approval_policy="never"`, `shell_environment_policy.inherit="all"`, `model="gpt-test"`,
		`model_reasoning_effort="high"`} {
		if !slices.Contains(server, want) {
			t.Errorf("the app-server's argv lacks %q: %q", want, server)
		}
	}
	if exec := recorded(t, record, "exec-argv"); !slices.Equal(exec,
		[]string{"exec", "--json", "--remote", "unix://" + p.CodexSocket(), "-"}) {
		t.Errorf("exec's argv is %q", exec)
	}
	for _, argv := range [][]string{server, recorded(t, record, "exec-argv")} {
		if strings.Contains(strings.Join(argv, " "), "run-token-placeholder") {
			t.Errorf("a command line carries the bridge's token: %q", argv)
		}
	}
	// THE SERVER WORKS WHERE EXEC DOES: under --remote a relative path and an
	// item's cwd resolve against the server's directory (CODEX-MEASURE §18).
	if server, exec := recorded(t, record, "server-cwd"), recorded(t, record, "exec-cwd"); !slices.Equal(server, exec) {
		t.Errorf("the app-server runs in %q and exec in %q", server, exec)
	}
	if stdin := strings.Join(recorded(t, record, "stdin"), "\n"); stdin != brief {
		t.Errorf("exec read %q on stdin, want the brief", stdin)
	}
	if !slices.Contains(recorded(t, record, "env"), "CODEX_HOME="+p.CodexHome()) {
		t.Error("exec ran without the box's own CODEX_HOME")
	}
	cfg := boxFile(t, box, p.CodexLiveConfig())
	for _, want := range []string{`url = "` + codexBridgeURL + `"`, "required = true",
		`developer_instructions = "You are the seat.`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the configuration lacks %q:\n%s", want, cfg)
		}
	}
}

// A CODEX RUN THAT CONTINUES ITS SESSION RESUMES IT AND IS CHARGED ITS OWN
// TURNS: `exec --remote … resume <id>`, the record naming the session it
// continued, and the session's earlier turns, in the same rollout, never
// charged again.
//
// Mutation: drop the line count before the run, or the resume argument, and
// this goes red.
func TestACodexRunThatContinuesItsSessionIsChargedOnlyItsOwnTurns(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	const thread = "019a1f00-0000-7000-8000-000000000001"
	earlier := `{"timestamp":"2026-11-21T09:00:00.000Z","ordinal":1,"type":"token_usage_record","payload":{"turn_id":"t1","turn_token_usage":{"input_tokens":5000,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":700,"reasoning_output_tokens":0,"total_tokens":5700}}}` + "\n"
	if err := box.WriteFile(t.Context(), p.CodexSessions()+"/2026/11/21/rollout-2026-11-21T09-00-00-"+thread+".jsonl",
		[]byte(earlier)); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go on", Resume: thread})
	if exec := recorded(t, record, "exec-argv"); !slices.Equal(exec,
		[]string{"exec", "--json", "--remote", "unix://" + p.CodexSocket(), "resume", thread, "-"}) {
		t.Errorf("exec's argv is %q, want a resume of %s", exec, thread)
	}
	if res.SessionID != thread || res.FellBack != "" {
		t.Errorf("the run's session is %q (fell back %q), want the one it continued", res.SessionID, res.FellBack)
	}
	if res.InputTokens != 300 || res.OutputTokens != 40 {
		t.Errorf("the run is charged %d in, %d out; want its own turn's 300, 40", res.InputTokens, res.OutputTokens)
	}
}

// A CODEX RUN ASKED TO CONTINUE A SESSION ITS BOX CANNOT TAKE UP STARTS A NEW
// ONE: no resume, the record saying why, the brief written for a new session,
// and the session the one Codex minted.
//
// Mutation: resume whatever the gate said, or keep the continued brief, and
// this goes red.
func TestACodexRunAskedToContinueASessionItsBoxLacksStartsANewOne(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go on", FreshBrief: "start over",
		Resume: "019a1f00-0000-7000-8000-00000000dead"})
	if exec := recorded(t, record, "exec-argv"); slices.Contains(exec, "resume") {
		t.Errorf("exec resumed a session its box does not hold: %q", exec)
	}
	if stdin := strings.Join(recorded(t, record, "stdin"), "\n"); stdin != "start over" {
		t.Errorf("exec read %q, want the brief written for a new session", stdin)
	}
	if res.SessionID != codexStandInThread || res.FellBack == "" {
		t.Errorf("the run's session is %q (fell back %q), want the thread Codex minted, and why",
			res.SessionID, res.FellBack)
	}
}

// A BOX WHOSE CODEX IS OLDER THAN THE RUNNER'S FLOOR STARTS NOTHING, and says
// which release it runs and which it needs.
//
// Mutation: drop the floor's check, and the stand-in runs.
func TestACodexOlderThanItsFloorStartsNothing(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.170.9")
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if res.Success {
		t.Fatal("a run on Codex 0.170.9 succeeded")
	}
	if _, err := os.Stat(filepath.Join(record, "exec-argv")); err == nil {
		t.Error("Codex 0.170.9 was run")
	}
	if got := boxFile(t, box, executor.PathsFor(box).Err()); !strings.Contains(got, "0.170.9") ||
		!strings.Contains(got, "0.171.0 or later") {
		t.Errorf("the error stream does not name the release and the floor:\n%s", got)
	}
}

// codexRealBoxsNotesReachTheBridgesRoute is Codex's case of the contract's
// route: a real box wired to a real bridge's notes route runs the note helper
// the engine embeds, which polls the route the script handed it in its
// environment and steers the note, whole, into the thread `exec --json`
// reported, on the turn in flight.
func codexRealBoxsNotesReachTheBridgesRoute(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	if err := os.WriteFile(filepath.Join(record, "wait-steer"), nil, 0o600); err != nil {
		t.Fatalf("arm the stand-in: %v", err)
	}
	note := mcpbridge.SteerNote{Seq: 1, ID: "req-7", Inbox: executor.InboxID(executor.CodexName, "job-1", "req-7"),
		Message: probeNote}
	endpoint := probeBridge(t, &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)},
		&probeLedger{})
	runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go",
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)}})
	steers := recorded(t, record, "steers")
	if len(steers) == 0 {
		t.Fatalf("no note was steered into the run:\n%s", boxFile(t, box, executor.PathsFor(box).Err()))
	}
	var steer struct {
		ThreadID       string `json:"threadId"`
		ExpectedTurnID string `json:"expectedTurnId"`
		Input          []struct{ Type, Text string }
	}
	if err := json.Unmarshal([]byte(steers[0]), &steer); err != nil {
		t.Fatalf("the steer %q: %v", steers[0], err)
	}
	if steer.ThreadID != codexStandInThread || steer.ExpectedTurnID != codexStandInTurn ||
		len(steer.Input) != 1 || steer.Input[0].Text != probeNote {
		t.Errorf("the steer was %+v, want the note on thread %s's turn in flight", steer, codexStandInThread)
	}
}

// codexConfirmsATake is Codex's case of the contract's confirm: a take is
// held where the session's rollout records its item as a user message, by
// the item's digest; never where the item is only in a compaction's summary
// or on a torn last line; and a rollout the box does not hold decides nothing.
func codexConfirmsATake(t *testing.T) {
	t.Parallel()
	runner, box, _ := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	const thread = "019a1f00-0000-7000-8000-0000000000c0"
	user := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"timestamp": "2026-11-22T09:00:00.000Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": text}}}})
		return string(raw)
	}
	held, summarised, torn := probeNote, probeNote+" (folded)", probeNote+" (torn)"
	// A COMPACTION'S SUMMARY is a user message Codex marks as its own
	// (content_item_kinds, measured on 0.171.0).
	summary, _ := json.Marshal(map[string]any{"timestamp": "2026-11-22T09:00:01.000Z", "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": summarised}},
			"internal_chat_message_metadata_passthrough": map[string]any{
				"content_item_kinds": []string{"compaction.summary"}}}})
	rollout := user(held) + "\n" + string(summary) + "\n" + user(torn)
	if err := box.WriteFile(t.Context(), p.CodexSessions()+"/rollout-2026-11-22T09-00-00-"+thread+".jsonl", []byte(rollout)); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}
	take := func(id, text string) sandbox.AskedTake {
		return sandbox.AskedTake{Take: id, Digest: executor.ItemDigest(text)}
	}
	got := runner.ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: thread,
		Takes: []sandbox.AskedTake{take("held", held), take("summarised", summarised), take("torn", torn)}})
	if !slices.Equal(got.Receipts.Held, []string{"held"}) {
		t.Errorf("the rollout holds %q, want the user message's take alone", got.Receipts.Held)
	}
	none := runner.ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{
		Session: "019a1f00-0000-7000-8000-00000000abcd", Takes: []sandbox.AskedTake{take("held", held)}})
	if len(none.Receipts.Held) != 0 || none.Receipts.Why == "" {
		t.Errorf("a session the box holds no rollout of reads %+v, want undecided with why", none.Receipts)
	}
}

// codexWakeRead is what a Codex session handed wake as a steered user message
// reads back as: its archive, the rollout under the name it is restored by.
func codexWakeRead(t *testing.T, wake string) []executor.TranscriptEntry {
	t.Helper()
	const thread = "019a1f00-0000-7000-8000-0000000000aa"
	line := func(text string) string {
		raw, _ := json.Marshal(map[string]any{"timestamp": "2026-11-22T09:00:00.000Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": text}}}})
		return string(raw) + "\n"
	}
	body := line("Fix the flake.") + line(wake)
	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "rollout-2020-01-01T00-00-00-" + thread + ".jsonl", Mode: 0o600,
		Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	got, err := executor.ReadTranscriptWithin(t.Context(), executor.CodexName, executor.Codex{}.ArchiveFormat(),
		opener(bytes.NewReader(archive.Bytes())), 0, executor.TranscriptReadBounds())
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
	return got.Entries
}

// codexInstructions is the standing instructions a Codex run's configuration
// hands it, read back from their TOML string.
func codexInstructions(t *testing.T, box sandbox.Sandbox) string {
	t.Helper()
	for _, line := range strings.Split(boxFile(t, box, executor.PathsFor(box).CodexLiveConfig()), "\n") {
		if value, ok := strings.CutPrefix(line, "developer_instructions = "); ok {
			// TOML's basic-string escapes are a subset of Go's.
			text, err := strconv.Unquote(value)
			if err != nil {
				t.Fatalf("the instructions %s do not read back: %v", value, err)
			}
			return text
		}
	}
	return ""
}

// A CODEX RUN IS TOLD OF NOTES EXACTLY WHEN IT IS WIRED TO THEM, in Codex's own
// words, after the report instruction: told without the wiring it describes a
// channel that never opens.
//
// Mutation: append the instruction whatever the wiring, or before the report
// instruction, and a case goes red.
func TestACodexRunIsToldOfNotesExactlyWhenWiredToThem(t *testing.T) {
	t.Parallel()
	for name, steer := range map[string]sandbox.SteerWiring{
		"wired to its notes": {Server: "crewlet", Tool: mcpbridge.SteerTool, URL: codexBridgeURL + "/steer"},
		"wired to none":      {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, _ := codexBox(t, "0.171.0")
			runToEnd(t, runner, box, sandbox.RunRequest{System: seatSystem, Brief: "fix the flake", Steer: steer})
			told := codexInstructions(t, box)
			at := strings.Index(told, executor.CodexSteerInstruction)
			if got, want := at >= 0, steer.Set(); got != want {
				t.Fatalf("the run is told of notes: %v, want %v:\n%s", got, want, told)
			}
			if !strings.HasPrefix(told, seatSystem) {
				t.Errorf("the instructions do not open with the seat's own:\n%s", told)
			}
			if at >= 0 && at < strings.Index(told, executor.PathsFor(box).Findings()) {
				t.Errorf("the note instruction precedes the report instruction:\n%s", told)
			}
		})
	}
}

// A CODEX RUN READS ITS LOGIN WHERE A LOCAL BOX IS SEEDED WITH IT AND WRITES
// IT BACK FROM: the codex profile's credential path, relative to the box's
// home, is auth.json in the CODEX_HOME the run is pinned to, so the login the
// engine seeds is the one Codex signs in with and refreshes in place, and the
// teardown's compare-and-swap writes the rotated tokens back (internal/sandbox).
//
// Mutation: pin CODEX_HOME anywhere but the box home's .codex, and this goes red.
func TestACodexRunReadsItsLoginWhereALocalBoxIsSeededWithIt(t *testing.T) {
	t.Parallel()
	profile, ok := harnessprofile.Builtin(executor.CodexName)
	if !ok || len(profile.CredentialPaths) == 0 {
		t.Fatal("the codex profile names no credential file")
	}
	p := executor.Paths{Home: "/home/box"}
	pins := executor.Codex{}.Pins(t.Context(), sandbox.RunRequest{}, p)
	home := ""
	for _, pin := range pins {
		if pin.Name == "CODEX_HOME" {
			home = pin.Value
		}
	}
	for _, rel := range profile.CredentialPaths {
		if got := p.Home + "/" + rel; got != home+"/auth.json" {
			t.Errorf("a local box is seeded with %s, and the run reads its login from %s/auth.json", got, home)
		}
	}
}

// A CODEX RUN STARTS FROM ITS OWN CONFIGURATION, every run: whatever an
// earlier run or the model left in CODEX_HOME's config.toml is replaced whole
// by the run's, readable by the box's user alone.
//
// Mutation: append to the file, or leave a stale one where none was staged,
// and this goes red.
func TestACodexRunStartsFromItsOwnConfigurationEveryRun(t *testing.T) {
	t.Parallel()
	runner, box, _ := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	if err := box.WriteFile(t.Context(), p.CodexLiveConfig(),
		[]byte("[mcp_servers.left]\nurl = \"http://127.0.0.1:9/mcp/stale-token\"\n")); err != nil {
		t.Fatalf("leave a configuration behind: %v", err)
	}
	runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}}})
	cfg := boxFile(t, box, p.CodexLiveConfig())
	if strings.Contains(cfg, "stale-token") || !strings.Contains(cfg, codexBridgeURL) {
		t.Errorf("the run's configuration is not its own:\n%s", cfg)
	}
	info, err := os.Stat(p.CodexLiveConfig())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the configuration's mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}
}

// NO COMMAND LINE AND NO MODEL SHELL CARRIES THE BRIDGE'S TOKEN, which is in
// the path of every bridge URL: the run's script, the app-server's and exec's
// argv, and the helper's command line name files, and the notes and skill
// routes reach the helper's environment alone, which the model's shell policy
// excludes too.
//
// Mutation: put the MCP url or the notes route on a command line, or export
// the route to Codex, and this goes red.
func TestNoCodexCommandLineCarriesTheBridgesToken(t *testing.T) {
	t.Parallel()
	const token = "run-token-placeholder"
	req := sandbox.RunRequest{Brief: "go", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: codexBridgeURL + "/steer"},
		Skills:     []sandbox.Skill{{Name: "deploy", Description: "How we deploy.", Body: "Run make deploy.", Required: true}},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: mcpbridge.SkillTool, URL: codexBridgeURL + "/skills"}}
	if script := (executor.Codex{}).Command(req, executor.Session{}, executor.Paths{Home: "/home/box"}, ""); strings.Contains(script, token) {
		t.Errorf("the run's script carries the token:\n%s", script)
	}
	runner, box, record := codexBox(t, "0.171.0")
	runToEnd(t, runner, box, req)
	for _, file := range []string{"server-argv", "exec-argv", "env"} {
		if got := strings.Join(recorded(t, record, file), "\n"); strings.Contains(got, token) ||
			strings.Contains(got, "CREWLET_NOTES_URL") || strings.Contains(got, "CREWLET_SKILLS_URL") {
			t.Errorf("%s carries the bridge's route:\n%s", file, got)
		}
	}
	p := executor.PathsFor(box)
	// AND THE MODEL'S SHELL IS HANDED NONE OF THEM: every credential name the
	// codex profile signs in by, and both routes, are excluded from the
	// environment Codex hands the commands it runs (its shell_environment_policy).
	profile, _ := harnessprofile.Builtin(executor.CodexName)
	excluded := codexShellExcluded(t, recorded(t, record, "server-argv"))
	for _, name := range append(profile.CredentialEnvNames(nil), "CREWLET_NOTES_URL", "CREWLET_SKILLS_URL") {
		if !slices.Contains(excluded, name) {
			t.Errorf("the model's shell may be handed %s: the policy excludes %q", name, excluded)
		}
	}
	if got := boxFile(t, box, p.CodexNotesURL()); got != codexBridgeURL+"/steer" {
		t.Errorf("the helper's notes route file holds %q", got)
	}
	if got := boxFile(t, box, p.CodexSkillsURL()); got != codexBridgeURL+"/skills" {
		t.Errorf("the helper's skill route file holds %q", got)
	}
	// A REQUIRED SKILL IS HANDED, its loads the helper's to report.
	if got := boxFile(t, box, p.CodexSkills()+"/crewlet:deploy/SKILL.md"); !strings.Contains(got, "Run make deploy.") {
		t.Errorf("the run's skill was not written: %q", got)
	}
}

// A CODEX RUN TORN DOWN ENDS IN ORDER WITHIN ITS GRACE: on the group's
// SIGTERM, exec goes first, then the app-server, sent SIGTERM again since it
// finishes the turn in flight on the first (measured on 0.171.0), and the
// session is packed before the run exits.
//
// Mutation: stop the app-server before exec, send it one SIGTERM, or drop the
// pack from the teardown, and this goes red.
func TestACodexRunTornDownEndsInOrderAndPacksItsSession(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	if err := os.WriteFile(filepath.Join(record, "block"), nil, 0o600); err != nil {
		t.Fatalf("arm the stand-in: %v", err)
	}
	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
	if err := box.KillJob(t.Context(), handle.CommandID); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	awaitOver(t, runner, box, handle, 15*time.Second, "the job was torn down")
	if order := recorded(t, record, "order"); !slices.Equal(order, []string{"exec", "server"}) {
		t.Errorf("the teardown stopped %q, want exec, then the app-server", order)
	}
	if boxFile(t, box, executor.PathsFor(box).SessionCapture()) == "" {
		t.Error("the torn-down run packed no session")
	}
}

// A CODEX RUN ENDS AT ITS ROUND CAP, role.sandbox.max_turns, counted in model
// responses as Claude Code's --max-turns counts them: Codex has no cap of its
// own, so the note helper counts the turn's responses off the app-server,
// marks the cap and interrupts the turn, and the run reads as capped. A later
// run continues the session.
//
// Mutation: start no helper for a capped run, drop -step-cap, or read the cap
// without the helper's marker, and this goes red.
func TestACodexRunEndsAtItsRoundCapAndItsSessionGoesOn(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	for name, body := range map[string]string{"block": "", "steps": "3"} {
		if err := os.WriteFile(filepath.Join(record, name), []byte(body), 0o600); err != nil {
			t.Fatalf("arm the stand-in: %v", err)
		}
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Limits: sandbox.Limits{MaxTurns: 2}})
	if res.Success || !res.RoundCapped {
		t.Fatalf("a run past its cap collects success %v, capped %v (%q):\n%s", res.Success, res.RoundCapped,
			res.Error, boxFile(t, box, executor.PathsFor(box).Err()))
	}
	if len(recorded(t, record, "interrupted")) != 1 {
		t.Error("the turn was not interrupted once at its cap")
	}
	for _, name := range []string{"block", "steps"} {
		if err := os.Remove(filepath.Join(record, name)); err != nil {
			t.Fatal(err)
		}
	}
	_, again := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go on", Resume: res.SessionID})
	if exec := recorded(t, record, "exec-argv"); !slices.Contains(exec, "resume") || !again.Success {
		t.Errorf("the capped session was not continued: %q, success %v (%q)", exec, again.Success, again.Error)
	}
}

// A CODEX RUN WHOSE HELPER ENDS UNDER IT FAILS CLOSED: its guard interrupts the
// turn in flight and the run fails, never going on past a cap no helper holds,
// and never reads as capped.
//
// Mutation: drop the guard, or read its interrupt as the cap, and this goes red.
func TestACodexRunWhoseHelperEndsUnderItFails(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	if err := os.WriteFile(filepath.Join(record, "block"), nil, 0o600); err != nil {
		t.Fatalf("arm the stand-in: %v", err)
	}
	p := executor.PathsFor(box)
	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go", Limits: sandbox.Limits{MaxTurns: 50}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
	killProcess(t, p.CodexNoteHelper()+" -adapter")
	awaitOver(t, runner, box, handle, 15*time.Second, "the helper was killed")
	res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Success || res.RoundCapped || !strings.Contains(res.Error, "note helper ended") {
		t.Errorf("a run whose helper ended collects success %v, capped %v, %q", res.Success, res.RoundCapped, res.Error)
	}
	if len(recorded(t, record, "interrupted")) != 1 {
		t.Error("the guard did not interrupt the turn")
	}
}

// killProcess sends SIGKILL to the one process whose command line begins with
// prefix, failing the case where there is none.
func killProcess(t *testing.T, prefix string) {
	t.Helper()
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline")) //nolint:gosec // a process's own record
		if err != nil || !strings.HasPrefix(strings.ReplaceAll(string(raw), "\x00", " "), prefix) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("killing %d: %v", pid, err)
		}
		return
	}
	t.Fatalf("no process runs %s", prefix)
}

// codexShellExcluded is the names the app-server's argv excludes from the
// environment of the model's shell commands (shell_environment_policy.exclude).
func codexShellExcluded(t *testing.T, argv []string) []string {
	t.Helper()
	for _, arg := range argv {
		if value, ok := strings.CutPrefix(arg, "shell_environment_policy.exclude="); ok {
			var names []string
			if err := json.Unmarshal([]byte(value), &names); err != nil {
				t.Fatalf("the shell policy %q does not read as a list: %v", value, err)
			}
			return names
		}
	}
	t.Fatalf("the app-server's argv sets no shell policy: %q", argv)
	return nil
}
