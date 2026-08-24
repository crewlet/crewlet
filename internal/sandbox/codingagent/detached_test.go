package codingagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// box mints a fake sandbox with the shared plumbing installed.
func box(t *testing.T, runner *codingagent.Runner) *sandbox.FakeSandbox {
	t.Helper()
	b := sandbox.NewFakeSandbox("box-1")
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return b
}

func paths(b sandbox.Sandbox) codingagent.Paths { return codingagent.PathsFor(b) }

// ---------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------

// A local backend runs many boxes on one filesystem; a shared work dir would
// have every run reading its neighbour's done marker.
func TestEveryArtefactPathIsUnderTheBoxsOwnHome(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	p := codingagent.PathsFor(b)
	for _, path := range []string{
		p.WorkDir(), p.Result(), p.Err(), p.Done(), p.ExitCode(),
		p.Ask(), p.MCPConfig(), p.Findings(), p.BinDir(), p.AskShim(),
	} {
		if !strings.HasPrefix(path, b.Home()+"/") {
			t.Fatalf("%q is not under the box home %q", path, b.Home())
		}
	}
}

// The shim goes under the box, not a system directory: a local backend runs as
// an unprivileged user and two boxes would overwrite each other there.
func TestTheAskShimLivesInTheBoxNotASystemPath(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)

	shim, err := b.ReadFile(t.Context(), p.AskShim())
	if err != nil || len(shim) == 0 {
		t.Fatalf("the ask shim was not installed: %v", err)
	}
	if strings.HasPrefix(p.AskShim(), "/usr/") {
		t.Fatalf("the shim was installed at %q", p.AskShim())
	}
	if !strings.Contains(string(shim), p.Ask()) {
		t.Fatalf("the shim does not write to this box's ask path:\n%s", shim)
	}
}

// ---------------------------------------------------------------------
// start
// ---------------------------------------------------------------------

func TestStartLaunchesDetachedAndWritesBothMarkers(t *testing.T) {
	runner := codingagent.NewClaudeCode()
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
		"< /dev/null", // a headless agent must never block on input
		p.Result(),    // stdout is the parseable output
		p.Err(),       // stderr is the transcript fallback
		p.ExitCode(),  // the crash explanation
		p.Done(),      // the completion signal
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

// The brief's `crewlet-ask` instruction only resolves if the shim directory is
// on the agent's PATH.
func TestTheShimDirectoryIsOnTheAgentsPath(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "fix it"})
	if !strings.Contains(script, paths(b).BinDir()) || !strings.Contains(script, `:"$PATH"`) {
		t.Fatalf("the shim directory is not on PATH:\n%s", script)
	}
}

// The findings path is a property of the box, which the launch does not know.
func TestTheBriefGainsTheReportInstructionForThisBox(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "fix the flake"})

	if !strings.Contains(script, "fix the flake") {
		t.Fatalf("the brief was lost:\n%s", script)
	}
	if !strings.Contains(script, paths(b).Findings()) {
		t.Fatalf("the agent was not told where to write its report:\n%s", script)
	}
}

// A reused box carries the prior run's marker; without clearing it the poll
// fires immediately and collect reads the old result.
func TestStartClearsThePriorRunsArtefacts(t *testing.T) {
	runner := codingagent.NewClaudeCode()
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
	for _, want := range []string{p.Done(), p.ExitCode(), p.Result(), p.Findings(), p.Ask()} {
		if !strings.Contains(cleared, want) {
			t.Fatalf("%q was not cleared: %s", want, cleared)
		}
	}
	// The CHECKOUT is left alone — that disk state is the whole reason to
	// reuse a box.
	if strings.Contains(cleared, "/"+sandbox.WorkspaceSubdir) {
		t.Fatalf("the clear reached into the checkout: %s", cleared)
	}
}

// ---------------------------------------------------------------------
// poll — the three signals
// ---------------------------------------------------------------------

func TestARunningJobIsNotDone(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	// The wrapper answers the liveness probe.
	b.ExecFunc = alive(true)

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || done {
		t.Fatalf("Poll = %v, %v; want a running job", done, err)
	}
}

func TestTheDoneMarkerEndsTheRun(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)
	b.Put(paths(b).Done(), "0")

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || !done {
		t.Fatalf("Poll = %v, %v; want done", done, err)
	}
}

// A zero-byte marker reads identically to a missing one, which is why the
// shell writes the exit code into it.
func TestAnEmptyMarkerIsNotACompletion(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)
	b.Put(paths(b).Done(), "")

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || done {
		t.Fatalf("Poll = %v, %v; an empty marker is indistinguishable from none", done, err)
	}
}

// The whole process group died before the tail echo ran. Without this the run
// hangs forever.
func TestADeadWrapperWithNoMarkerEndsTheRun(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(false)

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || !done {
		t.Fatalf("Poll = %v, %v; want the dead job reaped", done, err)
	}
}

// A genuinely hung-but-alive process is indistinguishable from a working one
// without a timer, and imposing one is exactly what this design refuses.
func TestAnAliveWrapperKeepsWaitingHoweverLong(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = alive(true)

	for range 100 {
		if done, _ := runner.Poll(t.Context(), b, handle); done {
			t.Fatal("a live job was declared finished")
		}
	}
}

// An unreadable probe is not proof of death; collecting on one would read a
// partial result from a job that is still working.
func TestAnUnreadableLivenessProbeIsNotACompletion(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, context.DeadlineExceeded
	}
	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || done {
		t.Fatalf("Poll = %v, %v; want the next tick to ask again", done, err)
	}
}

// ---------------------------------------------------------------------
// collect
// ---------------------------------------------------------------------

// The report survives a run whose streamed message was lost and one that
// parsed to no text at all, so it wins.
func TestTheFindingsFileIsTheResultCarrierOfRecord(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"result":"streamed summary","subtype":"success"}`)
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
	runner := codingagent.NewClaudeCode()
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
	runner := codingagent.NewClaudeCode()
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

// The transcript is the observability surface for an agent that emits no
// telemetry of its own.
func TestTheTranscriptFallsBackToStderr(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"result":"done","subtype":"success"}`)
	b.Put(p.Err(), "cloning…\nrunning tests…")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(res.Transcript, "running tests") {
		t.Fatalf("transcript = %q", res.Transcript)
	}
}

// Everything collected came out of a box whose environment holds the seat's
// credentials, and everything returned reaches a model, a store and a screen.
func TestEverythingCollectedIsRedacted(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	secret := "ghp_" + strings.Repeat("b", 36)
	b.Put(p.Findings(), "cloned with "+secret)
	b.Put(p.Err(), "git clone https://"+secret+"@example.com/acme/api")
	b.Put(p.Ask(), `{"question":"is `+secret+` right?","to":"requester"}`)

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for name, field := range map[string]string{
		"text": res.Text, "transcript": res.Transcript,
		"error": res.Error, "question": res.Question,
	} {
		if strings.Contains(field, secret) {
			t.Fatalf("a credential survived in %s: %q", name, field)
		}
	}
}

func TestARecordedQuestionSurfacesAsNeedingInput(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: blocked")
	b.Put(p.Ask(), `{"question":"which branch should I target?","to":"team"}`)

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.NeedsInput {
		t.Fatal("a recorded question did not park the run")
	}
	if res.Question != "which branch should I target?" || res.AskTo != "team" {
		t.Fatalf("question = %q, to = %q", res.Question, res.AskTo)
	}
}

// A question with no audience is still a question.
func TestAQuestionWithNoAudienceDefaultsToTheRequester(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	b.Put(paths(b).Ask(), `{"question":"which branch?"}`)

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.NeedsInput || res.AskTo != "requester" {
		t.Fatalf("NeedsInput = %v, AskTo = %q", res.NeedsInput, res.AskTo)
	}
}

// A malformed signal must not lose the result the run did produce.
func TestAMalformedAskDoesNotLoseTheResult(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: succeeded")
	b.Put(p.Ask(), "{not json")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.NeedsInput {
		t.Fatal("an unreadable signal parked the run")
	}
	if !strings.Contains(res.Text, "succeeded") {
		t.Fatalf("the result was lost: %q", res.Text)
	}
}

func TestTheTranscriptIsTailCapped(t *testing.T) {
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: succeeded")
	b.Put(p.Err(), strings.Repeat("x", codingagent.MaxTranscript*2)+"\nTHE CONCLUSION")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(res.Transcript) > codingagent.MaxTranscript+64 {
		t.Fatalf("transcript is %d characters", len(res.Transcript))
	}
	// The TAIL is kept: the conclusion is what a reader wants.
	if !strings.Contains(res.Transcript, "THE CONCLUSION") {
		t.Fatal("the tail cap dropped the end of the transcript instead of the start")
	}
	if !strings.Contains(res.Transcript, "truncated") {
		t.Fatal("the cap was silent")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func start(t *testing.T, r *codingagent.Runner, b *sandbox.FakeSandbox) sandbox.RunHandle {
	t.Helper()
	handle, err := r.Start(t.Context(), b, sandbox.RunRequest{Brief: "do the work"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle
}

func startAndScript(t *testing.T, r *codingagent.Runner, b *sandbox.FakeSandbox, req sandbox.RunRequest) string {
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

func shellQuoted(s string) string { return "'" + s + "'" }

// lastBackground is the script the fake was asked to run detached.
func lastBackground(t *testing.T, b *sandbox.FakeSandbox) string {
	t.Helper()
	cmds := b.Background()
	if len(cmds) == 0 {
		t.Fatal("nothing was started in the background")
	}
	return cmds[len(cmds)-1]
}
