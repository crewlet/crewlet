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
// MINT INSTANT: the instant the work BEGAN, never the run's. The state log
// refuses to decide again an operation minted before its ledger's watermark
// whose row it lacks; an id carrying the RUN's clock would read a re-run after
// a loss as new, and publish it a second time.
func TestARerunReusesTheOperationIDAndItsMintInstant(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	// Two runs of ONE trigger: a redelivery re-derives the work key and its
	// instant, under a run id minted hours later, whose clock must not leak in.
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

// AND A RUN WITH NO UNIT OF WORK CARRIES THE RUN'S OWN START, the same choice
// [builtin.Actor.OperationSeed] makes, so the seed and its instant are always
// one identity.
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

// A PAGE WRITE MADE FROM A TURN CARRIES THE INSTANT THE TURN'S WORK BEGAN, as
// the tracker's do: every page write (a comment, a create, a save, the rename a
// save makes, a comment edit) derives its operation from the key, and a key
// with no instant is read as older than every ledger loss, so it would answer
// `unknown` for ever on a node whose ledger ever lost a row.
func TestAPageWriteCarriesTheTurnsInstant(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	kb := newFakeKB()
	turn := workTurn(t)
	turn.WorkSince = began
	reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
	for name, args := range map[string]map[string]any{
		builtin.CommentOnPageTool: {"asks": false, "page": "p1", "body": "noted"},
		builtin.WritePageTool:     {"title": "New Page", "body": "text", "container": "ENG"},
		builtin.SavePageTool: {"page": "p1", "base_version": 4, "body": "text",
			"title": "Deploy Guide"},
	} {
		call(t, reg, turn, name, args)
	}
	call(t, reg, turn, builtin.CommentOnPageTool,
		map[string]any{"asks": false, "page": "p1", "body": "noted, again", "edit": "m1"})
	// A comment, a create, a save and its rename, and an edit.
	if len(kb.keys) != 5 {
		t.Fatalf("the page writes reached the knowledge base with %d keys, want 5",
			len(kb.keys))
	}
	// EACH KEY IS AN ID THE ENGINE'S GRAMMAR MINTED, at the instant the work
	// began: the store derives every write's id from it.
	for i, key := range kb.keys {
		at, ok := statelog.OpMintedAt(key)
		if !ok || !at.Equal(began) || statelog.CheckCallerOpID(key) != nil {
			t.Errorf("page write %d carries key %q minted at %v (%v), want an "+
				"op id carrying %s when the work began — a write re-made after "+
				"an adoption is otherwise decided twice", i, key, at, ok, began)
		}
	}
}

// A TURN REBASED PAST THE LEDGER MINTS EVERY WRITE AT ITS REBASE, the tracker's
// and the knowledge base's alike. The engine rebases a turn that started longer
// ago than the ledger can vouch for (turnctx.Turn.RebasedTo), since an
// operation minted at that start, its row swept, is answered `unknown` on every
// node. The rebase outranks BOTH starts, the work's and the run's own, as
// either can be that old.
func TestARebasedTurnMintsEveryWriteAtItsRebase(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	rebasedTo := began.Add(40 * 24 * time.Hour)
	for name, keyed := range map[string]bool{"a keyed turn": true, "a keyless turn": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rebased := func() *turnctx.Turn {
				tn := workTurn(t)
				tn.RunID = statelog.NewOpID(began, "")
				tn.WorkKey, tn.WorkSince = "", time.Time{}
				if keyed {
					tn.WorkKey, tn.WorkSince = "wk-1", began
				}
				tn.RebasedTo = rebasedTo
				return tn
			}

			trk := newFakeTracker()
			call(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
				rebased(), builtin.UpdateWorkItemTool, map[string]any{
					"item": "ENG-1", "priority": "urgent",
				})
			if len(trk.opIDs) != 1 {
				t.Fatalf("the update wrote %v", trk.opIDs)
			}
			if minted, _ := statelog.OpMintedAt(trk.opIDs[0]); !minted.Equal(rebasedTo) {
				t.Fatalf("the rebased update was minted at %s, want the rebase's %s — "+
					"minted at the start, no node could vouch for it", minted, rebasedTo)
			}

			kb := newFakeKB()
			call(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb}), rebased(),
				builtin.CommentOnPageTool, map[string]any{"asks": false, "page": "p1", "body": "noted"})
			if len(kb.comments) != 1 || len(kb.keys) != 1 {
				t.Fatalf("the comment reached the knowledge base %d time(s) under %v",
					len(kb.comments), kb.keys)
			}
			if minted, _ := statelog.OpMintedAt(kb.keys[0]); !minted.Equal(rebasedTo) {
				t.Fatalf("the rebased comment was minted at %s, want the rebase's %s",
					minted, rebasedTo)
			}
		})
	}
}

// call drives a seat-callable tool under the given turn, as a caller holding
// every grant: a rename of a container this fixture holds no chart for is
// otherwise undecidable.
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
