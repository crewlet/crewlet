package runner_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/tools"
)

// served is one scripted completion billed to the model that answered it.
func served(c llm.Completion, model string, in, out int) llm.Completion {
	c.Model, c.InputTokens, c.OutputTokens = model, in, out
	return c
}

// splitOf is a record's split as a comparable value.
func splitOf(rec *types.AgentPhaseCompleted) map[string][2]int {
	out := map[string][2]int{}
	for _, m := range rec.Models {
		out[m.Model] = [2]int{m.InputTokens, m.OutputTokens}
	}
	return out
}

// rollupOf folds records the way both spend readers hand them to the fold:
// the columns, the split and the flag.
func rollupOf(records ...*types.AgentPhaseCompleted) tokens.Rollup {
	fold := make([]tokens.Record, 0, len(records))
	for _, rec := range records {
		models := make([]tokens.ModelSpend, 0, len(rec.Models))
		for _, m := range rec.Models {
			models = append(models, tokens.ModelSpend{Model: m.Model, InputTokens: m.InputTokens,
				OutputTokens: m.OutputTokens, CostUSD: m.CostUSD})
		}
		fold = append(fold, tokens.Record{
			Phase: string(rec.Phase), TurnID: "t1", Model: rec.Model,
			InputTokens: rec.InputTokens, OutputTokens: rec.OutputTokens,
			TotalTokens: rec.TotalTokens, CostUSD: rec.CostUSD,
			Models: models, Unreported: rec.RunSpendUnreported,
		})
	}
	return tokens.Aggregate(fold, tokens.Options{})
}

// AN EXTENDED PHASE BILLS EACH ROUND TO THE MODEL THAT SERVED IT. The phase
// runs the loop again when it is granted more rounds, and the second
// invocation's split names only its own rounds: kept whole in its totals and
// not in its split, the phase's record billed every round before the
// extension to the model that answered after it. And the record names the
// model the PHASE began on.
func TestAnExtendedPhaseBillsEachInvocationToItsOwnModel(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	// Two rounds against the two-round cap on one model; the judge grants
	// more, and the chain has moved to another by the third.
	prov := &slowProvider{next: []llm.Completion{
		served(llm.Completion{ToolCalls: []llm.ToolCall{{ID: "a", Name: "read_file"}}}, "model-a", 100, 10),
		served(llm.Completion{ToolCalls: []llm.ToolCall{{ID: "b", Name: "read_file"}}}, "model-a", 100, 10),
		served(submitCall(t, runner.SubmitWorkTool, `{"outcome":"delivered","summary":"read both"}`),
			"model-b", 50, 5),
	}}
	r := extendableRunner(t, prov, pub, alwaysExtend{})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 3 || done.TotalTokens != 275 {
		t.Fatalf("the premise: %d rounds and %d tokens, want an extended phase of 3 and 275",
			done.RoundsUsed, done.TotalTokens)
	}
	want := map[string][2]int{"model-a": {200, 20}, "model-b": {50, 5}}
	if got := splitOf(done); len(got) != 2 || got["model-a"] != want["model-a"] || got["model-b"] != want["model-b"] {
		t.Errorf("models = %+v, want each invocation's rounds under the model that served them %v",
			done.Models, want)
	}
	if done.Model != "model-a" {
		t.Errorf("model = %q, want the model the phase's first completion named", done.Model)
	}
}

// A RESUMED PHASE BILLS THE ROUNDS BEFORE ITS SUSPENSION TO THEIR OWN MODEL.
// The pending-run row carries their split beside their tokens, so the phase's
// one record splits both halves — and a row a build that kept no split wrote
// resumes with those tokens unsplit, which the rollup counts under the
// record's model rather than dropping them.
func TestAResumedPhaseBillsThePreSuspendRoundsToTheirOwnModel(t *testing.T) {
	t.Parallel()
	finish := served(submitWork(t), "post-model", 30, 3)

	resume := func(t *testing.T, carried bool) *types.AgentPhaseCompleted {
		t.Helper()
		state := suspendedAfterTwoRounds()
		if carried {
			state.Model = "pre-model"
			state.Models = []types.ModelSpend{{Model: "pre-model", InputTokens: 400, OutputTokens: 100}}
		}
		pub := newCapture()
		r, _ := buildWith(t, []phase.Entry{{Key: "default",
			Provider: &scriptedProvider{execute: []llm.Completion{finish}}}}, buildOpts{
			pub:    pub,
			resume: &runner.Resume{State: state, Answer: "the run succeeded"},
		})
		if _, _, err := r.Resume(context.Background(), 1, nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return completedPhase(t, pub, "execute")
	}

	t.Run("a row that carries the split", func(t *testing.T) {
		t.Parallel()
		done := resume(t, true)
		got := splitOf(done)
		if len(got) != 2 || got["pre-model"] != [2]int{400, 100} || got["post-model"] != [2]int{30, 3} {
			t.Errorf("models = %+v, want the pre-suspend rounds under pre-model and the "+
				"re-entry under post-model", done.Models)
		}
		if done.Model != "pre-model" {
			t.Errorf("model = %q, want the model the phase began on before it suspended", done.Model)
		}
	})
	t.Run("a row from a build that kept no split", func(t *testing.T) {
		t.Parallel()
		done := resume(t, false)
		if got := splitOf(done); len(got) != 1 || got["post-model"] != [2]int{30, 3} {
			t.Errorf("models = %+v, want the re-entry's alone", done.Models)
		}
		rollup := rollupOf(done)
		if len(rollup.ByModel) != 1 || rollup.ByModel[0].TotalTokens != 533 {
			t.Errorf("by_model = %+v, want every token — the unsplit 500 under the record's model",
				rollup.ByModel)
		}
	})
}

// A SUSPENSION RECORDS WHICH MODEL SERVED ITS ROUNDS. The read above supplies
// its own split, so it cannot notice the suspension no longer writing one;
// this drives the suspension and reads the row the engine would persist.
func TestASuspensionRecordsItsModelSplit(t *testing.T) {
	t.Parallel()
	prov := &slowProvider{next: []llm.Completion{
		served(activate("run_sandbox"), "model-a", 70, 7),
		served(submitCall(t, "run_sandbox", `{"task":"fix the failing test"}`), "model-b", 20, 2),
	}}
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{})
	if err := reg.RegisterWith(suspendingTool{}, tools.Origin("sandbox"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if w, _, err := r.Execute(context.Background(), 1, "", nil); err != nil || !w.Suspended {
		t.Fatalf("Execute = %+v, %v; want a suspension", w, err)
	}
	parked, ok := r.Suspended()
	if !ok {
		t.Fatal("no suspension was recorded")
	}
	want := []types.ModelSpend{
		{Model: "model-a", InputTokens: 70, OutputTokens: 7},
		{Model: "model-b", InputTokens: 20, OutputTokens: 2},
	}
	if !slices.Equal(parked.State.Models, want) || parked.State.Model != "model-a" {
		t.Errorf("the parked row carries model %q and split %+v, want model-a and %+v",
			parked.State.Model, parked.State.Models, want)
	}
}

// A COLLECTED RUN'S OWN SPEND JOINS THE RECORD OF THE PHASE IT RESUMES, ONCE.
//
// The coding agent calls its models inside the box, so no round of this
// process billed them: the resumed phase's record is the one account of the
// run a rollup can read. Its tokens join the record's under the models the
// agent named, a run whose agent gave no whole account marks the record a
// floor, and the turn's totals — what its phase records sum to — take them in
// too. Like the rest of the carried spend, it is counted by the first record
// any attempt at the resume publishes and by no later one.
func TestACollectedRunsSpendJoinsTheResumedRecordOnce(t *testing.T) {
	t.Parallel()
	box := runner.RunRecord{
		CodingAgent: "claude-code", SandboxID: "box-1", CostUSD: 0.9,
		Spend: types.RunSpend{Collected: true, Models: []types.ModelSpend{
			{Model: "claude-sonnet", InputTokens: 600, OutputTokens: 60, CostUSD: 0.9},
		}},
	}
	state := func() runner.Resume {
		s := suspendedAfterTwoRounds()
		s.Model = "pre-model"
		s.Models = []types.ModelSpend{{Model: "pre-model", InputTokens: 400, OutputTokens: 100}}
		return runner.Resume{State: s, Answer: "the run succeeded", Run: box}
	}
	pub := newCapture()

	// The first attempt bills a round, then its provider fails.
	billed := served(thinkAndCall(t, "lookup_colleague", `{"query":"ana"}`, "who asked"), "post-model", 60, 40)
	first := state()
	r1, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &postsThenFails{post: billed}}},
		buildOpts{pub: pub, resume: &first})
	if _, _, err := r1.Resume(context.Background(), 1, nil); err == nil {
		t.Fatal("the first attempt's provider failed and the resume reported no error")
	}
	// The retry, told by the row that a record already counted the carried spend.
	finish := served(submitWork(t), "post-model", 30, 20)
	second := state()
	second.CarriedCounted = true
	r2, _ := buildWith(t, []phase.Entry{{Key: "default",
		Provider: &scriptedProvider{execute: []llm.Completion{finish}}}},
		buildOpts{pub: pub, resume: &second})
	if _, _, err := r2.Resume(context.Background(), 1, nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}

	records := phasesOfKind(t, pub, "execute")
	if len(records) != 2 {
		t.Fatalf("%d execute records, want one per attempt", len(records))
	}
	failed, retry := records[0], records[1]
	// 500 before the suspend, the run's 660, the attempt's own 100.
	if failed.TotalTokens != 1260 || !failed.RunSpendUnreported {
		t.Errorf("the first record counts %d tokens (unreported %v), want the carried 500, "+
			"the run's 660 and its own 100, marked a floor", failed.TotalTokens, failed.RunSpendUnreported)
	}
	if got := splitOf(failed); got["claude-sonnet"] != [2]int{600, 60} || got["pre-model"] != [2]int{400, 100} ||
		got["post-model"] != [2]int{60, 40} {
		t.Errorf("the first record's split = %+v, want the run's model beside the phase's own", failed.Models)
	}
	if retry.TotalTokens != 50 || retry.RunSpendUnreported || retry.CostUSD != 0 {
		t.Errorf("the retry counts %d tokens (unreported %v, $%v), want its own 50 and none of the "+
			"carried spend", retry.TotalTokens, retry.RunSpendUnreported, retry.CostUSD)
	}
	if got := splitOf(retry); len(got) != 1 || got["post-model"] != [2]int{30, 20} {
		t.Errorf("the retry's split = %+v, want its own round alone", retry.Models)
	}
	rollup := rollupOf(records...)
	if rollup.Totals.TotalTokens != 1310 || rollup.Totals.UnreportedCalls != 1 || rollup.Totals.CostUSD != 0.9 {
		t.Errorf("the attempts roll up to %+v, want 1310 tokens, the run's $0.90 once and one floor",
			rollup.Totals)
	}
	// THE TURN'S TOTALS ARE WHAT ITS RECORDS SUM TO.
	if got := r1.Spend().Total(); got != failed.TotalTokens {
		t.Errorf("the first attempt's turn totals %d, its record %d", got, failed.TotalTokens)
	}
	if got := r2.Spend().Total(); got != retry.TotalTokens {
		t.Errorf("the retry's turn totals %d, its record %d", got, retry.TotalTokens)
	}
}
