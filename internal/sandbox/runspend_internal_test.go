package sandbox

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// spendingResult is a finished job whose agent accounted for its whole spend,
// split over two models.
func spendingResult() Result {
	return Result{
		Success: true, Text: "done",
		InputTokens: 900, OutputTokens: 100, CostUSD: 0.4,
		Models: []types.ModelSpend{
			{Model: "claude-sonnet", InputTokens: 600, OutputTokens: 60, CostUSD: 0.3},
			{Model: "claude-haiku", InputTokens: 300, OutputTokens: 40, CostUSD: 0.1},
		},
		UsageWhole: true,
	}
}

// THE RUN'S OWN SPEND REACHES THE RESUMED PHASE. The coding agent in the box
// spends on models the engine never called, so the resume is the one place its
// account can reach the phase record: its tokens by model, and whether they are
// the whole run's.
func TestACollectedRunsSpendReachesItsResume(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(spendingResult())
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want once", len(calls))
	}
	want := types.RunSpend{Collected: true, Whole: true, Models: spendingResult().Models}
	if fmt.Sprint(calls[0].RunSpend) != fmt.Sprint(want) {
		t.Errorf("run spend = %+v, want %+v", calls[0].RunSpend, want)
	}
	rig.finished("t1")
}

// A HELD RESULT KEEPS ITS SPEND. A result the budget had no room for waits on
// the run's row, and the resume that later re-enters the turn is handed the
// run's account from there — the process resuming may not be the one that
// collected it.
func TestAHeldResultKeepsItsSpendForTheResume(t *testing.T) {
	rig := newCoordRig(t)
	budget := &roomSpy{room: Room{Scope: "org", Used: 1000, Limit: 1000}}
	coordinator := rig.budgeted(t, budget)
	rig.launch("t1")
	coordinator.markBusy("swe")
	rig.runner.Finish(spendingResult())
	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if held, ok := rig.get("t1").Held(); !ok || held.Spend == nil || !held.Spend.Whole ||
		len(held.Spend.Models) != 2 {
		t.Fatalf("the row holds %+v, want the result with its spend", held)
	}

	budget.set(Room{OK: true}, nil)
	if err := rig.signalReady(t, coordinator, "t1"); err != nil {
		t.Fatalf("the held result's resume: %v", err)
	}
	calls := rig.resumer.calls()
	want := types.RunSpend{Collected: true, Whole: true, Models: spendingResult().Models}
	if len(calls) != 1 || fmt.Sprint(calls[0].RunSpend) != fmt.Sprint(want) {
		t.Fatalf("resumes = %+v, want one carrying %+v", calls, want)
	}
	rig.finished("t1")
}

// A RESULT HELD WITHOUT A SPEND IS A RUN NOBODY ACCOUNTED FOR. A build that
// kept no spend beside a held result still collected a run that spent, so the
// resume reports that run as collected with its spend unreported — never as a
// run that cost nothing, and never as no run at all.
func TestAResultHeldWithoutASpendResumesAsUnreported(t *testing.T) {
	t.Parallel()
	var none *HeldSpend
	got := none.runSpend()
	if !got.Collected || got.Whole || len(got.Models) != 0 {
		t.Errorf("runSpend = %+v, want a collected run whose spend went unreported", got)
	}
}

// WHAT A SPLIT DOES NOT NAME STAYS IN THE RUN'S SPEND, as one part under no
// model's name: an agent that reports totals only is still counted whole, and
// its price travels with the tokens it prices.
func TestARunReportingTotalsOnlyIsOnePartUnderNoName(t *testing.T) {
	t.Parallel()
	got := Result{InputTokens: 70, OutputTokens: 30, CostUSD: 0.2}.spend()
	want := []types.ModelSpend{{InputTokens: 70, OutputTokens: 30, CostUSD: 0.2}}
	if !got.Collected || got.Whole || fmt.Sprint(got.Models) != fmt.Sprint(want) {
		t.Errorf("spend = %+v, want the totals as one unnamed part of a floor", got)
	}
}

// A HELD SPEND CARRIES WHAT IT DOES NOT KNOW, at both depths: a newer build's
// member on the spend, or on one model's part of it, survives this build's
// read-modify-write of the row it is held on.
func TestAHeldSpendCarriesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	raw := `{"launch":"l1","at":"2026-06-14T12:00:00Z","spend":{"input_tokens":5,"whole":true,` +
		`"cache_tokens":9,"models":[{"model":"m","input_tokens":5,"tier":"priority"}]}}`
	var held HeldAnswer
	if err := json.Unmarshal([]byte(raw), &held); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(held)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, member := range []string{`"cache_tokens":9`, `"tier":"priority"`} {
		if !strings.Contains(string(out), member) {
			t.Errorf("a write dropped %s: %s", member, out)
		}
	}
}

// A PERSON'S ANSWER CARRIES THE SPEND OF THE RUN THAT ASKED. A coding agent
// stops once it has asked, so the run that asked finished: it was collected
// and charged when it parked, and the phase the answer resumes is the one
// record its figures can reach. The run the answer leads to is a new launch,
// reported on its own.
func TestAPersonsAnswerCarriesTheSpendOfTheRunThatAsked(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	asked := Result{NeedsInput: true, Question: "which branch?", AskTo: "requester",
		InputTokens: 40, OutputTokens: 4, CostUSD: 0.02, UsageWhole: true,
		Models: []types.ModelSpend{{Model: "claude-sonnet", InputTokens: 40, OutputTokens: 4, CostUSD: 0.02}}}
	rig.runner.Finish(asked)
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	reply := events.New(types.ExternalNotification{Body: "main"}, events.TraceContext{})
	handled, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", "chat:C1", "main", reply)
	if err != nil || !handled {
		t.Fatalf("TryResumeFromAnswer = %v, %v", handled, err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want once", len(calls))
	}
	want := types.RunSpend{Collected: true, Whole: true, Models: asked.Models}
	if fmt.Sprint(calls[0].RunSpend) != fmt.Sprint(want) || calls[0].CostUSD != 0.02 {
		t.Errorf("the answer resumed with spend %+v at $%v, want the asking run's %+v at $0.02",
			calls[0].RunSpend, calls[0].CostUSD, want)
	}
	rig.finished("t1")
}

// A SPEND HELD FOR ANOTHER LAUNCH IS NOBODY'S. A build that keeps no spend can
// park a later launch on the same row and carry an earlier launch's figures
// through its write; the answer to the later question must not report them as
// its own run's, so it reports that run's spend as unreported instead.
func TestASpendHeldForAnotherLaunchIsUnreported(t *testing.T) {
	t.Parallel()
	run := PendingRun{LaunchID: "l2", AskedLaunch: "l1",
		AskedSpend: &HeldSpend{InputTokens: 40, OutputTokens: 4, CostUSD: 0.02, Whole: true}}
	price, spend := run.askedSpend()
	if price != 0 || !spend.Collected || spend.Whole || len(spend.Models) != 0 {
		t.Errorf("askedSpend = $%v %+v, want an unreported run and no price", price, spend)
	}
}
