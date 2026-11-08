package executor_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The run that continues a turn's session, on a real box and under the run's
// own script: which brief its harness reads, what is restored over what, the
// transcript cut back to its last whole record, and the environment a
// continued session runs in.

// briefHarness is [sessionHarness] that also records the brief it was handed
// on its stdin, in the box's home.
func briefHarness(release string) string {
	return strings.Replace(sessionHarness(release), "for a; do printf", `cat > "$HOME/stdin"`+"\nfor a; do printf", 1)
}

// briefBox is a real box with a stand-in `claude` of release that records its
// brief ([briefHarness]).
func briefBox(t *testing.T, release string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := realBox(t)
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", briefHarness(release))
	return runner, box
}

// A RUN THAT CANNOT CONTINUE ITS SESSION IS HANDED THE FRESH BRIEF. Whether a
// session is taken up is decided in the box, by its script — the session may be
// neither held there nor restorable — so a run asked to continue one carries
// the brief a session of its own needs beside the one that continues: its
// harness reads the continuation where it takes the session up, and the fresh
// brief where its script started a new session in the session's place, so a
// new session never answers a brief written for a model that remembers.
//
// Mutation: hand the harness the continuation whatever its restore came to, or
// the fresh brief where it continued, and a row goes red.
func TestARunThatCannotContinueItsSessionIsHandedTheFreshBrief(t *testing.T) {
	t.Parallel()
	runner, box := briefBox(t, "2.1.310")
	_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	for _, tc := range []struct {
		name, resume string
		continued    bool
		want         string
	}{
		{name: "its session held", resume: first.SessionID, continued: true, want: "carry on where you stopped"},
		{name: "a session not in it", resume: uuid.NewString(), want: "the task, from the top"},
	} {
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on where you stopped",
			FreshBrief: "the task, from the top", Resume: tc.resume})
		if res.Continued != tc.continued || (!tc.continued && res.FellBack == "") {
			t.Fatalf("%s: the run continued %v (fell back: %q), want %v", tc.name, res.Continued, res.FellBack,
				tc.continued)
		}
		if got := boxFile(t, box, box.Home()+"/stdin"); got != tc.want {
			t.Errorf("%s: the harness read %q, want %q", tc.name, got, tc.want)
		}
	}
}

// AN OPENCODE RUN THAT CANNOT CONTINUE ITS SESSION READS THE FRESH BRIEF, under
// every sh a box may run its script with: `opencode run` reads the brief the
// script points its stdin at, the continuation where the box holds the session
// and the fresh brief where the script starts the session minted in its place.
//
// Mutation: leave the script's stdin on the continuation after it starts anew,
// and the fallback row goes red.
func TestAnOpenCodeRunThatCannotContinueItsSessionReadsTheFreshBrief(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, tc := range map[string]struct {
			held []string
			want string
		}{
			"the session held":    {held: []string{carriedOpenCode}, want: `fix "the" flake`},
			"nothing to continue": {want: "the task, from the top"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: tc.held,
					freshBrief: "the task, from the top",
					req:        sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				if got := readIn(t, run.dir, "brief"); got != tc.want {
					t.Errorf("`opencode run` read %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// wiredHarness is [sessionHarness] that also records, as it starts, the MCP
// configuration it was pointed at, and whether the configuration of the other
// session was still beside it in the box: what the run's model could read.
func wiredHarness(release string) string {
	return strings.Replace(sessionHarness(release), "for a; do printf",
		`prev=; for a; do if [ "$prev" = --mcp-config ]; then cp "$a" "$HOME/mcp-seen"; `+
			`if [ -e "$(dirname "$a")/mcp-fresh.json" ]; then echo yes; else echo no; fi > "$HOME/other-left"; `+
			`fi; prev=$a; done`+"\nfor a; do printf", 1)
}

// bridgeAt is a run's tool bridge, reached under token.
func bridgeAt(token string) map[string]sandbox.MCPServer {
	return map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
		URL: "https://engine.example.com/mcp/" + token}}
}

// A RUN IS WIRED TO THE SESSION ITS BOX STARTED, AND ITS BOX HOLDS NO OTHER
// TOKEN. A run that continues a session is handed the seat's tools under two
// bridge tokens: that session's, and one for a session of its own. Where its
// box takes the session up, its harness starts on the first; where the session
// is neither held nor restorable, on the second — so the token it dials is its
// box's word to the bridge on which session it started. Either way the other is
// gone from the box before the harness starts, so nothing the run's model reads
// hands it the token of a session it is not in. AND THE RUN'S START NAMES BOTH
// SESSIONS AS ITS HARNESS RUNS THEM: the one it continues, which a box that took
// it up ran, and the one its script starts of its own, which a box that fell
// back ran — what a relaunch carrying the job's round on in its box reads which
// session that box holds from ([sandbox.PendingRun.SessionRan]).
//
// Mutation: start the harness on the continued session's token whatever its
// restore came to, leave the other configuration in the box, or name in the
// run's handle a session of its own other than the one its script starts, and a
// row goes red.
func TestARunIsWiredToTheSessionItsBoxStarted(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", wiredHarness("2.1.310"))
	_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", MCPServers: bridgeAt("first-job")})
	for _, tc := range []struct {
		name, resume, want string
		continued          bool
	}{
		{name: "its session held", resume: first.SessionID, want: "continued-token", continued: true},
		{name: "a session it cannot take up", resume: uuid.NewString(), want: "fresh-token"},
	} {
		handle, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on where you stopped",
			FreshBrief: "the task, from the top", Resume: tc.resume, MCPServers: bridgeAt("continued-token"),
			FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token")}})
		if seen := boxFile(t, box, box.Home()+"/mcp-seen"); !strings.Contains(seen, tc.want) {
			t.Errorf("%s: the harness started on %s, want the %s", tc.name, seen, tc.want)
		}
		if left := strings.TrimSpace(boxFile(t, box, box.Home()+"/other-left")); left != "no" {
			t.Errorf("%s: the other session's configuration was in the box as the harness started (%q)",
				tc.name, left)
		}
		if handle.SessionID != tc.resume || handle.FreshSessionID == "" || handle.FreshSessionID == tc.resume {
			t.Fatalf("%s: the run's start named %q and %q, want the session it continues and another of "+
				"its own", tc.name, handle.SessionID, handle.FreshSessionID)
		}
		ran := handle.FreshSessionID
		if tc.continued {
			ran = handle.SessionID
		}
		if res.Continued != tc.continued || res.SessionID != ran {
			t.Errorf("%s: the harness ran session %q (continued %v), want %q, the one the start named",
				tc.name, res.SessionID, res.Continued, ran)
		}
	}
}

// strippedBox is a box from which path is gone by the time the run's script
// starts: a run whose start wrote less than it should have.
type strippedBox struct {
	sandbox.Sandbox
	path string
}

func (b strippedBox) StartBackground(ctx context.Context, cmd string, opts sandbox.ExecOptions) (string, error) {
	if _, err := b.Exec(ctx, "rm -f "+shQuote(b.path), sandbox.ExecOptions{}); err != nil {
		return "", err
	}
	return b.Sandbox.StartBackground(ctx, cmd, opts)
}

// A CLAUDE CODE RUN WHOSE FRESH WIRING IS MISSING DOES NOT START. Every run that
// continues a session is written both wirings of the seat's tools, so a run
// whose box falls back to a session of its own and finds only the continued
// session's is one whose start did not finish writing the other: it ends before
// its harness starts, saying so — never started on the continued session's
// token, which would tell the bridge it took that session up and arm the guard
// with skills the new session never read.
//
// Mutation: carry on past a move that failed, and the harness starts on the
// continued session's wiring.
func TestAClaudeCodeRunWhoseFreshWiringIsMissingDoesNotStart(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", wiredHarness("2.1.310"))
	stripped := strippedBox{Sandbox: box, path: executor.PathsFor(box).MCPConfigFresh()}
	_, res := runToEnd(t, runner, stripped, sandbox.RunRequest{Brief: "carry on where you stopped",
		FreshBrief: "the task, from the top", Resume: uuid.NewString(), MCPServers: bridgeAt("continued-token"),
		FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token")}})
	if res.Success {
		t.Fatalf("a fall back with no fresh wiring succeeded: %+v", res)
	}
	if stderr := boxFile(t, box, executor.PathsFor(box).Err()); !strings.Contains(stderr,
		"could not be wired to the tools of its seat") {
		t.Errorf("the run's error stream does not say why it did not start:\n%s", stderr)
	}
	if argv := boxFile(t, box, box.Home()+"/argv"); argv != "" {
		t.Errorf("the harness started, on %q", argv)
	}
}

// AN OPENCODE RUN IS WIRED TO THE SESSION ITS BOX STARTED, AND ITS BOX HOLDS NO
// OTHER TOKEN, under every sh a box may run its script with: the run's server
// starts on the configuration's values under the continued session's token
// where the box holds the session, and on those under a session of its own's
// where it starts one — the other set gone from the box before the server
// starts, which is what reads them.
//
// Mutation: start the server on the continued session's values whatever the
// gate came to, or leave the other set in the box, and a row goes red.
func TestAnOpenCodeRunIsWiredToTheSessionItsBoxStarted(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, tc := range map[string]struct {
			held []string
			want string
		}{
			"the session held":    {held: []string{carriedOpenCode}, want: "continued-token"},
			"nothing to continue": {want: "fresh-token"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: tc.held,
					freshBrief: "the task, from the top", values: "continued-token", freshValues: "fresh-token",
					req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly,
						FreshWiring: &sandbox.Wiring{MCPServers: bridgeOnly}}})
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				if got := readIn(t, run.dir, "served-value"); got != tc.want {
					t.Errorf("the run's server started on %q, want %q", got, tc.want)
				}
				if left := strings.TrimSpace(readIn(t, run.dir, "served-beside-other")); left != "no" {
					t.Errorf("the other session's values were in the box as the server started (%q)", left)
				}
			})
		}
	})
}

// AN OPENCODE RUN WHOSE CONFIGURATION NAMES NO VALUE STARTS ON IT AS IT IS,
// whichever session its box started: with neither set of values written, a fall
// back has nothing to swap, and finding nothing is no reason to end the run.
//
// Mutation: move the other set over the first whether or not there is one, and
// the fall back ends the run before its server starts.
func TestAnOpenCodeRunWithNoValuesStartsOnItsConfigurationAsItIs(t *testing.T) {
	t.Parallel()
	run := runScript(t, scriptOpts{session: &continuingOpenCode, freshBrief: "the task, from the top",
		req: sandbox.RunRequest{Resume: carriedOpenCode, FreshWiring: &sandbox.Wiring{}}})
	if run.code != 0 {
		t.Fatalf("a fall back with no values exited %d:\n%s", run.code, run.stderr)
	}
	if got := readIn(t, run.dir, "brief"); got != "the task, from the top" {
		t.Errorf("`opencode run` read %q, want the fresh brief", got)
	}
}

// AN OPENCODE RUN WHOSE OTHER VALUES ARE MISSING DOES NOT START. Every run whose
// configuration names a value is written both sets, file for file, so a run
// whose box falls back to a session of its own and finds only the continued
// session's values is one whose start did not finish writing the other: it
// ends before its server starts, saying so — never started on the continued
// session's values, nor with them deleted and none in their place, a server
// reaching none of the seat's tools.
//
// Mutation: swap the sets without asking whether the other is there, and the
// server starts on no values at all.
func TestAnOpenCodeRunWhoseOtherValuesAreMissingDoesNotStart(t *testing.T) {
	t.Parallel()
	run := runScript(t, scriptOpts{session: &continuingOpenCode, freshBrief: "the task, from the top",
		values: "continued-token", req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly,
			FreshWiring: &sandbox.Wiring{MCPServers: bridgeOnly}}})
	if run.code == 0 || !strings.Contains(run.stderr, "could not be wired to the tools of its seat") {
		t.Fatalf("a fall back with one set of values exited %d, want it refused, saying so:\n%s",
			run.code, run.stderr)
	}
	if served := readIn(t, run.dir, "served-value"); served != "" {
		t.Errorf("the run's server started on %q", served)
	}
}

// AN OPENCODE RUN THAT CONTINUES A SESSION IS CONFIGURED FOR EITHER SESSION ITS
// BOX STARTS. Its configuration names every value by a reference into its values
// directory, and the same values under the job's other bridge token are written
// beside them under the same names, so the one configuration reads either set,
// which the run's script moves into place for the session it starts. Two
// wirings that would not be one configuration — another server, or the notes
// route on one alone — are refused rather than started on the wrong values.
//
// Mutation: write the other set over the first, write it nowhere, or let two
// wirings that are not one configuration through, and a case goes red.
func TestAnOpenCodeRunThatContinuesASessionIsConfiguredForEither(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	req := sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeAt("continued-token"),
		FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token")}}
	path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, req, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if config := boxFile(t, box, path); !strings.Contains(config, "{file:"+p.OpenCodeValues()+"/v1}") ||
		strings.Contains(config, "continued-token") || strings.Contains(config, "fresh-token") {
		t.Fatalf("the configuration does not name its one value by reference alone:\n%s", config)
	}
	if got := boxFile(t, box, p.OpenCodeValues()+"/v1"); !strings.HasSuffix(got, "/continued-token") {
		t.Errorf("the continued session's value is %q, want its own token", got)
	}
	if got := boxFile(t, box, p.OpenCodeValuesFresh()+"/v1"); !strings.HasSuffix(got, "/fresh-token") {
		t.Errorf("a session of its own's value, by the same name, is %q, want its own token", got)
	}
	for name, other := range map[string]*sandbox.Wiring{
		"another server": {MCPServers: map[string]sandbox.MCPServer{"elsewhere": {Name: "elsewhere",
			Transport: sandbox.TransportHTTP, URL: "https://engine.example.com/mcp/fresh-token"}}},
		"the notes route on one alone": {MCPServers: bridgeAt("fresh-token"),
			Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take",
				URL: "https://engine.example.com/mcp/fresh-token/steer"}},
	} {
		mismatched := req
		mismatched.FreshWiring = other
		if _, err := (executor.OpenCode{}).WriteConfig(t.Context(), box, mismatched, p); err == nil ||
			!strings.Contains(err.Error(), "not the same configuration") {
			t.Errorf("%s: WriteConfig = %v, want the two wirings refused", name, err)
		}
	}
}

// A SECOND WIRING IS A CONTINUING RUN'S ALONE. A run that starts a session of
// its own has one session, and a second token written into its box would be one
// its model could read and dial as a session it is not in: the runner refuses
// to start it.
//
// Mutation: start a run that continues nothing with a second wiring, and its box
// holds another token of the job.
func TestASecondWiringIsAContinuingRunsAlone(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	_, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin", MCPServers: bridgeAt("first-job"),
		FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("other-token")}})
	if err == nil || !strings.Contains(err.Error(), "second wiring") {
		t.Fatalf("Start = %v, want a run that continues no session refused its second wiring", err)
	}
}

// A SESSION ITS HARNESS WAS CUT OFF IN IS RESTORED OVER THE BOX'S OWN COPY. The
// capture of a job that did not exit cleanly was packed by the engine, and the
// box's copy may hold what its harness never finished — or what it went on to
// do after the capture, which the continuation lists instead — so the archive
// replaces the copy even where the box holds the archive's digest; a capture
// taken at a clean exit keeps a copy that holds it.
//
// Mutation: keep a copy that holds the archive whatever was asked, and the
// box's own line survives.
func TestACutOffSessionIsRestoredOverTheBoxsOwnCopy(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	described, body, err := runner.OpenCapture(t.Context(), box)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	archive, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	captured := boxFile(t, box, transcriptOf(box, ran.SessionID))
	own := captured + `{"type":"user","line":"after the capture"}` + "\n"
	if err := box.WriteFile(t.Context(), transcriptOf(box, ran.SessionID), []byte(own)); err != nil {
		t.Fatal(err)
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: strings.NewReader(string(archive)),
			Replace: true}})
	if !res.Continued {
		t.Fatalf("the run did not continue the session restored over its box's copy:\n%s",
			boxFile(t, box, paths(box).Err()))
	}
	got := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if strings.Contains(got, "after the capture") || !strings.HasPrefix(got, captured) {
		t.Errorf("the box's own copy survived the archive that replaces it:\n%s", got)
	}
}

// AN OPENCODE SESSION ITS RUN WAS CUT OFF IN IS IMPORTED OVER THE BOX'S COPY,
// under every sh: the copy is deleted and the export imported though the box
// keeps the export's digest — an export holds only settled messages, and the
// copy may hold a step that never settled.
//
// Mutation: keep a copy that holds the export whatever was asked, and the
// import never happens.
func TestACutOffOpenCodeSessionIsImportedOverTheBoxsCopy(t *testing.T) {
	t.Parallel()
	replacing := continuingOpenCode
	replacing.Replaces = true
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &replacing, held: []string{carriedOpenCode},
			carried: openCodeExport(carriedOpenCode), ownsCarried: true,
			req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		deleted, imported := run.callIndex("session delete"), run.callIndex("session import --standalone")
		if deleted < 0 || imported < deleted {
			t.Fatalf("the box's copy was not replaced by the export:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := sessionOf(t, run); got != carriedOpenCode {
			t.Errorf("the run worked in %q, want the restored %q", got, carriedOpenCode)
		}
	})
}

// A TRANSCRIPT CUT OFF MID-LINE IS CUT BACK TO ITS LAST WHOLE RECORD BEFORE IT
// IS RESUMED. A harness killed while it appended leaves a line with no newline,
// which no reader of a JSON-lines transcript can use; resumed as it is, the
// next record would be written onto its end. The script cuts it, and a
// transcript that ends whole is left as it is.
//
// Mutation: resume the transcript as the box left it, and the torn line is
// welded to the next run's first record.
func TestATranscriptCutOffMidLineIsCutBackBeforeItIsResumed(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	whole := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if err := box.WriteFile(t.Context(), transcriptOf(box, ran.SessionID),
		[]byte(whole+`{"type":"assistant","message":{"content":[{"type":"tool_us`)); err != nil {
		t.Fatal(err)
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	got := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if strings.Contains(got, "tool_us") || !strings.HasPrefix(got, whole) ||
		strings.Count(got, "\n") != strings.Count(whole, "\n")+1 {
		t.Errorf("the torn line was resumed as it was:\n%s", got)
	}
}

// tornThreads are the files a [tornThreadsHarness] leaves under its session's
// sidecar directory, by their path there: as it left each, and as a harness
// resuming the session must read it.
var tornThreads = map[string]struct{ left, resumed string }{
	"subagents/agent-1.jsonl": {left: tornWhole + `{"type":"assistant","message":{"content":[{"type":"tool_us`,
		resumed: tornWhole},
	"subagents/workflow/agent-2.jsonl": {left: tornWhole + `{"type":"user","n":`, resumed: tornWhole},
	"subagents/agent-3.jsonl":          {left: tornWhole, resumed: tornWhole},
	"subagents/agent-4.jsonl":          {left: `{"type":"us`, resumed: `{"type":"us`},
	"tool-results/out.txt":             {left: "spilled, with no newline", resumed: "spilled, with no newline"},
}

// tornWhole is the whole lines a torn thread holds before its torn one.
const tornWhole = `{"type":"user","n":1}` + "\n" + `{"type":"assistant","n":2}` + "\n"

// tornThreadsHarness is [sessionHarness] killed, as it were, while its
// subagents appended to their threads: it leaves [tornThreads] under its
// session's sidecar directory, each written as it ends.
func tornThreadsHarness(release string) string {
	var b strings.Builder
	for name, thread := range tornThreads {
		b.WriteString(`mkdir -p "$(dirname "$p/$s/` + name + `")" && printf '%s' ` + shQuote(thread.left) +
			` > "$p/$s/` + name + `"` + "\n")
	}
	return strings.Replace(sessionHarness(release), `printf '{"type":"result"`, b.String()+`printf '{"type":"result"`, 1)
}

// threadsSeenHarness is [sessionHarness] that first copies the session's
// sidecar directory, as its box handed it over, to `seen` in the box's home —
// what a resumed harness reads of its subagents' threads before it writes a
// line of its own.
func threadsSeenHarness(release string) string {
	return strings.Replace(sessionHarness(release), `mkdir -p "$p/$s/subagents"`,
		`cp -R "$p/$s" "$HOME/seen" 2> /dev/null; mkdir -p "$p/$s/subagents"`, 1)
}

// A RESTORE CUTS A TORN LAST LINE OF EVERY SUBAGENT THREAD, not only of the
// session's transcript. A harness cut off while its subagents appended to their
// threads — or a session packed while they did — ends those threads mid-line,
// and resumed as they are, the record the harness next writes to such a thread
// runs into its torn line and is lost with it (measured on Claude Code 2.1.310,
// whose resume takes a thread cut back to its last whole line). So a session
// carried into a fresh box has every `.jsonl` under its sidecar directory, at
// any depth, cut back to its last whole line before its harness resumes it; a
// thread that ends whole, one with no whole line at all, and a file that is no
// thread are left as they are.
//
// Mutation: cut the session's own transcript alone, as the restore did, or
// only the threads one level down, and a torn thread is resumed as it was.
func TestARestoreCutsATornLastLineOfEverySubagentThread(t *testing.T) {
	t.Parallel()
	first, a := realBox(t)
	writeBoxStandIn(t, a, executor.PathsFor(a).BinDir()+"/claude", tornThreadsHarness("2.1.310"))
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()

	second, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", threadsSeenHarness("2.1.310"))
	_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
	if !res.Continued {
		t.Fatalf("the carried session was not continued:\n%s", boxFile(t, b, paths(b).Err()))
	}
	for name, thread := range tornThreads {
		if got := boxFile(t, b, b.Home()+"/seen/"+name); got != thread.resumed {
			t.Errorf("%s reached the resumed harness as %q, want %q", name, got, thread.resumed)
		}
	}
}

// A CONTINUED CLAUDE CODE SESSION RUNS UNDER THE ENGINE'S WINDOW AND LEAVES ITS
// INTERRUPTED TURN AS IT IS. Every run compacts at the window of its model
// (sandbox.ClaudeCodeWindow), whatever the seat's environment set: a window of
// its own, or compaction switched off altogether (DISABLE_AUTO_COMPACT,
// DISABLE_COMPACT). So no request of it, or of a run that continues it,
// carries more; and a resumed run is started without
// CLAUDE_CODE_RESUME_INTERRUPTED_TURN, which would have it run the turn its
// last process was cut off in again before it reads its brief.
//
// Mutation: drop the window from the pins or a switch from the unsets, or
// leave the variable set on a resume, and the harness's environment shows it.
func TestAContinuedClaudeSessionRunsUnderTheEnginesWindow(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	seat := map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN": "1",
		"DISABLE_AUTO_COMPACT": "1", "DISABLE_COMPACT": "1"}
	const model = "claude-haiku-4-5"
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", Env: seat, Model: model})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID, Env: seat,
		Model: model})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	env := strings.Split(boxFile(t, box, box.Home()+"/env"), "\n")
	if want := "CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + strconv.Itoa(sandbox.ClaudeCodeWindow(model).Tokens); !slices.Contains(env, want) {
		t.Errorf("the harness's environment lacks %s, the seat's own value replaced", want)
	}
	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE_CODE_RESUME_INTERRUPTED_TURN=")
	}) {
		t.Error("a resumed run kept CLAUDE_CODE_RESUME_INTERRUPTED_TURN")
	}
	for _, off := range []string{"DISABLE_AUTO_COMPACT=", "DISABLE_COMPACT="} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, off) }) {
			t.Errorf("the harness's environment keeps %s1, which switches its compaction off", off)
		}
	}
}

// usageHarness is a stand-in `claude` that files a session whose transcript
// records two requests' usage on its main thread, then one on a subagent's
// sidechain and a later assistant message that recorded none — each after the
// main thread's last — and a person's message quoting the words a usage line
// is found by.
func usageHarness() string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
mkdir -p "$p"
cat >> "$p/$s.jsonl" <<'EOF'
{"type":"user","message":{"content":"begin"}}
{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":7}}}
{"type":"user","message":{"content":"a tool's answer quoting \"usage\" and \"assistant\""}}
{"type":"assistant","message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":9}}}
{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":99999,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
EOF
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// openCodeTaken is the context the take of an OpenCode session whose export
// holds messages (a JSON list) reads off the archive it streams out of its box:
// the run's own take, through the stand-in opencode's export.
func openCodeTaken(t *testing.T, messages string) int64 {
	t.Helper()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
	if err != nil || taken.Body == nil || taken.Context == nil {
		t.Fatalf("TakeSession = %+v, %v", taken, err)
	}
	if _, err := io.ReadAll(taken.Body); err != nil {
		t.Fatal(err)
	}
	_ = taken.Body.Close()
	return taken.Context()
}

// WHAT A SESSION'S LAST REQUEST CARRIED IS READ OFF ITS ARCHIVE AS IT IS KEPT —
// the context a launch that continues it sends again, which that launch's
// floor is made of. Claude Code's: the whole input of the last assistant
// message on the main thread that recorded its usage, cache reads and writes
// included — never a subagent's, never a later message that recorded none.
// OpenCode's: the last settled assistant message that measured a prompt,
// never one that failed and never a compaction's own request — in the export
// each release this build drives printed for a real session
// (testdata/opencode) as much as in one written here: there the last step that
// measured one is the answer to "Carry on.", 1015 tokens its cache did not
// serve and 200 it read, and every step after it failed — one its provider
// refused, which measured nothing, and two the session was interrupted in, the
// first of which measured 1217.
//
// Mutation: read the first usage rather than the last, count a sidechain, a
// failed message or a compaction, or leave the cache out, and a figure goes
// red.
func TestTheContextASessionCarriesIsReadOffItsArchive(t *testing.T) {
	t.Parallel()
	t.Run("claude-code", func(t *testing.T) {
		t.Parallel()
		runner, b := realBox(t)
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", usageHarness())
		if _, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"}); res.Captured == nil {
			t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: takeBound})
		if err != nil || taken.Body == nil || taken.Context == nil {
			t.Fatalf("TakeSession = %+v, %v", taken, err)
		}
		if _, err := io.ReadAll(taken.Body); err != nil {
			t.Fatal(err)
		}
		_ = taken.Body.Close()
		if got := taken.Context(); got != 20+300+4000 {
			t.Errorf("the session's last request carried %d tokens, want 4320", got)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		// A COMPACTION CARRIES TOKENS TOO, as each of the real exports' does:
		// its own request's, which measured the conversation it summarised
		// and not the context the session carries on with.
		messages := `[{"id":"msg_1","type":"user","text":"x"},` +
			`{"id":"msg_2","type":"assistant","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1000,"write":50}}},` +
			`{"id":"msg_3","type":"assistant","error":null,"tokens":{"input":20,"output":9,"cache":{"read":2000,"write":80}}},` +
			`{"id":"msg_4","type":"assistant","error":{"name":"APIError"},"tokens":{"input":999999,"cache":{"read":0,"write":0}}},` +
			`{"id":"msg_5","type":"user","text":"a later message"},` +
			`{"id":"msg_6","type":"compaction","status":"completed","reason":"manual","summary":"## Objective",` +
			`"tokens":{"input":777777,"output":30,"reasoning":0,"cache":{"read":200,"write":0}}}]`
		if got := openCodeTaken(t, messages); got != 20+2000+80 {
			t.Errorf("the session's last request carried %d tokens, want 2100", got)
		}
	})
	for _, version := range openCodeFixtureVersions {
		t.Run("opencode "+version+"'s real export", func(t *testing.T) {
			t.Parallel()
			var export struct {
				Messages json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal([]byte(openCodeExportFixture(t, version)), &export); err != nil {
				t.Fatalf("the fixture does not decode: %v", err)
			}
			if got := openCodeTaken(t, string(export.Messages)); got != 1015+200+0 {
				t.Errorf("the session's last request carried %d tokens, want 1215", got)
			}
		})
	}
}

// A RUNNER RESTORES ONLY WHAT IT COULD CONTINUE, decided without a box: a
// session its own harness wrote, in the one format it packs, by a release whose
// sessions another box continues — the answer every reader of whether a session
// continues takes beside the launch, before the box's own release is asked.
//
// Mutation: drop the writer's floor, or answer for another harness's session,
// and a row goes red.
func TestARunnerRestoresOnlyWhatItCouldContinue(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runner   *executor.Runner
		archive  sandbox.SessionArchive
		restores bool
		why      string
	}{
		"claude's own": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.ClaudeCodeName,
			Format: executor.ClaudeArchiveFormat, Version: "2.1.310"}, true, ""},
		"claude, a writer too old": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.ClaudeCodeName,
			Format: executor.ClaudeArchiveFormat, Version: "2.1.299"}, false, "older than 2.1.300"},
		"claude, opencode's session": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat, Version: "2.0.32"}, false, "continued only by the harness that wrote it"},
		"opencode's own": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat, Version: "2.2.5"}, true, ""},
		"opencode, another format": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat + 1, Version: "2.2.5"}, false, "format"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			restores, why := tc.runner.Restores(tc.archive)
			if restores != tc.restores || !strings.Contains(why, tc.why) || (restores && why != "") {
				t.Errorf("Restores = %v, %q; want %v saying %q", restores, why, tc.restores, tc.why)
			}
		})
	}
}

// A CONTINUED OPENCODE RUN IS CONFIGURED WITH ITS OWN INSTRUCTIONS, as a fresh
// one is. OpenCode keeps, per session, the instructions it last told the model,
// and an import writes none of that state (session/transfer.ts at 2.0.32 and
// 2.2.5), so the first request of a session carried into a box reads the run's
// instruction file as the session's first: its system prompt is this run's
// standing instructions, and no earlier launch's ride the conversation as
// updates. What holds that on the engine's side is that a run continuing a
// session is configured exactly as one starting one: its instruction file
// named, and written afresh with its own instructions.
//
// Mutation: configure a run that continues a session without its instruction
// file, or leave an earlier run's in place, and this goes red.
func TestAContinuedOpenCodeRunIsConfiguredWithItsOwnInstructions(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	configure := func(req sandbox.RunRequest) string {
		t.Helper()
		path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, req, p)
		if err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		return boxFile(t, box, path)
	}
	fresh := configure(sandbox.RunRequest{System: "the first launch's instructions"})
	continued := configure(sandbox.RunRequest{System: "this launch's instructions", Resume: carriedOpenCode,
		Carried: &sandbox.CarriedSession{Archive: sandbox.SessionArchive{Harness: executor.OpenCodeName,
			SessionID: carriedOpenCode, Format: executor.OpenCodeArchiveFormat, Version: "2.0.32"},
			Body: strings.NewReader(openCodeExport(carriedOpenCode))}})
	if continued != fresh || !strings.Contains(continued, `"instructions"`) || !strings.Contains(continued, p.System()) {
		t.Errorf("the continued run is configured\n%s\nwant the fresh run's configuration, naming %s:\n%s",
			continued, p.System(), fresh)
	}
	if got := boxFile(t, box, p.System()); got != "this launch's instructions" {
		t.Errorf("the continued run's instruction file holds %q, want its own instructions", got)
	}
}

// A HARNESS KILLED AFTER IT WROTE ITS SESSION IS SAID TO HAVE BEEN CUT OFF. The
// run's script packs the session after its harness exits whatever the exit, so
// a harness killed while its wrapper lived on — the signal a memory limit
// sends — leaves an archive all the same, whose transcript ends wherever the
// kill found it: the result says the harness did not exit cleanly, beside the
// archive, for the capture to be kept as one cut off; and one that exited
// cleanly says nothing of the kind.
//
// Mutation: read the archive as whole whatever the harness's exit status, and
// the killed row goes red.
func TestAHarnessKilledAfterItWroteItsSessionIsSaidToHaveBeenCutOff(t *testing.T) {
	t.Parallel()
	for name, killed := range map[string]bool{"killed": true, "exited cleanly": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			harness := sessionHarness("2.1.310")
			if killed {
				harness = strings.Replace(harness, `printf '{"type":"result"`,
					"kill -KILL $$\n"+`printf '{"type":"result"`, 1)
			}
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", harness)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
			if res.Captured == nil || res.HarnessCutOff != killed {
				t.Fatalf("the run packed %+v and says its harness was cut off %v; want its session packed, "+
					"and cut off %v", res.Captured, res.HarnessCutOff, killed)
			}
		})
	}
}
