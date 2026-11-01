package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// playLauncher is the engine's launch seam with no box behind it: it records
// every request it is handed — the prompt, the surface, the bridged-spend
// meter — and starts nothing. What a run then DOES is the test's to say, with
// [play], against the surface the launch was handed.
type playLauncher struct {
	mu   sync.Mutex
	reqs []runner.AgentRunRequest
	err  error
	// took is how long each launch takes before it returns, as a real one
	// provisions a box and starts its job.
	took time.Duration
}

func (l *playLauncher) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	time.Sleep(l.took)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, req)
	if l.err != nil {
		return runner.Launched{}, l.err
	}
	// EACH LAUNCH A JOB OF ITS OWN, named as a store names one.
	return runner.Launched{LaunchID: fmt.Sprintf("job-%d", len(l.reqs))}, nil
}

// launched is how many launches the runner asked for, refused ones included.
func (l *playLauncher) launched() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

// last is the most recent launch request; it fails the test when there is none.
func (l *playLauncher) last(t *testing.T) runner.AgentRunRequest {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		t.Fatal("no run was launched")
	}
	return l.reqs[len(l.reqs)-1]
}

// run is what a fake executor run does: the calls its CLI makes over the
// bridge, in order, and the closing text it ends on.
type run struct {
	calls []llm.ToolCall
	text  string
}

// call is one bridged call, for a [run]'s script.
func call(t *testing.T, name, argsJSON string) llm.ToolCall {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return llm.ToolCall{Name: name, Arguments: args}
}

// play performs r against the surface a launch was handed, call by call as
// the bridge dispatches every bridged call — admitted to its place in the
// run's call log ([tools.Surface.Admit]), then run — and returns what the
// bridge's durable log holds for it: each call's name, arguments, answer and
// whether it failed, its place in the order the bridge admitted the run's
// calls, and the bridge's measurement of it — when it started, how long it
// took and who answered.
func play(t *testing.T, req runner.AgentRunRequest, r run) []runner.BridgedCall {
	t.Helper()
	out := make([]runner.BridgedCall, 0, len(r.calls))
	for i, c := range r.calls {
		began := time.Now()
		res, err := req.Surface.Admit(c.Name, c.Arguments)(context.Background(), c)
		if err != nil {
			t.Fatalf("bridged %s: %v", c.Name, err)
		}
		out = append(out, runner.BridgedCall{
			Call: ledger.Call{Name: c.Name, Args: c.Arguments, Result: res.Output, Failed: res.Failed},
			Seq:  i + 1, StartedAt: began.UTC(), Duration: time.Since(began),
			Origin: res.Origin, Server: res.Server,
		})
	}
	return out
}

// collected is one executor round, launched and collected.
type collected struct {
	work    turn.Work
	surface turn.Surface
	// launch is what the run was launched with; collector the runner that
	// collected it, which a case drives its reviewer on as the engine does.
	launch    runner.AgentRunRequest
	collector *runner.Runner
}

// launch builds a runner on entries and opts, runs its Execute, and returns it
// with the request its run was launched with — for the cases about what a run
// is handed, and what its surface does with the calls it makes.
func launch(t *testing.T, entries []phase.Entry, opts buildOpts, round int, notes string,
	history []ledger.Iteration,
) (*runner.Runner, runner.AgentRunRequest) {
	t.Helper()
	launcher := &playLauncher{}
	opts.launcher = launcher
	r, _ := buildWith(t, entries, opts)
	w, _, err := r.Execute(context.Background(), round, notes, history)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !w.Suspended {
		t.Fatal("the executor did not park on its run")
	}
	return r, launcher.last(t)
}

// collect resumes the turn a launched runner parked, as the process that
// collects the run does: a FRESH runner, built from the parked state, the
// bridge's log of what the run called, the run's closing text, and record as
// the run's own account of where it ran.
func collect(t *testing.T, entries []phase.Entry, opts buildOpts, launched *runner.Runner,
	history []ledger.Iteration, bridged []runner.BridgedCall, text string, record runner.RunRecord,
) (*runner.Runner, turn.Work, turn.Surface) {
	t.Helper()
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state, so nothing could collect the run")
	}
	if opts.launcher == nil {
		opts.launcher = &playLauncher{}
	}
	// THE PARK'S CONTEXT, not the fixture's: a collecting runner is handed
	// none, as the engine's is, and builds its prompts from what the turn
	// froze at its start.
	opts.context, opts.conversation = prefetch.Blocks{}, ""
	opts.resume = &runner.Resume{State: parked.State, Answer: text, Bridged: bridged, Run: record}
	collector, _ := buildWith(t, entries, opts)
	work, surface, err := collector.Resume(context.Background(), history)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	return collector, work, surface
}

// executeRound runs one executor round the way the engine runs it across two
// segments of a turn: Execute launches the run and parks; the run plays r
// against the surface it was launched with; and a fresh runner collects it
// ([collect]).
func executeRound(t *testing.T, entries []phase.Entry, opts buildOpts, round int, notes string,
	history []ledger.Iteration, r run, record runner.RunRecord,
) collected {
	t.Helper()
	launching, req := launch(t, entries, opts, round, notes, history)
	bridged := play(t, req, r)
	opts.launcher = nil
	collector, work, surface := collect(t, entries, opts, launching, history, bridged, r.text, record)
	return collected{work: work, surface: surface, launch: req, collector: collector}
}

// A PERSON'S ANSWER CARRIES THE RUN ON; IT IS NEVER THE RUN'S REPORT. A run
// asks only when it cannot go on without the answer, so the round it asked in
// is not over. Filed as the run's report, the answer reached the turn as a run
// that ended without submitting — rescued as incomplete, its delivery judged
// on a reply rather than on any work — and the run that asked never saw it. So
// the resume relaunches the executor in the same round: the asking run's
// session continued with the answer, its brief for a fresh run carrying the
// question and the answer, and the turn parked on it again.
//
// Mutation: collect the answer as resumeAgentRun did and the work is the
// rescued report; drop the continuation and the run starts a conversation that
// knows nothing.
func TestAnAnswerCarriesTheRunOnRatherThanReportingIt(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{activate("slack_post"),
		call(t, "slack_post", `{"text":"looking into it"}`)}})
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	parked.State.ActiveTools = req.Surface.Active()

	relauncher := &playLauncher{}
	asked := &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
		Branch: "wip/t1", SessionID: "sess-ask", BoxKept: true}
	collector, _ := buildWith(t, entries, buildOpts{
		calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Asked: asked, Bridged: before},
	})
	work, _, err := collector.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !work.Suspended || work.Rescued || strings.Contains(work.Text, "use main") {
		t.Fatalf("work = %+v, want the turn parked on the carried-on run and nothing filed "+
			"as its report", work)
	}
	again := relauncher.last(t)
	if again.Continue == nil || again.Continue.SessionID != "sess-ask" ||
		!strings.Contains(again.Continue.Brief, "use main") {
		t.Fatalf("the relaunch continues %+v, want the asking run's session with the answer",
			again.Continue)
	}
	for _, want := range []string{"which branch?", "use main", "wip/t1"} {
		if !strings.Contains(again.Brief, want) {
			t.Errorf("a fresh run's brief does not carry %q:\n%s", want, again.Brief)
		}
	}
	if again.Round != parked.State.Round {
		t.Errorf("the relaunch is round %d, want the round it carries on, %d", again.Round, parked.State.Round)
	}
	if !slices.Contains(again.Surface.Active(), "slack_post") {
		t.Errorf("the carried-on run is offered %v, want the tool it had active", again.Surface.Active())
	}
	if got := again.Surface.Turn().CallLog().Ordinal("slack_post",
		map[string]any{"text": "the fix is on main"}); got != 1 {
		t.Errorf("a post after the answer counts %d earlier posts, want the one before the question", got)
	}
}

// A CARRIED-ON RUN IS SERVED THE SURFACE THE ASKING RUN LEFT, and may cite
// what it delivered. The run that asked activated tools and made deliveries
// over the bridge after its launch recorded the park, and the run carrying it
// on — the same round, often the same CLI session — goes on calling those
// tools and reports those deliveries. Rebuilt from the launch's record alone,
// a tool it activated was refused as inactive; judged against the relaunch's
// own calls alone, its submission citing a delivery made before the question
// was refused as it was made, though the collection accepts it.
//
// Mutation: rebuild the relaunch's surface from the park's active list alone,
// or leave the asking run's calls out of the submission's record, and this
// goes red.
func TestACarriedOnRunKeepsWhatTheAskingRunDid(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{activate("slack_post"),
		call(t, "slack_post", `{"text":"posted the summary"}`)}})
	// THE PARK AS THE LAUNCH WROTE IT: before the run activated anything.
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	if slices.Contains(parked.State.ActiveTools, "slack_post") {
		t.Fatal("the park already holds the activation, so this case proves nothing")
	}

	relauncher := &playLauncher{}
	collector, _ := buildWith(t, entries, buildOpts{
		calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Bridged: before,
			Asked: &runner.Asked{Question: "which channel?", Answer: "#eng", BoxKept: true,
				SessionID: "sess-ask"}},
	})
	if _, _, err := collector.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if !slices.Contains(again.Surface.Active(), "slack_post") {
		t.Errorf("the carried-on run is offered %v, want the tool the asking run activated",
			again.Surface.Active())
	}
	cited := play(t, again, run{calls: []llm.ToolCall{call(t, runner.SubmitWorkTool,
		`{"outcome":"delivered","summary":"posted to #eng","deliveries":["slack_post"]}`)}})
	if cited[0].Failed {
		t.Errorf("a submission citing the asking run's delivery was refused: %s", cited[0].Result)
	}
}

// A FRESH RUN IS NEVER PROMISED A WORKING TREE THAT IS GONE. Where the asking
// run's box was reclaimed while its question waited, the run carrying it on is
// on a fresh machine and must be told so, and pointed at the branch — git is
// the durable state — or it starts by looking for files that are not there.
func TestAFreshRunIsToldWhereTheWorkIs(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		asked     runner.Asked
		want, not []string
	}{
		"its box was reclaimed, and it pushed a branch": {
			runner.Asked{Question: "q", Answer: "a", Branch: "wip/t1"},
			[]string{"FRESH machine", "the existing branch `wip/t1`"}, nil,
		},
		"its box was reclaimed, and it named no branch": {
			runner.Asked{Question: "q", Answer: "a"},
			[]string{"FRESH machine", "work-in-progress branch it pushed earlier"}, nil,
		},
		"its box is kept": {
			runner.Asked{Question: "q", Answer: "a", Branch: "wip/t1", BoxKept: true},
			[]string{"`wip/t1`"}, []string{"FRESH machine"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			text := tc.asked.Reseed(provenance.NewBoundary())
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the brief does not say %q:\n%s", want, text)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(text, not) {
					t.Errorf("the brief says %q:\n%s", not, text)
				}
			}
		})
	}
}
