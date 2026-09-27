package chart_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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

// op is one operation. A create_seat is an AGENT's unless a case says
// otherwise ([seatOp]): a create_seat states its kind, and these cases are
// about everything else.
func op(kind chart.OperationKind, objectKind chart.ObjectKind, id, parent string) chart.Operation {
	o := chart.Operation{
		Kind:   kind,
		Object: chart.ObjectRef{Kind: objectKind, ID: id},
		Parent: parent,
	}
	if kind == chart.OpCreateSeat {
		o.SeatKind = chart.SeatAgent
	}
	return o
}

// seatOp is a create_seat of the kind named.
func seatOp(kind chart.SeatKind, handle, parent string) chart.Operation {
	return chart.Operation{Kind: chart.OpCreateSeat,
		Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: handle},
		Parent:   parent,
		SeatKind: kind}
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

	h.must(create("op-create", chart.KindUnit, "platform", ""))
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

	h.must(create("op-create", chart.KindUnit, "platform", ""))
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

	for _, kind := range []chart.ObjectKind{chart.KindUnit, chart.KindSeat} {
		verb := chart.OpCreateUnit
		if kind == chart.KindSeat {
			verb = chart.OpCreateSeat
		}
		for _, key := range chart.ReservedKeys(kind) {
			_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
				op(verb, kind, key, ""),
			}})
			if ref := refusal(t, err); ref.Rule != chart.RuleReservedKey {
				t.Errorf("%s %q: rule = %q, want %q — it names the org root, one "+
					"of this log's own subject kinds or, for a seat, the word "+
					"Datadog's routing uses for nobody",
					kind, key, ref.Rule, chart.RuleReservedKey)
			}
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

// A MOVE DOES NOT CLEAR THE UNIT'S LEAD.
//
// The published edge is FULL POST-STATE for the unit's structure, and a move
// states a parent and no lead — so an edge built from the operation alone
// carries an empty lead, and the apply takes the unit's authored lead away as
// a side effect of moving it: `chart_leads` loses the row, every escalation
// from the team reaches the parent's lead instead, and nothing says why. The
// lead comes from the working copy, as a set_lead's parent does.
func TestMovingAUnitKeepsItsLead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-seed",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "product"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "engineering", Lead: "sarah-chen"}))

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpMove, chart.KindUnit, "platform", "product"),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(edges) != 1 || edges[0].Parent != "product" || edges[0].Lead != "sarah-chen" {
		t.Fatalf("the move published %+v, want platform under product and still "+
			"led by sarah-chen — an edge is full post-state, so a lead read from "+
			"the operation rather than from the chart is an empty one", edges)
	}
	h.must(place("op-move", edges...))
	if got := h.column(`SELECT handle FROM chart_leads WHERE unit_key = 'platform'`); !slices.Equal(
		got, []string{"sarah-chen"}) {
		t.Errorf("after the move the unit's lead edge is %v, want [sarah-chen]", got)
	}
}

// A CREATED UNIT IS LED BY THE LEAD ITS CREATE NAMES.
//
// "Add the platform team, led by the SRE" is one gesture, and the create_unit
// that states both was published with the lead dropped: the edge was built from
// a working copy that recorded a create's parent and never its lead, so the team
// landed unled and inherited its parent's lead instead, with a 200 in hand.
func TestACreatedUnitIsLedByTheLeadItNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		{Kind: chart.OpCreateUnit, Object: chart.ObjectRef{Kind: chart.KindUnit,
			ID: "platform"}, Lead: "sre"},
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(edges) != 1 || edges[0].Lead != "sre" {
		t.Fatalf("the create published %+v, want platform led by sre", edges)
	}
	h.must(place("op-batch", edges...))
	if got := h.column(`SELECT handle FROM chart_leads WHERE unit_key = 'platform'`); !slices.Equal(
		got, []string{"sre"}) {
		t.Errorf("the created unit's lead edge is %v, want [sre]", got)
	}
}

// EVERY EDGE SAYS WHAT ITS BATCH DID TO THE OBJECT, AND CARRIES ITS WHOLE
// POST-STATE.
//
// The verb is what the apply decides by — a create is declined where its
// address turned out to be held, a move or a lead change where its object is
// gone — and there is one per edge because there is one edge per object: a
// create the batch then moved is still a create, since only a create may make
// the row.
func TestABatchMarksEachEdgeWithWhatItDid(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(create("op-product", chart.KindUnit, "product", ""))
	h.must(place("op-platform", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen", Op: chart.OpCreateUnit}))

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		{Kind: chart.OpCreateUnit, Object: chart.ObjectRef{Kind: chart.KindUnit,
			ID: "design"}, Lead: "ana"},
		op(chart.OpMove, chart.KindUnit, "platform", "product"),
		{Kind: chart.OpSetLead, Object: chart.ObjectRef{Kind: chart.KindUnit,
			ID: "engineering"}, Lead: "omar"},
		op(chart.OpCreateSeat, chart.KindSeat, "bob", "design"),
		op(chart.OpMove, chart.KindSeat, "bob", "product"),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := []chart.Edge{
		{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "design"},
			Lead: "ana", Op: chart.OpCreateUnit},
		{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Parent: "product", Lead: "sarah-chen", Op: chart.OpMove},
		{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
			Lead: "omar", Op: chart.OpSetLead},
		{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "bob"},
			Parent: "product", Op: chart.OpCreateSeat, Kind: chart.SeatAgent},
	}
	if !reflect.DeepEqual(edges, want) {
		t.Errorf("the batch published\n  %+v\nwant\n  %+v", edges, want)
	}

	// AND THE CREATE LANDS AS ONE: design exists, led by ana.
	h.must(place("op-batch", edges...))
	if got := h.column(`SELECT handle FROM chart_leads WHERE unit_key = 'design'`); !slices.Equal(
		got, []string{"ana"}) {
		t.Errorf("the created unit's lead edge is %v, want [ana] — a create_unit "+
			"that names its lead makes the unit led", got)
	}
}

// A FIELD AN OPERATION DOES NOT TAKE IS REFUSED, NOT DROPPED.
//
// Dropped, the batch answered as though it asked for less than it said: a
// caller who sent a set_lead with a parent believed they had moved the unit
// too, and was told it landed.
func TestAFieldAnOperationDoesNotTakeIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(place("op-seed",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"}}))

	for _, c := range []struct {
		name string
		op   chart.Operation
	}{
		{"a parent on a set_lead", chart.Operation{Kind: chart.OpSetLead,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
			Parent: "product", Lead: "sre"}},
		{"a lead on a move", chart.Operation{Kind: chart.OpMove,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
			Lead:   "sre"}},
		{"a lead on a create_seat", chart.Operation{Kind: chart.OpCreateSeat,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"},
			Lead:   "sre"}},
		{"a parent on a remove", chart.Operation{Kind: chart.OpRemoveObject,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
			Parent: "engineering"}},
	} {
		_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{c.op}})
		if ref := refusal(t, err); ref.Rule != chart.RuleUnusedField {
			t.Errorf("%s: rule = %q, want %q", c.name, ref.Rule, chart.RuleUnusedField)
		}
	}

	// THE CONTROL: the same operations carrying only what they take are
	// accepted, so the refusals above are about the extra field.
	if _, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		{Kind: chart.OpSetLead, Object: chart.ObjectRef{Kind: chart.KindUnit,
			ID: "engineering"}, Lead: "sre"},
		op(chart.OpMove, chart.KindSeat, "sre", "engineering"),
	}}); err != nil {
		t.Errorf("a well-formed batch was refused: %v", err)
	}
}

// A CREATE NAMES THE KIND IT MAKES, AND AN OBJECT OF THE OTHER KIND IS
// REFUSED.
//
// The replay used to take the object's kind and ignore the operation's, so a
// `create_unit` naming a seat created a seat: a batch whose two halves
// disagreed about what it was making landed as whichever half the replay
// happened to read.
func TestACreateNamingTheOtherKindIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, o := range []chart.Operation{
		op(chart.OpCreateUnit, chart.KindSeat, "sre", ""),
		op(chart.OpCreateSeat, chart.KindUnit, "platform", ""),
	} {
		_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{o}})
		if ref := refusal(t, err); ref.Rule != chart.RuleUnknownKind {
			t.Errorf("%s of a %s: rule = %q, want %q", o.Kind, o.Object.Kind,
				ref.Rule, chart.RuleUnknownKind)
		}
	}
	// THE CONTROL: each create naming its own kind is accepted.
	if _, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "sre", "platform"),
	}}); err != nil {
		t.Errorf("a well-formed create was refused: %v", err)
	}
}

// renameOp is one rename: the object by the address it answers to, and the one
// it moves onto.
func renameOp(kind chart.ObjectKind, id, to string) chart.Operation {
	return chart.Operation{Kind: chart.OpRename,
		Object: chart.ObjectRef{Kind: kind, ID: id}, To: to}
}

// managesOp is a set_manages giving one seat the whole list entries.
func managesOp(handle string, entries ...string) chart.Operation {
	return chart.Operation{Kind: chart.OpSetManages,
		Object:  chart.ObjectRef{Kind: chart.KindSeat, ID: handle},
		Manages: entries}
}

// A RENAME IS REPLAYED LIKE EVERY OTHER OPERATION: what follows it in the batch
// sees the chart it leaves.
//
// The renamed unit's members follow it in the replay exactly as the apply's
// cascade will move them, so a later operation names the unit by its new key,
// a later create may take the address it left as an alias, and the record
// states one edge for the renamed object — from the address the batch found it
// at to the one it ends on, however many renames it made on the way.
func TestARenameIsReplayedForTheOperationsAfterIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(place("op-platform", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen", Op: chart.OpCreateUnit}))
	h.must(create("op-bob", chart.KindSeat, "bob", "platform"))

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
		renameOp(chart.KindUnit, "infra", "core"),
		op(chart.OpCreateSeat, chart.KindSeat, "ana", "core"),
		op(chart.OpCreateUnit, chart.KindUnit, "infra", ""),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := []chart.Edge{
		{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "core"},
			Parent: "engineering", Lead: "sarah-chen", Op: chart.OpRename,
			From: "platform"},
		{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"},
			Parent: "core", Op: chart.OpCreateSeat, Kind: chart.SeatAgent},
		{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "infra"},
			Op: chart.OpCreateUnit},
	}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("the batch published\n  %+v\nwant\n  %+v", edges, want)
	}

	// AND A LATER OPERATION SEES THE MEMBERS THE RENAME CARRIED: the team is
	// not empty under its new key.
	_, _, err = h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
		op(chart.OpRemoveObject, chart.KindUnit, "infra", ""),
	}})
	if ref := refusal(t, err); ref.Rule != chart.RuleUnitNotEmpty {
		t.Errorf("removing the renamed team: rule = %q, want %q — the replay "+
			"lost the members its rename carried", ref.Rule, chart.RuleUnitNotEmpty)
	}

	// AND IT LANDS AS THE REPLAY SAID: bob followed the unit by cascade, ana
	// was created in it, and the address it left belongs to a new unit.
	h.must(place("op-batch", edges...))
	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'bob'`); got != "core" {
		t.Errorf("bob sits in %q, want core", got)
	}
	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'ana'`); got != "core" {
		t.Errorf("ana sits in %q, want core", got)
	}
	if got := h.unit("core"); got.Origin() != "platform" ||
		!slices.Equal(got.FormerKeys, []string{"platform"}) {
		t.Errorf("core carries origin %q and former keys %v, want platform "+
			"alone — infra was an address only between two operations of "+
			"one record, which nothing could ever have referred to",
			got.Origin(), got.FormerKeys)
	}
	if got := h.column(`SELECT key FROM chart_units ORDER BY key`); !slices.Equal(got,
		[]string{"core", "engineering", "infra"}) {
		t.Errorf("the units are %v, want [core engineering infra]", got)
	}
}

// A REMOVAL AFTER A RENAME IS REPLAYED AS THE LOG WILL APPLY IT.
//
// The removal record names the object by the address the rows hold it at and
// tombstones that one, so the replay does the same: the address the batch
// found the object at is removed for the operations after it, and the address
// a rename in this batch moved it onto — which the log never gives it — is
// not. A replay that tombstoned the second refused a creation the log would
// have accepted and accepted one onto the first that the log would drop.
func TestARemovalAfterARenameIsReplayedAsTheLogWillApplyIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-platform", chart.KindUnit, "platform", ""))

	_, removed, err := h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
		op(chart.OpRemoveObject, chart.KindUnit, "infra", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "infra", ""),
	}})
	if err != nil {
		t.Fatalf("a creation onto the address the removed object never held "+
			"on the log was refused: %v", err)
	}
	if want := []chart.ObjectRef{{Kind: chart.KindUnit, ID: "platform"}}; !slices.Equal(removed, want) {
		t.Errorf("the batch removes %v, want %v", removed, want)
	}
	_, _, err = h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
		op(chart.OpRemoveObject, chart.KindUnit, "infra", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""),
	}})
	if ref := refusal(t, err); ref.Rule != chart.RuleKeyRemoved {
		t.Errorf("a creation onto the address the removal tombstones: rule = "+
			"%q, want %q", ref.Rule, chart.RuleKeyRemoved)
	}
}

// WHAT A RENAME MAY NOT DO, each refused on its own rule.
func TestARenameIsRefusedWhereItCouldNotLand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-platform", chart.KindUnit, "platform", ""))
	h.must(create("op-design", chart.KindUnit, "design", ""))

	for _, c := range []struct {
		name string
		ops  []chart.Operation
		rule string
	}{
		{"onto the address it has", []chart.Operation{
			renameOp(chart.KindUnit, "platform", "platform"),
		}, chart.RuleRenameUnchanged},
		{"onto another unit's key", []chart.Operation{
			renameOp(chart.KindUnit, "platform", "design"),
		}, chart.RuleKeyTaken},
		{"onto a reserved word", []chart.Operation{
			renameOp(chart.KindUnit, "platform", "tree"),
		}, chart.RuleReservedKey},
		{"an object the batch creates", []chart.Operation{
			op(chart.OpCreateUnit, chart.KindUnit, "sales", ""),
			renameOp(chart.KindUnit, "sales", "revenue"),
		}, chart.RuleRenameCreated},
		{"with no address to move onto", []chart.Operation{
			renameOp(chart.KindUnit, "platform", ""),
		}, chart.RuleBadKey},
		{"by the address a rename earlier in the batch left", []chart.Operation{
			renameOp(chart.KindUnit, "platform", "infra"),
			op(chart.OpMove, chart.KindUnit, "platform", "design"),
		}, chart.RuleNoSuchObject},
		{"onto the alias another rename left", []chart.Operation{
			renameOp(chart.KindUnit, "platform", "infra"),
			renameOp(chart.KindUnit, "design", "platform"),
		}, chart.RuleKeyTaken},
	} {
		_, _, err := h.validate(chart.Batch{Operations: c.ops})
		if ref := refusal(t, err); ref.Rule != c.rule {
			t.Errorf("%s: rule = %q, want %q (%s)", c.name, ref.Rule, c.rule, ref.Detail)
		}
	}

	// THE CONTROL: a rename onto a free address, and back again, are each
	// accepted — and the pair together publishes nothing, because it leaves
	// the unit exactly where it was.
	if _, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
	}}); err != nil {
		t.Errorf("a rename onto a free address was refused: %v", err)
	}
	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		renameOp(chart.KindUnit, "platform", "infra"),
		renameOp(chart.KindUnit, "infra", "platform"),
	}})
	if err != nil || len(edges) != 0 {
		t.Errorf("a rename there and back published %+v (%v), want nothing", edges, err)
	}
}
