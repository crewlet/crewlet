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
// It is what `operator.WorkActor` builds — the operator's own name as the
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

// AND A TURN'S SECOND WRITE STILL IS ONE, which is the half that was always
// right and the half a careless fix breaks: a re-run turn re-issues the same
// call, and two writes there are two comments, two status moves and two
// notifications for one thing that happened.
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

// AND A TURN WITH NO LEDGERABLE TRIGGER IS STILL IDEMPOTENT WITHIN ITS RUN.
//
// A scheduled fire has no work key — the documented "nothing to collapse" —
// and its seed used to be empty, so every call minted a fresh id and an
// executor that updated the same item in two rounds wrote twice. Falling back
// to the run keeps the within-run guarantee without inventing a cross-run one.
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
	// OUTSIDE A TURN WITH NO REQUEST KEY THERE IS NOTHING TO BE IDEMPOTENT
	// AGAINST: an operator's MCP client made one call and nothing will
	// redeliver it.
	if got := (builtin.Actor{}).OperationSeed(); got != "" {
		t.Errorf("seed = %q, want empty for a write with no turn behind it", got)
	}
}

// A PERSON'S RETRY IS ONE WRITE: outside a turn, the caller's own request key
// seeds every derived id.
//
// # What was wrong
//
// Outside a turn every derived id was fresh, which is right for an MCP call
// nobody will repeat and wrong for a person who DOES repeat one: a write that
// answered `unknown` — the broker took it and nobody heard back — is sent
// again, and a fresh id made the retry a second item, a second comment, a
// second priority write. Worse, three verbs (`set_priorities`, `set_pins`,
// `mark_inbox`) did not derive through [builtin.Actor.OperationSeed] at all but
// through a helper of their own, so a seed the actor carried could never have
// reached them.
//
// # What is asserted
//
// The operation ids, as in the cases above: equal ids are writes the ledger
// collapses. The same key twice is one id on every verb; a different key is a
// different one, or the key would collapse a person's SECOND write — the
// defect the case above guards, reached from the other side.
func TestARequestKeySeedsTheOperationOutsideATurn(t *testing.T) {
	t.Parallel()
	keyed := func(key string) func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			return builtin.Actor{
				Handle: "founder", Kind: tracker.AuthorOperator,
				OperatorID: "founder", RequestKey: key,
			}, nil
		}
	}

	// THE PRECEDENCE: a turn's identities outrank a request key, which a
	// turn never carries, and the key is prefixed so it can never equal
	// either of them.
	for name, c := range map[string]struct {
		actor builtin.Actor
		want  string
	}{
		"the key alone":       {builtin.Actor{RequestKey: "r-1"}, "req-r-1"},
		"the run outranks it": {builtin.Actor{TurnID: "run-1", RequestKey: "r-1"}, "run-1"},
		"the work key first":  {builtin.Actor{WorkKey: "wk-1", TurnID: "run-1", RequestKey: "r-1"}, "wk-1"},
	} {
		if got := c.actor.OperationSeed(); got != c.want {
			t.Errorf("%s: seed = %q, want %q", name, got, c.want)
		}
	}

	t.Run("a work item update", func(t *testing.T) {
		t.Parallel()
		ids := func(keys ...string) []string {
			trk := newFakeTracker()
			for _, key := range keys {
				reg := workRegistry(t, builtin.WorkDeps{
					Reader: trk, Writer: trk.as, Actor: keyed(key),
				})
				if got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
					"item": "ENG-1", "priority": "urgent",
				}); got.Failed {
					t.Fatalf("update failed: %s", got.Output)
				}
			}
			return trk.opIDs
		}
		retried := ids("r-1", "r-1")
		if retried[0] != retried[1] {
			t.Errorf("a retried update wrote under %q and then %q — the ledger "+
				"cannot collapse it", retried[0], retried[1])
		}
		if !strings.HasPrefix(retried[0], "req-r-1-update-") {
			t.Errorf("the retried update wrote under %q, which is not seeded "+
				"from its request", retried[0])
		}
		if two := ids("r-1", "r-2"); two[0] == two[1] {
			t.Errorf("two requests wrote under one id %q, so the second is "+
				"collapsed as a retry of the first", two[0])
		}
	})

	t.Run("a created work item", func(t *testing.T) {
		t.Parallel()
		create := func(key, title string) (string, string) {
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as, Actor: keyed(key),
			})
			if got := callNoTurn(t, reg, builtin.CreateWorkItemTool, map[string]any{
				"title": title, "project": "ENG",
			}); got.Failed {
				t.Fatalf("create failed: %s", got.Output)
			}
			if len(trk.created) != 1 || len(trk.opIDs) != 1 {
				t.Fatalf("one create wrote %v", trk.opIDs)
			}
			return trk.created[0].ID, trk.opIDs[0]
		}
		// A CREATE'S OPERATION ID IS OVER THE NEW ITEM'S OWN ID, so a
		// fresh item id made every create a fresh operation and a retried
		// "file this" was two items.
		firstID, firstOp := create("r-1", "Ship the thing")
		againID, againOp := create("r-1", "Ship the thing")
		if firstID != againID || firstOp != againOp {
			t.Errorf("a retried create filed %s under %q and then %s under %q "+
				"— two items for one request", firstID, firstOp, againID, againOp)
		}
		if otherID, _ := create("r-2", "Ship the thing"); otherID == firstID {
			t.Errorf("two requests filed one item %s", otherID)
		}
		freshA, _ := create("", "Ship the thing")
		freshB, _ := create("", "Ship the thing")
		if freshA == freshB {
			t.Errorf("two calls naming no request filed one item %s — an "+
				"assistant that asks twice is asking for two", freshA)
		}
	})

	t.Run("the person verbs", func(t *testing.T) {
		t.Parallel()
		for _, verb := range []struct {
			tool string
			args map[string]any
		}{
			{tracker.SetPrioritiesTool, map[string]any{"items": []any{"ENG-1"}}},
			{tracker.SetPinsTool, map[string]any{"views": map[string]any{"add": []any{"v-1"}}}},
			{tracker.MarkInboxTool, map[string]any{"read": []any{"r-1"}}},
		} {
			opID := func(key string) string {
				person := &personSpy{}
				reg := personSurface(t, newFakeTracker(), person, keyed(key), nil)
				if got := callNoTurn(t, reg, verb.tool, verb.args); got.Failed {
					t.Fatalf("%s failed: %q", verb.tool, got.Output)
				}
				return person.opID
			}
			if a, b := opID("r-1"), opID("r-1"); a != b {
				t.Errorf("a retried %s wrote under %q and then %q — the "+
					"request key never reached it", verb.tool, a, b)
			}
			if a, b := opID("r-1"), opID("r-2"); a == b {
				t.Errorf("two %s requests wrote under one id %q", verb.tool, a)
			}
			if a, b := opID(""), opID(""); a == b {
				t.Errorf("two %s calls naming no request wrote under one id "+
					"%q, so the second is collapsed as a redelivery", verb.tool, a)
			}
		}
	})
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

// AND A REDELIVERED TURN FILES ITS WORK ONCE, which is the same defect reached
// from inside a turn: the create's operation id is over the new item's id, and
// that id was minted fresh on every attempt — so the work key that collapses a
// redelivered update, comment or status move never collapsed a create, and a
// turn that failed after filing its work filed it again on the retry.
func TestARedeliveredTurnFilesOneItem(t *testing.T) {
	t.Parallel()
	filed := map[string]string{}
	for _, run := range []string{"run-1", "run-2"} {
		turn := workTurn(t)
		turn.RunID, turn.WorkKey = run, "wk-1"
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		entry, ok := reg.Lookup(builtin.CreateWorkItemTool)
		if !ok {
			t.Fatal("create_work_item is not registered")
		}
		got, err := entry.Tool.(tools.SeatCallable).CallForTurn(t.Context(), turn,
			map[string]any{"title": "Ship the thing", "project": "ENG"})
		if err != nil || got.Failed {
			t.Fatalf("%s: err=%v result=%+v", run, err, got)
		}
		if len(trk.created) != 1 {
			t.Fatalf("%s filed %d items", run, len(trk.created))
		}
		filed[run] = trk.created[0].ID
	}
	if filed["run-1"] != filed["run-2"] {
		t.Errorf("the attempts filed %s and %s — a redelivery the engine "+
			"guarantees is a second item on somebody's board",
			filed["run-1"], filed["run-2"])
	}
}
