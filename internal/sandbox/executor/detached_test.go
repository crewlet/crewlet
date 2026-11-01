package executor_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// box mints a fake sandbox with the shared plumbing installed, reading whole
// up to [boxReadCap] rather than the real 32 MiB, so a case going past the cap
// builds a mebibyte rather than thirty-two of them. Every case here sizes a
// file past the cap from [sandbox.FakeSandbox.ReadCap]; that real boxes read
// to MaxFileBytes is the file contract's to certify.
func box(t *testing.T, runner *executor.Runner) *sandbox.FakeSandbox {
	t.Helper()
	b := sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return b
}

func paths(b sandbox.Sandbox) executor.Paths { return executor.PathsFor(b) }

// boxReadCap is the whole-read cap of the box [box] mints: four times the
// record's bound ([sandbox.MaxRunTextBytes]), so a report past what the
// record keeps — which the runner must hand on whole — is still a file the
// box reads whole, as it is on a real one.
const boxReadCap = 4 * sandbox.MaxRunTextBytes

// ---------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------

// A local backend runs many boxes on one filesystem; a shared work dir would
// have every run reading its neighbour's done marker.
func TestEveryArtefactPathIsUnderTheBoxsOwnHome(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	p := executor.PathsFor(b)
	for _, path := range []string{
		p.WorkDir(), p.Result(), p.Stream(), p.Err(), p.Done(), p.ExitCode(),
		p.MCPConfig(), p.Findings(), p.BinDir(),
		p.Brief(), p.System(), p.ClaudeNotesSystem(), p.ClaudeSettings(),
		p.CommitAttribution(), p.AttributionHook(), p.GitHooks(),
		p.SessionRecord(), p.SessionCapture(), p.SessionCarried(), p.SessionStaging(), p.SessionDigests(),
		p.ClaudeConfigDir(),
	} {
		if !strings.HasPrefix(path, b.Home()+"/") {
			t.Fatalf("%q is not under the box home %q", path, b.Home())
		}
	}
}

// ---------------------------------------------------------------------
// start
// ---------------------------------------------------------------------

func TestStartLaunchesDetachedAndWritesBothMarkers(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)

	handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "fix the flake"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.CommandID == "" {
		t.Fatal("Start returned no handle — nothing could ever probe the job")
	}
	// The whole thing is one shell-quoted argument to `sh -lc`, so the
	// assertions are on what survives that quoting: the paths themselves,
	// and the redirections around them.
	script := lastBackground(t, b)
	for _, want := range []string{
		// A headless agent must never block on input: its stdin is the
		// brief's file, which ends.
		`< '\''` + p.Brief() + `'\''`,
		p.Stream(),   // stdout is the event stream
		p.Result(),   // its last line is the result
		p.Err(),      // stderr is the transcript fallback
		p.ExitCode(), // the crash explanation
		p.Done(),     // the completion signal
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("the launch script is missing %q:\n%s", want, script)
		}
	}
	// The marker must CARRY THE EXIT CODE, not be a zero-byte touch:
	// ReadFile answers the same for a missing and an empty file, so a
	// touched marker would read as "not written yet" forever. Twice —
	// once into the exit-code file, once into the marker.
	if got := strings.Count(script, "echo $code >"); got != 2 {
		t.Fatalf("the exit code is written %d times, want 2 (the code file and the marker):\n%s", got, script)
	}
}

// An executable a setup step installs in the box's own bin directory only
// resolves ahead of the image's if that directory is first on the agent's PATH.
func TestTheBoxsBinDirectoryIsOnTheAgentsPath(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "fix it"})
	if !strings.Contains(script, paths(b).BinDir()) || !strings.Contains(script, `:"$PATH"`) {
		t.Fatalf("the box's bin directory is not on PATH:\n%s", script)
	}
}

// The findings path is a property of the box, which the launch does not know —
// so the runner adds the report instruction, addressed at this box, and adds it
// to the run's STANDING instructions: it is how every run ends, not part of
// what this one was asked. A run whose launch gave it no system text of its own
// still gets it, on the same channel.
func TestTheReportInstructionForThisBoxIsAStandingInstruction(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "fix the flake"})

	if brief, err := b.ReadFile(t.Context(), p.Brief()); err != nil || string(brief) != "fix the flake" {
		t.Fatalf("the brief was lost: %q (%v)", brief, err)
	}
	system, err := b.ReadFile(t.Context(), p.System())
	if err != nil || !strings.Contains(string(system), p.Findings()) {
		t.Fatalf("the agent was not told where to write its report (system text %q, %v)", system, err)
	}
	if strings.Contains(script, p.Findings()) || strings.Contains(script, "Before you finish") {
		t.Errorf("the report instruction rides the prompt the harness runs, not its system prompt:\n%s", script)
	}
	if !strings.Contains(script, "--append-system-prompt-file") {
		t.Errorf("the system text was written and never handed to the harness:\n%s", script)
	}
}

// A reused box carries the prior run's marker; without clearing it the poll
// fires immediately and collect reads the old result.
func TestStartClearsThePriorRunsArtefacts(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Done(), "0")
	b.Put(p.Findings(), "the previous run's report")

	if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "second pass"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cleared := ""
	for _, cmd := range b.Commands() {
		if strings.HasPrefix(cmd, "rm -f ") {
			cleared = cmd
		}
	}
	if cleared == "" {
		t.Fatal("the prior run's artefacts were never cleared")
	}
	for _, want := range []string{
		p.Done(), p.ExitCode(), p.Result(), p.Stream(), p.Findings(),
		// The previous run's instructions, which are not this run's — its
		// hooks name a bridge session its end closed.
		p.Brief(), p.System(), p.ClaudeNotesSystem(), p.ClaudeSettings(), p.OpenCodeConfig(),
		// And its session's pieces: a run that packs nothing must never be
		// collected with the last run's capture as its own, nor restore the
		// last run's carried archive.
		p.SessionRecord(), p.SessionCapture(), p.SessionCarried(),
	} {
		if !strings.Contains(cleared, want) {
			t.Fatalf("%q was not cleared: %s", want, cleared)
		}
	}
	// OpenCode's values and its notes plugin are DIRECTORIES, removed whole —
	// a run with fewer values than the last would keep the last one's beside
	// its own, and a run wired to no notes a plugin nothing names. So is the
	// commit attribution: a run that credits nothing leaves no hook behind,
	// and one naming another run never credits the last one. And so are a
	// run's own skills, on either harness, with what reports their loads: a
	// run handed fewer skills than the last would list the last one's too,
	// and report their loads to an endpoint that is over.
	_, removed, _ := strings.Cut(cleared, "; rm -rf ")
	for _, dir := range []string{p.OpenCodeValues(), p.OpenCodeSteerPlugin(), p.CommitAttribution(),
		p.SessionStaging(), p.ClaudeSkillsPlugin(), p.OpenCodeSkills(), p.OpenCodeSkillsPlugin()} {
		if !strings.Contains(removed, "'"+dir+"'") {
			t.Fatalf("the directory %q was not removed whole: %s", dir, cleared)
		}
	}
	// The CHECKOUT is left alone — that disk state is the whole reason to
	// reuse a box — and so is the harness's own session store, which is the
	// session a run in this box continues, and the digests of the archives
	// that store's copy holds, without which an archive the box packed
	// itself would be restored over the copy that ran on past it.
	if strings.Contains(cleared, "/"+sandbox.WorkspaceSubdir) {
		t.Fatalf("the clear reached into the checkout: %s", cleared)
	}
	if strings.Contains(cleared, p.ClaudeConfigDir()) {
		t.Fatalf("the clear reached into the harness's own sessions: %s", cleared)
	}
	if strings.Contains(cleared, p.SessionDigests()) {
		t.Fatalf("the clear removed the digests of the sessions the box holds: %s", cleared)
	}
}

// ---------------------------------------------------------------------
// poll — the three signals
// ---------------------------------------------------------------------

func TestARunningJobIsNotDone(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	// The wrapper answers the liveness probe.
	b.ExecFunc = alive(true)

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobRunning {
		t.Fatalf("Poll = %v, %v; want a running job", state, err)
	}
}

func TestTheDoneMarkerEndsTheRun(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)
	b.Put(paths(b).Done(), "0")

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobEnded {
		t.Fatalf("Poll = %v, %v; want ended", state, err)
	}
}

// A zero-byte marker reads identically to a missing one, which is why the
// shell writes the exit code into it.
func TestAnEmptyMarkerIsNotACompletion(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)
	b.Put(paths(b).Done(), "")

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobRunning {
		t.Fatalf("Poll = %v, %v; an empty marker is indistinguishable from none", state, err)
	}
}

// The whole process group died before the tail echo ran. Without this the run
// hangs forever.
func TestADeadWrapperWithNoMarkerEndsTheRun(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(false)

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobEnded {
		t.Fatalf("Poll = %v, %v; want the dead job ended and collected", state, err)
	}
}

// lostBox is a box that says its job was lost with the engine that last saw it
// running, as a local box does once its engine's restart killed it — writing
// the done marker as the box is asked, where marked says to: a wrapper that
// finished between the poll's first read and the box's answer.
type lostBox struct {
	*sandbox.FakeSandbox
	marker string
}

func (b *lostBox) Job(context.Context, string) (sandbox.JobState, error) {
	if b.marker != "" {
		b.Put(paths(b).Done(), b.marker)
	}
	return sandbox.JobLost, nil
}

// A JOB ITS BOX SAYS WAS LOST IS LOST, NOT ENDED: it went with the engine that
// last saw it running, mid-step, and what it left is no result — so the poll
// says so, and the waiter has its round carried on rather than collected.
//
// Mutation: answer every job that is not running as ended, and a job its
// engine's restart killed is collected as the round's result.
func TestAJobItsBoxSaysWasLostIsLost(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := &lostBox{FakeSandbox: box(t, runner)}
	handle := start(t, runner, b.FakeSandbox)

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobLost {
		t.Fatalf("Poll = %v, %v; want the job lost", state, err)
	}
}

// A JOB THAT WROTE ITS MARKER AS IT WAS ASKED ABOUT FINISHED: the wrapper
// writes the marker and exits, and one that did both between the poll's read
// and the box's answer is a job that ended, whatever the box then says of its
// process.
//
// Mutation: drop the second read of the marker, and a run that finished is
// started again.
func TestALostJobWhoseMarkerIsThereAfterAllEnded(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := &lostBox{FakeSandbox: box(t, runner), marker: "0"}
	handle := start(t, runner, b.FakeSandbox)

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobEnded {
		t.Fatalf("Poll = %v, %v; want the job that wrote its marker ended", state, err)
	}
}

// A genuinely hung-but-alive process is indistinguishable from a working one
// without a timer, and imposing one is exactly what this design refuses.
func TestAnAliveWrapperKeepsWaitingHoweverLong(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)

	for range 100 {
		if state, _ := runner.Poll(t.Context(), b, handle); state.Over() {
			t.Fatal("a live job was declared finished")
		}
	}
}

// An unreadable probe is not proof of death; collecting on one would read a
// partial result from a job that is still working.
func TestAnUnreadableLivenessProbeIsNotACompletion(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, context.DeadlineExceeded
	}
	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobRunning {
		t.Fatalf("Poll = %v, %v; want the next tick to ask again", state, err)
	}
}

// ---------------------------------------------------------------------
// collect
// ---------------------------------------------------------------------

// The report survives a run whose streamed message was lost and one that
// parsed to no text at all, so it wins.
func TestTheFindingsFileIsTheResultCarrierOfRecord(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"type":"result","result":"streamed summary","subtype":"success"}`)
	b.Put(p.Findings(), "Outcome: succeeded\nOpened https://github.com/acme/api/pull/42")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(res.Text, "Outcome: succeeded") {
		t.Fatalf("the report did not win: %q", res.Text)
	}
	if !res.Success {
		t.Fatal("a written report on a clean exit did not read as success")
	}
	if len(res.DeliveredRefs) != 1 || !strings.Contains(res.DeliveredRefs[0], "/pull/42") {
		t.Fatalf("the delivered ref was not scraped: %v", res.DeliveredRefs)
	}
}

// A report plus a crash is a partial run, not a success.
func TestACrashOverridesTheReportsSuccessSignal(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: partial")
	b.Put(p.ExitCode(), "137")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Success {
		t.Fatal("a run killed mid-flight reported success because it had written a report")
	}
}

// A silent stall must report a real failure, not an empty success.
func TestARunThatProducedNothingReportsWhyRatherThanStallingSilently(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Err(), "claude: command not found")
	b.Put(p.ExitCode(), "127")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Success {
		t.Fatal("a run that produced nothing reported success")
	}
	if !strings.Contains(res.Error, "127") || !strings.Contains(res.Error, "command not found") {
		t.Fatalf("the failure does not say what happened: %q", res.Error)
	}
}

// THE ERROR STREAM IS THE TRANSCRIPT ONLY WHERE THE STREAM SAYS NOTHING — a
// harness that never got as far as streaming, which says why on stderr. (It used
// to be the whole of Claude Code's transcript, on a fabricated premise: under
// `--output-format json` in print mode that harness writes nothing to stderr, so
// the case certified a narration no real run produced.)
func TestTheTranscriptFallsBackToStderrWhereTheStreamSaysNothing(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Err(), "error: unknown option '--output-formt'")
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Transcript != "error: unknown option '--output-formt'" {
		t.Fatalf("transcript = %q; want what the harness said before it could stream", res.Transcript)
	}
}

// Everything collected came out of a box whose environment holds the seat's
// credentials, and everything returned reaches a model, a store and a screen.
func TestEverythingCollectedIsRedacted(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	secret := "ghp_" + strings.Repeat("b", 36)
	b.Put(p.Findings(), "cloned with "+secret+"\nDelivered: "+secret)
	b.Put(p.Err(), "git clone https://"+secret+"@example.com/acme/api")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("refs = %q, want the one the report named", res.DeliveredRefs)
	}
	for name, field := range map[string]string{
		"text": res.Text, "transcript": res.Transcript, "error": res.Error,
		"delivered ref": res.DeliveredRefs[0],
	} {
		if strings.Contains(field, secret) {
			t.Fatalf("a credential survived in %s: %q", name, field)
		}
	}
}

// THE TRANSCRIPT LEAVES THE RUNNER WHOLE AND REDACTED. The record's bound is
// the coordinator's to apply, in one place, after it redacts again; a runner
// that cut it first would decide by position what of the log a reader sees,
// and a runner that forgot to cut it would no longer be a hole.
func TestTheTranscriptLeavesTheRunnerWholeAndRedacted(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	secret := "sk-ant-" + strings.Repeat("Q7", 20)
	// Just past the record's bound — the bound this runner must NOT apply —
	// and well inside the failure bound the stream is read to.
	stderr := "start " + secret + "\n" + strings.Repeat("日", sandbox.MaxRunTextBytes/3+1024) + "\nTHE CONCLUSION"
	b.Put(p.Findings(), "Outcome: succeeded")
	b.Put(p.Err(), stderr)

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if strings.Contains(res.Transcript, secret) {
		t.Error("a credential left the runner unredacted")
	}
	if !strings.HasPrefix(res.Transcript, "start ") || !strings.HasSuffix(res.Transcript, "THE CONCLUSION") ||
		len(res.Transcript) < sandbox.MaxRunTextBytes {
		t.Errorf("the transcript is %d bytes opening %.20q; want the whole of it", len(res.Transcript), res.Transcript)
	}
	if !utf8.ValidString(res.Transcript) {
		t.Error("the transcript is not valid UTF-8")
	}
}

// A crash explains itself at the bottom, so the failure a run that produced
// nothing reports is its WHOLE stderr behind its exit status: the runner
// bounds nothing a model reads, and the coordinator condenses a failure past
// the record's bound keeping its cause.
func TestACrashDetailIsCarriedWholeBehindItsStatus(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	// Past the record's bound, which the runner leaves to the coordinator,
	// and inside the failure bound it reads the stream to.
	stderr := strings.Repeat("noise\n", sandbox.MaxRunTextBytes/6+1024) + "FATAL: migrations/0007.sql is missing"
	b.Put(p.Err(), stderr)
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// The harness produced no output, so its own "no output" is the middle part.
	want := "the harness exited with status 1:\nthe harness produced no output:\n" + stderr
	if res.Error != want {
		t.Errorf("the failure is %d bytes opening %.80q, want the status, the harness's error and "+
			"the whole stderr (%d bytes)", len(res.Error), res.Error, len(want))
	}
}

// THE HARNESS'S OWN ERROR SURVIVES A STDERR. It used to be replaced by the stderr
// whenever the stderr held anything, so a run that hit its turn cap and
// printed one deprecation warning reported the warning as why it failed.
func TestTheHarnessErrorIsNotReplacedByItsStderr(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"type":"result","subtype":"error_max_turns","is_error":false,`+
		`"errors":["Reached maximum number of turns (30)"]}`)
	b.Put(p.Err(), "warning: a deprecated flag")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Error != "error_max_turns: Reached maximum number of turns (30):\nwarning: a deprecated flag" {
		t.Errorf("failure = %q, want the harness's error and then the stderr", res.Error)
	}
}

// THE REPORT LEAVES THE RUNNER WHOLE. It is what the run's reviewer judges the
// round on, and a cut keeping its head reads as the whole report; the
// coordinator holds it to the record's bound by condensing it.
func TestTheReportLeavesTheRunnerWhole(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	// Past the record's bound, which the runner leaves to the coordinator,
	// and inside the cap the box reads a report to.
	report := "Outcome: succeeded\n" + strings.Repeat("detail\n", sandbox.MaxRunTextBytes/7+1024)
	b.Put(p.Findings(), report)
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Text != strings.TrimSpace(report) {
		t.Errorf("the report is %d bytes, want the whole %d", len(res.Text), len(strings.TrimSpace(report)))
	}
	if !res.Success {
		t.Error("a long report stopped reading as a success")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func start(t *testing.T, r *executor.Runner, b *sandbox.FakeSandbox) sandbox.RunHandle {
	t.Helper()
	handle, err := r.Start(t.Context(), b, sandbox.RunRequest{Brief: "do the work"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle
}

func startAndScript(t *testing.T, r *executor.Runner, b *sandbox.FakeSandbox, req sandbox.RunRequest) string {
	t.Helper()
	if _, err := r.Start(t.Context(), b, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return lastBackground(t, b)
}

// alive answers the kill -0 probe.
func alive(yes bool) func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
	return func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if strings.HasPrefix(cmd, "kill -0 ") && !yes {
			return sandbox.ExecResult{ExitCode: 1}, nil
		}
		return sandbox.ExecResult{}, nil
	}
}

// lastBackground is the script the fake was asked to run detached.
func lastBackground(t *testing.T, b *sandbox.FakeSandbox) string {
	t.Helper()
	cmds := b.Background()
	if len(cmds) == 0 {
		t.Fatal("nothing was started in the background")
	}
	return cmds[len(cmds)-1]
}

// containerLike is a box whose network is not the engine host's, as a
// container box's is: it names the host by alias.
type containerLike struct {
	*sandbox.FakeSandbox
}

func (containerLike) EngineHost() string { return "host.docker.internal" }

// THE HARNESS DIALS ITS SERVERS FROM INSIDE THE BOX, so the config it is handed
// names the engine host as the box reaches it. Every run gets its config
// through Start, a seat's executor run and the assistant's deep work alike,
// so the translation is here and only here: a
// bridge at `127.0.0.1` was written into a container's config as it stood,
// and every call the harness made reached the container's own loopback.
func TestTheBoxsConfigNamesTheEngineHostAsTheBoxReachesIt(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := containerLike{sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)}
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	servers := map[string]sandbox.MCPServer{
		"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:8000/mcp/tok"},
	}
	if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "b", MCPServers: servers}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	body, err := b.ReadFile(t.Context(), paths(b).MCPConfig())
	if err != nil {
		t.Fatalf("reading the MCP config: %v", err)
	}
	if !strings.Contains(string(body), "http://host.docker.internal:8000/mcp/tok") ||
		strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("the box's MCP config does not name the engine host by its alias:\n%s", body)
	}
	if servers["crewlet"].URL != "http://127.0.0.1:8000/mcp/tok" {
		t.Fatalf("the caller's servers were rewritten: %q", servers["crewlet"].URL)
	}
}

// THE NOTES ROUTE IS DIALLED FROM INSIDE THE BOX TOO. OpenCode's plugin polls
// the run's notes route by its URL rather than through the MCP server Claude
// Code's hooks name, so that URL is translated in Start like the servers'
// own: written as the engine host's loopback, a plugin in a container box
// polled the container itself and its run read no note.
//
// Mutation: drop the notes route's translation from Start, and the plugin's
// route names 127.0.0.1.
func TestTheBoxsNotesRouteNamesTheEngineHostAsTheBoxReachesIt(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := containerLike{sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)}
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	const route = "http://127.0.0.1:8000/mcp/tok/steer"
	req := sandbox.RunRequest{Brief: "b",
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: route}}
	if _, err := runner.Start(t.Context(), b, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	raw, err := b.ReadFile(t.Context(), paths(b).OpenCodeConfig())
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}
	var cfg struct {
		Plugins []openCodePlugin `json:"plugins"`
	}
	if err = json.Unmarshal(raw, &cfg); err != nil || len(cfg.Plugins) != 1 {
		t.Fatalf("the configuration names no notes plugin (%v):\n%s", err, raw)
	}
	file, ok := referencedPath(cfg.Plugins[0].Options.URL)
	if !ok {
		t.Fatalf("the plugin's route is %q, want a reference to a file", cfg.Plugins[0].Options.URL)
	}
	value, err := b.ReadFile(t.Context(), file)
	if err != nil {
		t.Fatalf("reading the route's file: %v", err)
	}
	if got := strings.TrimSpace(string(value)); got != "http://host.docker.internal:8000/mcp/tok/steer" {
		t.Fatalf("the plugin polls %q, want the engine host by its alias", got)
	}
	if req.Steer.URL != route {
		t.Fatalf("the caller's wiring was rewritten: %q", req.Steer.URL)
	}
}
