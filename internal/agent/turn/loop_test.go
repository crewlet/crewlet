package turn_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// fake is a scripted Phases. Each phase reads the round-th entry of its
// script, or the last one when the script runs short, so a test that only
// cares about round 1 does not have to spell out five identical rounds.
type fake struct {
	plans    []turn.Plan
	surfaces []turn.Surface
	// execSurfaces is what EXECUTE reports, when it differs from Plan's.
	// Activating a tool mid-phase is exactly that case, and with one shared
	// field the two are indistinguishable — which is how the gate came to
	// be judged against a stale catalogue with every test still passing.
	execSurfaces []turn.Surface
	execs        []turn.Execution
	reviews      []turn.Review

	planErr, execErr, revErr error

	planRounds, execRounds, revRounds int
	resumeRounds                      int
	resumeErr                         error
	notesSeen                         []string
	historySeen                       [][]ledger.Iteration
}

func at[T any](s []T, round int) T {
	var zero T
	if len(s) == 0 {
		return zero
	}
	if round-1 < len(s) {
		return s[round-1]
	}
	return s[len(s)-1]
}

func (f *fake) Plan(_ context.Context, round int, notes string, h []ledger.Iteration) (turn.Plan, turn.Surface, error) {
	f.planRounds++
	f.notesSeen = append(f.notesSeen, notes)
	f.historySeen = append(f.historySeen, h)
	if f.planErr != nil {
		return turn.Plan{}, turn.Surface{}, f.planErr
	}
	return at(f.plans, round), at(f.surfaces, round), nil
}

func (f *fake) Execute(_ context.Context, round int, _ turn.Plan, _ []ledger.Iteration) (turn.Execution, turn.Surface, error) {
	f.execRounds++
	if f.execErr != nil {
		return turn.Execution{}, turn.Surface{}, f.execErr
	}
	if len(f.execSurfaces) > 0 {
		return at(f.execs, round), at(f.execSurfaces, round), nil
	}
	return at(f.execs, round), at(f.surfaces, round), nil
}

func (f *fake) Resume(_ context.Context, _ []ledger.Iteration) (turn.Execution, turn.Surface, error) {
	f.resumeRounds++
	if f.resumeErr != nil {
		return turn.Execution{}, turn.Surface{}, f.resumeErr
	}
	// A resumed phase re-enters the FIRST round, so it reads the same slot
	// an ordinary Execute would have.
	return at(f.execs, 1), at(f.surfaces, 1), nil
}

func (f *fake) Review(_ context.Context, round int, _ turn.Plan, _ turn.Execution, _ []ledger.Iteration) (turn.Review, error) {
	f.revRounds++
	if f.revErr != nil {
		return turn.Review{}, f.revErr
	}
	return at(f.reviews, round), nil
}

func settings() turn.Settings { return turn.Settings{MaxIterations: 5} }

// slackSurface is a plain one-write-tool surface, which most tests here only
// need as a backdrop for the delivery gate.
func slackSurface() turn.Surface {
	return turn.Surface{
		Catalogue:  []string{"slack_post", "slack_history", "lookup_colleague"},
		MCPTools:   []string{"slack_post", "slack_history"},
		KnownReads: []string{"slack_history"},
	}
}

func TestADeliveredPlanReviewedDoneEndsTheTurn(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}, Summary: "post it"}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "posted", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{TurnID: "t1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done", res.Decision)
	}
	if res.Artifact != "posted" {
		t.Errorf("artifact = %q", res.Artifact)
	}
	if res.Rounds != 1 || f.planRounds != 1 || f.revRounds != 1 {
		t.Errorf("rounds = %d (plan %d, review %d), want one of each", res.Rounds, f.planRounds, f.revRounds)
	}
	if len(res.Iterations) != 0 {
		t.Errorf("a done round appended %d ledger entries; only a loop-back closes a round",
			len(res.Iterations))
	}
	if res.Breach != nil {
		t.Errorf("a clean turn reported a breach: %+v", res.Breach)
	}
}

func TestReviewsArtifactWinsOverExecutesText(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "draft", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.Done, FinalArtifact: "polished"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(), turn.Input{})
	if res.Artifact != "polished" {
		t.Errorf("artifact = %q, want the reviewer's", res.Artifact)
	}
	// And an empty one falls back rather than returning nothing.
	f.reviews = []turn.Review{{Decision: phase.Done}}
	res, _ = turn.Run(context.Background(), f, settings(), turn.Input{})
	if res.Artifact != "draft" {
		t.Errorf("artifact = %q, want Execute's text", res.Artifact)
	}
}

func TestASkipEndsTheTurnBeforeExecute(t *testing.T) {
	t.Parallel()
	// Nobody was asking. Nothing must reach the surface — running Execute
	// at all risks a side effect on a trigger that was not addressed here.
	f := &fake{plans: []turn.Plan{{Decision: turn.PlanSkip, Reasoning: "addressed to someone else"}}}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Skipped {
		t.Errorf("decision = %s, want skipped", res.Decision)
	}
	if res.Artifact != "addressed to someone else" {
		t.Errorf("artifact = %q, want the planner's reasoning", res.Artifact)
	}
	if f.execRounds != 0 || f.revRounds != 0 {
		t.Errorf("a skip ran Execute %d times and Review %d times; it must run neither",
			f.execRounds, f.revRounds)
	}
}

func TestADirectPlanThatDeliveredSkipsReview(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanDirect, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "posted", Calls: []ledger.Call{{Name: "slack_post"}}}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done", res.Decision)
	}
	if f.revRounds != 0 {
		t.Errorf("Review ran %d times on a delivered direct plan", f.revRounds)
	}
}

func TestADirectPlanThatDeliveredNothingIsReviewedAnyway(t *testing.T) {
	t.Parallel()
	// The safety net. Without it the turn completes as a silent no-op: the
	// seat appears to have answered and nothing reached the surface.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanDirect, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "I would post this", Calls: []ledger.Call{{Name: "lookup_colleague"}}}},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "you never posted"}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.revRounds == 0 {
		t.Fatal("Review was skipped on a direct plan that delivered nothing")
	}
	if res.Decision == phase.Done {
		t.Error("the turn completed as done having delivered nothing")
	}
}

// A RESCUED PLAN IS NOT A CHOSEN ONE, and the difference decides whether
// Review runs.
//
// This is the case observed against a live vendor stack: a seat was addressed
// on chat, the planner ran its rounds and never submitted a decision, the
// engine rescued the turn with a synthesised `direct`, Execute produced an
// answer, nothing was called, and the turn reported done. The reply never
// reached the channel and no warning said so.
//
// Both nets miss it by construction. `direct` skips Review outright, and the
// delivery gate that would force Review back on is keyed on ToolsNeeded —
// which a rescue, having no submitted plan, cannot have. So the mark has to
// be what the loop reads.
func TestARescuedPlanIsReviewedEvenThoughItSaysDirect(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans: []turn.Plan{{
			Decision: turn.PlanDirect,
			Rescued:  true,
			// EMPTY, as every rescue is: the planner submitted nothing,
			// so there is no tool list to key a gate on.
			ToolsNeeded: nil,
		}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "here is my answer"}},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "you never posted it"}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.revRounds == 0 {
		t.Fatal("Review was skipped on a plan the ENGINE wrote: the planner never " +
			"committed to Execute finishing in one shot, so there is no decision to honour")
	}
	if res.Decision == phase.Done {
		t.Error("a rescued turn that called nothing completed as done")
	}
}

// The counterfactual, and the reason the fix keys on the mark rather than on
// an empty tool list: a planner that DELIBERATELY chose `direct` with nothing
// to call is answering in conversation, and forcing Review on it would put a
// second model call on every "thanks, noted" in the company.
func TestAChosenDirectPlanWithNoToolsStillSkipsReview(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanDirect, ToolsNeeded: nil}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "acknowledged"}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.revRounds != 0 {
		t.Errorf("Review ran %d times on a chosen direct plan", f.revRounds)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done", res.Decision)
	}
}

func TestDoneIsOverturnedWhenNothingDelivered(t *testing.T) {
	t.Parallel()
	// Review judges from the produced text and says done even though no
	// tool sent it. The override loops back with a correction the reviewer
	// itself never wrote — which is why the correction has to come from
	// the engine.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "here is the answer", Calls: []ledger.Call{{Name: "slack_history"}}}},
		reviews: []turn.Review{
			{Decision: phase.Done},
			{Decision: phase.Done},
		},
	}
	f.execs = append(f.execs, turn.Execution{Text: "posted now", Calls: []ledger.Call{{Name: "slack_post"}}})
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d, want the override to have forced a second", res.Rounds)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done once the second round delivered", res.Decision)
	}
	if len(f.notesSeen) < 2 || !strings.Contains(f.notesSeen[1], "slack_post") {
		t.Errorf("the second Plan round did not receive the engine's correction: %q", f.notesSeen)
	}
}

func TestAKnownReadIsNotADelivery(t *testing.T) {
	t.Parallel()
	// The specific shape the override exists for: the phase DID call an MCP
	// tool, but a read-only one. Without the read annotation this reads as
	// delivery and the turn completes having sent nothing.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "answer", Calls: []ledger.Call{{Name: "slack_history"}}}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1}, turn.Input{})
	if res.Decision == phase.Done {
		t.Error("a read-only MCP call satisfied the delivery gate")
	}
}

func TestAPlanThatIntendedNothingIsTakenAtItsWord(t *testing.T) {
	t.Parallel()
	// A turn that was only ever going to think has nothing to deliver, so
	// done must stand. Overriding here would loop every advisory turn until
	// it ran out of rounds.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, Summary: "just answer in text"}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "my opinion"}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, settings(), turn.Input{})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done — nothing was ever going to be delivered", res.Decision)
	}
}

func TestTwoIdenticalRoundsAbortAsAStall(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "same every time", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "try again"}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachStall {
		t.Fatalf("breach = %+v, want a stall", res.Breach)
	}
	if res.Rounds != 2 {
		t.Errorf("rounds = %d, want the stall to fire on the second", res.Rounds)
	}
}

func TestProgressResetsNothingButAlsoDoesNotFalselyStall(t *testing.T) {
	t.Parallel()
	// Rounds that keep CHANGING the artifact are working, however many
	// there are. A stall guard that fired on round count instead of on
	// sameness would kill every genuinely iterating turn.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs: []turn.Execution{
			{Text: "v1", Calls: []ledger.Call{{Name: "slack_post"}}},
			{Text: "v2", Calls: []ledger.Call{{Name: "slack_post"}}},
			{Text: "v3", Calls: []ledger.Call{{Name: "slack_post"}}},
		},
		reviews: []turn.Review{
			{Decision: phase.SelfIterate, Notes: "closer"},
			{Decision: phase.SelfIterate, Notes: "closer still"},
			{Decision: phase.Done},
		},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done — every round changed the artifact", res.Decision)
	}
	if res.Breach != nil {
		t.Errorf("an iterating turn reported %+v", res.Breach)
	}
}

func TestRunningOutOfRoundsIsAFailureThatSaysSo(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs: []turn.Execution{
			{Text: "v1", Calls: []ledger.Call{{Name: "slack_post"}}},
			{Text: "v2", Calls: []ledger.Call{{Name: "slack_post"}}},
		},
		reviews: []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}},
	}
	res, err := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 2}, turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachMaxIterations {
		t.Fatalf("breach = %+v, want max_iter", res.Breach)
	}
	if !strings.Contains(res.Breach.Detail, "2") {
		t.Errorf("the breach does not say how many rounds it had: %q", res.Breach.Detail)
	}
}

func TestZeroIterationsStillRunsOneRound(t *testing.T) {
	t.Parallel()
	// A misconfigured 0 must not mean "unbounded" — that would spend a
	// company's whole budget on one trigger — and must not mean "do
	// nothing", which no operator ever configured on purpose.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "posted", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{}, turn.Input{})
	if res.Rounds != 1 || res.Decision != phase.Done {
		t.Errorf("rounds = %d, decision = %s; want exactly one round", res.Rounds, res.Decision)
	}
}

func TestTheLedgerCarriesWhatTheNextRoundNeeds(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans: []turn.Plan{{
			Decision: turn.PlanRun, Summary: "post the summary",
			ToolsNeeded: []string{"slack_post"},
			Calls:       []ledger.Call{{Name: "slack_history"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
		execs: []turn.Execution{
			{Text: "v1", Calls: []ledger.Call{{Name: "slack_post", Args: map[string]any{"channel": "C0ENG"}}}},
			{Text: "v2", Calls: []ledger.Call{{Name: "slack_post"}}},
		},
		reviews: []turn.Review{
			{Decision: phase.SelfIterate, Notes: "wrong link", CompletedWork: "the post landed"},
			{Decision: phase.Done},
		},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger has %d entries, want the one closed round", len(res.Iterations))
	}
	rec := res.Iterations[0]
	if rec.PlanSummary != "post the summary" || rec.ExecuteText != "v1" {
		t.Errorf("ledger entry lost its content: %+v", rec)
	}
	if rec.CompletedWork != "the post landed" || rec.ReviewNotes != "wrong link" {
		t.Errorf("ledger entry lost the reviewer's words: %+v", rec)
	}
	// The reads recorded are the ones actually CALLED, not the whole
	// surface's annotation set — the row is persisted across a suspend and
	// would otherwise grow with the catalogue rather than with the round.
	if len(rec.Reads) != 1 || rec.Reads[0] != "slack_history" {
		t.Errorf("reads = %v, want just the read this round used", rec.Reads)
	}
	// And the second Plan round actually SAW it. A ledger nothing reads is
	// a ledger that cannot stop a duplicate delivery.
	if len(f.historySeen) < 2 || len(f.historySeen[1]) != 1 {
		t.Errorf("the second Plan round saw %v, want the closed round", f.historySeen)
	}
}

func TestAResumedTurnInheritsTheLedgerItLeftBehind(t *testing.T) {
	t.Parallel()
	// A suspended turn ends and its completion starts a NEW one. Without
	// inheritance the new turn forgets every earlier round and re-fires
	// their deliveries.
	// Spare CAPACITY, deliberately. A len==cap slice reallocates on the
	// first append and hides an aliasing bug completely; found by mutation,
	// where dropping the clone changed nothing.
	prior := make([]ledger.Iteration, 1, 4)
	prior[0] = ledger.Iteration{Iteration: 1, ExecuteText: "posted before the suspend"}
	// The turn must CLOSE a round for an append to happen at all — a done
	// round appends nothing, so a single-round turn cannot observe this.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs: []turn.Execution{
			{Text: "v1", Calls: []ledger.Call{{Name: "slack_post"}}},
			{Text: "v2", Calls: []ledger.Call{{Name: "slack_post"}}},
		},
		reviews: []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}, {Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{History: prior})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.historySeen) == 0 || len(f.historySeen[0]) != 1 {
		t.Fatalf("the first Plan round saw %v, want the inherited round", f.historySeen)
	}
	if len(res.Iterations) != 2 {
		t.Errorf("ledger = %d entries, want the inherited round plus the closed one",
			len(res.Iterations))
	}
	if res.Iterations[0].ExecuteText != "posted before the suspend" {
		t.Errorf("the inherited round was overwritten: %+v", res.Iterations[0])
	}
	// And the caller's slice must not be the one the loop appends to. The
	// length cannot change through an append, so the check is on the
	// backing ARRAY: a loop appending into the caller's spare capacity
	// writes a round into memory the caller still owns.
	if prior[:cap(prior)][1].Iteration != 0 {
		t.Errorf("Run appended into the caller's backing array: %v", prior[:cap(prior)])
	}
}

func TestASuspendHandsTheLedgerOutWithoutClosingTheRound(t *testing.T) {
	t.Parallel()
	// The round's Review has not run. Appending it would tell the resumed
	// turn a delivery was judged when nothing judged it.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"run_sandbox"}}},
		surfaces: []turn.Surface{{Catalogue: []string{"run_sandbox"}}},
		execs:    []turn.Execution{{Text: "launched", Suspended: true}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{
		History: []ledger.Iteration{{Iteration: 1}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended {
		t.Error("a suspended Execute did not report the turn as suspended")
	}
	if f.revRounds != 0 {
		t.Error("Review ran on a suspended round")
	}
	if len(res.Iterations) != 1 {
		t.Errorf("ledger has %d entries, want only the inherited one", len(res.Iterations))
	}
}

func TestTheDelegationCapEndsTheTurnBeforeAnyPhaseRuns(t *testing.T) {
	t.Parallel()
	// The always-on backstop against a circular handoff. It must fire
	// before Plan, or a cycle still costs one full turn of tokens per hop.
	f := &fake{plans: []turn.Plan{{Decision: turn.PlanRun}}}
	res, err := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 5, DelegationDepthLimit: 3},
		turn.Input{Depth: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachDepth {
		t.Fatalf("breach = %+v, want depth", res.Breach)
	}
	if f.planRounds != 0 {
		t.Error("Plan ran despite the depth cap")
	}
	// One below the cap still runs — otherwise the cap is off by one and
	// every chain is a hop shorter than configured.
	f2 := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "ok"}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	if res, _ := turn.Run(context.Background(), f2, turn.Settings{MaxIterations: 5, DelegationDepthLimit: 3},
		turn.Input{Depth: 2}); res.Decision != phase.Done {
		t.Errorf("depth 2 under a limit of 3 was refused: %s", res.Decision)
	}
}

func TestADepthLimitOfZeroDisablesTheCap(t *testing.T) {
	t.Parallel()
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "ok"}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 2}, turn.Input{Depth: 99})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done — a zero limit disables the cap", res.Decision)
	}
}

func TestAReviewerThatGivesUpEndsTheTurnWithoutABreach(t *testing.T) {
	t.Parallel()
	// A reviewed failure and a guard breach are different facts. Reporting
	// a breach here would make a considered "this cannot be done" look like
	// the engine cut the turn off.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "tried", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.Failed, Notes: "the API is down"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(), turn.Input{})
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach != nil {
		t.Errorf("a reviewed failure reported a guard breach: %+v", res.Breach)
	}
	if res.LastReview == nil || res.LastReview.Notes != "the API is down" {
		t.Error("the reviewer's last word was lost")
	}
}

func TestTheReviewersLastWordSurvivesADoneRound(t *testing.T) {
	t.Parallel()
	// A done round appends no ledger entry, so without carrying the review
	// out the reviewer's prose about what landed never reaches the
	// conversation ledger written at turn end.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "posted", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:  []turn.Review{{Decision: phase.Done, CompletedWork: "the #eng post landed"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(), turn.Input{})
	if res.LastReview == nil || res.LastReview.CompletedWork != "the #eng post landed" {
		t.Errorf("last review = %+v", res.LastReview)
	}
}

func TestAPhaseThatBrokeIsAnErrorNotAFailedTurn(t *testing.T) {
	t.Parallel()
	// "The model did not finish" and "the process is broken" are different
	// conditions and a caller has to be able to tell them apart.
	boom := errors.New("provider unreachable")
	for name, f := range map[string]*fake{
		"plan":    {planErr: boom},
		"execute": {plans: []turn.Plan{{Decision: turn.PlanRun}}, execErr: boom},
		"review": {
			plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
			surfaces: []turn.Surface{slackSurface()},
			execs:    []turn.Execution{{Calls: []ledger.Call{{Name: "slack_post"}}}},
			revErr:   boom,
		},
	} {
		_, err := turn.Run(context.Background(), f, settings(), turn.Input{})
		if !errors.Is(err, boom) {
			t.Errorf("%s: err = %v, want the phase's own error", name, err)
		}
	}
}

func TestNoPhasesIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := turn.Run(context.Background(), nil, settings(), turn.Input{}); err == nil {
		t.Error("a nil Phases ran without error")
	}
}

func TestExecutesSurfaceIsWhatTheGateJudges(t *testing.T) {
	t.Parallel()
	// Activating a tool mid-Execute changes the catalogue. Judging against
	// PLAN's view reads a real delivery through the newly-activated tool as
	// a phantom, overturns a genuine done, and loops the turn re-posting.
	//
	// Plan could not see slack_post; Execute discovered and activated it,
	// then used it.
	f := &fake{
		plans: []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"}}},
		surfaces: []turn.Surface{{
			Catalogue: []string{"list_mcp_server_tools", "activate_tool"},
		}},
		execSurfaces: []turn.Surface{slackSurface()},
		execs:        []turn.Execution{{Text: "posted", Calls: []ledger.Call{{Name: "slack_post"}}}},
		reviews:      []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1}, turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done — the delivery went through a tool "+
			"Execute activated after Plan reported its surface", res.Decision)
	}
}

func TestAPhantomOnlyPlanStillCountsAsIntendingToAct(t *testing.T) {
	t.Parallel()
	// ExpectedAction keys off the RAW tools_needed. A planner that named
	// only tools it cannot see still meant to deliver, and reading the
	// unresolvable list as "intended nothing" turns a failed delivery into
	// a clean turn — silently, on exactly the surfaces where the planner
	// cannot see the catalogue.
	//
	// Found by mutation: keying off the RESOLVED list instead passed every
	// other case in this file.
	f := &fake{
		plans:    []turn.Plan{{Decision: turn.PlanRun, ToolsNeeded: []string{"slack_send_msg"}}},
		surfaces: []turn.Surface{slackSurface()},
		execs:    []turn.Execution{{Text: "here is the answer"}},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1}, turn.Input{})
	if res.Decision == phase.Done {
		t.Error("a plan whose only delivery tool was a wrong guess, which then " +
			"delivered nothing, completed as done")
	}
	if len(f.notesSeen) == 0 {
		t.Fatal("no rounds ran")
	}
}

func TestOnlyTheReadsActuallyUsedAreRecorded(t *testing.T) {
	t.Parallel()
	// The ledger row is persisted across a sandbox suspend. Carrying every
	// read-only tool on the surface makes it grow with the CATALOGUE rather
	// than with what the round did — on a large MCP surface that is
	// hundreds of names per round, forever.
	//
	// Found by mutation: with one annotated read on the surface and that
	// same read called, "the whole surface" and "what was called" are the
	// same list and the narrowing is unasserted.
	wide := turn.Surface{
		Catalogue:  []string{"slack_post", "slack_history", "jira_get", "confluence_get"},
		MCPTools:   []string{"slack_post", "slack_history", "jira_get", "confluence_get"},
		KnownReads: []string{"slack_history", "jira_get", "confluence_get"},
	}
	f := &fake{
		plans: []turn.Plan{{
			Decision: turn.PlanRun, ToolsNeeded: []string{"slack_post"},
			Calls: []ledger.Call{{Name: "slack_history"}},
		}},
		surfaces: []turn.Surface{wide},
		execs: []turn.Execution{
			{Text: "v1", Calls: []ledger.Call{{Name: "slack_post"}}},
			{Text: "v2", Calls: []ledger.Call{{Name: "slack_post"}}},
		},
		reviews: []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}, {Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger has %d entries, want one", len(res.Iterations))
	}
	if got := res.Iterations[0].Reads; len(got) != 1 || got[0] != "slack_history" {
		t.Errorf("reads = %v, want only the read this round used", got)
	}
}

// A resumed turn that re-planned would re-derive a plan for work already
// half-done. The FIRST round re-enters the suspended conversation instead.
func TestAResumedTurnSkipsPlanForItsFirstRound(t *testing.T) {
	f := &fake{
		execs:   []turn.Execution{{Text: "finished the code work"}},
		reviews: []turn.Review{{Decision: "done", FinalArtifact: "shipped"}},
	}
	res, err := turn.Run(t.Context(), f, settings(), turn.Input{TurnID: "t1", Resume: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.resumeRounds != 1 {
		t.Fatalf("Resume ran %d times, want 1", f.resumeRounds)
	}
	if f.planRounds != 0 {
		t.Fatalf("Plan ran %d times on a resumed turn's first round", f.planRounds)
	}
	if f.execRounds != 0 {
		t.Fatalf("Execute ran %d times instead of Resume", f.execRounds)
	}
	if f.revRounds != 1 {
		t.Fatalf("Review ran %d times, want the resumed round judged", f.revRounds)
	}
	if res.Decision != phase.Done {
		t.Fatalf("decision = %v, want done", res.Decision)
	}
}

// Only the first round. A resumed executor that self-iterates gets an ordinary
// planned round after that — otherwise the turn could never change course.
func TestOnlyTheFirstRoundOfAResumedTurnSkipsPlan(t *testing.T) {
	f := &fake{
		plans: []turn.Plan{{Summary: "second pass"}},
		execs: []turn.Execution{{Text: "resumed"}, {Text: "second pass"}},
		reviews: []turn.Review{
			{Decision: "self_iterate", Notes: "not there yet"},
			{Decision: "done", FinalArtifact: "shipped"},
		},
	}
	if _, err := turn.Run(t.Context(), f, settings(), turn.Input{TurnID: "t1", Resume: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.resumeRounds != 1 {
		t.Fatalf("Resume ran %d times, want exactly the first round", f.resumeRounds)
	}
	if f.planRounds != 1 || f.execRounds != 1 {
		t.Fatalf("round two ran plan %d / execute %d, want one of each", f.planRounds, f.execRounds)
	}
}

// The resumed executor may call run_sandbox again to continue in the same box.
func TestAResumedTurnCanSuspendAgain(t *testing.T) {
	f := &fake{execs: []turn.Execution{{Text: "launched another job", Suspended: true}}}
	res, err := turn.Run(t.Context(), f, settings(), turn.Input{TurnID: "t1", Resume: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended {
		t.Fatal("a second suspend was not reported")
	}
	if f.revRounds != 0 {
		t.Fatal("Review judged a round that suspended before it finished")
	}
}

// The suspended turn's closed rounds have to reach the resumed one, or it
// re-fires deliveries that already went.
func TestAResumedTurnInheritsTheSuspendedTurnsLedger(t *testing.T) {
	prior := []ledger.Iteration{{Iteration: 1, PlanSummary: "clone and fix"}}
	f := &fake{
		execs:   []turn.Execution{{Text: "done"}},
		reviews: []turn.Review{{Decision: "done", FinalArtifact: "shipped"}},
	}
	res, err := turn.Run(t.Context(), f, settings(), turn.Input{
		TurnID: "t1", Resume: true, History: prior,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Iterations) == 0 || res.Iterations[0].PlanSummary != "clone and fix" {
		t.Fatalf("iterations = %+v, want the suspended turn's round carried in", res.Iterations)
	}
}

// A resume that cannot re-enter is an engine failure, not a turn outcome.
func TestAFailedResumeIsAnError(t *testing.T) {
	f := &fake{resumeErr: errors.New("state is unreadable")}
	if _, err := turn.Run(t.Context(), f, settings(), turn.Input{TurnID: "t1", Resume: true}); err == nil {
		t.Fatal("a resume that could not re-enter returned no error")
	}
}
