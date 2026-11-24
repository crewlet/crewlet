package executor_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// Continuing a turn's session, on a real box under the run's own script: the
// brief its harness reads, what is restored over what, torn transcripts cut
// back, and the environment a continued session runs in.

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

// A RUN THAT CANNOT CONTINUE ITS SESSION IS HANDED THE FRESH BRIEF. The box's
// script decides whether the session is taken up (it may be neither held nor
// restorable), so a continuing run carries both briefs: its harness reads the
// continuation where the session is taken up and the fresh brief where the
// script started a new one, so a new session never answers a brief written for
// a model that remembers.
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
// every sh: `opencode run` reads the brief the script points its stdin at, the
// continuation where the box holds the session and the fresh brief otherwise.
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

// wiredHarness is [sessionHarness] that also records the MCP configuration it
// started on, and whether the other session's was still beside it: what the
// run's model could read.
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
// TOKEN. A continuing run gets two bridge tokens, the continued session's and
// a fresh one's; its harness starts on the one for the session its box started
// (the box's word to the bridge on which that is), and the other is gone from
// the box first, so the model never reads a token of a session it is not in.
// AND THE RUN'S START NAMES BOTH SESSIONS, which a relaunch in the same box
// reads to learn which one it holds ([sandbox.PendingRun.SessionRan]).
//
// Mutation: start the harness on the continued session's token whatever its
// restore came to, leave the other configuration in the box, or name a fresh
// session other than the one the script starts, and a row goes red.
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
	if _, err := b.Exec(ctx, "rm -f "+shellword.Quote(b.path), sandbox.ExecOptions{}); err != nil {
		return "", err
	}
	return b.Sandbox.StartBackground(ctx, cmd, opts)
}

// A CLAUDE CODE RUN WHOSE FRESH WIRING IS MISSING DOES NOT START. A continuing
// run is always written both wirings, so a fall back that finds only the
// continued one means its start did not finish: it ends before its harness
// starts, saying so. Started on the continued token, it would tell the bridge
// it took that session up and arm the guard with skills the new session never
// read.
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
// OTHER TOKEN, under every sh: its server starts on the values under the
// continued session's token where the box holds the session and on the fresh
// set where it starts one, the other set gone before the server reads them.
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
// whichever session its box started: with no values written a fall back has
// nothing to swap, which is no reason to end the run.
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

// AN OPENCODE RUN WHOSE OTHER VALUES ARE MISSING DOES NOT START. A run whose
// configuration names a value is written both sets, so a fall back finding only
// the continued session's means its start did not finish: it ends before its
// server starts, saying so, rather than start on the continued values or on
// none (a server reaching none of the seat's tools).
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
// BOX STARTS. Its configuration names every value by reference into its values
// directory, and the other bridge token's values are written beside them under
// the same names, so one configuration reads whichever set the script moves
// into place. Two wirings that are not one configuration (another server, or
// the notes route on one alone) are refused rather than started wrong.
//
// Mutation: write the other set over the first, write it nowhere, or let two
// wirings that are not one configuration through, and a case goes red.
func TestAnOpenCodeRunThatContinuesASessionIsConfiguredForEither(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	req := sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeAt("continued-token"),
		FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token")}}
	path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, req, executor.Session{}, p)
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
		if _, err := (executor.OpenCode{}).WriteConfig(t.Context(), box, mismatched, executor.Session{}, p); err == nil ||
			!strings.Contains(err.Error(), "not the same configuration") {
			t.Errorf("%s: WriteConfig = %v, want the two wirings refused", name, err)
		}
	}
}

// A SECOND WIRING IS A CONTINUING RUN'S ALONE. A run starting its own session
// has one session; a second token in its box is one its model could dial as a
// session it is not in, so the runner refuses to start it.
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
// copy may hold what the harness never finished, or work after the capture
// (which the continuation lists instead), so the archive replaces it even where
// the box holds the archive's digest; a capture taken at a clean exit keeps a
// copy that holds it.
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
// under every sh, even where the box keeps the export's digest: an export holds
// only settled messages, and the copy may hold a step that never settled.
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
// IS RESUMED: resumed as it is, the next record would be written onto the torn
// line's end. A transcript that ends whole is left as it is.
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
	"tool-results/out.txt": {left: "spilled\nand a last line with no newline",
		resumed: "spilled\nand a last line with no newline"},
}

// tornWhole is the whole lines a torn thread holds before its torn one.
const tornWhole = `{"type":"user","n":1}` + "\n" + `{"type":"assistant","n":2}` + "\n"

// tornThreadsHarness is [sessionHarness] killed, as it were, while its
// subagents appended to their threads: it leaves [tornThreads] under its
// session's sidecar directory, each written as it ends.
func tornThreadsHarness(release string) string {
	var b strings.Builder
	for name, thread := range tornThreads {
		b.WriteString(`mkdir -p "$(dirname "$p/$s/` + name + `")" && printf '%s' ` + shellword.Quote(thread.left) +
			` > "$p/$s/` + name + `"` + "\n")
	}
	return strings.Replace(sessionHarness(release), `printf '{"type":"result"`, b.String()+`printf '{"type":"result"`, 1)
}

// threadsSeenHarness is [sessionHarness] that first copies the session's
// sidecar directory to `seen` in the box's home: what a resumed harness reads
// of its subagents' threads before it writes.
func threadsSeenHarness(release string) string {
	return strings.Replace(sessionHarness(release), `mkdir -p "$p/$s/subagents"`,
		`cp -R "$p/$s" "$HOME/seen" 2> /dev/null; mkdir -p "$p/$s/subagents"`, 1)
}

// A RESTORE CUTS A TORN LAST LINE OF EVERY SUBAGENT THREAD, not only of the
// session's transcript: a record written onto a torn thread is lost with it.
// Every resume, of a carried session and of the box's own copy, cuts every
// `.jsonl` under the sidecar directory, at any depth, to its last whole line.
// A thread that ends whole, one with no whole line (2.1.310 continues it from
// nothing whatever a restore makes of it), and a file that is no thread are
// left as they are.
//
// Mutation: cut only the session's transcript, only threads one level down,
// or only a carried session, and a torn thread is resumed as it was; empty or
// remove a thread with no whole line, and its row goes red; cut every file,
// and the spilled result loses its last line.
func TestARestoreCutsATornLastLineOfEverySubagentThread(t *testing.T) {
	t.Parallel()
	t.Run("carried into a fresh box", func(t *testing.T) {
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
		threadsResumed(t, b)
	})
	t.Run("resumed in the box that holds it", func(t *testing.T) {
		t.Parallel()
		runner, a := realBox(t)
		writeBoxStandIn(t, a, executor.PathsFor(a).BinDir()+"/claude", tornThreadsHarness("2.1.310"))
		_, ran := runToEnd(t, runner, a, sandbox.RunRequest{Brief: "begin"})
		writeBoxStandIn(t, a, executor.PathsFor(a).BinDir()+"/claude", threadsSeenHarness("2.1.310"))
		_, res := runToEnd(t, runner, a, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID})
		if !res.Continued {
			t.Fatalf("the box's own copy of the session was not continued:\n%s", boxFile(t, a, paths(a).Err()))
		}
		threadsResumed(t, a)
	})
}

// threadsResumed checks what the harness resumed in b read of the session's
// sidecar directory against [tornThreads]: each file there, as it must read it.
func threadsResumed(t *testing.T, b sandbox.Sandbox) {
	t.Helper()
	for name, thread := range tornThreads {
		seen := b.Home() + "/seen/" + name
		if !boxHas(t, b, seen) {
			t.Errorf("%s did not reach the resumed harness at all", name)
			continue
		}
		if got := boxFile(t, b, seen); got != thread.resumed {
			t.Errorf("%s reached the resumed harness as %q, want %q", name, got, thread.resumed)
		}
	}
}

// boxHas reports whether anything is at path in box, a file, a directory or a
// link.
func boxHas(t *testing.T, box sandbox.Sandbox, path string) bool {
	t.Helper()
	res, err := box.Exec(t.Context(), "[ -e "+shellword.Quote(path)+" ] || [ -L "+shellword.Quote(path)+" ]", sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("asking the box for %s: %v", path, err)
	}
	return res.ExitCode == 0
}

// A CONTINUED CLAUDE CODE SESSION RUNS UNDER THE ENGINE'S WINDOW: it compacts
// at the Claude Code runner's PinnedWindow less the harness's headroom whatever the seat
// set (its own window, DISABLE_AUTO_COMPACT, DISABLE_COMPACT,
// CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE), so no
// request carries more or compacts where the engine does not reckon. Its
// settings' `env` block, the whole pin, holds the same window with both
// overrides and a resume's switches blank, against a repository's own
// settings (TestAResumeUnsetsWhatMovesWhatItHandsItsModel).
//
// Mutation: drop the window from the pins or a variable from the unsets, or
// hand 2.1.310 the settings without the pin, and the harness's environment or
// its settings show it.
func TestAContinuedClaudeSessionRunsUnderTheEnginesWindow(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	seat := map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
		"DISABLE_AUTO_COMPACT": "1", "DISABLE_COMPACT": "1", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50",
		"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "60000"}
	const model = "claude-haiku-4-5"
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", Env: seat, Model: model})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID, Env: seat,
		Model: model})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	env := strings.Split(boxFile(t, box, box.Home()+"/env"), "\n")
	if want := "CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + strconv.Itoa(executor.NewClaudeCode().PinnedWindow(model)); !slices.Contains(env, want) {
		t.Errorf("the harness's environment lacks %s, the seat's own value replaced", want)
	}
	for _, off := range []string{"DISABLE_AUTO_COMPACT=", "DISABLE_COMPACT=", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=",
		"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE="} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, off) }) {
			t.Errorf("the harness's environment keeps %s, which switches its compaction off or moves its point", off)
		}
	}
	named := after(strings.Split(strings.TrimSpace(boxFile(t, box, box.Home()+"/argv")), "\n"), "--settings")
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, box, named)), &settings); err != nil {
		t.Fatalf("the settings the harness was handed, %q, do not read: %v", named, err)
	}
	if want := map[string]string{
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":     strconv.Itoa(executor.NewClaudeCode().PinnedWindow(model)),
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":     "",
		"CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "",
		"CLAUDE_CODE_RESUME_INTERRUPTED_TURN": "",
		"CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP": "",
		"DISABLE_AUTOUPDATER":                 "1",
	}; !maps.Equal(settings.Env, want) {
		got, _ := json.Marshal(settings.Env)
		t.Errorf("the settings the harness was handed, %q, hold env %s, want the window pinned and both "+
			"overrides and both of a resume's switches blank, over a repository's own", named, got)
	}
}

// A BOX'S CLAUDE CODE RELEASE CANNOT CHANGE UNDER A SESSION IT CONTINUES IN
// PLACE. Every run starts with DISABLE_AUTOUPDATER=1 over the seat's own value,
// and the settings a release that takes the pin is handed set it too, over a
// repository's own; so the release that wrote the box's copy resumes it (the
// package doc's "A box's release is pinned").
//
// Mutation (claudecode.go): drop DISABLE_AUTOUPDATER from claudeSessionLayout's
// pins or from claudeSettingsPin's block, and a check goes red.
func TestABoxsClaudeCodeReleaseCannotChangeUnderItsSession(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	seat := map[string]string{"DISABLE_AUTOUPDATER": "0"}
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", Env: seat})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID, Env: seat})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	env := strings.Split(boxFile(t, box, box.Home()+"/env"), "\n")
	if !slices.Contains(env, "DISABLE_AUTOUPDATER=1") {
		t.Error("the harness's environment lacks DISABLE_AUTOUPDATER=1, the seat's own value replaced")
	}
	named := after(strings.Split(strings.TrimSpace(boxFile(t, box, box.Home()+"/argv")), "\n"), "--settings")
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, box, named)), &settings); err != nil {
		t.Fatalf("the settings the harness was handed, %q, do not read: %v", named, err)
	}
	if got, set := settings.Env["DISABLE_AUTOUPDATER"]; !set || got != "1" {
		t.Errorf("the settings the harness was handed, %q, set DISABLE_AUTOUPDATER to %q, want 1 over a "+
			"repository's own", named, got)
	}
}

// A RESUME UNSETS WHAT MOVES WHAT IT HANDS ITS MODEL, on every release and sh,
// whatever the seat set: CLAUDE_CODE_RESUME_INTERRUPTED_TURN (reruns the
// cut-off turn before the brief, repeating a call that may have run),
// CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP (past 5 MiB reads the transcript by a
// rule that can hand the model other records) and CLAUDE_CODE_REMOTE. A
// repository's settings could set the first two again, so 2.1.310's pinned
// settings name both, blank; 2.1.309, below the pin's floor, gets no `env`
// block and still resumes, held only by the script's unset.
//
// Mutation: drop either switch from the unsets, or unset them only on a fresh
// start or only where the pin applies, and a resumed harness starts with it;
// leave either out of the pin, and 2.1.310's settings leave it to the repo.
func TestAResumeUnsetsWhatMovesWhatItHandsItsModel(t *testing.T) {
	t.Parallel()
	seat := map[string]string{"CLAUDE_CODE_RESUME_INTERRUPTED_TURN": "1", "CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP": "1",
		"CLAUDE_CODE_REMOTE": "true"}
	// ONE RELEASE ON EACH SIDE OF THE PIN'S FLOOR, held to it here, so a floor
	// that moves past either fails this case rather than leave a side unplayed.
	releases := []struct {
		release string
		pinned  bool
	}{{release: "2.1.310", pinned: true}, {release: "2.1.309", pinned: false}}
	for _, r := range releases {
		if executor.ReleaseAtLeast(r.release, executor.ClaudeSettingsPinFloor) != r.pinned {
			t.Fatalf("%s is no longer on the side of the pin's floor, %s, this case plays it on (takes the pin: %v)",
				r.release, executor.ClaudeSettingsPinFloor, r.pinned)
		}
	}
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, r := range releases {
			t.Run(r.release, func(t *testing.T) {
				t.Parallel()
				runner, b := realBoxUnder(t, sh)
				p := paths(b)
				writeBoxStandIn(t, b, p.BinDir()+"/claude", sessionHarness(r.release))
				_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin", Env: seat})
				// THE FRESH RUN'S RECORD CLEARED, so the resumed run's own is read
				// and a harness that never started reads as none.
				for _, file := range []string{b.Home() + "/env", b.Home() + "/argv"} {
					if err := b.WriteFile(t.Context(), file, nil); err != nil {
						t.Fatalf("clear %s: %v", file, err)
					}
				}
				_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
					Env: seat})
				if !res.Continued {
					t.Fatalf("the run did not continue its session:\n%s", boxFile(t, b, p.Err()))
				}
				env := boxFile(t, b, b.Home()+"/env")
				if env == "" {
					t.Fatalf("the resumed run's harness recorded no environment:\n%s", boxFile(t, b, p.Err()))
				}
				for kv := range strings.SplitSeq(env, "\n") {
					for name := range seat {
						if strings.HasPrefix(kv, name+"=") {
							t.Errorf("the resumed harness started with %s, which moves what its resume hands "+
								"its model", kv)
						}
					}
				}
				named, want := after(argvOf(t, b), "--settings"), p.ClaudeSettings()
				if r.pinned {
					want = p.ClaudeSettingsPinned()
				}
				if named != want {
					t.Fatalf("the resumed harness was handed the settings %q, want %q", named, want)
				}
				var settings struct {
					Env map[string]string `json:"env"`
				}
				if err := json.Unmarshal([]byte(boxFile(t, b, named)), &settings); err != nil {
					t.Fatalf("the settings the resumed harness was handed, %q, do not read: %v", named, err)
				}
				if !r.pinned {
					// NO PIN BEHIND THE UNSET: these settings name no variable, so
					// the environment above is all that holds the switches off.
					if settings.Env != nil {
						got, _ := json.Marshal(settings.Env)
						t.Errorf("the settings %s was handed, %q, hold env %s, which a release older than the "+
							"pin's floor is not handed", r.release, named, got)
					}
					return
				}
				for _, name := range []string{"CLAUDE_CODE_RESUME_INTERRUPTED_TURN",
					"CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP"} {
					if value, set := settings.Env[name]; !set || value != "" {
						t.Errorf("the settings the resumed harness was handed, %q, hold %s as %q (named: %v), want it "+
							"named and blank, over a repository's own", named, name, value, set)
					}
				}
			})
		}
	})
}

// usageHarness is a stand-in `claude` whose transcript records two main-thread
// requests' usage (isSidechain false, as Claude Code's own records say), then
// a sidechain's and a later assistant message with none, and a person's
// message quoting "usage" and "assistant".
func usageHarness() string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
mkdir -p "$p"
cat >> "$p/$s.jsonl" <<'EOF'
{"type":"user","isSidechain":false,"message":{"content":"begin"}}
{"type":"assistant","isSidechain":false,"message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":7}}}
{"type":"user","isSidechain":false,"message":{"content":"a tool's answer quoting \"usage\" and \"assistant\""}}
{"type":"assistant","isSidechain":false,"message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":9}}}
{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":99999,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","isSidechain":false,"message":{"content":[{"type":"text","text":"done"}]}}
EOF
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// openCodeTaken is the context the run's own take of an OpenCode session reads
// off the archive it streams out of its box, the stand-in's export holding
// messages (a JSON list).
func openCodeTaken(t *testing.T, messages string) int64 {
	t.Helper()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.3.2\n")); err != nil {
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
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Pack: true, MaxBytes: takeBound})
	if err != nil || taken.Body == nil || taken.Context == nil {
		t.Fatalf("TakeSession = %+v, %v", taken, err)
	}
	if _, err := io.ReadAll(taken.Body); err != nil {
		t.Fatal(err)
	}
	_ = taken.Body.Close()
	return taken.Context()
}

// WHAT A SESSION'S LAST REQUEST CARRIED IS READ OFF ITS ARCHIVE AS IT IS KEPT:
// the context a continuing launch sends again, its floor. Claude Code's is the
// whole input, cache included, of the last main-thread assistant message that
// recorded usage, never a subagent's or a later one that recorded none.
// OpenCode's is the last settled assistant message that measured a prompt,
// never a failed one or a compaction's request, in a written export as in each
// release's real one (testdata/opencode: the answer to "Carry on.", 1017 tokens
// uncached and 200 read; every later step failed, one measuring 1219).
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
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
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
		// A COMPACTION CARRIES TOKENS TOO, as each real export's does: its own
		// request's, measuring the conversation it summarised, not the context
		// the session carries on with.
		messages := `[{"id":"msg_1","type":"user","text":"x"},` +
			`{"id":"msg_2","type":"assistant","time":{"created":1,"completed":2},` +
			`"tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1000,"write":50}}},` +
			`{"id":"msg_3","type":"assistant","time":{"created":3,"completed":4},"error":null,` +
			`"tokens":{"input":20,"output":9,"cache":{"read":2000,"write":80}}},` +
			`{"id":"msg_4","type":"assistant","time":{"created":5,"completed":6},"error":{"name":"APIError"},` +
			`"tokens":{"input":999999,"cache":{"read":0,"write":0}}},` +
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
			if got := openCodeTaken(t, string(export.Messages)); got != 1017+200+0 {
				t.Errorf("the session's last request carried %d tokens, want 1217", got)
			}
		})
	}
}

// A RUNNER RESTORES ONLY WHAT IT COULD CONTINUE, decided without a box: a
// session its own harness wrote, in the one format it packs, by a release whose
// sessions another box continues: what every reader of whether a session
// continues asks before the box's own release is asked. A runner of one
// release (Kimi Code, Copilot, Muse Code, Grok Build) restores a session that
// release wrote: judged of the writer alone, never refused for want of a box.
//
// Mutation: drop the writer's floor or the one release Gemini CLI and Qwen
// Code continue on, answer for another harness's session, or judge a
// one-release runner's writer beside no box ([carrier.Continues] of ""), and
// a row goes red.
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
			Format: executor.OpenCodeArchiveFormat, Version: "2.3.2"}, false, "continued only by the harness that wrote it"},
		"opencode's own": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat, Version: "2.3.2"}, true, ""},
		"opencode, another format": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat + 1, Version: "2.3.2"}, false, "format"},
		"kimi's own": {executor.NewKimi(), sandbox.SessionArchive{Harness: executor.KimiName,
			Format: executor.KimiArchiveFormat, Version: "2.7.2"}, true, ""},
		"copilot's own": {executor.NewCopilot(), sandbox.SessionArchive{Harness: executor.CopilotName,
			Format: executor.CopilotArchiveFormat, Version: "1.0.114"}, true, ""},
		"muse's own": {executor.NewMuse(), sandbox.SessionArchive{Harness: executor.MuseName,
			Format: executor.MuseArchiveFormat, Version: "1.4.25"}, true, ""},
		"muse, another release's writer": {executor.NewMuse(), sandbox.SessionArchive{Harness: executor.MuseName,
			Format: executor.MuseArchiveFormat, Version: "1.4.26"}, false, "only a session 1.4.25 wrote"},
		"hermes's own": {executor.NewHermes(), sandbox.SessionArchive{Harness: executor.HermesName,
			Format: executor.HermesArchiveFormat, Version: "0.23.4"}, true, ""},
		"hermes, another release's writer": {executor.NewHermes(), sandbox.SessionArchive{Harness: executor.HermesName,
			Format: executor.HermesArchiveFormat, Version: "0.23.5"}, false, "only a session Hermes Agent 0.23.4 wrote"},
		"gemini's own": {executor.NewGeminiCLI(), sandbox.SessionArchive{Harness: executor.GeminiCLIName,
			Format: executor.GeminiArchiveFormat, Version: "0.70.0"}, true, ""},
		"gemini, a newer writer": {executor.NewGeminiCLI(), sandbox.SessionArchive{Harness: executor.GeminiCLIName,
			Format: executor.GeminiArchiveFormat, Version: "0.71.0"}, false, "only on 0.70.0, and the session was written by 0.71.0"},
		"gemini, an older writer": {executor.NewGeminiCLI(), sandbox.SessionArchive{Harness: executor.GeminiCLIName,
			Format: executor.GeminiArchiveFormat, Version: "0.65.0"}, false, "written by 0.65.0"},
		"gemini, an unrecorded writer": {executor.NewGeminiCLI(), sandbox.SessionArchive{Harness: executor.GeminiCLIName,
			Format: executor.GeminiArchiveFormat}, false, "named no release"},
		"qwen's own": {executor.NewQwenCode(), sandbox.SessionArchive{Harness: executor.QwenCodeName,
			Format: executor.QwenArchiveFormat, Version: "0.28.4"}, true, ""},
		"qwen, another format": {executor.NewQwenCode(), sandbox.SessionArchive{Harness: executor.QwenCodeName,
			Format: executor.QwenArchiveFormat + 1, Version: "0.28.4"}, false, "format"},
		"qwen, an older writer": {executor.NewQwenCode(), sandbox.SessionArchive{Harness: executor.QwenCodeName,
			Format: executor.QwenArchiveFormat, Version: "0.26.1"}, false, "only on 0.28.4, and the session was written by 0.26.1"},
		"grok's own": {executor.NewGrok(), sandbox.SessionArchive{Harness: executor.GrokName,
			Format: executor.GrokArchiveFormat, Version: "1.0.92"}, true, ""},
		"grok, another release's writer": {executor.NewGrok(), sandbox.SessionArchive{Harness: executor.GrokName,
			Format: executor.GrokArchiveFormat, Version: "1.0.93"}, false, "only a session 1.0.92 wrote"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// UNDER THE ENTRY IT RAN UNDER, so no row turns on the entry
			// (TestACarriedSessionContinuesOnlyUnderTheEntryItRanUnder).
			archive := tc.archive
			archive.Entry = "an-entry"
			restores, why := tc.runner.RestoresUnder(archive, archive.Entry)
			if restores != tc.restores || !strings.Contains(why, tc.why) || (restores && why != "") {
				t.Errorf("Restores = %v, %q; want %v saying %q", restores, why, tc.restores, tc.why)
			}
		})
	}
}

// A CONTINUED OPENCODE RUN IS CONFIGURED WITH ITS OWN INSTRUCTIONS, as a fresh
// one is. An import writes none of OpenCode's per-session instruction state
// (OpenCode 2's session/transfer.ts), so a carried session's first
// request reads the run's instruction file as its first: this run's
// instructions are its system prompt, and no earlier launch's ride along as
// updates. So a continuing run is configured exactly as a fresh one, its
// instruction file named and written afresh.
//
// Mutation: configure a run that continues a session without its instruction
// file, or leave an earlier run's in place, and this goes red.
func TestAContinuedOpenCodeRunIsConfiguredWithItsOwnInstructions(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	configure := func(req sandbox.RunRequest) string {
		t.Helper()
		path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, req, executor.Session{}, p)
		if err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		return boxFile(t, box, path)
	}
	fresh := configure(sandbox.RunRequest{System: "the first launch's instructions"})
	continued := configure(sandbox.RunRequest{System: "this launch's instructions", Resume: carriedOpenCode,
		Carried: &sandbox.CarriedSession{Archive: sandbox.SessionArchive{Harness: executor.OpenCodeName,
			SessionID: carriedOpenCode, Format: executor.OpenCodeArchiveFormat, Version: "2.3.2"},
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
// script packs the session whatever the exit, so a harness killed under a
// living wrapper (as a memory limit kills it) leaves an archive whose
// transcript ends wherever the kill found it: the result says so beside the
// archive, for the capture to be kept as cut off; a clean exit says nothing.
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
