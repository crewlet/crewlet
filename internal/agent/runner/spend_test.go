package runner_test

import (
	"context"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
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
	if !reflect.DeepEqual(parked.State.Models, want) || parked.State.Model != "model-a" {
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

// A RUN COLLECTED BEFORE THE PHASE LAUNCHES AGAIN REACHES THE RECORD THAT
// FINALLY PUBLISHES, ONCE.
//
// A resumed phase that calls run_sandbox a second time — how it continues
// after a person answers a question a run stopped to ask — suspends again, and
// a suspending phase publishes no record. The run it re-entered from rides on
// the new suspension, through the wire the pending-run row holds, into the
// resume after it, whose record counts both runs' price, spend and refs beside
// every round the phase ran. The carried spend is still counted by the first
// record any attempt at a resume publishes and by no later one, and a
// suspension written by an attempt whose spend an earlier record counted
// carries none of it forward.
func TestARunCollectedBeforeARelaunchReachesTheFinalRecordOnce(t *testing.T) {
	t.Parallel()
	const (
		pr1 = "https://github.com/acme/app/pull/1"
		pr2 = "https://github.com/acme/app/pull/2"
	)
	// The first run's agent accounted for part of the run, so its figures
	// are a floor; the second accounted for all of its own.
	first := runner.RunRecord{
		CodingAgent: "claude-code", SandboxID: "box-1", CostUSD: 0.9,
		Spend: types.RunSpend{Collected: true, Models: []types.ModelSpend{
			{Model: "claude-sonnet", InputTokens: 600, OutputTokens: 60, CostUSD: 0.9},
		}},
		DeliveredRefs: []string{pr1},
	}
	second := runner.RunRecord{
		CodingAgent: "claude-code", SandboxID: "box-1", CostUSD: 0.5,
		Spend: types.RunSpend{Collected: true, Whole: true, Models: []types.ModelSpend{
			{Model: "claude-sonnet", InputTokens: 300, OutputTokens: 30, CostUSD: 0.4},
			{Model: "claude-haiku", InputTokens: 100, OutputTokens: 10, CostUSD: 0.1},
		}},
		DeliveredRefs: []string{pr1, pr2},
	}

	// relaunch resumes the phase parked after two rounds (500 tokens on
	// pre-model) with the first run, and the phase launches again in a
	// round of its own (100 on post-model). What it returns is the new
	// suspension as the next resume decodes it off the row.
	relaunch := func(t *testing.T, counted bool) execstate.State {
		t.Helper()
		state := suspendedAfterTwoRounds()
		state.Model = "pre-model"
		state.Models = []types.ModelSpend{{Model: "pre-model", InputTokens: 400, OutputTokens: 100}}
		state.ActiveTools = append(state.ActiveTools, "run_sandbox")
		again := served(llm.Completion{ToolCalls: []llm.ToolCall{{
			ID: "call-2", Name: "run_sandbox", Arguments: map[string]any{"task": "apply the answer"},
		}}}, "post-model", 60, 40)
		pub := newCapture()
		r, reg := buildWith(t, []phase.Entry{{Key: "default",
			Provider: &scriptedProvider{execute: []llm.Completion{again}}}}, buildOpts{
			pub: pub,
			resume: &runner.Resume{State: state, Answer: "the first run finished",
				Run: first, CarriedCounted: counted},
		})
		if err := reg.RegisterWith(suspendingTool{}, tools.Origin("sandbox"), tools.Annotations{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if w, _, err := r.Resume(context.Background(), 1, nil); err != nil || !w.Suspended {
			t.Fatalf("Resume = %+v, %v; want the phase suspended on its second run", w, err)
		}
		if got := phasesOfKind(t, pub, "execute"); len(got) != 0 {
			t.Fatalf("the phase that suspended again published %d execute records, want none", len(got))
		}
		if r.CountedCarried() {
			t.Error("the phase that suspended again says a record counted its carried spend")
		}
		parked, ok := r.Suspended()
		if !ok {
			t.Fatal("no suspension was recorded")
		}
		blob, err := execstate.Encode(parked.State)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		decoded, ok, err := execstate.Decode(blob)
		if err != nil || !ok {
			t.Fatalf("Decode = %v, %v", ok, err)
		}
		return decoded
	}
	// finish resumes the relaunched phase with the second run.
	finish := func(t *testing.T, pub *capture, state execstate.State, prov llm.Provider,
		counted bool,
	) (*runner.Runner, error) {
		t.Helper()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
			pub: pub,
			resume: &runner.Resume{State: state, Answer: "the second run finished",
				Run: second, CarriedCounted: counted},
		})
		_, _, err := r.Resume(context.Background(), 1, nil)
		return r, err
	}
	submitted := func(t *testing.T) llm.Provider {
		return &scriptedProvider{execute: []llm.Completion{served(submitWork(t), "post-model", 30, 20)}}
	}

	t.Run("the final record counts both runs", func(t *testing.T) {
		t.Parallel()
		pub := newCapture()
		r, err := finish(t, pub, relaunch(t, false), submitted(t), false)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		done := completedPhase(t, pub, "execute")
		// 500 before the first suspend, the first run's 660, the relaunching
		// round's 100, the second run's 440 and the final round's 50.
		if done.TotalTokens != 1750 || math.Abs(done.CostUSD-1.4) > 1e-9 {
			t.Errorf("record = %d tokens at $%v, want 1750 at both runs' $1.40", done.TotalTokens, done.CostUSD)
		}
		want := map[string][2]int{
			"pre-model": {400, 100}, "post-model": {90, 60},
			"claude-sonnet": {900, 90}, "claude-haiku": {100, 10},
		}
		got := splitOf(done)
		if len(got) != len(want) {
			t.Errorf("models = %+v, want %v", done.Models, want)
		}
		for model, tokens := range want {
			if got[model] != tokens {
				t.Errorf("models = %+v, want %v", done.Models, want)
				break
			}
		}
		if !done.RunSpendUnreported {
			t.Error("the record is not marked a floor, but the first run's agent accounted for part of it")
		}
		if !slices.Equal(done.DeliveredRefs, []string{pr1, pr2}) {
			t.Errorf("delivered_refs = %v, want both runs' refs, each once", done.DeliveredRefs)
		}
		if got := r.Spend().Total(); got != done.TotalTokens {
			t.Errorf("the turn totals %d, its record %d", got, done.TotalTokens)
		}
		rollup := rollupOf(done)
		if rollup.Totals.TotalTokens != 1750 || math.Abs(rollup.Totals.CostUSD-1.4) > 1e-9 {
			t.Errorf("the record rolls up to %+v, want 1750 tokens and $1.40", rollup.Totals)
		}
	})

	t.Run("a retry of the final resume counts none of them again", func(t *testing.T) {
		t.Parallel()
		pub := newCapture()
		state := relaunch(t, false)
		billed := served(thinkAndCall(t, "lookup_colleague", `{"query":"ana"}`, "who asked"), "post-model", 60, 40)
		if _, err := finish(t, pub, state, &postsThenFails{post: billed}, false); err == nil {
			t.Fatal("the first attempt's provider failed and the resume reported no error")
		}
		if _, err := finish(t, pub, state, submitted(t), true); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		records := phasesOfKind(t, pub, "execute")
		if len(records) != 2 {
			t.Fatalf("%d execute records, want one per attempt", len(records))
		}
		failed, retry := records[0], records[1]
		if failed.TotalTokens != 1800 || math.Abs(failed.CostUSD-1.4) > 1e-9 {
			t.Errorf("the first record = %d tokens at $%v, want 1800 at $1.40", failed.TotalTokens, failed.CostUSD)
		}
		if retry.TotalTokens != 50 || retry.CostUSD != 0 || retry.RunSpendUnreported {
			t.Errorf("the retry = %d tokens at $%v (unreported %v), want its own 50 and nothing carried",
				retry.TotalTokens, retry.CostUSD, retry.RunSpendUnreported)
		}
		if !slices.Equal(retry.DeliveredRefs, []string{pr1, pr2}) {
			t.Errorf("the retry's delivered_refs = %v, want both runs' refs: they are evidence, not spend",
				retry.DeliveredRefs)
		}
		rollup := rollupOf(records...)
		if rollup.Totals.TotalTokens != 1850 || math.Abs(rollup.Totals.CostUSD-1.4) > 1e-9 {
			t.Errorf("the attempts roll up to %+v, want 1850 tokens and both runs' $1.40 once", rollup.Totals)
		}
	})

	t.Run("a relaunch whose carried spend a record counted carries none of it", func(t *testing.T) {
		t.Parallel()
		state := relaunch(t, true)
		runs := state.CollectedRuns
		if runs == nil || runs.CostUSD != 0 || runs.Collected || len(runs.Models) != 0 ||
			!slices.Equal(runs.DeliveredRefs, []string{pr1}) {
			t.Fatalf("the suspension carries %+v, want the first run's refs and none of its spend", runs)
		}
		if state.InputTokens != 60 || state.OutputTokens != 40 {
			t.Errorf("the suspension carries %d/%d tokens, want the relaunching round's own 60/40",
				state.InputTokens, state.OutputTokens)
		}
		pub := newCapture()
		if _, err := finish(t, pub, state, submitted(t), false); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		done := completedPhase(t, pub, "execute")
		// The relaunching round's 100, the second run's 440 and the final
		// round's 50: the first run and the rounds before it are on the
		// record an earlier attempt published.
		if done.TotalTokens != 590 || math.Abs(done.CostUSD-0.5) > 1e-9 || done.RunSpendUnreported {
			t.Errorf("record = %d tokens at $%v (unreported %v), want 590 at the second run's $0.50, whole",
				done.TotalTokens, done.CostUSD, done.RunSpendUnreported)
		}
	})
}
