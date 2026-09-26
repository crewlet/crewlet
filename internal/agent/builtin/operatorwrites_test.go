package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN OPERATOR'S SECOND WRITE TO ONE OBJECT IS A SECOND WRITE.
//
// # What was wrong
//
// Every operation id a work tool writes under is derived from the TURN, so a
// re-run turn — which the engine's redelivery guarantees make ordinary —
// writes once. Outside a turn the derivation fell back to values that were
// STABLE rather than fresh: `verb + "-" + object` for a task, and the literal
// string `operator` for the person tools. So an operation id was stable for
// the life of the deployment, and the operation ledger collapsed
// every write after the first as a redelivery.
//
// `/operator/mcp` is the only surface that writes with no turn. It is also the
// ONLY write path the dashboard offers — the whole `ToolCall` surface tells a
// reader to paste these calls to the assistant they have connected to it — and
// what an operator's own assistant connects to. Measured against a running
// engine: a work item could be updated exactly once. The second update, and
// every update after it, wrote nothing and answered `outcome: "applied"` with
// the first write's position and version.
//
// That is the worst shape a write surface has. A refusal is recoverable and a
// silent no-op reported as success is not: the caller reads the receipt, sees
// `applied`, and moves on.
//
// # Why no existing case caught it
//
// `callWork` binds a turn to every call, which is correct for the seat tools
// it was written for and is exactly the branch that works. The no-turn path
// had no case at all.
//
// # What is asserted
//
// The OPERATION IDS, not the rows: this suite's writers are fakes, so the
// ledger that does the collapsing is not here. Two ids that are equal are two
// writes the ledger will collapse, wherever it runs — which is the property,
// and it is checkable without a broker.

// operatorActor is a write with no turn: a token acting for the company.
//
// It is what `opsmcp.WorkActor` builds — the operator's own name as the
// handle, the kind saying it is not a seat, and NO TURN ID, which is the whole
// point of the case.
func operatorActor(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return builtin.Actor{
		Handle: "founder", Kind: tracker.AuthorOperator, OperatorID: "founder",
	}, nil
}

// callNoTurn drives a seat-callable tool with NO turn, which is what the
// operator surface does.
func callNoTurn(t *testing.T, reg *tools.Registry, name string,
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
	got, err := callable.CallForTurn(t.Context(), nil, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func TestAnOperatorsSecondWriteIsNotCollapsedAsARedelivery(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Actor: operatorActor,
	})

	for i := range 2 {
		if got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "priority": "urgent",
		}); got.Failed {
			t.Fatalf("update %d failed: %s", i+1, got.Output)
		}
	}
	if len(trk.opIDs) != 2 {
		t.Fatalf("the two updates produced %d writes: %v", len(trk.opIDs), trk.opIDs)
	}
	if trk.opIDs[0] == trk.opIDs[1] {
		t.Errorf("both updates wrote under %q, so the operation ledger collapses "+
			"the second as a redelivery: an operator can change a work item "+
			"exactly once, and every later change answers `applied` and does "+
			"nothing", trk.opIDs[0])
	}
	// AND THE ID STILL SAYS WHAT IT IS. Fresh is not the same as opaque: the
	// verb and the object are what makes a stuck operation findable in the
	// ledger, and a bare uuid would be a row nobody can trace back. So the two
	// ids share everything up to the part that makes them unique.
	shared := commonPrefix(trk.opIDs[0], trk.opIDs[1])
	if !strings.HasPrefix(shared, "update-") || len(shared) <= len("update-") {
		t.Errorf("the two operation ids are %q and %q, which share only %q — an "+
			"id that does not name its verb and object is a ledger row nobody "+
			"can trace back", trk.opIDs[0], trk.opIDs[1], shared)
	}
}

// AND A RE-RUN OF A TURN'S WRITE IS STILL ONE OPERATION, which is the half a
// careless fix breaks: a re-run turn re-issues the same call, and two writes
// there are two comments, two status moves and two notifications for one thing
// that happened. (Two calls in ONE run are two writes — see operation_test.go.)
func TestATurnsRepeatedWriteIsStillOneOperation(t *testing.T) {
	t.Parallel()
	first, second := newFakeTracker(), newFakeTracker()
	for _, trk := range []*fakeTracker{first, second} {
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "priority": "urgent",
		}); got.Failed {
			t.Fatalf("update failed: %s", got.Output)
		}
	}
	if len(first.opIDs) != 1 || len(second.opIDs) != 1 {
		t.Fatalf("one update each wrote %v and %v", first.opIDs, second.opIDs)
	}
	if first.opIDs[0] != second.opIDs[0] {
		t.Errorf("a re-run turn wrote under %q and then %q — the ledger cannot "+
			"collapse the retry, so the redelivery the engine guarantees "+
			"becomes a second write", first.opIDs[0], second.opIDs[0])
	}
}

// AND TWO RUNS OF ONE TRIGGER ARE STILL ONE OPERATION, which is the case that
// stopped working when a turn id started naming a run.
//
// A turn that fails without reaching outside the engine is redelivered and
// runs again under a NEW run id. An operation id seeded from that is a
// different id every attempt, so the ledger has nothing to collapse and the
// retry posts a second comment, moves the status a second time and notifies
// everybody twice. The seed has to be the identity a redelivery reproduces,
// which is the work key. See ADR-0017.
func TestTwoRunsOfOneTriggerWriteOneOperation(t *testing.T) {
	t.Parallel()
	seeds := map[string]string{}
	// The SAME work key from two different runs, which is exactly what a
	// redelivered trigger produces.
	attempt := func(runID string) *turnctx.Turn {
		tn := workTurn(t)
		tn.RunID, tn.WorkKey = runID, "wk-1"
		return tn
	}
	for name, turn := range map[string]*turnctx.Turn{
		"first attempt":  attempt("run-1"),
		"the redelivery": attempt("run-2"),
	} {
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		entry, ok := reg.Lookup(builtin.UpdateWorkItemTool)
		if !ok {
			t.Fatal("update_work_item is not registered")
		}
		callable, ok := entry.Tool.(tools.SeatCallable)
		if !ok {
			t.Fatal("update_work_item is not seat-callable")
		}
		if _, err := callable.CallForTurn(t.Context(), turn, map[string]any{
			"item": "ENG-1", "priority": "urgent",
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(trk.opIDs) != 1 {
			t.Fatalf("%s wrote %v", name, trk.opIDs)
		}
		seeds[name] = trk.opIDs[0]
	}
	if seeds["first attempt"] != seeds["the redelivery"] {
		t.Errorf("the attempts wrote under %q and %q — a redelivery the engine "+
			"guarantees becomes a second write, with a second comment and a "+
			"second notification for one thing that happened",
			seeds["first attempt"], seeds["the redelivery"])
	}
}

// AND A TURN WITH NO LEDGERABLE TRIGGER SEEDS FROM ITS RUN.
//
// A scheduled fire has no work key — the documented "nothing to collapse" —
// but a run can still repeat its own calls: a resumed coding turn that fails is
// retried under the same run, re-entering the same conversation and making its
// calls again. An empty seed would mint a fresh id per call and land each of
// those twice; the run names them again, and invents no cross-run guarantee.
func TestATurnWithNoWorkKeySeedsFromItsRun(t *testing.T) {
	t.Parallel()
	agent := builtin.Actor{TurnID: "run-1", WorkKey: ""}
	if got := agent.OperationSeed(); got != "run-1" {
		t.Errorf("seed = %q, want the run — an empty seed mints a fresh id per call", got)
	}
	keyed := builtin.Actor{TurnID: "run-1", WorkKey: "wk-1"}
	if got := keyed.OperationSeed(); got != "wk-1" {
		t.Errorf("seed = %q, want the work key — the run does not survive a redelivery", got)
	}
	// OUTSIDE A TURN THERE IS NOTHING TO BE IDEMPOTENT AGAINST: an operator's
	// MCP client made one call and nothing will redeliver it.
	if got := (builtin.Actor{}).OperationSeed(); got != "" {
		t.Errorf("seed = %q, want empty for a write with no turn behind it", got)
	}
}

// commonPrefix is the leading text two strings share.
func commonPrefix(a, b string) string {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}
