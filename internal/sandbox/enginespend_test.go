package sandbox

import (
	"testing"
)

// bridgedSpend is a bridge session's running total, every figure distinct.
var bridgedSpend = EngineSpend{
	Aux:     AuxTokens{Input: 1200, Output: 90, CacheRead: 400, CacheWrite: 30},
	Workers: 2, WorkerInput: 6000, WorkerOutput: 500,
}

// condensing is what one rewrite of a run's account costs, in the fake.
var condensing = AuxTokens{Input: 3000, Output: 250, CacheRead: 1000}

// condensingRig is a coordinator rig whose seat's auxiliary model rewrites
// whatever it is handed, at a cost, and a job whose bridged calls already cost
// the engine [bridgedSpend].
func condensingRig(t *testing.T, turnID string) *coordRig {
	t.Helper()
	rig := newCoordRig(t)
	rig.coordinator.condense = &fakeCondenser{cost: condensing,
		answer: func(part RunPart, _ string, _ int) (string, error) {
			return "(condensed) the " + string(part), nil
		}}
	rig.launch(turnID)
	rig.coordinator.countRun("swe", StatusRunning)
	if launch, err := rig.pending.AppendBridgeCall(t.Context(), turnID, BridgeAppend{
		Call: BridgeCall{Name: "query_episodes"}, Spent: bridgedSpend,
	}); err != nil || launch == "" {
		t.Fatalf("AppendBridgeCall = %q, %v", launch, err)
	}
	return rig
}

// WHAT THE ENGINE SPENT ON A JOB BETWEEN SEGMENTS REACHES THE RESUME THAT PAYS
// FOR IT.
//
// Two things the engine pays for happen while no segment of the turn is
// running: the calls an agent-mode run makes through the tool bridge, after the
// segment that launched it ended, and the condensation of a collected run's
// report, which the coordinator makes before any segment resumes. Neither has
// a segment's tally to add to, so both ride the resume request to the segment
// that resumes from the job — its charge to the turn's work item is the only
// one that can include them (ADR-0022).
//
// Mutation: drop the bridged half or the condensation from the request, and
// the resume carries less than the job cost.
func TestACompletionsResumeCarriesWhatTheEngineSpentOnTheJob(t *testing.T) {
	rig := condensingRig(t, "t1")
	rig.runner.Finish(Result{Success: true, Text: lines("report", 7000),
		InputTokens: 900, OutputTokens: 200})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want 1", len(calls))
	}
	want := bridgedSpend.Plus(EngineSpend{Aux: condensing})
	if calls[0].Engine != want {
		t.Fatalf("the resume carries engine spend %+v, want the bridged calls' and the "+
			"report's condensation together, %+v", calls[0].Engine, want)
	}
	if calls[0].InputTokens != 900 || calls[0].OutputTokens != 200 {
		t.Fatalf("the job's own tokens moved: %d/%d", calls[0].InputTokens, calls[0].OutputTokens)
	}
}

// A PARKED JOB'S ENGINE SPEND IS PAID BY THE ANSWER'S RESUME.
//
// The completion that parks resumes nothing, and the answer's resume — days
// later, perhaps on another node — collects nothing. So the condensation of the
// collection that parked (here the question itself, past what a question
// carries) is written with the question, and the answer's resume reads it, with
// the job's bridged spend, off the job's own record.
//
// Mutation: leave the condensation off the park, or the bridged half off the
// answer's request, and the answer's resume carries less than the job cost.
func TestAParkedJobsEngineSpendReachesTheAnswersResume(t *testing.T) {
	rig := condensingRig(t, "asks")
	rig.runner.Finish(Result{NeedsInput: true, AskTo: "requester",
		Question: lines("which branch", 1000), InputTokens: 700, OutputTokens: 80})

	payload, ev := rig.completion("asks")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 0 {
		t.Fatalf("a job that asked a question resumed %d times before any answer", got)
	}
	if got := rig.get("asks").LaunchFacts().Condensed; got != condensing {
		t.Fatalf("the parked job's record holds condensation %+v, want %+v", got, condensing)
	}

	disposition, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", nil)
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("TryResumeFromAnswer = %q, %v", disposition, err)
	}
	calls := rig.resumer.calls()
	want := bridgedSpend.Plus(EngineSpend{Aux: condensing})
	if len(calls) != 1 || calls[0].Engine != want {
		t.Fatalf("the answer's resume carries %+v, want engine spend %+v", calls, want)
	}
}

// A FAILED REWRITE WAS STILL PAID FOR. A model whose rewrite came back past the
// bound, or that answered nothing usable, spent its tokens all the same, so
// what the condenser reports it cost is counted whether or not its answer is
// used — and a runner's own value for the field is never trusted, since the
// field is the coordinator's.
func TestAFailedCondensationIsStillCounted(t *testing.T) {
	t.Parallel()
	c := &Coordinator{condense: &fakeCondenser{cost: condensing,
		answer: func(RunPart, string, int) (string, error) {
			return lines("still too long", 9000), nil
		}}}
	got := c.fitResult(t.Context(), PendingRun{AgentHandle: "dev", TurnID: "t1"}, Result{
		Text: lines("report", 7000), Error: lines("stderr", 7000),
		Condensed: AuxTokens{Input: 99999},
	})
	if want := condensing.Plus(condensing); got.Condensed != want {
		t.Fatalf("two rewrites that came back too long count %+v, want both paid for, %+v",
			got.Condensed, want)
	}
}
