package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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
// Mutation: charge turn.completed's usage, put the MCP servers on argv, or
// drop the CODEX_HOME pin, and this goes red. (The report made while the run
// is live is TestACodexRunEndedOnRequestStopsExecThenItsServer's.)
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
	cfg := codexConfigSeen(t, record)
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
// charged again; where it began in the rollout is recorded for its notes'
// confirm, past the earlier lines.
//
// Mutation: drop the line count before the run, or the resume argument, and
// this goes red.
func TestACodexRunThatContinuesItsSessionIsChargedOnlyItsOwnTurns(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	const thread = "019a1f00-0000-7000-8000-000000000001"
	earlier := codexInstructionsLine("an earlier launch's") + "\n" + `{"timestamp":"2026-11-21T09:00:00.000Z","ordinal":1,"type":"token_usage_record","payload":{"turn_id":"t1","turn_token_usage":{"input_tokens":5000,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":700,"reasoning_output_tokens":0,"total_tokens":5700}}}` + "\n"
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
	if got := boxFile(t, box, p.CodexRunStart()); got != thread+" 2\n" {
		t.Errorf("the run began at %q in its session, want past the 2 earlier lines", got)
	}
}

// A CONTINUED CODEX RUN IS HANDED ITS OWN STANDING INSTRUCTIONS AS CURRENT, not
// the earlier launch's its session recorded: Codex resumes on the developer
// message its rollout replays and sends nothing of the configuration again
// (measured on 0.171.0), so the box's note helper renews that message to the
// run's text before the resume. The rollout Codex resumes holds the run's
// standing instructions, exactly as its configuration hands them, and no word
// of the earlier ones; a rollout that records none starts a session of its own.
//
// Mutation: run no renew in the session gate, renew from config.toml's old
// text, or resume a rollout the helper refused, and this goes red.
func TestAContinuedCodexRunIsHandedItsOwnStandingInstructionsAsCurrent(t *testing.T) {
	t.Parallel()
	const thread = "019a1f00-0000-7000-8000-000000000002"
	for name, c := range map[string]struct {
		seed      string
		continued bool
	}{
		"recorded":      {codexInstructionsLine("EARLIER LAUNCH: thread so far, old memory"), true},
		"none recorded": {`{"timestamp":"2026-11-21T09:00:00.000Z","ordinal":1,"type":"turn_context","payload":{"turn_id":"t1"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, record := codexBox(t, "0.171.0")
			p := executor.PathsFor(box)
			if err := box.WriteFile(t.Context(), p.CodexSessions()+"/2026/11/21/rollout-2026-11-21T09-00-00-"+thread+".jsonl",
				[]byte(c.seed+"\n")); err != nil {
				t.Fatalf("seed the session: %v", err)
			}
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go on", FreshBrief: "start over",
				Resume: thread, System: "THIS LAUNCH: thread so far, <new> memory & knowledge"})
			if !c.continued {
				if res.SessionID == thread || !strings.Contains(res.FellBack, "could not be renewed") {
					t.Errorf("the run continued (%q, fell back %q) a session recording no standing instructions",
						res.SessionID, res.FellBack)
				}
				return
			}
			if res.SessionID != thread || res.FellBack != "" {
				t.Fatalf("the run's session is %q (fell back %q), want the one it continued:\n%s", res.SessionID,
					res.FellBack, boxFile(t, box, p.Err()))
			}
			resumed := strings.Join(recorded(t, record, "rollout-resumed"), "\n")
			want := codexInstructions(t, record)
			if !strings.Contains(want, "THIS LAUNCH") {
				t.Fatalf("the run's configuration hands Codex %q, not its standing instructions", want)
			}
			if got := codexStandingItems(t, resumed); !slices.Equal(got, []string{want}) ||
				strings.Contains(resumed, "EARLIER LAUNCH") {
				t.Errorf("Codex resumed on standing instructions %q, want the run's own %q and no earlier ones",
					got, want)
			}
		})
	}
}

// codexStandingItems is the text of each item rollout records standing
// instructions in (content kind generic.developer_instructions), in order.
func codexStandingItems(t *testing.T, rollout string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(rollout, "\n") {
		var l struct {
			Payload struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				Meta struct {
					Kinds []string `json:"content_item_kinds"`
				} `json:"internal_chat_message_metadata_passthrough"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &l) != nil || len(l.Payload.Meta.Kinds) != len(l.Payload.Content) {
			continue
		}
		for i, kind := range l.Payload.Meta.Kinds {
			if kind == "generic.developer_instructions" {
				out = append(out, l.Payload.Content[i].Text)
			}
		}
	}
	return out
}

// A CODEX RUN ASKED TO CONTINUE A SESSION ITS BOX CANNOT TAKE UP STARTS A NEW
// ONE: no resume, the record saying why, the brief written for a new session,
// and the session the one Codex minted.
//
// Mutation: resume whatever the gate said, keep the continued brief, or count
// the asked session's lines for the new one, and this goes red.
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
	if got := boxFile(t, box, executor.PathsFor(box).CodexRunStart()); got != codexStandInThread+" 0\n" {
		t.Errorf("the new session's run began at %q, want its first line", got)
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

// A CODEX BOX CONTINUES ITS OWN COPY OF A SESSION ONLY ON THE ONE SUPPORTED
// RELEASE, as the engine asks it before a relaunch in the box hands the brief
// that continues the copy: exactly 0.171.0 does; a newer, older or pre-release
// Codex does not, naming what it found. A runner whose harness holds no
// release to its own copy answers yes without asking the box.
//
// Mutation: answer yes for every Codex, compare releases by order, or read a
// pre-release as its numbers, and a row goes red.
func TestACodexBoxTakesUpItsOwnCopyOnlyOnTheOneSupportedRelease(t *testing.T) {
	t.Parallel()
	for release, want := range map[string]string{
		"0.171.0": "", "0.172.0": "Codex 0.172.0", "0.170.9": "Codex 0.170.9",
		"0.171.0-alpha.1": "named no release",
	} {
		runner, box, _ := codexBox(t, release)
		// THE COPY 0.171.0 WROTE, as its last run's record names it, so only
		// the box's own release decides here.
		if err := box.WriteFile(t.Context(), executor.PathsFor(box).SessionRecord(),
			[]byte("id s-1\ncontinued yes\nwhy \nrelease codex-cli 0.171.0\n")); err != nil {
			t.Fatal(err)
		}
		check, err := runner.CheckInPlace(t.Context(), box)
		if err != nil {
			t.Fatalf("%s: CheckInPlace: %v", release, err)
		}
		if check.Continues != (want == "") || !strings.Contains(check.Why, want) {
			t.Errorf("%s: continues its copy %v (%q), want %v naming %q", release, check.Continues, check.Why,
				want == "", want)
		}
	}
	_, box, _ := codexBox(t, "0.172.0")
	if check, err := executor.NewClaudeCode().CheckInPlace(t.Context(), box); err != nil || !check.Continues {
		t.Errorf("a Claude Code runner answers %+v, %v for its own copy, want it continued unasked", check, err)
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
// held where the run's part of the session's rollout records its item as a
// user message, by the item's digest, and the model replied after it; never
// where the item is only in a compaction's summary or on a torn last line;
// and a rollout the box does not hold decides nothing.
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
	reply, _ := json.Marshal(map[string]any{"timestamp": "2026-11-22T09:00:00.500Z", "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "assistant",
			"content": []map[string]string{{"type": "output_text", "text": "On it."}}}})
	rollout := user(held) + "\n" + string(reply) + "\n" + string(summary) + "\n" + string(reply) + "\n" + user(torn)
	if err := box.WriteFile(t.Context(), p.CodexSessions()+"/rollout-2026-11-22T09-00-00-"+thread+".jsonl", []byte(rollout)); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}
	if err := box.WriteFile(t.Context(), p.CodexRunStart(), []byte(thread+" 0\n")); err != nil {
		t.Fatalf("write where the run began: %v", err)
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

// codexConfigSeen is the config.toml the stand-in app-server found as it
// started: what Codex loads, the home's own being back once the run ends.
func codexConfigSeen(t *testing.T, record string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(record, "config.toml"))
	if err != nil {
		t.Fatalf("the app-server found no configuration: %v", err)
	}
	return string(raw)
}

// codexInstructions is the standing instructions a Codex run's configuration
// hands it, read back from their TOML string.
func codexInstructions(t *testing.T, record string) string {
	t.Helper()
	for _, line := range strings.Split(codexConfigSeen(t, record), "\n") {
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
			runner, box, record := codexBox(t, "0.171.0")
			runToEnd(t, runner, box, sandbox.RunRequest{System: seatSystem, Brief: "fix the flake", Steer: steer})
			told := codexInstructions(t, record)
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

// A CODEX RUN LOADS THE HOME'S CONFIGURATION WITH ITS OWN OVER IT, AND PUTS
// THE HOME'S BACK: as Codex starts, its config.toml holds the home's own MCP
// server and instructions beside the run's, the run's server in place of the
// home's of the same name, readable by the box's user alone; once the run
// ends the home's file is back byte for byte. Where an earlier run's
// configuration was left installed, the home's held copy is the base, never
// that run's.
//
// Mutations, each red: install the run's alone; leave the run's installed;
// put nothing held back as the run starts.
func TestACodexRunLoadsTheHomesConfigurationWithItsOwnOverIt(t *testing.T) {
	t.Parallel()
	const own = "developer_instructions = \"HOME-INSTRUCTIONS\"\n\n[mcp_servers.home]\ncommand = \"home-mcp\"\n\n" +
		"[mcp_servers.crewlet]\nurl = \"http://127.0.0.1:9/mcp/home-token\"\n"
	for name, leftInstalled := range map[string]bool{"the home's own installed": false,
		"an earlier run's left installed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, record := codexBox(t, "0.171.0")
			p := executor.PathsFor(box)
			live := own
			if leftInstalled {
				live = "[mcp_servers.stale]\nurl = \"http://127.0.0.1:9/mcp/stale-token\"\n"
				if err := box.WriteFile(t.Context(), p.CodexHomeConfig(), []byte(own)); err != nil {
					t.Fatal(err)
				}
			}
			if err := box.WriteFile(t.Context(), p.CodexLiveConfig(), []byte(live)); err != nil {
				t.Fatal(err)
			}
			runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: seatSystem,
				MCPServers: map[string]sandbox.MCPServer{
					"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}}})
			cfg := codexConfigSeen(t, record)
			for _, want := range []string{"home-mcp", codexBridgeURL, "HOME-INSTRUCTIONS", seatSystem[:20]} {
				if !strings.Contains(cfg, want) {
					t.Errorf("Codex started without %q:\n%s", want, cfg)
				}
			}
			for _, gone := range []string{"home-token", "stale-token"} {
				if strings.Contains(cfg, gone) {
					t.Errorf("Codex started with %q:\n%s", gone, cfg)
				}
			}
			if told := codexInstructions(t, record); !strings.HasPrefix(told, "HOME-INSTRUCTIONS\n\n"+seatSystem) {
				t.Errorf("the instructions are %q, want the home's, then the run's", told)
			}
			if mode := strings.Join(recorded(t, record, "config-mode"), ""); mode != "600" {
				t.Errorf("the installed configuration's mode is %s, want 600", mode)
			}
			if got := boxFile(t, box, p.CodexLiveConfig()); got != own {
				t.Errorf("after the run the home's config.toml is\n%s\nwant its own back:\n%s", got, own)
			}
			if _, err := os.Stat(p.CodexHomeConfig()); !os.IsNotExist(err) {
				t.Errorf("the held copy outlived the run (%v)", err)
			}
		})
	}
}

// A KILLED CODEX RUN'S LEFTOVERS GO AS THE NEXT RUN STARTS: a run whose job is
// killed before its script puts the home's config.toml back leaves its own
// installed, its bridge token in it; the next run's start puts the home's
// back before anything else, so even a run it refuses leaves that token in no
// home file, and the run after starts on the home's own entries with its own
// and puts the home's back byte for byte.
//
// Mutations, each red: put nothing back at the start; hold the live file over
// the held copy.
func TestAKilledCodexRunsLeftoversGoAsTheNextRunStarts(t *testing.T) {
	t.Parallel()
	const own = "[mcp_servers.home]\ncommand = \"home-mcp\"\n"
	const killedToken = "killed-run-token"
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	plant(t, box, map[string]string{".codex/config.toml": own})
	bridge := func(url string) sandbox.RunRequest {
		return sandbox.RunRequest{Brief: "go", MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: url, AlwaysLoaded: true}}}
	}
	plantFile(t, filepath.Join(record, "block"), "")
	handle, err := runner.Start(t.Context(), box, bridge("http://127.0.0.1:9/mcp/"+killedToken))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
	killJob(t, p.BinDir()+"/codex exec")
	awaitOver(t, runner, box, handle, 15*time.Second, "its job was killed")
	if got := boxFile(t, box, p.CodexLiveConfig()); !strings.Contains(got, killedToken) {
		t.Fatalf("the killed run left the home's config.toml as\n%s\nnot its own installed", got)
	}
	if err := os.Remove(filepath.Join(record, "block")); err != nil {
		t.Fatal(err)
	}
	plantFile(t, filepath.Join(record, "config-read"), `{"id":2,"result":{"config":{},"layers":[`+
		`{"name":{"type":"project","dotCodexFolder":"/r/.codex"},"config":{"mcp_servers":{"crewlet":{"url":"http://x"}}}}]}}`)
	if _, err := runner.Start(t.Context(), box, bridge(codexBridgeURL)); err == nil {
		t.Fatal("a run a repository changes the bridge of started")
	}
	if got := boxFile(t, box, p.CodexLiveConfig()); got != own {
		t.Errorf("after a refused next run the home's config.toml is\n%s\nwant its own:\n%s", got, own)
	}
	homeHoldsNo(t, box, killedToken)
	if err := os.Remove(filepath.Join(record, "config-read")); err != nil {
		t.Fatal(err)
	}
	if _, res := runToEnd(t, runner, box, bridge(codexBridgeURL)); !res.Success {
		t.Fatalf("the run after failed: %s", res.Error)
	}
	if cfg := codexConfigSeen(t, record); !strings.Contains(cfg, "home-mcp") || !strings.Contains(cfg, codexBridgeURL) ||
		strings.Contains(cfg, killedToken) {
		t.Errorf("the run after started on\n%s\nwant the home's own server and its own bridge alone", cfg)
	}
	if got := boxFile(t, box, p.CodexLiveConfig()); got != own {
		t.Errorf("after the run the home's config.toml is\n%s\nwant its own back:\n%s", got, own)
	}
	homeHoldsNo(t, box, killedToken)
	homeHoldsNo(t, box, "run-token-placeholder")
}

// homeHoldsNo fails the case where a file of box's home, the run's work
// directory aside, holds token.
func homeHoldsNo(t *testing.T, box sandbox.Sandbox, token string) {
	t.Helper()
	work := executor.PathsFor(box).WorkDir()
	err := filepath.WalkDir(box.Home(), func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path == work:
			return filepath.SkipDir
		case !d.Type().IsRegular():
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test's own box
		if err == nil && bytes.Contains(raw, []byte(token)) {
			t.Errorf("%s holds %s", path, token)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// NO COMMAND LINE AND NO MODEL SHELL CARRIES THE BRIDGE'S TOKEN, which is in
// the path of every bridge URL: the run's script, the app-server's and exec's
// argv, and the helper's command line name files and variables, never a
// route's value; the notes and skill routes reach the helper's environment
// alone, never Codex's, and the model's shell policy excludes them by name,
// beside Crewlet's other variables, the runner's shared strip.
//
// Mutation: put the MCP url or the notes route on a command line, export the
// route to Codex, drop a route from the shell policy's exclude list, or hand
// the helper no digest of the skill as written, and this goes red.
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
	// A VALUE, NEVER A NAME: the shell policy on the server's argv names the
	// routes' variables to exclude them, and carries neither route.
	for _, file := range []string{"server-argv", "exec-argv", "env"} {
		if got := strings.Join(recorded(t, record, file), "\n"); strings.Contains(got, token) ||
			strings.Contains(got, codexBridgeURL) {
			t.Errorf("%s carries the bridge's token or a route:\n%s", file, got)
		}
	}
	for _, kv := range recorded(t, record, "env") {
		if strings.HasPrefix(kv, "CREWLET_NOTES_URL=") || strings.HasPrefix(kv, "CREWLET_SKILLS_URL=") {
			t.Errorf("Codex's environment carries a route meant for the helper alone: %s", kv)
		}
	}
	p := executor.PathsFor(box)
	// AND THE COMMANDS THE MODEL RUNS START WITHOUT THE HELPER'S ROUTES: both
	// are what Codex's shell_environment_policy excludes, and nothing else,
	// the sign-in and every OTEL_ variable included.
	excluded := codexShellExcluded(t, recorded(t, record, "server-argv"))
	if want := []string{"CREWLET_NOTES_URL", "CREWLET_SKILLS_URL"}; !slices.Equal(excluded, want) {
		t.Errorf("the commands the model runs start without %q, want %q", excluded, want)
	}
	if got := boxFile(t, box, p.CodexNotesURL()); got != codexBridgeURL+"/steer" {
		t.Errorf("the helper's notes route file holds %q", got)
	}
	if got := boxFile(t, box, p.CodexSkillsURL()); got != codexBridgeURL+"/skills" {
		t.Errorf("the helper's skill route file holds %q", got)
	}
	// A REQUIRED SKILL IS HANDED, its loads the helper's to report against
	// the digest of the file as written, on the helper's command line, which
	// the runner fixes before the model runs, never in a file the model can
	// write.
	skill := boxFile(t, box, p.CodexSkills()+"/crewlet:deploy/SKILL.md")
	if !strings.Contains(skill, "Run make deploy.") {
		t.Errorf("the run's skill was not written: %q", skill)
	}
	sum := sha256.Sum256([]byte(skill))
	script := (executor.Codex{}).Command(req, executor.Session{}, p, "")
	if !strings.Contains(script, "-skill-digest ") || !strings.Contains(script, "crewlet:deploy="+hex.EncodeToString(sum[:])) {
		t.Errorf("the helper is not handed the written skill's digest on its command line:\n%s", script)
	}
}

// A CODEX RUN TORN DOWN ENDS IN ORDER WITHIN ITS GRACE: on the group's
// SIGTERM, the script's teardown ends exec first, then the app-server, and the
// session is packed before the run exits. The stand-in exec runs in a process
// group of its own, so the group's SIGTERM misses it and the script's own ends
// it: the order is the script's, never the group's.
//
// Mutation: stop the app-server before exec in the teardown, or drop the pack
// from it, and this goes red.
func TestACodexRunTornDownEndsInOrderAndPacksItsSession(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	for name, body := range map[string]string{"block": "", "own-group": ""} {
		if err := os.WriteFile(filepath.Join(record, name), []byte(body), 0o600); err != nil {
			t.Fatalf("arm the stand-in: %v", err)
		}
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

// A CODEX RUN ENDED ON REQUEST STOPS EXEC, THEN ITS SERVER, AND PACKS ITS
// SESSION, every signal the script's own (the end request reaches the script
// alone, never the group): exec is ended first; the app-server, which
// finishes the turn in flight on one SIGTERM (measured on 0.171.0), is sent
// SIGTERM again until it goes, never left to the grace's SIGKILL; the session
// is packed and the end recorded. While the run is live, the thread `exec
// --json` reported is already reported, by the script's watcher.
//
// Mutation: drop the thread watcher, send the app-server one SIGTERM, or end
// it before exec, and this goes red.
func TestACodexRunEndedOnRequestStopsExecThenItsServer(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	if err := os.WriteFile(filepath.Join(record, "block"), nil, 0o600); err != nil {
		t.Fatalf("arm the stand-in: %v", err)
	}
	p := executor.PathsFor(box)
	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
	waitForFile(t, p.SessionReport(), "the thread to be reported while the run is live")
	if got := boxFile(t, box, p.SessionReport()); got != codexStandInThread+"\n" {
		t.Errorf("the live report holds %q, want the thread exec reported", got)
	}
	askEnd(t, runner, box, handle)
	awaitOver(t, runner, box, handle, 15*time.Second, "the end was asked")
	if order := recorded(t, record, "order"); !slices.Equal(order, []string{"exec", "server"}) {
		t.Errorf("the end stopped %q, want exec, then the app-server on the script's own SIGTERMs", order)
	}
	if boxFile(t, box, p.SessionCapture()) == "" {
		t.Error("the ended run packed no session")
	}
	if got := strings.TrimSpace(boxFile(t, box, p.JobEnded())); got != "asked" {
		t.Errorf("the run's end is recorded %q, want asked", got)
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

// A CODEX RUN WHOSE NTH RESPONSE IS ITS FINAL ANSWER COMPLETES, as Claude
// Code's --max-turns lets one: the cap counts model responses per turn, and
// the helper marks it and interrupts at the Nth, but the turn had completed,
// so Codex interrupted nothing and the rollout records no turn aborted. The
// run succeeds, exits 0 and is no round cap.
//
// Mutation: read the cap off the helper's marker alone in the script or the
// result, and this goes red.
func TestACodexRunWhoseNthResponseIsItsFinalAnswerCompletes(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	for name, body := range map[string]string{"final": "", "steps": "2"} {
		if err := os.WriteFile(filepath.Join(record, name), []byte(body), 0o600); err != nil {
			t.Fatalf("arm the stand-in: %v", err)
		}
	}
	p := executor.PathsFor(box)
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Limits: sandbox.Limits{MaxTurns: 2}})
	if len(recorded(t, record, "interrupt-refused")) != 1 || boxFile(t, box, p.CodexCapFired()) == "" {
		t.Fatalf("the helper did not mark its cap and ask the ended turn's interrupt:\n%s", boxFile(t, box, p.Err()))
	}
	if !res.Success || res.RoundCapped || res.HarnessCutOff || res.Text != "done" {
		t.Errorf("a run that answered at its Nth response collects success %v, capped %v, cut off %v, %q (%q)",
			res.Success, res.RoundCapped, res.HarnessCutOff, res.Text, res.Error)
	}
}

// A CODEX RUN WHOSE HELPER ENDS BEFORE ITS TURN STARTS FAILS AT ONCE: with the
// thread reported but no turn in flight, the guard's interrupt cuts nothing
// short, so it stops the app-server rather than let a turn run uncapped, and
// the run fails as one whose helper ended, its turn never interrupted.
//
// Mutation: take -interrupt-once's "no turn in flight" for a turn stopped, or
// stop the app-server only where the helper cannot tell, and the run never
// ends.
func TestACodexRunWhoseHelperEndsBeforeItsTurnStartsFailsAtOnce(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	for _, name := range []string{"block", "idle"} {
		if err := os.WriteFile(filepath.Join(record, name), nil, 0o600); err != nil {
			t.Fatalf("arm the stand-in: %v", err)
		}
	}
	p := executor.PathsFor(box)
	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go", Limits: sandbox.Limits{MaxTurns: 50}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
	waitForFile(t, p.CodexThread(), "the thread to be reported")
	killProcess(t, p.CodexNoteHelper()+" -adapter")
	awaitOver(t, runner, box, handle, 15*time.Second, "the helper was killed before its turn started")
	res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Success || res.RoundCapped || !strings.Contains(res.Error, "note helper ended") {
		t.Errorf("a run whose helper ended before its turn collects success %v, capped %v, %q", res.Success,
			res.RoundCapped, res.Error)
	}
	if !slices.Contains(recorded(t, record, "order"), "server") {
		t.Error("the guard left the app-server running")
	}
	if _, err := os.Stat(filepath.Join(record, "interrupted")); err == nil {
		t.Error("the guard interrupted a turn the stand-in never ran")
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

// killJob sends SIGKILL to the process group of the one process whose
// command line holds marker, a run's job: its script and all it started, as
// a box's end or a node's death takes it, its traps never run.
func killJob(t *testing.T, marker string) {
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
		if err != nil || !strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), marker) {
			continue
		}
		group, err := syscall.Getpgid(pid)
		if err != nil || group == syscall.Getpgrp() {
			t.Fatalf("the job running %s has no group of its own (%d, %v)", marker, group, err)
		}
		if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
			t.Fatalf("killing group %d: %v", group, err)
		}
		return
	}
	t.Fatalf("no process runs %s", marker)
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
