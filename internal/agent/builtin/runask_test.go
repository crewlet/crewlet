package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// askDesk is the run record `ask_and_wait` writes to: what it was asked to
// record, under which run and job, and the outcome it answers with.
type askDesk struct {
	outcome  sandbox.AskOutcome
	err      error
	recorded []sandbox.Ask
	runs     []string
	launches []string
}

func (d *askDesk) RecordAsk(_ context.Context, turnID, launch string, ask sandbox.Ask,
) (sandbox.AskOutcome, error) {
	d.recorded = append(d.recorded, ask)
	d.runs = append(d.runs, turnID)
	d.launches = append(d.launches, launch)
	if d.err != nil {
		return "", d.err
	}
	return d.outcome, nil
}

// Get reads no row: the desk records asks and holds none of the run.
func (d *askDesk) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, d.err
}

// askCall calls `ask_and_wait` as the job "launch-1" of the run "run-1" makes
// it over its bridge.
func askCall(t *testing.T, desk *askDesk, args map[string]any) tools.Result {
	t.Helper()
	tool := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: desk,
		Work: builtin.WorkDeps{Reader: newFakeTracker()}, Pages: newFakeKB()})
	ctx := turnctx.WithJob(t.Context(), "launch-1")
	got, err := tool.CallForTurn(ctx, &turnctx.Turn{RunID: "run-1"}, args)
	if err != nil {
		t.Fatalf("ask_and_wait: %v", err)
	}
	return got
}

// askArgs is a well-formed structured ask, as a model sends it: the owner's
// GP-71 question, with its options as options.
func askArgs() map[string]any {
	return map[string]any{
		"question": "How should the sanitizer test treat style attributes?",
		"context":  "The **sanitizer** rewrites `style` attributes. WIP is pushed on `wip/gp-71`.",
		"to":       "team",
		"branch":   "wip/gp-71",
		"options": []any{
			map[string]any{"id": "expected", "label": "Keep EXPECTED_DIFFERENCES",
				"detail": "Document the rewrite as expected."},
			map[string]any{"id": "exact", "label": "Keep style attributes byte for byte"},
		},
		"recommended": "expected",
		"rationale":   "The rewrite is harmless and already reviewed.",
		"role":        "approver",
		"evidence": []any{
			map[string]any{"kind": "task", "ref": "ENG-2", "label": "the audit"},
			map[string]any{"kind": "url", "ref": "https://git.example.com/acme/web/pull/71"},
		},
	}
}

// A RUN ASKS A PERSON IN THE SHAPE A DECISION IS ASKED, and parks on it. The
// ask is recorded on the job the call was made from, with its question as the
// decision's own, its options, its recommendation and its evidence resolved
// as a work item's decision is (a task to its id); and the run is told to end
// its turn, because its answer resumes it.
//
// Mutation: record the ask under the run alone, without its job, and the case
// fails on the launch.
func TestARunsAskIsRecordedAsADecisionOnItsJob(t *testing.T) {
	t.Parallel()
	desk := &askDesk{outcome: sandbox.AskRecorded}
	got := askCall(t, desk, askArgs())
	if got.Failed {
		t.Fatalf("ask_and_wait refused a well-formed ask: %s", got.Output)
	}
	if !strings.Contains(got.Output, "END YOUR TURN") {
		t.Errorf("the run is not told to end its turn: %s", got.Output)
	}
	if len(desk.recorded) != 1 || desk.runs[0] != "run-1" || desk.launches[0] != "launch-1" {
		t.Fatalf("recorded %d asks under %v / %v, want one under run-1 / launch-1",
			len(desk.recorded), desk.runs, desk.launches)
	}
	ask := desk.recorded[0]
	if ask.To != "team" || ask.Branch != "wip/gp-71" || !strings.Contains(ask.Context, "**sanitizer**") {
		t.Errorf("the ask is %+v", ask)
	}
	d := ask.Decision
	if d == nil || d.Question != ask.Question || len(d.Options) != 2 ||
		d.Recommended != "expected" || d.Role != decision.RoleApprover {
		t.Fatalf("the decision is %+v, want the question's own with its options", d)
	}
	if d.Evidence[0].Ref != "id-2" || d.Evidence[1].Kind != decision.EvidenceURL {
		t.Errorf("the evidence is %+v, want the task resolved to its id", d.Evidence)
	}
}

// AN OPEN QUESTION IS AN ASK TOO, answered in words: a question and its
// context, with no decision at all.
func TestAnOpenQuestionIsAskedWithoutADecision(t *testing.T) {
	t.Parallel()
	desk := &askDesk{outcome: sandbox.AskRecorded}
	got := askCall(t, desk, map[string]any{
		"question": "Which staging account should the migration run against?",
		"context":  "Both `stg-a` and `stg-b` hold a copy.", "to": "requester",
	})
	if got.Failed || len(desk.recorded) != 1 || desk.recorded[0].Decision != nil {
		t.Fatalf("ask_and_wait = %+v, recorded %+v; want an open question", got, desk.recorded)
	}
}

// EVERY FAULT OF AN ASK COMES BACK AT ONCE (ADR-0059), and nothing is recorded:
// a run asks again once it is refused, and a refusal naming one fault of
// several spends that attempt on the next. The parts of a choice need its
// options; an `inform` is no argument of a run's ask; a key the schema does not
// know is refused by name rather than dropped.
//
// Mutation: return at the first fault, and only the question is named.
func TestEveryFaultOfARunsAskIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	desk := &askDesk{outcome: sandbox.AskRecorded}
	got := askCall(t, desk, map[string]any{
		"question": strings.Repeat("q", decision.MaxQuestion+1), "to": "",
		"branch": "has space", "recommended": "a", "rationale": "because",
	})
	if !got.Failed {
		t.Fatalf("a malformed ask was recorded: %s", got.Output)
	}
	for _, want := range []string{"question is", "to is empty", "branch is",
		"recommended is set", "rationale is set"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, got.Output)
		}
	}
	for name, args := range map[string]map[string]any{
		"an inform": func() map[string]any {
			a := askArgs()
			a["inform"] = map[string]any{"surface": "slack", "channel": "eng"}
			return a
		}(),
		"a misspelled key": func() map[string]any {
			a := askArgs()
			a["recommendation"] = a["recommended"]
			return a
		}(),
		"one option": func() map[string]any {
			a := askArgs()
			a["options"] = a["options"].([]any)[:1]
			return a
		}(),
	} {
		if got := askCall(t, desk, args); !got.Failed {
			t.Errorf("an ask with %s was recorded: %s", name, got.Output)
		}
	}
	if len(desk.recorded) != 0 {
		t.Fatalf("%d refused asks were recorded", len(desk.recorded))
	}
}

// WHAT THE RECORD ANSWERS IS WHAT THE RUN IS TOLD: a second question in one job
// stands behind the first, a job that is over parks nothing, a store that
// failed asked nothing, and a call no run's job made is no ask at all.
func TestARunIsToldWhatBecameOfItsAsk(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		desk  *askDesk
		want  string
		class tools.Refusal
	}{
		// NOT A CONFLICT: a conflict tells the run to try again, and a
		// second question is refused for as long as the first stands.
		{"already asked", &askDesk{outcome: sandbox.AskAlreadyAsked}, "already asked",
			tools.RefusalNotRunning},
		{"not running", &askDesk{outcome: sandbox.AskNotRunning}, "no longer running",
			tools.RefusalNotRunning},
		{"a failed store", &askDesk{err: errors.New("coord: unreachable")}, "Nothing was asked",
			tools.RefusalInternalError},
	} {
		got := askCall(t, tc.desk, askArgs())
		if !got.Failed || !strings.Contains(got.Output, tc.want) || tools.RefusalOf(got) != tc.class {
			t.Errorf("%s: ask_and_wait = %+v (%s), want a %s refusal saying %q", tc.name, got,
				tools.RefusalOf(got), tc.class, tc.want)
		}
	}
	tool := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: &askDesk{outcome: sandbox.AskRecorded}})
	got, err := tool.CallForTurn(t.Context(), &turnctx.Turn{RunID: "run-1"}, askArgs())
	if err != nil || !got.Failed || !strings.Contains(got.Output, "was not made by one") {
		t.Errorf("a call with no job = %+v, %v; want it refused as no run's", got, err)
	}
}

// AN ASK THAT DID NOT LAND IS NEVER A SILENT STOP. Against the run record
// itself: the job the call was made from has been replaced (the compare-and-
// set the record writes by finds another job), so nothing is recorded, the run
// is told in words that nothing waits on its question, and the call fails,
// which the bridge records as failed and wakes no waiter over; the job that
// does hold the row records its question and is told to end its turn. And a
// record that cannot be written at all is said as such: nothing was asked, ask
// again.
//
// Mutation: answer AskNotRunning as recorded, and the replaced job is told to
// stop on a question nobody was asked.
func TestAnAskThatDidNotLandIsSaidToTheRun(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	run, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: "run-1",
		AgentHandle: "swe", Status: sandbox.StatusLaunching}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	tool := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: store})
	call := func(job string) tools.Result {
		got, err := tool.CallForTurn(turnctx.WithJob(t.Context(), job),
			&turnctx.Turn{RunID: "run-1"}, map[string]any{
				"question": "Which branch should I target?", "to": "requester"})
		if err != nil {
			t.Fatalf("ask_and_wait: %v", err)
		}
		return got
	}

	stale := call("an-earlier-job")
	if !stale.Failed || tools.RefusalOf(stale) != tools.RefusalNotRunning ||
		!strings.Contains(stale.Output, "no longer running") {
		t.Fatalf("an ask from a replaced job answered %+v; want a not-running refusal in words", stale)
	}
	if got, _, _ := store.Get(t.Context(), "run-1"); got.Question != "" {
		t.Fatalf("a replaced job's ask was recorded on the row: %q", got.Question)
	}

	held := call(run.LaunchID)
	if held.Failed || !strings.Contains(held.Output, "END YOUR TURN") {
		t.Fatalf("the job holding the row answered %+v; want its question recorded", held)
	}
	if got, _, _ := store.Get(t.Context(), "run-1"); got.Question != "Which branch should I target?" {
		t.Fatalf("the row holds %q, want the job's question", got.Question)
	}

	down := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: &askDesk{err: errors.New("coord: unreachable")}})
	got, err := down.CallForTurn(turnctx.WithJob(t.Context(), "l"), &turnctx.Turn{RunID: "run-1"},
		map[string]any{"question": "Which branch?", "to": "requester"})
	if err != nil || !got.Failed || !strings.Contains(got.Output, "Nothing was asked: call it again") {
		t.Fatalf("an unreachable record answered %+v, %v; want the run told to ask again", got, err)
	}
}

// A RUN'S ASK IS REFUSED IN ITS OWN ARGUMENTS' NAMES, EVERY FAULT IN ONE PASS.
// The choice's fields sit at the top of the run's arguments, so a fault is
// named `options[0].label` or `recommended`, never under a `decision.` the run
// never sent; and evidence that names nothing is checked in the same pass as
// the shape, so a run that fixes what it was told is not refused again for
// what it was not (ADR-0059, ADR-0068).
//
// Mutation: check the evidence only once the shape passes, or name the
// fields under `decision.`, and this fails.
func TestARunsAskIsRefusedInItsOwnNamesInOnePass(t *testing.T) {
	t.Parallel()
	args := askArgs()
	args["options"].([]any)[0].(map[string]any)["label"] = ""
	args["recommended"] = "neither"
	args["evidence"] = []any{map[string]any{"kind": "task", "ref": "ENG-404"}}
	desk := &askDesk{outcome: sandbox.AskRecorded}
	got := askCall(t, desk, args)
	if !got.Failed || len(desk.recorded) != 0 {
		t.Fatalf("a malformed ask was recorded: %+v", got)
	}
	for _, want := range []string{"options[0].label is empty", `recommended is "neither"`,
		`evidence[0].ref names the work item "ENG-404"`} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got.Output)
		}
	}
	if strings.Contains(got.Output, "decision.") {
		t.Errorf("the refusal names a field the run never sent:\n%s", got.Output)
	}
}

// gatedTool is a submission that records whether it was called.
type gatedTool struct{ called int }

func (g *gatedTool) Name() string               { return "submit_work" }
func (g *gatedTool) Description() string        { return "Submit your work." }
func (g *gatedTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (g *gatedTool) Call(context.Context, map[string]any) (tools.Result, error) {
	g.called++
	return tools.Result{Output: "submitted"}, nil
}

// A JOB THAT ASKED IS PARKED, AND ITS RUN IS TOLD SO IF IT SUBMITS. Once the
// job's question is on its row, a `submit_work` it makes is refused with the
// instruction to end its turn and never reaches the submission: accepted, the
// CLI read it as delivered and ended believing so while the turn waited on a
// person (ADR-0068). A job that has not asked, and a call no job makes, submit
// as before; a row that cannot be read refuses the call as the node's fault,
// so the run calls again.
//
// Mutation: drop the gate's row read, and the asking job's submission lands.
func TestAJobThatAskedCannotSubmit(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	run, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: "run-1",
		AgentHandle: "swe", Status: sandbox.StatusLaunching}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	ask := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: store})
	inner := &gatedTool{}
	submit := ask.Gate(inner)
	if submit.Name() != "submit_work" {
		t.Fatalf("the gated submission is named %q", submit.Name())
	}
	turn := &turnctx.Turn{RunID: "run-1"}
	job := turnctx.WithJob(t.Context(), run.LaunchID)
	call := func(ctx context.Context, turn *turnctx.Turn) tools.Result {
		t.Helper()
		got, err := submit.(tools.SeatCallable).CallForTurn(ctx, turn, map[string]any{})
		if err != nil {
			t.Fatalf("submit_work: %v", err)
		}
		return got
	}

	if got := call(job, turn); got.Failed || inner.called != 1 {
		t.Fatalf("a job that has not asked submitted %+v (called %d); want it through", got, inner.called)
	}
	if got, err := ask.CallForTurn(job, turn, map[string]any{
		"question": "Which branch should I target?", "to": "requester"}); err != nil || got.Failed {
		t.Fatalf("ask_and_wait: %+v, %v", got, err)
	}
	parked := call(job, turn)
	if !parked.Failed || tools.RefusalOf(parked) != tools.RefusalNotRunning ||
		!strings.Contains(parked.Output, "END YOUR TURN") || inner.called != 1 {
		t.Fatalf("the asking job's submission answered %+v (called %d); want a not-running "+
			"refusal telling it to end its turn", parked, inner.called)
	}
	if got := call(turnctx.WithJob(t.Context(), "an-earlier-job"), turn); got.Failed || inner.called != 2 {
		t.Fatalf("a job the row no longer holds was refused for the next job's question: %+v", got)
	}
	if got := call(t.Context(), nil); got.Failed || inner.called != 3 {
		t.Fatalf("a call no run made was refused: %+v", got)
	}

	down := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: &askDesk{err: errors.New("coord: unreachable")}})
	got, err := down.Gate(inner).(tools.SeatCallable).CallForTurn(job, turn, map[string]any{})
	if err != nil || !got.Failed || !strings.Contains(got.Output, "Nothing was submitted") || inner.called != 3 {
		t.Fatalf("an unreadable row answered %+v, %v; want the run told to submit again", got, err)
	}
}
