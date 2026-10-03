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
// vouch for, which is one minted before the ledger's watermark — the point
// before which it may have lost rows, to its sweep or to a snapshot adopted
// from a donor that scrubbed it — whose row it does not hold. A re-run comes
// after the loss it is judged against by construction, so an id carrying the
// RUN's clock is one a re-run after a loss publishes a second time — which is
// exactly how the operation ledger was defeated while every writer stamped its
// own clock beside the id.
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

// A PAGE WRITE MADE FROM A TURN CARRIES THE INSTANT THE TURN'S WORK BEGAN, so
// the knowledge base derives its operation id from the same identity the
// tracker's writes use — every page write, not only a comment: a create, a
// save, the rename a save makes and a comment edit each derive an operation
// from the key, and one carrying no instant is read as older than every loss
// the ledger has had, so a keyed page write on a node whose ledger ever lost a
// row would answer `unknown` for ever.
func TestAPageWriteCarriesTheTurnsInstant(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	kb := newFakeKB()
	turn := workTurn(t)
	turn.WorkSince = began
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	for name, args := range map[string]map[string]any{
		builtin.CommentOnPageTool: {"page": "p1", "body": "noted"},
		builtin.WritePageTool:     {"title": "New Page", "body": "text", "container": "ENG"},
		builtin.SavePageTool: {"page": "p1", "base_version": 4, "body": "text",
			"title": "Deploy Guide"},
	} {
		call(t, reg, turn, name, args)
	}
	call(t, reg, turn, builtin.CommentOnPageTool,
		map[string]any{"page": "p1", "body": "noted, again", "edit": "m1"})
	// A comment, a create, a save and its rename, and an edit.
	if len(kb.keys) != 5 {
		t.Fatalf("the page writes reached the knowledge base with %d keys, want 5",
			len(kb.keys))
	}
	// EACH KEY IS AN ID THE ENGINE'S GRAMMAR MINTED, at the instant the
	// work began — the store derives every write's id from it and the
	// instant rides along.
	for i, key := range kb.keys {
		at, ok := statelog.OpMintedAt(key)
		if !ok || !at.Equal(began) || statelog.CheckCallerOpID(key) != nil {
			t.Errorf("page write %d carries key %q minted at %v (%v), want an "+
				"op id carrying %s when the work began — a write re-made after "+
				"an adoption is otherwise decided twice", i, key, at, ok, began)
		}
	}
}

// call drives a seat-callable tool under the given turn, as a caller holding
// every grant: these cases are about the operation a write carries, and a
// rename asked of a container this fixture holds no chart for is otherwise
// undecidable rather than refused or allowed.
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
	got, err := callable.CallForTurn(everyGrant(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got.Failed {
		t.Fatalf("%s failed: %s", name, got.Output)
	}
	return got
}
