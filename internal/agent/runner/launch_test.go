package runner_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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

func (l *playLauncher) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) error {
	time.Sleep(l.took)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, req)
	return l.err
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
	return submitCall(t, name, argsJSON).ToolCalls[0]
}

// play performs r against the surface a launch was handed, call by call
// through [tools.Surface.Execute] — the frame the bridge dispatches every
// bridged call through — and returns what the bridge's durable log holds for
// it: each call's name, arguments, answer and whether it failed, and the
// bridge's measurement of it — when it started, how long it took and who
// answered.
func play(t *testing.T, req runner.AgentRunRequest, r run) []toolloop.Execution {
	t.Helper()
	out := make([]toolloop.Execution, 0, len(r.calls))
	for _, c := range r.calls {
		began := time.Now()
		res, err := req.Surface.Execute(context.Background(), c)
		if err != nil {
			t.Fatalf("bridged %s: %v", c.Name, err)
		}
		out = append(out, toolloop.Execution{
			Name: c.Name, Args: c.Arguments, Output: res.Output, Failed: res.Failed,
			StartedAt: began.UTC(), Duration: time.Since(began),
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
	history []ledger.Iteration, bridged []toolloop.Execution, text string, record runner.RunRecord,
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

// A REBUILT BRIDGE SURFACE COUNTS THE RUN'S CALLS BEFORE IT, AND OFFERS WHAT
// THE RUN HAD ACTIVE. A node that lost a running job's bridge session rebuilds
// it from the park, and the box's next write derives its operation id from how
// many calls to that tool came before — so a count restarted at the rebuild
// would hand a new write an earlier one's id, answered as a retry of work
// already done. And a tool the run activated before the restart is one it
// goes on calling.
//
// Mutation: drop the seeding and the post after the rebuild counts none
// before it; rebuild on the executor's plan rather than the park's active set
// and the activated tool is gone.
func TestARebuiltBridgeSurfaceCarriesOnFromTheRunsCalls(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{
		activate("slack_post"),
		call(t, "slack_post", `{"text":"first"}`),
	}})
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	// The park as a restart reads it: off the row, after the activation.
	parked.State.ActiveTools = req.Surface.Active()

	// A FRESH LOG, as the engine hands every turn it rebuilds one: what it
	// holds is what the rebuild seeds.
	rebuilder, _ := buildWith(t, entries, buildOpts{
		calls:  turnctx.NewCallLog(),
		resume: &runner.Resume{State: parked.State, Bridged: bridged},
	})
	surface, meter, err := rebuilder.BridgeSurface(context.Background())
	if err != nil || surface == nil || meter == nil {
		t.Fatalf("BridgeSurface = %v, %v, %v", surface, meter, err)
	}
	if !slices.Contains(surface.Active(), "slack_post") {
		t.Errorf("the rebuilt surface offers %v, want the tool the run had activated", surface.Active())
	}
	if !slices.Contains(surface.Active(), runner.SubmitWorkTool) {
		t.Errorf("the rebuilt surface offers %v, want the submission", surface.Active())
	}
	if got := surface.Turn().CallLog().Ordinal(context.Background(), "slack_post",
		map[string]any{"text": "second"}); got != 1 {
		t.Errorf("a post after the rebuild counts %d earlier posts, want the run's one", got)
	}
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
	if got := again.Surface.Turn().CallLog().Ordinal(context.Background(), "slack_post",
		map[string]any{"text": "the fix is on main"}); got != 1 {
		t.Errorf("a post after the answer counts %d earlier posts, want the one before the question", got)
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
			text := tc.asked.Reseed()
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
