package executor_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// endingHarness is a stand-in `claude` that files its session as
// [sessionHarness] does and then WORKS ON, a sleep only a signal ends: a harness
// an end request finds mid-run. ignoreTerm makes it ignore SIGTERM.
//
// It writes [workingMarker] last, and a case asks the end only once that is
// there ([harnessWorking]). The transcript is filed FIRST, before its sidecars,
// task list and the ignore, so an end asked on the transcript alone could catch
// a capture missing its sidecars, or kill a harness meant to outlive its grace
// (recorded as asked with 143, not killed with 137).
func endingHarness(release string, ignoreTerm bool) string {
	harness := sessionHarness(release)
	work := `echo working > "$HOME/` + workingMarker + "\"\nexec sleep 300\n"
	if ignoreTerm {
		// IGNORED, never trapped: an ignored signal stays ignored across the
		// exec, and the sleep is what the end has to stop.
		work = "trap '' TERM\n" + work
	}
	return strings.Replace(harness, `printf '{"type":"result"`, work+`printf '{"type":"result"`, 1)
}

// workingMarker is the file, in the box's home, an [endingHarness] writes once
// it is working.
const workingMarker = "harness-working"

// harnessWorking waits for the [endingHarness] in box to be working: its
// session filed whole and, where it ignores SIGTERM, the ignore in place.
func harnessWorking(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	boxFileAppears(t, box, box.Home()+"/"+workingMarker, "the harness to file its session and work on")
}

// timedClaude is Claude Code with its harness given stop to go once its run is
// asked to end ([executor.ClaudeCodeCommandTimed]), for a case about a harness
// that outlives its grace.
type timedClaude struct {
	executor.ClaudeCode
	stop time.Duration
}

func (c timedClaude) Command(req sandbox.RunRequest, session executor.Session, paths executor.Paths,
	configPath string) string {
	return executor.ClaudeCodeCommandTimed(req, session, paths, configPath, c.stop)
}

// boxFileAppears waits for path in a real box to exist, failing the case after
// ten seconds.
func boxFileAppears(t *testing.T, box sandbox.Sandbox, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := box.ReadFile(t.Context(), path); err == nil && len(raw) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s (%s)", what, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitOver polls the job handle names until it is over, failing the case
// after limit, which says why it is that long.
func awaitOver(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, handle sandbox.RunHandle,
	limit time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		state, err := runner.Poll(t.Context(), box, handle)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if state.Over() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job was not over %s on, and %s:\n%s", limit, why,
				boxFile(t, box, executor.PathsFor(box).Err()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startedRecord is what a stand-in harness recorded at path as it started. A
// box reads a missing file as empty, so a run that ended before its harness
// started would read as a harness that started on nothing: that run fails as
// itself, with its exit code and error stream.
func startedRecord(t *testing.T, box sandbox.Sandbox, path string) string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		p := executor.PathsFor(box)
		t.Fatalf("the harness never started (%v): the run exited %q, its error stream:\n%s", err,
			strings.TrimSpace(boxFile(t, box, p.ExitCode())), boxFile(t, box, p.Err()))
	}
	return boxFile(t, box, path)
}

// askEnd asks the job handle names to end and fails the case unless the
// request was delivered.
func askEnd(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, handle sandbox.RunHandle) {
	t.Helper()
	ask, err := runner.AskEnd(t.Context(), box, handle)
	if err != nil || ask != sandbox.EndAsked {
		t.Fatalf("AskEnd = %q, %v; want the request delivered to the job's script", ask, err)
	}
}

// AN ENDED CLAUDE CODE RUN PACKS AS ITS OWN END DOES, under every sh a box may
// have: the end request stops the working harness, the script packs the session
// through its own exit's pack (transcript, sidecars, task list, digest kept),
// records the end and exits with the harness's status, which the wrapper writes
// to the done marker. The collection reads not succeeded, opening with the end.
//
// Mutations: drop the script's trap, and SIGUSR1's default kills it with no pack
// and no record; pack only on a clean exit, and there is no capture; drop the
// second wait, and the exit is the interrupted wait's 138.
func TestAnEndedClaudeCodeRunPacksAsItsOwnEndDoes(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		runner, box := realBoxUnder(t, sh)
		writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", endingHarness("2.1.310", false))
		p := executor.PathsFor(box)
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		harnessWorking(t, box)
		askEnd(t, runner, box, handle)
		awaitOver(t, runner, box, handle, 15*time.Second, "the end gives its harness five seconds")

		if ended := boxFile(t, box, p.JobEnded()); ended != "asked\n" {
			t.Errorf("the job's end is recorded as %q, want asked", ended)
		}
		if code := boxFile(t, box, p.ExitCode()); code != "143\n" {
			t.Errorf("the run exited %q, want its harness's own 143", code)
		}
		if done := boxFile(t, box, p.Done()); done != "143\n" {
			t.Errorf("the done marker holds %q, want the harness's status", done)
		}
		res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		const opening = "the run was ended on request before it finished: its harness was told to stop and exited 143"
		if res.Success || res.JobEnd != sandbox.JobEndAsked || !strings.HasPrefix(res.Error, opening) {
			t.Errorf("the collection reads success %v, end %q, failure %q; want a run that says it was ended",
				res.Success, res.JobEnd, res.Error)
		}
		if res.Captured == nil {
			t.Fatalf("the ended run packed no session:\n%s", boxFile(t, box, p.Err()))
		}
		described, body, err := runner.OpenCapture(t.Context(), box)
		if err != nil || body == nil {
			t.Fatalf("OpenCapture = %v, %v", body, err)
		}
		defer func() { _ = body.Close() }()
		members := archiveMembers(t, body)
		project := "projects/" + executor.ClaudeProjectDir + "/"
		session := handle.SessionID
		for _, want := range []string{project + session + ".jsonl", project + session + "/subagents/agent-1.jsonl",
			project + session + "/tool-results/out.txt", "tasks/" + session + "/1.json"} {
			if _, ok := members[want]; !ok {
				t.Errorf("the ended run's capture lacks %s: %v", want, slices.Sorted(maps.Keys(members)))
			}
		}
		for name := range members {
			if !strings.HasPrefix(name, project+session) && !strings.HasPrefix(name, "tasks/"+session+"/") {
				t.Errorf("the ended run's capture holds %s, which is no part of the session", name)
			}
		}
		capture := []byte(boxFile(t, box, p.SessionCapture()))
		if digests := boxFile(t, box, p.SessionDigests()); !strings.Contains(digests,
			session+" "+digestOf(t, capture)+"\n") || described.SessionID != session {
			t.Errorf("the box keeps no digest of the ended run's capture: %q", digests)
		}
	})
}

// A CLAUDE CODE HARNESS THAT OUTLIVES ITS GRACE IS KILLED, THEN PACKED, under
// every sh a box may have: a harness ignoring SIGTERM is killed when its grace
// is out, the end recorded as a kill, and its session packed all the same.
//
// Mutation: drop the kill after the grace, and the script waits for its
// harness for ever, past the case's bound.
func TestAClaudeCodeHarnessThatOutlivesItsGraceIsKilledThenPacked(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		_, box := realBoxUnder(t, sh)
		runner := executor.New(timedClaude{stop: 300 * time.Millisecond})
		writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", endingHarness("2.1.310", true))
		p := executor.PathsFor(box)
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		harnessWorking(t, box)
		askEnd(t, runner, box, handle)
		awaitOver(t, runner, box, handle, 10*time.Second, "the harness is given a third of a second here")

		if ended := boxFile(t, box, p.JobEnded()); ended != "killed\n" {
			t.Errorf("the job's end is recorded as %q, want killed", ended)
		}
		if code := boxFile(t, box, p.ExitCode()); code != "137\n" {
			t.Errorf("the run exited %q, want the killed harness's 137", code)
		}
		res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if res.Captured == nil || !strings.HasPrefix(res.Error,
			"the run was ended on request before it finished: its harness did not stop within") {
			t.Errorf("the killed run collected capture %+v, failure %q", res.Captured, res.Error)
		}
	})
}

// AN END ASKED BEFORE THE HARNESS STARTS STARTS NOTHING, under every sh a box
// may have. The request comes while the script asks the harness its release,
// right after publishing its pid, and is handled once that returns: the script
// starts no harness, records the end as `before`, and exits with SIGTERM's
// status.
//
// Mutation: drop the check before the launch, and the harness runs its brief.
func TestAnEndAskedBeforeTheHarnessStartsStartsNothing(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		runner, box := realBoxUnder(t, sh)
		p := executor.PathsFor(box)
		asked := box.Home() + "/release-asked"
		harness := strings.Replace(sessionHarness("2.1.310"),
			`if [ "$1" = --version ]; then echo`,
			`if [ "$1" = --version ]; then : > `+shellword.Quote(asked)+`; sleep 2; echo`, 1)
		writeBoxStandIn(t, box, p.BinDir()+"/claude", harness)
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		boxFileAppears(t, box, p.JobScript(), "the script to publish its pid")
		waitForFile(t, asked, "the script to ask its harness's release")
		askEnd(t, runner, box, handle)
		awaitOver(t, runner, box, handle, 15*time.Second, "the release takes two seconds here")

		if ended := boxFile(t, box, p.JobEnded()); ended != "before\n" {
			t.Errorf("the job's end is recorded as %q, want before", ended)
		}
		if code := boxFile(t, box, p.ExitCode()); code != "143\n" {
			t.Errorf("the run exited %q, want 143", code)
		}
		if argv := boxFile(t, box, box.Home()+"/argv"); argv != "" {
			t.Errorf("the harness ran its brief after its run was asked to end:\n%s", argv)
		}
	})
}

// waitForFile waits for a host path to exist, failing the case after ten
// seconds.
func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE SCRIPT PUBLISHES ITS PID ONLY ONCE ITS TRAP IS SET, so a request sent the
// moment the pid is readable meets the handler, never SIGUSR1's default. The
// script's text sets the trap, then publishes `1 <pid>`, before it runs the
// harness; and under every sh a box may have, a stand-in whose `--version` reads
// the record and sends the request at once leaves a job ended and recorded so.
//
// Mutation: write the pid before the trap, and the script's text says so; send
// the request through a record that names another process, and the job is
// never ended.
func TestTheScriptPublishesItsPIDOnlyOnceItsTrapIsSet(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/box"}
	for _, harness := range []executor.Harness{executor.ClaudeCode{}, executor.OpenCode{}} {
		script := scriptOf(t, commandOf(harness, sandbox.RunRequest{Brief: "x"}, p, ""))
		trap := strings.Index(script, "trap endask USR1\n")
		published := strings.Index(script, `printf '1 %s\n' "$$" > '`+p.JobScript()+`.next'`)
		first := strings.Index(script, harness.Name())
		if harness.Name() == executor.ClaudeCodeName {
			first = strings.Index(script, "claude ")
		}
		if trap < 0 || published < trap || first < published {
			t.Errorf("%s's script does not trap the request, then publish its pid, before it runs anything:\n%s",
				harness.Name(), script)
		}
	}

	forEachShell(t, func(t *testing.T, sh boxShell) {
		runner, box := realBoxUnder(t, sh)
		bp := executor.PathsFor(box)
		seen := box.Home() + "/job-script-at-release"
		harness := strings.Replace(sessionHarness("2.1.310"),
			`if [ "$1" = --version ]; then echo`,
			`if [ "$1" = --version ]; then cp `+shellword.Quote(bp.JobScript())+` `+shellword.Quote(seen)+
				`; read -r proto pid < `+shellword.Quote(bp.JobScript())+`; kill -USR1 "$pid"; echo`, 1)
		writeBoxStandIn(t, box, bp.BinDir()+"/claude", harness)
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, 15*time.Second, "the run starts nothing here")
		recorded := boxFile(t, box, seen)
		fields := strings.Fields(recorded)
		if len(fields) != 2 || fields[0] != "1" {
			t.Fatalf("the script's record at its release gate is %q, want `1 <pid>`", recorded)
		}
		if pid, err := strconv.Atoi(fields[1]); err != nil || pid < 2 {
			t.Fatalf("the script's record names %q, no pid", fields[1])
		}
		if ended := boxFile(t, box, bp.JobEnded()); ended != "before\n" {
			t.Errorf("a request sent the moment the pid could be read left the job recorded %q, want before; "+
				"its exit %q", ended, boxFile(t, box, bp.ExitCode()))
		}
	})
}

// slowTar is a prelude for a run's script that shadows its `tar` with a
// FUNCTION, which a shell runs ahead of applets and PATH alike (a busybox that
// prefers its own applets, Debian's and Ubuntu's busybox-static among them,
// passes over a PATH stand-in). It marks begun, then: with partial, writes part
// of an archive at its -f path and sleeps for good; otherwise sleeps a second
// and runs the box's tar. Its variables are its own, leaving the script's alone.
func slowTar(begun string, partial bool) string {
	if partial {
		return `tar() { slowtarout= slowtarprev=; for slowtararg; do [ "$slowtarprev" = -czf ] && ` +
			`slowtarout=$slowtararg; slowtarprev=$slowtararg; done; [ -z "$slowtarout" ] || ` +
			`printf 'part of an archive' > "$slowtarout"; : > ` + shellword.Quote(begun) + `; sleep 300; }` + "\n"
	}
	return `tar() { : > ` + shellword.Quote(begun) + `; sleep 1; command tar "$@"; }` + "\n"
}

// A SECOND END REQUEST LEAVES THE PACK WHOLE, on Claude Code as on OpenCode (see
// the OpenCode case beside the script's), under every sh a box may have: a
// request during the ended run's pack is ignored, the archive whole and the exit
// the harness's.
//
// Mutation: restore SIGUSR1's default once the end is under way rather than
// ignoring it, and the second request kills the script mid-pack, leaving no
// archive.
func TestASecondEndRequestLeavesAClaudeCodePackWhole(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		runner, box := realBox(t)
		begun := box.Home() + "/tar-begun"
		runsUnder(t, box, sh, slowTar(begun, false))
		p := executor.PathsFor(box)
		writeBoxStandIn(t, box, p.BinDir()+"/claude", endingHarness("2.1.310", false))
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		harnessWorking(t, box)
		askEnd(t, runner, box, handle)
		waitForFile(t, begun, "the ended run to pack")
		askEnd(t, runner, box, handle)
		awaitOver(t, runner, box, handle, 15*time.Second, "the pack takes a second here")

		if code := boxFile(t, box, p.ExitCode()); code != "143\n" {
			t.Errorf("the run exited %q, want its harness's 143", code)
		}
		_, body, err := runner.OpenCapture(t.Context(), box)
		if err != nil || body == nil {
			t.Fatalf("the run left no whole capture: %v, %v\n%s", body, err, boxFile(t, box, p.Err()))
		}
		defer func() { _ = body.Close() }()
		if members := archiveMembers(t, body); len(members) == 0 {
			t.Error("the capture holds nothing")
		}
	})
}

// A PACK KILLED MIDWAY LEAVES NO ARCHIVE WHERE THE TAKE READS, under every sh a
// box may have: the script packs in its staging directory and moves a whole
// archive into place, so a job killed mid-pack leaves only the staging
// directory, which the clear before the next run removes. AND THE JOB'S END
// STANDS: recorded before the pack, so a run killed mid-pack (its wrapper killed
// too, writing no exit status) still collects as ended on request, never as the
// success its harness's report would make it.
//
// Mutations: tar straight into the capture's path, and the kill leaves part of
// an archive where the take reads it as whole; record the end only after the
// pack, and the run killed mid-pack reads as a success.
func TestAPackKilledMidwayLeavesNoArchiveWhereTheTakeReads(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		runner, box := realBox(t)
		begun := box.Home() + "/tar-begun"
		runsUnder(t, box, sh, slowTar(begun, true))
		p := executor.PathsFor(box)
		writeBoxStandIn(t, box, p.BinDir()+"/claude", endingHarness("2.1.310", false))
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		harnessWorking(t, box)
		// THE REPORT A HARNESS WRITES before it stops, which reads as a
		// success where nothing says the run was ended.
		if err := box.WriteFile(t.Context(), p.Findings(), []byte("fixed the flake")); err != nil {
			t.Fatal(err)
		}
		askEnd(t, runner, box, handle)
		waitForFile(t, begun, "the ended run to pack")
		if err := box.KillJob(t.Context(), handle.CommandID); err != nil {
			t.Fatalf("KillJob: %v", err)
		}
		awaitOver(t, runner, box, handle, 15*time.Second, "the job was killed")

		if ended := boxFile(t, box, p.JobEnded()); ended != "asked\n" {
			t.Errorf("the job killed mid-pack has its end recorded as %q, want asked", ended)
		}
		res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		const opening = "the run was ended on request before it finished: its harness was told to stop"
		if res.Success || res.JobEnd != sandbox.JobEndAsked || !strings.HasPrefix(res.Error, opening) {
			t.Errorf("the run killed mid-pack collects success %v, end %q, failure %q; want one that says it "+
				"was ended", res.Success, res.JobEnd, res.Error)
		}
		if left := boxFile(t, box, p.SessionCapture()); left != "" {
			t.Fatalf("the killed pack left %q where the take reads a whole archive", left)
		}
		if _, err := os.Stat(p.SessionStaging()); err != nil {
			t.Fatalf("the premise: the killed pack left no staging directory (%v)", err)
		}
		if err := runner.ClearArtifacts(t.Context(), box); err != nil {
			t.Fatalf("ClearArtifacts: %v", err)
		}
		if _, err := os.Stat(p.SessionStaging()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the killed pack's staging directory outlived the clear before the next run: %v", err)
		}
	})
}

// AN END IS ASKED OF THE PID THE SCRIPT RECORDED, and only of one recorded in
// the protocol this build speaks: `1 <pid>` with a pid above 1 is sent the
// request through the box; a box with no record, a record of another
// protocol, a pid that is no number or below 2, are asked nothing and answer
// unheard; and a pid the box says is no process of the job answers gone.
//
// Mutation: read the pid off a record of another protocol, or accept pid 1, and
// a request is sent where none should be.
func TestAnEndIsAskedOfThePIDTheScriptRecorded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		recorded string
		endErr   error
		want     sandbox.EndAsk
		call     string
		fails    bool
	}{
		"the protocol's record":       {recorded: "1 4242\n", want: sandbox.EndAsked, call: "end cmd-1 4242"},
		"no record":                   {want: sandbox.EndUnheard},
		"another protocol":            {recorded: "2 4242\n", want: sandbox.EndUnheard},
		"no pid":                      {recorded: "1 x\n", want: sandbox.EndUnheard},
		"pid 1":                       {recorded: "1 1\n", want: sandbox.EndUnheard},
		"pid 0":                       {recorded: "1 0\n", want: sandbox.EndUnheard},
		"more than a pid":             {recorded: "1 4242 4243\n", want: sandbox.EndUnheard},
		"a pid that is not the job's": {recorded: "1 4242\n", endErr: sandbox.ErrNotJobProcess, want: sandbox.EndGone, call: "end cmd-1 4242"},
		"a box that cannot be asked": {recorded: "1 4242\n", endErr: errors.New("the runtime is down"),
			fails: true, call: "end cmd-1 4242"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			if tc.recorded != "" {
				b.Put(paths(b).JobScript(), tc.recorded)
			}
			if tc.endErr != nil {
				b.EndJobFunc = func(context.Context, string, int) error { return tc.endErr }
			}
			ask, err := runner.AskEnd(t.Context(), b, sandbox.RunHandle{CommandID: "cmd-1"})
			if tc.fails {
				if err == nil {
					t.Fatalf("AskEnd = %q, nil; want the box's failure", ask)
				}
			} else if err != nil || ask != tc.want {
				t.Fatalf("AskEnd = %q, %v; want %q", ask, err, tc.want)
			}
			var calls []string
			if tc.call != "" {
				calls = []string{tc.call}
			}
			if got := b.JobCalls(); !slices.Equal(got, calls) {
				t.Fatalf("the box was asked %v, want %v", got, calls)
			}
		})
	}
}

// A COLLECTION SAYS ITS JOB WAS ENDED. A run whose job its script records as
// ended on request did not finish, whatever it wrote and however its harness
// exited — a harness that exits 0 when told to stop, with a report written,
// included — so it never reads as a success, and its failure opens with how it
// was ended; a record of a word this build does not know is no end.
//
// Mutation: ignore the record, and a run ended gracefully with a report reads as
// a success.
func TestACollectionSaysItsJobWasEnded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ended, code string
		success     bool
		opening     string
	}{
		"asked, exiting 0": {ended: "asked\n", code: "0\n",
			opening: "the run was ended on request before it finished: its harness was told to stop and exited 0"},
		"killed": {ended: "killed\n", code: "137\n",
			opening: "the run was ended on request before it finished: its harness did not stop within 5s and was killed"},
		"before": {ended: "before\n", code: "143\n",
			opening: "the run was ended on request before it finished: before its harness started"},
		"a word this build does not know": {ended: "maybe\n", code: "0\n", success: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			b.Put(p.Done(), tc.code)
			b.Put(p.ExitCode(), tc.code)
			b.Put(p.Findings(), "fixed the flake")
			// The harness's own end, beside which the report is a success.
			b.Put(p.Result(), `{"type":"result","subtype":"success","result":"done"}`)
			b.Put(p.JobEnded(), tc.ended)
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{CommandID: "1"}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if res.Success != tc.success || !strings.HasPrefix(res.Error, tc.opening) {
				t.Fatalf("collected success %v, failure %q; want success %v and a failure opening %q",
					res.Success, res.Error, tc.success, tc.opening)
			}
			if res.Text != "fixed the flake" {
				t.Errorf("the report is %q, want what the run wrote", res.Text)
			}
		})
	}
}

// A TAKE OF AN ENDED JOB PACKS ONLY WHERE ITS SCRIPT LEFT NO ARCHIVE. A job that
// is over is taken as its script packed it, with no pack made again; one whose
// script left none (killed before its pack, lost with its engine) is packed now,
// by the same pack, its digest kept.
//
// Mutation: answer a take of a job that is over with the archive alone, and a
// job that left none keeps no session.
func TestATakeOfAnEndedJobPacksOnlyWhereItsScriptLeftNoArchive(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	p := executor.PathsFor(box)
	handle, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	if res.Captured == nil {
		t.Fatalf("the premise: the run packed no session:\n%s", boxFile(t, box, p.Err()))
	}
	before, err := os.Stat(p.SessionCapture())
	if err != nil {
		t.Fatal(err)
	}
	taken, err := runner.TakeSession(t.Context(), box, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("TakeSession of the archive the script left = %+v, %v", taken, err)
	}
	_ = keptAndOpened(t, taken)
	after, err := os.Stat(p.SessionCapture())
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("a take of the archive the script left packed it again (%v)", err)
	}

	if err := os.Remove(p.SessionCapture()); err != nil {
		t.Fatal(err)
	}
	digests := boxFile(t, box, p.SessionDigests())
	taken, err = runner.TakeSession(t.Context(), box, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("a take of an over job that left no archive = %+v, %v; want one packed now", taken, err)
	}
	if kept := keptAndOpened(t, taken); len(kept) == 0 {
		t.Fatal("the archive packed now holds nothing")
	}
	if taken.Archive.SessionID != handle.SessionID {
		t.Errorf("the archive packed now is of session %q, want %q", taken.Archive.SessionID, handle.SessionID)
	}
	if now := boxFile(t, box, p.SessionDigests()); len(now) <= len(digests) {
		t.Errorf("the pack made now kept no digest in the box: %q", now)
	}
}
