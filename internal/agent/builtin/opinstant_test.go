package builtin_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A RE-RUN OF ONE UNIT OF WORK WRITES UNDER THE SAME OPERATION ID AND THE SAME
// MINT INSTANT — the instant the work BEGAN, never the instant of the run.
//
// The state log refuses to decide again an operation its node's ledger cannot
// vouch for, which is one minted before the node's latest adoption of a
// donated snapshot. A re-run comes after the adoption it is judged against by
// construction, so an id carrying the RUN's clock is one a re-run after an
// adoption publishes a second time — which is exactly how the operation ledger
// was defeated while every writer stamped its own clock beside the id.
func TestARerunReusesTheOperationIDAndItsMintInstant(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	// Two runs of ONE trigger: a redelivery re-derives the work key and
	// the instant it began from the same events, under a run id of its
	// own minted hours later — which is the clock that must not leak in.
	attempt := func(minted time.Time) *turnctx.Turn {
		tn := workTurn(t)
		tn.RunID = statelog.NewOpID(minted, "")
		tn.WorkKey, tn.WorkSince = "wk-1", began
		return tn
	}
	ids := map[string]string{}
	for name, turn := range map[string]*turnctx.Turn{
		"the first run":  attempt(began.Add(time.Minute)),
		"the redelivery": attempt(began.Add(3 * time.Hour)),
	} {
		trk := newFakeTracker()
		call(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			turn, builtin.UpdateWorkItemTool, map[string]any{
				"item": "ENG-1", "priority": "urgent",
			})
		if len(trk.opIDs) != 1 {
			t.Fatalf("%s wrote %v", name, trk.opIDs)
		}
		ids[name] = trk.opIDs[0]
	}
	if ids["the first run"] != ids["the redelivery"] {
		t.Fatalf("the two runs wrote under %q and %q — a redelivery the engine "+
			"guarantees becomes a second write", ids["the first run"],
			ids["the redelivery"])
	}
	minted, ok := statelog.OpMintedAt(ids["the redelivery"])
	if !ok {
		t.Fatalf("the operation id %q carries no mint instant, so the state log "+
			"reads it as older than every adoption and a node that ever adopted "+
			"a snapshot can never write it", ids["the redelivery"])
	}
	if !minted.Equal(began) {
		t.Fatalf("the redelivery's operation was minted at %s, want %s when the "+
			"unit of work began — a later instant reads a re-run after an "+
			"adoption as a new operation, and it is decided twice", minted, began)
	}
}

// AND A RUN WITH NO UNIT OF WORK CARRIES THE RUN'S OWN START, which its id holds
// — the same choice [builtin.Actor.OperationSeed] makes for the seed, so the
// seed and its instant are always one identity.
func TestARunWithNoWorkKeyCarriesItsOwnStart(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	actor := builtin.Actor{TurnID: statelog.NewOpID(started, "")}
	if got := actor.OperationSince(); !got.Equal(started) {
		t.Fatalf("OperationSince = %s, want the run's start %s", got, started)
	}
	keyed := builtin.Actor{
		TurnID: statelog.NewOpID(started.Add(time.Hour), ""), WorkKey: "wk-1",
		WorkSince: started,
	}
	if got := keyed.OperationSince(); !got.Equal(started) {
		t.Fatalf("OperationSince = %s, want the work's start %s — a work key "+
			"seeds the id, so its instant is the work's and not this run's",
			got, started)
	}
}

// A PAGE COMMENT MADE FROM A TURN CARRIES THE INSTANT THE TURN'S WORK BEGAN, so
// the knowledge base derives its operation id from the same identity the
// tracker's writes use.
func TestAPageCommentCarriesTheTurnsInstant(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	kb := newFakeKB()
	turn := workTurn(t)
	turn.WorkSince = began
	call(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb}), turn,
		builtin.CommentOnPageTool, map[string]any{"page": "p1", "body": "noted"})
	if len(kb.comments) != 1 {
		t.Fatalf("the comment reached the knowledge base %d time(s)", len(kb.comments))
	}
	if got := kb.comments[0].TurnSince; !got.Equal(began) {
		t.Fatalf("the comment carries %s, want %s when the work began — a "+
			"comment re-posted after an adoption is otherwise decided twice",
			got, began)
	}
}

// call drives a seat-callable tool under the given turn.
func call(t *testing.T, reg *tools.Registry, turn *turnctx.Turn, name string,
	args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	callable, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is not seat-callable", name)
	}
	got, err := callable.CallForTurn(t.Context(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got.Failed {
		t.Fatalf("%s failed: %s", name, got.Output)
	}
	return got
}
