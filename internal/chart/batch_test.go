package chart_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// THE BATCH RULES, and why each one is checked against a REPLAY.
//
// Every rule here is about the chart AFTER the batch: a move closes a cycle
// only in combination with the moves beside it, a parent may be created by an
// earlier operation, and a unit may be emptied by one operation and removed by
// the next. Each case below is one that a per-operation check against the
// stored rows alone would get wrong — in one direction or the other.

// validate runs a batch against the harness's own estate.
//
// OVER A DIRECTORY THAT HOLDS NOBODY, which is what every case in this file
// is about: these are the chart's OWN rules — cycles, empty units, addresses —
// and a directory that refused a removal would make each of them fail for a
// reason they are not about. The cases that ARE about the directory supply
// their own; see [harness.validateWith].
func (h *harness) validate(batch chart.Batch) (
	edges []chart.Edge, removed []chart.ObjectRef, err error) {

	h.t.Helper()
	return h.validateWith(batch, noHolders{})
}

// validateWith runs a batch against a directory a case names.
func (h *harness) validateWith(batch chart.Batch, holders chart.Holders) (
	edges []chart.Edge, removed []chart.ObjectRef, err error) {

	h.t.Helper()
	txErr := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		edges, removed, err = batch.Validate(h.t.Context(), tx, holders)
		return nil
	})
	if txErr != nil {
		h.t.Fatalf("read the estate: %v", txErr)
	}
	return edges, removed, err
}

// refusal is the typed refusal a batch produced, or a failure.
func refusal(t *testing.T, err error) *chart.RefusalError {
	t.Helper()
	if err == nil {
		t.Fatal("the batch was accepted and this case is about it being refused")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Fatalf("the error does not answer ErrRefused, so a caller cannot "+
			"tell a rule it broke from a broker that is down: %v", err)
	}
	var ref *chart.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("the refusal is not typed, so a surface has to parse prose "+
			"to know which operation failed: %v", err)
	}
	return ref
}

func op(kind chart.OperationKind, objectKind chart.ObjectKind, id, parent string) chart.Operation {
	return chart.Operation{
		Kind:   kind,
		Object: chart.ObjectRef{Kind: objectKind, ID: id},
		Parent: parent,
	}
}

// A PARENT CREATED EARLIER IN THE BATCH IS A PARENT.
//
// This is the case a per-operation check against the stored rows gets wrong in
// the REFUSING direction: it would reject the ordinary way anybody builds a
// chart — the department, then the team inside it, then the seat inside that —
// because none of the containers exists yet when its contents are stated.
func TestAParentCreatedEarlierInTheBatchIsAParent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", "engineering"),
		op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "platform"),
	}})
	if err != nil {
		t.Fatalf("a chart built top down was refused: %v", err)
	}
	if len(edges) != 3 {
		t.Fatalf("the batch produced %d edges, want 3: %+v", len(edges), edges)
	}
	if edges[2].Parent != "platform" {
		t.Errorf("the seat landed under %q, want platform", edges[2].Parent)
	}
}

// AND A PARENT NOTHING CREATES IS REFUSED, NAMING IT.
func TestAPlacementUnderAUnitNobodyCreatesIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "platform", "engineering"),
	}})
	ref := refusal(t, err)
	if ref.Rule != chart.RuleNoSuchParent {
		t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleNoSuchParent)
	}
	if ref.Index != 0 {
		t.Errorf("index = %d, want 0", ref.Index)
	}
}

// A CYCLE THE BATCH'S OWN MOVES CLOSE IS REFUSED, NAMING BOTH UNITS.
//
// THE CASE THE WHOLE DESIGN IS FOR. Neither move is wrong on its own: moving
// engineering under platform is fine while platform is at the root, and moving
// platform under engineering is fine while engineering is at the root. Together
// they put each under the other, and no per-object subject would ever have
// shown either writer the other's move — which is exactly why the structure
// arbitrates on ONE subject and is validated as a replay.
func TestACycleTheBatchsOwnMovesCloseIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
		op(chart.OpMove, chart.KindUnit, "platform", "engineering"),
		op(chart.OpMove, chart.KindUnit, "engineering", "platform"),
	}})
	ref := refusal(t, err)
	if ref.Rule != chart.RuleCycle {
		t.Fatalf("rule = %q, want %q — two moves that are each locally valid "+
			"can jointly put each unit under the other, and a check against "+
			"the stored rows alone accepts exactly that pair",
			ref.Rule, chart.RuleCycle)
	}
	if ref.Index != 3 {
		t.Errorf("index = %d, want 3 — the refusal names the operation that "+
			"closed the cycle, not the one that started the path", ref.Index)
	}
	for _, name := range []string{"engineering", "platform"} {
		if !strings.Contains(ref.Detail, name) {
			t.Errorf("the refusal does not name %q: %s", name, ref.Detail)
		}
	}
}

// AND A SINGLE MOVE THAT CLOSES A CYCLE AGAINST THE STORED ROWS IS REFUSED TOO.
func TestAMoveUnderItsOwnDescendantIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-seed",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "engineering"}))

	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpMove, chart.KindUnit, "engineering", "platform"),
	}})
	if ref := refusal(t, err); ref.Rule != chart.RuleCycle {
		t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleCycle)
	}
}

// A UNIT THAT STILL HOLDS ANYTHING CANNOT BE REMOVED.
//
// An orphaned subtree is reachable from nothing and removable by nothing: its
// children's `parent_key` names a unit that is not there, so no walk finds
// them, and nothing names them to take them out.
func TestRemovingAUnitThatStillHoldsSomethingIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-seed",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
			Parent: "engineering"}))

	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpRemoveObject, chart.KindUnit, "engineering", ""),
	}})
	ref := refusal(t, err)
	if ref.Rule != chart.RuleUnitNotEmpty {
		t.Fatalf("rule = %q, want %q", ref.Rule, chart.RuleUnitNotEmpty)
	}
	if !strings.Contains(ref.Detail, "sarah-chen") {
		t.Errorf("the refusal does not say what is still in it: %s", ref.Detail)
	}

	// AND EMPTYING IT IN THE SAME BATCH IS ENOUGH. This is the case a
	// per-operation check against the stored rows gets wrong in the
	// refusing direction: "move everybody out, then dissolve the team" is
	// one gesture, and it must be able to be one batch.
	_, removed, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpMove, chart.KindSeat, "sarah-chen", ""),
		op(chart.OpRemoveObject, chart.KindUnit, "engineering", ""),
	}})
	if err != nil {
		t.Fatalf("emptying a unit and then removing it was refused: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != "engineering" {
		t.Errorf("removed = %+v, want the one unit", removed)
	}
}

// A CREATE ONTO A REMOVED ADDRESS IS REFUSED, AND SAYS SO DIFFERENTLY.
//
// A taken key needs a different name; a REMOVED key can never be used again at
// all, because its history, its references and the tombstone that stops its old
// records applying are all keyed on it. Two remedies, so two rules.
func TestACreateOntoARemovedAddressIsRefusedOnItsOwnRule(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-content", "platform", nil))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion,
			Objects: []chart.ObjectRef{{Kind: chart.KindUnit, ID: "platform"}}},
		chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermUnit, ID: "platform"},
		})))

	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
	}})
	if ref := refusal(t, err); ref.Rule != chart.RuleKeyRemoved {
		t.Errorf("rule = %q, want %q — a removed address and a taken one have "+
			"different remedies, so they are different rules",
			ref.Rule, chart.RuleKeyRemoved)
	}
}

// A CREATE ONTO AN ADDRESS SOMETHING ALREADY HOLDS IS REFUSED.
func TestACreateOntoATakenAddressIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-content", "platform", nil))
	_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
	}})
	if ref := refusal(t, err); ref.Rule != chart.RuleKeyTaken {
		t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleKeyTaken)
	}

	// AND THE SAME SPELLING IN THE OTHER NAMESPACE IS LEGAL. A unit key
	// and a seat handle are different namespaces — neither reserves a
	// prefix — so a company may hold a team and a person that answer to
	// one word, and refusing that would be an invented rule.
	if _, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateSeat, chart.KindSeat, "platform", ""),
	}}); err != nil {
		t.Errorf("a seat sharing a unit's spelling was refused: %v — a unit "+
			"key and a seat handle are different namespaces", err)
	}
}

// A RESERVED KEY IS REFUSED, NAMING THE SET.
func TestAReservedKeyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, key := range chart.ReservedKeys {
		_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
			op(chart.OpCreateUnit, chart.KindUnit, key, ""),
		}})
		if ref := refusal(t, err); ref.Rule != chart.RuleReservedKey {
			t.Errorf("%q: rule = %q, want %q — it names the org root or one of "+
				"this log's own subject kinds, and a company object keyed on "+
				"it would collide with them in every scope path",
				key, ref.Rule, chart.RuleReservedKey)
		}
	}
}

// A BATCH PAST THE CAP IS REFUSED BEFORE ANY RULE RUNS.
//
// One batch is one record, and a record past the broker's maximum payload is
// refused PERMANENTLY with no retry that can ever place it — so the cap is a
// size rather than a taste, and it is checked first because every rule below it
// costs a read the refusal makes pointless.
func TestABatchPastTheCapIsRefusedBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	ops := make([]chart.Operation, 0, chart.MaxBatchOperations+1)
	for i := range chart.MaxBatchOperations + 1 {
		// EVERY ONE OF THEM ALSO BREAKS A RULE — no such parent — so a
		// cap checked after the replay would report that instead, and
		// this case would pass while proving nothing about the cap.
		ops = append(ops, op(chart.OpCreateUnit, chart.KindUnit,
			"unit-"+strconv.Itoa(i), "nowhere"))
	}
	_, _, err := h.validate(chart.Batch{Operations: ops})
	if ref := refusal(t, err); ref.Rule != chart.RuleTooManyOperations {
		t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleTooManyOperations)
	}
}

// A BATCH THAT MOVES ONE OBJECT TWICE PUBLISHES ITS FINAL PLACEMENT.
//
// The record is FULL POST-STATE, so two edges for one object would leave the
// applied result dependent on which the applier wrote last — which is arrival
// order deciding the chart.
func TestAnObjectMovedTwiceInOneBatchPublishesOneEdge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "product", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "engineering"),
		op(chart.OpMove, chart.KindSeat, "sarah-chen", "product"),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var seats []chart.Edge
	for _, edge := range edges {
		if edge.Object.Kind == chart.KindSeat {
			seats = append(seats, edge)
		}
	}
	if len(seats) != 1 {
		t.Fatalf("the batch published %d edges for one seat, want 1: %+v",
			len(seats), seats)
	}
	if seats[0].Parent != "product" {
		t.Errorf("the seat's published parent is %q, want product — the final "+
			"placement, because the record is full post-state", seats[0].Parent)
	}
}

// AND A CREATE FOLLOWED BY A REMOVE IN ONE BATCH PUBLISHES NEITHER.
//
// The placement would create the row the removal's tombstone then has to
// delete, which is a record whose only effect is on itself.
func TestAnObjectCreatedAndRemovedInOneBatchIsNeverPlaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	edges, removed, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
		op(chart.OpRemoveObject, chart.KindUnit, "platform", ""),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("the batch published %d placements for an object it also "+
			"removed: %+v", len(edges), edges)
	}
	if len(removed) != 1 {
		t.Errorf("removed = %+v, want the one object", removed)
	}
}

// A SET_LEAD DOES NOT MOVE THE UNIT.
//
// An edge is FULL POST-STATE and a set_lead states no parent, so an edge built
// from the operation alone would carry an empty parent — moving the unit to the
// org root as a side effect of naming its lead. The parent comes from the
// working copy instead.
func TestSettingALeadDoesNotMoveTheUnitToTheRoot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", "engineering"),
		{Kind: chart.OpSetLead,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Lead:   "sarah-chen"},
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	idx := slices.IndexFunc(edges, func(e chart.Edge) bool {
		return e.Object.ID == "platform"
	})
	if idx < 0 {
		t.Fatalf("no edge for platform: %+v", edges)
	}
	if edges[idx].Lead != "sarah-chen" {
		t.Errorf("the lead is %q, want sarah-chen", edges[idx].Lead)
	}
	if edges[idx].Parent != "engineering" {
		t.Errorf("naming a lead moved the unit to %q, want it left under "+
			"engineering — an edge is full post-state, so a parent read from "+
			"the operation rather than from the chart is an empty one",
			edges[idx].Parent)
	}
}

// noHolders is a directory that holds nobody, readably.
//
// NOT NIL, which is a different fact: nil is a node that cannot read the
// directory at all and refuses every seat removal naming itself. This is a
// node that read it and found nobody, which is the ordinary state of every
// agent seat in a company.
type noHolders struct{}

func (noHolders) HolderOf(context.Context, *sql.Tx, string) (string, error) {
	return "", nil
}

// heldBy is a directory in which one seat is held.
type heldBy struct{ handle, login string }

func (h heldBy) HolderOf(_ context.Context, _ *sql.Tx, handle string) (string, error) {
	if handle == h.handle {
		return h.login, nil
	}
	return "", nil
}

// unreadable is a directory this node could not read.
type unreadable struct{ err error }

func (u unreadable) HolderOf(context.Context, *sql.Tx, string) (string, error) {
	return "", u.err
}

// A SEAT SOMEBODY HOLDS IS NOT REMOVED, and the refusal names them.
//
// The ordinary mistake this exists for: a reorganisation removes a seat a
// colleague is still using, and what they get afterwards is a session that
// signs in and leads nothing — every authority rule asking what they lead
// falls through, with no message anywhere saying why.
func TestARemoveOfAHeldSeatIsRefusedNamingThePerson(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// CREATED AND REMOVED IN ONE BATCH, which is what the validator works
	// over: these cases are about the rule rather than about persistence,
	// and the removal's own check runs the same either way.
	remove := chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "eng", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "ana", "eng"),
		op(chart.OpRemoveObject, chart.KindSeat, "ana", ""),
	}}

	_, _, err := h.validateWith(remove, heldBy{handle: "ana", login: "ana.admin"})

	refused := refusal(t, err)
	if refused.Rule != chart.RuleSeatHeld {
		t.Fatalf("rule = %q, want %q", refused.Rule, chart.RuleSeatHeld)
	}
	if !strings.Contains(refused.Detail, "ana.admin") {
		t.Errorf("the refusal does not name who holds the seat: %s", refused.Detail)
	}

	// THE CONTROL: a seat nobody holds is removed. Without it this case
	// would pass on a validator that refused every removal.
	if _, _, err := h.validate(remove); err != nil {
		t.Errorf("a seat nobody holds was refused: %v", err)
	}
}

// A NODE THAT CANNOT READ THE DIRECTORY REFUSES, NAMING ITSELF.
//
// This is the arm that makes the whole check worth having. A node that does
// not run the identity domain holds an EMPTY copy of those rows, so reading
// them answers "nobody holds this seat" for every seat in the company — which
// would make a seats-only satellite the one place every removal succeeds, and
// the one place it is least likely to be noticed.
func TestASatelliteRefusesASeatRemoveNamingTheNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	remove := chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "eng", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "ana", "eng"),
		op(chart.OpRemoveObject, chart.KindSeat, "ana", ""),
	}}

	// NO DIRECTORY AT ALL, which is what a node outside the domain's
	// declared set supplies.
	_, _, err := h.validateWith(remove, nil)
	refused := refusal(t, err)
	if refused.Rule != chart.RuleDirectoryUnreadable {
		t.Fatalf("rule = %q, want %q", refused.Rule, chart.RuleDirectoryUnreadable)
	}
	if !strings.Contains(refused.Detail, "node") {
		t.Errorf("the refusal does not say the remedy is another node: %s",
			refused.Detail)
	}

	// AND A DIRECTORY THAT FAILED TO READ, which is the same answer for a
	// different reason: a removal decided without it silently orphans
	// whoever holds the seat.
	_, _, err = h.validateWith(remove, unreadable{err: errors.New("the estate is closed")})
	if refusal(t, err).Rule != chart.RuleDirectoryUnreadable {
		t.Errorf("an unreadable directory did not refuse the removal: %v", err)
	}

	// THE CONTROL: the same removal, on a node that read the directory and
	// found nobody, goes through. Without it both arms above would pass on
	// a validator that refused every seat removal.
	if _, _, err := h.validateWith(remove, noHolders{}); err != nil {
		t.Errorf("a readable directory holding nobody refused the removal: %v", err)
	}
}

// AND A UNIT'S REMOVAL ASKS NOTHING OF THE DIRECTORY, which is the scope
// control: a unit holds no person, so consulting it would refuse a removal on
// a satellite for a reason that cannot apply.
func TestAUnitRemovalNeedsNoDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, _, err := h.validateWith(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "eng", ""),
		op(chart.OpRemoveObject, chart.KindUnit, "eng", ""),
	}}, nil); err != nil {
		t.Errorf("an empty unit's removal consulted a directory it has no "+
			"business asking: %v", err)
	}
}
